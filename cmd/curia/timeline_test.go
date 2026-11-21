package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/testkit"
)

func timelineSeed(t *testing.T) {
	t.Helper()
	db := testkit.HookHome(t)
	prev := clock
	clock = func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() {
		clock, timelineSinceFlag, timelineRepo, timelineTask, timelineJSON, timelineLimit = prev, "", "", "", false, 200
	})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	raw := rawSQLite(t, db)
	for _, q := range []string{
		`INSERT INTO tasks (id, title, repo, status, created_at, updated_at) VALUES ('T-1','one','/r/a','ready','2026-11-19T10:00:00.000000000Z','2026-11-19T10:00:00.000000000Z')`,
		`INSERT INTO tasks (id, title, repo, status, created_at, updated_at) VALUES ('T-2','two','/r/b','ready','2026-11-19T11:00:00.000000000Z','2026-11-19T11:00:00.000000000Z')`,
		"INSERT INTO tasks (id, title, repo, status, created_at, updated_at) VALUES ('T-3','old','/r/a','ready','2026-11-10T11:00:00.000000000Z','2026-11-10T11:00:00.000000000Z')",
		"INSERT INTO tasks (id, title, repo, status, created_at, updated_at) VALUES ('T-4','esc \x1b[31mred 日本語日本語日本語','/r/a','ready','2026-11-19T11:30:00.000000000Z','2026-11-19T11:30:00.000000000Z')",
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTimelineCommand(t *testing.T) {
	timelineSeed(t)
	out, err := runRoot(t, "timeline")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// header, then oldest first; T-3 is older than the default 24h.
	if len(lines) != 4 || !strings.Contains(lines[1], "T-1") || !strings.Contains(lines[2], "T-2") || strings.Contains(out, "T-3") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("an escape sequence reached the output: %q", out)
	}
	out, _ = runRoot(t, "timeline", "--since", "30d", "--repo", "/r/a")
	if strings.Contains(out, "T-2") || !strings.Contains(out, "T-3") {
		t.Errorf("--since 30d --repo:\n%s", out)
	}
	out, _ = runRoot(t, "timeline", "--task", "T-2", "--json", "--repo", "", "--since", "")
	var rows []struct{ Task, Kind string }
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].Task != "T-2" || rows[0].Kind != "task_filed" {
		t.Errorf("--task --json = %q, %v", out, err)
	}
}

func TestTimelineSinceRejectsGarbage(t *testing.T) {
	timelineSeed(t)
	if _, err := runRoot(t, "timeline", "--since", "yesterday-ish"); err == nil {
		t.Error("--since garbage accepted")
	}
}

func TestTimelineSinceForms(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"6h": now.Add(-6 * time.Hour), "2d": now.AddDate(0, 0, -2), "2026-11-01": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "": now.Add(-time.Hour),
	} {
		if got, err := timelineSince(in, now, time.Hour); err != nil || !got.Equal(want) {
			t.Errorf("timelineSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

// rawSQLite opens the database file directly, to seed rows the commands read.
func rawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustEnv(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is not set", k)
	}
	return v
}

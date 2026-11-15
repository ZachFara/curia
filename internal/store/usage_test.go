package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// usageNow is the clock of the ingest tests; it carries nanoseconds so
// the fixed-width layout is exercised.
var usageNow = time.Date(2026, 11, 15, 12, 0, 0, 123_456_789, time.UTC)

func usageF(v float64) *float64 { return &v }
func usageI(v int64) *int64     { return &v }

// usageIngest runs one Ingest and fails the test on an error.
func usageIngest(t *testing.T, s *store.Store, r store.Reading, now time.Time) store.IngestResult {
	t.Helper()
	res, err := s.Ingest(context.Background(), r, now)
	if err != nil {
		t.Fatalf("Ingest(%+v): %v", r, err)
	}
	return res
}

// usageFull is a reading that carries every value, taken at at.
func usageFull(id string, at time.Time) store.Reading {
	return store.Reading{
		SessionID:       id,
		At:              at,
		CostUSD:         usageF(4.12),
		CacheExpiresAt:  at.Add(5 * time.Minute),
		CacheRebuildUSD: usageF(1.68),
		RecacheTokens:   usageI(120_000),
		Limits: []model.UsageSample{
			{Window: model.WindowFiveHour, UsedPct: 62, ResetsAt: at.Add(2 * time.Hour)},
			{Window: model.WindowSevenDay, UsedPct: 30.5, ResetsAt: at.Add(72 * time.Hour)},
		},
	}
}

// usageSession stores a live session id the way a hook does.
func usageSession(t *testing.T, s *store.Store, id string) model.Session {
	t.Helper()
	sess := model.Session{
		ID: id, Agent: "claude", Pane: "%7", State: model.StateWorking, LastUpdate: "Edit usage.go",
		StartedAt: usageNow.Add(-time.Hour), UpdatedAt: usageNow.Add(-time.Minute), SeenAt: usageNow.Add(-30 * time.Second),
	}
	upsert(t, s, sess)
	return sess
}

// usageRows reads every usage sample as stored, by n.
func usageRows(t *testing.T, path string) []model.UsageSample {
	t.Helper()
	rows, err := rawDB(t, path).Query(`SELECT n, at, window, used_pct, resets_at, session_id FROM usage_samples ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.UsageSample
	for rows.Next() {
		var u model.UsageSample
		var at string
		var resets sql.NullString
		if err := rows.Scan(&u.N, &at, &u.Window, &u.UsedPct, &resets, &u.SessionID); err != nil {
			t.Fatal(err)
		}
		u.At = decisionsTime(t, at)
		if resets.Valid {
			u.ResetsAt = decisionsTime(t, resets.String)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// costRows reads every cost sample as stored, by n.
func costRows(t *testing.T, path string) []model.CostSample {
	t.Helper()
	rows, err := rawDB(t, path).Query(`SELECT n, at, session_id, cost_usd FROM cost_samples ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.CostSample
	for rows.Next() {
		var c model.CostSample
		var at string
		if err := rows.Scan(&c.N, &at, &c.SessionID, &c.CostUSD); err != nil {
			t.Fatal(err)
		}
		c.At = decisionsTime(t, at)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// usageCostColumns is the session's four ingest columns as stored text
// (NULL as "NULL"), for byte-equal comparisons.
func usageCostColumns(t *testing.T, path, id string) string {
	t.Helper()
	var out string
	err := rawDB(t, path).QueryRow(`SELECT COALESCE(CAST(cost_usd AS TEXT), 'NULL') || '|' ||
  COALESCE(cache_expires_at, 'NULL') || '|' || COALESCE(CAST(cache_rebuild_usd AS TEXT), 'NULL') || '|' ||
  COALESCE(CAST(recache_tokens AS TEXT), 'NULL') FROM sessions WHERE id = ?`, id).Scan(&out)
	if err != nil {
		t.Fatalf("cost columns of %s: %v", id, err)
	}
	return out
}

// usageActivityColumns is what marks a session's activity, as stored text.
func usageActivityColumns(t *testing.T, path, id string) string {
	t.Helper()
	var out string
	err := rawDB(t, path).QueryRow(`SELECT COALESCE(updated_at, 'NULL') || '|' || COALESCE(seen_at, 'NULL') || '|' ||
  COALESCE(started_at, 'NULL') || '|' || state || '|' || COALESCE(last_update, 'NULL') FROM sessions WHERE id = ?`, id).Scan(&out)
	if err != nil {
		t.Fatalf("activity columns of %s: %v", id, err)
	}
	return out
}

func TestMigrationAddsCostTables(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	for _, obj := range [][2]string{
		{"table", "usage_samples"},
		{"table", "cost_samples"},
		{"index", "usage_samples_window"},
		{"index", "cost_samples_session"},
	} {
		var n int
		err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = ? AND name = ?`, obj[0], obj[1]).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s %s missing", obj[0], obj[1])
		}
	}
	for _, c := range []struct {
		table, col, typ string
		notNull         int
		dflt            sql.NullString
	}{
		{"tasks", "priority", "INTEGER", 1, sql.NullString{String: "2", Valid: true}},
		{"tasks", "agent", "TEXT", 1, sql.NullString{String: "'claude'", Valid: true}},
		{"decisions", "exported_at", "TEXT", 0, sql.NullString{}},
		{"sessions", "recache_tokens", "INTEGER", 0, sql.NullString{}}, // Ruling P5-R2
	} {
		var typ string
		var notNull int
		var dflt sql.NullString
		err := db.QueryRow(`SELECT type, "notnull", dflt_value FROM pragma_table_info(?) WHERE name = ?`, c.table, c.col).
			Scan(&typ, &notNull, &dflt)
		if err != nil {
			t.Fatalf("%s.%s: %v", c.table, c.col, err)
		}
		if typ != c.typ || notNull != c.notNull || dflt != c.dflt {
			t.Errorf("%s.%s = %s notnull=%d default %v, want %s notnull=%d default %v",
				c.table, c.col, typ, notNull, dflt, c.typ, c.notNull, c.dflt)
		}
	}
	if err := ftsIntegrity(db); err != nil {
		t.Errorf("decisions_fts integrity-check: %v", err)
	}
	if n := countMigrations(t, path); n != 7 {
		t.Errorf("schema_migrations has %d rows, want 7", n)
	}
}

// Plan 4's session statements, copied from its store.go, so this test
// keeps describing what a Plan 4 binary runs after this package changes.
const (
	plan4Upsert = `
INSERT INTO sessions (
  id, agent, tmux_pane, tmux_session, tmux_window, cwd, repo, branch, state, model,
  context_used, context_max, subagents_active, last_update, seen_at, transcript_path,
  started_at, updated_at, transcript_offset, task_id, worktree, tmux_socket, end_reason
) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)
ON CONFLICT(id) DO UPDATE SET
  agent = excluded.agent, tmux_pane = excluded.tmux_pane,
  tmux_session = excluded.tmux_session, tmux_window = excluded.tmux_window,
  cwd = excluded.cwd, repo = excluded.repo, branch = excluded.branch,
  state = excluded.state, model = excluded.model,
  context_used = excluded.context_used, context_max = excluded.context_max,
  subagents_active = excluded.subagents_active, last_update = excluded.last_update,
  seen_at = excluded.seen_at, transcript_path = excluded.transcript_path,
  started_at = excluded.started_at, updated_at = excluded.updated_at,
  transcript_offset = excluded.transcript_offset,
  task_id = excluded.task_id, worktree = excluded.worktree, tmux_socket = excluded.tmux_socket,
  end_reason = excluded.end_reason`
	plan4Select = `
SELECT COALESCE(id, ''), COALESCE(agent, ''), COALESCE(tmux_pane, ''), COALESCE(tmux_session, ''),
  COALESCE(tmux_window, ''), COALESCE(cwd, ''), COALESCE(repo, ''), COALESCE(branch, ''),
  COALESCE(state, ''), COALESCE(model, ''), COALESCE(context_used, 0), COALESCE(context_max, 0),
  COALESCE(subagents_active, 0), COALESCE(last_update, ''), seen_at, COALESCE(transcript_path, ''),
  started_at, updated_at, transcript_offset, COALESCE(task_id, ''), COALESCE(worktree, ''),
  COALESCE(team_id, ''), COALESCE(team_role, ''), COALESCE(team_member, ''), COALESCE(tmux_socket, ''),
  COALESCE(end_reason, '')
FROM sessions WHERE id = ?`
	// plan4OpenTickets is the session part of Plan 4's OpenTickets.
	plan4OpenTickets = `
SELECT d.id, COALESCE(s.state, ''), s.cache_expires_at, COALESCE(s.cache_rebuild_usd, 0)
FROM decisions d LEFT JOIN sessions s ON s.id = d.session_id WHERE d.status = 'open' ORDER BY d.n`
)

// plan4Session writes session p4, ended by a tmux kill, the way a Plan 4
// hook does.
func plan4Session(t *testing.T, db *sql.DB, state model.State, at string) {
	t.Helper()
	_, err := db.Exec(plan4Upsert, "p4", "claude", "%9", "work", "@4", "/src/curia", "/src/curia", "main",
		state, "claude-opus-5-5", 3000, 200000, 0, "Run tests", nil, "/tmp/t4.jsonl", at, at, 11, "", "",
		"/private/tmp/tmux-501/default", "other")
	if err != nil {
		t.Fatalf("Plan 4 upsert: %v", err)
	}
}

// plan4Database builds, at path, a database as Plan 4's binary leaves it:
// 0001 to both 0005 files applied, the Plan 1 to 3 rows of plan3Database,
// and a review, a push and an ended session with its socket and end reason
// written in Plan 4's form.
func plan4Database(t *testing.T, path, at string) *sql.DB {
	t.Helper()
	db := plan3Database(t, path, at)
	for _, name := range []string{"0005_reviews.sql", "0005_send_limits.sql"} {
		schema, err := os.ReadFile(filepath.Clean(filepath.Join("migrations", name)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(schema)); err != nil {
			t.Fatalf("building the Plan 4 database: %s: %v", name, err)
		}
		if _, err = db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO reviews (id, task_id, session_id, summary, tests_status, status, created_at, updated_at)
VALUES ('R-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM reviews), 'T-1', 'lead', 'Refresh fixed.', 'passed', 'pending', '` + at + `', '` + at + `')`,
		`INSERT INTO pushes (kind, ref, device, status, not_before, token_hash, created_at)
VALUES ('review', 'R-1', 'phone', 'pending', '` + at + `', 'abc', '` + at + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("building the Plan 4 database: %v", err)
		}
	}
	plan4Session(t, db, model.StateEnded, at)
	return db
}

func TestMigrationOnPlan4Database(t *testing.T) {
	path := dbPath(t)
	const at = "2026-11-14T14:00:05.000000000Z"
	db := plan4Database(t, path, at)

	s := open(t, path)
	if names := migrationNames(t, db); !reflect.DeepEqual(names, []string{
		"0001_init.sql", "0002_decisions.sql", "0003_typed_confirmation.sql", "0004_tasks.sql",
		"0005_reviews.sql", "0005_send_limits.sql", "0006_cost.sql",
	}) {
		t.Errorf("migrations = %v, want 0001 to 0006 (both 0005 files)", names)
	}
	var priority int
	var agent string
	if err := db.QueryRow(`SELECT priority, agent FROM tasks WHERE id = 'T-1'`).Scan(&priority, &agent); err != nil {
		t.Fatal(err)
	}
	if priority != 2 || agent != "claude" {
		t.Errorf("Plan 3 task after migration: priority %d agent %q, want 2 claude", priority, agent)
	}
	p4 := get(t, s, "p4")
	want := model.Session{
		ID: "p4", Agent: "claude", Pane: "%9", TmuxSession: "work", TmuxWindow: "@4", Cwd: "/src/curia",
		Repo: "/src/curia", Branch: "main", State: model.StateEnded, Model: "claude-opus-5-5",
		ContextUsed: 3000, ContextMax: 200000, LastUpdate: "Run tests", TranscriptPath: "/tmp/t4.jsonl",
		TranscriptOffset: 11, Socket: "/private/tmp/tmux-501/default", EndReason: "other",
		StartedAt: decisionsTime(t, at), UpdatedAt: decisionsTime(t, at),
	}
	if !p4.StartedAt.Equal(want.StartedAt) || !p4.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("times = %v %v, want %s", p4.StartedAt, p4.UpdatedAt, at)
	}
	p4.StartedAt, p4.UpdatedAt, want.StartedAt, want.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(p4, want) {
		t.Errorf("Plan 4 session after migration (no cost known):\n got %+v\nwant %+v", p4, want)
	}
	assertPlan4Rows(t, s, db)
	assertPlan3Rows(t, s, db, at)

	// The new schema works on the old rows.
	if res := usageIngest(t, s, usageFull("p4", usageNow), usageNow); res != (store.IngestResult{SessionFound: true, UsageAdded: 2, CostAdded: 1}) {
		t.Errorf("first ingest on the migrated file = %+v", res)
	}
	assertPlan4Runs(t, db, path, at)
}

// assertPlan4Rows checks the Plan 4 rows plan4Database wrote, other than
// the session, after the migration.
func assertPlan4Rows(t *testing.T, s *store.Store, db *sql.DB) {
	t.Helper()
	if r := getReview(t, s, "R-1"); r.Status != model.ReviewPending || r.Summary != "Refresh fixed." || r.TaskID != "T-1" {
		t.Errorf("review after migration: %+v", r)
	}
	if p, err := s.PushForTest(context.Background(), 1); err != nil || p.Ref != "R-1" || p.Status != model.PushPending {
		t.Errorf("push after migration: %+v, %v", p, err)
	}
	var exported sql.NullString
	if err := db.QueryRow(`SELECT exported_at FROM decisions WHERE id = 'D-1'`).Scan(&exported); err != nil || exported.Valid {
		t.Errorf("decision exported_at after migration = %v, %v; want NULL", exported, err)
	}
}

// assertPlan4Runs checks that a Plan 4 binary keeps running against the
// migrated schema, and that its blind upsert keeps what Ingest wrote.
func assertPlan4Runs(t *testing.T, db *sql.DB, path, at string) {
	t.Helper()
	before := usageCostColumns(t, path, "p4")
	plan4Session(t, db, model.StateWorking, at) // the ON CONFLICT path
	if after := usageCostColumns(t, path, "p4"); after != before {
		t.Errorf("Plan 4 upsert changed the cost columns: %s, want %s", after, before)
	}
	cols := make([]any, 26)
	for i := range cols {
		cols[i] = new(any)
	}
	var state, socket string
	cols[8], cols[24] = &state, &socket
	if err := db.QueryRow(plan4Select, "p4").Scan(cols...); err != nil {
		t.Fatalf("Plan 4 select on the new schema: %v", err)
	}
	if state != string(model.StateWorking) || socket != "/private/tmp/tmux-501/default" {
		t.Errorf("Plan 4 select: state %s socket %s", state, socket)
	}
	var id, sessState string
	var cache sql.NullString
	var rebuild float64
	if err := db.QueryRow(plan4OpenTickets).Scan(&id, &sessState, &cache, &rebuild); err != nil || id != "D-1" {
		t.Fatalf("Plan 4 open tickets on the new schema: %s, %v", id, err)
	}
	// A Plan 4 task insert names neither priority nor agent.
	if _, err := db.Exec(`INSERT INTO tasks (id, title, repo, status, created_at, updated_at)
VALUES ('T-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM tasks), 'Old binary task', '/src/curia', 'backlog', ?, ?)`, at, at); err != nil {
		t.Fatalf("Plan 4 task insert on the new schema: %v", err)
	}
	var priority int
	var agent string
	if err := db.QueryRow(`SELECT priority, agent FROM tasks WHERE title = 'Old binary task'`).Scan(&priority, &agent); err != nil || priority != 2 || agent != "claude" {
		t.Errorf("old binary's task: priority %d agent %q, %v; want 2 claude", priority, agent, err)
	}
	assertPlan3Runs(t, db, at)
}

func TestMigrationChecks(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	const at = "2026-11-15T09:00:00.000000000Z"
	usage := func(window string, pct float64) error {
		_, err := db.Exec(`INSERT INTO usage_samples (at, window, used_pct) VALUES (?, ?, ?)`, at, window, pct)
		return err
	}
	cost := func(v float64) error {
		_, err := db.Exec(`INSERT INTO cost_samples (at, session_id, cost_usd) VALUES (?, 's1', ?)`, at, v)
		return err
	}
	task := func(priority int, agent string) error {
		_, err := db.Exec(`INSERT INTO tasks (id, title, repo, status, created_at, updated_at, priority, agent)
VALUES ('T-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM tasks), 'x', '/src/curia', 'backlog', ?, ?, ?, ?)`, at, at, priority, agent)
		return err
	}
	for _, c := range []struct {
		name string
		err  error
		ok   bool
	}{
		{"window one_hour", usage("one_hour", 10), false},
		{"used_pct 101", usage("five_hour", 101), false},
		{"used_pct -1", usage("five_hour", -1), false},
		{"used_pct 0", usage("five_hour", 0), true},
		{"used_pct 100 seven_day", usage("seven_day", 100), true},
		{"cost_usd -1", cost(-1), false},
		{"cost_usd 0", cost(0), true},
		{"priority 4", task(4, "claude"), false},
		{"priority 0", task(0, "claude"), false},
		{"agent gemini", task(2, "gemini"), false},
		{"priority 1 codex", task(1, "codex"), true},
		{"priority 3 claude", task(3, "claude"), true},
	} {
		if (c.err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.name, c.err, c.ok)
		}
	}
	if _, err := db.Exec(`INSERT INTO usage_samples (at, used_pct) VALUES (?, 1)`, at); err == nil {
		t.Error("usage sample without a window was stored")
	}
	if _, err := db.Exec(`INSERT INTO cost_samples (at, cost_usd) VALUES (?, 1)`, at); err == nil {
		t.Error("cost sample without a session was stored")
	}
}

func TestSessionCostAndCacheRoundTrip(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	usageSession(t, s, "s1")
	usageSession(t, s, "s2")
	usageSession(t, s, "s3")
	r := usageFull("s1", usageNow)
	usageIngest(t, s, r, usageNow)
	// The start-up status line carries a cost of 0, which is known.
	usageIngest(t, s, store.Reading{SessionID: "s3", At: usageNow, CostUSD: usageF(0)}, usageNow)

	list, err := s.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(list); !reflect.DeepEqual(got, []string{"s1", "s2", "s3"}) {
		t.Fatalf("ListSessions = %v", got)
	}
	for k, want := range []usageCost{
		{4.12, true, r.CacheExpiresAt, 1.68, 120_000},
		{}, // never ingested: no cost known, a zero expiry
		{0, true, time.Time{}, 0, 0},
	} {
		for _, got := range []model.Session{get(t, s, list[k].ID), list[k]} {
			usageAssertCost(t, got, want)
			if got.State != model.StateWorking || got.LastUpdate != "Edit usage.go" {
				t.Errorf("%s hook fields = %s %q", got.ID, got.State, got.LastUpdate)
			}
		}
	}
}

// usageCost is a session's four ingest values and CostKnown.
type usageCost struct {
	cost    float64
	known   bool
	cache   time.Time
	rebuild float64
	recache int64
}

func usageAssertCost(t *testing.T, got model.Session, want usageCost) {
	t.Helper()
	g := usageCost{got.CostUSD, got.CostKnown, got.CacheExpiresAt, got.CacheRebuildUSD, got.RecacheTokens}
	if g.cost != want.cost || g.known != want.known || !g.cache.Equal(want.cache) || g.rebuild != want.rebuild || g.recache != want.recache {
		t.Errorf("%s cost values = %+v, want %+v", got.ID, g, want)
	}
}

func TestOldUpsertKeepsCostColumns(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	sess := usageSession(t, s, "s1")
	usageIngest(t, s, usageFull("s1", usageNow), usageNow)
	want := usageCostColumns(t, path, "s1")
	if want == "NULL|NULL|NULL|NULL" {
		t.Fatalf("Ingest wrote nothing: %s", want)
	}

	// A hook's snapshot, even one that carries other cost values, never
	// writes the four columns.
	sess.State, sess.CostUSD, sess.CostKnown, sess.CacheRebuildUSD, sess.RecacheTokens = model.StateIdle, 99, true, 9, 9
	sess.CacheExpiresAt = usageNow.Add(time.Hour)
	upsert(t, s, sess)
	if got := usageCostColumns(t, path, "s1"); got != want {
		t.Errorf("after UpsertSession: %s, want %s", got, want)
	}
	err := s.UpdateSession(ctx, "s1", func(cur model.Session, _ bool) model.Session {
		cur.State, cur.LastUpdate = model.StateWaiting, "Bash: go test"
		cur.CostUSD, cur.CostKnown, cur.CacheExpiresAt, cur.CacheRebuildUSD, cur.RecacheTokens = 0, false, time.Time{}, 0, 0
		return cur
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := usageCostColumns(t, path, "s1"); got != want {
		t.Errorf("after UpdateSession: %s, want %s", got, want)
	}
	if got := get(t, s, "s1"); got.State != model.StateWaiting || got.CostUSD != 4.12 || got.RecacheTokens != 120_000 {
		t.Errorf("after both: state %s cost %v recache %d", got.State, got.CostUSD, got.RecacheTokens)
	}
	// Plan 1's upsert too.
	plan1Session(t, rawDB(t, path), "s1", "%7", model.StateIdle, "2026-11-15T12:00:00.000000000Z")
	if got := usageCostColumns(t, path, "s1"); got != want {
		t.Errorf("after Plan 1's upsert: %s, want %s", got, want)
	}
}

func TestIngestWritesSessionAndSamples(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	usageSession(t, s, "s1")
	r := usageFull("s1", usageNow.Add(-10*time.Second))
	res := usageIngest(t, s, r, usageNow)
	if want := (store.IngestResult{SessionFound: true, UsageAdded: 2, CostAdded: 1, Pruned: 0}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	usageAssertCost(t, get(t, s, "s1"), usageCost{4.12, true, r.CacheExpiresAt, 1.68, 120_000})
	usageAssertSamples(t, path, r)
	// The stored text is the fixed-width layout.
	var at string
	if err := rawDB(t, path).QueryRow(`SELECT at FROM cost_samples`).Scan(&at); err != nil || at != "2026-11-15T11:59:50.123456789Z" {
		t.Errorf("stored at = %q, %v", at, err)
	}
}

// usageAssertSamples checks that the store holds exactly r's samples: its
// cost and each of its windows, at r.At from r.SessionID.
func usageAssertSamples(t *testing.T, path string, r store.Reading) {
	t.Helper()
	costs := costRows(t, path)
	if len(costs) != 1 || !costs[0].At.Equal(r.At) || costs[0].SessionID != r.SessionID || costs[0].CostUSD != *r.CostUSD {
		t.Errorf("cost samples = %+v, want one of %v at %v", costs, *r.CostUSD, r.At)
	}
	usage := usageRows(t, path)
	if len(usage) != len(r.Limits) {
		t.Fatalf("usage samples = %+v, want %d", usage, len(r.Limits))
	}
	for i, l := range r.Limits {
		u := usage[i]
		if !u.At.Equal(r.At) || u.SessionID != r.SessionID || u.Window != l.Window || u.UsedPct != l.UsedPct || !u.ResetsAt.Equal(l.ResetsAt) {
			t.Errorf("usage sample %d = %+v, want %+v at %v from %s", i, u, l, r.At, r.SessionID)
		}
	}
}

func TestIngestDefaultsAtToNow(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	usageSession(t, s, "s1")
	r := usageFull("s1", usageNow)
	r.At = time.Time{}
	usageIngest(t, s, r, usageNow)
	if costs := costRows(t, path); len(costs) != 1 || !costs[0].At.Equal(usageNow) {
		t.Errorf("cost samples = %+v, want one at now", costs)
	}
	if usage := usageRows(t, path); len(usage) != 2 || !usage[0].At.Equal(usageNow) || !usage[1].At.Equal(usageNow) {
		t.Errorf("usage samples = %+v, want two at now", usage)
	}
}

func TestIngestAbsentFieldsChangeNothing(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	usageSession(t, s, "s1")
	first := usageFull("s1", usageNow)
	usageIngest(t, s, first, usageNow)

	res := usageIngest(t, s, store.Reading{SessionID: "s1", At: usageNow.Add(time.Second), CostUSD: usageF(5)}, usageNow.Add(time.Second))
	if want := (store.IngestResult{SessionFound: true, CostAdded: 1}); res != want {
		t.Errorf("cost-only result = %+v, want %+v", res, want)
	}
	got := get(t, s, "s1")
	if got.CostUSD != 5 || !got.CacheExpiresAt.Equal(first.CacheExpiresAt) || got.CacheRebuildUSD != 1.68 || got.RecacheTokens != 120_000 {
		t.Errorf("after a cost-only reading: %+v", got)
	}

	before := usageCostColumns(t, path, "s1")
	nCost, nUsage := len(costRows(t, path)), len(usageRows(t, path))
	res = usageIngest(t, s, store.Reading{SessionID: "s1", At: usageNow.Add(2 * time.Second)}, usageNow.Add(2*time.Second))
	if want := (store.IngestResult{SessionFound: true}); res != want {
		t.Errorf("empty result = %+v, want %+v", res, want)
	}
	if after := usageCostColumns(t, path, "s1"); after != before {
		t.Errorf("an empty reading changed the session: %s, want %s", after, before)
	}
	if len(costRows(t, path)) != nCost || len(usageRows(t, path)) != nUsage {
		t.Error("an empty reading added a sample")
	}
	// A reading with only the cache keeps the cost.
	usageIngest(t, s, store.Reading{SessionID: "s1", At: usageNow.Add(3 * time.Second), RecacheTokens: usageI(7)}, usageNow.Add(3*time.Second))
	if got := get(t, s, "s1"); got.CostUSD != 5 || got.RecacheTokens != 7 || got.CacheRebuildUSD != 1.68 {
		t.Errorf("after a recache-only reading: %+v", got)
	}
}

func TestIngestIsNotActivity(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	usageSession(t, s, "s1")
	before := usageActivityColumns(t, path, "s1")
	usageIngest(t, s, usageFull("s1", usageNow), usageNow)
	usageIngest(t, s, store.Reading{SessionID: "s1", At: usageNow.Add(time.Minute), CostUSD: usageF(9)}, usageNow.Add(time.Minute))
	if after := usageActivityColumns(t, path, "s1"); after != before {
		t.Errorf("activity columns = %s, want %s", after, before)
	}
}

func TestIngestUnknownSessionKeepsUsageOnly(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	res := usageIngest(t, s, usageFull("ghost", usageNow), usageNow)
	if want := (store.IngestResult{SessionFound: false, UsageAdded: 2}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
	if _, found, err := s.GetSession(context.Background(), "ghost"); err != nil || found {
		t.Errorf("GetSession(ghost) found %v, %v; want no row", found, err)
	}
	if costs := costRows(t, path); len(costs) != 0 {
		t.Errorf("cost samples = %+v, want none", costs)
	}
	usage := usageRows(t, path)
	if len(usage) != 2 || usage[0].SessionID != "ghost" || usage[1].SessionID != "ghost" {
		t.Errorf("usage samples = %+v, want both windows from ghost", usage)
	}
}

func TestIngestSkipsRepeatedReading(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	usageSession(t, s, "s1")
	resets := usageNow.Add(2 * time.Hour)
	reading := func(at time.Time, cost, pct float64, resets time.Time) store.Reading {
		return store.Reading{
			SessionID: "s1", At: at, CostUSD: usageF(cost),
			Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: pct, ResetsAt: resets}},
		}
	}
	for _, c := range []struct {
		name         string
		after        time.Duration
		cost, pct    float64
		resets       time.Time
		usage, cost2 int
	}{
		{"first", 0, 1, 40, resets, 1, 1},
		{"same 30 s later", 30 * time.Second, 1, 40, resets, 0, 0},
		{"same 61 s later", 61 * time.Second, 1, 40, resets, 1, 1},
		{"used_pct changed 5 s later", 66 * time.Second, 1, 41, resets, 1, 0},
		{"cost changed 5 s later", 71 * time.Second, 2, 41, resets, 0, 1},
		{"resets_at changed 1 s later", 72 * time.Second, 2, 41, resets.Add(time.Hour), 1, 0},
		{"resets_at gone 1 s later", 73 * time.Second, 2, 41, time.Time{}, 1, 0},
		{"no reset, same 1 s later", 74 * time.Second, 2, 41, time.Time{}, 0, 0},
		{"an older reading with the same values", 10 * time.Second, 2, 41, time.Time{}, 0, 0},
	} {
		at := usageNow.Add(c.after)
		res := usageIngest(t, s, reading(at, c.cost, c.pct, c.resets), usageNow.Add(80*time.Second))
		if res.UsageAdded != c.usage || res.CostAdded != c.cost2 {
			t.Errorf("%s: added usage %d cost %d, want %d %d", c.name, res.UsageAdded, res.CostAdded, c.usage, c.cost2)
		}
	}
	if n := len(usageRows(t, path)); n != 5 {
		t.Errorf("usage samples = %d, want 5", n)
	}
	if n := len(costRows(t, path)); n != 3 {
		t.Errorf("cost samples = %d, want 3", n)
	}
}

// The repeat interval at its boundary: an identical reading less than
// SampleRepeat after the newest sample adds nothing, one at exactly
// SampleRepeat adds a row.
func TestIngestRepeatBoundary(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	usageSession(t, s, "s1")
	r := usageFull("s1", usageNow)
	now := usageNow.Add(2 * store.SampleRepeat)
	usageIngest(t, s, r, now)
	r.At = usageNow.Add(store.SampleRepeat - time.Nanosecond)
	if res := usageIngest(t, s, r, now); res.UsageAdded != 0 || res.CostAdded != 0 {
		t.Errorf("1 ns inside the interval: %+v, want nothing added", res)
	}
	r.At = usageNow.Add(store.SampleRepeat)
	if res := usageIngest(t, s, r, now); res.UsageAdded != 2 || res.CostAdded != 1 {
		t.Errorf("at the interval: %+v, want 2 usage and 1 cost", res)
	}
	if store.SampleRepeat != 60*time.Second {
		t.Errorf("SampleRepeat = %v, want 60s", store.SampleRepeat)
	}
}

// A burst of identical status-line readings from several processes at
// once (probe S1: about three a second, for every live session) adds one
// row per series: the repeat rule is enforced inside the transaction.
func TestIngestBurstOfIdenticalReadings(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	usageSession(t, hs[0], "s1")
	const burst = 50
	results := make([]store.IngestResult, burst)
	errs := make([]error, burst)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range burst {
		wg.Go(func() {
			<-start
			r := usageFull("s1", usageNow) // the same values, read at different instants
			r.At = usageNow.Add(time.Duration(i) * 100 * time.Millisecond)
			errs[i] = retryBusy(func() error {
				var err error
				results[i], err = hs[i%len(hs)].Ingest(context.Background(), r, usageNow.Add(10*time.Second))
				return err
			}, busyAtBegin)
		})
	}
	close(start)
	wg.Wait()
	usage, cost := 0, 0
	for i := range burst {
		if errs[i] != nil {
			t.Fatalf("ingest %d: %v", i, errs[i])
		}
		usage += results[i].UsageAdded
		cost += results[i].CostAdded
	}
	if usage != 2 || cost != 1 {
		t.Errorf("results added usage %d cost %d, want 2 and 1", usage, cost)
	}
	if n, m := len(usageRows(t, path)), len(costRows(t, path)); n != 2 || m != 1 {
		t.Errorf("stored usage %d cost %d, want 2 and 1", n, m)
	}
}

// usageOldRows inserts count usage samples (alternating windows) and cost
// samples of session id, the newest at newest and each one second older
// than the one before.
func usageOldRows(t *testing.T, db *sql.DB, table, id string, count int, newest time.Time) {
	t.Helper()
	for i := range count {
		at := newest.Add(-time.Duration(i) * time.Second).UTC().Format("2006-01-02T15:04:05.000000000Z")
		var err error
		if table == "usage_samples" {
			window := model.WindowFiveHour
			if i%2 == 1 {
				window = model.WindowSevenDay
			}
			_, err = db.Exec(`INSERT INTO usage_samples (at, window, used_pct, session_id) VALUES (?, ?, ?, ?)`, at, window, float64(i%100), id)
		} else {
			_, err = db.Exec(`INSERT INTO cost_samples (at, session_id, cost_usd) VALUES (?, ?, ?)`, at, id, float64(i))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// usageFresh is the samples taken in the last two hours.
func usageFresh(t *testing.T, path string) (u []model.UsageSample, c []model.CostSample) {
	t.Helper()
	for _, x := range usageRows(t, path) {
		if x.At.After(usageNow.Add(-2 * time.Hour)) {
			u = append(u, x)
		}
	}
	for _, x := range costRows(t, path) {
		if x.At.After(usageNow.Add(-2 * time.Hour)) {
			c = append(c, x)
		}
	}
	return u, c
}

// usageOldestFirst orders samples by at, then n.
func usageOldestFirst(rows []model.UsageSample) []model.UsageSample {
	out := slices.Clone(rows)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].N < out[j].N
	})
	return out
}

func TestIngestPrunesBounded(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	db := rawDB(t, path)
	usageSession(t, s, "s1")
	usageOldRows(t, db, "usage_samples", "s1", 500, usageNow.Add(-9*24*time.Hour))
	usageOldRows(t, db, "cost_samples", "s1", 300, usageNow.Add(-2*24*time.Hour))
	usageOldRows(t, db, "usage_samples", "s1", 10, usageNow.Add(-time.Hour)) // fresh, both windows
	usageOldRows(t, db, "cost_samples", "s1", 10, usageNow.Add(-time.Hour))  // fresh
	freshUsage, freshCost := usageFresh(t, path)
	before := usageRows(t, path)
	res := usageIngest(t, s, store.Reading{SessionID: "s1", At: usageNow}, usageNow)
	if res.Pruned != store.PruneBatch || store.PruneBatch != 200 {
		t.Fatalf("pruned %d, want %d (200)", res.Pruned, store.PruneBatch)
	}
	after := usageRows(t, path)
	if want := usageOldestFirst(before)[200:]; !reflect.DeepEqual(usageOldestFirst(after), want) {
		t.Errorf("the first prune did not delete exactly the 200 oldest usage samples: %d left, want %d", len(after), len(want))
	}
	if n := len(costRows(t, path)); n != 310 {
		t.Errorf("cost samples after the first prune = %d, want 310 (usage samples go first)", n)
	}

	// Later ingests take the rest, at most 200 rows each, usage first.
	var pruned []int
	for range 10 {
		res := usageIngest(t, s, store.Reading{SessionID: "s1", At: usageNow}, usageNow)
		if res.Pruned == 0 {
			break
		}
		pruned = append(pruned, res.Pruned)
	}
	if want := []int{200, 200, 200}; !reflect.DeepEqual(pruned, want) { // 300 usage + 300 cost - 0 kept
		t.Errorf("later prunes = %v, want %v", pruned, want)
	}
	u, c := usageFresh(t, path)
	if !reflect.DeepEqual(u, freshUsage) || !reflect.DeepEqual(c, freshCost) {
		t.Errorf("fresh rows changed:\n usage %+v\n want %+v\n cost %+v\n want %+v", u, freshUsage, c, freshCost)
	}
	if n, m := len(usageRows(t, path)), len(costRows(t, path)); n != 10 || m != 10 {
		t.Errorf("left usage %d cost %d, want the 10 fresh of each", n, m)
	}
	if store.UsageKeep != 8*24*time.Hour || store.CostKeep != 24*time.Hour {
		t.Errorf("UsageKeep %v CostKeep %v, want 192h and 24h", store.UsageKeep, store.CostKeep)
	}
}

// Pruning deletes exactly what is older than its keep, and never the
// newest reading of a window (usage), however old, nor the newest reading
// of a session (cost: CostSince's baseline) until it is older than
// BaselineKeep.
func TestIngestPruneKeepsNewestReading(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	db := rawDB(t, path)
	usageSession(t, s, "s1")
	usageSession(t, s, "s2")
	// s1 has only old cost samples; s2 old ones and one fresh one.
	usageOldRows(t, db, "cost_samples", "s1", 3, usageNow.Add(-2*24*time.Hour))
	usageOldRows(t, db, "cost_samples", "s2", 3, usageNow.Add(-2*24*time.Hour))
	usageOldRows(t, db, "cost_samples", "s2", 1, usageNow.Add(-time.Hour))
	// Exactly at the keep is not older than it.
	usageOldRows(t, db, "cost_samples", "s3", 1, usageNow.Add(-store.CostKeep))
	usageOldRows(t, db, "cost_samples", "s3", 1, usageNow.Add(-time.Minute))
	// A baseline outlives CostKeep by BaselineKeep, and no longer: s4's
	// newest is just past it, s5's exactly at it.
	usageOldRows(t, db, "cost_samples", "s4", 2, usageNow.Add(-store.BaselineKeep-time.Nanosecond))
	usageOldRows(t, db, "cost_samples", "s5", 2, usageNow.Add(-store.BaselineKeep))
	for _, q := range []struct {
		window string
		at     time.Time
	}{
		{"seven_day", usageNow.Add(-10 * 24 * time.Hour)},
		{"seven_day", usageNow.Add(-9 * 24 * time.Hour)}, // the window's newest
		{"five_hour", usageNow.Add(-9 * 24 * time.Hour)},
		{"five_hour", usageNow.Add(-store.UsageKeep)}, // at the keep
		{"five_hour", usageNow.Add(-store.UsageKeep - time.Nanosecond)},
		{"five_hour", usageNow.Add(-time.Hour)},
	} {
		if _, err := db.Exec(`INSERT INTO usage_samples (at, window, used_pct) VALUES (?, ?, 1)`,
			q.at.UTC().Format("2006-01-02T15:04:05.000000000Z"), q.window); err != nil {
			t.Fatal(err)
		}
	}
	res := usageIngest(t, s, store.Reading{SessionID: "nobody", At: usageNow}, usageNow)
	if res.Pruned != 11 { // s1: 2, s2: 3, s4: 2, s5: 1, seven_day: 1, five_hour: 2
		t.Errorf("pruned %d, want 11", res.Pruned)
	}
	got := map[string][]time.Time{}
	for _, c := range costRows(t, path) {
		got[c.SessionID] = append(got[c.SessionID], c.At)
	}
	want := map[string][]time.Time{
		"s1": {usageNow.Add(-2 * 24 * time.Hour)},
		"s2": {usageNow.Add(-time.Hour)},
		"s3": {usageNow.Add(-store.CostKeep), usageNow.Add(-time.Minute)},
		"s5": {usageNow.Add(-store.BaselineKeep)},
	}
	if !reflect.DeepEqual(usageTimesUTC(got), usageTimesUTC(want)) {
		t.Errorf("cost samples left = %v, want %v", got, want)
	}
	var left []string
	for _, u := range usageRows(t, path) {
		left = append(left, fmt.Sprintf("%s %s", u.Window, usageNow.Sub(u.At)))
	}
	if want := []string{"seven_day 216h0m0s", "five_hour 192h0m0s", "five_hour 1h0m0s"}; !reflect.DeepEqual(left, want) {
		t.Errorf("usage samples left = %v, want %v", left, want)
	}
	if res := usageIngest(t, s, store.Reading{SessionID: "nobody", At: usageNow}, usageNow); res.Pruned != 0 {
		t.Errorf("a second prune deleted %d, want 0", res.Pruned)
	}
	// s1's old newest sample is its baseline.
	costs, err := s.CostSince(context.Background(), usageNow.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(costs) == 0 || costs[0].SessionID != "s1" || !costs[0].At.Equal(usageNow.Add(-2*24*time.Hour)) {
		t.Errorf("CostSince = %+v, want s1's old sample first", costs)
	}
}

func usageTimesUTC(m map[string][]time.Time) map[string][]string {
	out := map[string][]string{}
	for k, ts := range m {
		for _, ts := range ts {
			out[k] = append(out[k], ts.UTC().Format(time.RFC3339Nano))
		}
	}
	return out
}

// The cost prune walks PruneWalk sessions per ingest from a cursor, so
// every session's old samples go in turn, however many sessions there are,
// and one ingest never touches more than PruneWalk sessions.
func TestIngestPruneWalksEverySession(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	db := rawDB(t, path)
	sessions := 3*store.PruneWalk + 5
	for i := range sessions {
		id := fmt.Sprintf("s%03d", i)
		usageOldRows(t, db, "cost_samples", id, 2, usageNow.Add(-2*24*time.Hour)) // one goes, the newest stays
	}
	var pruned []int
	for range 10 {
		pruned = append(pruned, usageIngest(t, s, store.Reading{SessionID: "nobody", At: usageNow}, usageNow).Pruned)
	}
	// Four walks cover every session: 32, 32, 32 and the last 5, then
	// the cursor starts again and finds nothing left.
	want := []int{store.PruneWalk, store.PruneWalk, store.PruneWalk, 5, 0, 0, 0, 0, 0, 0}
	if !reflect.DeepEqual(pruned, want) {
		t.Errorf("pruned per ingest = %v, want %v", pruned, want)
	}
	per := map[string]int{}
	for _, c := range costRows(t, path) {
		per[c.SessionID]++
	}
	if len(per) != sessions {
		t.Errorf("%d sessions keep samples, want all %d", len(per), sessions)
	}
	for id, n := range per {
		if n != 1 {
			t.Errorf("%s keeps %d samples, want its newest only", id, n)
		}
	}
	if store.PruneWalk != 32 {
		t.Errorf("PruneWalk = %d, want 32", store.PruneWalk)
	}
}

// A hook's UpdateSession and an Ingest on the same row, racing in four
// processes' worth of handles: neither loses the other's columns, the hook
// is never refused, and each round's identical readings add one sample.
func TestIngestSurvivesConcurrentHook(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 4)
	usageSession(t, hs[0], "s1")
	ctx := context.Background()
	const goroutines, rounds = 8, 50
	var seq atomic.Int64
	for round := range rounds {
		at := usageNow.Add(time.Duration(round) * store.SampleRepeat)
		errs := make([]error, goroutines)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Go(func() {
				<-start
				h := hs[g%len(hs)]
				if g%2 == 0 {
					errs[g] = retryBusy(func() error {
						return h.UpdateSession(ctx, "s1", func(cur model.Session, _ bool) model.Session {
							n := seq.Add(1) // under the write lock: commit order
							cur.State = []model.State{model.StateWorking, model.StateIdle}[n%2]
							cur.LastUpdate = fmt.Sprintf("hook %d", n)
							cur.UpdatedAt = at
							return cur
						})
					}, busyBeforeAnything)
					return
				}
				r := store.Reading{
					SessionID: "s1", At: at, CostUSD: usageF(float64(round + 1)),
					Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: float64(round)}},
				}
				errs[g] = retryBusy(func() error {
					_, err := h.Ingest(ctx, r, at)
					return err
				}, busyAtBegin)
			})
		}
		close(start)
		wg.Wait()
		for g, err := range errs {
			if err != nil {
				t.Fatalf("round %d goroutine %d: %v", round, g, err)
			}
		}
	}
	got := get(t, hs[1], "s1")
	last := seq.Load()
	if got.LastUpdate != fmt.Sprintf("hook %d", last) || got.State != []model.State{model.StateWorking, model.StateIdle}[last%2] {
		t.Errorf("final hook fields = %s %q, want the last hook's (%d)", got.State, got.LastUpdate, last)
	}
	if !got.CostKnown || got.CostUSD != rounds {
		t.Errorf("final cost = %v known %v, want %d (the highest)", got.CostUSD, got.CostKnown, rounds)
	}
	if n := len(costRows(t, path)); n != rounds {
		t.Errorf("cost samples = %d, want one per round (%d)", n, rounds)
	}
	if n := len(usageRows(t, path)); n != rounds {
		t.Errorf("usage samples = %d, want one per round (%d)", n, rounds)
	}
}

func TestConcurrentIngestsMergeIntoOneSeries(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 3)
	sessions := []string{"s1", "s2", "s3"}
	const each = 10
	errs := make([]error, len(sessions))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for k, id := range sessions {
		wg.Go(func() {
			<-start
			for i := range each {
				// The same instant from every session: the order falls to n.
				r := store.Reading{
					SessionID: id, At: usageNow.Add(time.Duration(i) * time.Second),
					Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: float64(10*k + i)}},
				}
				err := retryBusy(func() error {
					_, err := hs[k].Ingest(context.Background(), r, usageNow.Add(time.Minute))
					return err
				}, busyAtBegin)
				if err != nil {
					errs[k] = err
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	for k, err := range errs {
		if err != nil {
			t.Fatalf("%s: %v", sessions[k], err)
		}
	}
	got, err := hs[0].UsageSince(context.Background(), usageNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(sessions)*each {
		t.Fatalf("UsageSince = %d samples, want %d", len(got), len(sessions)*each)
	}
	if !reflect.DeepEqual(got, usageOldestFirst(got)) || !usageDistinctN(got) {
		t.Errorf("UsageSince is not ordered by at then n: %+v", got)
	}
	per := map[string]int{}
	for _, u := range got {
		per[u.SessionID]++
	}
	if !reflect.DeepEqual(per, map[string]int{"s1": each, "s2": each, "s3": each}) {
		t.Errorf("samples per session = %v", per)
	}
}

// usageDistinctN reports whether no two samples share an n.
func usageDistinctN(rows []model.UsageSample) bool {
	seen := map[int64]bool{}
	for _, u := range rows {
		if seen[u.N] {
			return false
		}
		seen[u.N] = true
	}
	return true
}

func TestUsageSinceOrderAndBound(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	five := func(pct float64, resets time.Time) []model.UsageSample {
		return []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: pct, ResetsAt: resets}}
	}
	now := usageNow.Add(time.Hour)
	resets := usageNow.Add(3 * time.Hour)
	for _, r := range []store.Reading{
		{SessionID: "a", At: usageNow.Add(10 * time.Minute), Limits: five(3, resets)},
		{SessionID: "a", At: usageNow, Limits: five(1, resets)},
		{SessionID: "b", At: usageNow.Add(5 * time.Minute), Limits: []model.UsageSample{
			{Window: model.WindowFiveHour, UsedPct: 2}, {Window: model.WindowSevenDay, UsedPct: 50, ResetsAt: usageNow.Add(48 * time.Hour)},
		}},
		{SessionID: "a", At: usageNow.Add(20 * time.Minute), Limits: five(4, resets)},
	} {
		usageIngest(t, s, r, now)
	}
	got, err := s.UsageSince(ctx, usageNow.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, u := range got {
		resets := "none"
		if !u.ResetsAt.IsZero() {
			resets = u.ResetsAt.Sub(usageNow).String()
		}
		rows = append(rows, fmt.Sprintf("%s %s %v %s %s", u.At.Sub(usageNow), u.Window, u.UsedPct, resets, u.SessionID))
	}
	want := []string{
		"5m0s five_hour 2 none b",
		"5m0s seven_day 50 48h0m0s b",
		"10m0s five_hour 3 3h0m0s a",
		"20m0s five_hour 4 3h0m0s a",
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("UsageSince = %v, want %v", rows, want)
	}
	if got[0].N >= got[1].N {
		t.Errorf("same at: n %d then %d, want rising", got[0].N, got[1].N)
	}
	all, err := s.UsageSince(ctx, time.Time{})
	if err != nil || len(all) != 5 {
		t.Errorf("UsageSince(zero) = %d samples, %v; want all 5", len(all), err)
	}
	none, err := s.UsageSince(ctx, usageNow.Add(21*time.Minute))
	if err != nil || len(none) != 0 {
		t.Errorf("UsageSince(after the last) = %+v, %v; want none", none, err)
	}
}

func TestCostSinceIncludesBaseline(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		usageSession(t, s, id)
	}
	for _, c := range []struct {
		id   string
		ago  time.Duration
		cost float64
	}{
		{"s1", 40 * time.Minute, 1},
		{"s1", 25 * time.Minute, 2},
		{"s1", 5 * time.Minute, 3},
		{"s2", 30 * time.Minute, 7}, // only a baseline
		{"s3", 10 * time.Minute, 9}, // no baseline
		{"s4", 50 * time.Minute, 4}, // two at one instant before since:
		{"s4", 50 * time.Minute, 5}, // the later row is the baseline
		{"s4", 20 * time.Minute, 6}, // exactly at since: in
	} {
		usageIngest(t, s, store.Reading{SessionID: c.id, At: usageNow.Add(-c.ago), CostUSD: usageF(c.cost)}, usageNow)
	}
	got, err := s.CostSince(ctx, usageNow.Add(-20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, c := range got {
		rows = append(rows, fmt.Sprintf("%s %s %v", c.SessionID, usageNow.Sub(c.At), c.CostUSD))
	}
	want := []string{"s1 25m0s 2", "s1 5m0s 3", "s2 30m0s 7", "s3 10m0s 9", "s4 50m0s 5", "s4 20m0s 6"}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("CostSince = %v, want %v", rows, want)
	}
	all, err := s.CostSince(ctx, time.Time{})
	if err != nil || len(all) != 8 {
		t.Errorf("CostSince(zero) = %d samples, %v; want all 8", len(all), err)
	}
}

func TestTicketsSeeIngestedCache(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	usageSession(t, s, "a")
	usageSession(t, s, "b")
	older := create(t, s, ticket(t, "Which queue backend?", model.ModeBlocking, "a", t0))
	newer := create(t, s, ticket(t, "Which cache backend?", model.ModeBlocking, "b", t0.Add(time.Minute)))
	now := t0.Add(2 * time.Minute)

	order := func() []model.Ticket {
		t.Helper()
		ts, err := s.OpenTickets(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return decision.Triage(ts, now)
	}
	if got := order(); len(got) != 2 || got[0].ID != older.ID {
		t.Fatalf("before the ingest: %v, want the older ticket first", got)
	}
	expires := now.Add(5 * time.Minute)
	usageIngest(t, s, store.Reading{
		SessionID: "b", At: now, CacheExpiresAt: expires,
		CacheRebuildUSD: usageF(0.42), RecacheTokens: usageI(90_000),
	}, now)
	got := order()
	if len(got) != 2 || got[0].ID != newer.ID {
		t.Fatalf("after the ingest: %v, want the warm ticket %s first", got, newer.ID)
	}
	if !got[0].CacheExpiresAt.Equal(expires) || got[0].CacheRebuildUSD != 0.42 || got[0].RecacheTokens != 90_000 {
		t.Errorf("warm ticket = cache %v rebuild %v recache %d", got[0].CacheExpiresAt, got[0].CacheRebuildUSD, got[0].RecacheTokens)
	}
	if !got[1].CacheExpiresAt.IsZero() || got[1].RecacheTokens != 0 {
		t.Errorf("cold ticket = cache %v recache %d, want unknown", got[1].CacheExpiresAt, got[1].RecacheTokens)
	}
}

func TestIngestOnClosedStoreIsAnError(t *testing.T) {
	s, err := store.Open(dbPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Ingest(ctx, usageFull("s1", usageNow), usageNow); err == nil {
		t.Error("Ingest on a closed store: no error")
	}
	if _, err := s.UsageSince(ctx, usageNow); err == nil {
		t.Error("UsageSince on a closed store: no error")
	}
	if _, err := s.CostSince(ctx, usageNow); err == nil {
		t.Error("CostSince on a closed store: no error")
	}
}

// Values come from other programs: the store checks them again and
// refuses a reading with any value out of the ingest contract's range,
// writing nothing, rather than store it.
func TestIngestValidatesAtTheStore(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	usageSession(t, s, "s1")
	now := usageNow
	limit := func(w model.UsageWindow, pct float64, resets time.Time) store.Reading {
		return store.Reading{SessionID: "s1", At: now, Limits: []model.UsageSample{{Window: w, UsedPct: pct, ResetsAt: resets}}}
	}
	with := func(f func(r *store.Reading)) store.Reading {
		r := store.Reading{SessionID: "s1", At: now}
		f(&r)
		return r
	}
	bad := map[string]store.Reading{
		"negative cost":        with(func(r *store.Reading) { r.CostUSD = usageF(-0.01) }),
		"NaN cost":             with(func(r *store.Reading) { r.CostUSD = usageF(math.NaN()) }),
		"infinite cost":        with(func(r *store.Reading) { r.CostUSD = usageF(math.Inf(1)) }),
		"cost over 100000":     with(func(r *store.Reading) { r.CostUSD = usageF(100_000.01) }),
		"negative rebuild":     with(func(r *store.Reading) { r.CacheRebuildUSD = usageF(-1) }),
		"rebuild over 1000":    with(func(r *store.Reading) { r.CacheRebuildUSD = usageF(1000.5) }),
		"NaN rebuild":          with(func(r *store.Reading) { r.CacheRebuildUSD = usageF(math.NaN()) }),
		"negative recache":     with(func(r *store.Reading) { r.RecacheTokens = usageI(-1) }),
		"huge recache":         with(func(r *store.Reading) { r.RecacheTokens = usageI(store.MaxRecacheTokens + 1) }),
		"at a day ago":         with(func(r *store.Reading) { r.At = now.Add(-24*time.Hour - time.Nanosecond) }),
		"at in the future":     with(func(r *store.Reading) { r.At = now.Add(time.Minute + time.Nanosecond) }),
		"cache a week ago":     with(func(r *store.Reading) { r.CacheExpiresAt = now.Add(-7*24*time.Hour - time.Nanosecond) }),
		"cache in 3h":          with(func(r *store.Reading) { r.CacheExpiresAt = now.Add(2*time.Hour + time.Nanosecond) }),
		"used_pct -1":          limit(model.WindowFiveHour, -1, time.Time{}),
		"used_pct 101":         limit(model.WindowFiveHour, 100.0001, time.Time{}),
		"used_pct NaN":         limit(model.WindowSevenDay, math.NaN(), time.Time{}),
		"used_pct infinite":    limit(model.WindowSevenDay, math.Inf(1), time.Time{}),
		"window one_hour":      limit("one_hour", 10, time.Time{}),
		"window empty":         limit("", 10, time.Time{}),
		"resets an hour ago":   limit(model.WindowFiveHour, 10, now.Add(-time.Hour-time.Nanosecond)),
		"resets in 9 days":     limit(model.WindowFiveHour, 10, now.Add(8*24*time.Hour+time.Nanosecond)),
		"a window twice":       {SessionID: "s1", At: now, Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: 1}, {Window: model.WindowFiveHour, UsedPct: 2}}},
		"empty session":        with(func(r *store.Reading) { r.SessionID = "" }),
		"session with a space": with(func(r *store.Reading) { r.SessionID = "s 1" }),
		"session with a slash": with(func(r *store.Reading) { r.SessionID = "../s1" }),
		"session of 65 bytes":  with(func(r *store.Reading) { r.SessionID = strings.Repeat("a", 65) }),
		"session not ASCII":    with(func(r *store.Reading) { r.SessionID = "sé" }),
	}
	before := usageCostColumns(t, path, "s1")
	for name, r := range bad {
		r.CostUSD = cmpOr(r.CostUSD, usageF(3))
		if _, err := s.Ingest(context.Background(), r, now); err == nil {
			t.Errorf("%s: stored, want refused", name)
		} else if strings.Contains(err.Error(), "../s1") || strings.Contains(err.Error(), "sé") {
			t.Errorf("%s: the error quotes the bad session id: %v", name, err)
		}
	}
	if after := usageCostColumns(t, path, "s1"); after != before {
		t.Errorf("refused readings changed the session: %s", after)
	}
	if n, m := len(usageRows(t, path)), len(costRows(t, path)); n != 0 || m != 0 {
		t.Errorf("refused readings stored %d usage and %d cost samples", n, m)
	}

	// The edges of every range are accepted.
	for name, r := range map[string]store.Reading{
		"cost 0 and the earliest at": with(func(r *store.Reading) { r.CostUSD, r.At = usageF(0), now.Add(-24*time.Hour) }),
		"cost 100000 and the latest at": with(func(r *store.Reading) {
			r.CostUSD, r.At = usageF(100_000), now.Add(time.Minute)
		}),
		"rebuild 0, cache a week ago":    with(func(r *store.Reading) { r.CacheRebuildUSD, r.CacheExpiresAt = usageF(0), now.Add(-7*24*time.Hour) }),
		"rebuild 1000, cache in 2h":      with(func(r *store.Reading) { r.CacheRebuildUSD, r.CacheExpiresAt = usageF(1000), now.Add(2*time.Hour) }),
		"recache 0":                      with(func(r *store.Reading) { r.RecacheTokens = usageI(0) }),
		"recache at the bound":           with(func(r *store.Reading) { r.RecacheTokens = usageI(store.MaxRecacheTokens) }),
		"used_pct 0, resets an hour ago": limit(model.WindowFiveHour, 0, now.Add(-time.Hour)),
		"used_pct 100, resets in 8 days": limit(model.WindowSevenDay, 100, now.Add(8*24*time.Hour)),
		"a 64-byte session id":           {SessionID: strings.Repeat("A-z_9", 12) + "abcd", At: now},
	} {
		if _, err := s.Ingest(context.Background(), r, now); err != nil {
			t.Errorf("%s: %v, want stored", name, err)
		}
	}
}

func cmpOr(a, b *float64) *float64 {
	if a != nil {
		return a
	}
	return b
}

// usageFill writes, by raw SQL, a database the size probe S1 implies after
// a busy week: perWindow usage samples of each window, sessions sessions
// with perSession cost samples each over the last day, and history old
// sessions with one kept cost sample each; then ANALYZE when analyze is
// set.
func usageFill(t testing.TB, db *sql.DB, perWindow, sessions, perSession, history int, analyze bool) {
	t.Helper()
	ref := usageNow.UTC().Format("2006-01-02 15:04:05")
	for _, q := range []string{
		`WITH RECURSIVE i(k) AS (SELECT 1 UNION ALL SELECT k + 1 FROM i WHERE k < ` + fmt.Sprint(perWindow) + `)
INSERT INTO usage_samples (at, window, used_pct, session_id)
SELECT strftime('%Y-%m-%dT%H:%M:%f000000Z', '` + ref + `', '-' || (k * 20) || ' seconds'), w, k % 100, 's' || (k % 10)
FROM i, (SELECT 'five_hour' AS w UNION ALL SELECT 'seven_day')`,
		`WITH RECURSIVE i(k) AS (SELECT 1 UNION ALL SELECT k + 1 FROM i WHERE k < ` + fmt.Sprint(sessions*perSession) + `)
INSERT INTO cost_samples (at, session_id, cost_usd)
SELECT strftime('%Y-%m-%dT%H:%M:%f000000Z', '` + ref + `', '-' || (k * 86400 / ` + fmt.Sprint(sessions*perSession) + `) || ' seconds'),
  'live' || (k % ` + fmt.Sprint(sessions) + `), k FROM i`,
		`WITH RECURSIVE i(k) AS (SELECT 1 UNION ALL SELECT k + 1 FROM i WHERE k < ` + fmt.Sprint(history) + `)
INSERT INTO cost_samples (at, session_id, cost_usd)
SELECT strftime('%Y-%m-%dT%H:%M:%f000000Z', '` + ref + `', '-' || (2 + k % 20) || ' days'), 'old' || k, k FROM i`,
		`WITH RECURSIVE i(k) AS (SELECT 0 UNION ALL SELECT k + 1 FROM i WHERE k < ` + fmt.Sprint(sessions-1) + `)
INSERT INTO sessions (id, agent, state) SELECT 'live' || k, 'claude', 'working' FROM i`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("filling: %v\n%s", err, q)
		}
	}
	if analyze {
		if _, err := db.Exec(`ANALYZE`); err != nil {
			t.Fatal(err)
		}
	}
}

// usageAssertPlan fails unless plan uses every one of indexes and scans
// no table (the walk's own CTE, ids, is not a table).
func usageAssertPlan(t *testing.T, name, plan string, indexes []string) {
	t.Helper()
	for _, idx := range indexes {
		if !strings.Contains(plan, idx) {
			t.Errorf("%s does not use %s:\n%s", name, idx, plan)
		}
	}
	for line := range strings.SplitSeq(plan, "\n") {
		if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "INDEX") &&
			!strings.HasPrefix(line, "SCAN ids") && !strings.HasPrefix(line, "SCAN CONSTANT ROW") {
			t.Errorf("%s scans a table: %s\n%s", name, line, plan)
		}
	}
	t.Logf("%s:\n%s", name, plan)
}

// Every statement of the ingest path and of the two readers searches an
// index, on an empty file and on a filled and analyzed one.
func TestUsageQueriesUseIndexes(t *testing.T) {
	type query struct {
		name, sql string
		args      []any
		indexes   []string // all of them
	}
	at := "2026-11-15T12:00:00.000000000Z"
	qs := []query{
		{"session update", store.IngestSessionQuery, []any{1.0, at, 1.0, int64(1), "s1"}, []string{"sqlite_autoindex_sessions_1"}},
		{"session exists", store.IngestSessionExistsQuery, []any{"s1"}, []string{"sqlite_autoindex_sessions_1"}},
		{"newest cost sample", store.NewestCostQuery, []any{"s1"}, []string{"cost_samples_session"}},
		{"newest usage sample", store.NewestUsageQuery, []any{"five_hour"}, []string{"usage_samples_window"}},
		{"prune usage", store.PruneUsageQuery, []any{at, 200}, []string{"usage_samples_window"}},
		{"prune cost", store.PruneCostQuery, []any{"live7", 32, at, at, 200}, []string{"cost_samples_session"}},
		{"prune walk", store.PruneWalkQuery, []any{"live7", 32}, []string{"cost_samples_session"}},
		{"UsageSince", store.UsageSinceQuery, []any{at}, []string{"usage_samples_window"}},
		{"CostSince", store.CostSinceQuery, []any{at, at}, []string{"cost_samples_session"}},
	}
	for _, c := range []struct {
		name    string
		fill    bool
		analyze bool
	}{{"empty", false, false}, {"filled", true, false}, {"filled and analyzed", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			path := dbPath(t)
			open(t, path)
			db := rawDB(t, path)
			if c.fill {
				usageFill(t, db, 15_000, 30, 1_000, 2_000, c.analyze)
			}
			for _, q := range qs {
				usageAssertPlan(t, q.name, queryPlan(t, db, q.sql, q.args...), q.indexes)
			}
		})
	}
}

// BenchmarkIngest measures one status-line ingest (cost, cache and both
// windows, a changed value each time so every step writes) on a filled
// file, and reports its p50 and p95.
func BenchmarkIngest(b *testing.B) {
	path := filepath.Join(b.TempDir(), "curia.db")
	s, err := store.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	usageFill(b, db, 15_000, 30, 1_000, 2_000, false)
	ctx := context.Background()
	durations := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := range b.N {
		r := usageFull("live1", usageNow)
		r.CostUSD = usageF(float64(i))
		r.Limits[0].UsedPct = float64(i % 100)
		start := time.Now()
		if _, err := s.Ingest(ctx, r, usageNow); err != nil {
			b.Fatal(err)
		}
		durations = append(durations, time.Since(start))
	}
	b.StopTimer()
	slices.Sort(durations)
	b.ReportMetric(float64(durations[len(durations)/2].Microseconds()), "p50-µs")
	b.ReportMetric(float64(durations[len(durations)*95/100].Microseconds()), "p95-µs")
}

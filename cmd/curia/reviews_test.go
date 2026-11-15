package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

const reviewsNow = "2026-11-14T12:00:00Z"

// reviewsSpec is one seeded review, inserted by raw SQL so every value is exact.
type reviewsSpec struct {
	task, summary, tests, detail, status, how, diff, head, pr, notes, note, via string
	created, closed                                                             string
}

const reviewsInsertSQL = `INSERT INTO reviews (task_id, id, session_id, summary, tests_status, tests_detail, status,
  approved_how, diff_stat, head, pr_url, my_notes, note_message, closed_via, created_at, updated_at, closed_at)
  VALUES (?, 'R-' || ?, 's1', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// reviewsHome isolates HOME and CURIA_DB, fixes the clock, seeds one task per
// title and then the specs (task is an index into titles, as text).
func reviewsHome(t *testing.T, titles []string, specs ...reviewsSpec) {
	t.Helper()
	db := hookHome(t)
	prev := clock
	clock = func() time.Time {
		ts, err := time.Parse(time.RFC3339, reviewsNow)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	t.Cleanup(func() {
		clock = prev
		reviewsStatus, reviewsJSON = "pending", false
	})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, title := range titles {
		if _, err = st.CreateTask(context.Background(), model.Task{
			Title: title, Repo: "/src/curia", Status: model.TaskReady, Source: model.SourceLocal, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	for i, s := range specs {
		var closed any
		if s.closed != "" {
			closed = s.closed
		}
		if _, err := raw.Exec(reviewsInsertSQL, s.task, i+1, s.summary, s.tests, s.detail, s.status, s.how, s.diff,
			s.head, s.pr, s.notes, s.note, s.via, s.created, s.created, closed); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// reviewsOnePerStatus seeds a review of each status, in n order R-1..R-5.
func reviewsOnePerStatus(t *testing.T) {
	t.Helper()
	const c = "2026-11-14T11:00:00.000000000Z"
	reviewsHome(t, []string{"One", "Two", "Three", "Four", "Five"},
		reviewsSpec{task: "T-1", summary: "s1", tests: "passed", status: "pending", created: c},
		reviewsSpec{task: "T-2", summary: "s2", tests: "failed", status: "approved", how: "pr", pr: "https://x/pr/1", created: c},
		reviewsSpec{task: "T-3", summary: "s3", tests: "not_run", status: "changes_requested", notes: "fix", created: c},
		reviewsSpec{task: "T-4", summary: "s4", tests: "passed", status: "merged", how: "merge", created: c, closed: c},
		reviewsSpec{task: "T-5", summary: "s5", tests: "passed", status: "dropped", created: c, closed: c})
}

func reviewsIDs(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runRoot(t, append([]string{"reviews", "ls", "--json"}, args...)...)
	if err != nil {
		t.Fatalf("reviews ls: %v\n%s", err, out)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, " ")
}

func TestReviewsLsDefaultIsPending(t *testing.T) {
	reviewsOnePerStatus(t)
	if got := reviewsIDs(t); got != "R-1" {
		t.Errorf("default = %q, want R-1", got)
	}
}

func TestReviewsLsStatusFilterAndAll(t *testing.T) {
	reviewsOnePerStatus(t)
	for _, c := range []struct{ status, want string }{
		{"pending", "R-1"},
		{"approved", "R-2"},
		{"changes_requested", "R-3"},
		{"merged", "R-4"},
		{"dropped", "R-5"},
		{"all", "R-1 R-2 R-3 R-4 R-5"},
	} {
		if got := reviewsIDs(t, "--status", c.status); got != c.want {
			t.Errorf("--status %s = %q, want %q", c.status, got, c.want)
		}
		reviewsStatus = "pending"
	}
	out, err := runRoot(t, "reviews", "ls", "--status", "x\x1b[31m")
	if err == nil {
		t.Fatalf("--status x succeeded: %s", out)
	}
	for _, name := range []string{"pending", "approved", "changes_requested", "merged", "dropped", "all"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
	if strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("error holds ESC: %q", err)
	}
}

func TestReviewsLsJSONGolden(t *testing.T) {
	const z = ".000000000Z"
	reviewsHome(t, []string{"One", "Two", "Three"},
		reviewsSpec{
			task: "T-1", summary: "Added the parser.", tests: "passed", detail: "go test ./... ok", status: "pending",
			created: "2026-11-14T08:00:00" + z,
		},
		reviewsSpec{
			task: "T-2", summary: "Fixed <the> & bug.", tests: "failed", detail: "1 flaky", status: "merged", how: "merge",
			diff: " a.go | 2 +-\n 1 file changed", head: "abc123", via: "popup",
			created: "2026-11-14T09:00:00" + z, closed: "2026-11-14T10:00:00" + z,
		},
		reviewsSpec{
			task: "T-3", summary: "Refactor.", tests: "not_run", status: "changes_requested", notes: "Please add tests.",
			note: "M-1", via: "cli", created: "2026-11-14T07:00:00" + z, closed: "2026-11-14T07:30:00" + z,
		})
	out, err := runRoot(t, "reviews", "ls", "--json", "--status", "all")
	if err != nil {
		t.Fatalf("reviews ls: %v\n%s", err, out)
	}
	out = regexp.MustCompile(`("(?:created_at|updated_at|closed_at)": )"[^"]+"`).ReplaceAllString(out, `$1"<time>"`)
	want, err := os.ReadFile(filepath.Join("testdata", "reviews.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("reviews ls --json output:\n%s\nwant (testdata/reviews.golden.json):\n%s", out, want)
	}
}

func TestReviewsLsEmptyIsEmptyArray(t *testing.T) {
	reviewsHome(t, nil)
	out, err := runRoot(t, "reviews", "ls", "--json")
	if err != nil || out != "[]\n" {
		t.Errorf("ls --json on an empty store = %q, %v, want \"[]\\n\"", out, err)
	}
}

func TestReviewsLsTableStatusWords(t *testing.T) {
	reviewsOnePerStatus(t)
	reviewsStatus = "all"
	out, err := runRoot(t, "reviews", "ls", "--status", "all")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("table has %d lines, want 6:\n%s", len(lines), out)
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "ID AGE TASK STATUS TESTS SUMMARY" {
		t.Errorf("header = %q", lines[0])
	}
	for i, want := range []string{
		"R-1  1h   T-1 One    pending            agent: passed   s1",
		"R-2  1h   T-2 Two    approved (pr)      agent: failed   s2",
		"R-3  1h   T-3 Three  changes requested  agent: not run  s3",
		"R-4  1h   T-4 Four   merged             agent: passed   s4",
		"R-5  1h   T-5 Five   dropped            agent: passed   s5",
	} {
		if lines[i+1] != want {
			t.Errorf("line %d = %q, want %q", i+1, lines[i+1], want)
		}
	}
}

func TestReviewsLsTableCutsAndFirstLine(t *testing.T) {
	long := strings.Repeat("é", 100)
	reviewsHome(t, []string{long},
		reviewsSpec{task: "T-1", summary: long + "\nsecond line", tests: "passed", status: "pending", created: "2026-11-14T11:59:30.000000000Z"})
	out, err := runRoot(t, "reviews", "ls")
	if err != nil {
		t.Fatal(err)
	}
	row := strings.Split(strings.TrimRight(out, "\n"), "\n")[1]
	if strings.Contains(row, "second") {
		t.Errorf("second summary line shown: %q", row)
	}
	if want := strings.Repeat("é", 29) + "…"; !strings.Contains(row, "T-1 "+want+"  ") {
		t.Errorf("title not cut to 30 runes: %q", row)
	}
	if want := strings.Repeat("é", 59) + "…"; !strings.HasSuffix(row, want) {
		t.Errorf("summary not cut to 60 runes: %q", row)
	}
	if !strings.Contains(row, "30s") {
		t.Errorf("age missing: %q", row)
	}
}

func TestReviewsLsTableIsSanitised(t *testing.T) {
	const hostile = "x\x1b[31mred\nnext\u202e\u2066\u2069\u200e\u200f\u061c\x7f\xff\xfey"
	reviewsHome(t, []string{hostile},
		reviewsSpec{task: "T-1", summary: hostile, tests: "passed", detail: hostile, status: "pending", created: "2026-11-14T11:00:00.000000000Z"})
	for _, args := range [][]string{{"reviews", "ls"}, {"reviews", "ls", "--json"}} {
		out, err := runRoot(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range strings.TrimRight(out, "\n") {
			if r == '\n' {
				continue
			}
			if !unicode.IsPrint(r) {
				t.Errorf("%v: output holds %U: %q", args, r, out)
				break
			}
		}
		reviewsJSON = false
	}
	// In the table the text is cleaned to one line per review.
	out, _ := runRoot(t, "reviews", "ls")
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("table has %d lines, want 2: %q", n, out)
	}
}

func TestReviewsLsAllowedInsideAgent(t *testing.T) {
	reviewsOnePerStatus(t)
	t.Setenv("CLAUDECODE", "1")
	if out, err := runRoot(t, "reviews", "ls"); err != nil {
		t.Errorf("reviews ls inside an agent: %v\n%s", err, out)
	}
}

func TestReviewsLsTaskGoneStillLists(t *testing.T) {
	reviewsHome(t, nil, reviewsSpec{task: "T-9", summary: "orphan", tests: "passed", status: "pending", created: "2026-11-14T11:00:00.000000000Z"})
	out, err := runRoot(t, "reviews", "ls")
	if err != nil || !strings.Contains(out, "T-9  ") {
		t.Errorf("ls = %q, %v", out, err)
	}
}

package store_test

import (
	"context"
	"database/sql"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func tlAt(m int) string {
	return t0.Add(time.Duration(m) * time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func tlExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// tlSeed fills one row of every source, minutes after t0, and returns the db.
func tlSeed(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	path := dbPath(t)
	s := open(t, path)
	db := rawDB(t, path)
	tlExec(t, db, `INSERT INTO sessions (id, agent, state, repo, task_id, started_at, updated_at, end_reason) VALUES ('s1','claude','ended','/r/a','T-1',?,?,'other')`, tlAt(1), tlAt(9))
	tlExec(t, db, `INSERT INTO sessions (id, agent, state, repo, started_at, updated_at) VALUES ('s2','claude','idle','/r/b',?,?)`, tlAt(2), tlAt(2))
	tlExec(t, db, `INSERT INTO events (session_id, kind, payload, at) VALUES ('s1','Stop','{"last_assistant_message":"all done"}',?)`, tlAt(3))
	tlExec(t, db, `INSERT INTO events (session_id, kind, payload, at) VALUES ('s2','Stop','not json',?)`, tlAt(4))
	tlExec(t, db, `INSERT INTO events (session_id, kind, payload, at) VALUES ('s2','PreToolUse','{}',?)`, tlAt(4))
	tlExec(t, db, `INSERT INTO tasks (id, title, repo, status, created_at, updated_at) VALUES ('T-1','the task','/r/a','ready',?,?)`, tlAt(0), tlAt(0))
	d := create(t, s, ticket(t, "Which store for webhooks?", model.ModeBlocking, "s1", t0.Add(5*time.Minute)))
	tlExec(t, db, `UPDATE decisions SET task_id = 'T-1', repo = '/r/a', status = 'overridden', answer = 'B: Redis', answered_at = ? WHERE id = ?`, tlAt(6), d.ID)
	tlExec(t, db, `INSERT INTO reviews (id, task_id, session_id, summary, tests_status, status, created_at, updated_at, closed_at) VALUES ('R-1','T-1','s1','done','passed','merged',?,?,?)`, tlAt(7), tlAt(8), tlAt(8))
	tlExec(t, db, `INSERT INTO messages (id, text, kind, target_session, routed_by, status, created_at, delivered_at) VALUES ('M-1','hello','instruction','s2','address','delivered',?,?)`, tlAt(10), tlAt(10))
	tlExec(t, db, `INSERT INTO pushes (kind, ref, device, status, not_before, created_at, sent_at) VALUES ('decision','D-1','phone','sent',?,?,?)`, tlAt(5), tlAt(5), tlAt(11))
	return s, db
}

func tlKinds(evs []model.TimelineEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestTimelineOrdersAcrossTables(t *testing.T) {
	s, _ := tlSeed(t)
	evs, err := s.Timeline(context.Background(), store.TimelineQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"push_sent", "message_delivered", "session_end", "review_closed", "review_opened", "overridden",
		"ticket_raised", "turn_end", "turn_end", "session_start", "session_start", "task_filed",
	}
	if got := tlKinds(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].At.After(evs[i-1].At) {
			t.Errorf("event %d is newer than %d", i, i-1)
		}
	}
	if evs[8].Line != "all done" && evs[7].Line != "all done" {
		t.Errorf("a Stop line carries the last message: %+v", evs[7:9])
	}
	if evs[7].Line != "turn ended" { // s2's payload is not JSON
		t.Errorf("fallback line = %q", evs[7].Line)
	}
}

func TestTimelineFilters(t *testing.T) {
	s, _ := tlSeed(t)
	ctx := context.Background()
	task, _ := s.Timeline(ctx, store.TimelineQuery{TaskID: "T-1"})
	for _, e := range task {
		if e.TaskID != "T-1" {
			t.Errorf("--task kept %+v", e)
		}
	}
	if got := tlKinds(task); !reflect.DeepEqual(got, []string{"push_sent", "session_end", "review_closed", "review_opened", "overridden", "ticket_raised", "turn_end", "session_start", "task_filed"}) {
		t.Errorf("task kinds = %v", got)
	}
	repo, _ := s.Timeline(ctx, store.TimelineQuery{Repo: "/r/b"})
	if got := tlKinds(repo); !reflect.DeepEqual(got, []string{"message_delivered", "turn_end", "session_start"}) {
		t.Errorf("repo kinds = %v", got)
	}
	win, _ := s.Timeline(ctx, store.TimelineQuery{Since: t0.Add(5 * time.Minute), Before: t0.Add(8 * time.Minute)})
	if got := tlKinds(win); !reflect.DeepEqual(got, []string{"review_opened", "overridden", "ticket_raised"}) {
		t.Errorf("window kinds = %v", got)
	}
}

func TestTimelinePages(t *testing.T) {
	s, _ := tlSeed(t)
	ctx := context.Background()
	p1, _ := s.Timeline(ctx, store.TimelineQuery{Limit: 5})
	p2, _ := s.Timeline(ctx, store.TimelineQuery{Limit: 5, Before: p1[len(p1)-1].At})
	all, _ := s.Timeline(ctx, store.TimelineQuery{})
	if len(p1) != 5 || !reflect.DeepEqual(append(p1, p2...)[:len(p1)+len(p2)], all[:len(p1)+len(p2)]) {
		t.Errorf("pages do not follow each other: %d %d", len(p1), len(p2))
	}
}

func TestTimelineCountsGroupBySessionAndKind(t *testing.T) {
	s, _ := tlSeed(t)
	c, err := s.TimelineCounts(context.Background(), t0, t0.Add(5*time.Minute), "turn_end", "ticket_raised")
	if err != nil {
		t.Fatal(err)
	}
	if c["s1"]["turn_end"] != 1 || c["s2"]["turn_end"] != 1 || c["s1"]["ticket_raised"] != 0 {
		t.Errorf("counts = %v", c)
	}
}

// More events than the timeline's cap are all counted (C2-3).
func TestTimelineCountsAboveTheCap(t *testing.T) {
	s, db := tlSeed(t)
	tlExec(t, db, `WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 1200)
INSERT INTO events (session_id, kind, payload, at) SELECT 's1', 'Stop', '{}', ? FROM c`, tlAt(30))
	c, err := s.TimelineCounts(context.Background(), t0, t0.Add(time.Hour), "turn_end")
	if err != nil {
		t.Fatal(err)
	}
	if got := c["s1"]["turn_end"]; got != 1201 {
		t.Errorf("s1 turn_end = %d, want 1201", got)
	}
}

// One row with an unreadable time is skipped and counted; the rest is read (C2-5).
func TestTimelineSkipsAnUnreadableRow(t *testing.T) {
	s, db := tlSeed(t)
	tlExec(t, db, `INSERT INTO events (session_id, kind, payload, at) VALUES ('s2','Stop','{}','2026-12-01 bogus')`)
	r, err := s.TimelineRead(context.Background(), store.TimelineQuery{Limit: 1000})
	if err != nil {
		t.Fatalf("one bad row fails the read: %v", err)
	}
	if r.Skipped != 1 || len(r.Events) != 12 {
		t.Errorf("skipped = %d, events = %d, want 1 and 12", r.Skipped, len(r.Events))
	}
}

// Away periods and denied actions are timeline sources (C2-2).
func TestTimelineAwayAndDenials(t *testing.T) {
	s, db := tlSeed(t)
	tlExec(t, db, `INSERT INTO away_runs (started_at, ends_at, ended_at) VALUES (?, ?, ?)`, tlAt(20), tlAt(60), tlAt(40))
	tlExec(t, db, `INSERT INTO permission_events (session_id, at, last_at, mode, tool, summary, verdict, n) VALUES ('s2', ?, ?, 'away', 'Bash', 'rm -rf x', 'deny', 7)`, tlAt(21), tlAt(25))
	tlExec(t, db, `INSERT INTO permission_events (session_id, at, last_at, mode, tool, summary, verdict, n) VALUES ('s2', ?, ?, 'away', 'Bash', 'ls', 'allow', 3)`, tlAt(22), tlAt(22))
	evs, err := s.Timeline(context.Background(), store.TimelineQuery{Since: t0.Add(15 * time.Minute), Before: t0.Add(50 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if got := tlKinds(evs); !reflect.DeepEqual(got, []string{"away_end", "action_denied", "away_start"}) {
		t.Fatalf("kinds = %v", got)
	}
	if !strings.Contains(evs[1].Line, "Bash") || !strings.Contains(evs[1].Line, "7") || evs[1].SessionID != "s2" {
		t.Errorf("denial = %+v", evs[1])
	}
}

// Any scan of a table, under its own name or an alias (the plan prints the alias).
var tlScan = regexp.MustCompile(`^SCAN [A-Za-z_]+( [A-Za-z_]+)?$`)

// A large store is read through the time indexes, never scanned whole.
func TestTimelineQueryUsesIndexes(t *testing.T) {
	s, db := tlSeed(t)
	// 30000 Stop events and 3000 answered decisions, one second apart.
	const gen = `WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM c WHERE i < 30000),
 ts(i, at) AS (SELECT i, printf('2026-12-%02dT%02d:%02d:%02d.000000000Z', 1 + i / 86400, (i / 3600) % 24, (i / 60) % 60, i % 60) FROM c) `
	tlExec(t, db, gen+`INSERT INTO events (session_id, kind, payload, at) SELECT 'x' || (i % 500), 'Stop', '{}', at FROM ts`)
	tlExec(t, db, gen+`INSERT INTO decisions (id, question, options, recommendation, rationale, reversibility, mode, scope, status, args_hash, created_at, answered_at, answer)
SELECT 'D-x' || i, 'q', '[]', 'a', 'r', 'cheap', 'blocking', 'repo', 'answered', i, at, at, 'x' FROM ts WHERE i % 10 = 0`)
	for _, line := range strings.Split(queryPlan(t, db, store.TimelineUnion(), tlAt(0), "9999", "", "", 50, "", "", false), "\n") {
		if tlScan.MatchString(strings.TrimSpace(line)) && !strings.Contains(line, "CONSTANT") {
			t.Errorf("a source scans its whole table: %q", line)
		}
	}
	evs, err := s.Timeline(context.Background(), store.TimelineQuery{Limit: 50})
	if err != nil || len(evs) != 50 {
		t.Fatalf("Timeline = %d events, %v", len(evs), err)
	}
}

// Events that share one timestamp across a page edge are neither skipped
// nor repeated when paging with the last event as the cursor.
func TestTimelinePagesThroughIdenticalTimestamps(t *testing.T) {
	s, db := tlSeed(t)
	for range 7 {
		tlExec(t, db, `INSERT INTO events (session_id, kind, payload, at) VALUES ('s2','Stop','{}',?)`, tlAt(20))
	}
	ctx := context.Background()
	all, err := s.Timeline(ctx, store.TimelineQuery{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{1, 2, 3, 4} {
		var got []model.TimelineEvent
		var cur model.TimelineEvent
		for len(got) <= len(all) {
			p, err := s.Timeline(ctx, store.TimelineQuery{Limit: size, Cursor: cur})
			if err != nil {
				t.Fatal(err)
			}
			if len(p) == 0 {
				break
			}
			got, cur = append(got, p...), p[len(p)-1]
		}
		if !reflect.DeepEqual(got, all) {
			t.Errorf("page size %d: %d events, want %d (or a different order)", size, len(got), len(all))
		}
	}
}

func TestTimelineTextIsTheWholeLine(t *testing.T) {
	s, db := tlSeed(t)
	long := strings.Repeat("word ", 200)
	tlExec(t, db, `INSERT INTO events (session_id, kind, payload, at) VALUES ('s1','Stop',json_object('last_assistant_message', ?),?)`, long, tlAt(30))
	evs, _ := s.Timeline(context.Background(), store.TimelineQuery{Limit: 1})
	if len(evs[0].Line) != 300 {
		t.Fatalf("line = %d bytes, want the 300 bound", len(evs[0].Line))
	}
	full, err := s.TimelineText(context.Background(), evs[0])
	if err != nil || full != long {
		t.Errorf("TimelineText = %d bytes, %v; want %d", len(full), err, len(long))
	}
}

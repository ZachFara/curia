package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// reviewT0 is when most test reviews are opened; its nanoseconds check that
// times survive to the nanosecond.
var reviewT0 = time.Date(2026, 11, 14, 9, 0, 0, 987_654_321, time.UTC)

// reviewCall is what complete_task passes for taskID from session at at.
func reviewCall(taskID, session, summary string, at time.Time) model.Review {
	return model.Review{
		TaskID: taskID, SessionID: session, Summary: summary,
		Tests: model.TestsPassed, TestsDetail: "go test ./... passed", CreatedAt: at,
	}
}

// completeReview runs CompleteTask and requires it to succeed.
func completeReview(t *testing.T, s *store.Store, r model.Review) (model.Review, bool) {
	t.Helper()
	got, updated, err := s.CompleteTask(context.Background(), r)
	if err != nil {
		t.Fatalf("CompleteTask(%s): %v", r.TaskID, err)
	}
	return got, updated
}

// reviewFor creates a task and opens its review, and returns the review.
func reviewFor(t *testing.T, s *store.Store, title string) model.Review {
	t.Helper()
	task := createTask(t, s, newTask(title))
	r, _ := completeReview(t, s, reviewCall(task.ID, "s-"+task.ID, "Done: "+title, reviewT0))
	return r
}

func getReview(t *testing.T, s *store.Store, id string) model.Review {
	t.Helper()
	r, ok, err := s.GetReview(context.Background(), id)
	if err != nil {
		t.Fatalf("GetReview(%s): %v", id, err)
	}
	if !ok {
		t.Fatalf("GetReview(%s): not found", id)
	}
	return r
}

func closeReview(t *testing.T, s *store.Store, id string, c store.ReviewClose, now time.Time) (model.Review, int) {
	t.Helper()
	r, n, err := s.CloseReview(context.Background(), id, c, now)
	if err != nil {
		t.Fatalf("CloseReview(%s, %s): %v", id, c.Status, err)
	}
	return r, n
}

// assertReview compares two reviews, times with Equal (the store returns
// them in UTC) and everything else with DeepEqual.
func assertReview(t *testing.T, got, want model.Review) {
	t.Helper()
	times := func(r *model.Review) map[string]*time.Time {
		return map[string]*time.Time{"CreatedAt": &r.CreatedAt, "UpdatedAt": &r.UpdatedAt, "ClosedAt": &r.ClosedAt}
	}
	gt, wt := times(&got), times(&want)
	for name, g := range gt {
		if !g.Equal(*wt[name]) {
			t.Errorf("%s %s = %v, want %v", want.ID, name, *g, *wt[name])
		}
		*g, *wt[name] = time.Time{}, time.Time{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("review:\n got %+v\nwant %+v", got, want)
	}
}

func reviewIDs(rs []model.Review) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func countReviews(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := rawDB(t, path).QueryRow(`SELECT count(*) FROM reviews`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// mergedClose is a merge approved from the command line.
var mergedClose = store.ReviewClose{
	Status: model.ReviewMerged, How: model.ApproveMerge,
	DiffStat: " store.go | 3 ++-\n 1 file changed", Head: "abc1234def", Via: model.AnsweredCLI,
}

func TestMigrationAddsReviewsAndPushes(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	for _, obj := range [][2]string{
		{"table", "reviews"},
		{"table", "pushes"},
		{"index", "reviews_one_pending"},
		{"index", "reviews_task"},
		{"index", "reviews_status"},
		{"index", "pushes_once"},
		{"index", "pushes_due"},
		{"index", "pushes_token"},
		{"index", "decisions_task"},
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
	for _, col := range []string{"tmux_socket", "end_reason"} {
		var typ string
		var notNull int
		err := db.QueryRow(`SELECT type, "notnull" FROM pragma_table_info('sessions') WHERE name = ?`, col).Scan(&typ, &notNull)
		if err != nil {
			t.Fatalf("sessions.%s: %v", col, err)
		}
		if typ != "TEXT" || notNull != 0 {
			t.Errorf("sessions.%s = %s notnull=%d, want nullable TEXT", col, typ, notNull)
		}
	}
	if n := countMigrations(t, path); n != 6 {
		t.Errorf("schema_migrations has %d rows, want 6", n)
	}
}

// The constraints of migration 0005, which no later task can change, on
// the pinned driver (the plan checked them with the system sqlite3).
func TestReviewMigrationConstraints(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	const at = "2026-11-14T09:00:00.000000000Z"
	insertReview := func(task, tests, status, how, via string) (string, error) {
		var id string
		err := db.QueryRow(`
INSERT INTO reviews (id, task_id, summary, tests_status, status, approved_how, closed_via, created_at, updated_at)
VALUES ('R-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM reviews), ?, 'done', ?, ?, ?, ?, ?, ?) RETURNING id`,
			task, tests, status, how, via, at, at).Scan(&id)
		return id, err
	}
	insertPush := func(kind, ref, device, status, hash string) error {
		_, err := db.Exec(`INSERT INTO pushes (kind, ref, device, status, not_before, token_hash, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, kind, ref, device, status, at, hash, at)
		return err
	}
	id, err := insertReview("T-1", "passed", "pending", "", "")
	if err != nil || id != "R-1" {
		t.Fatalf("first review = %q, %v; want R-1", id, err)
	}
	ok := func(_ string, err error) error { return err }
	for _, c := range []struct {
		name string
		err  error
		ok   bool
	}{
		{"second pending review of a task", ok(insertReview("T-1", "passed", "pending", "", "")), false},
		{"closed review of the same task", ok(insertReview("T-1", "failed", "approved", "pr", "popup")), true},
		{"pending review of another task", ok(insertReview("T-2", "not_run", "pending", "", "")), true},
		{"tests ok", ok(insertReview("T-3", "ok", "pending", "", "")), false},
		{"status done", ok(insertReview("T-3", "passed", "done", "", "")), false},
		{"approved by squash", ok(insertReview("T-3", "passed", "approved", "squash", "")), false},
		{"closed from the phone", ok(insertReview("T-3", "passed", "dropped", "", "ntfy")), false},
		{"every status", ok(insertReview("T-3", "passed", "changes_requested", "", "ssh")), true},
		{"merged", ok(insertReview("T-3", "passed", "merged", "merge", "cli")), true},
		{"push", insertPush("decision", "D-1", "phone", "pending", "aa"), true},
		{"same push again", insertPush("decision", "D-1", "phone", "sent", ""), false},
		{"same ref to another device", insertPush("decision", "D-1", "tablet", "pending", ""), true},
		{"a second push without a token", insertPush("review", "R-1", "phone", "pending", ""), true},
		{"a token hash twice", insertPush("review", "R-2", "phone", "pending", "aa"), false},
		{"unknown kind", insertPush("task", "T-1", "phone", "pending", ""), false},
		{"unknown push status", insertPush("review", "R-3", "phone", "queued", ""), false},
	} {
		if (c.err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.name, c.err, c.ok)
		}
	}
	for _, st := range []string{"sending", "sent", "failed", "dropped"} {
		if err := insertPush("review", "R-"+st, "phone", st, ""); err != nil {
			t.Errorf("push status %s: %v", st, err)
		}
	}
}

// Plan 3's session statements, copied from its store.go, so this test keeps
// describing what a Plan 3 binary runs after this package changes.
const (
	plan3Upsert = `
INSERT INTO sessions (
  id, agent, tmux_pane, tmux_session, tmux_window, cwd, repo, branch, state, model,
  context_used, context_max, subagents_active, last_update, seen_at, transcript_path,
  started_at, updated_at, transcript_offset, task_id, worktree
) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))
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
  task_id = excluded.task_id, worktree = excluded.worktree`
	plan3Select = `
SELECT COALESCE(id, ''), COALESCE(agent, ''), COALESCE(tmux_pane, ''), COALESCE(tmux_session, ''),
  COALESCE(tmux_window, ''), COALESCE(cwd, ''), COALESCE(repo, ''), COALESCE(branch, ''),
  COALESCE(state, ''), COALESCE(model, ''), COALESCE(context_used, 0), COALESCE(context_max, 0),
  COALESCE(subagents_active, 0), COALESCE(last_update, ''), seen_at, COALESCE(transcript_path, ''),
  started_at, updated_at, transcript_offset, COALESCE(task_id, ''), COALESCE(worktree, ''),
  COALESCE(team_id, ''), COALESCE(team_role, ''), COALESCE(team_member, '')
FROM sessions WHERE id = ?`
)

// plan3Session writes session lead of task T-1 the way a Plan 3 hook does.
func plan3Session(t *testing.T, db *sql.DB, state model.State, at string) {
	t.Helper()
	_, err := db.Exec(plan3Upsert, "lead", "claude", "%5", "work", "@2", "/src/curia/.curia-worktrees/T-1",
		"/src/curia", "curia/T-1-fix", state, "claude-opus-5-5", 2000, 200000, 0, "Edit tasks.go", nil,
		"/tmp/t3.jsonl", at, at, 7, "T-1", "/src/curia/.curia-worktrees/T-1")
	if err != nil {
		t.Fatalf("Plan 3 upsert: %v", err)
	}
}

// plan3Database builds, at path, a database as Plan 3's binary leaves it:
// 0001 to 0004 applied, Plan 1's session and event, Plan 2's decision, and
// a task, a message and a team-lead session of that task written by raw SQL
// in Plan 3's form.
func plan3Database(t *testing.T, path, at string) *sql.DB {
	t.Helper()
	db := plan2Database(t, path, at)
	schema, err := os.ReadFile(filepath.Clean(filepath.Join("migrations", "0004_tasks.sql")))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		string(schema),
		`INSERT INTO schema_migrations (name, applied_at) VALUES ('0004_tasks.sql', '` + at + `')`,
		`INSERT INTO tasks (id, title, body, repo, base_branch, status, branch, worktree, created_at, updated_at)
VALUES ('T-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM tasks), 'Fix the auth refresh', 'Brief.', '/src/curia',
  'main', 'ready', 'curia/T-1-fix', '/src/curia/.curia-worktrees/T-1', '` + at + `', '` + at + `')`,
		`INSERT INTO messages (id, text, kind, target_session, target_task, routed_by, status, created_at)
VALUES ('M-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM messages), 'Use the v2 endpoint.', 'instruction',
  'lead', 'T-1', 'address', 'queued', '` + at + `')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("building the Plan 3 database: %v", err)
		}
	}
	plan3Session(t, db, model.StateWorking, at)
	if _, err := db.Exec(`UPDATE sessions SET team_id = 'api', team_role = 'lead', team_member = 'team-lead' WHERE id = 'lead'`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrationOnPlan3Database(t *testing.T) {
	path := dbPath(t)
	const at = "2026-11-13T14:00:05.000000000Z"
	db := plan3Database(t, path, at)

	s := open(t, path)
	if names := migrationNames(t, db); !reflect.DeepEqual(names, []string{
		"0001_init.sql", "0002_decisions.sql",
		"0003_typed_confirmation.sql", "0004_tasks.sql", "0005_reviews.sql", "0005_send_limits.sql",
	}) {
		t.Errorf("migrations = %v, want 0001 to 0005 (both 0005 files)", names)
	}
	lead := get(t, s, "lead")
	want := model.Session{
		ID: "lead", Agent: "claude", Pane: "%5", TmuxSession: "work", TmuxWindow: "@2",
		Cwd: "/src/curia/.curia-worktrees/T-1", Repo: "/src/curia", Branch: "curia/T-1-fix",
		State: model.StateWorking, Model: "claude-opus-5-5", ContextUsed: 2000, ContextMax: 200000,
		LastUpdate: "Edit tasks.go", TranscriptPath: "/tmp/t3.jsonl", TranscriptOffset: 7,
		TaskID: "T-1", Worktree: "/src/curia/.curia-worktrees/T-1",
		TeamID: "api", TeamRole: model.RoleLead, TeamMember: "team-lead",
		StartedAt: decisionsTime(t, at), UpdatedAt: decisionsTime(t, at),
	}
	if !lead.StartedAt.Equal(want.StartedAt) || !lead.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("times = %v %v, want %s", lead.StartedAt, lead.UpdatedAt, at)
	}
	lead.StartedAt, lead.UpdatedAt, want.StartedAt, want.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(lead, want) {
		t.Errorf("Plan 3 session after migration:\n got %+v\nwant %+v", lead, want)
	}
	assertPlan3Rows(t, s, db, at)
	// The new schema works on the old rows.
	r, _ := completeReview(t, s, reviewCall("T-1", "lead", "Refresh fixed.", reviewT0))
	if r.ID != "R-1" {
		t.Errorf("first review on the migrated file = %s, want R-1", r.ID)
	}
	assertPlan3Runs(t, db, at)
}

// assertPlan3Rows checks the Plan 1 to 3 rows plan3Database wrote, other
// than the team lead's session, after the migration.
func assertPlan3Rows(t *testing.T, s *store.Store, db *sql.DB, at string) {
	t.Helper()
	if old := get(t, s, "old"); old.State != model.StateWorking || old.Pane != "%3" || old.Socket != "" || old.EndReason != "" {
		t.Errorf("Plan 1 session after migration: %+v", old)
	}
	task := getTask(t, s, "T-1")
	if task.Title != "Fix the auth refresh" || task.Status != model.TaskReady || task.Branch != "curia/T-1-fix" ||
		!task.CreatedAt.Equal(decisionsTime(t, at)) {
		t.Errorf("task after migration: %+v", task)
	}
	if m := getMessage(t, s, "M-1"); m.Text != "Use the v2 endpoint." || m.Status != model.MsgQueued || m.TargetTask != "T-1" {
		t.Errorf("message after migration: %+v", m)
	}
	if d := getDecision(t, s, "D-1"); d.Question != "Postgres or Redis for the event store?" || d.Status != model.StatusOpen {
		t.Errorf("decision after migration: %+v", d)
	}
	if err := ftsIntegrity(db); err != nil {
		t.Errorf("decisions_fts integrity-check: %v", err)
	}
}

// assertPlan3Runs checks that a Plan 3 binary keeps running against the
// migrated schema: its session upsert and select, and Plan 1's and Plan 2's
// statements.
func assertPlan3Runs(t *testing.T, db *sql.DB, at string) {
	t.Helper()
	plan3Session(t, db, model.StateIdle, at)
	cols := make([]any, 24)
	for i := range cols {
		cols[i] = new(any)
	}
	var state, teamRole string
	cols[8], cols[22] = &state, &teamRole
	if err := db.QueryRow(plan3Select, "lead").Scan(cols...); err != nil {
		t.Fatalf("Plan 3 select on the new schema: %v", err)
	}
	if state != string(model.StateIdle) || teamRole != model.RoleLead {
		t.Errorf("Plan 3 select: state %s role %s, want idle lead", state, teamRole)
	}
	assertPlan2Runs(t, db, at)
}

func TestSessionSocketRoundTrip(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	want := model.Session{ID: "s1", Agent: "claude", Pane: "%4", State: model.StateWorking, Socket: "/private/tmp/tmux-501/default"}
	upsert(t, s, want)
	if got := get(t, s, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("after UpsertSession:\n got %+v\nwant %+v", got, want)
	}
	err := s.UpdateSession(ctx, "s1", func(cur model.Session, _ bool) model.Session {
		cur.State = model.StateIdle
		return cur
	})
	if err != nil {
		t.Fatal(err)
	}
	want.State = model.StateIdle
	if got := get(t, s, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("after UpdateSession:\n got %+v\nwant %+v", got, want)
	}
	list, err := s.ListSessions(ctx, store.ListFilter{})
	if err != nil || len(list) != 1 || list[0].Socket != want.Socket {
		t.Errorf("ListSessions = %+v, %v; want the socket", list, err)
	}

	upsert(t, s, model.Session{ID: "s1", Agent: "claude", State: model.StateIdle})
	var isNull bool
	if err := rawDB(t, path).QueryRow(`SELECT tmux_socket IS NULL FROM sessions WHERE id = 's1'`).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("an empty socket is not stored as NULL")
	}
	if got := get(t, s, "s1"); got.Socket != "" {
		t.Errorf("Socket = %q after clearing, want empty", got.Socket)
	}
}

func TestOldUpsertKeepsSocket(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	upsert(t, s, model.Session{ID: "lead", Agent: "claude", State: model.StateWorking, Socket: "/tmp/tmux-501/work"})

	const at = "2026-11-14T10:00:00.000000000Z"
	plan3Session(t, rawDB(t, path), model.StateIdle, at)
	got := get(t, s, "lead")
	if got.State != model.StateIdle || got.TaskID != "T-1" {
		t.Errorf("Plan 3 upsert did not apply: state %s task %q", got.State, got.TaskID)
	}
	if got.Socket != "/tmp/tmux-501/work" {
		t.Errorf("Socket after Plan 3's upsert = %q, want it kept", got.Socket)
	}
	// Plan 1's upsert (still Plan 2's) leaves it too.
	plan1Session(t, rawDB(t, path), "lead", "%9", model.StateWorking, at)
	if got := get(t, s, "lead"); got.Socket != "/tmp/tmux-501/work" || got.Pane != "%9" {
		t.Errorf("after Plan 1's upsert: socket %q pane %q", got.Socket, got.Pane)
	}
}

// endReasonOf is sessions.end_reason of id as stored, and whether it is NULL.
func endReasonOf(t *testing.T, path, id string) (string, bool) {
	t.Helper()
	var v sql.NullString
	if err := rawDB(t, path).QueryRow(`SELECT end_reason FROM sessions WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v.String, !v.Valid
}

// Ruling 117: an ended session keeps why it ended.
func TestSessionEndReasonRoundTrip(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	want := model.Session{ID: "s1", Agent: "claude", Pane: "%4", State: model.StateEnded, EndReason: "other"}
	upsert(t, s, want)
	if got := get(t, s, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("after UpsertSession:\n got %+v\nwant %+v", got, want)
	}
	// An update that leaves it ended (an ignored SessionEnd) keeps it.
	err := s.UpdateSession(ctx, "s1", func(cur model.Session, _ bool) model.Session {
		cur.LastUpdate = "still ended"
		return cur
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "s1"); got.EndReason != "other" {
		t.Errorf("EndReason after an update = %q, want other", got.EndReason)
	}
	list, err := s.ListSessions(ctx, store.ListFilter{IncludeEnded: true})
	if err != nil || len(list) != 1 || list[0].EndReason != "other" {
		t.Errorf("ListSessions = %+v, %v; want the end reason", list, err)
	}
	upsert(t, s, model.Session{ID: "s2", Agent: "claude", State: model.StateEnded})
	if _, isNull := endReasonOf(t, path, "s2"); !isNull {
		t.Error("an empty end reason is not stored as NULL")
	}
}

func TestOldUpsertKeepsEndReason(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	upsert(t, s, model.Session{ID: "lead", Agent: "claude", State: model.StateEnded, EndReason: "other"})
	const at = "2026-11-14T10:00:00.000000000Z"
	plan3Session(t, rawDB(t, path), model.StateEnded, at)
	if got := get(t, s, "lead"); got.EndReason != "other" || got.TaskID != "T-1" {
		t.Errorf("after Plan 3's upsert: end reason %q task %q, want other T-1", got.EndReason, got.TaskID)
	}
	plan1Session(t, rawDB(t, path), "lead", "%9", model.StateEnded, at)
	if got := get(t, s, "lead"); got.EndReason != "other" || got.Pane != "%9" {
		t.Errorf("after Plan 1's upsert: end reason %q pane %q", got.EndReason, got.Pane)
	}
}

// A session that leaves ended (Ruling 27: it comes back on real activity,
// and the state machine keeps every other field) loses its end reason, as
// does one marked stale; ending others in a pane records none.
func TestSessionEndReasonClearedOnResume(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	for _, st := range []model.State{model.StateWorking, model.StateIdle, model.StateStale} {
		id := "s-" + string(st)
		upsert(t, s, model.Session{ID: id, Agent: "claude", Pane: "%1", State: model.StateEnded, EndReason: "other"})
		err := s.UpdateSession(ctx, id, func(cur model.Session, _ bool) model.Session {
			cur.State = st // cur.EndReason is still "other"
			return cur
		})
		if err != nil {
			t.Fatal(err)
		}
		if v, isNull := endReasonOf(t, path, id); !isNull {
			t.Errorf("%s: end_reason = %q after leaving ended, want NULL", id, v)
		}
		if got := get(t, s, id); got.EndReason != "" {
			t.Errorf("%s: EndReason = %q, want empty", id, got.EndReason)
		}
	}
	upsert(t, s, model.Session{ID: "live", Agent: "claude", Pane: "%7", State: model.StateIdle})
	if err := s.EndOthersInPane(ctx, "%7", "new", reviewT0); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "live"); got.State != model.StateEnded || got.EndReason != "" {
		t.Errorf("ended by another session in its pane: %s %q, want ended with no reason", got.State, got.EndReason)
	}
}

// The reason comes from a hook payload: 1 to 64 bytes of [a-z0-9_] are kept,
// anything else is stored as other.
func TestSessionEndReasonNormalized(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	for i, c := range []struct{ in, want string }{
		{"prompt_input_exit", "prompt_input_exit"},
		{"other", "other"},
		{"clear", "clear"},
		{"logout", "logout"},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{strings.Repeat("a", 65), "other"},
		{"Other", "other"},
		{"prompt input exit", "other"},
		{"x'; DROP TABLE sessions; --", "other"},
		{"\x1b[31mred", "other"},
		{"exit\n", "other"},
		{"caf\u00e9", "other"},
		{"a-b", "other"},
	} {
		id := fmt.Sprintf("s%d", i)
		upsert(t, s, model.Session{ID: id, Agent: "claude", State: model.StateEnded, EndReason: c.in})
		if got, isNull := endReasonOf(t, path, id); isNull || got != c.want {
			t.Errorf("end reason %q stored as %q (NULL %v), want %q", c.in, got, isNull, c.want)
		}
	}
	// A session that is not ended has none, whatever it carries.
	upsert(t, s, model.Session{ID: "live", Agent: "claude", State: model.StateWorking, EndReason: "other"})
	if v, isNull := endReasonOf(t, path, "live"); !isNull {
		t.Errorf("a working session stored end reason %q, want NULL", v)
	}
}

func TestCompleteTaskOpensThenUpdates(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	createTask(t, s, newTask("Fix the auth refresh"))
	est := time.FixedZone("EST", -5*3600)
	first := reviewCall("T-1", "s1", "Refresh tokens are renewed before expiry.", reviewT0.In(est))
	got, updated := completeReview(t, s, first)
	if updated {
		t.Error("first CompleteTask: updated = true, want false")
	}
	want := model.Review{
		N: 1, ID: "R-1", TaskID: "T-1", SessionID: "s1", Summary: first.Summary,
		Tests: model.TestsPassed, TestsDetail: first.TestsDetail, Status: model.ReviewPending,
		CreatedAt: reviewT0, UpdatedAt: reviewT0,
	}
	assertReview(t, got, want)
	assertReview(t, getReview(t, s, "R-1"), want)

	later := reviewT0.Add(time.Hour + 7)
	second := model.Review{
		TaskID: "T-1", SessionID: "s2", Summary: "Also covers the retry path.",
		Tests: model.TestsFailed, TestsDetail: "1 of 40 failed", CreatedAt: later,
	}
	got, updated = completeReview(t, s, second)
	if !updated {
		t.Error("second CompleteTask: updated = false, want true")
	}
	want.SessionID, want.Summary, want.Tests, want.TestsDetail = "s2", second.Summary, model.TestsFailed, "1 of 40 failed"
	want.UpdatedAt = later
	assertReview(t, got, want)
	assertReview(t, getReview(t, s, "R-1"), want)
	if n := countReviews(t, path); n != 1 {
		t.Errorf("reviews = %d, want 1", n)
	}
}

func TestCompleteTaskAfterCloseOpensNew(t *testing.T) {
	for _, c := range []store.ReviewClose{
		{Status: model.ReviewChanges, Notes: "Add a test for the retry.", Via: model.AnsweredPopup},
		{Status: model.ReviewApproved, How: model.ApprovePR, PRURL: "https://github.com/o/r/pull/9", Via: model.AnsweredCLI},
		mergedClose,
	} {
		t.Run(string(c.Status), func(t *testing.T) {
			s := open(t, dbPath(t))
			first := reviewFor(t, s, "Fix the auth refresh")
			closeReview(t, s, first.ID, c, reviewT0.Add(time.Minute))
			got, updated := completeReview(t, s, reviewCall("T-1", "s1", "Again.", reviewT0.Add(time.Hour)))
			if updated || got.ID != "R-2" || got.Status != model.ReviewPending || got.Summary != "Again." {
				t.Errorf("CompleteTask after %s = %s %s %q updated %v; want a new pending R-2",
					c.Status, got.ID, got.Status, got.Summary, updated)
			}
			if r1 := getReview(t, s, "R-1"); r1.Status != c.Status || r1.Summary != first.Summary {
				t.Errorf("R-1 after the new review = %s %q, want %s unchanged", r1.Status, r1.Summary, c.Status)
			}
		})
	}
}

func TestCompleteTaskNeedsLiveTask(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	dropped := newTask("Abandoned")
	dropped.Status = model.TaskDropped
	createTask(t, s, dropped)
	for _, id := range []string{"T-9", "T-1", ""} {
		_, _, err := s.CompleteTask(context.Background(), reviewCall(id, "s1", "Done.", reviewT0))
		if !errors.Is(err, store.ErrNoTask) {
			t.Errorf("CompleteTask(%q) = %v, want ErrNoTask", id, err)
		}
	}
	if n := countReviews(t, path); n != 0 {
		t.Errorf("reviews = %d, want none", n)
	}
}

// A bad tests status is refused by the table and stores nothing.
func TestReviewCompleteRejectsUnknownTestsStatus(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	createTask(t, s, newTask("Fix"))
	r := reviewCall("T-1", "s1", "Done.", reviewT0)
	r.Tests = "ok"
	if _, _, err := s.CompleteTask(context.Background(), r); err == nil {
		t.Error("CompleteTask with tests ok succeeded, want an error")
	}
	if n := countReviews(t, path); n != 0 {
		t.Errorf("reviews = %d, want none", n)
	}
}

func TestConcurrentCompletesOneReview(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	createTask(t, hs[0], newTask("Fix the auth refresh"))
	const goroutines = 20
	type result struct {
		r       model.Review
		updated bool
		err     error
	}
	results := make([]result, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Go(func() {
			<-start
			res := &results[g]
			res.err = retryBusy(func() error {
				var err error
				res.r, res.updated, err = hs[g%len(hs)].CompleteTask(context.Background(),
					reviewCall("T-1", fmt.Sprintf("s%d", g), fmt.Sprintf("Summary %d", g), reviewT0.Add(time.Duration(g))))
				return err
			}, busyAtBegin)
		})
	}
	close(start)
	wg.Wait()
	opened := 0
	for g, res := range results {
		if res.err != nil {
			t.Errorf("goroutine %d: %v", g, res.err)
			continue
		}
		if res.r.ID != "R-1" {
			t.Errorf("goroutine %d: review %s, want R-1", g, res.r.ID)
		}
		if !res.updated {
			opened++
		}
	}
	if opened != 1 {
		t.Errorf("%d calls opened a review, want exactly 1", opened)
	}
	if n := countReviews(t, path); n != 1 {
		t.Errorf("reviews = %d, want 1", n)
	}
}

func TestConcurrentCompletesDistinctIDs(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	const tasks = 20
	for i := range tasks {
		createTask(t, hs[0], newTask(fmt.Sprintf("Task %d", i)))
	}
	var mu sync.Mutex
	got := map[string]model.Review{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range tasks {
		wg.Go(func() {
			<-start
			var r model.Review
			err := retryBusy(func() error {
				var err error
				r, _, err = hs[g%len(hs)].CompleteTask(context.Background(),
					reviewCall(fmt.Sprintf("T-%d", g+1), "s", "Done.", reviewT0))
				return err
			}, busyAtBegin)
			if err != nil {
				t.Errorf("task %d: %v", g+1, err)
				return
			}
			mu.Lock()
			got[r.ID] = r
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	tasksSeen := map[string]bool{}
	for n := int64(1); n <= tasks; n++ {
		r, ok := got[fmt.Sprintf("R-%d", n)]
		if !ok || r.N != n {
			t.Errorf("R-%d: present %v, n %d", n, ok, r.N)
		}
		tasksSeen[r.TaskID] = true
	}
	if len(got) != tasks || len(tasksSeen) != tasks {
		t.Errorf("%d distinct reviews over %d tasks, want %d each", len(got), len(tasksSeen), tasks)
	}
}

func TestReviewRoundTrip(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	est := time.FixedZone("EST", -5*3600)
	opened := time.Date(2026, 11, 14, 4, 0, 0, 1, est)
	closed := time.Date(2026, 11, 14, 5, 30, 0, 999_999_999, est)

	createTask(t, s, newTask("Approve me"))
	in := model.Review{
		N: 77, ID: "R-77", Status: model.ReviewMerged, UpdatedAt: closed, // ignored
		TaskID: "T-1", SessionID: "sess-a", Summary: "Line one.\nLine two.",
		Tests: model.TestsNotRun, TestsDetail: "no tests for docs", CreatedAt: opened,
	}
	completeReview(t, s, in)
	c := store.ReviewClose{
		Status: model.ReviewApproved, How: model.ApprovePR, DiffStat: " a.go | 1 +\n 1 file changed",
		Head: "0123456789abcdef", PRURL: "https://github.com/o/r/pull/12", Via: model.AnsweredSSH,
	}
	got, _ := closeReview(t, s, "R-1", c, closed)
	want := model.Review{
		N: 1, ID: "R-1", TaskID: "T-1", SessionID: "sess-a", Summary: in.Summary,
		Tests: model.TestsNotRun, TestsDetail: in.TestsDetail, Status: model.ReviewApproved,
		ApprovedHow: model.ApprovePR, DiffStat: c.DiffStat, Head: c.Head, PRURL: c.PRURL,
		ClosedVia: model.AnsweredSSH, CreatedAt: opened, UpdatedAt: closed, ClosedAt: closed,
	}
	assertReview(t, got, want)
	assertReview(t, getReview(t, s, "R-1"), want)

	createTask(t, s, newTask("Change me"))
	completeReview(t, s, reviewCall("T-2", "sess-b", "First try.", opened))
	cr := store.ReviewClose{Status: model.ReviewChanges, Notes: "Split the commit.\nThen ask again.", Via: model.AnsweredPopup}
	closeReview(t, s, "R-2", cr, closed)
	if err := s.SetReviewNoteMessage(ctx, "R-2", "M-12"); err != nil {
		t.Fatalf("SetReviewNoteMessage: %v", err)
	}
	assertReview(t, getReview(t, s, "R-2"), model.Review{
		N: 2, ID: "R-2", TaskID: "T-2", SessionID: "sess-b", Summary: "First try.",
		Tests: model.TestsPassed, TestsDetail: "go test ./... passed", Status: model.ReviewChanges,
		Notes: cr.Notes, NoteMessage: "M-12", ClosedVia: model.AnsweredPopup,
		CreatedAt: opened, UpdatedAt: closed, ClosedAt: closed,
	})
	if _, ok, err := s.GetReview(ctx, "R-3"); ok || err != nil {
		t.Errorf("GetReview(R-3) = %v, %v; want not found", ok, err)
	}
}

func TestCloseReviewTable(t *testing.T) {
	now := reviewT0.Add(time.Hour + 3)
	everything := func(st model.ReviewStatus, how model.ApproveHow, via model.AnsweredVia) store.ReviewClose {
		return store.ReviewClose{
			Status: st, How: how, DiffStat: "stat", Head: "head", PRURL: "https://x/pull/1", Notes: "note", Via: via,
		}
	}
	for _, c := range []struct {
		in   store.ReviewClose
		want func(*model.Review)
	}{
		{everything(model.ReviewApproved, model.ApprovePR, model.AnsweredPopup), func(r *model.Review) {
			r.ApprovedHow, r.DiffStat, r.Head, r.PRURL, r.ClosedVia = model.ApprovePR, "stat", "head", "https://x/pull/1", model.AnsweredPopup
		}},
		{everything(model.ReviewMerged, model.ApproveMerge, model.AnsweredCLI), func(r *model.Review) {
			r.ApprovedHow, r.DiffStat, r.Head, r.PRURL, r.ClosedVia = model.ApproveMerge, "stat", "head", "https://x/pull/1", model.AnsweredCLI
		}},
		{everything(model.ReviewChanges, model.ApproveMerge, model.AnsweredSSH), func(r *model.Review) {
			r.Notes, r.ClosedVia = "note", model.AnsweredSSH
		}},
		{everything(model.ReviewDropped, model.ApproveMerge, model.AnsweredCLI), func(r *model.Review) {
			r.ClosedVia = model.AnsweredCLI
		}},
	} {
		t.Run(string(c.in.Status), func(t *testing.T) {
			path := dbPath(t)
			s := open(t, path)
			pending := reviewFor(t, s, "Close me")
			got, _ := closeReview(t, s, pending.ID, c.in, now)
			want := pending
			want.Status, want.UpdatedAt, want.ClosedAt = c.in.Status, now, now
			c.want(&want)
			assertReview(t, got, want)
			assertReview(t, getReview(t, s, pending.ID), want)
			var closedAt, updatedAt string
			if err := rawDB(t, path).QueryRow(`SELECT closed_at, updated_at FROM reviews WHERE id = ?`, pending.ID).
				Scan(&closedAt, &updatedAt); err != nil {
				t.Fatal(err)
			}
			if w := now.UTC().Format("2006-01-02T15:04:05.000000000Z"); closedAt != w || updatedAt != w {
				t.Errorf("stored closed_at, updated_at = %s, %s; want %s", closedAt, updatedAt, w)
			}
		})
	}

	// Refusals change nothing.
	s := open(t, dbPath(t))
	pending := reviewFor(t, s, "Keep me pending")
	for _, c := range []store.ReviewClose{
		{Status: model.ReviewPending, Via: model.AnsweredCLI},
		{Status: "done", Via: model.AnsweredCLI},
		{Status: model.ReviewApproved, How: model.ApprovePR, Via: model.AnsweredNtfy}, // the table refuses it
	} {
		if _, _, err := s.CloseReview(context.Background(), pending.ID, c, now); err == nil {
			t.Errorf("CloseReview(%s via %s) succeeded, want an error", c.Status, c.Via)
		}
	}
	assertReview(t, getReview(t, s, pending.ID), pending)
}

func TestCloseReviewTwiceIsRefused(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	r := reviewFor(t, s, "Merge me")
	now := reviewT0.Add(time.Hour)
	first, _ := closeReview(t, s, r.ID, mergedClose, now)
	for _, c := range []store.ReviewClose{
		{Status: model.ReviewDropped, Via: model.AnsweredCLI},
		{Status: model.ReviewChanges, Notes: "x", Via: model.AnsweredCLI},
		mergedClose,
	} {
		if _, _, err := s.CloseReview(ctx, r.ID, c, now.Add(time.Hour)); !errors.Is(err, store.ErrReviewClosed) {
			t.Errorf("second CloseReview(%s) = %v, want ErrReviewClosed", c.Status, err)
		}
	}
	assertReview(t, getReview(t, s, r.ID), first)
	if task := getTask(t, s, "T-1"); task.Status != model.TaskBacklog {
		t.Errorf("task status = %s after a refused drop, want backlog", task.Status)
	}
	if _, _, err := s.CloseReview(ctx, "R-9", mergedClose, now); !errors.Is(err, store.ErrNoReview) {
		t.Errorf("CloseReview(R-9) = %v, want ErrNoReview", err)
	}
	if err := s.SetReviewNoteMessage(ctx, "R-9", "M-1"); !errors.Is(err, store.ErrNoReview) {
		t.Errorf("SetReviewNoteMessage(R-9) = %v, want ErrNoReview", err)
	}
	// The note's message belongs to a change request only.
	if err := s.SetReviewNoteMessage(ctx, r.ID, "M-1"); err == nil {
		t.Error("SetReviewNoteMessage on a merged review succeeded, want an error")
	}
	if got := getReview(t, s, r.ID); got.NoteMessage != "" {
		t.Errorf("NoteMessage = %q, want none", got.NoteMessage)
	}
}

func TestConcurrentClosesOneWins(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	ctx := context.Background()
	const rounds = 200
	now := reviewT0.Add(time.Hour)
	for round := range rounds {
		task := createTask(t, hs[0], newTask(fmt.Sprintf("Race %d", round)))
		r, _ := completeReview(t, hs[0], reviewCall(task.ID, "s", "Done.", reviewT0))
		closes := []store.ReviewClose{mergedClose, {Status: model.ReviewDropped, Via: model.AnsweredPopup}}
		errs := make([]error, len(closes))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, c := range closes {
			wg.Go(func() {
				<-start
				errs[i] = retryBusy(func() error {
					_, _, err := hs[(round+i)%len(hs)].CloseReview(ctx, r.ID, c, now)
					return err
				}, busyAtBegin)
			})
		}
		close(start)
		wg.Wait()
		var won []model.ReviewStatus
		for i, err := range errs {
			switch {
			case err == nil:
				won = append(won, closes[i].Status)
			case !errors.Is(err, store.ErrReviewClosed):
				t.Fatalf("round %d, %s: %v", round, closes[i].Status, err)
			}
		}
		if len(won) != 1 {
			t.Fatalf("round %d: %d closes won, want exactly 1", round, len(won))
		}
		got := getReview(t, hs[0], r.ID)
		taskNow := getTask(t, hs[0], task.ID)
		if got.Status != won[0] || (taskNow.Status == model.TaskDropped) != (won[0] == model.ReviewDropped) {
			t.Fatalf("round %d: %s won but the review is %s and the task %s", round, won[0], got.Status, taskNow.Status)
		}
	}
}

// taskTicket stores an open ticket of task taskID, raised by session s1.
func taskTicket(t *testing.T, s *store.Store, question string, mode model.Mode, taskID string, at time.Time) model.Decision {
	t.Helper()
	d := ticket(t, question, mode, "s1", at)
	d.TaskID = taskID
	return create(t, s, d)
}

func TestApproveExpiresTaskAssumptions(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	r := reviewFor(t, s, "Fix the auth refresh")
	createTask(t, s, newTask("Other task"))
	now := t0.Add(2 * time.Hour)

	a1 := taskTicket(t, s, "Assume the v2 token endpoint?", model.ModeAssume, "T-1", t0)
	a2 := taskTicket(t, s, "Assume a 5 minute refresh margin?", model.ModeAssume, "T-1", t0)
	blocking := taskTicket(t, s, "Rotate the signing key now?", model.ModeBlocking, "T-1", t0)
	answered := taskTicket(t, s, "Keep the old cookie name?", model.ModeBlocking, "T-1", t0)
	answer(t, s, answered.ID, keyA, t0.Add(time.Minute))
	// Past its deadline at now: no longer open, left to the sweep.
	late := taskTicket(t, s, "Assume the staging hostname?", model.ModeAssume, "T-1", now.Add(-assumeExpiry-time.Second))
	other := taskTicket(t, s, "Assume the other default?", model.ModeAssume, "T-2", t0)

	_, expired := closeReview(t, s, r.ID, mergedClose, now)
	if expired != 2 {
		t.Errorf("expired = %d, want 2", expired)
	}
	for _, want := range []struct {
		d      model.Decision
		status model.DecisionStatus
	}{
		{a1, model.StatusExpired},
		{a2, model.StatusExpired},
		{blocking, model.StatusOpen},
		{answered, model.StatusAnswered},
		{late, model.StatusOpen},
		{other, model.StatusOpen},
	} {
		got := getDecision(t, s, want.d.ID)
		if got.Status != want.status {
			t.Errorf("%s (%s) = %s, want %s", got.ID, got.Question, got.Status, want.status)
		}
		if want.status == model.StatusExpired && !got.ClosedAt.Equal(now) {
			t.Errorf("%s closed at %v, want %v", got.ID, got.ClosedAt, now)
		}
	}
	found, err := s.SearchDecisions(ctx, store.PrecedentQuery{Text: "assume token endpoint refresh margin", Repo: "/src/curia", TaskID: "T-1", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range found {
		if d.ID == a1.ID || d.ID == a2.ID {
			t.Errorf("SearchDecisions found expired %s", d.ID)
		}
	}

	// An approval (not a merge) expires them too.
	r2 := reviewFor(t, s, "Third task")
	a3 := taskTicket(t, s, "Assume the third default?", model.ModeAssume, r2.TaskID, t0)
	_, expired = closeReview(t, s, r2.ID, store.ReviewClose{Status: model.ReviewApproved, How: model.ApprovePR, Via: model.AnsweredCLI}, now)
	if got := getDecision(t, s, a3.ID); expired != 1 || got.Status != model.StatusExpired {
		t.Errorf("approved: expired %d, %s %s; want 1 and expired", expired, a3.ID, got.Status)
	}
}

// When the expiry fails, the approval is rolled back with it.
func TestReviewApproveRollsBackWithItsExpiry(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	r := reviewFor(t, s, "Fix")
	a := taskTicket(t, s, "Assume the v2 endpoint?", model.ModeAssume, "T-1", t0)
	rawExec(t, path, `CREATE TRIGGER review_test_fail BEFORE UPDATE OF status ON decisions
BEGIN SELECT RAISE(ABORT, 'forced failure'); END`)
	if _, _, err := s.CloseReview(context.Background(), r.ID, mergedClose, t0.Add(time.Hour)); err == nil {
		t.Fatal("CloseReview succeeded past a failing expiry")
	}
	assertReview(t, getReview(t, s, r.ID), r)
	if got := getDecision(t, s, a.ID); got.Status != model.StatusOpen {
		t.Errorf("%s = %s, want open", a.ID, got.Status)
	}
}

func TestChangesRequestedExpiresNothing(t *testing.T) {
	for _, c := range []store.ReviewClose{
		{Status: model.ReviewChanges, Notes: "Add a test.", Via: model.AnsweredPopup},
		{Status: model.ReviewDropped, Via: model.AnsweredPopup},
	} {
		t.Run(string(c.Status), func(t *testing.T) {
			s := open(t, dbPath(t))
			r := reviewFor(t, s, "Fix")
			a := taskTicket(t, s, "Assume the v2 endpoint?", model.ModeAssume, "T-1", t0)
			_, expired := closeReview(t, s, r.ID, c, t0.Add(time.Hour))
			if got := getDecision(t, s, a.ID); expired != 0 || got.Status != model.StatusOpen {
				t.Errorf("expired %d, %s %s; want 0 and open", expired, a.ID, got.Status)
			}
		})
	}
}

func TestDropSetsTaskDropped(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	r := reviewFor(t, s, "Drop me")
	now := reviewT0.Add(time.Hour)
	got, _ := closeReview(t, s, r.ID, store.ReviewClose{Status: model.ReviewDropped, Via: model.AnsweredCLI}, now)
	if got.Status != model.ReviewDropped {
		t.Errorf("review = %s, want dropped", got.Status)
	}
	if task := getTask(t, s, "T-1"); task.Status != model.TaskDropped || !task.UpdatedAt.Equal(now) {
		t.Errorf("task = %s updated %v, want dropped at %v", task.Status, task.UpdatedAt, now)
	}

	// A failing second statement rolls both back.
	createTask(t, s, newTask("Keep me"))
	r2, _ := completeReview(t, s, reviewCall("T-2", "s", "Done.", reviewT0))
	rawExec(t, path, `CREATE TRIGGER review_test_fail BEFORE UPDATE OF status ON tasks
BEGIN SELECT RAISE(ABORT, 'forced failure'); END`)
	if _, _, err := s.CloseReview(context.Background(), r2.ID,
		store.ReviewClose{Status: model.ReviewDropped, Via: model.AnsweredCLI}, now); err == nil {
		t.Fatal("CloseReview(dropped) succeeded past a failing task update")
	}
	assertReview(t, getReview(t, s, r2.ID), r2)
	if task := getTask(t, s, "T-2"); task.Status != model.TaskBacklog {
		t.Errorf("T-2 = %s, want backlog", task.Status)
	}
}

func TestLatestReviewsIsNewestPerTask(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := reviewT0.Add(time.Minute)
	reviewFor(t, s, "One") // R-1 of T-1
	closeReview(t, s, "R-1", store.ReviewClose{Status: model.ReviewChanges, Notes: "x", Via: model.AnsweredCLI}, now)
	reviewFor(t, s, "Two") // R-2 of T-2
	closeReview(t, s, "R-2", mergedClose, now)
	completeReview(t, s, reviewCall("T-1", "s", "Again.", now)) // R-3 of T-1
	createTask(t, s, newTask("Three, never completed"))

	got, err := s.LatestReviews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["T-1"].ID != "R-3" || got["T-1"].Status != model.ReviewPending ||
		got["T-2"].ID != "R-2" || got["T-2"].Status != model.ReviewMerged {
		t.Errorf("LatestReviews = %v, want T-1: pending R-3, T-2: merged R-2", got)
	}
	assertReview(t, got["T-1"], getReview(t, s, "R-3"))
}

func TestPendingReviewsSkipsDroppedTasks(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	reviewFor(t, s, "One")
	reviewFor(t, s, "Two")
	reviewFor(t, s, "Three")
	closeReview(t, s, "R-3", mergedClose, reviewT0.Add(time.Minute))
	createTask(t, s, newTask("Four, never completed"))
	rawExec(t, path, `UPDATE tasks SET status = 'dropped' WHERE id = 'T-2'`)

	n, err := s.PendingReviews(ctx)
	if err != nil || n != 1 {
		t.Errorf("PendingReviews = %d, %v; want 1", n, err)
	}
	got, err := s.PendingReviewTasks(ctx)
	if err != nil || !reflect.DeepEqual(got, map[string]string{"T-1": "R-1"}) {
		t.Errorf("PendingReviewTasks = %v, %v; want T-1: R-1", got, err)
	}
}

func TestListReviewsFilters(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := reviewT0.Add(time.Minute)
	reviewFor(t, s, "One") // R-1, T-1
	closeReview(t, s, "R-1", store.ReviewClose{Status: model.ReviewChanges, Notes: "x", Via: model.AnsweredCLI}, now)
	reviewFor(t, s, "Two")                                      // R-2, T-2, pending
	completeReview(t, s, reviewCall("T-1", "s", "Again.", now)) // R-3, T-1, pending
	reviewFor(t, s, "Three")                                    // R-4, T-3
	closeReview(t, s, "R-4", mergedClose, now)
	reviewFor(t, s, "Four") // R-5, T-4
	closeReview(t, s, "R-5", store.ReviewClose{Status: model.ReviewApproved, How: model.ApprovePR, Via: model.AnsweredCLI}, now)

	for _, c := range []struct {
		name string
		f    store.ReviewFilter
		want []string
	}{
		{"all", store.ReviewFilter{}, []string{"R-1", "R-2", "R-3", "R-4", "R-5"}},
		{"pending", store.ReviewFilter{Statuses: []model.ReviewStatus{model.ReviewPending}}, []string{"R-2", "R-3"}},
		{
			"approved or merged",
			store.ReviewFilter{Statuses: []model.ReviewStatus{model.ReviewMerged, model.ReviewApproved}},
			[]string{"R-4", "R-5"},
		},
		{"dropped", store.ReviewFilter{Statuses: []model.ReviewStatus{model.ReviewDropped}}, nil},
		{"task", store.ReviewFilter{TaskID: "T-1"}, []string{"R-1", "R-3"}},
		{"task and status", store.ReviewFilter{TaskID: "T-1", Statuses: []model.ReviewStatus{model.ReviewChanges}}, []string{"R-1"}},
		{"unknown task", store.ReviewFilter{TaskID: "T-9"}, nil},
		{"newest two", store.ReviewFilter{Limit: 2}, []string{"R-4", "R-5"}},
		{"newest pending", store.ReviewFilter{Statuses: []model.ReviewStatus{model.ReviewPending}, Limit: 1}, []string{"R-3"}},
		{"newest of a task", store.ReviewFilter{TaskID: "T-1", Limit: 1}, []string{"R-3"}},
		{"limit above the count", store.ReviewFilter{TaskID: "T-1", Limit: 10}, []string{"R-1", "R-3"}},
	} {
		got, err := s.ListReviews(ctx, c.f)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if ids := reviewIDs(got); !slices.Equal(ids, c.want) {
			t.Errorf("%s: ListReviews = %v, want %v", c.name, ids, c.want)
		}
	}
	if all, _ := s.ListReviews(ctx, store.ReviewFilter{}); len(all) > 0 {
		assertReview(t, all[0], getReview(t, s, "R-1"))
	}
}

func TestTaskDecisionsByTask(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	d1 := taskTicket(t, s, "First of T-1?", model.ModeBlocking, "T-1", t0)
	taskTicket(t, s, "Only of T-2?", model.ModeAssume, "T-2", t0)
	d3 := taskTicket(t, s, "Second of T-1?", model.ModeAssume, "T-1", t0)
	createRule(t, s, globalRule("Always run the linter before committing."), t0)
	d5 := taskTicket(t, s, "Third of T-1?", model.ModeBlocking, "T-1", t0)
	answer(t, s, d5.ID, keyA, t0.Add(time.Minute))
	create(t, s, ticket(t, "Of no task?", model.ModeBlocking, "s9", t0))

	got, err := s.TaskDecisions(ctx, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	if ids := decisionIDs(got); !slices.Equal(ids, []string{d1.ID, d3.ID, d5.ID}) {
		t.Fatalf("TaskDecisions(T-1) = %v, want %s %s %s", ids, d1.ID, d3.ID, d5.ID)
	}
	assertDecision(t, got[2], getDecision(t, s, d5.ID))
	for _, id := range []string{"", "T-9"} {
		if got, err := s.TaskDecisions(ctx, id); err != nil || len(got) != 0 {
			t.Errorf("TaskDecisions(%q) = %v, %v; want none", id, decisionIDs(got), err)
		}
	}
}

func TestTaskRun(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	start := reviewT0.Add(-3 * time.Hour)
	for _, sess := range []model.Session{
		{ID: "lead", State: model.StateIdle, TaskID: "T-1", StartedAt: start.Add(time.Minute)},
		{ID: "mate1", State: model.StateWorking, TaskID: "T-1", StartedAt: start},
		{ID: "mate2", State: model.StateEnded, TaskID: "T-1", StartedAt: start.Add(2 * time.Minute)},
		{ID: "other", State: model.StateIdle, TaskID: "T-2", StartedAt: start.Add(-time.Hour)},
		{ID: "solo", State: model.StateIdle, StartedAt: start.Add(-time.Hour)},
	} {
		sess.Agent = "claude"
		upsert(t, s, sess)
	}
	rawExec(t, path, `UPDATE sessions SET team_id = 'api', team_role = 'lead' WHERE id = 'lead'`)
	rawExec(t, path, `UPDATE sessions SET team_id = 'api', team_role = 'teammate' WHERE id IN ('mate1', 'mate2')`)
	rawExec(t, path, `UPDATE sessions SET cost_usd = 1.5 WHERE id = 'lead'`)
	rawExec(t, path, `UPDATE sessions SET cost_usd = 2.0 WHERE id = 'mate2'`)
	rawExec(t, path, `UPDATE sessions SET cost_usd = 9.0 WHERE id IN ('other', 'solo')`)

	got, err := s.TaskRun(ctx, "T-1")
	if err != nil {
		t.Fatal(err)
	}
	want := store.TaskRun{Sessions: 3, Teammates: 2, Started: start, CostUSD: 3.5, CostKnown: true}
	if !got.Started.Equal(want.Started) {
		t.Errorf("Started = %v, want %v", got.Started, want.Started)
	}
	got.Started, want.Started = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("TaskRun(T-1) = %+v, want %+v", got, want)
	}

	rawExec(t, path, `UPDATE sessions SET cost_usd = NULL WHERE task_id = 'T-1'`)
	got, err = s.TaskRun(ctx, "T-1")
	if err != nil || got.CostKnown || got.CostUSD != 0 || got.Sessions != 3 {
		t.Errorf("TaskRun(T-1) without costs = %+v, %v; want 3 sessions, cost unknown", got, err)
	}
	for _, id := range []string{"T-9", ""} {
		if got, err := s.TaskRun(ctx, id); err != nil || got != (store.TaskRun{}) {
			t.Errorf("TaskRun(%q) = %+v, %v; want zero", id, got, err)
		}
	}
}

// Every list and count of reviews, and the task reads, use an index: no
// statement scans a table, and each reads through one of the indexes named.
func TestReviewQueriesUseIndexes(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	type query struct {
		name, sql string
		args      []any
		indexes   []string // any of them
	}
	pending := []string{"reviews_one_pending", "reviews_status"}
	qs := []query{
		{"PendingReviewTasks", store.PendingReviewTasksQuery, nil, pending},
		{"PendingReviews", store.PendingReviewsQuery, nil, pending},
		{"LatestReviews", store.LatestReviewsQuery, nil, []string{"COVERING INDEX reviews_task"}},
		{"TaskDecisions", store.TaskDecisionsQuery, []any{"T-1"}, []string{"decisions_task"}},
		{"TaskRun", store.TaskRunQuery, []any{"T-1"}, []string{"sessions_task"}},
		{"expiry", store.ExpireTaskAssumptionsQuery, []any{"x", "T-1", "x"}, []string{"decisions_task", "decisions_status_created"}},
		{"pending review of a task", store.PendingReviewOfTaskQuery, []any{"x", "x", "x", "x", "x", "T-1"}, []string{"reviews_one_pending"}},
	}
	for name, f := range map[string]store.ReviewFilter{
		"list by task":            {TaskID: "T-1"},
		"list by task, newest":    {TaskID: "T-1", Limit: 3},
		"list by status":          {Statuses: []model.ReviewStatus{model.ReviewPending, model.ReviewMerged}},
		"list by status, newest":  {Statuses: []model.ReviewStatus{model.ReviewPending}, Limit: 3},
		"list by task and status": {TaskID: "T-1", Statuses: []model.ReviewStatus{model.ReviewPending}},
	} {
		q, args := store.ListReviewsQuery(f)
		qs = append(qs, query{name, q, args, []string{"reviews_task", "reviews_status", "reviews_one_pending"}})
	}
	for _, q := range qs {
		plan := queryPlan(t, db, q.sql, q.args...)
		if !slices.ContainsFunc(q.indexes, func(idx string) bool { return strings.Contains(plan, idx) }) {
			t.Errorf("%s uses none of %v:\n%s", q.name, q.indexes, plan)
		}
		for line := range strings.SplitSeq(plan, "\n") {
			if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "INDEX") && !strings.HasPrefix(line, "SCAN (subquery") {
				t.Errorf("%s scans a table: %s\n%s", q.name, line, plan)
			}
		}
		t.Logf("%s:\n%s", q.name, plan)
	}
}

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
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// taskT0 is when most test tasks are created; its nanoseconds check that
// times survive to the nanosecond.
var taskT0 = time.Date(2026, 11, 13, 9, 0, 0, 123_456_789, time.UTC)

// launchHold is how long a launch claim holds a task (the setup limit plus
// one minute, plan section 4.6).
const launchHold = 11 * time.Minute

// newTask is a valid local backlog task in /src/curia.
func newTask(title string) model.Task {
	return model.Task{
		Title: title, Repo: "/src/curia", Status: model.TaskBacklog, Source: model.SourceLocal,
		CreatedAt: taskT0,
	}
}

func createTask(t *testing.T, s *store.Store, task model.Task) model.Task {
	t.Helper()
	got, err := s.CreateTask(context.Background(), task)
	if err != nil {
		t.Fatalf("CreateTask(%q): %v", task.Title, err)
	}
	return got
}

func getTask(t *testing.T, s *store.Store, id string) model.Task {
	t.Helper()
	got, ok, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask(%s): %v", id, err)
	}
	if !ok {
		t.Fatalf("GetTask(%s): not found", id)
	}
	return got
}

func countTasks(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := rawDB(t, path).QueryRow(`SELECT count(*) FROM tasks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertTask compares two tasks, times with Equal (the store returns them
// in UTC) and everything else with DeepEqual.
func assertTask(t *testing.T, got, want model.Task) {
	t.Helper()
	times := func(x *model.Task) map[string]*time.Time {
		return map[string]*time.Time{
			"CreatedAt": &x.CreatedAt, "UpdatedAt": &x.UpdatedAt, "LaunchedAt": &x.LaunchedAt,
			"PausedAt": &x.PausedAt, "CleanedAt": &x.CleanedAt,
		}
	}
	gt, wt := times(&got), times(&want)
	for name, g := range gt {
		if !g.Equal(*wt[name]) {
			t.Errorf("%s %s = %v, want %v", want.ID, name, *g, *wt[name])
		}
		*g, *wt[name] = time.Time{}, time.Time{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("task:\n got %+v\nwant %+v", got, want)
	}
}

func taskIDs(tasks []model.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, x := range tasks {
		out = append(out, x.ID)
	}
	return out
}

func TestMigrationAddsTasksAndMessages(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	for _, obj := range [][2]string{
		{"table", "tasks"},
		{"table", "messages"},
		{"index", "tasks_source_ref"},
		{"index", "sessions_task"},
		{"index", "messages_undelivered"},
		{"index", "messages_labels"},
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
	for _, col := range []string{"team_id", "team_role", "team_member"} {
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
	if n := countMigrations(t, path); n != 4 {
		t.Errorf("schema_migrations has %d rows, want 4", n)
	}
}

// The constraints of migration 0004, which no later task can change.
func TestMigrationConstraints(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	const at = "2026-11-13T09:00:00.000000000Z"
	insertTask := func(id, repo, status, source, ref string) error {
		_, err := db.Exec(`INSERT INTO tasks (id, title, repo, status, source, source_ref, created_at, updated_at)
VALUES (?, 't', ?, ?, ?, ?, ?, ?)`, id, repo, status, source, ref, at, at)
		return err
	}
	insertMessage := func(id, status, via string) error {
		_, err := db.Exec(`INSERT INTO messages (id, text, kind, routed_by, status, delivered_via, created_at)
VALUES (?, 'x', 'instruction', 'address', ?, ?, ?)`, id, status, via, at)
		return err
	}
	for _, c := range []struct {
		name string
		err  error
		ok   bool
	}{
		{"backlog task", insertTask("T-1", "/r", "backlog", "local", "a"), true},
		{"second local task with the same ref", insertTask("T-2", "/r", "ready", "local", "a"), true},
		{"running task", insertTask("T-3", "/r", "running", "local", ""), false},
		{"unknown source", insertTask("T-3", "/r", "ready", "github", ""), false},
		{"imported task", insertTask("T-3", "/r", "ready", "backlog_md", "f.md"), true},
		{"same file imported twice", insertTask("T-4", "/r", "ready", "backlog_md", "f.md"), false},
		{"same file from another repo", insertTask("T-4", "/r2", "ready", "backlog_md", "f.md"), true},
		{"linear task", insertTask("T-5", "/r", "dropped", "linear", "LIN-1"), true},
		{"canceled message", insertMessage("M-1", "canceled", ""), true},
		{"unknown message status", insertMessage("M-2", "sent", ""), false},
		{"applied message", insertMessage("M-2", "delivered", "applied"), true},
		{"unknown delivery path", insertMessage("M-3", "delivered", "ntfy"), false},
	} {
		if (c.err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.name, c.err, c.ok)
		}
	}
	// Every other message status passes.
	for _, s := range []string{"held", "queued", "delivered", "undone"} {
		if err := insertMessage("M-"+s, s, ""); err != nil {
			t.Errorf("message status %s: %v", s, err)
		}
	}
}

// plan2Database builds, at path, a database as Plan 2's binary leaves it:
// 0001 to 0003 applied, one session and one event written by Plan 1's
// statements (which Plan 2 kept) and one decision by Plan 2's insert.
func plan2Database(t *testing.T, path, at string) *sql.DB {
	t.Helper()
	db := plan1Database(t, path, at)
	for _, name := range []string{"0002_decisions.sql", "0003_typed_confirmation.sql"} {
		schema, err := os.ReadFile(filepath.Clean(filepath.Join("migrations", name)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(schema)); err != nil {
			t.Fatalf("building the Plan 2 database: %s: %v", name, err)
		}
		if _, err = db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, at); err != nil {
			t.Fatal(err)
		}
	}
	plan2Decision(t, db, "Postgres or Redis for the event store?", at)
	return db
}

// Plan 2's decision statements, copied from its decisions.go, so this test
// keeps describing what a Plan 2 binary runs after this package changes.
// Its session statements are Plan 1's (plan1Upsert and the rest).
const (
	plan2Insert = `
INSERT INTO decisions (
  id, session_id, task_id, repo, question, context, options, recommendation, rationale,
  reversibility, mode, assumption, scope, tags, status, differs_from, differs_why, args_hash,
  delivery, created_at, expires_at
) VALUES (
  'D-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM decisions), ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, 'open', ?, ?, ?, 'none', ?, ?
) RETURNING id`
	plan2Select = `
SELECT
  d.n, d.id, d.session_id, d.task_id, d.repo, d.question, d.context, d.options,
  d.recommendation, d.rationale, d.reversibility, d.mode, d.assumption, d.scope, d.tags, d.status,
  d.answer_key, d.answer, d.answer_reasoning, d.answered_via, d.precedent_of, d.differs_from,
  d.differs_why, d.withdraw_reason, d.superseded_by, d.args_hash, d.delivery, d.delivered_via,
  d.created_at, d.expires_at, d.answered_at, d.closed_at, d.superseded_at, d.delivered_at, d.confirmed_at
FROM decisions d WHERE d.id = ?`
	plan2Answer = `
UPDATE decisions SET status = ?, scope = ?, answer_key = ?, answer = ?, answer_reasoning = ?,
  answered_via = ?, delivery = ?, answered_at = ?
WHERE n = ?`
)

// plan2Decision raises a blocking ticket from session old the way a Plan 2
// binary does, and returns its id.
func plan2Decision(t *testing.T, db *sql.DB, question, at string) string {
	t.Helper()
	var id string
	err := db.QueryRow(plan2Insert, "old", "", "/src/curia", question, "200 events a minute.",
		`[{"key":"A","label":"Postgres table","tradeoffs":"Durable."}]`, "A", "Replays need history.",
		"costly", "blocking", "", "repo", "storage events", "", "", "hash-"+question, at, nil).Scan(&id)
	if err != nil {
		t.Fatalf("Plan 2 insert: %v", err)
	}
	return id
}

// assertPlan2Runs checks that a Plan 2 binary keeps running against the
// migrated schema: Plan 1's session statements (which Plan 2 kept), and
// Plan 2's decision insert, select and answer.
func assertPlan2Runs(t *testing.T, db *sql.DB, at string) {
	t.Helper()
	assertPlan1Runs(t, db, at)
	id := plan2Decision(t, db, "A second question after the upgrade?", at)
	cols := make([]any, 35)
	for i := range cols {
		cols[i] = new(any)
	}
	var n int64
	var status string
	cols[0], cols[15] = &n, &status
	if err := db.QueryRow(plan2Select, id).Scan(cols...); err != nil {
		t.Fatalf("Plan 2 select on the new schema: %v", err)
	}
	if status != string(model.StatusOpen) {
		t.Errorf("Plan 2 select %s: status %s, want open", id, status)
	}
	if _, err := db.Exec(plan2Answer, "answered", "repo", "A", "A: Postgres table", "", "cli", "queued", at, n); err != nil {
		t.Errorf("Plan 2 answer on the new schema: %v", err)
	}
}

func TestMigrationOnPlan2Database(t *testing.T) {
	path := dbPath(t)
	const at = "2026-11-12T14:00:05.000000000Z"
	db := plan2Database(t, path, at)

	s := open(t, path)
	if names := migrationNames(t, db); !reflect.DeepEqual(names,
		[]string{"0001_init.sql", "0002_decisions.sql", "0003_typed_confirmation.sql", "0004_tasks.sql"}) {
		t.Errorf("migrations = %v, want 0001 to 0004", names)
	}
	sess := get(t, s, "old")
	want := model.Session{
		ID: "old", Agent: "claude", Pane: "%3", TmuxSession: "work", TmuxWindow: "@1", Cwd: "/src/curia",
		Repo: "/src/curia", Branch: "main", State: model.StateWorking, Model: "claude-opus-5-5",
		ContextUsed: 1000, ContextMax: 200000, LastUpdate: "Edit store.go", TranscriptPath: "/tmp/t.jsonl",
		TranscriptOffset: 42,
	}
	if !sess.StartedAt.Equal(decisionsTime(t, at)) || !sess.UpdatedAt.Equal(decisionsTime(t, at)) {
		t.Errorf("times = %v %v, want %s", sess.StartedAt, sess.UpdatedAt, at)
	}
	sess.StartedAt, sess.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(sess, want) {
		t.Errorf("session after migration:\n got %+v\nwant %+v", sess, want)
	}
	d := getDecision(t, s, "D-1")
	if d.Question != "Postgres or Redis for the event store?" || d.Status != model.StatusOpen ||
		d.SessionID != "old" || !d.CreatedAt.Equal(decisionsTime(t, at)) || len(d.Options) != 1 {
		t.Errorf("decision after migration: %+v", d)
	}
	if _, err := db.Exec(`INSERT INTO decisions_fts(decisions_fts) VALUES ('integrity-check')`); err != nil {
		t.Errorf("decisions_fts integrity-check: %v", err)
	}
	var hits int
	if err := db.QueryRow(`SELECT count(*) FROM decisions_fts WHERE decisions_fts MATCH 'redis'`).Scan(&hits); err != nil || hits != 1 {
		t.Errorf("full-text search after migration = %d hits, %v; want 1", hits, err)
	}
	if tasks, err := s.ListTasks(context.Background(), store.TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Errorf("ListTasks = %v, %v; want none", tasks, err)
	}
	assertPlan2Runs(t, db, at)
}

func TestSessionTaskAndWorktreeRoundTrip(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	want := model.Session{
		ID: "s1", Agent: "claude", Pane: "%4", State: model.StateWorking,
		TaskID: "T-7", Worktree: "/src/curia/.worktrees/T-7-store",
	}
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

	upsert(t, s, model.Session{ID: "s1", Agent: "claude", State: model.StateIdle})
	var taskNull, worktreeNull bool
	err = rawDB(t, path).QueryRow(`SELECT task_id IS NULL, worktree IS NULL FROM sessions WHERE id = 's1'`).
		Scan(&taskNull, &worktreeNull)
	if err != nil {
		t.Fatal(err)
	}
	if !taskNull || !worktreeNull {
		t.Errorf("empty task_id, worktree stored as NULL = %v, %v; want true, true", taskNull, worktreeNull)
	}
}

// setTeam sets session id's team columns, as only SetTeams will.
func setTeam(t *testing.T, path, id string) {
	t.Helper()
	rawExec(t, path, `UPDATE sessions SET team_id = 'api', team_role = 'lead', team_member = 'team-lead' WHERE id = ?`, id)
}

func assertTeam(t *testing.T, sess model.Session) {
	t.Helper()
	if sess.TeamID != "api" || sess.TeamRole != model.RoleLead || sess.TeamMember != "team-lead" {
		t.Errorf("%s team = %q %q %q, want api lead team-lead", sess.ID, sess.TeamID, sess.TeamRole, sess.TeamMember)
	}
}

func TestOldUpsertKeepsNewColumns(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	upsert(t, s, model.Session{ID: "old", Agent: "claude", State: model.StateWorking, TaskID: "T-1", Worktree: "/wt/T-1"})
	setTeam(t, path, "old")

	// Plan 2's upsert is Plan 1's statement, unchanged (plan1Upsert).
	const at = "2026-11-13T10:00:00.000000000Z"
	plan1Session(t, rawDB(t, path), "old", "%9", model.StateIdle, at)

	got := get(t, s, "old")
	if got.State != model.StateIdle || got.Pane != "%9" {
		t.Errorf("old upsert did not apply: state %s pane %s", got.State, got.Pane)
	}
	if got.TaskID != "T-1" || got.Worktree != "/wt/T-1" {
		t.Errorf("after the old upsert: task %q worktree %q, want T-1 /wt/T-1", got.TaskID, got.Worktree)
	}
	assertTeam(t, got)
}

func TestUpsertNeverWritesTeamColumns(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	upsert(t, s, model.Session{ID: "lead", Agent: "claude", State: model.StateWorking})
	setTeam(t, path, "lead")

	upsert(t, s, model.Session{ID: "lead", Agent: "claude", State: model.StateIdle})
	assertTeam(t, get(t, s, "lead"))
	err := s.UpdateSession(ctx, "lead", func(cur model.Session, _ bool) model.Session {
		cur.TeamID, cur.TeamRole, cur.TeamMember = "", "", ""
		cur.State = model.StateWorking
		return cur
	})
	if err != nil {
		t.Fatal(err)
	}
	assertTeam(t, get(t, s, "lead"))

	// Nor does it write them on a new row.
	upsert(t, s, model.Session{ID: "mate", Agent: "claude", State: model.StateIdle, TeamID: "api", TeamRole: model.RoleTeammate})
	if got := get(t, s, "mate"); got.TeamID != "" || got.TeamRole != "" {
		t.Errorf("new row team = %q %q, want none", got.TeamID, got.TeamRole)
	}
}

func TestCreateTaskAllocatesSequentialIDs(t *testing.T) {
	s := open(t, dbPath(t))
	for i := 1; i <= 3; i++ {
		got := createTask(t, s, newTask(fmt.Sprintf("Task number %d", i)))
		if want := fmt.Sprintf("T-%d", i); got.ID != want || got.N != int64(i) {
			t.Errorf("task %d = %s (n %d), want %s (n %d)", i, got.ID, got.N, want, i)
		}
	}
}

func TestTaskRoundTrip(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	est := time.FixedZone("EST", -5*3600)
	in := model.Task{
		N: 99, ID: "T-99", // ignored
		Title: "Store: tasks", Body: "Write the tasks table.\n\nWith tests.", Repo: "/src/curia",
		BaseBranch: "plan-3", Status: model.TaskReady, Source: model.SourceBacklogMD,
		SourceRef: "backlog/tasks/task-8.md", Tags: []string{"store", "plan-3"},
		Branch: "curia/T-1-store-tasks", Worktree: "/src/curia/.worktrees/T-1", AgentModel: "opus",
		Pane: "%12", ResumeSession: "1a2b3c4d", Team: true,
		CreatedAt:  time.Date(2026, 11, 13, 4, 0, 0, 1, est),
		UpdatedAt:  time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), // replaced by CreatedAt
		LaunchedAt: time.Date(2026, 11, 13, 9, 1, 0, 999_999_999, time.UTC),
		PausedAt:   time.Date(2026, 11, 13, 9, 2, 0, 5, time.UTC),
		CleanedAt:  time.Date(2026, 11, 13, 9, 3, 0, 50, time.UTC),
	}
	created := createTask(t, s, in)
	want := in
	want.N, want.ID, want.UpdatedAt = 1, "T-1", in.CreatedAt
	assertTask(t, created, want)
	assertTask(t, getTask(t, s, "T-1"), want)

	next := model.Task{
		N: 5, ID: "T-5", CreatedAt: taskT0, // ignored
		Title: "Store: tasks, renamed", Body: "", Repo: "/src/other", BaseBranch: "main",
		Status: model.TaskDropped, Source: model.SourceLocal, SourceRef: "", Tags: []string{"x"},
		Branch: "b", Worktree: "", AgentModel: "", Pane: "%1", ResumeSession: "", Team: false,
		UpdatedAt: time.Date(2026, 11, 14, 1, 2, 3, 4, est),
		PausedAt:  time.Date(2026, 11, 14, 1, 2, 3, 5, time.UTC),
	}
	updated, err := s.UpdateTask(ctx, "T-1", func(cur model.Task) (model.Task, error) {
		assertTask(t, cur, want)
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want = next
	want.N, want.ID, want.CreatedAt = 1, "T-1", in.CreatedAt
	assertTask(t, updated, want)
	assertTask(t, getTask(t, s, "T-1"), want)

	if _, ok, err := s.GetTask(ctx, "T-2"); ok || err != nil {
		t.Errorf("GetTask(T-2) = ok %v, err %v; want false, nil", ok, err)
	}
}

func TestCreateRejectsDerivedStatus(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	for _, st := range []model.TaskStatus{
		model.TaskRunning, model.TaskNeedsDecision, model.TaskReview, model.TaskDone, "",
	} {
		task := newTask("Derived")
		task.Status = st
		if got, err := s.CreateTask(context.Background(), task); err == nil {
			t.Errorf("CreateTask(status %q) = %s, want an error", st, got.ID)
		}
	}
	if n := countTasks(t, path); n != 0 {
		t.Errorf("tasks = %d, want 0", n)
	}
	// A stored status is never derived on update either.
	created := createTask(t, s, newTask("Stored"))
	_, err := s.UpdateTask(context.Background(), created.ID, func(cur model.Task) (model.Task, error) {
		cur.Status = model.TaskRunning
		return cur, nil
	})
	if err == nil {
		t.Error("UpdateTask to running: nil error")
	}
	if got := getTask(t, s, created.ID); got.Status != model.TaskBacklog {
		t.Errorf("status = %s, want backlog", got.Status)
	}
}

func TestConcurrentTaskCreatesAllocateDistinctIDs(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	const goroutines, perGoroutine = 20, 10
	var mu sync.Mutex
	got := map[string]int64{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Go(func() {
			<-start
			s := hs[g%len(hs)]
			for i := range perGoroutine {
				var stored model.Task
				err := retryBusy(func() error {
					var err error
					stored, err = s.CreateTask(context.Background(), newTask(fmt.Sprintf("Task %d of goroutine %d", i, g)))
					return err
				}, busyAtBegin)
				if err != nil {
					t.Errorf("goroutine %d: CreateTask: %v", g, err)
					return
				}
				mu.Lock()
				got[stored.ID] = stored.N
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	for n := int64(1); n <= goroutines*perGoroutine; n++ {
		id := fmt.Sprintf("T-%d", n)
		if gotN, ok := got[id]; !ok || gotN != n {
			t.Errorf("%s: n %d, present %v; want n %d", id, gotN, ok, n)
		}
	}
	if len(got) != goroutines*perGoroutine {
		t.Errorf("distinct ids = %d, want %d", len(got), goroutines*perGoroutine)
	}
	if n := countTasks(t, path); n != goroutines*perGoroutine {
		t.Errorf("tasks = %d, want %d", n, goroutines*perGoroutine)
	}
}

func importTask(t *testing.T, s *store.Store, task model.Task) (model.Task, bool) {
	t.Helper()
	got, created, err := s.ImportTask(context.Background(), task)
	if err != nil {
		t.Fatalf("ImportTask(%q): %v", task.Title, err)
	}
	return got, created
}

func TestImportUpsertsBySourceRef(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	in := model.Task{
		Title: "Old title", Body: "Old body", Repo: "/src/curia", Status: model.TaskBacklog,
		Source: model.SourceBacklogMD, SourceRef: "backlog/tasks/task-1.md", Tags: []string{"old"},
		CreatedAt: taskT0,
	}
	first, created := importTask(t, s, in)
	if !created || first.ID != "T-1" || first.Title != "Old title" {
		t.Fatalf("first import = %s %q created %v, want T-1 created", first.ID, first.Title, created)
	}
	_, err := s.UpdateTask(ctx, "T-1", func(cur model.Task) (model.Task, error) {
		cur.Status, cur.Branch = model.TaskReady, "curia/T-1-old-title"
		return cur, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	again := in
	again.Title, again.Body, again.Tags = "New title", "New body", []string{"new", "tags"}
	again.Status, again.BaseBranch, again.CreatedAt = model.TaskDropped, "other", taskT0.Add(time.Hour)
	second, created := importTask(t, s, again)
	want := first
	want.Title, want.Body, want.Tags = "New title", "New body", []string{"new", "tags"}
	want.Status, want.Branch = model.TaskReady, "curia/T-1-old-title"
	if created {
		t.Error("second import created = true, want false")
	}
	assertTask(t, second, want)
	assertTask(t, getTask(t, s, "T-1"), want)

	elsewhere := in
	elsewhere.Repo = "/src/other"
	third, created := importTask(t, s, elsewhere)
	if !created || third.ID != "T-2" {
		t.Errorf("import from another repo = %s created %v, want T-2 created", third.ID, created)
	}
	if n := countTasks(t, path); n != 2 {
		t.Errorf("tasks = %d, want 2", n)
	}

	local := newTask("Local")
	if got, _, err := s.ImportTask(ctx, local); err == nil {
		t.Errorf("ImportTask(local) = %s, want an error", got.ID)
	}
	bad := in
	bad.SourceRef, bad.Status = "backlog/tasks/task-2.md", model.TaskRunning
	if got, _, err := s.ImportTask(ctx, bad); err == nil {
		t.Errorf("ImportTask(running) = %s, want an error", got.ID)
	}
	if n := countTasks(t, path); n != 2 {
		t.Errorf("tasks after refused imports = %d, want 2", n)
	}
}

func TestUpdateTaskLosesNoUpdates(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	createTask(t, hs[0], newTask("Shared"))
	const writers = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range writers {
		wg.Go(func() {
			<-start
			err := retryBusy(func() error {
				_, err := hs[i%len(hs)].UpdateTask(context.Background(), "T-1", func(cur model.Task) (model.Task, error) {
					cur.Tags = append(cur.Tags, fmt.Sprintf("tag%d", i))
					return cur, nil
				})
				return err
			}, busyAtBegin)
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	got := getTask(t, hs[0], "T-1").Tags
	if len(got) != writers {
		t.Errorf("tags = %d, want %d: %v", len(got), writers, got)
	}
	for i := range writers {
		if !slices.Contains(got, fmt.Sprintf("tag%d", i)) {
			t.Errorf("tag%d lost", i)
		}
	}
}

func TestUpdateTaskRollsBackOnError(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	before := createTask(t, s, newTask("Unchanged"))
	boom := errors.New("boom")
	_, err := s.UpdateTask(ctx, "T-1", func(cur model.Task) (model.Task, error) {
		cur.Title = "Changed"
		return cur, boom
	})
	if !errors.Is(err, boom) || errors.Unwrap(err) != nil {
		t.Errorf("UpdateTask error = %v, want boom itself", err)
	}
	assertTask(t, getTask(t, s, "T-1"), before)

	called := false
	_, err = s.UpdateTask(ctx, "T-9", func(cur model.Task) (model.Task, error) {
		called = true
		return cur, nil
	})
	if !errors.Is(err, store.ErrNoTask) || called {
		t.Errorf("UpdateTask(T-9) = %v, fn called %v; want ErrNoTask, not called", err, called)
	}
}

func TestClaimLaunchOneWins(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	ctx := context.Background()
	createTask(t, hs[0], newTask("Launch me"))
	now := taskT0.Add(time.Minute)
	errs := make([]error, 10)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Go(func() {
			<-start
			errs[i] = retryBusy(func() error {
				_, err := hs[i%len(hs)].ClaimLaunch(ctx, "T-1", now, launchHold)
				return err
			}, busyAtBegin)
		})
	}
	close(start)
	wg.Wait()
	var won, busy int
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, store.ErrTaskBusy):
			busy++
		default:
			t.Errorf("claim %d: %v", i, err)
		}
	}
	if won != 1 || busy != 9 {
		t.Errorf("claims: %d won, %d busy; want 1 and 9", won, busy)
	}
	if got := getTask(t, hs[0], "T-1"); !got.LaunchedAt.Equal(now) {
		t.Errorf("launched_at = %v, want %v", got.LaunchedAt, now)
	}

	if _, err := hs[0].ClaimLaunch(ctx, "T-1", now.Add(launchHold-time.Nanosecond), launchHold); !errors.Is(err, store.ErrTaskBusy) {
		t.Errorf("claim just inside the hold: %v, want ErrTaskBusy", err)
	}
	later := now.Add(launchHold)
	got, err := hs[0].ClaimLaunch(ctx, "T-1", later, launchHold)
	if err != nil || !got.LaunchedAt.Equal(later) {
		t.Errorf("claim after the hold = %v, %v; want launched at %v", got.LaunchedAt, err, later)
	}
	if _, err := hs[0].ClaimLaunch(ctx, "T-2", now, launchHold); !errors.Is(err, store.ErrNoTask) {
		t.Errorf("ClaimLaunch(T-2) = %v, want ErrNoTask", err)
	}
}

func TestTaskSessionsLiveOnly(t *testing.T) {
	s := open(t, dbPath(t))
	for i, sess := range []model.Session{
		{ID: "idle", State: model.StateIdle, TaskID: "T-1"},
		{ID: "working", State: model.StateWorking, TaskID: "T-1"},
		{ID: "ended", State: model.StateEnded, TaskID: "T-1"},
		{ID: "stale", State: model.StateStale, TaskID: "T-1"},
		{ID: "other", State: model.StateIdle, TaskID: "T-2"},
		{ID: "solo", State: model.StateIdle},
	} {
		sess.Agent, sess.UpdatedAt = "claude", taskT0.Add(time.Duration(i)*time.Second)
		upsert(t, s, sess)
	}
	got, err := s.TaskSessions(context.Background(), "T-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"working", "idle"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("TaskSessions(T-1) = %v, want %v", ids(got), want)
	}
	if got, err := s.TaskSessions(context.Background(), ""); err != nil || len(got) != 0 {
		t.Errorf(`TaskSessions("") = %v, %v; want none`, ids(got), err)
	}
}

func TestOpenBlockingByTask(t *testing.T) {
	s := open(t, dbPath(t))
	raise := func(question string, mode model.Mode, task string) model.Decision {
		d := ticket(t, question, mode, "s-"+task, t0)
		d.TaskID = task
		return create(t, s, d)
	}
	raise("First blocking question of T-1?", model.ModeBlocking, "T-1")
	raise("Second blocking question of T-1?", model.ModeBlocking, "T-1")
	raise("An assumption of T-1?", model.ModeAssume, "T-1")
	answered := raise("The answered question of T-2?", model.ModeBlocking, "T-2")
	answer(t, s, answered.ID, keyA, t0.Add(time.Minute))
	raise("A question from no task?", model.ModeBlocking, "")

	got, err := s.OpenBlockingByTask(context.Background(), t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"T-1": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("OpenBlockingByTask = %v, want %v", got, want)
	}
}

func TestListTasksFilters(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for _, x := range []struct {
		status model.TaskStatus
		repo   string
	}{
		{model.TaskBacklog, "/r1"}, {model.TaskReady, "/r1"}, {model.TaskDropped, "/r2"}, {model.TaskReady, "/r2"},
	} {
		task := newTask("Listed")
		task.Status, task.Repo = x.status, x.repo
		createTask(t, s, task)
	}
	for _, c := range []struct {
		f    store.TaskFilter
		want []string
	}{
		{store.TaskFilter{}, []string{"T-1", "T-2", "T-3", "T-4"}},
		{store.TaskFilter{Statuses: []model.TaskStatus{model.TaskReady}}, []string{"T-2", "T-4"}},
		{store.TaskFilter{Repo: "/r2"}, []string{"T-3", "T-4"}},
		{store.TaskFilter{Statuses: []model.TaskStatus{model.TaskBacklog, model.TaskDropped}, Repo: "/r1"}, []string{"T-1"}},
		{store.TaskFilter{Repo: "/r3"}, []string{}},
	} {
		got, err := s.ListTasks(ctx, c.f)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(taskIDs(got), c.want) {
			t.Errorf("ListTasks(%+v) = %v, want %v", c.f, taskIDs(got), c.want)
		}
	}
}

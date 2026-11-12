package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// t0 is when most test tickets are raised.
var t0 = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// assumeExpiry is the deadline of an assume ticket (the config default).
const assumeExpiry = 24 * time.Hour

// request is a valid ticket request with four options; question makes it
// distinct from other requests.
func request(question string, mode model.Mode) decision.Request {
	r := decision.Request{
		Question: question,
		Context:  "internal/events/store.go writes about 200 events a minute.",
		Options: []model.Option{
			{Key: "A", Label: "Postgres table", Tradeoffs: "Durable and queryable. Needs a migration."},
			{Key: "B", Label: "Redis stream", Tradeoffs: "No migration. History is capped by memory."},
			{Key: "C", Label: "SQLite file", Tradeoffs: "Local only."},
			{Key: "D", Label: "Flat files", Tradeoffs: "No queries at all."},
		},
		Recommendation: "A",
		Rationale:      "Replays need 30 days of history, which Redis cannot hold at this volume.",
		Reversibility:  string(model.ReversCostly),
		Mode:           string(mode),
		Scope:          string(model.ScopeRepo),
		Tags:           []string{"storage", "events"},
	}
	if mode == model.ModeAssume {
		r.Assumption = "Postgres"
	}
	return r
}

// ticket builds the open ticket the rule layer makes for request(question,
// mode) from session at at, and fails the test if the request is invalid.
func ticket(t *testing.T, question string, mode model.Mode, session string, at time.Time) model.Decision {
	t.Helper()
	r := request(question, mode)
	if ps := decision.Validate(decision.Normalize(r)); ps != nil {
		t.Fatalf("test request %q is invalid: %v", question, ps)
	}
	return decision.New(r, decision.Caller{SessionID: session, Repo: "/src/curia"}, at, assumeExpiry)
}

// create stores d and requires a new ticket.
func create(t *testing.T, s *store.Store, d model.Decision) model.Decision {
	t.Helper()
	got, dup, err := s.CreateDecision(context.Background(), d)
	if err != nil {
		t.Fatalf("CreateDecision(%q): %v", d.Question, err)
	}
	if dup {
		t.Fatalf("CreateDecision(%q) duplicate = true, want a new ticket (%s)", d.Question, got.ID)
	}
	return got
}

// retry stores d again and requires the duplicate.
func retry(t *testing.T, s *store.Store, d model.Decision) model.Decision {
	t.Helper()
	got, dup, err := s.CreateDecision(context.Background(), d)
	if err != nil {
		t.Fatalf("CreateDecision(%q) retry: %v", d.Question, err)
	}
	if !dup {
		t.Fatalf("CreateDecision(%q) retry duplicate = false (%s), want the twin", d.Question, got.ID)
	}
	return got
}

func getDecision(t *testing.T, s *store.Store, id string) model.Decision {
	t.Helper()
	d, ok, err := s.GetDecision(context.Background(), id)
	if err != nil {
		t.Fatalf("GetDecision(%s): %v", id, err)
	}
	if !ok {
		t.Fatalf("GetDecision(%s): not found", id)
	}
	return d
}

func answer(t *testing.T, s *store.Store, id string, a model.Answer, now time.Time) model.Decision {
	t.Helper()
	d, err := s.AnswerDecision(context.Background(), id, a, now)
	if err != nil {
		t.Fatalf("AnswerDecision(%s, %+v): %v", id, a, err)
	}
	return d
}

// keyA is the answer that picks option A from the command line.
var keyA = model.Answer{Key: "A", Via: model.AnsweredCLI}

// assertDecision compares two decisions, times with Equal (the store
// returns them in UTC) and everything else with DeepEqual.
func assertDecision(t *testing.T, got, want model.Decision) {
	t.Helper()
	times := func(d *model.Decision) map[string]*time.Time {
		return map[string]*time.Time{
			"CreatedAt": &d.CreatedAt, "ExpiresAt": &d.ExpiresAt, "AnsweredAt": &d.AnsweredAt,
			"ClosedAt": &d.ClosedAt, "SupersededAt": &d.SupersededAt, "DeliveredAt": &d.DeliveredAt,
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
		t.Errorf("decision:\n got %+v\nwant %+v", got, want)
	}
}

func countDecisions(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := rawDB(t, path).QueryRow(`SELECT count(*) FROM decisions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rawExec(t *testing.T, path, query string, args ...any) {
	t.Helper()
	if _, err := rawDB(t, path).Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// isBusy reports whether err is SQLITE_BUSY or one of its extended codes.
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// busyAtBegin reports whether err is SQLITE_BUSY from the BEGIN of a
// transaction (the store labels it "begin"): the transaction never started,
// so the call changed nothing and may be retried. SQLite's busy handler
// polls rather than queues, so under heavy contention a writer can starve
// past busy_timeout (Plan 1, Open Question 9).
func busyAtBegin(err error) bool {
	return isBusy(err) && strings.Contains(err.Error(), ": begin: ")
}

// retryBusy calls op until it returns an error that retryable rejects.
func retryBusy(op func() error, retryable func(error) bool) error {
	const maxAttempts = 20
	var err error
	for range maxAttempts {
		if err = op(); !retryable(err) {
			return err
		}
	}
	return fmt.Errorf("still busy after %d attempts: %w", maxAttempts, err)
}

// handles opens n Stores on one file.
func handles(t *testing.T, path string, n int) []*store.Store {
	t.Helper()
	out := make([]*store.Store, n)
	for i := range out {
		out[i] = open(t, path)
	}
	return out
}

func TestMigrationAddsDecisions(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	for _, obj := range [][2]string{
		{"table", "decisions"},
		{"table", "decisions_fts"},
		{"table", "meta"},
		{"index", "decisions_open_dedupe"},
		{"index", "decisions_session_status"},
		{"index", "decisions_status_created"},
		{"trigger", "decisions_ai"},
		{"trigger", "decisions_ad"},
		{"trigger", "decisions_au"},
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
	for col, typ := range map[string]string{"cache_expires_at": "TEXT", "cache_rebuild_usd": "REAL"} {
		var got string
		var notNull int
		err := db.QueryRow(`SELECT type, "notnull" FROM pragma_table_info('sessions') WHERE name = ?`, col).Scan(&got, &notNull)
		if err != nil {
			t.Fatalf("sessions.%s: %v", col, err)
		}
		if got != typ || notNull != 0 {
			t.Errorf("sessions.%s = %s notnull=%d, want nullable %s", col, got, notNull, typ)
		}
	}
}

// Plan 1's statements, copied from its store.go, so this test keeps
// describing what a Plan 1 binary runs after this package changes.
const (
	plan1Upsert = `
INSERT INTO sessions (
  id, agent, tmux_pane, tmux_session, tmux_window, cwd, repo, branch, state, model,
  context_used, context_max, subagents_active, last_update, seen_at, transcript_path,
  started_at, updated_at, transcript_offset
) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  agent = excluded.agent, tmux_pane = excluded.tmux_pane,
  tmux_session = excluded.tmux_session, tmux_window = excluded.tmux_window,
  cwd = excluded.cwd, repo = excluded.repo, branch = excluded.branch,
  state = excluded.state, model = excluded.model,
  context_used = excluded.context_used, context_max = excluded.context_max,
  subagents_active = excluded.subagents_active, last_update = excluded.last_update,
  seen_at = excluded.seen_at, transcript_path = excluded.transcript_path,
  started_at = excluded.started_at, updated_at = excluded.updated_at,
  transcript_offset = excluded.transcript_offset`
	plan1Append   = `INSERT INTO events (session_id, kind, payload, at) VALUES (?, ?, ?, ?)`
	plan1MarkSeen = `UPDATE sessions SET seen_at = ? WHERE tmux_pane = ? AND state != ?`
	plan1EndOther = `UPDATE sessions SET state = ?, updated_at = ? WHERE tmux_pane = ? AND id != ? AND state != ?`
	plan1Select   = `
SELECT COALESCE(id, ''), COALESCE(agent, ''), COALESCE(tmux_pane, ''), COALESCE(tmux_session, ''),
  COALESCE(tmux_window, ''), COALESCE(cwd, ''), COALESCE(repo, ''), COALESCE(branch, ''),
  COALESCE(state, ''), COALESCE(model, ''), COALESCE(context_used, 0), COALESCE(context_max, 0),
  COALESCE(subagents_active, 0), COALESCE(last_update, ''), seen_at, COALESCE(transcript_path, ''),
  started_at, updated_at, transcript_offset
FROM sessions WHERE id = ?`
)

// plan1Session writes session id the way a Plan 1 hook does.
func plan1Session(t *testing.T, db *sql.DB, id, pane string, state model.State, at string) {
	t.Helper()
	_, err := db.Exec(plan1Upsert, id, "claude", pane, "work", "@1", "/src/curia", "/src/curia", "main",
		state, "claude-opus-5-5", 1000, 200000, 0, "Edit store.go", nil, "/tmp/t.jsonl", at, at, 42)
	if err != nil {
		t.Fatalf("Plan 1 upsert: %v", err)
	}
}

// plan1Database builds, at path, a database as Plan 1's binary leaves it:
// WAL, only 0001 applied, and one session and one event written by its
// statements at at.
func plan1Database(t *testing.T, path, at string) *sql.DB {
	t.Helper()
	db := rawDB(t, path)
	schema, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`,
		string(schema),
		`INSERT INTO schema_migrations (name, applied_at) VALUES ('0001_init.sql', '` + at + `')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("building the Plan 1 database: %v", err)
		}
	}
	plan1Session(t, db, "old", "%3", model.StateWorking, at)
	if _, err = db.Exec(plan1Append, "old", "PreToolUse", `{"tool_name":"Edit"}`, at); err != nil {
		t.Fatal(err)
	}
	return db
}

// assertPlan1Runs checks that a Plan 1 binary keeps running its hooks
// against the migrated schema: its migration runner finds 0001 among the
// names it reads from schema_migrations, and each of its statements works.
func assertPlan1Runs(t *testing.T, db *sql.DB, at string) {
	t.Helper()
	if applied := migrationNames(t, db); !slices.Contains(applied, "0001_init.sql") {
		t.Errorf("applied = %v, want 0001_init.sql among them", applied)
	}
	plan1Session(t, db, "old", "%3", model.StateIdle, at) // the ON CONFLICT path
	plan1Session(t, db, "new", "%3", model.StateWorking, at)
	for _, step := range []struct {
		q    string
		args []any
	}{
		{plan1Append, []any{"new", "Stop", nil, at}},
		{plan1MarkSeen, []any{at, "%3", model.StateEnded}},
		{plan1EndOther, []any{model.StateEnded, at, "%3", "new", model.StateEnded}},
	} {
		if _, err := db.Exec(step.q, step.args...); err != nil {
			t.Errorf("Plan 1 statement on the new schema: %v\n%s", err, step.q)
		}
	}
	var id, state string
	cols := make([]any, 19)
	for i := range cols {
		cols[i] = new(any)
	}
	cols[0], cols[8] = &id, &state
	if err := db.QueryRow(plan1Select, "old").Scan(cols...); err != nil {
		t.Fatalf("Plan 1 select on the new schema: %v", err)
	}
	if id != "old" || state != string(model.StateEnded) {
		t.Errorf("Plan 1 select = %s %s, want old ended", id, state)
	}
}

// Ruling 86: a database migrated to 0003 counts answers typed before it as
// confirmed when they were typed, and leaves every other row unconfirmed.
func TestMigrationConfirmsEarlierTyping(t *testing.T) {
	path := dbPath(t)
	const at = "2026-11-11T14:00:05.000000000Z"
	db := plan1Database(t, path, at)
	schema, err := os.ReadFile("migrations/0002_decisions.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		string(schema),
		`INSERT INTO schema_migrations (name, applied_at) VALUES ('0002_decisions.sql', '` + at + `')`,
		`INSERT INTO decisions (id, question, options, recommendation, rationale, reversibility, mode, scope, status,
		  args_hash, delivery, delivered_via, delivered_at, created_at)
		 VALUES ('D-1', 'Typed?', '[]', 'A', 'r', 'cheap', 'blocking', 'repo', 'answered', 'h1', 'delivered', 'send_keys', '` + at + `', '` + at + `'),
		        ('D-2', 'Stopped?', '[]', 'A', 'r', 'cheap', 'blocking', 'repo', 'answered', 'h2', 'delivered', 'stop_hook', '` + at + `', '` + at + `')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatalf("building the Plan 2 database: %v", err)
		}
	}
	s := open(t, path)
	if got := getDecision(t, s, "D-1"); !got.ConfirmedAt.Equal(decisionsTime(t, at)) {
		t.Errorf("D-1 typed before the migration: confirmed at %v, want %s", got.ConfirmedAt, at)
	}
	if got := getDecision(t, s, "D-2"); !got.ConfirmedAt.IsZero() {
		t.Errorf("D-2 (stop_hook): confirmed at %v, want zero", got.ConfirmedAt)
	}
}

// decisionsTime parses a stored timestamp.
func decisionsTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestMigrationOnPlan1Database(t *testing.T) {
	path := dbPath(t)
	const at = "2026-11-11T14:00:05.000000000Z"
	db := plan1Database(t, path, at)

	s := open(t, path)
	if n := countMigrations(t, path); n != 3 {
		t.Errorf("schema_migrations has %d rows, want 3", n)
	}
	sess := get(t, s, "old")
	want := model.Session{
		ID: "old", Agent: "claude", Pane: "%3", TmuxSession: "work", TmuxWindow: "@1", Cwd: "/src/curia",
		Repo: "/src/curia", Branch: "main", State: model.StateWorking, Model: "claude-opus-5-5",
		ContextUsed: 1000, ContextMax: 200000, LastUpdate: "Edit store.go", TranscriptPath: "/tmp/t.jsonl",
		TranscriptOffset: 42,
	}
	wantAt, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatal(err)
	}
	if !sess.StartedAt.Equal(wantAt) || !sess.UpdatedAt.Equal(wantAt) {
		t.Errorf("times = %v %v, want %v", sess.StartedAt, sess.UpdatedAt, wantAt)
	}
	sess.StartedAt, sess.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(sess, want) {
		t.Errorf("session after migration:\n got %+v\nwant %+v", sess, want)
	}
	var events int
	if err = db.QueryRow(`SELECT count(*) FROM events WHERE json_extract(payload, '$.tool_name') = 'Edit'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Errorf("events = %d, want the 1 Plan 1 event", events)
	}
	assertPlan1Runs(t, db, at)
}

func TestCreateAllocatesSequentialIDs(t *testing.T) {
	s := open(t, dbPath(t))
	for i := 1; i <= 3; i++ {
		d := create(t, s, ticket(t, fmt.Sprintf("Question number %d?", i), model.ModeBlocking, "s1", t0))
		if want := fmt.Sprintf("D-%d", i); d.ID != want || d.N != int64(i) {
			t.Errorf("ticket %d = %s (n %d), want %s (n %d)", i, d.ID, d.N, want, i)
		}
	}
}

func TestDecisionRoundTrip(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	est := time.FixedZone("EST", -5*3600)
	created := time.Date(2026, 11, 12, 4, 0, 0, 123_456_789, est)

	r := request("Store webhook events in Postgres or in Redis?", model.ModeAssume)
	r.DiffersFrom, r.DiffersWhy = "D-7", "The volume changed."
	want := decision.New(r, decision.Caller{SessionID: "s1", TaskID: "task-1", Repo: "/src/curia"}, created, assumeExpiry)
	got := create(t, s, want)
	want.N, want.ID = 1, "D-1"
	assertDecision(t, got, want)
	assertDecision(t, getDecision(t, s, "D-1"), want)

	// Answer: every answer column.
	answered := created.Add(90*time.Minute + 7)
	a := model.Answer{Key: "B", Reasoning: "Redis is enough for a week.", Scope: model.ScopeGlobal, Via: model.AnsweredPopup}
	want, err := decision.Resolve(want, a, answered)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, answer(t, s, "D-1", a, answered), want)
	assertDecision(t, getDecision(t, s, "D-1"), want)

	// Edit: rescope and supersede by another decided ticket.
	other := create(t, s, ticket(t, "Another question?", model.ModeBlocking, "s1", created))
	answer(t, s, other.ID, keyA, answered)
	edited := answered.Add(time.Hour + 1)
	e := model.DecisionEdit{Scope: model.ScopeTask, Supersede: true, SupersededBy: other.ID}
	got, err = s.EditDecision(ctx, "D-1", e, edited)
	if err != nil {
		t.Fatal(err)
	}
	want.Scope, want.SupersededBy, want.SupersededAt = model.ScopeTask, other.ID, edited
	assertDecision(t, got, want)
	assertDecision(t, getDecision(t, s, "D-1"), want)

	// Delivery columns are written by later tasks; they read back here.
	delivered := edited.Add(time.Second + 5)
	rawExec(t, path, `UPDATE decisions SET delivery = 'delivered', delivered_via = 'stop_hook', delivered_at = ? WHERE id = 'D-1'`,
		delivered.UTC().Format("2006-01-02T15:04:05.000000000Z"))
	want.Delivery, want.DeliveredVia, want.DeliveredAt = model.DeliveryDelivered, model.ViaStopHook, delivered
	assertDecision(t, getDecision(t, s, "D-1"), want)

	if _, ok, err := s.GetDecision(ctx, "D-99"); err != nil || ok {
		t.Errorf("GetDecision(D-99) = ok %v, err %v; want false, nil", ok, err)
	}
}

func TestIdenticalRetryCollapses(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	first := create(t, s, ticket(t, "Same question?", model.ModeBlocking, "s1", t0))
	again := retry(t, s, ticket(t, "Same question?", model.ModeBlocking, "s1", t0.Add(time.Minute)))
	assertDecision(t, again, first)
	if n := countDecisions(t, path); n != 1 {
		t.Errorf("decisions = %d, want 1", n)
	}
}

func TestRetryAddsDiffers(t *testing.T) {
	s := open(t, dbPath(t))
	create(t, s, ticket(t, "The precedent?", model.ModeBlocking, "s1", t0))
	orig := create(t, s, ticket(t, "The new question?", model.ModeBlocking, "s1", t0))

	d := ticket(t, "The new question?", model.ModeBlocking, "s1", t0.Add(time.Minute))
	d.DiffersFrom, d.DiffersWhy = "D-1", "This one is about reads."
	got := retry(t, s, d)
	if got.ID != orig.ID || got.DiffersFrom != "D-1" || got.DiffersWhy != "This one is about reads." {
		t.Errorf("retry = %s differs %q %q, want %s with the differs fields", got.ID, got.DiffersFrom, got.DiffersWhy, orig.ID)
	}
	if got.ArgsHash != orig.ArgsHash {
		t.Errorf("hash changed: %s != %s", got.ArgsHash, orig.ArgsHash)
	}
	want := orig
	want.DiffersFrom, want.DiffersWhy = "D-1", "This one is about reads."
	assertDecision(t, getDecision(t, s, orig.ID), want)

	// A retry without differs_from leaves them as they are.
	retry(t, s, ticket(t, "The new question?", model.ModeBlocking, "s1", t0.Add(2*time.Minute)))
	assertDecision(t, getDecision(t, s, orig.ID), want)
}

func TestRetryAfterDecisionReturnsIt(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	create(t, s, ticket(t, "Decided question?", model.ModeBlocking, "s1", t0))
	decided := answer(t, s, "D-1", model.Answer{Key: "B", Reasoning: "Cheaper.", Via: model.AnsweredCLI}, t0.Add(time.Hour))

	d := ticket(t, "Decided question?", model.ModeBlocking, "s1", t0.Add(2*time.Hour))
	d.DiffersFrom, d.DiffersWhy = "D-5", "Not stored on a decided ticket."
	got := retry(t, s, d)
	assertDecision(t, got, decided)
	if got.Status != model.StatusAnswered || got.AnswerKey != "B" || got.Answer != "B: Redis stream" {
		t.Errorf("retry = %s %q %q, want the answer", got.Status, got.AnswerKey, got.Answer)
	}
	assertDecision(t, getDecision(t, s, "D-1"), decided)
	if n := countDecisions(t, path); n != 1 {
		t.Errorf("decisions = %d, want 1", n)
	}
}

func TestRetryAfterSupersedeOpensNew(t *testing.T) {
	// Ruling 64: a superseded decision is not a duplicate.
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	create(t, s, ticket(t, "Superseded question?", model.ModeBlocking, "s1", t0))
	decided := answer(t, s, "D-1", keyA, t0.Add(time.Hour))

	// Decided and not superseded: still the duplicate.
	assertDecision(t, retry(t, s, ticket(t, "Superseded question?", model.ModeBlocking, "s1", t0.Add(2*time.Hour))), decided)

	if _, err := s.EditDecision(ctx, "D-1", model.DecisionEdit{Supersede: true}, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	fresh := create(t, s, ticket(t, "Superseded question?", model.ModeBlocking, "s1", t0.Add(4*time.Hour)))
	if fresh.ID != "D-2" || fresh.Status != model.StatusOpen {
		t.Errorf("retry after supersede = %s %s, want a new open D-2", fresh.ID, fresh.Status)
	}
	// The new open ticket is now the twin.
	if got := retry(t, s, ticket(t, "Superseded question?", model.ModeBlocking, "s1", t0.Add(5*time.Hour))); got.ID != "D-2" {
		t.Errorf("next retry = %s, want D-2", got.ID)
	}
	if n := countDecisions(t, path); n != 2 {
		t.Errorf("decisions = %d, want 2", n)
	}
}

func TestRetryAfterWithdrawOrExpiryOpensNew(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()

	create(t, s, ticket(t, "Withdrawn question?", model.ModeBlocking, "s1", t0))
	if _, err := s.WithdrawDecision(ctx, "D-1", "s1", "Found the answer.", "", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if d := create(t, s, ticket(t, "Withdrawn question?", model.ModeBlocking, "s1", t0.Add(time.Hour))); d.ID != "D-2" {
		t.Errorf("after withdraw: %s, want D-2", d.ID)
	}

	create(t, s, ticket(t, "Expired question?", model.ModeAssume, "s1", t0))
	deadline := t0.Add(assumeExpiry)
	if n, err := s.ExpireDecisions(ctx, deadline); err != nil || n != 1 {
		t.Fatalf("ExpireDecisions = %d, %v; want 1, nil", n, err)
	}
	if d := create(t, s, ticket(t, "Expired question?", model.ModeAssume, "s1", deadline.Add(time.Second))); d.ID != "D-4" {
		t.Errorf("after expiry: %s, want D-4", d.ID)
	}
}

func TestRetryPastDeadlineBeforeAnySweepOpensNew(t *testing.T) {
	s := open(t, dbPath(t))
	first := create(t, s, ticket(t, "Assumed question?", model.ModeAssume, "s1", t0))
	deadline := first.ExpiresAt

	// One nanosecond before the deadline the twin is still open.
	retry(t, s, ticket(t, "Assumed question?", model.ModeAssume, "s1", deadline.Add(-time.Nanosecond)))

	got := create(t, s, ticket(t, "Assumed question?", model.ModeAssume, "s1", deadline))
	if got.ID != "D-2" {
		t.Errorf("retry at the deadline = %s, want D-2", got.ID)
	}
	twin := getDecision(t, s, "D-1")
	if twin.Status != model.StatusExpired || !twin.ClosedAt.Equal(deadline) {
		t.Errorf("twin = %s closed %v, want expired at %v", twin.Status, twin.ClosedAt, deadline)
	}
}

func TestSameTicketFromTwoSessionsIsTwoRows(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	for i, session := range []string{"s1", "s2", ""} {
		d := create(t, s, ticket(t, "Shared question?", model.ModeBlocking, session, t0))
		if want := fmt.Sprintf("D-%d", i+1); d.ID != want || d.SessionID != session {
			t.Errorf("session %q: %s, want %s", session, d.ID, want)
		}
	}
	if n := countDecisions(t, path); n != 3 {
		t.Errorf("decisions = %d, want 3", n)
	}
}

func TestConcurrentCreatesAllocateDistinctIDs(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	const goroutines, perGoroutine = 20, 10
	tickets := make([][]model.Decision, goroutines)
	for g := range tickets {
		for i := range perGoroutine {
			q := fmt.Sprintf("Question %d from goroutine %d?", i, g)
			tickets[g] = append(tickets[g], ticket(t, q, model.ModeBlocking, fmt.Sprintf("s%d", g), t0))
		}
	}
	var mu sync.Mutex
	got := map[string]int64{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Go(func() {
			<-start
			s := hs[g%len(hs)]
			for _, d := range tickets[g] {
				var stored model.Decision
				var dup bool
				err := retryBusy(func() error {
					var err error
					stored, dup, err = s.CreateDecision(context.Background(), d)
					return err
				}, busyAtBegin)
				if err != nil || dup {
					t.Errorf("goroutine %d: CreateDecision = dup %v, %v; want a new ticket", g, dup, err)
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
		id := fmt.Sprintf("D-%d", n)
		if gotN, ok := got[id]; !ok || gotN != n {
			t.Errorf("%s: n %d, present %v; want n %d", id, gotN, ok, n)
		}
	}
	if len(got) != goroutines*perGoroutine {
		t.Errorf("distinct ids = %d, want %d", len(got), goroutines*perGoroutine)
	}
	if n := countDecisions(t, path); n != goroutines*perGoroutine {
		t.Errorf("decisions = %d, want %d", n, goroutines*perGoroutine)
	}
}

func TestConcurrentIdenticalRetriesCollapse(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	const goroutines = 20
	d := ticket(t, "Asked twenty times at once?", model.ModeBlocking, "s1", t0)
	var fresh, calls int
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Go(func() {
			<-start
			var stored model.Decision
			var dup bool
			err := retryBusy(func() error {
				var err error
				stored, dup, err = hs[g%len(hs)].CreateDecision(context.Background(), d)
				return err
			}, busyAtBegin)
			if err != nil || stored.ID != "D-1" {
				t.Errorf("goroutine %d: CreateDecision = %s, %v; want D-1, nil", g, stored.ID, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			calls++
			if !dup {
				fresh++
			}
		})
	}
	close(start)
	wg.Wait()
	if calls != goroutines || fresh != 1 {
		t.Errorf("%d calls returned D-1, %d of them new; want %d and 1", calls, fresh, goroutines)
	}
	if n := countDecisions(t, path); n != 1 {
		t.Errorf("decisions = %d, want 1", n)
	}
}

func TestAnswerTable(t *testing.T) {
	s := open(t, dbPath(t))
	now := t0.Add(time.Hour)
	cases := []struct {
		name     string
		mode     model.Mode
		session  string
		a        model.Answer
		status   model.DecisionStatus
		delivery model.Delivery
	}{
		{"blocking key", model.ModeBlocking, "s1", keyA, model.StatusAnswered, model.DeliveryQueued},
		{"blocking free text", model.ModeBlocking, "s1", model.Answer{Text: "Neither; use S3."}, model.StatusAnswered, model.DeliveryQueued},
		{"assume recommended key", model.ModeAssume, "s1", keyA, model.StatusConfirmed, model.DeliveryNone},
		{"assume other key", model.ModeAssume, "s1", model.Answer{Key: "B"}, model.StatusOverridden, model.DeliveryQueued},
		{"assume free text", model.ModeAssume, "s1", model.Answer{Text: "A, but partitioned."}, model.StatusOverridden, model.DeliveryQueued},
		{"blocking, unknown caller", model.ModeBlocking, "", keyA, model.StatusAnswered, model.DeliveryNone},
		{"assume other key, unknown caller", model.ModeAssume, "", model.Answer{Key: "B"}, model.StatusOverridden, model.DeliveryNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := create(t, s, ticket(t, "Question: "+tc.name+"?", tc.mode, tc.session, t0))
			got := answer(t, s, d.ID, tc.a, now)
			stored := getDecision(t, s, d.ID)
			assertDecision(t, stored, got)
			if stored.Status != tc.status || stored.Delivery != tc.delivery || !stored.AnsweredAt.Equal(now) {
				t.Errorf("%s = %s, delivery %s, answered %v; want %s, %s, %v",
					d.ID, stored.Status, stored.Delivery, stored.AnsweredAt, tc.status, tc.delivery, now)
			}
		})
	}
}

func TestAnswerErrors(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	if _, err := s.AnswerDecision(ctx, "D-1", keyA, now); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown id: %v, want ErrNotFound", err)
	}
	d := create(t, s, ticket(t, "Answered twice?", model.ModeBlocking, "s1", t0))
	answered := answer(t, s, d.ID, keyA, now)
	if _, err := s.AnswerDecision(ctx, d.ID, model.Answer{Key: "B"}, now); !errors.Is(err, store.ErrNotOpen) {
		t.Errorf("answered twice: %v, want ErrNotOpen", err)
	}
	assertDecision(t, getDecision(t, s, d.ID), answered)

	bad := create(t, s, ticket(t, "Bad key?", model.ModeBlocking, "s1", t0))
	if _, err := s.AnswerDecision(ctx, bad.ID, model.Answer{Key: "Z"}, now); !errors.Is(err, decision.ErrBadAnswer) {
		t.Errorf("key Z: %v, want decision.ErrBadAnswer", err)
	}
	assertDecision(t, getDecision(t, s, bad.ID), bad)

	w := create(t, s, ticket(t, "Withdrawn first?", model.ModeBlocking, "s1", t0))
	if _, err := s.WithdrawDecision(ctx, w.ID, "s1", "", "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerDecision(ctx, w.ID, keyA, now); !errors.Is(err, store.ErrNotOpen) {
		t.Errorf("withdrawn: %v, want ErrNotOpen", err)
	}
}

func TestConcurrentAnswersOneWins(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	d := create(t, hs[0], ticket(t, "Answered ten times at once?", model.ModeBlocking, "s1", t0))
	const goroutines = 10
	keys := []string{"A", "B", "C", "D"}
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Go(func() {
			<-start
			a := model.Answer{Key: keys[g%len(keys)], Reasoning: fmt.Sprintf("goroutine %d", g), Via: model.AnsweredCLI}
			errs[g] = retryBusy(func() error {
				_, err := hs[g%len(hs)].AnswerDecision(context.Background(), d.ID, a, t0.Add(time.Hour))
				return err
			}, busyAtBegin)
		})
	}
	close(start)
	wg.Wait()

	winner := -1
	for g, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = g
		case err == nil:
			t.Errorf("goroutines %d and %d both answered", winner, g)
		case !errors.Is(err, store.ErrNotOpen):
			t.Errorf("goroutine %d: %v, want nil or ErrNotOpen", g, err)
		}
	}
	if winner < 0 {
		t.Fatal("no answer won")
	}
	got := getDecision(t, hs[0], d.ID)
	if got.AnswerKey != keys[winner%len(keys)] || got.AnswerReasoning != fmt.Sprintf("goroutine %d", winner) {
		t.Errorf("stored answer %q %q, want the winner's (goroutine %d)", got.AnswerKey, got.AnswerReasoning, winner)
	}
}

// raceRound runs AnswerDecision, WithdrawDecision and (for an assume
// ticket) ExpireDecisions on d at once, each from its own handle, and
// returns which of them changed the ticket. Each starts after a delay that
// depends on round: in some rounds all start at once, in others they are
// spaced 200µs or 3ms apart in a rotating order, so the calls overlap and
// each of them also gets rounds where it goes first.
func raceRound(t *testing.T, hs []*store.Store, d model.Decision, round int) []model.DecisionStatus {
	t.Helper()
	ctx := context.Background()
	step := []time.Duration{0, 200 * time.Microsecond, 3 * time.Millisecond}[(round/6)%3]
	stagger := func(k int) {
		time.Sleep(time.Duration((round/2+k)%3) * step)
	}
	before := raceNow(d)
	var answerErr, withdrawErr, expireErr error
	var expired int
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		stagger(0)
		answerErr = retryBusy(func() error {
			_, err := hs[0].AnswerDecision(ctx, d.ID, raceAnswer, before)
			return err
		}, busyAtBegin)
	})
	wg.Go(func() {
		<-start
		stagger(1)
		withdrawErr = retryBusy(func() error {
			_, err := hs[1].WithdrawDecision(ctx, d.ID, d.SessionID, "Race.", "", before)
			return err
		}, busyAtBegin)
	})
	if d.Mode == model.ModeAssume {
		wg.Go(func() {
			<-start
			stagger(2)
			// One statement: SQLITE_BUSY means it wrote nothing.
			expireErr = retryBusy(func() error {
				var err error
				expired, err = hs[2].ExpireDecisions(ctx, d.ExpiresAt)
				return err
			}, isBusy)
		})
	}
	close(start)
	wg.Wait()

	answered := model.StatusAnswered
	if d.Mode == model.ModeAssume {
		answered = model.StatusOverridden // key B is not the recommendation
	}
	var won []model.DecisionStatus
	for _, r := range []struct {
		err    error
		status model.DecisionStatus
	}{{answerErr, answered}, {withdrawErr, model.StatusWithdrawn}} {
		switch {
		case r.err == nil:
			won = append(won, r.status)
		case !errors.Is(r.err, store.ErrNotOpen):
			t.Errorf("%s: %s lost with %v, want ErrNotOpen", d.ID, r.status, r.err)
		}
	}
	if expireErr != nil {
		t.Errorf("%s: ExpireDecisions: %v", d.ID, expireErr)
	}
	for range expired {
		won = append(won, model.StatusExpired)
	}
	return won
}

// raceAnswer is the answer raceRound gives.
var raceAnswer = model.Answer{Key: "B", Reasoning: "Race.", Via: model.AnsweredCLI}

// raceNow is when raceRound answers and withdraws d: before its deadline.
func raceNow(d model.Decision) time.Time {
	if d.Mode == model.ModeAssume {
		return d.ExpiresAt.Add(-time.Nanosecond)
	}
	return t0.Add(time.Hour)
}

// assertConsistent checks that got, the open ticket orig after a race, is
// exactly what the one change its status names makes of orig: no answer on
// a withdrawn or expired row, no withdraw reason or close time on an
// answered one.
func assertConsistent(t *testing.T, orig, got model.Decision) {
	t.Helper()
	want := orig
	switch got.Status {
	case model.StatusAnswered, model.StatusOverridden:
		var err error
		if want, err = decision.Resolve(orig, raceAnswer, raceNow(orig)); err != nil {
			t.Fatal(err)
		}
	case model.StatusWithdrawn:
		want.Status, want.WithdrawReason, want.ClosedAt = model.StatusWithdrawn, "Race.", raceNow(orig)
	case model.StatusExpired:
		want.Status, want.ClosedAt = model.StatusExpired, orig.ExpiresAt
	default:
		t.Errorf("%s: status %s after the race", got.ID, got.Status)
		return
	}
	assertDecision(t, got, want)
}

// raceOutcomes are the outcomes TestAnswerRacesWithdrawAndExpire must see
// win at least once.
var raceOutcomes = []model.DecisionStatus{
	model.StatusAnswered, model.StatusOverridden, model.StatusWithdrawn, model.StatusExpired,
}

func TestAnswerRacesWithdrawAndExpire(t *testing.T) {
	// At least 200 rounds, and then more until every outcome has won once,
	// so the test covers each path however the machine schedules them; it
	// fails on coverage only at the cap.
	const minRounds, maxRounds = 200, 2000
	path := dbPath(t)
	hs := handles(t, path, 3)
	wins := map[model.DecisionStatus]int{}
	allWon := func() bool {
		for _, st := range raceOutcomes {
			if wins[st] == 0 {
				return false
			}
		}
		return true
	}
	round := 0
	for ; round < maxRounds && (round < minRounds || !allWon()); round++ {
		mode := model.ModeBlocking
		if round%2 == 1 {
			mode = model.ModeAssume
		}
		d := create(t, hs[0], ticket(t, fmt.Sprintf("Race round %d?", round), mode, "s1", t0))
		won := raceRound(t, hs, d, round)
		got := getDecision(t, hs[0], d.ID)
		if len(won) != 1 {
			t.Errorf("%s: %v changed the ticket, want exactly one (status %s)", d.ID, won, got.Status)
			continue
		}
		wins[won[0]]++
		if got.Status != won[0] {
			t.Errorf("%s: status %s, want the winner's %s", d.ID, got.Status, won[0])
		}
		assertConsistent(t, d, got)
	}
	t.Logf("winners over %d rounds: %v", round, wins)
	for _, st := range raceOutcomes {
		if wins[st] == 0 {
			t.Errorf("%s never won in %d rounds", st, round)
		}
	}
}

// needsDecision stores session id in state with fixed UpdatedAt and
// SeenAt, and returns it.
func needsDecision(t *testing.T, s *store.Store, id string, state model.State) model.Session {
	t.Helper()
	sess := model.Session{
		ID: id, Agent: "claude", Pane: "%3", State: state,
		UpdatedAt: t0.Add(-time.Minute), SeenAt: t0.Add(-2 * time.Minute),
	}
	upsert(t, s, sess)
	return sess
}

func TestAnswerClearsNeedsDecision(t *testing.T) {
	ctx := context.Background()
	for name, closeTicket := range map[string]func(s *store.Store, id string) error{
		"answer": func(s *store.Store, id string) error {
			_, err := s.AnswerDecision(ctx, id, keyA, t0.Add(time.Hour))
			return err
		},
		"withdraw": func(s *store.Store, id string) error {
			_, err := s.WithdrawDecision(ctx, id, "s1", "Not needed.", "", t0.Add(time.Hour))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := open(t, dbPath(t))
			want := needsDecision(t, s, "s1", model.StateNeedsDecision)
			d := create(t, s, ticket(t, "The only blocking question?", model.ModeBlocking, "s1", t0))
			// An open assume ticket does not keep the session waiting on a decision.
			create(t, s, ticket(t, "An assumed question?", model.ModeAssume, "s1", t0))
			if err := closeTicket(s, d.ID); err != nil {
				t.Fatal(err)
			}
			want.State = model.StateIdle
			if got := get(t, s, "s1"); !reflect.DeepEqual(got, want) {
				t.Errorf("session:\n got %+v\nwant %+v (idle, times unchanged)", got, want)
			}
		})
	}
}

func TestAnswerKeepsNeedsDecisionWhileAnotherIsOpen(t *testing.T) {
	s := open(t, dbPath(t))
	want := needsDecision(t, s, "s1", model.StateNeedsDecision)
	first := create(t, s, ticket(t, "First blocking question?", model.ModeBlocking, "s1", t0))
	second := create(t, s, ticket(t, "Second blocking question?", model.ModeBlocking, "s1", t0))
	answer(t, s, first.ID, keyA, t0.Add(time.Hour))
	if got := get(t, s, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("after one of two:\n got %+v\nwant %+v", got, want)
	}
	answer(t, s, second.ID, keyA, t0.Add(time.Hour))
	if got := get(t, s, "s1").State; got != model.StateIdle {
		t.Errorf("after both: state %s, want idle", got)
	}
}

func TestAnswerDoesNotTouchOtherStates(t *testing.T) {
	s := open(t, dbPath(t))
	for _, state := range []model.State{model.StateWorking, model.StateWaiting, model.StateIdle, model.StateEnded} {
		id := "s-" + string(state)
		want := needsDecision(t, s, id, state)
		d := create(t, s, ticket(t, "Question for "+id+"?", model.ModeBlocking, id, t0))
		answer(t, s, d.ID, keyA, t0.Add(time.Hour))
		if got := get(t, s, id); !reflect.DeepEqual(got, want) {
			t.Errorf("%s changed:\n got %+v\nwant %+v", state, got, want)
		}
	}
}

func TestWithdrawRules(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	withdraw := func(id, caller, precedent string) error {
		_, err := s.WithdrawDecision(ctx, id, caller, "Precedent applies.", precedent, now)
		return err
	}
	decided := create(t, s, ticket(t, "Decided precedent?", model.ModeBlocking, "s1", t0))
	answer(t, s, decided.ID, keyA, now)
	other := create(t, s, ticket(t, "Still open?", model.ModeBlocking, "s2", t0))
	mine := create(t, s, ticket(t, "My question?", model.ModeBlocking, "s1", t0))

	for _, tc := range []struct {
		name, id, caller, precedent string
		want                        error
	}{
		{"unknown id", "D-99", "s1", "", store.ErrNotFound},
		{"other session", mine.ID, "s2", "", store.ErrNotOwner},
		{"unknown caller", mine.ID, "", "", store.ErrNotOwner},
		{"precedent missing", mine.ID, "s1", "D-9", store.ErrBadPrecedent},
		{"precedent open", mine.ID, "s1", other.ID, store.ErrBadPrecedent},
		{"precedent itself", mine.ID, "s1", mine.ID, store.ErrBadPrecedent},
		{"decided ticket", decided.ID, "s1", "", store.ErrNotOpen},
	} {
		if err := withdraw(tc.id, tc.caller, tc.precedent); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	assertDecision(t, getDecision(t, s, mine.ID), mine)

	got, err := s.WithdrawDecision(ctx, mine.ID, "s1", "Precedent applies.", decided.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	want := mine
	want.Status, want.WithdrawReason, want.PrecedentOf, want.ClosedAt = model.StatusWithdrawn, "Precedent applies.", decided.ID, now
	assertDecision(t, got, want)
	assertDecision(t, getDecision(t, s, mine.ID), want)
	if err := withdraw(mine.ID, "s1", ""); !errors.Is(err, store.ErrNotOpen) {
		t.Errorf("withdrawn twice: %v, want ErrNotOpen", err)
	}

	// Two empty session ids are equal; a past-deadline ticket is not open.
	anon := create(t, s, ticket(t, "From no session?", model.ModeBlocking, "", t0))
	if err := withdraw(anon.ID, "", ""); err != nil {
		t.Errorf("unknown caller withdrawing its own: %v", err)
	}
	late := create(t, s, ticket(t, "Assumed long ago?", model.ModeAssume, "s1", now.Add(-assumeExpiry)))
	if err := withdraw(late.ID, "s1", ""); !errors.Is(err, store.ErrNotOpen) {
		t.Errorf("past deadline: %v, want ErrNotOpen", err)
	}
}

func TestPastDeadlineIsNotOpenBeforeTheSweep(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	d := create(t, s, ticket(t, "Assumed and forgotten?", model.ModeAssume, "s1", t0))

	visible := func(now time.Time) (inTickets, inCounts bool) {
		t.Helper()
		ts, err := s.OpenTickets(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		counts, err := s.OpenCounts(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return len(ts) == 1 && ts[0].ID == d.ID, counts["s1"].Assume == 1
	}
	if inTickets, inCounts := visible(d.ExpiresAt.Add(-time.Second)); !inTickets || !inCounts {
		t.Errorf("before the deadline: in tickets %v, in counts %v; want both", inTickets, inCounts)
	}
	for _, now := range []time.Time{d.ExpiresAt, d.ExpiresAt.Add(time.Second)} {
		if inTickets, inCounts := visible(now); inTickets || inCounts {
			t.Errorf("at %v: in tickets %v, in counts %v; want neither", now, inTickets, inCounts)
		}
	}
	if _, err := s.AnswerDecision(ctx, d.ID, keyA, d.ExpiresAt.Add(time.Second)); !errors.Is(err, store.ErrNotOpen) {
		t.Errorf("answer past the deadline: %v, want ErrNotOpen", err)
	}
	var status string
	if err := rawDB(t, path).QueryRow(`SELECT status FROM decisions WHERE id = ?`, d.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "open" {
		t.Errorf("status column = %s, want open (no sweep ran)", status)
	}
}

func TestExpireFlipsOnlyDueAssume(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	due := create(t, s, ticket(t, "Due assume?", model.ModeAssume, "s1", t0))
	notYet := create(t, s, ticket(t, "Later assume?", model.ModeAssume, "s1", t0.Add(2*time.Hour)))
	blocking := create(t, s, ticket(t, "Blocking forever?", model.ModeBlocking, "s1", t0.Add(-48*time.Hour)))
	decided := create(t, s, ticket(t, "Confirmed assume?", model.ModeAssume, "s1", t0))
	decided = answer(t, s, decided.ID, keyA, t0.Add(time.Hour))

	n, err := s.ExpireDecisions(ctx, due.ExpiresAt)
	if err != nil || n != 1 {
		t.Fatalf("ExpireDecisions = %d, %v; want 1, nil", n, err)
	}
	got := getDecision(t, s, due.ID)
	if got.Status != model.StatusExpired || !got.ClosedAt.Equal(due.ExpiresAt) {
		t.Errorf("due = %s closed %v, want expired at %v", got.Status, got.ClosedAt, due.ExpiresAt)
	}
	for _, want := range []model.Decision{notYet, blocking, decided} {
		assertDecision(t, getDecision(t, s, want.ID), want)
	}
	if n, err := s.ExpireDecisions(ctx, due.ExpiresAt); err != nil || n != 0 {
		t.Errorf("second sweep = %d, %v; want 0, nil", n, err)
	}
}

func TestEditRescopeAndSupersede(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	d := create(t, s, ticket(t, "Decided to rescope?", model.ModeBlocking, "s1", t0))
	d = answer(t, s, d.ID, keyA, now)
	replacement := create(t, s, ticket(t, "The replacement?", model.ModeBlocking, "s1", t0))
	replacement = answer(t, s, replacement.ID, keyA, now)
	pending := create(t, s, ticket(t, "Still open?", model.ModeBlocking, "s1", t0))

	edited := now.Add(time.Hour)
	got, err := s.EditDecision(ctx, d.ID, model.DecisionEdit{Scope: model.ScopeGlobal}, edited)
	if err != nil {
		t.Fatal(err)
	}
	want := d
	want.Scope = model.ScopeGlobal
	assertDecision(t, got, want)

	got, err = s.EditDecision(ctx, d.ID, model.DecisionEdit{Supersede: true}, edited)
	if err != nil {
		t.Fatal(err)
	}
	want.SupersededAt = edited
	assertDecision(t, got, want)
	assertDecision(t, getDecision(t, s, d.ID), want)

	for _, tc := range []struct {
		name, id string
		e        model.DecisionEdit
		want     error
	}{
		{"unknown id", "D-99", model.DecisionEdit{Scope: model.ScopeTask}, store.ErrNotFound},
		{"open ticket", pending.ID, model.DecisionEdit{Scope: model.ScopeTask}, store.ErrNotDecided},
		{"replacement missing", d.ID, model.DecisionEdit{Supersede: true, SupersededBy: "D-99"}, store.ErrBadPrecedent},
		{"replacement open", d.ID, model.DecisionEdit{Supersede: true, SupersededBy: pending.ID}, store.ErrBadPrecedent},
		{"replacement itself", d.ID, model.DecisionEdit{Supersede: true, SupersededBy: d.ID}, store.ErrBadPrecedent},
	} {
		if _, err = s.EditDecision(ctx, tc.id, tc.e, edited); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	assertDecision(t, getDecision(t, s, d.ID), want)
	assertDecision(t, getDecision(t, s, pending.ID), pending)

	// Superseding again names the replacement and keeps the first
	// superseded_at (Ruling 65).
	later := edited.Add(time.Minute)
	got, err = s.EditDecision(ctx, d.ID, model.DecisionEdit{Supersede: true, SupersededBy: replacement.ID}, later)
	if err != nil {
		t.Fatal(err)
	}
	want.SupersededBy = replacement.ID
	assertDecision(t, got, want)
	if got.Status != model.StatusAnswered {
		t.Errorf("status = %s, want answered (superseding changes no status)", got.Status)
	}
}

// Ruling 82: superseding an answer still queued leaves it queued (it is
// delivered as the retraction); the first supersession of an answer already
// delivered to a known session queues it again, once; an answer with no
// session, or one never delivered (a confirmation), is left as it is.
func TestSupersedeQueuesTheRetraction(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	now := t0.Add(time.Hour)
	edited := now.Add(time.Hour)

	still := answer(t, s, create(t, s, ticket(t, "Still queued?", model.ModeBlocking, "s1", t0)).ID, keyA, now)
	if got := supersede(t, s, still.ID, edited); got.Delivery != model.DeliveryQueued {
		t.Errorf("queued answer superseded: delivery %s, want queued", got.Delivery)
	}

	for _, via := range []model.DeliveryVia{model.ViaStopHook, model.ViaCheck, model.ViaSendKeys} {
		d := answer(t, s, create(t, s, ticket(t, "Delivered by "+string(via)+"?", model.ModeBlocking, "s1", t0)).ID, keyA, now)
		deliverBy(t, s, path, d.ID, via, now)
		got := supersede(t, s, d.ID, edited)
		if got.Delivery != model.DeliveryQueued || got.DeliveredVia != "" || !got.DeliveredAt.IsZero() {
			t.Errorf("delivered by %s, superseded: delivery %s via %q at %v; want queued again", via, got.Delivery, got.DeliveredVia, got.DeliveredAt)
		}
		assertQueued(t, s, d.ID)
		// The Stop hook delivers it once more, as the retraction; a second
		// supersession does not queue it again.
		if claimed := claimStop(t, s, "s1", edited.Add(time.Minute)); len(claimed) != 1 || claimed[0].ID != d.ID {
			t.Fatalf("Stop after supersession claimed %v, want [%s]", decisionIDs(claimed), d.ID)
		}
		if again := supersede(t, s, d.ID, edited.Add(2*time.Minute)); again.Delivery != model.DeliveryDelivered {
			t.Errorf("second supersession: delivery %s, want delivered (not queued again)", again.Delivery)
		}
	}

	orphan := create(t, s, ticket(t, "Nobody asked?", model.ModeBlocking, "", t0))
	if got := supersede(t, s, answer(t, s, orphan.ID, keyA, now).ID, edited); got.Delivery != model.DeliveryNone {
		t.Errorf("answer with no session superseded: delivery %s, want none", got.Delivery)
	}
	confirmed := answer(t, s, create(t, s, ticket(t, "Assumed right?", model.ModeAssume, "s1", t0)).ID, keyA, now)
	if confirmed.Status != model.StatusConfirmed {
		t.Fatalf("setup: status %s, want confirmed", confirmed.Status)
	}
	if got := supersede(t, s, confirmed.ID, edited); got.Delivery != model.DeliveryNone {
		t.Errorf("confirmation superseded: delivery %s, want none", got.Delivery)
	}
}

// supersede supersedes decided ticket id at at and returns the stored row.
func supersede(t *testing.T, s *store.Store, id string, at time.Time) model.Decision {
	t.Helper()
	got, err := s.EditDecision(context.Background(), id, model.DecisionEdit{Supersede: true}, at)
	if err != nil {
		t.Fatalf("EditDecision(%s): %v", id, err)
	}
	return got
}

// deliverBy delivers queued answer id of session s1 by path via at now.
func deliverBy(t *testing.T, s *store.Store, path, id string, via model.DeliveryVia, now time.Time) {
	t.Helper()
	if via == model.ViaCheck {
		if _, _, err := s.CheckDecision(context.Background(), id, "s1", now); err != nil {
			t.Fatal(err)
		}
		return
	}
	claimStop(t, s, "s1", now)
	if via == model.ViaSendKeys {
		rawExec(t, path, `UPDATE decisions SET delivered_via = 'send_keys' WHERE id = ?`, id)
	}
}

func TestEditSupersededByImpliesSupersede(t *testing.T) {
	// Ruling 65: a non-empty SupersededBy supersedes; superseding again
	// keeps the first superseded_at and never clears superseded_by.
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	var ids []string
	for i := range 3 {
		d := create(t, s, ticket(t, fmt.Sprintf("Decided question %d?", i), model.ModeBlocking, "s1", t0))
		answer(t, s, d.ID, keyA, now)
		ids = append(ids, d.ID)
	}
	edit := func(e model.DecisionEdit, at time.Time) model.Decision {
		t.Helper()
		got, err := s.EditDecision(ctx, ids[0], e, at)
		if err != nil {
			t.Fatalf("EditDecision(%+v): %v", e, err)
		}
		return got
	}
	want := getDecision(t, s, ids[0])

	first := now.Add(time.Hour)
	want.SupersededAt, want.SupersededBy = first, ids[1]
	assertDecision(t, edit(model.DecisionEdit{SupersededBy: ids[1]}, first), want)

	// Supersede with no replacement: superseded_by stays, superseded_at stays.
	assertDecision(t, edit(model.DecisionEdit{Supersede: true}, first.Add(time.Hour)), want)
	// A rescope alone touches neither.
	want.Scope = model.ScopeGlobal
	assertDecision(t, edit(model.DecisionEdit{Scope: model.ScopeGlobal}, first.Add(2*time.Hour)), want)
	// A new replacement is recorded; superseded_at stays.
	want.SupersededBy = ids[2]
	assertDecision(t, edit(model.DecisionEdit{SupersededBy: ids[2]}, first.Add(3*time.Hour)), want)
	assertDecision(t, getDecision(t, s, ids[0]), want)
}

func TestUndecodableOptionsFailsTheReadNamingTheID(t *testing.T) {
	// A corrupt row fails the read, and the error says which row; it is
	// never skipped silently.
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	create(t, s, ticket(t, "A good ticket?", model.ModeBlocking, "s1", t0))
	create(t, s, ticket(t, "A corrupted ticket?", model.ModeBlocking, "s1", t0))
	rawExec(t, path, `UPDATE decisions SET options = 'not json' WHERE id = 'D-2'`)

	_, _, getErr := s.GetDecision(ctx, "D-2")
	_, listErr := s.ListDecisions(ctx, store.DecisionFilter{})
	_, openErr := s.OpenTickets(ctx, t0)
	for name, err := range map[string]error{"GetDecision": getErr, "ListDecisions": listErr, "OpenTickets": openErr} {
		if err == nil || !strings.Contains(err.Error(), "decision D-2") {
			t.Errorf("%s = %v, want an error naming decision D-2", name, err)
		}
	}
	if _, _, err := s.GetDecision(ctx, "D-1"); err != nil {
		t.Errorf("GetDecision(D-1) = %v, want the good row", err)
	}
}

func TestOpenTicketsJoinsSession(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	upsert(t, s, model.Session{
		ID: "s1", Agent: "claude", Pane: "%3", TmuxSession: "work", TmuxWindow: "@2", Branch: "feat/x",
		State: model.StateNeedsDecision, ContextUsed: 123456,
	})
	cache := t0.Add(5 * time.Minute)
	rawExec(t, path, `UPDATE sessions SET cache_expires_at = ?, cache_rebuild_usd = ? WHERE id = 's1'`,
		cache.Format("2006-01-02T15:04:05.000000000Z"), 0.42)
	known := create(t, s, ticket(t, "From a known session?", model.ModeBlocking, "s1", t0))
	decided := create(t, s, ticket(t, "Already decided?", model.ModeBlocking, "s1", t0))
	answer(t, s, decided.ID, keyA, t0)
	ghost := create(t, s, ticket(t, "From an unknown session?", model.ModeAssume, "ghost", t0))

	got, err := s.OpenTickets(context.Background(), t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("OpenTickets = %d tickets, want 2", len(got))
	}
	assertDecision(t, got[0].Decision, known)
	if got[0].SessionState != model.StateNeedsDecision || got[0].Pane != "%3" || got[0].TmuxSession != "work" ||
		got[0].TmuxWindow != "@2" || got[0].Branch != "feat/x" || got[0].ContextUsed != 123456 ||
		!got[0].CacheExpiresAt.Equal(cache) || got[0].CacheRebuildUSD != 0.42 {
		t.Errorf("known session ticket = %+v", got[0])
	}
	assertDecision(t, got[1].Decision, ghost)
	got[1].Decision = model.Decision{}
	if !reflect.DeepEqual(got[1], model.Ticket{}) {
		t.Errorf("unknown session ticket fields = %+v, want zero", got[1])
	}
}

func TestOpenCountsAndBlockingCount(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	for i, tc := range []struct {
		session string
		mode    model.Mode
	}{
		{"S1", model.ModeBlocking}, {"S1", model.ModeBlocking}, {"S1", model.ModeAssume}, {"S2", model.ModeAssume},
	} {
		create(t, s, ticket(t, fmt.Sprintf("Open question %d?", i), tc.mode, tc.session, t0))
	}
	answered := create(t, s, ticket(t, "Answered question?", model.ModeBlocking, "S2", t0))
	answer(t, s, answered.ID, keyA, now)
	create(t, s, ticket(t, "Past its deadline?", model.ModeAssume, "S2", now.Add(-assumeExpiry)))

	counts, err := s.OpenCounts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]model.OpenCount{"S1": {Blocking: 2, Assume: 1}, "S2": {Assume: 1}}; !reflect.DeepEqual(counts, want) {
		t.Errorf("OpenCounts = %v, want %v", counts, want)
	}
	if n, err := s.CountOpenBlocking(ctx, now); err != nil || n != 2 {
		t.Errorf("CountOpenBlocking = %d, %v; want 2, nil", n, err)
	}
}

func decisionIDs(ds []model.Decision) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func TestListDecisionsFilters(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	for i, session := range []string{"s1", "s2", "s1", "s2", "s1"} {
		create(t, s, ticket(t, fmt.Sprintf("Listed question %d?", i), model.ModeBlocking, session, t0))
	}
	answer(t, s, "D-1", keyA, now)
	if _, err := s.WithdrawDecision(ctx, "D-2", "s2", "", "", now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		f    store.DecisionFilter
		want []string
	}{
		{"all", store.DecisionFilter{}, []string{"D-1", "D-2", "D-3", "D-4", "D-5"}},
		{"open", store.DecisionFilter{Statuses: []model.DecisionStatus{model.StatusOpen}}, []string{"D-3", "D-4", "D-5"}},
		{"answered or withdrawn", store.DecisionFilter{Statuses: []model.DecisionStatus{model.StatusAnswered, model.StatusWithdrawn}}, []string{"D-1", "D-2"}},
		{"session", store.DecisionFilter{SessionID: "s2"}, []string{"D-2", "D-4"}},
		{"limit, newest first", store.DecisionFilter{Limit: 2}, []string{"D-5", "D-4"}},
		{"everything at once", store.DecisionFilter{Statuses: []model.DecisionStatus{model.StatusOpen}, SessionID: "s1", Limit: 1}, []string{"D-5"}},
		{"nothing matches", store.DecisionFilter{Statuses: []model.DecisionStatus{model.StatusExpired}}, []string{}},
	} {
		got, err := s.ListDecisions(ctx, tc.f)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decisionIDs(got), tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, decisionIDs(got), tc.want)
		}
	}
	got, err := s.ListDecisions(ctx, store.DecisionFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, got[0], getDecision(t, s, "D-5"))
}

// ftsIntegrity runs FTS5's integrity check of decisions_fts against its
// content table.
func ftsIntegrity(db *sql.DB) error {
	_, err := db.Exec(`INSERT INTO decisions_fts(decisions_fts, rank) VALUES('integrity-check', 1)`)
	return err
}

func TestFTSStaysInSync(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	now := t0.Add(time.Hour)
	for i := range 4 {
		mode := model.ModeBlocking
		if i == 3 {
			mode = model.ModeAssume
		}
		create(t, s, ticket(t, fmt.Sprintf("Indexed question %d?", i), mode, "s1", t0))
	}
	answer(t, s, "D-1", model.Answer{Text: "Use a bloom filter.", Reasoning: "Memory is tight."}, now)
	answer(t, s, "D-2", keyA, now)
	if _, err := s.EditDecision(ctx, "D-1", model.DecisionEdit{Scope: model.ScopeGlobal, Supersede: true}, now); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireDecisions(ctx, t0.Add(assumeExpiry)); err != nil || n != 1 {
		t.Fatalf("ExpireDecisions = %d, %v; want 1, nil", n, err)
	}
	db := rawDB(t, path)
	if err := ftsIntegrity(db); err != nil {
		t.Errorf("integrity-check: %v", err)
	}
	var n int64
	if err := db.QueryRow(`SELECT rowid FROM decisions_fts WHERE decisions_fts MATCH 'bloom'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("MATCH bloom = rowid %d, %v; want 1 (the answer is indexed)", n, err)
	}

	// The check has teeth: without the update trigger the index drifts.
	rawExec(t, path, `DROP TRIGGER decisions_au`)
	rawExec(t, path, `UPDATE decisions SET answer = 'Something else entirely.' WHERE id = 'D-2'`)
	if err := ftsIntegrity(db); err == nil {
		t.Error("integrity-check passed with the index out of sync")
	}
}

func TestCacheColumnsSurviveSessionUpsert(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	upsert(t, s, model.Session{ID: "s1", Agent: "claude", State: model.StateWorking})
	const cache = "2026-11-12T09:05:00.000000000Z"
	rawExec(t, path, `UPDATE sessions SET cache_expires_at = ?, cache_rebuild_usd = ? WHERE id = 's1'`, cache, 1.25)
	upsert(t, s, model.Session{ID: "s1", Agent: "claude", State: model.StateIdle})
	err := s.UpdateSession(ctx, "s1", func(sess model.Session, _ bool) model.Session {
		sess.State = model.StateWaiting
		return sess
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotCache string
	var gotUSD float64
	err = rawDB(t, path).QueryRow(`SELECT cache_expires_at, cache_rebuild_usd FROM sessions WHERE id = 's1'`).Scan(&gotCache, &gotUSD)
	if err != nil {
		t.Fatal(err)
	}
	if gotCache != cache || gotUSD != 1.25 {
		t.Errorf("cache columns = %q %v, want %q 1.25", gotCache, gotUSD, cache)
	}
	if got := get(t, s, "s1").State; got != model.StateWaiting {
		t.Errorf("state = %s, want waiting", got)
	}
}

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

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// dbPath returns a fresh database path under t.TempDir().
func dbPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "curia.db")
}

// open opens a Store and closes it when the test ends.
func open(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// rawDB opens a plain driver connection to the same file, bypassing Store,
// so tests can inspect the schema and hold locks.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func upsert(t *testing.T, s *store.Store, sess model.Session) {
	t.Helper()
	if err := s.UpsertSession(context.Background(), sess); err != nil {
		t.Fatalf("UpsertSession(%s): %v", sess.ID, err)
	}
}

func get(t *testing.T, s *store.Store, id string) model.Session {
	t.Helper()
	got, ok, err := s.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession(%s): %v", id, err)
	}
	if !ok {
		t.Fatalf("GetSession(%s): not found", id)
	}
	return got
}

func ids(sessions []model.Session) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.ID)
	}
	return out
}

func countMigrations(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := rawDB(t, path).QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// migrationNames lists schema_migrations by name, as the migration runner
// reads it.
func migrationNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestOpenCreatesSchemaAndWAL(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	for _, table := range []string{"sessions", "events", "schema_migrations"} {
		var n int
		err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	// The two spec deviations: task_id is plain TEXT, transcript_offset exists.
	var fks int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('sessions')`).Scan(&fks); err != nil {
		t.Fatal(err)
	}
	if fks != 0 {
		t.Errorf("sessions has %d foreign keys, want 0", fks)
	}
	var offsetType string
	var notNull int
	err := db.QueryRow(`SELECT type, "notnull" FROM pragma_table_info('sessions') WHERE name = 'transcript_offset'`).
		Scan(&offsetType, &notNull)
	if err != nil {
		t.Fatalf("transcript_offset column: %v", err)
	}
	if offsetType != "INTEGER" || notNull != 1 {
		t.Errorf("transcript_offset = %s notnull=%d, want INTEGER NOT NULL", offsetType, notNull)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := dbPath(t)
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	open(t, path)
	if n := countMigrations(t, path); n != len(allMigrations) {
		t.Errorf("schema_migrations has %d rows, want %d", n, len(allMigrations))
	}
	names := migrationNames(t, rawDB(t, path))
	if want := allMigrations; !reflect.DeepEqual(names, want) {
		t.Errorf("migration names = %v, want %v", names, want)
	}
}

// allMigrations is every migration file in the order Open applies them:
// file name order, so the two files numbered 0005 (Plan 4's reviews and
// Plan 3's send limits, merged with their names kept) apply in a fixed order.
var allMigrations = migrationFiles()

// migrationFiles lists the files of migrations/ (os.ReadDir sorts by name).
func migrationFiles() []string {
	es, err := os.ReadDir("migrations")
	if err != nil {
		panic(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// plan3Applied is what a Plan 3 binary (with the fix wave) applies.
var plan3Applied = []string{
	"0001_init.sql", "0002_decisions.sql", "0003_typed_confirmation.sql", "0004_tasks.sql", "0005_send_limits.sql",
}

// plan5Applied and plan6Applied are what each branch's binary applied before the merge.
var (
	plan5Applied = []string{"0001_init.sql", "0002_decisions.sql", "0003_typed_confirmation.sql", "0004_tasks.sql", "0005_reviews.sql", "0005_send_limits.sql", "0006_cost.sql", "0007_usage_forecast.sql", "0008_context_live.sql", "0009_session_checked_cwd.sql"}
	plan6Applied = []string{"0001_init.sql", "0002_decisions.sql", "0003_typed_confirmation.sql", "0004_tasks.sql", "0005_reviews.sql", "0005_send_limits.sql", "0006_cost.sql", "0007_usage_forecast.sql", "0008_config_revisions.sql", "0009_decision_near_match.sql"}
)

// migratedDatabase builds, at path, a database with exactly the migrations
// named applied and recorded, as a binary built with only those files
// leaves it.
func migratedDatabase(t *testing.T, path string, names ...string) *sql.DB {
	t.Helper()
	db := rawDB(t, path)
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;
CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		body, err := os.ReadFile(filepath.Clean(filepath.Join("migrations", name)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err = db.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, '2026-11-14T09:00:00.000000000Z')`, name); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// appliedOrder is the migration names in the order they were recorded.
func appliedOrder(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM schema_migrations ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// wantMergedSchema checks that both 0005 migrations took effect: Plan 4's
// reviews table and session columns, and Plan 3's send-limit index.
func wantMergedSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []struct{ what, sql string }{
		{"table reviews", `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'reviews'`},
		{"table pushes", `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'pushes'`},
		{"sessions.end_reason", `SELECT count(*) FROM pragma_table_info('sessions') WHERE name = 'end_reason'`},
		{"index messages_agent_sends", `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'messages_agent_sends'`},
	} {
		var n int
		if err := db.QueryRow(q.sql).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d, %v; want it present", q.what, n, err)
		}
	}
}

// TestBothMigrationsNumberedFive: migrations are tracked by file name, and
// the merge of plan-3 into plan-4 keeps both 0005 files under their names
// (renaming one would run it again on a database that has it). Open
// applies both, in file name order, on a fresh database, on a Plan 4
// database (0005_reviews.sql applied), on a Plan 3 database
// (0005_send_limits.sql applied) and on a merged Plan 4 database (both
// applied), and each runs once; 0006_cost.sql and 0007_usage_forecast.sql follow both.
func TestBothMigrationsNumberedFive(t *testing.T) {
	for _, c := range []struct {
		name    string
		applied []string // nil: a fresh database
		order   []string // the recorded order after Open
	}{
		{"fresh", nil, allMigrations},
		{"plan 4 database", allMigrations[:5], allMigrations},
		{"plan 3 database", plan3Applied, append(append(slices.Clone(plan3Applied), "0005_reviews.sql"), allMigrations[6:]...)},
		{"plan 4 database with both 0005 files", allMigrations[:6], allMigrations},
		// 0008 and 0009 each exist twice (plan-5: context_live, checked_cwd;
		// plan-6: config_revisions, decision_near_match). They touch different
		// tables or columns, so their order does not matter; a store that ran
		// either branch's set gets the other branch's two files.
		{"plan-5 database", plan5Applied, append(slices.Clone(plan5Applied), "0008_config_revisions.sql", "0009_decision_near_match.sql", "0010_task_pane_socket.sql", "0011_task_filed.sql", "0012_task_budget.sql", "0013_broadcast.sql", "0014_resource_samples.sql", "0016_device_tokens.sql")},
		{"plan-6 database", plan6Applied, append(slices.Clone(plan6Applied), "0008_context_live.sql", "0009_session_checked_cwd.sql", "0010_task_pane_socket.sql", "0011_task_filed.sql", "0012_task_budget.sql", "0013_broadcast.sql", "0014_resource_samples.sql", "0016_device_tokens.sql")},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := dbPath(t)
			if c.applied != nil {
				migratedDatabase(t, path, c.applied...)
			}
			for range 2 { // the second Open finds nothing pending
				s, err := store.Open(path)
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			db := rawDB(t, path)
			if got := appliedOrder(t, db); !reflect.DeepEqual(got, c.order) {
				t.Errorf("applied = %v, want %v", got, c.order)
			}
			wantMergedSchema(t, db)
		})
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	// Spaces and URI metacharacters in the path must not confuse the DSN.
	path := filepath.Join(t.TempDir(), "My Passport", "a#b?c%d", "curia.db")
	open(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not at %q: %v", path, err)
	}
}

func TestUpsertRoundTrip(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	est := time.FixedZone("EST", -5*3600)
	want := model.Session{
		ID: "sess-1", Agent: "claude", Pane: "%12", TmuxSession: "work", TmuxWindow: "@3",
		Cwd: "/src/curia", Repo: "curia", Branch: "feat/x", Model: "claude-opus-5-5",
		LastUpdate: "Edit store.go", TranscriptPath: "/tmp/t.jsonl",
		State:       model.StateNeedsDecision,
		ContextUsed: 120000, ContextMax: 200000, SubagentsActive: 2,
		TranscriptOffset: 1 << 40,
		StartedAt:        time.Date(2026, 11, 11, 9, 0, 1, 999_000_000, est),
		UpdatedAt:        time.Date(2026, 11, 11, 14, 30, 2, 500, time.UTC),
		SeenAt:           time.Date(2026, 11, 11, 14, 29, 3, 0, time.UTC),
	}
	upsert(t, s, want)
	got := get(t, s, want.ID)

	// Times survive to the nanosecond (ruling 15).
	for name, pair := range map[string][2]time.Time{
		"StartedAt": {got.StartedAt, want.StartedAt},
		"UpdatedAt": {got.UpdatedAt, want.UpdatedAt},
		"SeenAt":    {got.SeenAt, want.SeenAt},
	} {
		if !pair[0].Equal(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
	}
	got.StartedAt, got.UpdatedAt, got.SeenAt = time.Time{}, time.Time{}, time.Time{}
	want.StartedAt, want.UpdatedAt, want.SeenAt = time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, want)
	}

	// A second upsert of the same id overwrites every field.
	changed := model.Session{ID: "sess-1", Agent: "codex", State: model.StateIdle}
	upsert(t, s, changed)
	if got := get(t, s, "sess-1"); !reflect.DeepEqual(got, changed) {
		t.Errorf("after overwrite:\n got %+v\nwant %+v", got, changed)
	}

	if _, ok, err := s.GetSession(ctx, "missing"); err != nil || ok {
		t.Errorf("GetSession(missing) = ok %v, err %v; want false, nil", ok, err)
	}
}

func TestUpsertZeroTimesRoundTripAsZero(t *testing.T) {
	s := open(t, dbPath(t))
	upsert(t, s, model.Session{ID: "z", Agent: "claude", State: model.StateWorking})
	got := get(t, s, "z")
	if !got.StartedAt.IsZero() || !got.UpdatedAt.IsZero() || !got.SeenAt.IsZero() {
		t.Errorf("times = %v %v %v, want zero", got.StartedAt, got.UpdatedAt, got.SeenAt)
	}
}

func TestUpsertStoresTimesAsFixedWidthUTC(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	cases := map[string]time.Time{
		"2026-11-11T14:00:05.000000123Z": time.Date(2026, 11, 11, 9, 0, 5, 123, time.FixedZone("EST", -5*3600)),
		"2026-11-11T14:00:05.000000000Z": time.Date(2026, 11, 11, 14, 0, 5, 0, time.UTC),
		"2026-11-11T14:00:05.500000000Z": time.Date(2026, 11, 11, 14, 0, 5, 500_000_000, time.UTC),
	}
	for want, at := range cases {
		upsert(t, s, model.Session{ID: "t", Agent: "claude", State: model.StateWorking, UpdatedAt: at})
		var text string
		if err := rawDB(t, path).QueryRow(`SELECT updated_at FROM sessions WHERE id = 't'`).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if text != want {
			t.Errorf("updated_at stored as %q, want %q", text, want)
		}
	}
}

func TestListFiltersEndedAndOldStale(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := time.Now()
	upsert(t, s, model.Session{ID: "a-working", Agent: "claude", State: model.StateWorking, UpdatedAt: now})
	upsert(t, s, model.Session{ID: "b-ended", Agent: "claude", State: model.StateEnded, UpdatedAt: now})
	upsert(t, s, model.Session{ID: "c-stale-old", Agent: "claude", State: model.StateStale, UpdatedAt: now.Add(-11 * time.Minute)})
	upsert(t, s, model.Session{ID: "d-stale-new", Agent: "claude", State: model.StateStale, UpdatedAt: now.Add(-2 * time.Minute)})

	cases := []struct {
		name string
		f    store.ListFilter
		want []string
	}{
		{"hide ended and old stale", store.ListFilter{HideStaleBefore: now.Add(-10 * time.Minute)}, []string{"a-working", "d-stale-new"}},
		{"include ended", store.ListFilter{IncludeEnded: true, HideStaleBefore: now.Add(-10 * time.Minute)}, []string{"a-working", "b-ended", "d-stale-new"}},
		{"zero cutoff hides no stale", store.ListFilter{}, []string{"a-working", "c-stale-old", "d-stale-new"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListSessions(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ids(got), tc.want) {
				t.Errorf("ids = %v, want %v", ids(got), tc.want)
			}
		})
	}
}

func TestListHidesStaleWithoutUpdatedAt(t *testing.T) {
	// A zero UpdatedAt is before any cutoff, so such a stale row is hidden.
	s := open(t, dbPath(t))
	upsert(t, s, model.Session{ID: "stale-never", Agent: "claude", State: model.StateStale})
	got, err := s.ListSessions(context.Background(), store.ListFilter{HideStaleBefore: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ids = %v, want none", ids(got))
	}
}

func TestListOrdersStaleCutoffWithinOneSecond(t *testing.T) {
	// Stored text must sort like the times it encodes, even within a second
	// and against a whole-second time (RFC3339Nano would trim ".000" and
	// sort "05Z" after "05.5Z").
	s := open(t, dbPath(t))
	base := time.Date(2026, 11, 11, 14, 0, 5, 0, time.UTC)
	upsert(t, s, model.Session{ID: "a-whole-second", Agent: "claude", State: model.StateStale, UpdatedAt: base})
	upsert(t, s, model.Session{ID: "b-100ms", Agent: "claude", State: model.StateStale, UpdatedAt: base.Add(100 * time.Millisecond)})
	upsert(t, s, model.Session{ID: "c-900ms", Agent: "claude", State: model.StateStale, UpdatedAt: base.Add(900 * time.Millisecond)})
	got, err := s.ListSessions(context.Background(), store.ListFilter{HideStaleBefore: base.Add(500 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c-900ms"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("ids = %v, want %v", ids(got), want)
	}
}

func TestListToleratesNullColumns(t *testing.T) {
	// A row written by something other than Store, with every nullable
	// column NULL, must not break the whole list.
	path := dbPath(t)
	s := open(t, path)
	upsert(t, s, model.Session{ID: "a-normal", Agent: "claude", State: model.StateWorking})
	_, err := rawDB(t, path).Exec(`INSERT INTO sessions (id, agent, state, subagents_active) VALUES ('b-sparse', 'claude', 'idle', NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSessions(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatalf("ListSessions with a sparse row: %v", err)
	}
	if want := []string{"a-normal", "b-sparse"}; !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("ids = %v, want %v", ids(got), want)
	}
	if want := (model.Session{ID: "b-sparse", Agent: "claude", State: model.StateIdle}); !reflect.DeepEqual(got[1], want) {
		t.Errorf("sparse row:\n got %+v\nwant %+v", got[1], want)
	}
}

func TestMarkSeen(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	upsert(t, s, model.Session{ID: "live", Agent: "claude", Pane: "%3", State: model.StateWaiting})
	upsert(t, s, model.Session{ID: "gone", Agent: "claude", Pane: "%3", State: model.StateEnded})
	upsert(t, s, model.Session{ID: "other", Agent: "claude", Pane: "%4", State: model.StateWaiting})

	at := time.Date(2026, 11, 11, 15, 0, 0, 123_456_789, time.UTC)
	if err := s.MarkSeen(ctx, "%3", "", at); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "live").SeenAt; !got.Equal(at) {
		t.Errorf("live SeenAt = %v, want %v", got, at)
	}
	if got := get(t, s, "gone").SeenAt; !got.IsZero() {
		t.Errorf("ended SeenAt = %v, want zero", got)
	}
	if got := get(t, s, "other").SeenAt; !got.IsZero() {
		t.Errorf("other pane SeenAt = %v, want zero", got)
	}
}

func TestEndOthersInPane(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	before := time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)
	a := model.Session{ID: "A", Agent: "claude", Pane: "%3", State: model.StateIdle, UpdatedAt: before}
	b := model.Session{ID: "B", Agent: "claude", Pane: "%3", State: model.StateWorking, UpdatedAt: before}
	c := model.Session{ID: "C", Agent: "claude", Pane: "%4", State: model.StateIdle, UpdatedAt: before}
	for _, sess := range []model.Session{a, b, c} {
		upsert(t, s, sess)
	}

	at := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	if err := s.EndOthersInPane(ctx, "%3", "", "B", at); err != nil {
		t.Fatal(err)
	}
	gotA := get(t, s, "A")
	if gotA.State != model.StateEnded || !gotA.UpdatedAt.Equal(at) {
		t.Errorf("A = %s at %v, want ended at %v", gotA.State, gotA.UpdatedAt, at)
	}
	if got := get(t, s, "B"); !reflect.DeepEqual(got, b) {
		t.Errorf("B changed:\n got %+v\nwant %+v", got, b)
	}
	if got := get(t, s, "C"); !reflect.DeepEqual(got, c) {
		t.Errorf("C (other pane) changed:\n got %+v\nwant %+v", got, c)
	}
}

func TestPaneUpdatesIgnoreSessionsWithoutPane(t *testing.T) {
	// Sessions started outside tmux have no pane; they are not "in" pane "".
	s := open(t, dbPath(t))
	ctx := context.Background()
	x := model.Session{ID: "x", Agent: "claude", State: model.StateWorking}
	y := model.Session{ID: "y", Agent: "claude", State: model.StateWorking}
	upsert(t, s, x)
	upsert(t, s, y)
	at := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	if err := s.MarkSeen(ctx, "", "", at); err != nil {
		t.Fatal(err)
	}
	if err := s.EndOthersInPane(ctx, "", "", "y", at); err != nil {
		t.Fatal(err)
	}
	for _, want := range []model.Session{x, y} {
		if got := get(t, s, want.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("%s changed:\n got %+v\nwant %+v", want.ID, got, want)
		}
	}
}

func TestAppendEvent(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	ev := model.Event{
		Kind: model.PreToolUse, SessionID: "sess-1", Pane: "%3",
		At: time.Date(2026, 11, 11, 9, 0, 5, 0, time.FixedZone("EST", -5*3600)),
	}
	raw := []byte(`{"session_id":"sess-1","tool_name":"Edit"}`)
	if err := s.AppendEvent(ctx, ev, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, ev, raw); err != nil {
		t.Fatal(err)
	}

	rows, err := rawDB(t, path).Query(`SELECT session_id, kind, json_extract(payload, '$.tool_name'), at FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var n int
	for rows.Next() {
		n++
		var sid, kind, tool, at string
		if err := rows.Scan(&sid, &kind, &tool, &at); err != nil {
			t.Fatal(err)
		}
		if sid != "sess-1" || kind != "PreToolUse" || tool != "Edit" || at != "2026-11-11T14:00:05.000000000Z" {
			t.Errorf("event row = %q %q %q %q", sid, kind, tool, at)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("events rows = %d, want 2 (append-only)", n)
	}
}

func TestConcurrentUpserts(t *testing.T) {
	path := dbPath(t)
	handles := make([]*store.Store, 5)
	for i := range handles {
		handles[i] = open(t, path)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := handles[g%len(handles)]
			for i := range 50 {
				sess := model.Session{
					ID: fmt.Sprintf("sess-%d", (g+i)%5), Agent: "claude", Pane: "%1",
					State: model.StateWorking, ContextUsed: g*1000 + i, UpdatedAt: time.Now(),
				}
				if err := s.UpsertSession(ctx, sess); err != nil {
					t.Errorf("goroutine %d upsert %d: %v", g, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	got, err := handles[0].ListSessions(ctx, store.ListFilter{IncludeEnded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("ListSessions returned %d rows, want 5: %v", len(got), ids(got))
	}
}

func TestConcurrentOpenOfFreshFile(t *testing.T) {
	// Several hooks may be the first to touch a new database at once, racing
	// to switch it to WAL and to migrate it. The race window is small, so
	// each run repeats it on several fresh files.
	const openers = 10
	for round := range 10 {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("round%d.db", round))
		openAndUpsertConcurrently(t, path, openers)
		if n := countMigrations(t, path); n != len(allMigrations) {
			t.Errorf("round %d: schema_migrations has %d rows, want %d", round, n, len(allMigrations))
		}
		got, err := open(t, path).ListSessions(context.Background(), store.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != openers {
			t.Errorf("round %d: ListSessions returned %d rows, want %d", round, len(got), openers)
		}
	}
}

// openAndUpsertConcurrently has n goroutines each Open path at the same
// time and upsert one session.
func openAndUpsertConcurrently(t *testing.T, path string, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for g := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := store.Open(path)
			if err != nil {
				t.Errorf("goroutine %d Open: %v", g, err)
				return
			}
			defer func() { _ = s.Close() }()
			sess := model.Session{ID: fmt.Sprintf("s%d", g), Agent: "claude", State: model.StateWorking}
			if err := s.UpsertSession(context.Background(), sess); err != nil {
				t.Errorf("goroutine %d upsert: %v", g, err)
			}
		}()
	}
	wg.Wait()
}

// holdExclusive opens the store at a fresh path with one session "held",
// then has another connection take BEGIN EXCLUSIVE until the test ends. It
// returns a Store opened while the lock is held.
func holdExclusive(t *testing.T) *store.Store {
	t.Helper()
	path := dbPath(t)
	upsert(t, open(t, path), model.Session{ID: "held", Agent: "claude", State: model.StateWorking})
	ctx := context.Background()
	conn, err := rawDB(t, path).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		_ = conn.Close()
	})
	return open(t, path)
}

// assertBusyAfterTimeout runs write and requires it to fail with
// SQLITE_BUSY after waiting out busy_timeout (2000ms), never hanging.
func assertBusyAfterTimeout(t *testing.T, write func() error) {
	t.Helper()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- write() }()
	select {
	case err := <-done:
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
			t.Fatalf("write under an exclusive lock returned %v, want SQLITE_BUSY", err)
		}
		if elapsed := time.Since(start); elapsed < 1900*time.Millisecond {
			t.Errorf("write failed after %v, want it to wait out busy_timeout (2000ms)", elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("write hung under an exclusive lock")
	}
}

func TestWriteUnderExclusiveLockReturnsError(t *testing.T) {
	t.Parallel()
	// A hook opening the store while the lock is held still opens it, and
	// views can still read (WAL).
	late := holdExclusive(t)
	ctx := context.Background()
	if _, ok, err := late.GetSession(ctx, "held"); err != nil || !ok {
		t.Fatalf("read under lock = ok %v, err %v; want true, nil", ok, err)
	}
	assertBusyAfterTimeout(t, func() error {
		return late.UpsertSession(ctx, model.Session{ID: "blocked", Agent: "claude", State: model.StateWorking})
	})
}

func TestUpdateSessionUnderExclusiveLockReturnsBusy(t *testing.T) {
	t.Parallel()
	late := holdExclusive(t)
	called := false
	assertBusyAfterTimeout(t, func() error {
		return late.UpdateSession(context.Background(), "held", func(s model.Session, _ bool) model.Session {
			called = true
			return s
		})
	})
	if called {
		t.Error("fn ran without the write lock; UpdateSession must take it before reading")
	}
}

func TestUpdateSessionLosesNoUpdates(t *testing.T) {
	path := dbPath(t)
	handles := make([]*store.Store, 5)
	for i := range handles {
		handles[i] = open(t, path)
	}
	ctx := context.Background()
	const goroutines, calls = 10, 10
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := handles[g%len(handles)]
			for i := range calls {
				if err := incrementShared(ctx, s); err != nil {
					t.Errorf("goroutine %d call %d: %v", g, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := get(t, handles[0], "shared").SubagentsActive; got != goroutines*calls {
		t.Errorf("SubagentsActive = %d, want %d (updates lost)", got, goroutines*calls)
	}
}

// incrementShared adds one to the shared session's SubagentsActive through
// UpdateSession. SQLite's busy handler polls rather than queues, so under
// this load a writer can starve past busy_timeout and get SQLITE_BUSY. That
// comes from BEGIN, before fn runs, so the attempt wrote nothing and is
// retried; any other error, or BUSY after fn ran, is returned.
func incrementShared(ctx context.Context, s *store.Store) error {
	const maxAttempts = 20
	var err error
	for range maxAttempts {
		ran := false
		err = s.UpdateSession(ctx, "shared", func(sess model.Session, _ bool) model.Session {
			ran = true
			sess.ID, sess.Agent, sess.State = "shared", "claude", model.StateWorking
			sess.SubagentsActive++
			return sess
		})
		var se *sqlite.Error
		if err == nil || ran || !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
			return err
		}
	}
	return fmt.Errorf("still busy after %d attempts: %w", maxAttempts, err)
}

func TestUpdateSessionPreservesConcurrentMarkSeen(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	upsert(t, s, model.Session{ID: "a", Agent: "claude", Pane: "%3", State: model.StateWorking})
	setState := func(st model.State) func(model.Session, bool) model.Session {
		return func(sess model.Session, _ bool) model.Session {
			sess.State = st
			return sess
		}
	}
	if err := s.UpdateSession(ctx, "a", setState(model.StateWaiting)); err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 11, 11, 15, 0, 0, 42, time.UTC)
	if err := s.MarkSeen(ctx, "%3", "", seen); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSession(ctx, "a", setState(model.StateIdle)); err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "a")
	if got.State != model.StateIdle || !got.SeenAt.Equal(seen) {
		t.Errorf("got state %s SeenAt %v, want idle and %v", got.State, got.SeenAt, seen)
	}
}

func TestUpdateSessionCreatesAbsentRow(t *testing.T) {
	s := open(t, dbPath(t))
	want := model.Session{ID: "new", Agent: "claude", State: model.StateWorking, ContextMax: 200000}
	var sawFound bool
	var sawSession model.Session
	err := s.UpdateSession(context.Background(), "new", func(sess model.Session, found bool) model.Session {
		sawSession, sawFound = sess, found
		return want
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawFound || !reflect.DeepEqual(sawSession, model.Session{}) {
		t.Errorf("fn got (%+v, %v), want zero Session and false", sawSession, sawFound)
	}
	if got := get(t, s, "new"); !reflect.DeepEqual(got, want) {
		t.Errorf("created row:\n got %+v\nwant %+v", got, want)
	}
}

func TestUpdateSessionRejectsChangedID(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	orig := model.Session{ID: "a", Agent: "claude", State: model.StateWorking}
	upsert(t, s, orig)
	err := s.UpdateSession(ctx, "a", func(sess model.Session, _ bool) model.Session {
		sess.ID, sess.State = "b", model.StateIdle
		return sess
	})
	if err == nil {
		t.Fatal("UpdateSession with a changed ID returned nil, want an error")
	}
	if got := get(t, s, "a"); !reflect.DeepEqual(got, orig) {
		t.Errorf("a changed:\n got %+v\nwant %+v", got, orig)
	}
	if _, ok, err := s.GetSession(ctx, "b"); err != nil || ok {
		t.Errorf("GetSession(b) = ok %v, err %v; want nothing written", ok, err)
	}
}

// TestMarkSeenOnlyThisServer: marking a pane seen on one tmux server leaves
// the session in the same pane id of another server unseen; a session with
// no recorded server, and a call that knows no server, behave as before.
func TestMarkSeenOnlyThisServer(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	upsert(t, s, model.Session{ID: "here", Agent: "claude", Pane: "%0", State: model.StateWaiting, Socket: "/tmp/tmux-501/a"})
	upsert(t, s, model.Session{ID: "there", Agent: "claude", Pane: "%0", State: model.StateWaiting, Socket: "/tmp/tmux-501/b"})
	upsert(t, s, model.Session{ID: "unknown", Agent: "claude", Pane: "%0", State: model.StateWaiting})
	at := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	if err := s.MarkSeen(ctx, "%0", "/tmp/tmux-501/a", at); err != nil {
		t.Fatal(err)
	}
	for id, seen := range map[string]bool{"here": true, "there": false, "unknown": true} {
		if got := !get(t, s, id).SeenAt.IsZero(); got != seen {
			t.Errorf("%s seen = %v, want %v", id, got, seen)
		}
	}
	if err := s.MarkSeen(ctx, "%0", "", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, "there").SeenAt; !got.Equal(at.Add(time.Minute)) {
		t.Errorf("no server given: there SeenAt = %v, want marked", got)
	}
}

// TestEndOthersInPaneOnlyThisServer: a SessionStart on one tmux server ends
// the other live sessions in its pane of that server only.
func TestEndOthersInPaneOnlyThisServer(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	upsert(t, s, model.Session{ID: "here", Agent: "claude", Pane: "%0", State: model.StateIdle, Socket: "/tmp/tmux-501/a"})
	upsert(t, s, model.Session{ID: "there", Agent: "claude", Pane: "%0", State: model.StateIdle, Socket: "/tmp/tmux-501/b"})
	upsert(t, s, model.Session{ID: "unknown", Agent: "claude", Pane: "%0", State: model.StateIdle})
	if err := s.EndOthersInPane(ctx, "%0", "/tmp/tmux-501/a", "new", time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]model.State{"here": model.StateEnded, "there": model.StateIdle, "unknown": model.StateEnded} {
		if got := get(t, s, id).State; got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
}

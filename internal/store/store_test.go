package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	if n := countMigrations(t, path); n != 1 {
		t.Errorf("schema_migrations has %d rows, want 1", n)
	}
	var name string
	if err := rawDB(t, path).QueryRow(`SELECT name FROM schema_migrations`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "0001_init.sql" {
		t.Errorf("migration name = %q, want 0001_init.sql", name)
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
	if err := s.MarkSeen(ctx, "%3", at); err != nil {
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
	if err := s.EndOthersInPane(ctx, "%3", "B", at); err != nil {
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
	if err := s.MarkSeen(ctx, "", at); err != nil {
		t.Fatal(err)
	}
	if err := s.EndOthersInPane(ctx, "", "y", at); err != nil {
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
		if n := countMigrations(t, path); n != 1 {
			t.Errorf("round %d: schema_migrations has %d rows, want 1", round, n)
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
	// SQLite's busy handler polls rather than queues, so a writer can starve
	// for as long as others keep the lock saturated. The load is sized to
	// finish well inside busy_timeout (2000ms), so no call may time out and
	// every call must count.
	const goroutines, calls = 10, 10
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := handles[g%len(handles)]
			for i := range calls {
				err := s.UpdateSession(ctx, "shared", func(sess model.Session, _ bool) model.Session {
					sess.ID, sess.Agent, sess.State = "shared", "claude", model.StateWorking
					sess.SubagentsActive++
					return sess
				})
				if err != nil {
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
	if err := s.MarkSeen(ctx, "%3", seen); err != nil {
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

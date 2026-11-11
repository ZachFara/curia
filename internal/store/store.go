// Package store persists agent sessions and raw hook events in a local
// SQLite database in WAL mode. Many hook processes write to one file at the
// same instant while views read it, so every write is a single statement
// and every connection waits out lock contention for busy_timeout.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"modernc.org/sqlite" // also registers the pure-Go "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ZachFara/curia/internal/model"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is a handle on the Curia database. It is safe for concurrent use,
// and several Stores (in one or many processes) may share one file.
type Store struct {
	db *sql.DB
}

// ListFilter selects which sessions ListSessions returns.
type ListFilter struct {
	// IncludeEnded keeps sessions in the ended state.
	IncludeEnded bool
	// HideStaleBefore drops stale sessions whose UpdatedAt is before it.
	// The zero value hides no stale session.
	HideStaleBefore time.Time
}

// Open opens the database at path, creating its parent directory and the
// file if needed, and applies any pending migrations.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path) // Abs also cleans the path
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("store: create directory for %s: %w", abs, err)
	}
	db, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	s := &Store{db: db}
	ctx := context.Background()
	if err = s.enableWAL(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: enable WAL on %s: %w", abs, err)
	}
	if err = s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate %s: %w", abs, err)
	}
	return s, nil
}

// busyTimeout is how long a connection waits for another's lock before a
// statement fails with SQLITE_BUSY.
const busyTimeout = 2000 * time.Millisecond

// dsn builds a file: URI for the driver (url.URL escapes spaces, '#' and
// '?' in the path). busy_timeout is per connection, so it goes in the DSN
// and applies to every pooled connection. _txlock=immediate makes BeginTx
// take the write lock up front, so a transaction never fails to upgrade.
func dsn(abs string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}
	return u.String()
}

// enableWAL switches the file to WAL mode, which persists in the file. When
// several processes create the database at once their switches race, and
// SQLite reports the loser as SQLITE_BUSY at once, without consulting
// busy_timeout (the pragma upgrades a read lock to a write lock). So the
// switch is retried here until busyTimeout has passed.
func (s *Store) enableWAL(ctx context.Context) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		var mode string
		err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode)
		if err == nil && mode != "wal" {
			return fmt.Errorf("journal_mode is %q, want wal", mode)
		}
		if !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isBusy reports whether err is SQLITE_BUSY or one of its extended codes.
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// Close closes the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate applies every embedded migration not yet recorded in
// schema_migrations, in filename order. The common case (nothing pending)
// only reads, so opening a store never takes the write lock needlessly.
func (s *Store) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	todo, err := pending(ctx, s.db, names)
	if err != nil || len(todo) == 0 {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Another process may have migrated between the check and the lock.
	if todo, err = pending(ctx, tx, names); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	for _, name := range todo {
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("%s: %w", path.Base(name), err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			path.Base(name), formatTime(time.Now())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// dbtx is satisfied by both *sql.DB and *sql.Tx.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// pending returns the migration paths in names whose base name is not
// recorded in schema_migrations.
func pending(ctx context.Context, q dbtx, names []string) ([]string, error) {
	var exists bool
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations')`).
		Scan(&exists)
	if err != nil || !exists {
		return names, err
	}
	rows, err := q.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	applied := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var todo []string
	for _, name := range names {
		if !applied[path.Base(name)] {
			todo = append(todo, name)
		}
	}
	return todo, nil
}

// UpsertSession inserts s or overwrites every stored field of the row with
// the same ID, in one statement. It writes the caller's snapshot blindly:
// to change a session based on its stored value, use UpdateSession.
// An empty Pane is stored as NULL, so pane-scoped updates never match it.
func (s *Store) UpsertSession(ctx context.Context, sess model.Session) error {
	if err := upsert(ctx, s.db, sess); err != nil {
		return fmt.Errorf("store: upsert session %s: %w", sess.ID, err)
	}
	return nil
}

// UpdateSession changes one session based on its stored value, without
// losing concurrent updates. In one write transaction that takes the write
// lock before reading (BEGIN IMMEDIATE), it loads the session id (the zero
// Session and found=false if there is none), calls fn, and upserts what fn
// returns. Concurrent callers in any process therefore run one at a time,
// and each fn sees what the previous caller committed. A caller that waits
// longer than busy_timeout for the lock gets an error and fn is not called.
//
// fn must not call the Store: the transaction holds the write lock, so a
// write from fn would wait on it until busy_timeout. If fn returns a Session
// whose ID is not id, nothing is written and an error is returned.
func (s *Store) UpdateSession(ctx context.Context, id string, fn func(s model.Session, found bool) model.Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: update session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, found, err := getSession(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("store: update session %s: %w", id, err)
	}
	next := fn(cur, found)
	if next.ID != id {
		return fmt.Errorf("store: update session %s: fn returned a session with ID %q", id, next.ID)
	}
	if err = upsert(ctx, tx, next); err != nil {
		return fmt.Errorf("store: update session %s: %w", id, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: update session %s: commit: %w", id, err)
	}
	return nil
}

// upsert is the single INSERT ... ON CONFLICT statement behind UpsertSession
// and UpdateSession.
func upsert(ctx context.Context, e dbtx, sess model.Session) error {
	_, err := e.ExecContext(ctx, `
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
  transcript_offset = excluded.transcript_offset`,
		sess.ID, sess.Agent, sess.Pane, sess.TmuxSession, sess.TmuxWindow, sess.Cwd, sess.Repo, sess.Branch,
		sess.State, sess.Model, sess.ContextUsed, sess.ContextMax, sess.SubagentsActive, sess.LastUpdate,
		formatTime(sess.SeenAt), sess.TranscriptPath, formatTime(sess.StartedAt), formatTime(sess.UpdatedAt),
		sess.TranscriptOffset)
	return err
}

// selectSessions reads every column Session maps to. Each nullable column is
// COALESCEd (times are scanned as NullString), so a row written by something
// other than Store with NULLs in it cannot break a whole list.
const selectSessions = `
SELECT COALESCE(id, ''), COALESCE(agent, ''), COALESCE(tmux_pane, ''), COALESCE(tmux_session, ''),
  COALESCE(tmux_window, ''), COALESCE(cwd, ''), COALESCE(repo, ''), COALESCE(branch, ''),
  COALESCE(state, ''), COALESCE(model, ''), COALESCE(context_used, 0), COALESCE(context_max, 0),
  COALESCE(subagents_active, 0), COALESCE(last_update, ''), seen_at, COALESCE(transcript_path, ''),
  started_at, updated_at, transcript_offset
FROM sessions`

// GetSession returns the session with the given id, and false if there is none.
func (s *Store) GetSession(ctx context.Context, id string) (model.Session, bool, error) {
	sess, found, err := getSession(ctx, s.db, id)
	if err != nil {
		return model.Session{}, false, fmt.Errorf("store: get session %s: %w", id, err)
	}
	return sess, found, nil
}

func getSession(ctx context.Context, q dbtx, id string) (model.Session, bool, error) {
	sess, err := scanSession(q.QueryRowContext(ctx, selectSessions+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Session{}, false, nil
	}
	if err != nil {
		return model.Session{}, false, err
	}
	return sess, true, nil
}

// ListSessions returns the sessions f selects, ordered by ID.
func (s *Store) ListSessions(ctx context.Context, f ListFilter) ([]model.Session, error) {
	rows, err := s.db.QueryContext(ctx, selectSessions+`
WHERE (? OR state != ?)
  AND NOT (? AND state = ? AND (updated_at IS NULL OR updated_at < ?))
ORDER BY id`,
		f.IncludeEnded, model.StateEnded,
		!f.HideStaleBefore.IsZero(), model.StateStale, formatTime(f.HideStaleBefore))
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list sessions: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	return out, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanSession(sc scanner) (model.Session, error) {
	var sess model.Session
	var seen, started, updated sql.NullString
	err := sc.Scan(&sess.ID, &sess.Agent, &sess.Pane, &sess.TmuxSession, &sess.TmuxWindow, &sess.Cwd,
		&sess.Repo, &sess.Branch, &sess.State, &sess.Model, &sess.ContextUsed, &sess.ContextMax,
		&sess.SubagentsActive, &sess.LastUpdate, &seen, &sess.TranscriptPath, &started, &updated,
		&sess.TranscriptOffset)
	if err != nil {
		return model.Session{}, err
	}
	if sess.SeenAt, err = parseTime(seen); err != nil {
		return model.Session{}, err
	}
	if sess.StartedAt, err = parseTime(started); err != nil {
		return model.Session{}, err
	}
	if sess.UpdatedAt, err = parseTime(updated); err != nil {
		return model.Session{}, err
	}
	return sess, nil
}

// AppendEvent records one hook event with its raw payload.
func (s *Store) AppendEvent(ctx context.Context, ev model.Event, raw []byte) error {
	var payload any
	if len(raw) > 0 {
		payload = string(raw) // TEXT, so SQLite's JSON functions read it
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (session_id, kind, payload, at) VALUES (?, ?, ?, ?)`,
		ev.SessionID, ev.Kind, payload, formatTime(ev.At))
	if err != nil {
		return fmt.Errorf("store: append %s event for %s: %w", ev.Kind, ev.SessionID, err)
	}
	return nil
}

// MarkSeen sets SeenAt to at on every non-ended session in pane.
func (s *Store) MarkSeen(ctx context.Context, pane string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET seen_at = ? WHERE tmux_pane = ? AND state != ?`,
		formatTime(at), pane, model.StateEnded)
	if err != nil {
		return fmt.Errorf("store: mark pane %s seen: %w", pane, err)
	}
	return nil
}

// EndOthersInPane sets every non-ended session in pane other than keepID to
// ended, with UpdatedAt at.
func (s *Store) EndOthersInPane(ctx context.Context, pane, keepID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET state = ?, updated_at = ? WHERE tmux_pane = ? AND id != ? AND state != ?`,
		model.StateEnded, formatTime(at), pane, keepID, model.StateEnded)
	if err != nil {
		return fmt.Errorf("store: end sessions in pane %s other than %s: %w", pane, keepID, err)
	}
	return nil
}

// timeLayout is UTC RFC 3339 with exactly nine fractional digits. The fixed
// width makes stored text sort like the times it encodes, which SQL
// comparisons rely on; RFC3339Nano trims trailing zeros and would not.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// formatTime renders t in timeLayout, and the zero time as NULL.
func formatTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

// parseTime reverses formatTime.
func parseTime(v sql.NullString) (time.Time, error) {
	if !v.Valid {
		return time.Time{}, nil
	}
	return time.Parse(timeLayout, v.String)
}

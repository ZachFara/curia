package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// F29 auto-spawn (migration 0020).

// AutoLimits are the caps AutoClaim enforces inside its one statement.
type AutoLimits struct {
	MaxPerDay int
	Interval  time.Duration
}

// MarkReady stores the hash of task id's title and body as of now. Only the
// user-level paths call it (curia task ready, the board key, and a task
// filed ready by the user's own level = ready); a task that is not stored
// ready is refused.
func (s *Store) MarkReady(ctx context.Context, id string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return fail("mark ready", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	t, err := loadTask(ctx, tx, id)
	if err == nil && t.Status != model.TaskReady {
		err = fmt.Errorf("%s is %s, not ready", id, t.Status)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET ready_hash = ? WHERE n = ?`, model.ReadyHash(t.Title, t.Body), t.N)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return failTask("mark ready", id, err)
	}
	return nil
}

// AutoClaim is the one UPDATE that lets the monitor start task t, as the
// caller's eligibility check read it: it needs the stored hash and the text
// (an edit keeps a stale hash, so the hash alone does not see one) to be
// unchanged since, no
// auto-spawn or failed attempt within the interval, and fewer than
// MaxPerDay in 24 hours (failed spawn attempts count, so a task that keeps
// failing is not retried without end). It sets auto_spawned_at and clears the hash (the
// readiness is consumed), so a second claimer, or a restart, finds nothing
// to claim. ok is false when it lost.
func (s *Store) AutoClaim(ctx context.Context, t model.Task, now time.Time, lim AutoLimits) (ok bool, err error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE tasks SET auto_spawned_at = ?1, ready_hash = NULL
WHERE id = ?2 AND status = 'ready' AND ready_hash = ?3 AND title = ?7 AND body = ?8
  AND NOT EXISTS (SELECT 1 FROM tasks WHERE auto_spawned_at > ?4)
  AND NOT EXISTS (SELECT 1 FROM auto_spawn_runs WHERE at > ?4 AND check_ = 'spawn_failed')
  AND (SELECT count(*) FROM tasks WHERE auto_spawned_at > ?5)
    + (SELECT count(*) FROM auto_spawn_runs WHERE at > ?5 AND check_ = 'spawn_failed') < ?6`,
		formatTime(now), t.ID, t.ReadyHash, formatTime(now.Add(-lim.Interval)), formatTime(now.Add(-24*time.Hour)), lim.MaxPerDay, t.Title, t.Body)
	if err != nil {
		return false, fail("auto claim", t.ID, err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// AutoRelease undoes AutoClaim's claim made at claimedAt after a failed
// spawn: the hash and the previous auto_spawned_at come back, unless
// something else changed the task since (it left ready: no hash returns).
func (s *Store) AutoRelease(ctx context.Context, id string, claimedAt, prevAt time.Time, hash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET auto_spawned_at = ?, ready_hash = ? WHERE id = ? AND auto_spawned_at = ? AND ready_hash IS NULL AND status = 'ready'`,
		formatTime(prevAt), hash, id, formatTime(claimedAt))
	if err != nil {
		return fail("auto release", id, err)
	}
	return nil
}

// AutoRun is one row of the auto log: a start (fired) or a refusal.
type AutoRun struct {
	At                                        time.Time
	Outcome, Check, TaskID, SessionID, Detail string
}

// LogAuto adds r, except a refusal equal (outcome, check, task) to the
// newest row, which is not repeated; written says whether a row was added.
func (s *Store) LogAuto(ctx context.Context, r AutoRun) (written bool, err error) {
	if r.Outcome == "refused" {
		var o, c, tk string
		err = s.db.QueryRowContext(ctx, `SELECT outcome, check_, task_id FROM auto_spawn_runs ORDER BY id DESC LIMIT 1`).Scan(&o, &c, &tk)
		if err == nil && o == r.Outcome && c == r.Check && tk == r.TaskID && r.Check != "spawn_failed" { // a retry gate reads the failure time
			return false, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fail("log auto", "", err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO auto_spawn_runs (at, outcome, check_, task_id, session_id, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		formatTime(r.At), r.Outcome, r.Check, r.TaskID, r.SessionID, r.Detail); err != nil {
		return false, fail("log auto", "", err)
	}
	return true, nil
}

// AutoRuns returns the newest n rows, newest first.
func (s *Store) AutoRuns(ctx context.Context, n int) ([]AutoRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, outcome, check_, task_id, session_id, detail FROM auto_spawn_runs ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fail("auto runs", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AutoRun
	for rows.Next() {
		var r AutoRun
		var at sql.NullString
		if err = rows.Scan(&at, &r.Outcome, &r.Check, &r.TaskID, &r.SessionID, &r.Detail); err != nil {
			return nil, fail("auto runs", "", err)
		}
		if r.At, err = parseTime(at); err != nil {
			return nil, fail("auto runs", "", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AutoStats is the number of auto-spawns and failed attempts in the 24 hours before now and the
// time of the newest start or failed attempt (zero: none).
func (s *Store) AutoStats(ctx context.Context, now time.Time) (today int, last time.Time, err error) {
	var l sql.NullString
	err = s.db.QueryRowContext(ctx, `
SELECT (SELECT count(*) FROM tasks WHERE auto_spawned_at > ?1) + (SELECT count(*) FROM auto_spawn_runs WHERE at > ?1 AND check_ = 'spawn_failed'),
  (SELECT max(a) FROM (SELECT max(auto_spawned_at) AS a FROM tasks UNION ALL SELECT max(at) FROM auto_spawn_runs WHERE check_ = 'spawn_failed'))`,
		formatTime(now.Add(-24*time.Hour))).Scan(&today, &l)
	if err == nil {
		last, err = parseTime(l)
	}
	if err != nil {
		return 0, time.Time{}, fail("auto stats", "", err)
	}
	return today, last, nil
}

// BypassActive reports whether any live session is in bypass mode at now.
func (s *Store) BypassActive(ctx context.Context, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE mode = ? AND state NOT IN (?, ?) AND (mode_until IS NULL OR mode_until > ?)`,
		model.ModeBypass, model.StateEnded, model.StateStale, formatTime(now)).Scan(&n)
	if err != nil {
		return false, fail("bypass active", "", err)
	}
	return n > 0, nil
}

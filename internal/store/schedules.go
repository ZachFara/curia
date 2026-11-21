package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// F22 scheduled messages (migration 0017).

// Errors of the schedule store.
var (
	ErrTooManySchedules = errors.New("too many active schedules")
	// ErrScheduleState is UpdateScheduleStatus finding no schedule in a state it may leave.
	ErrScheduleState = errors.New("the schedule does not exist or is not in a state that allows this")
)

// Schedule is one row of schedules. Kind is at, daily or on_reset; Status is
// active, paused, done or canceled; Target is all, repo:<name> or task:<id>.
type Schedule struct {
	ID, Text, Kind, Spec, Target string
	IfIdle, OnlyLimited          bool
	Status                       string
	NextRun                      time.Time // zero once done or canceled
	CreatedVia                   string
	CreatedAt                    time.Time
	LastOutcome                  string // the outcome of its latest run, "" before one (ListSchedules, GetSchedule)
}

// ScheduleRun is one fire (or skipped fire) of a schedule.
type ScheduleRun struct {
	ID               int64
	ScheduleID       string
	DueAt, RanAt     time.Time
	Outcome, Results string
}

const scheduleColumns = ` id, text, kind, spec, target, if_idle, only_limited, status, next_run_at, created_via, created_at,
  COALESCE((SELECT outcome FROM schedule_runs r WHERE r.schedule_id = schedules.id ORDER BY r.id DESC LIMIT 1), '') `

func scanSchedule(row interface{ Scan(...any) error }) (Schedule, error) {
	var sc Schedule
	var next, created sql.NullString
	if err := row.Scan(&sc.ID, &sc.Text, &sc.Kind, &sc.Spec, &sc.Target, &sc.IfIdle, &sc.OnlyLimited, &sc.Status, &next,
		&sc.CreatedVia, &created, &sc.LastOutcome); err != nil {
		return Schedule{}, err
	}
	var err error
	if sc.NextRun, err = parseTime(next); err != nil {
		return Schedule{}, err
	}
	sc.CreatedAt, err = parseTime(created)
	return sc, err
}

// CreateSchedule stores sc as the next S-<n>, active, unless maxActive
// schedules are already active or paused (ErrTooManySchedules).
func (s *Store) CreateSchedule(ctx context.Context, sc Schedule, maxActive int) (Schedule, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Schedule{}, failIf("create schedule", "", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schedules WHERE status IN ('active','paused')`).Scan(&n); err != nil {
		return Schedule{}, failIf("create schedule", "", err)
	}
	if n >= maxActive {
		return Schedule{}, fmt.Errorf("%w: %d already (cancel one first)", ErrTooManySchedules, n)
	}
	// Rows are never deleted and the write lock is held, so an id is never reused.
	out, err := scanSchedule(tx.QueryRowContext(ctx, `
INSERT INTO schedules (id, text, kind, spec, target, if_idle, only_limited, status, next_run_at, created_via, created_at)
VALUES ('S-' || (SELECT COALESCE(MAX(CAST(substr(id, 3) AS INTEGER)), 0) + 1 FROM schedules), ?, ?, ?, ?, ?, ?, 'active', ?, COALESCE(NULLIF(?, ''), 'cli'), ?)
RETURNING`+scheduleColumns, sc.Text, sc.Kind, sc.Spec, sc.Target, sc.IfIdle, sc.OnlyLimited, formatTime(sc.NextRun), sc.CreatedVia, formatTime(sc.CreatedAt)))
	if err != nil {
		return Schedule{}, failIf("create schedule", "", err)
	}
	if err = tx.Commit(); err != nil {
		return Schedule{}, failIf("create schedule", "", err)
	}
	return out, nil
}

// GetSchedule returns schedule id, and false if there is none.
func (s *Store) GetSchedule(ctx context.Context, id string) (Schedule, bool, error) {
	sc, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT`+scheduleColumns+`FROM schedules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, false, nil
	}
	if err != nil {
		return Schedule{}, false, failIf("get schedule", id, err)
	}
	return sc, true, nil
}

// ListSchedules returns up to limit schedules, newest first.
func (s *Store) ListSchedules(ctx context.Context, limit int) ([]Schedule, error) {
	return s.schedules(ctx, "list schedules", `SELECT`+scheduleColumns+`FROM schedules ORDER BY CAST(substr(id, 3) AS INTEGER) DESC LIMIT ?`, limit)
}

// DueSchedules returns the active schedules whose next run is at or before now, oldest first.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	return s.schedules(ctx, "due schedules", `SELECT`+scheduleColumns+`FROM schedules
WHERE status = 'active' AND next_run_at <= ? ORDER BY next_run_at, id LIMIT ?`, formatTime(now), 200)
}

func (s *Store) schedules(ctx context.Context, op, query string, args ...any) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, failIf(op, "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Schedule
	for rows.Next() {
		sc, serr := scanSchedule(rows)
		if serr != nil {
			return nil, failIf(op, "", serr)
		}
		out = append(out, sc)
	}
	return out, failIf(op, "", rows.Err())
}

// UpdateScheduleStatus moves schedule id from one of the states in from to
// to, with next as its next run (zero: none). It returns ErrScheduleState
// when the schedule is not in one of them.
func (s *Store) UpdateScheduleStatus(ctx context.Context, id string, from []string, to string, next time.Time) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return failIf("update schedule", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var cur string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM schedules WHERE id = ?`, id).Scan(&cur); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return failIf("update schedule", id, err)
	}
	ok := false
	for _, f := range from {
		ok = ok || f == cur
	}
	if !ok {
		return ErrScheduleState
	}
	if _, err = tx.ExecContext(ctx, `UPDATE schedules SET status = ?, next_run_at = ? WHERE id = ?`, to, formatTime(next), id); err != nil {
		return failIf("update schedule", id, err)
	}
	return failIf("update schedule", id, tx.Commit())
}

// ClaimSchedule is the one step that lets a due schedule fire: it succeeds
// for exactly one caller, whoever moves the row from the next run sc still
// shows to next (zero: the schedule is done), and then records the run as
// outcome in the same write. false means another claimer, a cancel or a pause
// got there first; nothing is recorded. The run's results are added by
// FinishScheduleRun.
func (s *Store) ClaimSchedule(ctx context.Context, sc Schedule, next time.Time, outcome string, now time.Time) (int64, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, false, failIf("claim schedule", sc.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	status := "active"
	if next.IsZero() {
		status = "done"
	}
	res, err := tx.ExecContext(ctx, `UPDATE schedules SET status = ?, next_run_at = ? WHERE id = ? AND status = 'active' AND next_run_at = ?`,
		status, formatTime(next), sc.ID, formatTime(sc.NextRun))
	if err != nil {
		return 0, false, failIf("claim schedule", sc.ID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, false, nil
	}
	var id int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO schedule_runs (schedule_id, due_at, ran_at, outcome) VALUES (?, ?, ?, ?) RETURNING id`,
		sc.ID, formatTime(sc.NextRun), formatTime(now), outcome).Scan(&id); err != nil {
		return 0, false, failIf("claim schedule", sc.ID, err)
	}
	if err = tx.Commit(); err != nil {
		return 0, false, failIf("claim schedule", sc.ID, err)
	}
	return id, true, nil
}

// FinishScheduleRun sets the outcome and the results (JSON) of run id.
func (s *Store) FinishScheduleRun(ctx context.Context, id int64, outcome, results string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedule_runs SET outcome = ?, results = ? WHERE id = ?`, outcome, results, id)
	return failIf("finish schedule run", "", err)
}

// InterruptScheduleRuns marks every run recorded fired before cutoff whose
// results were never written (a crash or a store error after the claim) as
// interrupted: its message was not sent, and it is not fired again.
func (s *Store) InterruptScheduleRuns(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedule_runs SET outcome = 'interrupted', results = '{"note":"claimed but never finished; nothing was sent"}'
WHERE outcome = 'fired' AND results IN ('', '{}') AND ran_at < ?`, formatTime(cutoff))
	return failIf("interrupt schedule runs", "", err)
}

// ScheduleRuns returns up to limit runs of schedule id, newest first.
func (s *Store) ScheduleRuns(ctx context.Context, id string, limit int) ([]ScheduleRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, schedule_id, due_at, ran_at, outcome, results FROM schedule_runs
WHERE schedule_id = ? ORDER BY id DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, failIf("schedule runs", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ScheduleRun
	for rows.Next() {
		var r ScheduleRun
		var due, ran sql.NullString
		if err = rows.Scan(&r.ID, &r.ScheduleID, &due, &ran, &r.Outcome, &r.Results); err != nil {
			return nil, failIf("schedule runs", id, err)
		}
		if r.DueAt, err = parseTime(due); err == nil {
			r.RanAt, err = parseTime(ran)
		}
		if err != nil {
			return nil, failIf("schedule runs", id, err)
		}
		out = append(out, r)
	}
	return out, failIf("schedule runs", id, rows.Err())
}

// LatestUsage returns each window's newest usage sample (F22's --only limited
// and --on reset read them).
func (s *Store) LatestUsage(ctx context.Context) ([]model.UsageSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n, at, window, used_pct, resets_at FROM usage_samples
WHERE n IN (SELECT MAX(n) FROM usage_samples GROUP BY window)`)
	if err != nil {
		return nil, failIf("latest usage", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.UsageSample
	for rows.Next() {
		var u model.UsageSample
		var at, resets sql.NullString
		if err = rows.Scan(&u.N, &at, &u.Window, &u.UsedPct, &resets); err != nil {
			return nil, failIf("latest usage", "", err)
		}
		if u.At, err = parseTime(at); err == nil {
			u.ResetsAt, err = parseTime(resets)
		}
		if err != nil {
			return nil, failIf("latest usage", "", err)
		}
		out = append(out, u)
	}
	return out, failIf("latest usage", "", rows.Err())
}

// failIf is fail for an error that may be nil.
func failIf(op, id string, err error) error {
	if err == nil {
		return nil
	}
	return fail(op, id, err)
}

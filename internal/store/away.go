package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// ErrAwayOpen is StartAway's refusal: a period is already running.
var ErrAwayOpen = errors.New("store: an away period is already running")

// SavedMode is a session's mode row as it was before an away period.
type SavedMode struct {
	Mode  string    `json:"mode"`
	Until time.Time `json:"until"`
}

// AwayRun is one away period (F20).
type AwayRun struct {
	ID                                  int64
	StartedAt, ReadyAt, EndsAt, EndedAt time.Time
	ShownAt                             time.Time
	Scope                               string // "" everyone, else a repository
	Saved                               map[string]SavedMode
	NotReady                            []string
	Summary                             string
}

const awayCols = `id, started_at, ready_at, ends_at, ended_at, shown_at, scope, saved_modes, not_ready, summary`

func scanAway(sc scanner) (AwayRun, error) {
	var r AwayRun
	var started, ready, ends, ended, shown, saved, notReady, summary sql.NullString
	if err := sc.Scan(&r.ID, &started, &ready, &ends, &ended, &shown, &r.Scope, &saved, &notReady, &summary); err != nil {
		return AwayRun{}, err
	}
	var err error
	for _, f := range []struct {
		dst *time.Time
		src sql.NullString
	}{{&r.StartedAt, started}, {&r.ReadyAt, ready}, {&r.EndsAt, ends}, {&r.EndedAt, ended}, {&r.ShownAt, shown}} {
		if *f.dst, err = parseTime(f.src); err != nil {
			return AwayRun{}, err
		}
	}
	r.Summary = summary.String
	if saved.Valid {
		if err = json.Unmarshal([]byte(saved.String), &r.Saved); err != nil {
			return AwayRun{}, err
		}
	}
	if notReady.Valid {
		if err = json.Unmarshal([]byte(notReady.String), &r.NotReady); err != nil {
			return AwayRun{}, err
		}
	}
	return r, nil
}

func (s *Store) awayWhere(ctx context.Context, where string, args ...any) (AwayRun, bool, error) {
	r, err := scanAway(s.db.QueryRowContext(ctx, `SELECT `+awayCols+` FROM away_runs WHERE `+where+` ORDER BY id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return AwayRun{}, false, nil
	}
	if err != nil {
		return AwayRun{}, false, fmt.Errorf("store: away run: %w", err)
	}
	return r, true, nil
}

// OpenAway is the running away period, if any.
func (s *Store) OpenAway(ctx context.Context) (AwayRun, bool, error) {
	return s.awayWhere(ctx, `ended_at IS NULL`)
}

// UnshownAway is the newest ended period whose summary the user has not seen.
func (s *Store) UnshownAway(ctx context.Context) (AwayRun, bool, error) {
	return s.awayWhere(ctx, `ended_at IS NOT NULL AND shown_at IS NULL`)
}

// StartAway, in one transaction, refuses when a period is open, saves the mode
// of every live session in scope, puts them all in away until ends and
// records the period. A session that starts later joins through InheritAway.
func (s *Store) StartAway(ctx context.Context, scope ModeScope, now, ends time.Time, notReady []string) (AwayRun, error) {
	if !scope.All && scope.Repo == "" {
		return AwayRun{}, errors.New("store: start away: no scope")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return AwayRun{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var open int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM away_runs WHERE ended_at IS NULL`).Scan(&open); err != nil {
		return AwayRun{}, fmt.Errorf("store: start away: %w", err)
	}
	if open > 0 {
		return AwayRun{}, ErrAwayOpen
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, COALESCE(mode, ''), mode_until FROM sessions WHERE state != ? AND (? OR repo = ?)`,
		model.StateEnded, scope.All, scope.Repo)
	if err != nil {
		return AwayRun{}, fmt.Errorf("store: start away: %w", err)
	}
	saved := map[string]SavedMode{}
	for rows.Next() {
		var id, mode string
		var until sql.NullString
		if err = rows.Scan(&id, &mode, &until); err != nil {
			_ = rows.Close()
			return AwayRun{}, fmt.Errorf("store: start away: %w", err)
		}
		if mode == "" {
			continue
		}
		u, perr := parseTime(until)
		if perr != nil {
			_ = rows.Close()
			return AwayRun{}, fmt.Errorf("store: start away: %w", perr)
		}
		saved[id] = SavedMode{Mode: mode, Until: u}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return AwayRun{}, fmt.Errorf("store: start away: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET mode = ?, mode_until = ? WHERE state != ? AND (? OR repo = ?)`,
		model.ModeAway, formatTime(ends), model.StateEnded, scope.All, scope.Repo); err != nil {
		return AwayRun{}, fmt.Errorf("store: start away: %w", err)
	}
	sj, _ := json.Marshal(saved)
	nj, _ := json.Marshal(notReady)
	repo := scope.Repo
	if scope.All {
		repo = ""
	}
	var id int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO away_runs (started_at, ready_at, ends_at, scope, saved_modes, not_ready) VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		formatTime(now), formatTime(now), formatTime(ends), repo, string(sj), string(nj)).Scan(&id); err != nil {
		return AwayRun{}, fmt.Errorf("store: start away: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return AwayRun{}, fmt.Errorf("store: start away: %w", err)
	}
	r, _, err := s.awayWhere(ctx, `id = ?`, id)
	return r, err
}

// EndAway ends run id once (the claim is one UPDATE: of two callers only one
// gets true) and restores Curia's own mode rows: a session still in away goes
// back to the mode it had, or the default when it started during the period,
// when its saved mode was bypass, or when that had lapsed. A session whose
// mode the user changed meanwhile keeps the user's choice.
func (s *Store) EndAway(ctx context.Context, id int64, now time.Time) (AwayRun, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return AwayRun{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE away_runs SET ended_at = ? WHERE id = ? AND ended_at IS NULL`, formatTime(now), id)
	if err != nil {
		return AwayRun{}, false, fmt.Errorf("store: end away: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return AwayRun{}, false, nil
	}
	r, err := scanAway(tx.QueryRowContext(ctx, `SELECT `+awayCols+` FROM away_runs WHERE id = ?`, id))
	if err != nil {
		return AwayRun{}, false, fmt.Errorf("store: end away: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE mode = ? AND state != ? AND (? = '' OR repo = ?)`,
		model.ModeAway, model.StateEnded, r.Scope, r.Scope)
	if err != nil {
		return AwayRun{}, false, fmt.Errorf("store: end away: %w", err)
	}
	var ids []string
	for rows.Next() {
		var sid string
		if err = rows.Scan(&sid); err != nil {
			_ = rows.Close()
			return AwayRun{}, false, fmt.Errorf("store: end away: %w", err)
		}
		ids = append(ids, sid)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return AwayRun{}, false, fmt.Errorf("store: end away: %w", err)
	}
	for _, sid := range ids {
		var mode, until any // NULL: the default
		if sv, ok := r.Saved[sid]; ok && sv.Mode != model.ModeBypass && (sv.Until.IsZero() || now.Before(sv.Until)) {
			mode, until = sv.Mode, formatTime(sv.Until)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET mode = ?, mode_until = ? WHERE id = ?`, mode, until, sid); err != nil {
			return AwayRun{}, false, fmt.Errorf("store: end away: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return AwayRun{}, false, fmt.Errorf("store: end away: %w", err)
	}
	r.EndedAt = now
	return r, true, nil
}

// SetAwaySummary stores run id's summary text.
func (s *Store) SetAwaySummary(ctx context.Context, id int64, text string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE away_runs SET summary = ? WHERE id = ?`, text, id); err != nil {
		return fmt.Errorf("store: away summary: %w", err)
	}
	return nil
}

// MarkAwayShown records that the user saw run id's summary.
func (s *Store) MarkAwayShown(ctx context.Context, id int64, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE away_runs SET shown_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
		return fmt.Errorf("store: away shown: %w", err)
	}
	return nil
}

// InheritAway puts a session that started during an away period in away until
// the period's end, unless it already has a mode. One statement; called once
// per session start, never per tool call.
func (s *Store) InheritAway(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE sessions SET mode = ?, mode_until = (SELECT ends_at FROM away_runs WHERE ended_at IS NULL ORDER BY id DESC LIMIT 1)
WHERE id = ? AND mode IS NULL AND state != ?
  AND EXISTS (SELECT 1 FROM away_runs r WHERE r.ended_at IS NULL AND (r.scope = '' OR r.scope = sessions.repo))`,
		model.ModeAway, sessionID, model.StateEnded)
	if err != nil {
		return fmt.Errorf("store: inherit away: %w", err)
	}
	return nil
}

// SessionAway reports whether the session is in away now.
func (s *Store) SessionAway(ctx context.Context, sessionID string, now time.Time) (bool, error) {
	var mode, until sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT mode, mode_until FROM sessions WHERE id = ?`, sessionID).Scan(&mode, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: session away: %w", err)
	}
	u, err := parseTime(until)
	if err != nil {
		return false, fmt.Errorf("store: session away: %w", err)
	}
	return mode.String == model.ModeAway && (u.IsZero() || now.Before(u)), nil
}

// Denial is one group of refused tool calls.
type Denial struct {
	SessionID, Tool, Summary string
	Count                    int
}

// maxDenialGroups and maxDenialSummary bound what a summary reads of
// permission_events, whatever was stored there.
const (
	maxDenialGroups  = 30
	maxDenialSummary = 200
)

// Denials groups the denied calls in [from, to] by session, tool and command
// line, most frequent first, and returns how many there were in all.
func (s *Store) Denials(ctx context.Context, from, to time.Time) (groups []Denial, total int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, tool, substr(summary, 1, ?), COUNT(*) AS c FROM permission_events
WHERE verdict = 'deny' AND at >= ? AND at <= ? GROUP BY session_id, tool, substr(summary, 1, ?) ORDER BY c DESC, MIN(id) LIMIT ?`,
		maxDenialSummary, formatTime(from), formatTime(to), maxDenialSummary, maxDenialGroups)
	if err != nil {
		return nil, 0, fmt.Errorf("store: denials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d Denial
		if err = rows.Scan(&d.SessionID, &d.Tool, &d.Summary, &d.Count); err != nil {
			return nil, 0, fmt.Errorf("store: denials: %w", err)
		}
		groups = append(groups, d)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: denials: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM permission_events WHERE verdict = 'deny' AND at >= ? AND at <= ?`,
		formatTime(from), formatTime(to)).Scan(&total)
	return groups, total, err
}

// AwayWaiting is what waits for the user at the end of a period.
type AwayWaiting struct {
	Blocking, Assumed, Coerced int     // open tickets raised in the window (Coerced counts within Assumed)
	Reviews, Messages          int     // reviews pending, messages queued or held, now
	CostUSD                    float64 // growth of the sessions' running cost totals over the window
	FivePct, SevenPct          float64 // the latest usage reading, "" windows are 0; Has says whether there is one
	HasUsage                   bool
}

// Waiting counts, for the window [from, to], what the summary names besides denials.
func (s *Store) Waiting(ctx context.Context, from, to time.Time) (AwayWaiting, error) {
	var w AwayWaiting
	f, t := formatTime(from), formatTime(to)
	q := func(dst any, query string, args ...any) error {
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(dst); err != nil {
			return fmt.Errorf("store: away waiting: %w", err)
		}
		return nil
	}
	if err := errors.Join(
		q(&w.Blocking, `SELECT COUNT(*) FROM decisions WHERE status = 'open' AND mode = 'blocking' AND created_at >= ? AND created_at <= ?`, f, t),
		q(&w.Assumed, `SELECT COUNT(*) FROM decisions WHERE status = 'open' AND mode = 'assume' AND created_at >= ? AND created_at <= ?`, f, t),
		q(&w.Coerced, `SELECT COUNT(*) FROM decisions WHERE status = 'open' AND coerced_from IS NOT NULL AND created_at >= ? AND created_at <= ?`, f, t),
		q(&w.Reviews, `SELECT COUNT(*) FROM reviews WHERE status = 'pending'`),
		q(&w.Messages, `SELECT COUNT(*) FROM messages WHERE status IN ('queued','held')`),
		q(&w.CostUSD, `SELECT COALESCE(SUM(hi - lo), 0) FROM (
SELECT MAX(CASE WHEN at <= ?2 THEN cost_usd END) AS hi, COALESCE(MAX(CASE WHEN at < ?1 THEN cost_usd END), 0) AS lo FROM cost_samples GROUP BY session_id) WHERE hi IS NOT NULL AND hi > lo`, f, t),
	); err != nil {
		return AwayWaiting{}, err
	}
	for _, p := range []struct {
		win string
		dst *float64
	}{{"five_hour", &w.FivePct}, {"seven_day", &w.SevenPct}} {
		var pct sql.NullFloat64
		if err := s.db.QueryRowContext(ctx, `SELECT used_pct FROM usage_samples WHERE window = ? AND at <= ? ORDER BY at DESC LIMIT 1`, p.win, t).Scan(&pct); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AwayWaiting{}, fmt.Errorf("store: away waiting: %w", err)
		}
		if pct.Valid {
			*p.dst, w.HasUsage = pct.Float64, true
		}
	}
	return w, nil
}

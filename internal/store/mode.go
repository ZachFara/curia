package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// ModeScope picks the sessions SetModes changes: every live one, or those of
// one repository (the main checkout's path, as sessions.repo holds it).
type ModeScope struct {
	All  bool
	Repo string
}

// modeRootSQL is the one rule for the folder a mode freezes: the session's worktree, else its repository, else NULL.
const modeRootSQL = `COALESCE(NULLIF(worktree, ''), NULLIF(repo, ''))`

// SetModes sets the mode of every session in scope that has not ended and
// returns how many it changed. until is when the mode lapses (zero: never).
// It also freezes sessions.mode_root from the worktree, else the repository, as
// they are now (NULL when neither is known). It is the only writer of both, so a session update by the hook
// cannot change a mode. mode must be one of model.Modes.
func (s *Store) SetModes(ctx context.Context, scope ModeScope, mode string, until time.Time) (int, error) {
	if !slices.Contains(model.Modes, mode) {
		return 0, fmt.Errorf("store: unknown mode %q", mode)
	}
	if !scope.All && scope.Repo == "" {
		return 0, fmt.Errorf("store: set modes: no scope")
	}
	var u any
	if !until.IsZero() {
		u = formatTime(until)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET mode = ?, mode_until = ?, mode_root = `+modeRootSQL+` WHERE state != ? AND (? OR repo = ?)`,
		mode, u, model.StateEnded, scope.All, scope.Repo)
	if err != nil {
		return 0, fmt.Errorf("store: set modes: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PermissionEvent is one thing the hook denied under a mode.
type PermissionEvent struct {
	SessionID, Mode, Tool, Summary, Verdict string
	At                                      time.Time
	N                                       int // how many times it was refused (PermissionEvents only)
}

// maxPermissionSummary bounds a stored summary, in bytes.
const maxPermissionSummary = 200

// PermissionDistinctCap bounds the distinct refusals one session keeps in the
// open away period. Past it, further refusals of other commands are counted in
// one overflow row (verdict permissionOverflow) and the summary says how many.
const PermissionDistinctCap = 500

const permissionOverflow = "overflow"

// AddPermissionEvent records a refusal in one statement: a repeat of a row
// of the same session, away period, tool and summary only adds to its count
// (so an agent's repeats never grow the table or push out a later refusal);
// a new one is a row unless the session already has PermissionDistinctCap in
// the period, then it adds to the session's overflow row. The count reads at
// most the cap. The summary is cut to maxPermissionSummary.
func (s *Store) AddPermissionEvent(ctx context.Context, e PermissionEvent) error {
	if len(e.Summary) > maxPermissionSummary {
		e.Summary = e.Summary[:maxPermissionSummary]
	}
	at := formatTime(e.At)
	if _, err := s.db.ExecContext(ctx, `
WITH p(v) AS (SELECT COALESCE((SELECT started_at FROM away_runs WHERE ended_at IS NULL ORDER BY id DESC LIMIT 1), '')),
over(x) AS (SELECT NOT EXISTS (SELECT 1 FROM permission_events, p WHERE session_id = ?1 AND period = p.v AND tool = ?4 AND summary = ?5 AND verdict = ?6)
  AND (SELECT COUNT(*) FROM (SELECT 1 FROM permission_events, p WHERE session_id = ?1 AND period = p.v AND verdict = ?6 LIMIT ?7)) >= ?7)
INSERT INTO permission_events (session_id, period, at, last_at, mode, tool, summary, verdict, n)
SELECT ?1, p.v, ?2, ?2, ?3, IIF(over.x, '', ?4), IIF(over.x, '', ?5), IIF(over.x, ?8, ?6), 1 FROM p, over WHERE true
ON CONFLICT (session_id, period, tool, summary, verdict) DO UPDATE SET n = n + 1, last_at = excluded.last_at`,
		e.SessionID, at, e.Mode, e.Tool, e.Summary, e.Verdict, PermissionDistinctCap, permissionOverflow); err != nil {
		return fmt.Errorf("store: add permission event: %w", err)
	}
	return nil
}

// PermissionEvents returns a session's stored events, oldest first.
func (s *Store) PermissionEvents(ctx context.Context, sessionID string) ([]PermissionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, mode, tool, summary, verdict, n FROM permission_events WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: permission events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PermissionEvent
	for rows.Next() {
		var e PermissionEvent
		if err := rows.Scan(&e.SessionID, &e.Mode, &e.Tool, &e.Summary, &e.Verdict, &e.N); err != nil {
			return nil, fmt.Errorf("store: permission events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

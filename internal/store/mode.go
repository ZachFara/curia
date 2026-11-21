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
}

// maxPermissionSummary bounds a stored summary, in bytes.
const maxPermissionSummary = 200

// PermissionEventCap bounds the rows one session keeps in the open away
// period: further denials are dropped (an agent controls the rate), and the
// summary says "or more" for a session at the cap. The count reads at most
// the cap, so the hook does bounded work.
const PermissionEventCap = 500

// AddPermissionEvent stores e unless its session already has
// PermissionEventCap rows since the open away period began (all its rows when
// none is open); the summary is cut to maxPermissionSummary.
func (s *Store) AddPermissionEvent(ctx context.Context, e PermissionEvent) error {
	if len(e.Summary) > maxPermissionSummary {
		e.Summary = e.Summary[:maxPermissionSummary]
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO permission_events (session_id, at, mode, tool, summary, verdict)
SELECT ?1, ?2, ?3, ?4, ?5, ?6 WHERE (SELECT COUNT(*) FROM (SELECT 1 FROM permission_events WHERE session_id = ?1
  AND at >= COALESCE((SELECT started_at FROM away_runs WHERE ended_at IS NULL ORDER BY id DESC LIMIT 1), '') LIMIT ?7)) < ?7`,
		e.SessionID, formatTime(e.At), e.Mode, e.Tool, e.Summary, e.Verdict, PermissionEventCap); err != nil {
		return fmt.Errorf("store: add permission event: %w", err)
	}
	return nil
}

// PermissionEvents returns a session's stored events, oldest first.
func (s *Store) PermissionEvents(ctx context.Context, sessionID string) ([]PermissionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, mode, tool, summary, verdict FROM permission_events WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: permission events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PermissionEvent
	for rows.Next() {
		var e PermissionEvent
		if err := rows.Scan(&e.SessionID, &e.Mode, &e.Tool, &e.Summary, &e.Verdict); err != nil {
			return nil, fmt.Errorf("store: permission events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

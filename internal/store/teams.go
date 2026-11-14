package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/team"
)

// SetTeams writes team membership for every session that is not ended, and
// returns how many rows changed. In one write transaction, each such
// session's team columns become its entry in assigned, or NULL when it has
// none or is stale (a stale session is in no team, whatever assigned says),
// and are written only when they differ. Then each teammate with no task
// takes the task of its team's live lead, when that has one. It writes no
// other column: updated_at and seen_at are left alone, so a refresh never
// looks like activity. An ended session is never written.
func (s *Store) SetTeams(ctx context.Context, assigned map[string]team.Membership) (int, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: set teams: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	changed := map[string]bool{}
	if err := writeMemberships(ctx, tx, assigned, changed); err != nil {
		return 0, fmt.Errorf("store: set teams: %w", err)
	}
	if err := inheritLeadTask(ctx, tx, changed); err != nil {
		return 0, fmt.Errorf("store: set teams: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: set teams: commit: %w", err)
	}
	return len(changed), nil
}

// teamRow is a session's id, state and stored membership.
type teamRow struct {
	id    string
	state model.State
	cur   team.Membership
}

// writeMemberships sets the team columns of every session that is not
// ended to what assigned gives it, where they differ, and records the ids
// it wrote in changed. An empty value is stored as NULL.
func writeMemberships(ctx context.Context, tx *sql.Tx, assigned map[string]team.Membership, changed map[string]bool) error {
	all, err := teamRows(ctx, tx)
	if err != nil {
		return err
	}
	for _, r := range all {
		var want team.Membership
		if r.state != model.StateStale {
			want = assigned[r.id]
		}
		if want == r.cur {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE sessions SET team_id = NULLIF(?, ''), team_role = NULLIF(?, ''), team_member = NULLIF(?, '')
WHERE id = ?`, want.TeamID, want.Role, want.Member, r.id); err != nil {
			return err
		}
		changed[r.id] = true
	}
	return nil
}

// teamRows reads every session that is not ended, with its membership.
func teamRows(ctx context.Context, tx *sql.Tx) ([]teamRow, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, state, COALESCE(team_id, ''), COALESCE(team_role, ''), COALESCE(team_member, '')
FROM sessions WHERE state != ? ORDER BY id`, model.StateEnded)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var all []teamRow
	for rows.Next() {
		var r teamRow
		if err := rows.Scan(&r.id, &r.state, &r.cur.TeamID, &r.cur.Role, &r.cur.Member); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	return all, rows.Err()
}

// leadTask is the task of the lead of the team of the sessions row being
// updated: a session that is not ended, has role lead in the same team and
// has a task. The lowest id wins if a team somehow has two.
const leadTask = `(
  SELECT l.task_id FROM sessions l
  WHERE l.team_id = sessions.team_id AND l.team_role = 'lead' AND l.state != 'ended'
    AND l.task_id IS NOT NULL AND l.task_id != ''
  ORDER BY l.id LIMIT 1)`

// inheritLeadTask gives each teammate that is not ended and has no task
// its lead's task, and records the ids it wrote in changed.
func inheritLeadTask(ctx context.Context, tx *sql.Tx, changed map[string]bool) error {
	rows, err := tx.QueryContext(ctx, `
UPDATE sessions SET task_id = `+leadTask+`
WHERE team_role = ? AND state != ? AND COALESCE(task_id, '') = '' AND `+leadTask+` IS NOT NULL
RETURNING id`, model.RoleTeammate, model.StateEnded)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		changed[id] = true
	}
	return rows.Err()
}

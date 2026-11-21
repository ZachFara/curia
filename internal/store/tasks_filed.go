package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// ErrFileCap is returned by FileTask when the filing session has already
// filed its cap of tasks in the last 24 hours.
var ErrFileCap = errors.New("store: daily cap of agent-filed tasks reached")

// FiledWindow is the rolling window the cap counts over.
const FiledWindow = 24 * time.Hour

// FileTask stores t, which an agent filed (t.FiledBy and t.FiledReason set,
// status backlog or ready), in one write transaction: it counts the tasks
// t.FiledBy filed in the 24 hours before t.CreatedAt, returns ErrFileCap
// when that is perDay or more, and otherwise inserts the task with the filer's
// lineage as Curia can see it (its team, its role and the team's lead from
// the sessions table; display and audit only, no permission rests on it).
func (s *Store) FileTask(ctx context.Context, t model.Task, perDay int) (model.Task, error) {
	if t.FiledBy == "" || t.FiledReason == "" {
		return model.Task{}, fail("file task", "", errors.New("a filed task needs a filer and a reason"))
	}
	if err := checkStored(t.Status); err != nil {
		return model.Task{}, fail("file task", "", err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Task{}, fail("file task", "", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE filed_by = ? AND created_at > ?`,
		t.FiledBy, formatTime(t.CreatedAt.Add(-FiledWindow))).Scan(&n); err != nil {
		return model.Task{}, fail("file task", "", err)
	}
	if n >= perDay {
		return model.Task{}, ErrFileCap
	}
	t.Source = model.SourceLocal
	id, err := insertTask(ctx, tx, t)
	if err != nil {
		return model.Task{}, fail("file task", "", err)
	}
	lineage, err := filerLineage(ctx, tx, t.FiledBy)
	if err != nil {
		return model.Task{}, fail("file task", id, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET filed_by = ?, filed_reason = ?, filer_lineage = ?, ready_hash = ? WHERE id = ?`,
		t.FiledBy, t.FiledReason, lineage, readyHashFor(t), id); err != nil {
		return model.Task{}, fail("file task", id, err)
	}
	stored, err := commitTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, fail("file task", id, err)
	}
	return stored, nil
}

// filerLineage is the JSON of the filer's team, role and the team's lead.
// A session Curia has no row for, or one in no team, gives only its id.
func filerLineage(ctx context.Context, tx *sql.Tx, session string) (string, error) {
	l := struct {
		Session string `json:"session"`
		Team    string `json:"team,omitempty"`
		Role    string `json:"role,omitempty"`
		Lead    string `json:"lead,omitempty"`
	}{Session: session}
	var team, role sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT team_id, team_role FROM sessions WHERE id = ?`, session).Scan(&team, &role)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	l.Team, l.Role = team.String, role.String
	if l.Team != "" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE team_id = ? AND team_role = 'lead' ORDER BY updated_at DESC, id LIMIT 1`, l.Team).Scan(&l.Lead)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	b, err := json.Marshal(l)
	return string(b), err
}

// readyHashFor is the F29 hash of a task filed ready by the user's own
// level = ready setting; any other task has none (NULL).
func readyHashFor(t model.Task) any {
	if t.Status != model.TaskReady {
		return nil
	}
	return model.ReadyHash(t.Title, t.Body)
}

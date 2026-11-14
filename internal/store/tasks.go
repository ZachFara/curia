package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// Errors of the task operations. They are returned as they are, never
// wrapped, so callers may compare them with errors.Is or ==.
var (
	ErrNoTask   = errors.New("store: no such task")
	ErrTaskBusy = errors.New("store: task is being launched")
)

// TaskFilter selects which tasks ListTasks returns.
type TaskFilter struct {
	Statuses []model.TaskStatus // stored statuses; empty: all
	Repo     string             // empty: all
}

// failTask labels an SQL error with the operation and, when id is not
// empty, the task it was on. The task errors are returned as they are.
func failTask(op, id string, err error) error {
	if errors.Is(err, ErrNoTask) || errors.Is(err, ErrTaskBusy) {
		return err
	}
	return fail(op, id, err)
}

// selectTasks is the column list every reader of tasks uses, in scanTask's
// order.
const selectTasks = `
SELECT n, id, title, body, repo, base_branch, status, source, source_ref, tags, branch, worktree,
  team, agent_model, pane, resume_session, created_at, updated_at, launched_at, paused_at, cleaned_at
FROM tasks`

// scanTask reads one row of selectTasks.
func scanTask(sc scanner) (model.Task, error) {
	var t model.Task
	var tags string
	var created, updated, launched, paused, cleaned sql.NullString
	err := sc.Scan(&t.N, &t.ID, &t.Title, &t.Body, &t.Repo, &t.BaseBranch, &t.Status, &t.Source, &t.SourceRef,
		&tags, &t.Branch, &t.Worktree, &t.Team, &t.AgentModel, &t.Pane, &t.ResumeSession,
		&created, &updated, &launched, &paused, &cleaned)
	if err != nil {
		return model.Task{}, err
	}
	t.Tags = strings.Fields(tags)
	for _, f := range []struct {
		dst *time.Time
		src sql.NullString
	}{
		{&t.CreatedAt, created},
		{&t.UpdatedAt, updated},
		{&t.LaunchedAt, launched},
		{&t.PausedAt, paused},
		{&t.CleanedAt, cleaned},
	} {
		v, err := parseTime(f.src)
		if err != nil {
			return model.Task{}, fmt.Errorf("task %s: undecodable time: %w", t.ID, err)
		}
		*f.dst = v
	}
	return t, nil
}

// getTask loads task id through q, and false if there is none.
func getTask(ctx context.Context, q dbtx, id string) (model.Task, bool, error) {
	t, err := scanTask(q.QueryRowContext(ctx, selectTasks+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Task{}, false, nil
	}
	if err != nil {
		return model.Task{}, false, err
	}
	return t, true, nil
}

// loadTask loads task id inside tx, or ErrNoTask.
func loadTask(ctx context.Context, tx *sql.Tx, id string) (model.Task, error) {
	t, found, err := getTask(ctx, tx, id)
	if err == nil && !found {
		err = ErrNoTask
	}
	return t, err
}

// commitTask reads task id back inside tx, so the caller gets the row
// exactly as stored, and commits.
func commitTask(ctx context.Context, tx *sql.Tx, id string) (model.Task, error) {
	t, err := loadTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Task{}, fmt.Errorf("commit: %w", err)
	}
	return t, nil
}

// checkStored refuses a status that is derived rather than stored (plan
// section 4.4): only backlog, ready and dropped are written.
func checkStored(st model.TaskStatus) error {
	switch st {
	case model.TaskBacklog, model.TaskReady, model.TaskDropped:
		return nil
	}
	return fmt.Errorf("status %q is not a stored task status", st)
}

// insertTask inserts t with the next id, T-<n>, and created_at = updated_at
// = t.CreatedAt, and returns the id. The write lock makes the subselect
// agree with the rowid n the row receives; rows are never deleted, so an
// id is never reused. An empty source is local.
func insertTask(ctx context.Context, tx *sql.Tx, t model.Task) (string, error) {
	if t.Source == "" {
		t.Source = model.SourceLocal
	}
	var id string
	err := tx.QueryRowContext(ctx, `
INSERT INTO tasks (
  id, title, body, repo, base_branch, status, source, source_ref, tags, branch, worktree,
  team, agent_model, pane, resume_session, created_at, updated_at, launched_at, paused_at, cleaned_at
) VALUES (
  'T-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM tasks), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, ?, ?, ?, ?
) RETURNING id`,
		t.Title, t.Body, t.Repo, t.BaseBranch, t.Status, t.Source, t.SourceRef, strings.Join(t.Tags, " "),
		t.Branch, t.Worktree, t.Team, t.AgentModel, t.Pane, t.ResumeSession,
		formatTime(t.CreatedAt), formatTime(t.CreatedAt), formatTime(t.LaunchedAt), formatTime(t.PausedAt),
		formatTime(t.CleanedAt)).Scan(&id)
	return id, err
}

// CreateTask stores t as a new task in one write transaction, with the next
// id (T-<n>, plan section 4.2) and created_at = updated_at = t.CreatedAt;
// t's N, ID and UpdatedAt are ignored. A status other than backlog, ready
// or dropped is an error and stores nothing. It returns the stored row.
func (s *Store) CreateTask(ctx context.Context, t model.Task) (model.Task, error) {
	if err := checkStored(t.Status); err != nil {
		return model.Task{}, fail("create task", "", err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Task{}, fail("create task", "", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := insertTask(ctx, tx, t)
	if err != nil {
		return model.Task{}, fail("create task", "", err)
	}
	stored, err := commitTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, fail("create task", id, err)
	}
	return stored, nil
}

// ImportTask stores t, which comes from a source other than local, in one
// write transaction. When a task with the same repo, source and source ref
// exists, only its title, body and tags are replaced by t's, and it is
// returned with created false. Otherwise t is created as CreateTask does
// and returned with created true.
func (s *Store) ImportTask(ctx context.Context, t model.Task) (stored model.Task, created bool, err error) {
	if t.Source == "" || t.Source == model.SourceLocal {
		return model.Task{}, false, fail("import task", "", errors.New("a local task is not imported"))
	}
	if err = checkStored(t.Status); err != nil {
		return model.Task{}, false, fail("import task", "", err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Task{}, false, fail("import task", "", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `
UPDATE tasks SET title = ?, body = ?, tags = ?
WHERE repo = ? AND source = ? AND source_ref = ?
RETURNING id`, t.Title, t.Body, strings.Join(t.Tags, " "), t.Repo, t.Source, t.SourceRef).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		created = true
		id, err = insertTask(ctx, tx, t)
	}
	if err != nil {
		return model.Task{}, false, fail("import task", id, err)
	}
	if stored, err = commitTask(ctx, tx, id); err != nil {
		return model.Task{}, false, fail("import task", id, err)
	}
	return stored, created, nil
}

// GetTask returns the task with the given id, and false if there is none.
func (s *Store) GetTask(ctx context.Context, id string) (model.Task, bool, error) {
	t, found, err := getTask(ctx, s.db, id)
	if err != nil {
		return model.Task{}, false, fail("get task", id, err)
	}
	return t, found, nil
}

// listTasks is ListTasks' query. Parameters: the number of statuses, the
// statuses as a JSON array, then the repo twice.
const listTasks = selectTasks + `
WHERE (? = 0 OR status IN (SELECT value FROM json_each(?)))
  AND (? = '' OR repo = ?)
ORDER BY n`

// ListTasks returns the tasks f selects, in id order.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]model.Task, error) {
	statuses := f.Statuses
	if statuses == nil {
		statuses = []model.TaskStatus{}
	}
	js, err := json.Marshal(statuses)
	if err != nil {
		return nil, fail("list tasks", "", err)
	}
	rows, err := s.db.QueryContext(ctx, listTasks, len(f.Statuses), string(js), f.Repo, f.Repo)
	if err != nil {
		return nil, fail("list tasks", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fail("list tasks", "", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("list tasks", "", err)
	}
	return out, nil
}

// UpdateTask changes task id based on its stored value, in one write
// transaction that takes the write lock before reading: it loads the task
// (ErrNoTask if there is none, and fn is not called), calls fn and writes
// every column of what fn returns but n, id and created_at, updated_at
// included. An error from fn rolls back and is returned as it is. A status
// other than backlog, ready or dropped is an error and writes nothing. It
// returns the stored row.
//
// fn must not call the Store: the transaction holds the write lock.
func (s *Store) UpdateTask(ctx context.Context, id string, fn func(model.Task) (model.Task, error)) (model.Task, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Task{}, failTask("update task", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := loadTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, failTask("update task", id, err)
	}
	next, err := fn(cur)
	if err != nil {
		return model.Task{}, err
	}
	if err = checkStored(next.Status); err == nil {
		_, err = tx.ExecContext(ctx, `
UPDATE tasks SET title = ?, body = ?, repo = ?, base_branch = ?, status = ?, source = ?, source_ref = ?,
  tags = ?, branch = ?, worktree = ?, team = ?, agent_model = ?, pane = ?, resume_session = ?,
  updated_at = ?, launched_at = ?, paused_at = ?, cleaned_at = ?
WHERE n = ?`,
			next.Title, next.Body, next.Repo, next.BaseBranch, next.Status, next.Source, next.SourceRef,
			strings.Join(next.Tags, " "), next.Branch, next.Worktree, next.Team, next.AgentModel, next.Pane,
			next.ResumeSession, formatTime(next.UpdatedAt), formatTime(next.LaunchedAt), formatTime(next.PausedAt),
			formatTime(next.CleanedAt), cur.N)
	}
	if err != nil {
		return model.Task{}, failTask("update task", id, err)
	}
	stored, err := commitTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, failTask("update task", id, err)
	}
	return stored, nil
}

// ClaimLaunch claims task id for a launch at now, in one write transaction
// (plan section 4.6, step 1): ErrNoTask if there is none, ErrTaskBusy when
// its launched_at is after now - hold (a launch is under way, or started
// too recently to have died); otherwise launched_at becomes now and the
// stored task is returned.
func (s *Store) ClaimLaunch(ctx context.Context, id string, now time.Time, hold time.Duration) (model.Task, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Task{}, failTask("claim launch of", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := loadTask(ctx, tx, id)
	if err == nil && cur.LaunchedAt.After(now.Add(-hold)) {
		err = ErrTaskBusy
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET launched_at = ? WHERE n = ?`, formatTime(now), cur.N)
	}
	if err != nil {
		return model.Task{}, failTask("claim launch of", id, err)
	}
	stored, err := commitTask(ctx, tx, id)
	if err != nil {
		return model.Task{}, failTask("claim launch of", id, err)
	}
	return stored, nil
}

// TaskSessions returns the live sessions linked to task taskID (neither
// ended nor stale), most recently updated first. An empty taskID has none.
func (s *Store) TaskSessions(ctx context.Context, taskID string) ([]model.Session, error) {
	if taskID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, selectSessions+`
WHERE task_id = ? AND state NOT IN (?, ?)
ORDER BY updated_at DESC, id`, taskID, model.StateEnded, model.StateStale)
	if err != nil {
		return nil, fail("task sessions of", taskID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fail("task sessions of", taskID, err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("task sessions of", taskID, err)
	}
	return out, nil
}

// OpenBlockingByTask counts, by task id, the blocking tickets open at now.
// Tickets with no task, and tasks with none open, are absent.
func (s *Store) OpenBlockingByTask(ctx context.Context, now time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.task_id, count(*) FROM decisions d
WHERE d.task_id != '' AND d.mode = 'blocking' AND `+openAt+`
GROUP BY d.task_id`, formatTime(now))
	if err != nil {
		return nil, fail("open blocking by task", "", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fail("open blocking by task", "", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fail("open blocking by task", "", err)
	}
	return out, nil
}

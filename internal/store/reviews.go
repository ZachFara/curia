package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// Errors of the review operations. They are returned as they are, never
// wrapped, so callers may compare them with errors.Is or ==.
var (
	ErrNoReview     = errors.New("store: no such review")
	ErrReviewClosed = errors.New("store: review is not pending")
)

// ReviewFilter selects which reviews ListReviews returns.
type ReviewFilter struct {
	Statuses []model.ReviewStatus // empty: all
	TaskID   string               // empty: all
	Limit    int                  // 0: no limit; otherwise the newest Limit, still oldest first
}

// ReviewClose is what closing a pending review writes.
type ReviewClose struct {
	Status                model.ReviewStatus // approved, merged, changes_requested or dropped
	How                   model.ApproveHow   // for approved and merged
	DiffStat, Head, PRURL string             // for approved and merged
	Notes                 string             // for changes_requested
	Via                   model.AnsweredVia
}

// TaskRun sums up the sessions ever linked to a task.
type TaskRun struct {
	Sessions, Teammates int
	Started             time.Time // the earliest started_at; zero when no session
	CostUSD             float64
	CostKnown           bool // at least one linked session has a cost
}

// failReview labels an SQL error with the operation and, when id is not
// empty, the review or task it was on. The review errors and ErrNoTask are
// returned as they are.
func failReview(op, id string, err error) error {
	if errors.Is(err, ErrNoReview) || errors.Is(err, ErrReviewClosed) || errors.Is(err, ErrNoTask) {
		return err
	}
	return fail(op, id, err)
}

// selectReviews is the column list every reader of reviews uses, in
// scanReview's order.
const selectReviews = `
SELECT n, id, task_id, session_id, summary, tests_status, tests_detail, status, approved_how,
  diff_stat, head, pr_url, my_notes, note_message, closed_via, created_at, updated_at, closed_at
FROM reviews`

// scanReview reads one row of selectReviews.
func scanReview(sc scanner) (model.Review, error) {
	var r model.Review
	var created, updated, closed sql.NullString
	err := sc.Scan(&r.N, &r.ID, &r.TaskID, &r.SessionID, &r.Summary, &r.Tests, &r.TestsDetail, &r.Status,
		&r.ApprovedHow, &r.DiffStat, &r.Head, &r.PRURL, &r.Notes, &r.NoteMessage, &r.ClosedVia,
		&created, &updated, &closed)
	if err != nil {
		return model.Review{}, err
	}
	for _, f := range []struct {
		dst *time.Time
		src sql.NullString
	}{
		{&r.CreatedAt, created},
		{&r.UpdatedAt, updated},
		{&r.ClosedAt, closed},
	} {
		v, err := parseTime(f.src)
		if err != nil {
			return model.Review{}, fmt.Errorf("review %s: undecodable time: %w", r.ID, err)
		}
		*f.dst = v
	}
	return r, nil
}

// getReview loads review id through q, and false if there is none.
func getReview(ctx context.Context, q dbtx, id string) (model.Review, bool, error) {
	r, err := scanReview(q.QueryRowContext(ctx, selectReviews+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Review{}, false, nil
	}
	if err != nil {
		return model.Review{}, false, err
	}
	return r, true, nil
}

// queryReviews runs a query of selectReviews and scans every row.
func queryReviews(ctx context.Context, q dbtx, query string, args ...any) ([]model.Review, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// commitReview reads review id back inside tx, so the caller gets the row
// exactly as stored, and commits.
func commitReview(ctx context.Context, tx *sql.Tx, id string) (model.Review, error) {
	r, found, err := getReview(ctx, tx, id)
	if err == nil && !found {
		err = ErrNoReview
	}
	if err != nil {
		return model.Review{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Review{}, fmt.Errorf("commit: %w", err)
	}
	return r, nil
}

// The statements of CompleteTask. updatePendingReview's parameters: the
// session, summary, tests status and detail, updated_at, then the task id
// (by the reviews_one_pending index). insertReview allocates R-<n> under
// the write lock, as insertTask does; its parameters: the task, session,
// summary, tests status and detail, then created_at twice.
const (
	updatePendingReview = `
UPDATE reviews SET session_id = ?, summary = ?, tests_status = ?, tests_detail = ?, updated_at = ?
WHERE task_id = ? AND status = 'pending'
RETURNING id`
	insertReview = `
INSERT INTO reviews (id, task_id, session_id, summary, tests_status, tests_detail, status, created_at, updated_at)
VALUES ('R-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM reviews), ?, ?, ?, ?, ?, 'pending', ?, ?)
RETURNING id`
)

// CompleteTask records that r.TaskID's agent says it is done: it updates the task's pending review
// (summary, tests, session, updated_at) or opens a new pending one. r carries TaskID, SessionID, Summary,
// Tests, TestsDetail and CreatedAt (the time of the call). ErrNoTask: no such task, or it is stored dropped.
// It runs in one write transaction; the partial unique index reviews_one_pending also guarantees that a
// task never has two pending reviews.
func (s *Store) CompleteTask(ctx context.Context, r model.Review) (stored model.Review, updated bool, err error) {
	if r.TaskID == "" {
		return model.Review{}, false, ErrNoTask
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Review{}, false, failReview("complete task", r.TaskID, err)
	}
	defer func() { _ = tx.Rollback() }()
	id, updated, err := completeIn(ctx, tx, r)
	if err != nil {
		return model.Review{}, false, failReview("complete task", r.TaskID, err)
	}
	if stored, err = commitReview(ctx, tx, id); err != nil {
		return model.Review{}, false, failReview("complete task", r.TaskID, err)
	}
	return stored, updated, nil
}

// completeIn is CompleteTask inside its transaction: it returns the id of
// the review it updated or opened, and whether it updated one.
func completeIn(ctx context.Context, tx *sql.Tx, r model.Review) (string, bool, error) {
	var status model.TaskStatus
	err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, r.TaskID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status == model.TaskDropped) {
		return "", false, ErrNoTask
	}
	if err != nil {
		return "", false, err
	}
	at := formatTime(r.CreatedAt)
	var id string
	err = tx.QueryRowContext(ctx, updatePendingReview, r.SessionID, r.Summary, r.Tests, r.TestsDetail, at, r.TaskID).
		Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	err = tx.QueryRowContext(ctx, insertReview, r.TaskID, r.SessionID, r.Summary, r.Tests, r.TestsDetail, at, at).
		Scan(&id)
	return id, false, err
}

// GetReview returns the review with the given id, and false if there is none.
func (s *Store) GetReview(ctx context.Context, id string) (model.Review, bool, error) {
	r, found, err := getReview(ctx, s.db, id)
	if err != nil {
		return model.Review{}, false, fail("get review", id, err)
	}
	return r, found, nil
}

// listReviewsQuery builds ListReviews' statement for f from fixed
// fragments (no value of f is ever part of the SQL): by task through the
// reviews_task index, by status through reviews_status, and with a Limit
// the newest rows, put back in n order by the outer select.
func listReviewsQuery(f ReviewFilter) (string, []any) {
	var where []string
	var args []any
	if f.TaskID != "" {
		where, args = append(where, `task_id = ?`), append(args, f.TaskID)
	}
	if len(f.Statuses) > 0 {
		where = append(where, `status IN (?`+strings.Repeat(`, ?`, len(f.Statuses)-1)+`)`)
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	q := selectReviews
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	if f.Limit <= 0 {
		return q + ` ORDER BY n`, args
	}
	return `SELECT * FROM (` + q + ` ORDER BY n DESC LIMIT ?) ORDER BY n`, append(args, f.Limit)
}

// ListReviews returns the reviews f selects, by n, oldest first; with a
// positive Limit only the newest Limit of them, still oldest first.
func (s *Store) ListReviews(ctx context.Context, f ReviewFilter) ([]model.Review, error) {
	q, args := listReviewsQuery(f)
	out, err := queryReviews(ctx, s.db, q, args...)
	if err != nil {
		return nil, fail("list reviews", "", err)
	}
	return out, nil
}

// latestReviews selects each task's newest review: the subquery reads only
// the reviews_task index, then each row is one rowid lookup.
const latestReviews = selectReviews + `
WHERE n IN (SELECT MAX(n) FROM reviews GROUP BY task_id)`

// LatestReviews returns each task's newest review, by task id.
func (s *Store) LatestReviews(ctx context.Context) (map[string]model.Review, error) {
	rs, err := queryReviews(ctx, s.db, latestReviews)
	if err != nil {
		return nil, fail("latest reviews", "", err)
	}
	out := make(map[string]model.Review, len(rs))
	for _, r := range rs {
		out[r.TaskID] = r
	}
	return out, nil
}

// pendingOfLiveTasks is the pending reviews (by the reviews_one_pending
// index) of tasks not stored dropped (one lookup in tasks' id index each).
const pendingOfLiveTasks = `
FROM reviews r JOIN tasks t ON t.id = r.task_id
WHERE r.status = 'pending' AND t.status != 'dropped'`

// The statements of PendingReviewTasks and PendingReviews.
const (
	pendingReviewTasks  = `SELECT r.task_id, r.id` + pendingOfLiveTasks
	pendingReviewsCount = `SELECT count(*)` + pendingOfLiveTasks
)

// PendingReviewTasks returns the id of the pending review of each task that has one and is not stored
// dropped, by task id.
func (s *Store) PendingReviewTasks(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, pendingReviewTasks)
	if err != nil {
		return nil, fail("pending reviews by task", "", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var task, id string
		if err := rows.Scan(&task, &id); err != nil {
			return nil, fail("pending reviews by task", "", err)
		}
		out[task] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fail("pending reviews by task", "", err)
	}
	return out, nil
}

// PendingReviews counts what PendingReviewTasks would return, in one statement (the badge).
func (s *Store) PendingReviews(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, pendingReviewsCount).Scan(&n); err != nil {
		return 0, fail("count pending reviews", "", err)
	}
	return n, nil
}

// The statements of CloseReview. closePending's parameters: the status,
// approved_how, diff_stat, head, pr_url, my_notes, closed_via, updated_at,
// closed_at, then the review id. expireTaskAssumptions' parameters: now
// (closed_at), the task id, now (the ticket is open at now: an assume
// ticket already past its deadline is the sweep's, closed at its deadline).
// dropReviewTask's: now, the task id.
const (
	closePending = `
UPDATE reviews SET status = ?, approved_how = ?, diff_stat = ?, head = ?, pr_url = ?, my_notes = ?,
  closed_via = ?, updated_at = ?, closed_at = ?
WHERE id = ? AND status = 'pending'
RETURNING task_id`
	expireTaskAssumptions = `
UPDATE decisions SET status = 'expired', closed_at = ?
WHERE task_id = ? AND mode = 'assume' AND status = 'open' AND (expires_at IS NULL OR expires_at > ?)`
	dropReviewTask = `UPDATE tasks SET status = 'dropped', updated_at = ? WHERE id = ?`
)

// closing is what closing a review with c writes: c with only the fields
// its status records.
func closing(c ReviewClose) (ReviewClose, error) {
	w := ReviewClose{Status: c.Status, Via: c.Via}
	switch c.Status {
	case model.ReviewApproved, model.ReviewMerged:
		w.How, w.DiffStat, w.Head, w.PRURL = c.How, c.DiffStat, c.Head, c.PRURL
	case model.ReviewChanges:
		w.Notes = c.Notes
	case model.ReviewDropped:
	default:
		return ReviewClose{}, fmt.Errorf("a review is not closed as %q", c.Status)
	}
	return w, nil
}

// CloseReview closes pending review id as c says, in one write transaction. For approved and merged it
// also expires the task's open assume tickets and returns how many; for dropped it also stores the task
// as dropped. ErrNoReview; ErrReviewClosed when the review is not pending. A Status other than approved,
// merged, changes_requested or dropped is an error and changes nothing. Of c's fields only those its
// Status records are written: How, DiffStat, Head and PRURL for approved and merged, Notes for
// changes_requested.
func (s *Store) CloseReview(ctx context.Context, id string, c ReviewClose, now time.Time) (r model.Review, expired int, err error) {
	w, err := closing(c)
	if err != nil {
		return model.Review{}, 0, fail("close review", id, err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Review{}, 0, failReview("close review", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if expired, err = closeIn(ctx, tx, id, w, now); err != nil {
		return model.Review{}, 0, failReview("close review", id, err)
	}
	if r, err = commitReview(ctx, tx, id); err != nil {
		return model.Review{}, 0, failReview("close review", id, err)
	}
	return r, expired, nil
}

// closeIn is CloseReview inside its transaction.
func closeIn(ctx context.Context, tx *sql.Tx, id string, w ReviewClose, now time.Time) (int, error) {
	at := formatTime(now)
	var taskID string
	err := tx.QueryRowContext(ctx, closePending, w.Status, w.How, w.DiffStat, w.Head, w.PRURL, w.Notes, w.Via,
		at, at, id).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, notPending(ctx, tx, id)
	}
	if err != nil || taskID == "" {
		return 0, err
	}
	switch w.Status {
	case model.ReviewApproved, model.ReviewMerged:
		return expireAssumptions(ctx, tx, taskID, at)
	case model.ReviewDropped:
		_, err = tx.ExecContext(ctx, dropReviewTask, at, taskID)
	}
	return 0, err
}

// expireAssumptions expires the assume tickets of task taskID open at at,
// closing them at at, in one statement, and returns how many.
func expireAssumptions(ctx context.Context, tx *sql.Tx, taskID string, at any) (int, error) {
	res, err := tx.ExecContext(ctx, expireTaskAssumptions, at, taskID, at)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// notPending is the error for review id when an UPDATE that expected it
// pending matched nothing: ErrNoReview or ErrReviewClosed.
func notPending(ctx context.Context, tx *sql.Tx, id string) error {
	_, found, err := getReview(ctx, tx, id)
	switch {
	case err != nil:
		return err
	case !found:
		return ErrNoReview
	}
	return ErrReviewClosed
}

// SetReviewNoteMessage records the message that carries a change request,
// in one write transaction. ErrNoReview; a review that is not
// changes_requested is an error and changes nothing.
func (s *Store) SetReviewNoteMessage(ctx context.Context, id, messageID string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return failReview("set note message of", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE reviews SET note_message = ? WHERE id = ? AND status = 'changes_requested'`,
		messageID, id)
	var n int64
	if err == nil {
		n, err = res.RowsAffected()
	}
	if err == nil && n == 0 {
		if err = notPending(ctx, tx, id); errors.Is(err, ErrReviewClosed) {
			err = errors.New("the review is not changes_requested")
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return failReview("set note message of", id, err)
	}
	return nil
}

// taskDecisions selects every ticket of one task, by the decisions_task
// index, in n order.
const taskDecisions = selectDecisions + `
FROM decisions d WHERE d.task_id = ? ORDER BY d.n`

// TaskDecisions returns every ticket of the task, by n. An empty taskID has
// none.
func (s *Store) TaskDecisions(ctx context.Context, taskID string) ([]model.Decision, error) {
	if taskID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, taskDecisions, taskID)
	if err != nil {
		return nil, fail("decisions of task", taskID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Decision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, fail("decisions of task", taskID, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("decisions of task", taskID, err)
	}
	return out, nil
}

// taskRun sums up the sessions of one task, of any state, by the
// sessions_task partial index (whose condition the WHERE repeats).
const taskRun = `
SELECT count(*), COALESCE(SUM(team_role = 'teammate'), 0), MIN(started_at), COALESCE(SUM(cost_usd), 0), count(cost_usd)
FROM sessions WHERE task_id = ? AND task_id != ''`

// TaskRun sums up the sessions ever linked to task taskID, in any state:
// how many, how many are teammates, the earliest start and the summed cost
// of those whose cost is known. An empty taskID has none.
func (s *Store) TaskRun(ctx context.Context, taskID string) (TaskRun, error) {
	if taskID == "" {
		return TaskRun{}, nil
	}
	var run TaskRun
	var started sql.NullString
	var costs int
	err := s.db.QueryRowContext(ctx, taskRun, taskID).Scan(&run.Sessions, &run.Teammates, &started, &run.CostUSD, &costs)
	if err == nil {
		run.Started, err = parseTime(started)
	}
	if err != nil {
		return TaskRun{}, fail("sum up the run of task", taskID, err)
	}
	run.CostKnown = costs > 0
	return run, nil
}

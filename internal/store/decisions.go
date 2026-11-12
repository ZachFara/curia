package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
)

// Errors of the decision operations. They are returned as they are, never
// wrapped, so callers may compare them with errors.Is or ==.
var (
	ErrNotFound     = errors.New("store: no such decision")
	ErrNotOpen      = errors.New("store: decision is not open")
	ErrNotDecided   = errors.New("store: decision is not decided")
	ErrNotOwner     = errors.New("store: decision belongs to another session")
	ErrBadPrecedent = errors.New("store: precedent does not exist or is not decided")
)

// ruleErrors are the errors fail passes through unwrapped.
var ruleErrors = []error{ErrNotFound, ErrNotOpen, ErrNotDecided, ErrNotOwner, ErrBadPrecedent}

// fail labels an SQL error with the operation and, when id is not empty,
// the decision it was on. The store's own errors are returned as they are.
func fail(op, id string, err error) error {
	for _, e := range ruleErrors {
		if errors.Is(err, e) {
			return err
		}
	}
	if id == "" {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	return fmt.Errorf("store: %s %s: %w", op, id, err)
}

// DecisionFilter selects which decisions ListDecisions returns.
type DecisionFilter struct {
	Statuses  []model.DecisionStatus // empty: all
	SessionID string                 // empty: all
	Limit     int                    // 0: no limit; newest first when set
}

// selectDecisions is the column list every reader of decisions uses, in
// scanDecision's order, read from the alias d. A query adds
// "FROM decisions d" after it, or first the columns of a join, which
// scanDecision scans into its extra destinations.
const selectDecisions = `
SELECT d.n, d.id, d.session_id, d.task_id, d.repo, d.question, d.context, d.options,
  d.recommendation, d.rationale, d.reversibility, d.mode, d.assumption, d.scope, d.tags, d.status,
  d.answer_key, d.answer, d.answer_reasoning, d.answered_via, d.precedent_of, d.differs_from,
  d.differs_why, d.withdraw_reason, d.superseded_by, d.args_hash, d.delivery, d.delivered_via,
  d.created_at, d.expires_at, d.answered_at, d.closed_at, d.superseded_at, d.delivered_at`

// openAt is what "open" means at a time, its one parameter (plan section
// 4.7): status open and not past the deadline, whether or not a sweep has
// run. It reads the alias d.
const openAt = `d.status = 'open' AND (d.expires_at IS NULL OR d.expires_at > ?)`

// decided matches the decided statuses, the only ones that are precedent.
const decided = `d.status IN ('answered', 'confirmed', 'overridden')`

// scanDecision reads one row of selectDecisions, then any extra columns
// into extra.
func scanDecision(sc scanner, extra ...any) (model.Decision, error) {
	var d model.Decision
	var options, tags string
	var created, expires, answered, closed, superseded, delivered sql.NullString
	dest := append([]any{
		&d.N, &d.ID, &d.SessionID, &d.TaskID, &d.Repo, &d.Question, &d.Context, &options,
		&d.Recommendation, &d.Rationale, &d.Reversibility, &d.Mode, &d.Assumption, &d.Scope, &tags, &d.Status,
		&d.AnswerKey, &d.Answer, &d.AnswerReasoning, &d.AnsweredVia, &d.PrecedentOf, &d.DiffersFrom,
		&d.DiffersWhy, &d.WithdrawReason, &d.SupersededBy, &d.ArgsHash, &d.Delivery, &d.DeliveredVia,
		&created, &expires, &answered, &closed, &superseded, &delivered,
	}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return model.Decision{}, err
	}
	if err := json.Unmarshal([]byte(options), &d.Options); err != nil {
		return model.Decision{}, fmt.Errorf("options of %s: %w", d.ID, err)
	}
	d.Tags = strings.Fields(tags)
	for _, f := range []struct {
		dst *time.Time
		src sql.NullString
	}{
		{&d.CreatedAt, created},
		{&d.ExpiresAt, expires},
		{&d.AnsweredAt, answered},
		{&d.ClosedAt, closed},
		{&d.SupersededAt, superseded},
		{&d.DeliveredAt, delivered},
	} {
		t, err := parseTime(f.src)
		if err != nil {
			return model.Decision{}, fmt.Errorf("times of %s: %w", d.ID, err)
		}
		*f.dst = t
	}
	return d, nil
}

// getDecision loads decision id through q, and false if there is none.
func getDecision(ctx context.Context, q dbtx, id string) (model.Decision, bool, error) {
	d, err := scanDecision(q.QueryRowContext(ctx, selectDecisions+` FROM decisions d WHERE d.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Decision{}, false, nil
	}
	if err != nil {
		return model.Decision{}, false, err
	}
	return d, true, nil
}

// begin starts a write transaction. The DSN's _txlock=immediate makes it
// BEGIN IMMEDIATE, so the write lock is held before anything is read. Its
// error says "begin": SQLITE_BUSY there means nothing ran.
func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	return tx, nil
}

// commitRead reads decision id back inside tx, so the caller gets the row
// exactly as stored, and commits.
func commitRead(ctx context.Context, tx *sql.Tx, id string) (model.Decision, error) {
	d, _, err := getDecision(ctx, tx, id)
	if err != nil {
		return model.Decision{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Decision{}, fmt.Errorf("commit: %w", err)
	}
	return d, nil
}

// expireDue is the sweep: every open assume ticket at or past its deadline
// becomes expired, closed at the deadline. Its parameter is now.
const expireDue = `
UPDATE decisions SET status = 'expired', closed_at = expires_at
WHERE status = 'open' AND mode = 'assume' AND expires_at <= ?`

// CreateDecision stores the open ticket d, which comes from decision.New,
// in one write transaction (plan sections 4.2 and 4.5). A twin of d (same
// session and args hash) past its deadline is expired first. If a live
// twin remains, open or decided, the newest one is returned with duplicate
// true and nothing is inserted; an open twin takes d's DiffersFrom and
// DiffersWhy when d has a DiffersFrom. Otherwise d is inserted as open,
// with the next id, and the stored row is returned.
func (s *Store) CreateDecision(ctx context.Context, d model.Decision) (stored model.Decision, duplicate bool, err error) {
	if d.Options == nil {
		d.Options = []model.Option{}
	}
	options, err := json.Marshal(d.Options) // before the lock: only SQL runs under it
	if err != nil {
		return model.Decision{}, false, fail("create decision", "", err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, false, fail("create decision", "", err)
	}
	defer func() { _ = tx.Rollback() }()
	twin, found, err := liveTwin(ctx, tx, d)
	if err != nil {
		return model.Decision{}, false, fail("create decision", "", err)
	}
	id := twin.ID
	if !found {
		if id, err = insertDecision(ctx, tx, d, string(options)); err != nil {
			return model.Decision{}, false, fail("create decision", "", err)
		}
	}
	if stored, err = commitRead(ctx, tx, id); err != nil {
		return model.Decision{}, false, fail("create decision", id, err)
	}
	return stored, found, nil
}

// liveTwin expires d's past-deadline twins, then finds the newest live one
// and adds d's differs fields to it when it is open.
func liveTwin(ctx context.Context, tx *sql.Tx, d model.Decision) (model.Decision, bool, error) {
	now := formatTime(d.CreatedAt)
	if _, err := tx.ExecContext(ctx, expireDue+` AND session_id = ? AND args_hash = ?`,
		now, d.SessionID, d.ArgsHash); err != nil {
		return model.Decision{}, false, err
	}
	twin, err := scanDecision(tx.QueryRowContext(ctx, selectDecisions+`
FROM decisions d
WHERE d.session_id = ? AND d.args_hash = ? AND ((`+openAt+`) OR `+decided+`)
ORDER BY d.n DESC LIMIT 1`, d.SessionID, d.ArgsHash, now))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Decision{}, false, nil
	}
	if err != nil {
		return model.Decision{}, false, err
	}
	if twin.Status == model.StatusOpen && d.DiffersFrom != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE decisions SET differs_from = ?, differs_why = ? WHERE n = ?`,
			d.DiffersFrom, d.DiffersWhy, twin.N); err != nil {
			return model.Decision{}, false, err
		}
	}
	return twin, true, nil
}

// insertDecision inserts d as an open ticket and returns its id, D-<n>. The
// write lock makes the subselect agree with the rowid n the row receives;
// rows are never deleted, so an id is never reused.
func insertDecision(ctx context.Context, tx *sql.Tx, d model.Decision, options string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
INSERT INTO decisions (
  id, session_id, task_id, repo, question, context, options, recommendation, rationale,
  reversibility, mode, assumption, scope, tags, status, differs_from, differs_why, args_hash,
  delivery, created_at, expires_at
) VALUES (
  'D-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM decisions), ?, ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, 'open', ?, ?, ?, 'none', ?, ?
) RETURNING id`,
		d.SessionID, d.TaskID, d.Repo, d.Question, d.Context, options, d.Recommendation, d.Rationale,
		d.Reversibility, d.Mode, d.Assumption, d.Scope, strings.Join(d.Tags, " "), d.DiffersFrom, d.DiffersWhy,
		d.ArgsHash, formatTime(d.CreatedAt), formatTime(d.ExpiresAt)).Scan(&id)
	return id, err
}

// GetDecision returns the decision with the given id, and false if there is
// none.
func (s *Store) GetDecision(ctx context.Context, id string) (model.Decision, bool, error) {
	d, found, err := getDecision(ctx, s.db, id)
	if err != nil {
		return model.Decision{}, false, fail("get decision", id, err)
	}
	return d, found, nil
}

// The queries of ListDecisions: f.Statuses as a JSON array (with its
// length first), then f.SessionID twice, then for the newest-first form
// f.Limit.
const (
	listDecisions = selectDecisions + `
FROM decisions d
WHERE (? = 0 OR d.status IN (SELECT value FROM json_each(?)))
  AND (? = '' OR d.session_id = ?)`
	listDecisionsAll    = listDecisions + ` ORDER BY d.n`
	listDecisionsNewest = listDecisions + ` ORDER BY d.n DESC LIMIT ?`
)

// ListDecisions returns the decisions f selects: in id order, or with a
// positive Limit the newest Limit of them, newest first.
func (s *Store) ListDecisions(ctx context.Context, f DecisionFilter) ([]model.Decision, error) {
	statuses := f.Statuses
	if statuses == nil {
		statuses = []model.DecisionStatus{}
	}
	js, err := json.Marshal(statuses)
	if err != nil {
		return nil, fail("list decisions", "", err)
	}
	q, args := listDecisionsAll, []any{len(f.Statuses), string(js), f.SessionID, f.SessionID}
	if f.Limit > 0 {
		q, args = listDecisionsNewest, append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fail("list decisions", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Decision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, fail("list decisions", "", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("list decisions", "", err)
	}
	return out, nil
}

// OpenTickets returns the tickets open at now, in id order, each with what
// the queue shows of its session (zero values for an unknown session). The
// caller applies decision.Triage.
func (s *Store) OpenTickets(ctx context.Context, now time.Time) ([]model.Ticket, error) {
	rows, err := s.db.QueryContext(ctx, selectDecisions+`,
  COALESCE(s.state, ''), COALESCE(s.tmux_pane, ''), COALESCE(s.tmux_session, ''),
  COALESCE(s.tmux_window, ''), COALESCE(s.branch, ''), COALESCE(s.context_used, 0),
  s.cache_expires_at, COALESCE(s.cache_rebuild_usd, 0)
FROM decisions d LEFT JOIN sessions s ON s.id = d.session_id
WHERE `+openAt+`
ORDER BY d.n`, formatTime(now))
	if err != nil {
		return nil, fail("open tickets", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Ticket
	for rows.Next() {
		var t model.Ticket
		var cache sql.NullString
		t.Decision, err = scanDecision(rows, &t.SessionState, &t.Pane, &t.TmuxSession, &t.TmuxWindow,
			&t.Branch, &t.ContextUsed, &cache, &t.CacheRebuildUSD)
		if err != nil {
			return nil, fail("open tickets", "", err)
		}
		if t.CacheExpiresAt, err = parseTime(cache); err != nil {
			return nil, fail("open tickets", t.ID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("open tickets", "", err)
	}
	return out, nil
}

// OpenCounts counts the tickets open at now by session id ("" for tickets
// from an unknown caller) and mode. Sessions with none are absent.
func (s *Store) OpenCounts(ctx context.Context, now time.Time) (map[string]model.OpenCount, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.session_id, SUM(d.mode = 'blocking'), SUM(d.mode = 'assume')
FROM decisions d WHERE `+openAt+`
GROUP BY d.session_id`, formatTime(now))
	if err != nil {
		return nil, fail("open counts", "", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]model.OpenCount{}
	for rows.Next() {
		var id string
		var c model.OpenCount
		if err := rows.Scan(&id, &c.Blocking, &c.Assume); err != nil {
			return nil, fail("open counts", "", err)
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fail("open counts", "", err)
	}
	return out, nil
}

// CountOpenBlocking counts the blocking tickets open at now.
func (s *Store) CountOpenBlocking(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM decisions d WHERE d.mode = 'blocking' AND `+openAt,
		formatTime(now)).Scan(&n)
	if err != nil {
		return 0, fail("count open blocking", "", err)
	}
	return n, nil
}

// loadOpen loads decision id inside tx and requires it open at now.
func loadOpen(ctx context.Context, tx *sql.Tx, id string, now time.Time) (model.Decision, error) {
	d, found, err := getDecision(ctx, tx, id)
	switch {
	case err != nil:
		return model.Decision{}, err
	case !found:
		return model.Decision{}, ErrNotFound
	case d.Status != model.StatusOpen || (!d.ExpiresAt.IsZero() && !d.ExpiresAt.After(now)):
		return model.Decision{}, ErrNotOpen
	}
	return d, nil
}

// requireDecided returns ErrBadPrecedent unless ref names a decided
// decision other than self.
func requireDecided(ctx context.Context, tx *sql.Tx, ref, self string) error {
	if ref == self {
		return ErrBadPrecedent
	}
	d, found, err := getDecision(ctx, tx, ref)
	if err != nil {
		return err
	}
	if !found || !decision.IsDecided(d.Status) {
		return ErrBadPrecedent
	}
	return nil
}

// clearNeedsDecision sets session id to idle when it is needs_decision and
// has no blocking ticket open at now (plan section 4.3). Only state
// changes: updated_at and seen_at are left alone.
func clearNeedsDecision(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	if id == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
UPDATE sessions SET state = ?
WHERE id = ? AND state = ?
  AND NOT EXISTS (SELECT 1 FROM decisions d WHERE d.session_id = sessions.id AND d.mode = 'blocking' AND `+openAt+`)`,
		model.StateIdle, id, model.StateNeedsDecision, formatTime(now))
	return err
}

// AnswerDecision answers ticket id with a at now, in one write transaction:
// the ticket must exist (else ErrNotFound) and be open at now (else
// ErrNotOpen); decision.Resolve decides the status and delivery, and its
// error is returned unchanged. The answering session leaves needs_decision
// when no other blocking ticket of its is open. It returns the stored row.
func (s *Store) AnswerDecision(ctx context.Context, id string, a model.Answer, now time.Time) (model.Decision, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, fail("answer decision", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := loadOpen(ctx, tx, id, now)
	if err != nil {
		return model.Decision{}, fail("answer decision", id, err)
	}
	next, err := decision.Resolve(cur, a, now)
	if err != nil {
		return model.Decision{}, err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE decisions SET status = ?, scope = ?, answer_key = ?, answer = ?, answer_reasoning = ?,
  answered_via = ?, delivery = ?, answered_at = ?
WHERE n = ?`,
		next.Status, next.Scope, next.AnswerKey, next.Answer, next.AnswerReasoning,
		next.AnsweredVia, next.Delivery, formatTime(next.AnsweredAt), cur.N)
	if err == nil {
		err = clearNeedsDecision(ctx, tx, cur.SessionID, now)
	}
	if err != nil {
		return model.Decision{}, fail("answer decision", id, err)
	}
	stored, err := commitRead(ctx, tx, id)
	if err != nil {
		return model.Decision{}, fail("answer decision", id, err)
	}
	return stored, nil
}

// WithdrawDecision withdraws ticket id for callerSession at now, in one
// write transaction. The ticket must exist (ErrNotFound), be open at now
// (ErrNotOpen) and belong to callerSession (ErrNotOwner; two empty ids are
// equal). A non-empty precedentID must name another, decided ticket
// (ErrBadPrecedent); it is recorded as precedent_of. The session leaves
// needs_decision as AnswerDecision says. It returns the stored row.
func (s *Store) WithdrawDecision(ctx context.Context, id, callerSession, reason, precedentID string, now time.Time) (model.Decision, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, fail("withdraw decision", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := loadOpen(ctx, tx, id, now)
	if err == nil && cur.SessionID != callerSession {
		err = ErrNotOwner
	}
	if err == nil && precedentID != "" {
		err = requireDecided(ctx, tx, precedentID, id)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `
UPDATE decisions SET status = 'withdrawn', withdraw_reason = ?, precedent_of = ?, closed_at = ?
WHERE n = ?`, reason, precedentID, formatTime(now), cur.N)
	}
	if err == nil {
		err = clearNeedsDecision(ctx, tx, cur.SessionID, now)
	}
	if err != nil {
		return model.Decision{}, fail("withdraw decision", id, err)
	}
	stored, err := commitRead(ctx, tx, id)
	if err != nil {
		return model.Decision{}, fail("withdraw decision", id, err)
	}
	return stored, nil
}

// ExpireDecisions expires every open assume ticket at or past its deadline
// at now, closing it at the deadline, in one statement, and returns how
// many it expired. Blocking tickets never expire.
func (s *Store) ExpireDecisions(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, expireDue, formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store: expire decisions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: expire decisions: %w", err)
	}
	return int(n), nil
}

// EditDecision rescopes and/or supersedes decided ticket id at now, in one
// write transaction. The ticket must exist (ErrNotFound) and be decided
// (ErrNotDecided). A non-empty e.SupersededBy must name another decided
// ticket (ErrBadPrecedent). A non-empty e.Scope replaces the scope; with
// e.Supersede, superseded_at becomes now and superseded_by e.SupersededBy.
// The status never changes. It returns the stored row.
func (s *Store) EditDecision(ctx context.Context, id string, e model.DecisionEdit, now time.Time) (model.Decision, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, fail("edit decision", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, found, err := getDecision(ctx, tx, id)
	switch {
	case err != nil:
	case !found:
		err = ErrNotFound
	case !decision.IsDecided(cur.Status):
		err = ErrNotDecided
	case e.SupersededBy != "":
		err = requireDecided(ctx, tx, e.SupersededBy, id)
	}
	if err == nil {
		next := cur
		if e.Scope != "" {
			next.Scope = e.Scope
		}
		if e.Supersede {
			next.SupersededAt, next.SupersededBy = now, e.SupersededBy
		}
		_, err = tx.ExecContext(ctx, `UPDATE decisions SET scope = ?, superseded_at = ?, superseded_by = ? WHERE n = ?`,
			next.Scope, formatTime(next.SupersededAt), next.SupersededBy, cur.N)
	}
	if err != nil {
		return model.Decision{}, fail("edit decision", id, err)
	}
	stored, err := commitRead(ctx, tx, id)
	if err != nil {
		return model.Decision{}, fail("edit decision", id, err)
	}
	return stored, nil
}

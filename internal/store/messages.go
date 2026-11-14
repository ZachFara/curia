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

// Errors of the message operations. They are returned as they are, never
// wrapped, so callers may compare them with errors.Is or ==.
var (
	ErrNoMessage = errors.New("store: no such message")
	ErrNotHeld   = errors.New("store: message is not held")
	ErrNotQueued = errors.New("store: message is not queued")
)

// failMessage labels an SQL error with the operation and, when id is not
// empty, the message it was on. The message errors are returned as they are.
func failMessage(op, id string, err error) error {
	if errors.Is(err, ErrNoMessage) || errors.Is(err, ErrNotHeld) || errors.Is(err, ErrNotQueued) {
		return err
	}
	return fail(op, id, err)
}

// MessageFilter selects which messages ListMessages returns.
type MessageFilter struct {
	Statuses []model.MessageStatus // empty: all
	Limit    int                   // 0: no limit; newest first when set
}

// msgUndelivered matches the instructions an agent has not got yet: queued,
// or typed into its pane and not confirmed. It is the WHERE clause of the
// messages_undelivered index, word for word, so a query that uses it reads
// only that index.
const msgUndelivered = `(status = 'queued' OR (status = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL))`

// msgTyped matches the messages typed into a pane and not confirmed.
const msgTyped = `status = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL`

// messageColumns are the columns of a message in scanMessage's order.
// RETURNING takes no alias, so they are unqualified.
const messageColumns = `
  n, id, text, kind, sender, sender_session, target_session, target_task, target_repo,
  suggested_target, suggested_kind, runner_up, confidence, routed_by, auto_eligible, status,
  delivered_via, result_ref, created_at, hold_until, delivered_at, confirmed_at`

// selectMessages is the column list every reader of messages uses.
const selectMessages = `
SELECT` + messageColumns + `
FROM messages`

// scanMessage reads one row of messageColumns.
func scanMessage(sc scanner) (model.Message, error) {
	var m model.Message
	var conf sql.NullFloat64
	var created, hold, delivered, confirmed sql.NullString
	err := sc.Scan(&m.N, &m.ID, &m.Text, &m.Kind, &m.Sender, &m.SenderSession, &m.TargetSession, &m.TargetTask,
		&m.TargetRepo, &m.SuggestedTarget, &m.SuggestedKind, &m.RunnerUp, &conf, &m.RoutedBy, &m.AutoEligible,
		&m.Status, &m.DeliveredVia, &m.ResultRef, &created, &hold, &delivered, &confirmed)
	if err != nil {
		return model.Message{}, err
	}
	m.Confidence, m.HasConfidence = conf.Float64, conf.Valid
	for _, f := range []struct {
		dst *time.Time
		src sql.NullString
	}{
		{&m.CreatedAt, created},
		{&m.HoldUntil, hold},
		{&m.DeliveredAt, delivered},
		{&m.ConfirmedAt, confirmed},
	} {
		v, err := parseTime(f.src)
		if err != nil {
			return model.Message{}, fmt.Errorf("message %s: undecodable time: %w", m.ID, err)
		}
		*f.dst = v
	}
	return m, nil
}

// queryMessages runs q through db and scans every row.
func queryMessages(ctx context.Context, db dbtx, q string, args ...any) ([]model.Message, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

// getMessage loads message id through q, and false if there is none.
func getMessage(ctx context.Context, q dbtx, id string) (model.Message, bool, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, selectMessages+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Message{}, false, nil
	}
	if err != nil {
		return model.Message{}, false, err
	}
	return m, true, nil
}

// confidence is m's confidence for its column: NULL when no model was asked.
func confidence(m model.Message) any {
	if !m.HasConfidence {
		return nil
	}
	return m.Confidence
}

// checkCreated refuses what CreateMessage may not store: a status other
// than queued, held or canceled, or a held message with no hold_until (the
// sweep would never cancel it).
func checkCreated(m model.Message) error {
	switch {
	case m.Status != model.MsgQueued && m.Status != model.MsgHeld && m.Status != model.MsgCanceled:
		return fmt.Errorf("a message is created queued, held or canceled, not %q", m.Status)
	case m.Status == model.MsgHeld && m.HoldUntil.IsZero():
		return errors.New("a held message needs its hold_until")
	}
	return nil
}

// CreateMessage stores m as a new message in one write transaction, with the
// next id (M-<n>, plan section 4.2), and returns the stored row. Its status
// must be queued, held (with HoldUntil) or canceled; anything else is an
// error and stores nothing. m's N, ID and delivery columns (DeliveredVia,
// ResultRef, DeliveredAt, ConfirmedAt) are ignored: only a transition sets
// them. Confidence is stored only when HasConfidence is set.
func (s *Store) CreateMessage(ctx context.Context, m model.Message) (model.Message, error) {
	if err := checkCreated(m); err != nil {
		return model.Message{}, failMessage("create message", "", err)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Message{}, failMessage("create message", "", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The write lock makes the subselect agree with the rowid n the row
	// receives; rows are never deleted, so an id is never reused.
	stored, err := scanMessage(tx.QueryRowContext(ctx, `
INSERT INTO messages (
  id, text, kind, sender, sender_session, target_session, target_task, target_repo,
  suggested_target, suggested_kind, runner_up, confidence, routed_by, auto_eligible, status,
  created_at, hold_until
) VALUES (
  'M-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM messages), ?, ?, ?, ?, ?, ?, ?,
  ?, ?, ?, ?, ?, ?, ?, ?, ?
) RETURNING`+messageColumns,
		m.Text, m.Kind, m.Sender, m.SenderSession, m.TargetSession, m.TargetTask, m.TargetRepo,
		m.SuggestedTarget, m.SuggestedKind, m.RunnerUp, confidence(m), m.RoutedBy, m.AutoEligible, m.Status,
		formatTime(m.CreatedAt), formatTime(m.HoldUntil)))
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return model.Message{}, failMessage("create message", "", err)
	}
	return stored, nil
}

// GetMessage returns the message with the given id, and false if there is
// none. It returns any message, whatever its status or path.
func (s *Store) GetMessage(ctx context.Context, id string) (model.Message, bool, error) {
	m, found, err := getMessage(ctx, s.db, id)
	if err != nil {
		return model.Message{}, false, failMessage("get message", id, err)
	}
	return m, found, nil
}

// The queries of ListMessages: the number of statuses, the statuses as a
// JSON array, then for the newest-first form the limit.
const (
	listMessages = selectMessages + `
WHERE (? = 0 OR status IN (SELECT value FROM json_each(?)))`
	listMessagesAll    = listMessages + ` ORDER BY n`
	listMessagesNewest = listMessages + ` ORDER BY n DESC LIMIT ?`
)

// ListMessages returns the messages f selects: in id order, or with a
// positive Limit the newest Limit of them, newest first.
func (s *Store) ListMessages(ctx context.Context, f MessageFilter) ([]model.Message, error) {
	statuses := f.Statuses
	if statuses == nil {
		statuses = []model.MessageStatus{}
	}
	js, err := json.Marshal(statuses)
	if err != nil {
		return nil, failMessage("list messages", "", err)
	}
	q, args := listMessagesAll, []any{len(f.Statuses), string(js)}
	if f.Limit > 0 {
		q, args = listMessagesNewest, append(args, f.Limit)
	}
	out, err := queryMessages(ctx, s.db, q, args...)
	if err != nil {
		return nil, failMessage("list messages", "", err)
	}
	return out, nil
}

// Guards for transition: why message m may not make that transition, or
// nil.
func heldOnly(m model.Message) error {
	if m.Status != model.MsgHeld {
		return ErrNotHeld
	}
	return nil
}

func queuedOnly(m model.Message) error {
	if m.Status != model.MsgQueued {
		return ErrNotQueued
	}
	return nil
}

// appliable also refuses an instruction, which is delivered to its session,
// never applied.
func appliable(m model.Message) error {
	if err := queuedOnly(m); err != nil {
		return err
	}
	if m.Kind == model.KindInstruction {
		return errors.New("an instruction is delivered, not applied")
	}
	return nil
}

// transition changes message id in one write transaction, which takes the
// write lock before reading: it loads the message (ErrNoMessage when there
// is none), returns guard's error if guard refuses it, and otherwise runs
// an UPDATE with set as its SET clause (its parameters are args) and returns
// the stored row.
func (s *Store) transition(ctx context.Context, op, id string, guard func(model.Message) error,
	set string, args ...any,
) (model.Message, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Message{}, failMessage(op, id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, found, err := getMessage(ctx, tx, id)
	switch {
	case err != nil:
	case !found:
		err = ErrNoMessage
	default:
		err = guard(cur)
	}
	var m model.Message
	if err == nil {
		m, err = scanMessage(tx.QueryRowContext(ctx, `UPDATE messages SET `+set+` WHERE n = ? RETURNING`+messageColumns,
			append(args, cur.N)...))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return model.Message{}, failMessage(op, id, err)
	}
	return m, nil
}

// ReleaseMessage queues message id at the end of its hold: held -> queued,
// in one write transaction. ErrNotHeld when it is not held (undone, or
// canceled by a sweep), ErrNoMessage when there is none.
func (s *Store) ReleaseMessage(ctx context.Context, id string) (model.Message, error) {
	return s.transition(ctx, "release message", id, heldOnly, `status = 'queued'`)
}

// UndoMessage undoes held message id: held -> undone, in one write
// transaction. ErrNotHeld when it is not held, ErrNoMessage when there is
// none.
func (s *Store) UndoMessage(ctx context.Context, id string) (model.Message, error) {
	return s.transition(ctx, "undo message", id, heldOnly, `status = 'undone'`)
}

// CancelMessage cancels queued message id: queued -> canceled, in one write
// transaction. ErrNotQueued when it is not queued (a claim delivered it
// first, or it is held), ErrNoMessage when there is none.
func (s *Store) CancelMessage(ctx context.Context, id string) (model.Message, error) {
	return s.transition(ctx, "cancel message", id, queuedOnly, `status = 'canceled'`)
}

// SweepHeld cancels every held message whose hold ended at least 30 s
// before now (hold_until + 30 s <= now): the say process that held it died.
// It returns how many it canceled.
func (s *Store) SweepHeld(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE messages SET status = 'canceled' WHERE status = 'held' AND hold_until <= ?`,
		formatTime(now.Add(-30*time.Second)))
	if err != nil {
		return 0, failMessage("sweep held messages", "", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, failMessage("sweep held messages", "", err)
	}
	return int(n), nil
}

// ApplyMessage records that queued rule or task message id became
// resultRef (the D-<n> of the rule or T-<n> of the task) at now: queued ->
// delivered by applied, in one write transaction. ErrNotQueued when it is
// not queued, ErrNoMessage when there is none; an instruction is an error
// and changes nothing.
func (s *Store) ApplyMessage(ctx context.Context, id, resultRef string, now time.Time) (model.Message, error) {
	return s.transition(ctx, "apply message", id, appliable,
		`status = 'delivered', delivered_via = 'applied', result_ref = ?, delivered_at = ?`, resultRef, formatTime(now))
}

// MessageClaim is ClaimMessageTyping's result, as TypingClaim is for an
// answer: the message and its target session as stored (zero when absent),
// why it was not claimed (TypingClaimed when it was), and for
// TypingUnsubmitted the id (D-<n> or M-<n>) of the typed line, not
// confirmed yet, that blocks typing into the session.
type MessageClaim struct {
	Message     model.Message
	Session     model.Session
	Refusal     TypingRefusal
	Unconfirmed string // the id (D-<n> or M-<n>) of the typed line that blocks typing
}

// ClaimMessageTyping claims message id for typing into its target session's
// pane at now, in one write transaction and in ClaimTyping's order: the
// message must be a queued instruction with a target session
// (TypingNotQueued otherwise, also when there is no such message); the
// session must pass typingRefusal; no other live session may share its pane
// (Ruling 73); and no line typed into it earlier, answer or message, may be
// unconfirmed (Ruling 86). Then the message is marked delivered by
// send_keys at now, unconfirmed, and returned with TypingClaimed. Otherwise
// nothing changes and the claim says why.
//
// If the process dies after this claim and before typing, the message stays
// typed and unconfirmed: it is never typed again and the Stop hook does not
// take it; the recipient's check_messages returns and confirms it.
func (s *Store) ClaimMessageTyping(ctx context.Context, id string, now time.Time) (MessageClaim, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return MessageClaim{}, failMessage("claim typing of", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var c MessageClaim
	m, found, err := getMessage(ctx, tx, id)
	typable := found && m.Status == model.MsgQueued && m.Kind == model.KindInstruction && m.TargetSession != ""
	c.Message, c.Refusal = m, TypingNotQueued
	if err == nil && found && m.TargetSession != "" {
		c.Session, _, err = getSession(ctx, tx, m.TargetSession)
	}
	if err == nil && typable {
		c.Refusal, c.Unconfirmed, err = sessionRefusal(ctx, tx, c.Session)
	}
	if err == nil && c.Refusal == TypingClaimed {
		c.Message, err = scanMessage(tx.QueryRowContext(ctx, `
UPDATE messages SET status = 'delivered', delivered_via = 'send_keys', delivered_at = ?, confirmed_at = NULL
WHERE n = ? RETURNING`+messageColumns, formatTime(now), m.N))
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		return MessageClaim{}, failMessage("claim typing of", id, err)
	}
	return c, nil
}

// RequeueMessage undoes a typing claim whose typing failed: a message
// delivered by send_keys and not confirmed is queued again, with no delivery
// path or time. Any other message, or none, is left as it is: one the
// recipient has read with check_messages since the claim is delivered.
func (s *Store) RequeueMessage(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE messages SET status = 'queued', delivered_via = '', delivered_at = NULL
WHERE id = ? AND `+msgTyped, id)
	if err != nil {
		return failMessage("requeue message", id, err)
	}
	return nil
}

// CheckMessages is check_messages from session callerSession at now, in one
// write transaction: its queued instructions are delivered by check and
// confirmed at now, its typed, unconfirmed ones are confirmed at now, and
// it returns, oldest first, the ones it delivered and every instruction
// ever typed into the session, confirmed or not (Ruling 105: a typed line
// may have been simplified, so its full wording stays readable here). An
// empty callerSession gets nothing.
//
// If the process dies after this commits and before the tool result is
// written, the queued ones it took are lost to the agent; the typed ones
// are returned again by the next call.
func (s *Store) CheckMessages(ctx context.Context, callerSession string, now time.Time) ([]model.Message, error) {
	if callerSession == "" {
		return nil, nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, failMessage("check messages for", callerSession, err)
	}
	defer func() { _ = tx.Rollback() }()
	at := formatTime(now)
	var claimed []int64
	claimed, err = queryNs(ctx, tx, `
UPDATE messages SET status = 'delivered', delivered_via = 'check', delivered_at = ?, confirmed_at = ?
WHERE target_session = ? AND status = 'queued' AND kind = 'instruction' RETURNING n`, at, at, callerSession)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE messages SET confirmed_at = ? WHERE target_session = ? AND `+msgTyped,
			at, callerSession)
	}
	var out []model.Message
	if err == nil {
		out, err = queryMessages(ctx, tx, selectMessages+`
WHERE n IN (SELECT value FROM json_each(?))
   OR (target_session = ? AND status = 'delivered' AND delivered_via = 'send_keys')
ORDER BY n`, jsonNs(claimed), callerSession)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return nil, failMessage("check messages for", callerSession, err)
	}
	return out, nil
}

// queryNs runs q, which returns one column n, and collects it.
func queryNs(ctx context.Context, q dbtx, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

// jsonNs is ns as a JSON array, for json_each; never "null".
func jsonNs(ns []int64) string {
	if len(ns) == 0 {
		return "[]"
	}
	js, _ := json.Marshal(ns) // an []int64 always marshals
	return string(js)
}

// The meta keys of routing's two flags (plan section 4.1).
const (
	routingEarnedKey  = "routing_earned_at"
	routingTrippedKey = "routing_tripped_at"
)

// RouteLabels returns the confirmed and overridden routings whose
// confidence is at least minConfidence, newest first, at most limit of them
// (none for a limit of 0 or less). Accepted is true for a confirmed one.
func (s *Store) RouteLabels(ctx context.Context, minConfidence float64, limit int) ([]model.RouteLabel, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT confidence, routed_by = 'confirmed' FROM messages
WHERE routed_by IN ('confirmed', 'overridden') AND confidence >= ?
ORDER BY n DESC LIMIT ?`, minConfidence, limit)
	if err != nil {
		return nil, failMessage("read route labels", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.RouteLabel
	for rows.Next() {
		var l model.RouteLabel
		if err := rows.Scan(&l.Confidence, &l.Accepted); err != nil {
			return nil, failMessage("read route labels", "", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, failMessage("read route labels", "", err)
	}
	return out, nil
}

// AutoEligibleCount counts the messages that passed every auto-send guard
// but sampling.
func (s *Store) AutoEligibleCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE auto_eligible = 1`).Scan(&n); err != nil {
		return 0, failMessage("count auto-eligible messages", "", err)
	}
	return n, nil
}

// RoutingState returns when routing earned auto-send and when it tripped
// off, each the zero time when not set.
func (s *Store) RoutingState(ctx context.Context) (earnedAt, trippedAt time.Time, err error) {
	var earned, tripped sql.NullString
	err = s.db.QueryRowContext(ctx, `
SELECT (SELECT value FROM meta WHERE key = ?), (SELECT value FROM meta WHERE key = ?)`,
		routingEarnedKey, routingTrippedKey).Scan(&earned, &tripped)
	if err == nil {
		earnedAt, err = parseTime(earned)
	}
	if err == nil {
		trippedAt, err = parseTime(tripped)
	}
	if err != nil {
		return time.Time{}, time.Time{}, failMessage("read routing state", "", err)
	}
	return earnedAt, trippedAt, nil
}

// setOnce writes key = now in meta unless key is set.
func (s *Store) setOnce(ctx context.Context, key string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`,
		key, formatTime(now))
	if err != nil {
		return failMessage("set", key, err)
	}
	return nil
}

// SetRoutingEarned records that routing earned auto-send at now, unless it
// is already recorded.
func (s *Store) SetRoutingEarned(ctx context.Context, now time.Time) error {
	return s.setOnce(ctx, routingEarnedKey, now)
}

// SetRoutingTripped records that auto-send tripped off at now, unless it is
// already recorded.
func (s *Store) SetRoutingTripped(ctx context.Context, now time.Time) error {
	return s.setOnce(ctx, routingTrippedKey, now)
}

// ResetRouting clears both routing flags.
func (s *Store) ResetRouting(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key IN (?, ?)`, routingEarnedKey, routingTrippedKey)
	if err != nil {
		return failMessage("reset routing", "", err)
	}
	return nil
}

// badgeExtras is BadgeExtras' one statement: the instructions not yet
// received (by the messages_undelivered index, then one session lookup
// each) by a session that exists and is neither ended nor stale, and
// whether auto-send has tripped off.
const badgeExtras = `
SELECT
  (SELECT count(*) FROM messages JOIN sessions s ON s.id = messages.target_session
   WHERE ` + msgUndelivered + ` AND messages.kind = 'instruction' AND COALESCE(s.state, '') NOT IN ('ended', 'stale')),
  EXISTS (SELECT 1 FROM meta WHERE key = '` + routingTrippedKey + `')`

// BadgeExtras counts, in one statement, the instructions a live session has
// not yet received (queued, or typed and unconfirmed), for the badge's Q,
// and says whether auto-send is off (meta routing_tripped_at exists).
func (s *Store) BadgeExtras(ctx context.Context) (undeliveredMessages int, autoOff bool, err error) {
	if err = s.db.QueryRowContext(ctx, badgeExtras).Scan(&undeliveredMessages, &autoOff); err != nil {
		return 0, false, failMessage("count messages for the badge", "", err)
	}
	return undeliveredMessages, autoOff, nil
}

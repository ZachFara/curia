package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
)

// Tx is the store inside one write transaction: the operations a function
// passed to UpdateSessionTx may run on that transaction. It is valid only
// while the function runs.
type Tx struct {
	tx *sql.Tx
}

// UpdateSessionTx is UpdateSession with fn given a *Tx on the same write
// transaction and allowed to fail. In one BEGIN IMMEDIATE transaction it
// loads session id (the zero Session and found=false if there is none),
// calls fn and upserts what fn returns. An error from fn rolls everything
// back, including what fn did through the Tx, and is returned as it is.
// If fn returns a Session whose ID is not id, nothing is written and an
// error is returned. A caller that waits longer than busy_timeout for the
// lock gets an error and fn is not called.
//
// fn must use only the Tx, never the Store: the transaction holds the
// write lock, so a write through the Store would wait on it until
// busy_timeout. fn should run nothing slow: every writer waits on it.
func (s *Store) UpdateSessionTx(ctx context.Context, id string,
	fn func(tx *Tx, s model.Session, found bool) (model.Session, error),
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: update session %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, found, err := getSession(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("store: update session %s: %w", id, err)
	}
	next, err := fn(&Tx{tx: tx}, cur, found)
	if err != nil {
		return err
	}
	if next.ID != id {
		return fmt.Errorf("store: update session %s: fn returned a session with ID %q", id, next.ID)
	}
	if err = upsert(ctx, tx, next); err != nil {
		return fmt.Errorf("store: update session %s: %w", id, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: update session %s: commit: %w", id, err)
	}
	return nil
}

// claimCap is the most answers one ClaimDeliveries takes: the most that
// one decision.StopReason is sure to name, so a claimed answer is never
// left out of the reason that delivers it. The worst case is a first
// answer whose line does not fit: then every answer is named only in the
// "Also answered: D-1, D-2. Call check_decision for each." line, which
// gets the 4000 runes less the 49-rune header and its line break, 3950.
// Its fixed words take 46, leaving 3904 for the ids and their ", "
// separators. An id is at most 21 runes (D- and the 19 digits of the
// largest int64), so k ids need 23k-2 runes, and k = 169 is the most that
// fit. Any answers past it stay queued for the next Stop. The tests check
// this number against decision.StopReason itself.
const claimCap = 169

// returnedDecision is decisionColumns without the alias, which SQLite's
// RETURNING does not take. The two lists must agree; the claim tests
// compare every returned field with the stored row.
const returnedDecision = `
  n, id, session_id, task_id, repo, question, context, options,
  recommendation, rationale, reversibility, mode, assumption, scope, tags, status,
  answer_key, answer, answer_reasoning, answered_via, precedent_of, differs_from,
  differs_why, withdraw_reason, superseded_by, args_hash, delivery, delivered_via,
  created_at, expires_at, answered_at, closed_at, superseded_at, delivered_at, confirmed_at`

// claimDeliveries marks the session's oldest claimCap queued answers
// delivered by the Stop hook and returns them. Parameters: now, the
// session id, claimCap.
const claimDeliveries = `
UPDATE decisions SET delivery = 'delivered', delivered_via = 'stop_hook', delivered_at = ?
WHERE n IN (SELECT n FROM decisions WHERE session_id = ? AND delivery = 'queued' ORDER BY n LIMIT ?)
RETURNING` + returnedDecision

// ClaimDeliveries claims, for curia hook Stop, the queued answers of
// session sessionID, oldest first and at most claimCap of them (the rest
// stay queued for the next Stop), and returns them in id order, marked
// delivered by stop_hook at now. It runs inside a savepoint: on any error
// the savepoint is rolled back and nothing is claimed. An empty sessionID
// claims nothing.
func (t *Tx) ClaimDeliveries(ctx context.Context, sessionID string, now time.Time) ([]model.Decision, error) {
	if sessionID == "" {
		return nil, nil
	}
	var out []model.Decision
	err := t.savepoint(ctx, "claim_deliveries", func() error {
		var err error
		out, err = claim(ctx, t.tx, sessionID, now)
		return err
	})
	if err != nil {
		return nil, fail("claim deliveries for", sessionID, err)
	}
	// SQLite promises no order for RETURNING rows; no test can see this
	// sort (the plan returns id order today), so it is not dead code.
	slices.SortFunc(out, func(a, b model.Decision) int { return cmp.Compare(a.N, b.N) })
	return out, nil
}

// savepoint runs fn inside savepoint name of the transaction: released when
// fn succeeds, rolled back and released when it fails, so on any error
// nothing fn did is left in the transaction.
func (t *Tx) savepoint(ctx context.Context, name string, fn func() error) error {
	if _, err := t.tx.ExecContext(ctx, `SAVEPOINT `+name); err != nil {
		return err
	}
	// The savepoint is closed even when ctx is done, so a claim is never
	// left in the transaction after an error.
	end := context.WithoutCancel(ctx)
	err := fn()
	if err == nil {
		if _, err = t.tx.ExecContext(end, `RELEASE `+name); err == nil {
			return nil
		}
	}
	_, rbErr := t.tx.ExecContext(end, `ROLLBACK TO `+name)
	_, relErr := t.tx.ExecContext(end, `RELEASE `+name)
	return errors.Join(err, rbErr, relErr)
}

// claim runs claimDeliveries and scans the rows it returns.
func claim(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time) ([]model.Decision, error) {
	rows, err := tx.QueryContext(ctx, claimDeliveries, formatTime(now), sessionID, claimCap)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []model.Decision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, rows.Close()
}

// OpenBlocking counts the blocking tickets of session sessionID open at
// now. An empty sessionID has none.
func (t *Tx) OpenBlocking(ctx context.Context, sessionID string, now time.Time) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	var n int
	err := t.tx.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions d WHERE d.session_id = ? AND d.mode = 'blocking' AND `+openAt,
		sessionID, formatTime(now)).Scan(&n)
	if err != nil {
		return 0, fail("count open blocking for", sessionID, err)
	}
	return n, nil
}

// stopState is StopState's one statement: the session's open blocking
// tickets, whether an answer of it is queued (both by the
// decisions_session_status index on session_id) and whether an instruction
// for it is queued (by the messages_undelivered index). Parameters: the
// session id, now, the session id, the session id.
const stopState = `
SELECT
  (SELECT count(*) FROM decisions d WHERE d.session_id = ? AND d.mode = 'blocking' AND ` + openAt + `),
  EXISTS (SELECT 1 FROM decisions WHERE session_id = ? AND delivery = 'queued'),
  EXISTS (SELECT 1 FROM messages WHERE target_session = ? AND status = 'queued' AND kind = 'instruction')`

// StopState is what curia hook Stop reads before it claims anything.
type StopState struct {
	OpenBlocking      int
	Answers, Messages bool // something of that kind is queued for the session
}

// StopState is what curia hook Stop reads before it claims anything, in one
// small statement: the number of blocking tickets of session sessionID open
// at now (as OpenBlocking counts them), whether an answer of it is queued
// (ClaimDeliveries has something to claim) and whether an instruction for
// it is queued (ClaimMessages has something to claim). The hook runs a
// claim only when its kind is queued, so the common Stop never prepares a
// claim's UPDATE ... RETURNING. A claim changes only deliveries, never
// which tickets are open, so the count is the same before and after a
// claim in the same transaction. A held, canceled or typed message is not
// queued. An empty sessionID has nothing.
func (t *Tx) StopState(ctx context.Context, sessionID string, now time.Time) (StopState, error) {
	var st StopState
	if sessionID == "" {
		return st, nil
	}
	err := t.tx.QueryRowContext(ctx, stopState, sessionID, formatTime(now), sessionID, sessionID).
		Scan(&st.OpenBlocking, &st.Answers, &st.Messages)
	if err != nil {
		return StopState{}, fail("read stop state for", sessionID, err)
	}
	return st, nil
}

// ClaimMessages claims, for curia hook Stop, the queued instructions of
// session sessionID that the reason can hold: it reads the oldest
// message.MaxPerStop of them, keeps the first message.Fit(..., budget)
// (budget is the runes left in the reason), marks those delivered by
// stop_hook and confirmed at now (the blocked stop hands them over), and
// returns them in id order. The rest stay queued for the next Stop. It runs
// inside a savepoint: on any error nothing is claimed. An empty sessionID
// claims nothing.
//
// The claim commits with the hook's session update. If the hook dies after
// the commit and before it writes its output, the claimed messages are lost
// to the agent (at most once, as for answers).
func (t *Tx) ClaimMessages(ctx context.Context, sessionID string, now time.Time, budget int) ([]model.Message, error) {
	if sessionID == "" {
		return nil, nil
	}
	var out []model.Message
	err := t.savepoint(ctx, "claim_messages", func() error {
		var err error
		out, err = claimMessages(ctx, t.tx, sessionID, now, budget)
		return err
	})
	if err != nil {
		return nil, fail("claim messages for", sessionID, err)
	}
	return out, nil
}

// claimMessages is ClaimMessages inside its savepoint.
func claimMessages(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time, budget int) ([]model.Message, error) {
	ms, err := queryMessages(ctx, tx, selectMessages+`
WHERE target_session = ? AND status = 'queued' AND kind = 'instruction' ORDER BY n LIMIT ?`,
		sessionID, message.MaxPerStop)
	if err != nil {
		return nil, err
	}
	k := message.Fit(ms, budget)
	if k == 0 {
		return nil, nil
	}
	ns := make([]int64, k)
	for i, m := range ms[:k] {
		ns[i] = m.N
	}
	at := formatTime(now)
	out, err := queryMessages(ctx, tx, `
UPDATE messages SET status = 'delivered', delivered_via = 'stop_hook', delivered_at = ?, confirmed_at = ?
WHERE n IN (SELECT value FROM json_each(?)) AND status = 'queued'
RETURNING`+messageColumns, at, at, jsonNs(ns))
	if err != nil {
		return nil, err
	}
	// SQLite promises no order for RETURNING rows.
	slices.SortFunc(out, func(a, b model.Message) int { return cmp.Compare(a.N, b.N) })
	return out, nil
}

// ConfirmTypedMessages confirms, for curia hook UserPromptSubmit at now, the
// messages typed into session sessionID no later than now and not yet
// confirmed that prompt names (message.Names: "Message M-<n> ", never in a
// prompt Claude Code built itself, Ruling 102), inside the session's write
// transaction, and returns how many. An empty sessionID confirms nothing.
func (t *Tx) ConfirmTypedMessages(ctx context.Context, sessionID, prompt string, now time.Time) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	at := formatTime(now)
	// msgUndelivered, word for word, lets SQLite read the partial index
	// messages_undelivered; msgTyped alone would scan the table.
	typed, err := queryMessages(ctx, t.tx, selectMessages+`
WHERE target_session = ? AND `+msgUndelivered+` AND `+msgTyped+` AND delivered_at <= ?`, sessionID, at)
	if err != nil {
		return 0, fail("confirm typed messages for", sessionID, err)
	}
	var named []int64
	for _, m := range typed {
		if message.Names(prompt, m.ID) {
			named = append(named, m.N)
		}
	}
	if len(named) == 0 {
		return 0, nil
	}
	res, err := t.tx.ExecContext(ctx, `UPDATE messages SET confirmed_at = ? WHERE n IN (SELECT value FROM json_each(?))`,
		at, jsonNs(named))
	if err != nil {
		return 0, fail("confirm typed messages for", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fail("confirm typed messages for", sessionID, err)
	}
	return n, nil
}

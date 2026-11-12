package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// Delivery claims (plan section 4.9). Each is one write transaction that
// claims an answer only while its delivery is 'queued', so of the three
// paths at most one ever delivers it.

// TypingRefusal says why ClaimTyping did not claim an answer for typing.
// TypingClaimed, the zero value, means it did.
type TypingRefusal int

// The reasons ClaimTyping gives.
const (
	// TypingClaimed: the answer was claimed and is to be typed.
	TypingClaimed TypingRefusal = iota
	// TypingNotQueued: the decision is missing, has no answer to deliver,
	// or another path delivered it first.
	TypingNotQueued
	// TypingNoPane: the session has no row or no pane, or is stale or ended.
	TypingNoPane
	// TypingWorking: the session is working; its Stop hook delivers.
	TypingWorking
	// TypingWaiting: the session is at a prompt of its own.
	TypingWaiting
	// TypingSubagents: the session has subagents running, one of which may
	// have a permission dialog on screen (Ruling 84).
	TypingSubagents
	// TypingSharedPane: another live session is bound to the pane (Ruling 73).
	TypingSharedPane
	// TypingUnsubmitted: an answer typed into the session earlier is not
	// confirmed yet, so its line may still sit unsubmitted in the prompt, or
	// be lost (Rulings 81 and 86).
	TypingUnsubmitted
)

// typingRefusal is why curia decide may not type into sess, or
// TypingClaimed when it may: sess has a pane, is idle or needs_decision
// (never working or waiting, where typing could answer a permission prompt,
// nor stale or ended) and has no subagent running.
func typingRefusal(sess model.Session) TypingRefusal {
	switch {
	case sess.State == model.StateWorking:
		return TypingWorking
	case sess.State == model.StateWaiting:
		return TypingWaiting
	case sess.Pane == "" || (sess.State != model.StateIdle && sess.State != model.StateNeedsDecision):
		return TypingNoPane
	case sess.SubagentsActive != 0:
		return TypingSubagents
	}
	return TypingClaimed
}

// unconfirmed reports whether an answer typed into sess (delivered by
// send_keys) is not yet confirmed: no prompt naming it has been submitted
// and the agent has not read it with check_decision (Ruling 86). Its line
// may still sit in the prompt, or be lost, and a second line would be glued
// to it (Ruling 81).
func unconfirmed(ctx context.Context, tx *sql.Tx, sess model.Session) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM decisions
WHERE session_id = ? AND delivery = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL`,
		sess.ID).Scan(&n)
	return n > 0, err
}

// sharesPane reports whether a session row other than sess is bound to
// sess's pane in a state other than ended or stale (a row with no state
// counts as live). A nested session started from another session's pane
// inherits that pane, and one that died without SessionEnd stays idle or
// needs_decision while the outer session is alive there, perhaps at a
// permission prompt (Ruling 73).
func sharesPane(ctx context.Context, tx *sql.Tx, sess model.Session) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
SELECT count(*) FROM sessions
WHERE tmux_pane = ? AND id != ? AND COALESCE(state, '') NOT IN (?, ?)`,
		sess.Pane, sess.ID, model.StateEnded, model.StateStale).Scan(&n)
	return n > 0, err
}

// ClaimTyping claims decision id for typing into its session's pane at
// now, in one write transaction: the decision must be queued; its session
// row must exist, have a pane, be idle or needs_decision and have no
// subagent running (Ruling 84); no other session row that is neither ended
// nor stale may be bound to that pane (Ruling 73); and no answer typed into
// the session earlier may be unconfirmed (Rulings 81 and 86). Then the
// decision is marked delivered by send_keys, unconfirmed, and returned with
// TypingClaimed and the session. Otherwise nothing changes, and the refusal
// says why, with both rows as stored (zero when absent).
func (s *Store) ClaimTyping(ctx context.Context, id string, now time.Time) (d model.Decision, sess model.Session, why TypingRefusal, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, model.Session{}, 0, fail("claim typing", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	d, found, err := getDecision(ctx, tx, id)
	if err == nil && found && d.SessionID != "" {
		sess, _, err = getSession(ctx, tx, d.SessionID)
	}
	if err == nil {
		why, err = claimRefusal(ctx, tx, d, sess)
	}
	if err != nil {
		return model.Decision{}, model.Session{}, 0, fail("claim typing", id, err)
	}
	if why != TypingClaimed {
		return d, sess, why, nil
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE decisions SET delivery = 'delivered', delivered_via = 'send_keys', delivered_at = ? WHERE n = ?`,
		formatTime(now), d.N); err != nil {
		return model.Decision{}, model.Session{}, 0, fail("claim typing", id, err)
	}
	if d, err = commitRead(ctx, tx, id); err != nil {
		return model.Decision{}, model.Session{}, 0, fail("claim typing", id, err)
	}
	return d, sess, TypingClaimed, nil
}

// claimRefusal is why ClaimTyping may not claim d for typing into sess
// inside tx, or TypingClaimed.
func claimRefusal(ctx context.Context, tx *sql.Tx, d model.Decision, sess model.Session) (TypingRefusal, error) {
	if d.Delivery != model.DeliveryQueued {
		return TypingNotQueued, nil
	}
	if why := typingRefusal(sess); why != TypingClaimed {
		return why, nil
	}
	if shared, err := sharesPane(ctx, tx, sess); err != nil || shared {
		return TypingSharedPane, err
	}
	if typed, err := unconfirmed(ctx, tx, sess); err != nil || typed {
		return TypingUnsubmitted, err
	}
	return TypingClaimed, nil
}

// RequeueDelivery undoes a typing claim whose typing failed: a decision
// delivered by send_keys is queued again, with no delivery path or time.
// Any other decision, or none, is left as it is.
func (s *Store) RequeueDelivery(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE decisions SET delivery = 'queued', delivered_via = '', delivered_at = NULL, confirmed_at = NULL
WHERE id = ? AND delivery = 'delivered' AND delivered_via = 'send_keys'`, id)
	if err != nil {
		return fmt.Errorf("store: requeue delivery of %s: %w", id, err)
	}
	return nil
}

// CheckDecision returns decision id for check_decision from callerSession
// at now, and false when there is none, in one write transaction. When
// callerSession is the decision's non-empty session, a decided, queued
// answer is first marked delivered by check, so the answer the agent reads
// is never delivered again by another path, and an answer typed and not yet
// confirmed is confirmed (Ruling 86).
func (s *Store) CheckDecision(ctx context.Context, id, callerSession string, now time.Time) (model.Decision, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, false, fail("check decision", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
UPDATE decisions AS d SET delivery = 'delivered', delivered_via = 'check', delivered_at = ?
WHERE d.id = ? AND d.delivery = 'queued' AND `+decided+` AND d.session_id = ? AND d.session_id != ''`,
		formatTime(now), id, callerSession)
	if err == nil {
		_, err = tx.ExecContext(ctx, `
UPDATE decisions SET confirmed_at = ?
WHERE id = ? AND delivery = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL
  AND session_id = ? AND session_id != ''`,
			formatTime(now), id, callerSession)
	}
	if err != nil {
		return model.Decision{}, false, fail("check decision", id, err)
	}
	d, found, err := getDecision(ctx, tx, id)
	if err == nil && found {
		err = tx.Commit()
	}
	if err != nil {
		return model.Decision{}, false, fail("check decision", id, err)
	}
	return d, found, nil
}

// typedNames matches the decisions a submitted prompt names: "Decision D-n"
// followed by a space (an answer's wording) or a colon (a retraction's),
// so D-1 is not taken for D-12. Its parameters are the prompt, twice.
const typedNames = `(instr(?, 'Decision ' || id || ' ') > 0 OR instr(?, 'Decision ' || id || ':') > 0)`

// ConfirmTyped confirms, for curia hook UserPromptSubmit at now, the answers
// typed into session sessionID no later than now that prompt names
// (Ruling 86), in one statement, and returns how many. An empty sessionID
// confirms nothing.
func (s *Store) ConfirmTyped(ctx context.Context, sessionID, prompt string, now time.Time) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE decisions SET confirmed_at = ?
WHERE session_id = ? AND delivery = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL
  AND delivered_at <= ? AND `+typedNames,
		formatTime(now), sessionID, formatTime(now), prompt, prompt)
	if err != nil {
		return 0, fail("confirm typed answers for", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fail("confirm typed answers for", sessionID, err)
	}
	return n, nil
}

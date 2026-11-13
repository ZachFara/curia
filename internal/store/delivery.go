package store

import (
	"context"
	"database/sql"
	"errors"
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
	// TypingNoPane: the session has no row or no pane, or is stale.
	TypingNoPane
	// TypingEnded: the session has ended, though its pane may live on.
	TypingEnded
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
	case sess.State == model.StateEnded:
		return TypingEnded
	case sess.Pane == "" || (sess.State != model.StateIdle && sess.State != model.StateNeedsDecision):
		return TypingNoPane
	case sess.SubagentsActive != 0:
		return TypingSubagents
	}
	return TypingClaimed
}

// unconfirmed returns the id of the oldest answer typed into sess
// (delivered by send_keys) that is not yet confirmed: no prompt naming it
// has been submitted and the agent has not read it with check_decision
// (Ruling 86), or "" when there is none. Its line may still sit in the
// prompt, or be lost, and a second line would be glued to it (Ruling 81).
func unconfirmed(ctx context.Context, tx *sql.Tx, sess model.Session) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
SELECT id FROM decisions
WHERE session_id = ? AND delivery = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL
ORDER BY n LIMIT 1`,
		sess.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// sharesPane reports whether a session row other than sess is bound to
// sess's pane in a state other than ended or stale (a row with no state
// counts as live). A nested session started from another session's pane
// inherits that pane, and one that died without SessionEnd stays idle or
// needs_decision while the outer session is alive there, perhaps at a
// permission prompt (Ruling 73).
func sharesPane(ctx context.Context, q dbtx, sess model.Session) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `
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
// TypingClaimed and the session. Otherwise nothing changes, and the claim
// says why, with both rows as stored (zero when absent) and, for
// TypingUnsubmitted, the id of the unconfirmed answer that blocks it.
func (s *Store) ClaimTyping(ctx context.Context, id string, now time.Time) (TypingClaim, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return TypingClaim{}, fail("claim typing", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var c TypingClaim
	d, found, err := getDecision(ctx, tx, id)
	if err == nil && found && d.SessionID != "" {
		c.Session, _, err = getSession(ctx, tx, d.SessionID)
	}
	c.Decision = d
	if err == nil {
		c.Refusal, c.Unconfirmed, err = claimRefusal(ctx, tx, d, c.Session)
	}
	if err != nil {
		return TypingClaim{}, fail("claim typing", id, err)
	}
	if c.Refusal != TypingClaimed {
		return c, nil
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE decisions SET delivery = 'delivered', delivered_via = 'send_keys', delivered_at = ? WHERE n = ?`,
		formatTime(now), d.N); err != nil {
		return TypingClaim{}, fail("claim typing", id, err)
	}
	if c.Decision, err = commitRead(ctx, tx, id); err != nil {
		return TypingClaim{}, fail("claim typing", id, err)
	}
	return c, nil
}

// StillTypable re-reads session sessionID for a claim already made, just
// before typing into pane (re-review m3): it is why the session may no
// longer be typed into (state, subagents, pane binding, a shared pane), or
// TypingClaimed. A hook event that landed since the claim is seen. A row
// gone, or bound to another pane now, is TypingNoPane.
func (s *Store) StillTypable(ctx context.Context, sessionID, pane string) (TypingRefusal, error) {
	sess, found, err := getSession(ctx, s.db, sessionID)
	switch {
	case err != nil:
		return 0, fail("recheck typing for", sessionID, err)
	case !found || sess.Pane != pane:
		return TypingNoPane, nil
	}
	if why := typingRefusal(sess); why != TypingClaimed {
		return why, nil
	}
	shared, err := sharesPane(ctx, s.db, sess)
	if err != nil {
		return 0, fail("recheck typing for", sessionID, err)
	}
	if shared {
		return TypingSharedPane, nil
	}
	return TypingClaimed, nil
}

// TypingClaim is what ClaimTyping did with an answer: the decision and its
// session as stored, why it was not claimed (TypingClaimed when it was),
// and for TypingUnsubmitted the id of the typed answer, not confirmed yet,
// that blocks typing into the session (re-review N4).
type TypingClaim struct {
	Decision    model.Decision
	Session     model.Session
	Refusal     TypingRefusal
	Unconfirmed string
}

// claimRefusal is why ClaimTyping may not claim d for typing into sess
// inside tx, or TypingClaimed, and the id of the unconfirmed typed answer
// behind TypingUnsubmitted.
func claimRefusal(ctx context.Context, tx *sql.Tx, d model.Decision, sess model.Session) (TypingRefusal, string, error) {
	if d.Delivery != model.DeliveryQueued {
		return TypingNotQueued, "", nil
	}
	if why := typingRefusal(sess); why != TypingClaimed {
		return why, "", nil
	}
	if shared, err := sharesPane(ctx, tx, sess); err != nil || shared {
		return TypingSharedPane, "", err
	}
	if typed, err := unconfirmed(ctx, tx, sess); err != nil || typed != "" {
		return TypingUnsubmitted, typed, err
	}
	return TypingClaimed, "", nil
}

// RequeueDelivery undoes a typing claim whose typing failed: a decision
// delivered by send_keys and not confirmed is queued again, with no
// delivery path or time. Any other decision, or none, is left as it is: an
// answer the agent has read with check_decision since the claim (re-review
// m1) is delivered.
func (s *Store) RequeueDelivery(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE decisions SET delivery = 'queued', delivered_via = '', delivered_at = NULL
WHERE id = ? AND delivery = 'delivered' AND delivered_via = 'send_keys' AND confirmed_at IS NULL`, id)
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
// (Ruling 86), in one statement inside the session's write transaction, so
// the confirmation is written with the UserPromptSubmit's session update or
// not at all (re-review N4). It returns how many it confirmed. An empty
// sessionID confirms nothing.
func (t *Tx) ConfirmTyped(ctx context.Context, sessionID, prompt string, now time.Time) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	res, err := t.tx.ExecContext(ctx, `
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

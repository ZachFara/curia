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

// typable reports whether curia decide may type into sess: it has a pane
// and is idle or needs_decision (never working or waiting, where typing
// could answer a permission prompt, nor stale or ended).
func typable(sess model.Session) bool {
	return sess.Pane != "" && (sess.State == model.StateIdle || sess.State == model.StateNeedsDecision)
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
// row must exist, have a pane and be idle or needs_decision; and no other
// session row that is neither ended nor stale may be bound to that pane
// (Ruling 73). Then the decision is marked delivered by send_keys and
// returned with ok true and the session. Otherwise nothing changes and ok
// is false, with both rows as stored (zero when absent), so the caller can
// say why: a queued decision whose session is idle or needs_decision in a
// pane was refused because another live session shares the pane.
func (s *Store) ClaimTyping(ctx context.Context, id string, now time.Time) (d model.Decision, sess model.Session, ok bool, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, model.Session{}, false, fail("claim typing", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	d, found, err := getDecision(ctx, tx, id)
	if err == nil && found && d.SessionID != "" {
		sess, _, err = getSession(ctx, tx, d.SessionID)
	}
	if err != nil {
		return model.Decision{}, model.Session{}, false, fail("claim typing", id, err)
	}
	if d.Delivery != model.DeliveryQueued || !typable(sess) {
		return d, sess, false, nil
	}
	shared, err := sharesPane(ctx, tx, sess)
	if err != nil {
		return model.Decision{}, model.Session{}, false, fail("claim typing", id, err)
	}
	if shared {
		return d, sess, false, nil
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE decisions SET delivery = 'delivered', delivered_via = 'send_keys', delivered_at = ? WHERE n = ?`,
		formatTime(now), d.N); err != nil {
		return model.Decision{}, model.Session{}, false, fail("claim typing", id, err)
	}
	if d, err = commitRead(ctx, tx, id); err != nil {
		return model.Decision{}, model.Session{}, false, fail("claim typing", id, err)
	}
	return d, sess, true, nil
}

// RequeueDelivery undoes a typing claim whose typing failed: a decision
// delivered by send_keys is queued again, with no delivery path or time.
// Any other decision, or none, is left as it is.
func (s *Store) RequeueDelivery(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE decisions SET delivery = 'queued', delivered_via = '', delivered_at = NULL
WHERE id = ? AND delivery = 'delivered' AND delivered_via = 'send_keys'`, id)
	if err != nil {
		return fmt.Errorf("store: requeue delivery of %s: %w", id, err)
	}
	return nil
}

// CheckDecision returns decision id for check_decision from callerSession
// at now, and false when there is none, in one write transaction. When the
// decision is decided and queued and callerSession is its non-empty
// session, it is first marked delivered by check, so the answer the agent
// reads is never delivered again by another path.
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

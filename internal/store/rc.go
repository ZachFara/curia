package store

import (
	"context"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// RCClaim is what ClaimRC decided about typing /rc into a session.
type RCClaim struct {
	Session model.Session
	Refusal TypingRefusal
	Blocker string // the unconfirmed typed line behind TypingUnsubmitted; "/rc" for a /rc line
	On      bool   // refused because the session is known to have Remote Control on
}

// ClaimRC is the typing claim for a /rc line (F21), in one write transaction:
// the same rules as an answer or a message (sessionRefusal), plus no /rc line
// in flight or unconfirmed (rc_how typing or unconfirmed), and never a session
// known on (a second /rc opens a panel or disconnects it). Claiming marks the
// session typing, so a second process is refused until SetRC settles it.
func (s *Store) ClaimRC(ctx context.Context, id string, now time.Time) (RCClaim, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return RCClaim{}, fail("claim /rc for", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	var c RCClaim
	c.Session, _, err = getSession(ctx, tx, id)
	if err != nil {
		return RCClaim{}, fail("claim /rc for", id, err)
	}
	switch {
	case c.Session.RC == "on":
		c.On = true
		return c, nil
	case c.Session.RCHow == "typing" || c.Session.RCHow == "unconfirmed":
		c.Refusal, c.Blocker = TypingUnsubmitted, "/rc"
		return c, nil
	}
	if c.Refusal, c.Blocker, err = sessionRefusal(ctx, tx, c.Session); err != nil {
		return RCClaim{}, fail("claim /rc for", id, err)
	}
	if c.Refusal != TypingClaimed {
		return c, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET rc_how = 'typing', rc_checked_at = ? WHERE id = ?`, formatTime(now), id); err != nil {
		return RCClaim{}, fail("claim /rc for", id, err)
	}
	if err = tx.Commit(); err != nil {
		return RCClaim{}, fail("claim /rc for", id, err)
	}
	return c, nil
}

// SetRC records Curia's belief about session id's Remote Control (off,
// pending, on or unknown), how it knows (indicator, typed, unconfirmed) and
// when it looked.
func (s *Store) SetRC(ctx context.Context, id, state, how string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET rc = ?, rc_how = ?, rc_checked_at = ? WHERE id = ?`,
		state, how, formatTime(at), id); err != nil {
		return fail("record /rc state of", id, err)
	}
	return nil
}

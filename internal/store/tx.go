package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

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
  created_at, expires_at, answered_at, closed_at, superseded_at, delivered_at`

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
	if _, err := t.tx.ExecContext(ctx, `SAVEPOINT claim_deliveries`); err != nil {
		return nil, fail("claim deliveries for", sessionID, err)
	}
	// The savepoint is closed even when ctx is done, so a claim is never
	// left in the transaction after an error.
	end := context.WithoutCancel(ctx)
	out, err := claim(ctx, t.tx, sessionID, now)
	if err == nil {
		if _, err = t.tx.ExecContext(end, `RELEASE claim_deliveries`); err == nil {
			// SQLite promises no order for RETURNING rows; no test can see this
			// sort (the plan returns id order today), so it is not dead code.
			slices.SortFunc(out, func(a, b model.Decision) int { return cmp.Compare(a.N, b.N) })
			return out, nil
		}
	}
	_, rbErr := t.tx.ExecContext(end, `ROLLBACK TO claim_deliveries`)
	_, relErr := t.tx.ExecContext(end, `RELEASE claim_deliveries`)
	return nil, fail("claim deliveries for", sessionID, errors.Join(err, rbErr, relErr))
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

// stopCheck counts a session's open blocking tickets and says whether any
// answer of it is queued, in one read. Both parts use the
// decisions_session_status index by session_id. Parameters: the session
// id, now, the session id.
const stopCheck = `
SELECT
  (SELECT count(*) FROM decisions d WHERE d.session_id = ? AND d.mode = 'blocking' AND ` + openAt + `),
  EXISTS (SELECT 1 FROM decisions WHERE session_id = ? AND delivery = 'queued')`

// StopCheck is what curia hook Stop reads before it claims anything, in one
// small statement: the number of blocking tickets of session sessionID open
// at now (as OpenBlocking counts them), and whether any answer of the
// session is queued (so ClaimDeliveries has something to claim). A claim
// changes only the delivery of decided tickets, never which tickets are
// open, so the count is the same before and after a claim in the same
// transaction. The hook runs ClaimDeliveries only when queued is true, so
// the common Stop never prepares the claim's UPDATE ... RETURNING. An empty
// sessionID has no tickets and nothing queued.
func (t *Tx) StopCheck(ctx context.Context, sessionID string, now time.Time) (openBlocking int, queued bool, err error) {
	if sessionID == "" {
		return 0, false, nil
	}
	err = t.tx.QueryRowContext(ctx, stopCheck, sessionID, formatTime(now), sessionID).Scan(&openBlocking, &queued)
	if err != nil {
		return 0, false, fail("check stop for", sessionID, err)
	}
	return openBlocking, queued, nil
}

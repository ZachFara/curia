package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// notifyKey is the meta key of the last blocking-ticket notification.
const notifyKey = "last_blocking_notify_at"

// ClaimNotifySlot claims the right to send a blocking-ticket notification
// at now, at most one per every across all processes. In one statement
// (so one write transaction): when meta has no last_blocking_notify_at, or
// it is at least every before now, now is written and true returned;
// otherwise nothing changes and it returns false. With every <= 0 every
// call claims, unless the stored time is after now: a stored time in the
// future (a clock set back) holds the slot until now reaches it plus every.
func (s *Store) ClaimNotifySlot(ctx context.Context, now time.Time, every time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value WHERE meta.value <= ?`,
		notifyKey, formatTime(now), formatTime(now.Add(-every)))
	if err != nil {
		return false, fmt.Errorf("store: claim notify slot: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim notify slot: %w", err)
	}
	return n == 1, nil
}

// PruneEvents deletes the events before before among the oldest limit
// events by id, in one statement, and returns how many it deleted. Events
// are appended in time order, so the oldest ids are the oldest events, and
// one call reads at most limit rows and holds the write lock only that
// long; call it again until it returns 0. A limit of 0 or less deletes
// nothing.
func (s *Store) PruneEvents(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE at < ? AND id IN (SELECT id FROM events ORDER BY id LIMIT ?)`,
		formatTime(before), limit)
	if err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune events: %w", err)
	}
	return n, nil
}

// restoreKey is the meta key of the curia restore claim: the time its
// holder claimed it.
const restoreKey = "restore_claim_at"

// ClaimRestore claims the right to run curia restore at now, one holder at a time; a claim older than hold is taken over.
//
// It is ClaimNotifySlot's statement on restoreKey, so the take is atomic:
// of concurrent claimers exactly one gets true. A crashed restore holds the
// claim for hold at most. A stored time after now (a clock set back) holds
// it until now reaches that time plus hold. hold must be positive. The
// Store remembers the value it wrote, so that ReleaseRestore gives up only
// its own claim.
func (s *Store) ClaimRestore(ctx context.Context, now time.Time, hold time.Duration) (bool, error) {
	if hold <= 0 || now.IsZero() {
		return false, errors.New("store: claim restore: needs a time and a positive hold")
	}
	value := now.UTC().Format(timeLayout)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value WHERE meta.value <= ?`,
		restoreKey, value, formatTime(now.Add(-hold)))
	if err != nil {
		return false, fmt.Errorf("store: claim restore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim restore: %w", err)
	}
	if n != 1 {
		return false, nil
	}
	s.restoreMu.Lock()
	s.restoreClaim = value
	s.restoreMu.Unlock()
	return true, nil
}

// ReleaseRestore gives the claim up.
//
// It deletes the key only while it still holds the value this Store's last
// successful ClaimRestore wrote, in one statement: a restore whose claim
// was taken over after hold does not free the newer holder's claim, and a
// Store that holds no claim changes nothing.
func (s *Store) ReleaseRestore(ctx context.Context) error {
	s.restoreMu.Lock()
	defer s.restoreMu.Unlock()
	if s.restoreClaim == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ? AND value = ?`, restoreKey, s.restoreClaim); err != nil {
		return fmt.Errorf("store: release restore: %w", err)
	}
	s.restoreClaim = ""
	return nil
}

// restoreCandidates is RestoreCandidates' statement (parameters: since, limit).
const restoreCandidates = selectSessions + `
WHERE (state != 'ended' OR (state = 'ended' AND end_reason = 'other')) AND updated_at >= ?
ORDER BY updated_at DESC, id
LIMIT ?`

// RestoreCandidates returns the sessions curia restore classifies (plan
// section 4.10, step 3, with Ruling 117), most recently active first, at
// most limit of them (none when limit <= 0): every session that is not
// ended, plus every ended session whose end_reason is exactly "other" (a
// tmux kill or crash ends Claude Code that way), whose updated_at is at or
// after since. A session ended for any other reason, or with none (a
// user's own exit, or ended by a nested session), is never one. The
// transcript's age is restore's own check on top of this window.
func (s *Store) RestoreCandidates(ctx context.Context, since time.Time, limit int) ([]model.Session, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, restoreCandidates, formatTime(since), limit)
	if err != nil {
		return nil, fmt.Errorf("store: restore candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: restore candidates: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: restore candidates: %w", err)
	}
	return out, nil
}

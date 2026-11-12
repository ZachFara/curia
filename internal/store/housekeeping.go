package store

import (
	"context"
	"fmt"
	"time"
)

// notifyKey is the meta key of the last blocking-ticket notification.
const notifyKey = "last_blocking_notify_at"

// ClaimNotifySlot claims the right to send a blocking-ticket notification
// at now, at most one per every across all processes. In one statement
// (so one write transaction): when meta has no last_blocking_notify_at, or
// it is at least every before now, now is written and true returned;
// otherwise nothing changes and it returns false.
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

package store

import (
	"context"
	"fmt"
	"time"
)

const (
	// PermissionEventKeep is how long a denial row is kept: past any away period a summary is still built from.
	PermissionEventKeep = 30 * 24 * time.Hour
	// HistoryKeep is how long the auto log and the schedule runs are kept.
	HistoryKeep = 90 * 24 * time.Hour
	// ConfigRevisionsKeep is how many of the newest config revisions are kept, besides every baseline (the original file).
	ConfigRevisionsKeep = 200
	historyPruneBatch   = 1000
)

// PruneHistory trims the tables that only grow: denials, the auto log and
// schedule runs by age, config revisions to the newest ConfigRevisionsKeep
// and the baselines. Each statement reads at most historyPruneBatch rows from
// the oldest id (rows are appended in time order), like PruneEvents; the
// monitor runs it every minute, so a backlog drains in passes.
func (s *Store) PruneHistory(ctx context.Context, now time.Time) error {
	for _, q := range []struct {
		name, sql string
		arg       any
	}{
		{"denials", `DELETE FROM permission_events WHERE at < ?1 AND id IN (SELECT id FROM permission_events ORDER BY id LIMIT ?2)`, formatTime(now.Add(-PermissionEventKeep))},
		{"auto log", `DELETE FROM auto_spawn_runs WHERE at < ?1 AND id IN (SELECT id FROM auto_spawn_runs ORDER BY id LIMIT ?2)`, formatTime(now.Add(-HistoryKeep))},
		{"schedule runs", `DELETE FROM schedule_runs WHERE ran_at < ?1 AND id IN (SELECT id FROM schedule_runs ORDER BY id LIMIT ?2)`, formatTime(now.Add(-HistoryKeep))},
		{"config revisions", `DELETE FROM config_revisions WHERE actor <> 'baseline' AND n IN (SELECT n FROM config_revisions
WHERE n <= (SELECT MAX(n) FROM config_revisions) - ?1 ORDER BY n LIMIT ?2)`, ConfigRevisionsKeep},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, q.arg, historyPruneBatch); err != nil {
			return fmt.Errorf("store: prune %s: %w", q.name, err)
		}
	}
	return nil
}

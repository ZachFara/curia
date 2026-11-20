package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// F19 resource samples (migration 0014).
const (
	// ResourceKeep is how long a sample is kept; the readers look back
	// minutes, the rest is history for a later view.
	ResourceKeep = 24 * time.Hour
	// ResourceMaxRows bounds the table whatever the clock says: a session
	// count times 8640 samples a day stays far below it.
	ResourceMaxRows = 100000
	// resourcePruneBatch bounds one prune pass.
	resourcePruneBatch = 1000
)

// ResourceSample is one session's CPU percent and resident memory in MiB at one tick.
type ResourceSample struct {
	SessionID string
	At        time.Time
	CPU       float64
	RSSMB     int64
}

// AddResourceSamples inserts the samples in one transaction. It only
// inserts: pruning is PruneResources, run apart from it.
func (s *Store) AddResourceSamples(ctx context.Context, samples []ResourceSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: add resource samples: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range samples {
		if _, err = tx.ExecContext(ctx, `INSERT INTO resource_samples (at, session_id, cpu_pct, rss_mb) VALUES (?, ?, ?, ?)`,
			formatTime(r.At), r.SessionID, r.CPU, r.RSSMB); err != nil {
			return fmt.Errorf("store: add resource samples: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: add resource samples: commit: %w", err)
	}
	return nil
}

// LatestResources is each session's newest sample, by session id.
func (s *Store) LatestResources(ctx context.Context) (map[string]ResourceSample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, at, cpu_pct, rss_mb FROM resource_samples
WHERE n IN (SELECT MAX(n) FROM resource_samples WHERE session_id IS NOT NULL GROUP BY session_id)`)
	if err != nil {
		return nil, fmt.Errorf("store: read resource samples: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ResourceSample{}
	for rows.Next() {
		var r ResourceSample
		var at sql.NullString
		if err = rows.Scan(&r.SessionID, &at, &r.CPU, &r.RSSMB); err != nil {
			return nil, fmt.Errorf("store: read resource samples: %w", err)
		}
		if r.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("store: read resource samples: %w", err)
		}
		out[r.SessionID] = r
	}
	return out, rows.Err()
}

// PruneResources deletes at most resourcePruneBatch samples older than
// ResourceKeep, then rows beyond the last ResourceMaxRows numbers, and
// returns how many. The monitor runs it on its own, never inside an insert.
func (s *Store) PruneResources(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM resource_samples WHERE n IN
(SELECT n FROM resource_samples WHERE at < ? ORDER BY at LIMIT ?)`, formatTime(now.Add(-ResourceKeep)), resourcePruneBatch)
	if err != nil {
		return 0, fmt.Errorf("store: prune resource samples: %w", err)
	}
	aged, _ := res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `DELETE FROM resource_samples WHERE n IN
(SELECT n FROM resource_samples WHERE n <= (SELECT MAX(n) FROM resource_samples) - ? ORDER BY n LIMIT ?)`, ResourceMaxRows, resourcePruneBatch)
	if err != nil {
		return int(aged), fmt.Errorf("store: prune resource samples: %w", err)
	}
	capped, _ := res.RowsAffected()
	return int(aged + capped), nil
}

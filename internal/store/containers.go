package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// F19 part 2: the Docker containers the monitor last saw (migration 0018).

// ContainerRow is one container with its attribution (By is label, compose or none).
type ContainerRow struct {
	ID, SessionID, Name, By string
	CPU                     float64
	MemMB                   int64
}

// ReplaceContainers makes rows the whole set seen at at, in one transaction.
func (s *Store) ReplaceContainers(ctx context.Context, rows []ContainerRow, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: replace containers: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `DELETE FROM containers`); err != nil {
		return fmt.Errorf("store: replace containers: %w", err)
	}
	for _, r := range rows {
		if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO containers (id, session_id, name, attributed_by, cpu_pct, mem_mb, seen_at)
VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?)`, r.ID, r.SessionID, r.Name, r.By, r.CPU, r.MemMB, formatTime(at)); err != nil {
			return fmt.Errorf("store: replace containers: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: replace containers: commit: %w", err)
	}
	return nil
}

// LatestContainers is the last set and when it was seen (zero time for none).
func (s *Store) LatestContainers(ctx context.Context) ([]ContainerRow, time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(session_id, ''), name, attributed_by, cpu_pct, mem_mb, seen_at FROM containers ORDER BY id`)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("store: read containers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ContainerRow
	var seen time.Time
	for rows.Next() {
		var r ContainerRow
		var at sql.NullString
		if err = rows.Scan(&r.ID, &r.SessionID, &r.Name, &r.By, &r.CPU, &r.MemMB, &at); err != nil {
			return nil, time.Time{}, fmt.Errorf("store: read containers: %w", err)
		}
		if seen, err = parseTime(at); err != nil {
			return nil, time.Time{}, fmt.Errorf("store: read containers: %w", err)
		}
		out = append(out, r)
	}
	return out, seen, rows.Err()
}

// DockerVM is the Docker VM's memory at one tick: the containers' summed use
// and the VM's total, in MiB.
type DockerVM struct {
	At              time.Time
	UsedMB, LimitMB int64
}

// AddDockerVM stores the VM reading on a resource_samples row with no session.
func (s *Store) AddDockerVM(ctx context.Context, at time.Time, usedMB, limitMB int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO resource_samples (at, session_id, cpu_pct, rss_mb, docker_mem_mb, docker_mem_limit_mb)
VALUES (?, NULL, 0, 0, ?, ?)`, formatTime(at), usedMB, limitMB)
	if err != nil {
		return fmt.Errorf("store: add docker memory: %w", err)
	}
	return nil
}

// LatestDockerVM is the newest VM reading; ok is false when there is none.
func (s *Store) LatestDockerVM(ctx context.Context) (vm DockerVM, ok bool, err error) {
	var at sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT at, docker_mem_mb, docker_mem_limit_mb FROM resource_samples
WHERE session_id IS NULL AND docker_mem_limit_mb IS NOT NULL ORDER BY n DESC LIMIT 1`).Scan(&at, &vm.UsedMB, &vm.LimitMB)
	if errors.Is(err, sql.ErrNoRows) {
		return vm, false, nil
	}
	if err != nil {
		return vm, false, fmt.Errorf("store: read docker memory: %w", err)
	}
	if vm.At, err = parseTime(at); err != nil {
		return vm, false, fmt.Errorf("store: read docker memory: %w", err)
	}
	return vm, true, nil
}

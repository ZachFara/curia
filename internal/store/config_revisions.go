package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ConfigRevision is one change of the user config file.
type ConfigRevision struct {
	N                    int64
	At                   time.Time
	Actor, Key, Old, New string
	File                 string // the whole user file after the change
}

// configAckKey is the meta key holding the newest external revision the
// user has acknowledged with `curia config doctor`.
const configAckKey = "config_ack"

// AddConfigRevision appends a revision and returns its number.
func (s *Store) AddConfigRevision(ctx context.Context, r ConfigRevision) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO config_revisions (at, actor, key, old, new, file) VALUES (?, ?, ?, ?, ?, ?)`,
		formatTime(r.At), r.Actor, r.Key, r.Old, r.New, r.File)
	if err != nil {
		return 0, fmt.Errorf("store: add config revision: %w", err)
	}
	return res.LastInsertId()
}

// ConfigRevisions returns the newest limit revisions, newest first.
func (s *Store) ConfigRevisions(ctx context.Context, limit int) ([]ConfigRevision, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT n, at, actor, key, old, new, file FROM config_revisions ORDER BY n DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list config revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ConfigRevision
	for rows.Next() {
		var r ConfigRevision
		var at sql.NullString
		if err = rows.Scan(&r.N, &at, &r.Actor, &r.Key, &r.Old, &r.New, &r.File); err != nil {
			return nil, fmt.Errorf("store: scan config revision: %w", err)
		}
		if r.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("store: config revision time: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ConfigRevision returns revision n; ok is false when there is none.
func (s *Store) ConfigRevision(ctx context.Context, n int64) (r ConfigRevision, ok bool, err error) {
	var at sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT n, at, actor, key, old, new, file FROM config_revisions WHERE n = ?`, n).
		Scan(&r.N, &at, &r.Actor, &r.Key, &r.Old, &r.New, &r.File)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("store: read config revision: %w", err)
	}
	r.At, err = parseTime(at)
	return r, err == nil, err
}

// ConfigAck returns the number of the newest external revision the user
// acknowledged, or 0.
func (s *Store) ConfigAck(ctx context.Context) (int64, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, configAckKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: read config ack: %w", err)
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n, nil
}

// AckConfig records that external revisions up to n are acknowledged.
func (s *Store) AckConfig(ctx context.Context, n int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value`, configAckKey, strconv.FormatInt(n, 10))
	if err != nil {
		return fmt.Errorf("store: ack config: %w", err)
	}
	return nil
}

// ConfigHead is the newest revision's file and the marks `curia status`
// needs, read in one indexed query.
type ConfigHead struct {
	File          string
	External, Ack int64 // the newest external revision's number, and the acknowledged one; 0 when none
}

// ConfigHead returns the newest revision's head; ok is false with no revision.
func (s *Store) ConfigHead(ctx context.Context) (h ConfigHead, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `
SELECT file,
  COALESCE((SELECT MAX(n) FROM config_revisions WHERE actor = 'external'), 0),
  COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = ?), 0)
FROM config_revisions ORDER BY n DESC LIMIT 1`, configAckKey).Scan(&h.File, &h.External, &h.Ack)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	if err != nil {
		return h, false, fmt.Errorf("store: read config head: %w", err)
	}
	return h, true, nil
}

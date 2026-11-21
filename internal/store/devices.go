package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MaxDevices bounds the web device tokens.
const MaxDevices = 16

// Device errors.
var (
	ErrDeviceExists   = errors.New("store: a device with that name exists")
	ErrTooManyDevices = errors.New("store: too many devices")
)

// Device is one web device token: its name and the SHA-256 of the token.
type Device struct {
	Name, TokenSHA256 string
	CreatedAt         time.Time
}

// AddDevice stores a device token's hash under name.
func (s *Store) AddDevice(ctx context.Context, name, tokenHash string, now time.Time) error {
	if !validTokenHash(tokenHash) {
		return errors.New("store: add device: malformed token hash")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("store: add device: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT count(*), coalesce(max(name = ?), 0) FROM web_devices`, name).Scan(&n, &exists); err != nil {
		return fmt.Errorf("store: add device: %w", err)
	}
	switch {
	case exists:
		return ErrDeviceExists
	case n >= MaxDevices:
		return ErrTooManyDevices
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO web_devices (name, token_sha256, created_at) VALUES (?, ?, ?)`, name, tokenHash, formatTime(now)); err != nil {
		return fmt.Errorf("store: add device: %w", err)
	}
	return tx.Commit()
}

// Devices lists the devices, oldest first.
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, token_sha256, created_at FROM web_devices ORDER BY created_at, name LIMIT ?`, MaxDevices)
	if err != nil {
		return nil, fmt.Errorf("store: list devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Device
	for rows.Next() {
		var d Device
		var at sql.NullString
		if err = rows.Scan(&d.Name, &d.TokenSHA256, &at); err != nil {
			return nil, fmt.Errorf("store: scan device: %w", err)
		}
		if d.CreatedAt, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDevice revokes a device; false when there was none.
func (s *Store) DeleteDevice(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM web_devices WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("store: delete device: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

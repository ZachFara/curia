package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// setupApprovalKey is the meta key holding the SHA-256 of the setup script
// the user approved for repo (Ruling 108): one key per main repository.
func setupApprovalKey(repo string) string { return "setup_approved_sha256:" + repo }

// SetupApproval returns the hex SHA-256 of the setup script the user
// approved for the main repository repo, or "" when none is.
func (s *Store) SetupApproval(ctx context.Context, repo string) (string, error) {
	var sum string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, setupApprovalKey(repo)).Scan(&sum)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read setup approval: %w", err)
	}
	return sum, nil
}

// ApproveSetup records sum, a hex SHA-256, as the setup script approved for
// repo, replacing any earlier approval for it, in one statement.
func (s *Store) ApproveSetup(ctx context.Context, repo, sum string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value`, setupApprovalKey(repo), sum)
	if err != nil {
		return fmt.Errorf("store: approve setup script: %w", err)
	}
	return nil
}

package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
)

// The push outbox's limits (plan section 4.7).
const (
	// PushReclaim is how long a sending row may stay claimed: a row claimed
	// longer ago belongs to a sender that died, and is claimed again.
	PushReclaim = 2 * time.Minute
	// PushMaxAttempts bounds the send attempts of one push.
	PushMaxAttempts = 5
	// PushTokenTTL is how long after it was sent a push's one-time token can
	// be spent.
	PushTokenTTL = 24 * time.Hour
)

// ErrBadToken is UsePushToken's one refusal, whatever the reason. It is
// returned as it is, never wrapped, so no caller can tell the reasons apart.
var ErrBadToken = errors.New("store: unknown, spent, expired or mismatched token")

const (
	// maxPushError bounds a stored last_error, in bytes.
	maxPushError = 200
	// settleBatch bounds how many rows one ClaimPush settles (drops or fails).
	settleBatch = 64
	// maxTokenCandidates bounds the spendable pushes of one decision that
	// UsePushToken compares: one per device, and config allows at most 8.
	maxTokenCandidates = 32
	// tokenHashLen is the length of a hex SHA-256.
	tokenHashLen = 64
)

// pushColumns are the columns of a push in scanPush's order.
const pushColumns = `n, kind, ref, device, status, not_before, attempts, last_error, token_hash,
  created_at, claimed_at, sent_at, used_at`

// selectPushes reads every column of model.Push.
const selectPushes = `SELECT ` + pushColumns + ` FROM pushes`

// pushClaimable matches the rows ClaimPush may take at a time, from its two
// parameters: now, and now minus PushReclaim. Its first term is the WHERE
// clause of the pushes_due index word for word, and its second bounds
// not_before, so it is a range read of that index. The statements that
// order by n name the index (INDEXED BY): on a small table the planner
// would rather scan by rowid than sort, and with INDEXED BY a statement
// that could not use the index fails to prepare instead of scanning.
const pushClaimable = `p.status IN ('pending','sending') AND p.not_before <= ? AND (p.status = 'pending' OR p.claimed_at <= ?)`

// pushSubjectOpen is true while the push's subject still wants it at its
// one parameter, now: a decision push's ticket is blocking and open (the
// store's openAt), a review push's review is pending. Anything else (an
// answered, expired or withdrawn ticket, an assume ticket, a closed review,
// an unknown ref) is not.
const pushSubjectOpen = `CASE p.kind
  WHEN 'decision' THEN EXISTS (SELECT 1 FROM decisions d WHERE d.id = p.ref AND d.mode = 'blocking' AND ` + openAt + `)
  WHEN 'review' THEN EXISTS (SELECT 1 FROM reviews r WHERE r.id = p.ref AND r.status = 'pending')
  ELSE 0 END`

// pushesDue is PushesDue's statement (parameters: now, now minus PushReclaim).
const pushesDue = `SELECT EXISTS (SELECT 1 FROM pushes p WHERE ` + pushClaimable + `)`

// settlePushes picks the claimable rows that must not be sent again: out of
// attempts, or with a closed subject (parameters: now, now minus
// PushReclaim, PushMaxAttempts, now, the batch size).
const settlePushes = `SELECT p.n FROM pushes p INDEXED BY pushes_due WHERE ` + pushClaimable + `
  AND (p.attempts >= ? OR NOT (` + pushSubjectOpen + `)) ORDER BY p.n LIMIT ?`

// claimPushPick picks the oldest claimable row that may be sent
// (parameters: now, now minus PushReclaim, PushMaxAttempts, now).
const claimPushPick = `SELECT p.n FROM pushes p INDEXED BY pushes_due WHERE ` + pushClaimable + `
  AND p.attempts < ? AND (` + pushSubjectOpen + `) ORDER BY p.n LIMIT 1`

// spendablePushes reads the spendable pushes of one decision
// (parameters: the decision id, now minus PushTokenTTL, the bound).
const spendablePushes = `SELECT n, token_hash FROM pushes
WHERE kind = 'decision' AND ref = ? AND status = 'sent' AND used_at IS NULL AND sent_at > ?
ORDER BY n LIMIT ?`

func scanPush(sc scanner) (model.Push, error) {
	var p model.Push
	var notBefore, created, claimed, sent, used sql.NullString
	err := sc.Scan(&p.N, &p.Kind, &p.Ref, &p.Device, &p.Status, &notBefore, &p.Attempts, &p.LastError,
		&p.TokenHash, &created, &claimed, &sent, &used)
	if err != nil {
		return model.Push{}, err
	}
	for _, f := range []struct {
		dst *time.Time
		src sql.NullString
	}{{&p.NotBefore, notBefore}, {&p.CreatedAt, created}, {&p.ClaimedAt, claimed}, {&p.SentAt, sent}, {&p.UsedAt, used}} {
		if *f.dst, err = parseTime(f.src); err != nil {
			return model.Push{}, err
		}
	}
	return p, nil
}

// validTokenHash reports whether h is a lower-case hex SHA-256, the only
// form a push's token hash takes.
func validTokenHash(h string) bool {
	if len(h) != tokenHashLen {
		return false
	}
	for i := range len(h) {
		if c := h[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// EnqueuePushes adds one pending push per device for (kind, ref), due at notBefore, and returns how many
// rows it added; a row that exists for (kind, ref, device) is left as it is.
// All rows go in one write transaction. An unknown kind, an empty ref or
// device, or a zero notBefore is an error and adds nothing.
func (s *Store) EnqueuePushes(ctx context.Context, kind model.PushKind, ref string, devices []string, notBefore, now time.Time) (int, error) {
	if kind != model.PushDecision && kind != model.PushReview {
		return 0, fmt.Errorf("store: enqueue pushes: unknown kind %q", kind)
	}
	if ref == "" || notBefore.IsZero() {
		return 0, errors.New("store: enqueue pushes: a push needs a ref and a due time")
	}
	for _, d := range devices {
		if d == "" {
			return 0, errors.New("store: enqueue pushes: empty device name")
		}
	}
	if len(devices) == 0 {
		return 0, nil
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: enqueue pushes: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	added := 0
	for _, d := range devices {
		res, err := tx.ExecContext(ctx, `
INSERT INTO pushes (kind, ref, device, status, not_before, created_at) VALUES (?, ?, ?, 'pending', ?, ?)
ON CONFLICT (kind, ref, device) DO NOTHING`, kind, ref, d, formatTime(notBefore), formatTime(now))
		if err != nil {
			return 0, fmt.Errorf("store: enqueue pushes for %s: %w", ref, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: enqueue pushes for %s: %w", ref, err)
		}
		added += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: enqueue pushes for %s: commit: %w", ref, err)
	}
	return added, nil
}

// PushesDue reports whether ClaimPush would find a row at now. It writes nothing.
// It reads only the pushes_due index. A row it counts may be one ClaimPush
// settles instead of claiming (its subject closed, or no attempt left), so
// a true is followed by at most a few ClaimPush calls that return false.
func (s *Store) PushesDue(ctx context.Context, now time.Time) (bool, error) {
	var due bool
	if err := s.db.QueryRowContext(ctx, pushesDue, formatTime(now), formatTime(now.Add(-PushReclaim))).Scan(&due); err != nil {
		return false, fmt.Errorf("store: pushes due: %w", err)
	}
	return due, nil
}

// ClaimPush claims the oldest due push (section 4.7, step 2) and stores tokenHash on it ("" for none).
//
// In one write transaction it first settles, at most settleBatch at a
// time, the claimable rows that must not be sent: a row whose subject is
// closed (pushSubjectOpen) becomes dropped, and a row out of attempts (a
// sender that died on the last one) becomes failed, both without a token.
// Then, in one statement, the claimable row with the lowest n whose
// subject is open and that has attempts left becomes sending, with
// attempts + 1, claimed_at = now and token_hash = tokenHash. found is false
// when there is none. tokenHash must be "" or a lower-case hex SHA-256.
func (s *Store) ClaimPush(ctx context.Context, tokenHash string, now time.Time) (p model.Push, found bool, err error) {
	if tokenHash != "" && !validTokenHash(tokenHash) {
		return model.Push{}, false, errors.New("store: claim push: token hash is not a hex SHA-256")
	}
	at, reclaim := formatTime(now), formatTime(now.Add(-PushReclaim))
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Push{}, false, fmt.Errorf("store: claim push: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `
UPDATE pushes SET
  status = CASE WHEN attempts >= ? THEN 'failed' ELSE 'dropped' END,
  last_error = CASE WHEN attempts >= ? THEN 'no attempt left' ELSE last_error END,
  token_hash = ''
WHERE n IN (`+settlePushes+`)`,
		PushMaxAttempts, PushMaxAttempts, at, reclaim, PushMaxAttempts, at, settleBatch); err != nil {
		return model.Push{}, false, fmt.Errorf("store: claim push: settle: %w", err)
	}
	p, err = scanPush(tx.QueryRowContext(ctx, `
UPDATE pushes SET status = 'sending', attempts = attempts + 1, claimed_at = ?, token_hash = ?
WHERE n = (`+claimPushPick+`)
RETURNING `+pushColumns,
		at, tokenHash, at, reclaim, PushMaxAttempts, at))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		found = false
	case err != nil:
		return model.Push{}, false, fmt.Errorf("store: claim push: %w", err)
	default:
		found = true
	}
	if err := tx.Commit(); err != nil {
		return model.Push{}, false, fmt.Errorf("store: claim push: commit: %w", err)
	}
	return p, found, nil
}

// FinishPush records the result of the claim of push n whose attempt number was attempt (the claimed row's
// Attempts): sent (sets sent_at), dropped, failed, or pending again with not_before = retryAt. errText is
// stored cut to 200 bytes. It changes only a row that is sending under that same attempt, so a sender that
// stalled past PushReclaim cannot finish the row another sender has claimed again since (M5).
//
// It is one statement. sent keeps the row's token hash; every other status
// clears it, so the token of a push that was not sent can never be spent.
// A pending result for a row that has used PushMaxAttempts attempts is
// stored as failed, so no push is tried more often than that. A status
// other than those four, a pending result without retryAt or a sent one
// without now is an error and changes nothing. Finishing a row that is not
// sending (or does not exist) changes nothing and is not an error.
func (s *Store) FinishPush(ctx context.Context, n int64, attempt int, status model.PushStatus, errText string, retryAt, now time.Time) error {
	retry, sent := status == model.PushPending, status == model.PushSent
	switch {
	case status != model.PushSent && status != model.PushDropped && status != model.PushFailed && !retry:
		return fmt.Errorf("store: finish push %d: unknown result %q", n, status)
	case retry && retryAt.IsZero():
		return fmt.Errorf("store: finish push %d: a retry needs a time", n)
	case sent && now.IsZero():
		return fmt.Errorf("store: finish push %d: a sent push needs a time", n)
	}
	var sentAt, notBefore any
	if sent {
		sentAt = formatTime(now)
	}
	if retry {
		notBefore = formatTime(retryAt)
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE pushes SET
  status = CASE WHEN ? AND attempts >= ? THEN 'failed' ELSE ? END,
  last_error = ?,
  token_hash = CASE WHEN ? THEN token_hash ELSE '' END,
  sent_at = COALESCE(?, sent_at),
  not_before = COALESCE(?, not_before)
WHERE n = ? AND status = 'sending' AND attempts = ?`,
		retry, PushMaxAttempts, status, cutPushError(errText), sent, sentAt, notBefore, n, attempt)
	if err != nil {
		return fmt.Errorf("store: finish push %d: %w", n, err)
	}
	return nil
}

// ClearPushToken drops the token hash of push n, claimed and being sent, when
// its notification turned out to carry no button (M4): a hash no push
// carries is never left spendable. It changes only a row that is sending.
func (s *Store) ClearPushToken(ctx context.Context, n int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE pushes SET token_hash = '' WHERE n = ? AND status = 'sending'`, n); err != nil {
		return fmt.Errorf("store: clear push token %d: %w", n, err)
	}
	return nil
}

// cutPushError cuts s to at most maxPushError bytes, on a rune boundary.
func cutPushError(s string) string {
	if len(s) <= maxPushError {
		return s
	}
	i := maxPushError
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// UsePushToken spends the one-time token whose hash is tokenHash for decisionID (section 4.8, step 3).
//
// It succeeds once, for a decision push of decisionID that is sent, was
// sent less than PushTokenTTL before now, has not been spent, and holds
// tokenHash; it sets used_at = now and returns the push. Every other case
// is ErrBadToken itself. A malformed hash ("" among them) is refused
// without a query. Otherwise the decision's spendable pushes are read
// (the pushes_once index) and each stored hash is compared with tokenHash
// in constant time, all of them, so the time taken does not depend on
// where or whether they match. Only a match writes, in one conditional
// statement that repeats every condition, so of concurrent spends of one
// token exactly one succeeds and a refusal never writes.
func (s *Store) UsePushToken(ctx context.Context, tokenHash, decisionID string, now time.Time) (model.Push, error) {
	if !validTokenHash(tokenHash) || decisionID == "" || now.IsZero() {
		return model.Push{}, ErrBadToken
	}
	oldest := formatTime(now.Add(-PushTokenTTL))
	n, err := matchPushToken(ctx, s.db, tokenHash, decisionID, oldest)
	if err != nil {
		return model.Push{}, fmt.Errorf("store: use push token: %w", err)
	}
	if n == 0 {
		return model.Push{}, ErrBadToken
	}
	p, err := scanPush(s.db.QueryRowContext(ctx, `
UPDATE pushes SET used_at = ?
WHERE n = ? AND token_hash = ? AND kind = 'decision' AND ref = ? AND status = 'sent'
  AND used_at IS NULL AND sent_at > ?
RETURNING `+pushColumns, formatTime(now), n, tokenHash, decisionID, oldest))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Push{}, ErrBadToken
	}
	if err != nil {
		return model.Push{}, fmt.Errorf("store: use push token: %w", err)
	}
	return p, nil
}

// MatchPushToken reports, with nil, that tokenHash is the token of a spendable push of decisionID (every
// condition of UsePushToken), and spends nothing; otherwise ErrBadToken itself. The endpoint matches a
// token first and spends it with UsePushToken only once the answer has been checked.
func (s *Store) MatchPushToken(ctx context.Context, tokenHash, decisionID string, now time.Time) error {
	if !validTokenHash(tokenHash) || decisionID == "" || now.IsZero() {
		return ErrBadToken
	}
	n, err := matchPushToken(ctx, s.db, tokenHash, decisionID, formatTime(now.Add(-PushTokenTTL)))
	if err != nil {
		return fmt.Errorf("store: match push token: %w", err)
	}
	if n == 0 {
		return ErrBadToken
	}
	return nil
}

// matchPushToken returns the n of the spendable push of decisionID whose
// hash is tokenHash, or 0. Every candidate is compared in constant time.
func matchPushToken(ctx context.Context, q dbtx, tokenHash, decisionID string, oldest any) (int64, error) {
	rows, err := q.QueryContext(ctx, spendablePushes, decisionID, oldest, maxTokenCandidates)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var match int64
	want := []byte(tokenHash)
	for rows.Next() {
		var n int64
		var h string
		if err := rows.Scan(&n, &h); err != nil {
			return 0, err
		}
		if subtle.ConstantTimeCompare([]byte(h), want) == 1 {
			match = n
		}
	}
	return match, rows.Err()
}

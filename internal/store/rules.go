package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Rule is a standing rule to store as decided precedent.
type Rule struct {
	Text  string      // already normalised by message.Normalize
	Scope model.Scope // repo or global
	Repo  string      // required for scope repo
	Via   model.AnsweredVia
}

// ErrBadRule is returned by CreateRule for a rule with an empty text, a
// scope other than repo or global, or scope repo without a repository.
var ErrBadRule = errors.New("store: a rule needs scope repo with a repository, or scope global")

// ruleQuestionRunes is how much of a rule its question holds (section 4.11).
const ruleQuestionRunes = 300

// CreateRule stores r at now as a decided decision with no options (plan
// section 4.11), in one write transaction. A global rule has no repository,
// whatever r.Repo says; an empty r.Via is stored as cli. An existing row
// with the same args hash and no session that is decided and not
// superseded is returned instead, with duplicate true. Otherwise the rule
// is inserted with the next id, D-<n>, and the stored row is returned.
func (s *Store) CreateRule(ctx context.Context, r Rule, now time.Time) (d model.Decision, duplicate bool, err error) {
	switch {
	case strings.TrimSpace(r.Text) == "":
		return model.Decision{}, false, ErrBadRule
	case r.Scope == model.ScopeGlobal:
		r.Repo = ""
	case r.Scope != model.ScopeRepo || r.Repo == "":
		return model.Decision{}, false, ErrBadRule
	}
	if r.Via == "" {
		r.Via = model.AnsweredCLI
	}
	sum := sha256.Sum256([]byte("rule\n" + string(r.Scope) + "\n" + r.Repo + "\n" + r.Text))
	hash := hex.EncodeToString(sum[:])

	tx, err := s.begin(ctx)
	if err != nil {
		return model.Decision{}, false, fail("create rule", "", err)
	}
	defer func() { _ = tx.Rollback() }()
	twin, err := scanDecision(tx.QueryRowContext(ctx, selectDecisions+`
FROM decisions d
WHERE d.session_id = '' AND d.args_hash = ? AND `+decided+` AND d.superseded_at IS NULL
ORDER BY d.n DESC LIMIT 1`, hash))
	switch {
	case err == nil:
		return twin, true, nil // read only: the rollback releases the lock
	case !errors.Is(err, sql.ErrNoRows):
		return model.Decision{}, false, fail("create rule", "", err)
	}
	var id string
	err = tx.QueryRowContext(ctx, `
INSERT INTO decisions (
  id, session_id, repo, question, options, recommendation, rationale, reversibility, mode, scope,
  status, answer, answered_via, args_hash, delivery, created_at, answered_at
) VALUES (
  'D-' || (SELECT COALESCE(MAX(n), 0) + 1 FROM decisions), '', ?, ?, '[]', '', '', ?, ?, ?,
  ?, ?, ?, ?, ?, ?, ?
) RETURNING id`,
		r.Repo, sanitize.Line(r.Text, ruleQuestionRunes), model.ReversCheap, model.ModeBlocking, r.Scope,
		model.StatusAnswered, r.Text, r.Via, hash, model.DeliveryNone, formatTime(now), formatTime(now)).Scan(&id)
	if err != nil {
		return model.Decision{}, false, fail("create rule", "", err)
	}
	if d, err = commitRead(ctx, tx, id); err != nil {
		return model.Decision{}, false, fail("create rule", id, err)
	}
	return d, false, nil
}

package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/precedent"
)

// SetNearMatch records on ticket id the decision request_decision returned
// as its near-duplicate at raise time (F25, Ruling 160).
func (s *Store) SetNearMatch(ctx context.Context, id, nearID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE decisions SET near_match = ? WHERE id = ?`, nearID, id); err != nil {
		return fail("set near match", id, err)
	}
	return nil
}

// DecisionLogQuery selects decisions for the log. Text is searched with
// the same full-text index and expression as precedent search; with no
// words in it nothing matches. An empty Status means the decided ones.
type DecisionLogQuery struct {
	Text, Repo, Scope string
	Status            model.DecisionStatus
	Since             time.Time
	Limit             int // default 50
}

// decisionLogWhere is the filter of the log and its summary: the match
// expression (? = ” means no search), repo, scope, status (or decided),
// since.
const decisionLogWhere = `
FROM decisions d
WHERE (? = '' OR d.n IN (SELECT rowid FROM decisions_fts WHERE decisions_fts MATCH ?))
  AND (? = '' OR d.repo = ?) AND (? = '' OR d.scope = ?)
  AND ((? = '' AND ` + decided + `) OR d.status = ?)
  AND (? = '' OR d.created_at >= ?)`

const decisionLogRows = `SELECT d.id, d.question, d.answer, d.scope, d.repo, d.status, d.created_at, d.answered_at,
  d.superseded_at IS NOT NULL,
  (SELECT count(*) FROM decisions r WHERE r.precedent_of != '' AND r.precedent_of = d.id),
  (SELECT max(COALESCE(r.closed_at, r.created_at)) FROM decisions r WHERE r.precedent_of != '' AND r.precedent_of = d.id),
  (SELECT count(*) FROM decisions a WHERE a.near_match = d.id)` + decisionLogWhere + `
ORDER BY d.n DESC LIMIT ?`

const decisionLogSummary = `SELECT count(*), coalesce(sum(EXISTS (SELECT 1 FROM decisions r WHERE r.precedent_of != '' AND r.precedent_of = d.id)), 0)` +
	decisionLogWhere + ` AND ` + decided + ` AND d.superseded_at IS NULL`

// DecisionLog returns the newest decisions q selects with their reuse (the
// count of decisions whose precedent_of is this one, and the last such
// date) and how often a later ticket was raised although this was its
// near_match, and the summary: of the answered decisions that are not
// superseded, how many were reused at least once.
func (s *Store) DecisionLog(ctx context.Context, q DecisionLogQuery) ([]model.DecisionLogRow, model.DecisionLogSummary, error) {
	match := ""
	if strings.TrimSpace(q.Text) != "" {
		if match = precedent.MatchExpr(q.Text); match == "" {
			return nil, model.DecisionLogSummary{}, nil // no searchable word finds nothing
		}
	}
	since := ""
	if !q.Since.IsZero() {
		since = q.Since.UTC().Format(timeLayout)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	args := []any{match, match, q.Repo, q.Repo, q.Scope, q.Scope, string(q.Status), string(q.Status), since, since}
	var sum model.DecisionLogSummary
	if err := s.db.QueryRowContext(ctx, decisionLogSummary, args...).Scan(&sum.Answered, &sum.Reused); err != nil {
		return nil, sum, fail("decision log", "", err)
	}
	rows, err := s.db.QueryContext(ctx, decisionLogRows, append(args, limit)...)
	if err != nil {
		return nil, sum, fail("decision log", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.DecisionLogRow
	for rows.Next() {
		var r model.DecisionLogRow
		var created, answered, last sql.NullString
		if err = rows.Scan(&r.ID, &r.Question, &r.Answer, &r.Scope, &r.Repo, &r.Status, &created, &answered,
			&r.Superseded, &r.Reuse, &last, &r.AskedAgain); err != nil {
			return nil, sum, fail("decision log", "", err)
		}
		for _, f := range []struct {
			dst *time.Time
			src sql.NullString
		}{{&r.CreatedAt, created}, {&r.AnsweredAt, answered}, {&r.LastReuse, last}} {
			if *f.dst, err = parseTime(f.src); err != nil {
				return nil, sum, fail("decision log", r.ID, err)
			}
		}
		out = append(out, r)
	}
	return out, sum, rows.Err()
}

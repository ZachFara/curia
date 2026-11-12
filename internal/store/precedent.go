package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/precedent"
)

// Limits of SearchDecisions.
const (
	defaultSearchLimit = 5
	maxSearchLimit     = 10
)

// PrecedentQuery is a search for decided precedent (plan section 4.6).
type PrecedentQuery struct {
	Text                    string   // untrusted; turned into a MATCH expression here
	Repo, TaskID, SessionID string   // the caller, for scope (section 4.6)
	Tags                    []string // any of these; empty: no tag filter
	ExcludeID               string
	Limit                   int // default 5, at most 10
}

// searchDecisions is the precedent query. Its parameters, all bound: the
// MATCH expression; the caller's repo twice; task id twice; session id
// twice; the number of tags and the tags as a JSON array; the excluded id;
// the limit. Only decided rows not superseded are precedent, and the scope
// rules are section 4.6's table: global always; repo for the same,
// non-empty repo; task for the same, non-empty task id, or while the
// decision has no task, for the same, non-empty session. A tag matches a
// whole stored tag (tags are stored joined by single spaces, so a tag
// with a space in it matches none). bm25's
// weights follow the FTS columns: question, context, rationale, answer,
// answer_reasoning, tags.
const searchDecisions = selectDecisions + `
FROM decisions_fts JOIN decisions d ON d.n = decisions_fts.rowid
WHERE decisions_fts MATCH ?
  AND ` + decided + ` AND d.superseded_at IS NULL
  AND (d.scope = 'global'
    OR (d.scope = 'repo' AND ? != '' AND d.repo = ?)
    OR (d.scope = 'task' AND ((? != '' AND d.task_id = ?)
                           OR (d.task_id = '' AND ? != '' AND d.session_id = ?))))
  AND (? = 0 OR EXISTS (
    SELECT 1 FROM json_each(?) t
    WHERE instr(t.value, ' ') = 0
      AND instr(' ' || d.tags || ' ', ' ' || t.value || ' ') > 0))
  AND d.id != ?
ORDER BY bm25(decisions_fts, 10.0, 2.0, 2.0, 5.0, 3.0, 5.0), d.answered_at DESC
LIMIT ?`

// SearchDecisions returns the decided precedent q finds, best first: the
// rows the full-text index matches for precedent.MatchExpr(q.Text), within
// the caller's scope, carrying any of q.Tags, other than q.ExcludeID, at
// most q.Limit (5 when it is not positive, never more than 10). Tags are
// trimmed and lowercased, as stored tags are, and empty ones dropped; no
// tag left means no tag filter. Text with
// no searchable words finds nothing, and no query runs. The text reaches
// SQLite only as the MatchExpr expression, a bound parameter.
func (s *Store) SearchDecisions(ctx context.Context, q PrecedentQuery) ([]model.Decision, error) {
	match := precedent.MatchExpr(q.Text)
	if match == "" {
		return nil, nil // MATCH '' is an FTS5 syntax error (Ruling 62)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	limit = min(limit, maxSearchLimit)
	tags := searchTags(q.Tags)
	js, err := json.Marshal(tags)
	if err != nil {
		return nil, fail("search decisions", "", err)
	}
	rows, err := s.db.QueryContext(ctx, searchDecisions, match, q.Repo, q.Repo, q.TaskID, q.TaskID,
		q.SessionID, q.SessionID, len(tags), string(js), q.ExcludeID, limit)
	if err != nil {
		return nil, fail("search decisions", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Decision
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, fail("search decisions", "", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("search decisions", "", err)
	}
	return out, nil
}

// searchTags is tags trimmed and lowercased, without the empty ones, never
// nil (so it encodes as a JSON array).
func searchTags(tags []string) []string {
	out := []string{}
	for _, tag := range tags {
		if tag = strings.ToLower(strings.TrimSpace(tag)); tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

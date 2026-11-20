package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// Limits of Timeline.
const (
	defaultTimelineLimit = 100
	maxTimelineLimit     = 1000
	timelineEnd          = "9999-12-31T23:59:59.999999999Z"
)

// timelineSource is one source of the union, as SQL expressions over its
// FROM. at must be a plain indexed column, so each source is a bounded read
// of an index (TestTimelineQueryUsesIndexes). A source of a feature built
// later (modes, permission events, schedule runs) is one more entry here.
type timelineSource struct {
	from, where                        string
	at, ref, session, task, repo, kind string
	line                               string
}

var timelineSources = []timelineSource{
	{
		from: "sessions s", where: "1",
		ref: "s.id", at: "s.started_at", session: "s.id", task: "COALESCE(s.task_id, '')", repo: "COALESCE(s.repo, '')",
		kind: "'session_start'", line: "s.agent || ' session started'",
	},
	{
		from: "sessions s", where: "s.state = 'ended'",
		ref: "s.id", at: "s.updated_at", session: "s.id", task: "COALESCE(s.task_id, '')", repo: "COALESCE(s.repo, '')",
		kind: "'session_end'", line: "'session ended' || COALESCE(' (' || s.end_reason || ')', '')",
	},
	{
		from: "events e LEFT JOIN sessions s ON s.id = e.session_id", where: "e.kind = 'Stop'",
		ref: "CAST(e.id AS TEXT)", at: "e.at", session: "e.session_id", task: "COALESCE(s.task_id, '')", repo: "COALESCE(s.repo, '')",
		kind: "'turn_end'",
		line: "COALESCE(NULLIF(CASE WHEN json_valid(e.payload) THEN json_extract(e.payload, '$.last_assistant_message') END, ''), 'turn ended')",
	},
	{
		from: "decisions d", where: "1",
		ref: "d.id", at: "d.created_at", session: "d.session_id", task: "d.task_id", repo: "d.repo",
		kind: "'ticket_raised'", line: "d.id || ' ' || d.mode || ': ' || d.question",
	},
	{
		from: "decisions d", where: "1", // answered_at is set only by an answer
		ref: "d.id", at: "d.answered_at", session: "d.session_id", task: "d.task_id", repo: "d.repo",
		kind: "d.status", line: "d.id || ': ' || d.answer",
	},
	{
		from: "reviews r", where: "1",
		ref: "r.id", at: "r.created_at", session: "r.session_id", task: "r.task_id",
		repo: "COALESCE((SELECT t.repo FROM tasks t WHERE t.id = r.task_id), '')",
		kind: "'review_opened'", line: "r.id || ' ' || r.summary",
	},
	{
		from: "reviews r", where: "1",
		ref: "r.id", at: "r.closed_at", session: "r.session_id", task: "r.task_id",
		repo: "COALESCE((SELECT t.repo FROM tasks t WHERE t.id = r.task_id), '')",
		kind: "'review_closed'", line: "r.id || ' ' || r.status",
	},
	{
		from: "messages m", where: "m.status = 'delivered'",
		ref: "m.id", at: "m.delivered_at", session: "m.target_session", task: "m.target_task",
		repo: "COALESCE(NULLIF(m.target_repo, ''), (SELECT s.repo FROM sessions s WHERE s.id = m.target_session), '')",
		kind: "'message_delivered'", line: "m.id || ' ' || m.kind || ': ' || m.text",
	},
	{
		from: "tasks t", where: "1",
		ref: "t.id", at: "t.created_at", session: "''", task: "t.id", repo: "t.repo",
		kind: "'task_filed'", line: "t.id || ' ' || t.title",
	},
	{
		from: "pushes p", where: "p.status = 'sent'",
		ref: "CAST(p.n AS TEXT)", at: "p.sent_at", session: "''",
		task: "COALESCE((SELECT d.task_id FROM decisions d WHERE d.id = p.ref), (SELECT r.task_id FROM reviews r WHERE r.id = p.ref), '')",
		repo: "COALESCE((SELECT d.repo FROM decisions d WHERE d.id = p.ref), (SELECT t.repo FROM reviews r JOIN tasks t ON t.id = r.task_id WHERE r.id = p.ref), '')",
		kind: "'push_sent'", line: "p.ref || ' sent to ' || p.device",
	},
}

// timelineSQL is the union: per source the newest ?5 rows in [?1, ?2] that
// pass the repo (?3) and task (?4) filters and lie before the cursor, then
// the newest ?5 of all. Order is time, then kind, then ref (a row's own id,
// as text), which is unique per event: the cursor is that triple, so events
// that share a time are never skipped or repeated at a page edge. At the
// cursor's own time only the rows after (?6 kind, ?7 ref) count, and only
// when ?8 says there is a cursor; else ?2 is an exclusive bound.
var timelineSQL = func() string {
	var parts []string
	for _, s := range timelineSources {
		parts = append(parts, `SELECT * FROM (SELECT `+s.at+` AS at, `+s.ref+` AS ref, `+s.session+` AS session, `+s.task+` AS task, `+s.kind+` AS kind, substr(`+s.line+`, 1, 300) AS line
FROM `+s.from+` WHERE `+s.at+` >= ?1 AND `+s.at+` <= ?2 AND (`+s.at+` < ?2 OR (?8 AND (`+s.kind+`, `+s.ref+`) > (?6, ?7))) AND `+s.where+`
  AND (?3 = '' OR `+s.repo+` = ?3) AND (?4 = '' OR `+s.task+` = ?4)
ORDER BY `+s.at+` DESC, `+s.kind+`, `+s.ref+` LIMIT ?5)`)
	}
	return `SELECT at, ref, session, task, kind, line FROM (` + strings.Join(parts, "\nUNION ALL\n") + `)
ORDER BY at DESC, kind, ref LIMIT ?5`
}()

// TimelineQuery selects timeline events.
type TimelineQuery struct {
	Since, Before time.Time           // [Since, Before); a zero one is open
	Repo, TaskID  string              // "": any
	Cursor        model.TimelineEvent // when its At is set: only events after it in the order (older); it wins over Before
	Limit         int                 // default 100, at most 1000
}

// Timeline returns the newest events of the union over existing rows (F25,
// Ruling 160; no table of its own), newest first. Page backwards by
// passing the last event as Cursor.
func (s *Store) Timeline(ctx context.Context, q TimelineQuery) ([]model.TimelineEvent, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultTimelineLimit
	}
	since, before := "", timelineEnd
	if !q.Since.IsZero() {
		since = q.Since.UTC().Format(timeLayout)
	}
	if !q.Before.IsZero() {
		before = q.Before.UTC().Format(timeLayout)
	}
	tie := !q.Cursor.At.IsZero()
	if tie {
		before = q.Cursor.At.UTC().Format(timeLayout)
	}
	rows, err := s.db.QueryContext(ctx, timelineSQL, since, before, q.Repo, q.TaskID, min(limit, maxTimelineLimit), q.Cursor.Kind, q.Cursor.Ref, tie)
	if err != nil {
		return nil, fail("timeline", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.TimelineEvent
	for rows.Next() {
		var e model.TimelineEvent
		var at string
		if err = rows.Scan(&at, &e.Ref, &e.SessionID, &e.TaskID, &e.Kind, &e.Line); err != nil {
			return nil, fail("timeline", "", err)
		}
		if e.At, err = time.Parse(timeLayout, at); err != nil {
			return nil, fail("timeline", "", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("timeline", "", err)
	}
	return out, nil
}

// TimelineBetween is what `curia back`'s summary (F20) calls: the events in
// [from, to), grouped by kind, each group oldest first. shortcut: at most
// the newest maxTimelineLimit events, which is far above one away window's.
func (s *Store) TimelineBetween(ctx context.Context, from, to time.Time) (map[string][]model.TimelineEvent, error) {
	evs, err := s.Timeline(ctx, TimelineQuery{Since: from, Before: to, Limit: maxTimelineLimit})
	if err != nil {
		return nil, err
	}
	out := map[string][]model.TimelineEvent{}
	for i := len(evs) - 1; i >= 0; i-- {
		out[evs[i].Kind] = append(out[evs[i].Kind], evs[i])
	}
	return out, nil
}

// maxTimelineText bounds TimelineText.
const maxTimelineText = 8000

// TimelineText is the stored text of one event whole (at most
// maxTimelineText characters, uncleaned: a view cleans it), where its Line
// is cut to 300. "" when the row is gone.
func (s *Store) TimelineText(ctx context.Context, e model.TimelineEvent) (string, error) {
	for _, src := range timelineSources {
		var text string
		err := s.db.QueryRowContext(ctx, `SELECT substr(`+src.line+`, 1, ?3) FROM `+src.from+
			` WHERE `+src.ref+` = ?1 AND `+src.kind+` = ?2 AND `+src.where, e.Ref, e.Kind, maxTimelineText).Scan(&text)
		if err == nil {
			return text, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fail("timeline text", e.Ref, err)
		}
	}
	return "", nil
}

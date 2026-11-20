package store

import (
	"context"
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
	from, where                   string
	at, session, task, repo, kind string
	line                          string
}

// tl bounds an expression to the line length.
func tl(expr string) string { return "substr(" + expr + ", 1, 300)" }

var timelineSources = []timelineSource{
	{
		from: "sessions s", where: "1",
		at: "s.started_at", session: "s.id", task: "COALESCE(s.task_id, '')", repo: "COALESCE(s.repo, '')",
		kind: "'session_start'", line: tl("s.agent || ' session started'"),
	},
	{
		from: "sessions s", where: "s.state = 'ended'",
		at: "s.updated_at", session: "s.id", task: "COALESCE(s.task_id, '')", repo: "COALESCE(s.repo, '')",
		kind: "'session_end'", line: tl("'session ended' || COALESCE(' (' || s.end_reason || ')', '')"),
	},
	{
		from: "events e LEFT JOIN sessions s ON s.id = e.session_id", where: "e.kind = 'Stop'",
		at: "e.at", session: "e.session_id", task: "COALESCE(s.task_id, '')", repo: "COALESCE(s.repo, '')",
		kind: "'turn_end'",
		line: tl("COALESCE(NULLIF(CASE WHEN json_valid(e.payload) THEN json_extract(e.payload, '$.last_assistant_message') END, ''), 'turn ended')"),
	},
	{
		from: "decisions d", where: "1",
		at: "d.created_at", session: "d.session_id", task: "d.task_id", repo: "d.repo",
		kind: "'ticket_raised'", line: tl("d.id || ' ' || d.mode || ': ' || d.question"),
	},
	{
		from: "decisions d", where: "1", // answered_at is set only by an answer
		at: "d.answered_at", session: "d.session_id", task: "d.task_id", repo: "d.repo",
		kind: "d.status", line: tl("d.id || ': ' || d.answer"),
	},
	{
		from: "reviews r", where: "1",
		at: "r.created_at", session: "r.session_id", task: "r.task_id",
		repo: "COALESCE((SELECT t.repo FROM tasks t WHERE t.id = r.task_id), '')",
		kind: "'review_opened'", line: tl("r.id || ' ' || r.summary"),
	},
	{
		from: "reviews r", where: "1",
		at: "r.closed_at", session: "r.session_id", task: "r.task_id",
		repo: "COALESCE((SELECT t.repo FROM tasks t WHERE t.id = r.task_id), '')",
		kind: "'review_closed'", line: tl("r.id || ' ' || r.status"),
	},
	{
		from: "messages m", where: "m.status = 'delivered'",
		at: "m.delivered_at", session: "m.target_session", task: "m.target_task",
		repo: "COALESCE(NULLIF(m.target_repo, ''), (SELECT s.repo FROM sessions s WHERE s.id = m.target_session), '')",
		kind: "'message_delivered'", line: tl("m.id || ' ' || m.kind || ': ' || m.text"),
	},
	{
		from: "tasks t", where: "1",
		at: "t.created_at", session: "''", task: "t.id", repo: "t.repo",
		kind: "'task_filed'", line: tl("t.id || ' ' || t.title"),
	},
	{
		from: "pushes p", where: "p.status = 'sent'",
		at: "p.sent_at", session: "''",
		task: "COALESCE((SELECT d.task_id FROM decisions d WHERE d.id = p.ref), (SELECT r.task_id FROM reviews r WHERE r.id = p.ref), '')",
		repo: "COALESCE((SELECT d.repo FROM decisions d WHERE d.id = p.ref), (SELECT t.repo FROM reviews r JOIN tasks t ON t.id = r.task_id WHERE r.id = p.ref), '')",
		kind: "'push_sent'", line: tl("p.ref || ' sent to ' || p.device"),
	},
}

// timelineSQL is the union: per source the newest ?5 rows in [?1, ?2) that
// pass the repo (?3) and task (?4) filters, then the newest ?5 of all.
var timelineSQL = func() string {
	var parts []string
	for _, s := range timelineSources {
		parts = append(parts, `SELECT * FROM (SELECT `+s.at+` AS at, `+s.session+` AS session, `+s.task+` AS task, `+s.kind+` AS kind, `+s.line+` AS line
FROM `+s.from+` WHERE `+s.at+` >= ?1 AND `+s.at+` < ?2 AND `+s.where+`
  AND (?3 = '' OR `+s.repo+` = ?3) AND (?4 = '' OR `+s.task+` = ?4)
ORDER BY `+s.at+` DESC LIMIT ?5)`)
	}
	return `SELECT at, session, task, kind, line FROM (` + strings.Join(parts, "\nUNION ALL\n") + `)
ORDER BY at DESC, kind, session, line LIMIT ?5`
}()

// TimelineQuery selects timeline events.
type TimelineQuery struct {
	Since, Before time.Time // [Since, Before); a zero one is open
	Repo, TaskID  string    // "": any
	Limit         int       // default 100, at most 1000
}

// Timeline returns the newest events of the union over existing rows (F25,
// Ruling 160; no table of its own), newest first. Page backwards by
// passing the last event's time as Before.
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
	rows, err := s.db.QueryContext(ctx, timelineSQL, since, before, q.Repo, q.TaskID, min(limit, maxTimelineLimit))
	if err != nil {
		return nil, fail("timeline", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.TimelineEvent
	for rows.Next() {
		var e model.TimelineEvent
		var at string
		if err = rows.Scan(&at, &e.SessionID, &e.TaskID, &e.Kind, &e.Line); err != nil {
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

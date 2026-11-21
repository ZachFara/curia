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
// later is one more entry here. Mode changes are recorded nowhere, so they have no source.
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
	{
		// F26 part 2: what a web device did, recorded as a WebAction event.
		from: "events e", where: "e.kind = 'WebAction' AND json_valid(e.payload)",
		ref: "CAST(e.id AS TEXT)", at: "e.at", session: "''", task: "COALESCE(json_extract(e.payload, '$.task'), '')", repo: "''",
		kind: "'web_action'", line: "COALESCE(json_extract(e.payload, '$.device'), '') || ': ' || COALESCE(json_extract(e.payload, '$.action'), '')",
	},
	{
		from: "schedule_runs r", where: "1", // F22: one line per fire or skipped fire
		ref: "CAST(r.id AS TEXT)", at: "r.ran_at", session: "''", task: "''", repo: "''",
		kind: "'schedule_run'", line: "r.schedule_id || ' ' || r.outcome",
	},
	{
		from: "auto_spawn_runs a", where: "1",
		ref: "CAST(a.id AS TEXT)", at: "a.at", session: "a.session_id", task: "a.task_id",
		repo: "COALESCE((SELECT t.repo FROM tasks t WHERE t.id = a.task_id), '')",
		kind: "'auto_spawn'", line: "'auto-spawn ' || a.outcome || CASE WHEN a.check_ = '' THEN '' ELSE ' (' || a.check_ || ')' END || ': ' || a.task_id || ' ' || a.detail",
	},
	{
		from: "away_runs a", where: "1", // F20: an away period's start and end
		ref: "CAST(a.id AS TEXT)", at: "a.started_at", session: "''", task: "''", repo: "''",
		kind: "'away_start'", line: "'away started' || CASE WHEN a.scope = '' THEN '' ELSE ' (' || a.scope || ')' END",
	},
	{
		from: "away_runs a", where: "1", // a NULL ended_at never lies in a window
		ref: "CAST(a.id AS TEXT)", at: "a.ended_at", session: "''", task: "''", repo: "''",
		kind: "'away_end'", line: "'away ended'",
	},
	{
		// One line per stored counted row (a distinct tool and command) with its count, at its latest time.
		from: "permission_events p", where: "p.verdict = 'deny'",
		ref: "CAST(p.id AS TEXT)", at: "p.last_at", session: "p.session_id", task: "''", repo: "''",
		kind: "'action_denied'", line: "'denied ' || p.tool || ' x' || p.n || ': ' || substr(p.summary, 1, 100)",
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

// timelineCountSQL is, per source (kind and from), its count per session in [?1, ?2).
var timelineCountSQL = func() map[string]string {
	m := map[string]string{}
	for _, src := range timelineSources {
		m[src.kind+src.from] = `SELECT ` + src.session + `, count(*) FROM ` + src.from +
			` WHERE ` + src.at + ` >= ?1 AND ` + src.at + ` < ?2 AND ` + src.where + ` GROUP BY 1`
	}
	return m
}()

// TimelineQuery selects timeline events.
type TimelineQuery struct {
	Since, Before time.Time           // [Since, Before); a zero one is open
	Repo, TaskID  string              // "": any
	Cursor        model.TimelineEvent // when its At is set: only events after it in the order (older); it wins over Before
	Limit         int                 // default 100, at most 1000
}

// TimelineResult is a read of the timeline: the events and how many rows
// were left out because their time could not be read.
type TimelineResult struct {
	Events  []model.TimelineEvent
	Skipped int
}

// Timeline returns the newest events of the union over existing rows (F25,
// Ruling 160; no table of its own), newest first. Page backwards by
// passing the last event as Cursor. Unreadable rows are skipped (TimelineRead).
func (s *Store) Timeline(ctx context.Context, q TimelineQuery) ([]model.TimelineEvent, error) {
	r, err := s.TimelineRead(ctx, q)
	return r.Events, err
}

// TimelineRead is Timeline that also counts the rows it skipped because
// their time is not in the stored layout (a hand edit): one such row never
// fails the whole read.
func (s *Store) TimelineRead(ctx context.Context, q TimelineQuery) (TimelineResult, error) {
	var out TimelineResult
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
		return out, fail("timeline", "", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e model.TimelineEvent
		var at string
		if err = rows.Scan(&at, &e.Ref, &e.SessionID, &e.TaskID, &e.Kind, &e.Line); err != nil {
			return out, fail("timeline", "", err)
		}
		if e.At, err = time.Parse(timeLayout, at); err != nil {
			out.Skipped++
			continue
		}
		out.Events = append(out.Events, e)
	}
	if err := rows.Err(); err != nil {
		return out, fail("timeline", "", err)
	}
	return out, nil
}

// TimelineCounts is what `curia back`'s summary (F20) reads: for the events
// in [from, to) of the given kinds, the count per session and kind, from
// COUNT queries over the window (never a capped list).
func (s *Store) TimelineCounts(ctx context.Context, from, to time.Time, kinds ...string) (map[string]map[string]int, error) {
	out := map[string]map[string]int{}
	for _, kind := range kinds {
		for _, src := range timelineSources {
			if src.kind == "'"+kind+"'" {
				if err := s.countSource(ctx, src, kind, formatTime(from), formatTime(to), out); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

// countSource adds src's per-session counts in [from, to) to out[session][kind].
func (s *Store) countSource(ctx context.Context, src timelineSource, kind string, from, to any, out map[string]map[string]int) error {
	rows, err := s.db.QueryContext(ctx, timelineCountSQL[src.kind+src.from], from, to)
	if err != nil {
		return fail("timeline counts", kind, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var n int
		if err = rows.Scan(&id, &n); err != nil {
			return fail("timeline counts", kind, err)
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][kind] += n
	}
	if err = rows.Err(); err != nil {
		return fail("timeline counts", kind, err)
	}
	return nil
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

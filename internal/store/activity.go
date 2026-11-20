package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/usage"
)

// ActivityEvents returns the stored hook events at or after since that F27
// activity time reads, per session in order. sessionID "" means every
// session. The tool name and the first 200 characters of a Bash command
// come out of the raw payload (a payload that is not JSON gives neither),
// for a PreToolUse only; the command is returned for classification and
// never stored again. For background commands it also reads, typed and
// bounded, the call's run_in_background and tool_use_id, the PostToolUse's
// backgroundTaskId, a Stop's background_tasks ids, and the first 400 bytes
// of a prompt (a task notification names its id there). Nothing is written.
func (s *Store) ActivityEvents(ctx context.Context, since time.Time, sessionID string) (map[string][]usage.ActivityEvent, error) {
	// p is the payload when it is JSON, else NULL (every json_ call on NULL
	// is NULL). Every field is typed and bounded here: a hostile value of
	// another type or length reads as absent.
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, kind, at,
  CASE WHEN kind = ?1 THEN json_extract(p, '$.tool_name') END,
  CASE WHEN kind = ?1 THEN substr(json_extract(p, '$.tool_input.command'), 1, 200) END,
  CASE WHEN kind = ?1 AND json_type(p, '$.tool_input.run_in_background') = 'true' THEN 1 ELSE 0 END,
  CASE WHEN kind IN (?1, ?2) AND json_type(p, '$.tool_use_id') = 'text' AND length(json_extract(p, '$.tool_use_id')) <= 100
       THEN json_extract(p, '$.tool_use_id') END,
  CASE WHEN kind = ?2 AND json_type(p, '$.tool_response.backgroundTaskId') = 'text' AND length(json_extract(p, '$.tool_response.backgroundTaskId')) <= 64
       THEN json_extract(p, '$.tool_response.backgroundTaskId') END,
  CASE WHEN kind = ?3 AND json_type(p, '$.background_tasks') = 'array' THEN
       COALESCE((SELECT group_concat(id, char(31)) FROM (
         SELECT json_extract(j.value, '$.id') AS id FROM json_each(p, '$.background_tasks') j
         WHERE j.type = 'object' AND json_type(j.value, '$.id') = 'text' AND length(json_extract(j.value, '$.id')) <= 64 LIMIT 64)), '') END,
  CASE WHEN kind = ?4 AND json_type(p, '$.prompt') = 'text' THEN substr(json_extract(p, '$.prompt'), 1, 400) END
FROM (SELECT session_id, kind, at, id, CASE WHEN json_valid(payload) THEN payload END AS p FROM events
      WHERE at >= ?5 AND kind IN (?6, ?4, ?1, ?2, ?7, ?3, ?8) AND (?9 = '' OR session_id = ?9))
ORDER BY session_id, id`,
		model.PreToolUse, model.PostToolUse, model.Stop, model.UserPromptSubmit, formatTime(since),
		model.SessionStart, model.Notification, model.SessionEnd, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: read activity events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]usage.ActivityEvent{}
	for rows.Next() {
		var id, kind, at string
		var tool, cmd, use, task, listed, prompt any
		var bg int
		if err = rows.Scan(&id, &kind, &at, &tool, &cmd, &bg, &use, &task, &listed, &prompt); err != nil {
			return nil, fmt.Errorf("store: read activity events: %w", err)
		}
		t, perr := time.Parse(timeLayout, at)
		if perr != nil {
			return nil, fmt.Errorf("store: read activity events: %w", perr)
		}
		ev := usage.ActivityEvent{Kind: model.EventKind(kind), At: t, Background: bg == 1}
		ev.Tool, _ = tool.(string)
		ev.Command, _ = cmd.(string)
		ev.ToolUseID, _ = use.(string)
		ev.TaskID, _ = task.(string)
		ev.Prompt, _ = prompt.(string)
		if l, ok := listed.(string); ok {
			ev.HasList = true
			if l != "" {
				ev.Listed = strings.Split(l, "\x1f")
			}
		}
		out[id] = append(out[id], ev)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read activity events: %w", err)
	}
	return out, nil
}

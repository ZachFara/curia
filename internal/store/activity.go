package store

import (
	"context"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/usage"
)

// ActivityEvents returns the stored hook events at or after since that F27
// activity time reads, per session in order. sessionID "" means every
// session. The tool name and the first 200 characters of a Bash command
// come out of the raw payload (a payload that is not JSON gives neither),
// for a PreToolUse only; the command is returned for classification and
// never stored again. Nothing is written.
func (s *Store) ActivityEvents(ctx context.Context, since time.Time, sessionID string) (map[string][]usage.ActivityEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, kind, at,
  CASE WHEN kind = ? AND json_valid(payload) THEN json_extract(payload, '$.tool_name') END,
  CASE WHEN kind = ? AND json_valid(payload) THEN substr(json_extract(payload, '$.tool_input.command'), 1, 200) END
FROM events
WHERE at >= ? AND kind IN (?, ?, ?, ?, ?, ?, ?) AND (? = '' OR session_id = ?)
ORDER BY session_id, id`,
		model.PreToolUse, model.PreToolUse, formatTime(since),
		model.SessionStart, model.UserPromptSubmit, model.PreToolUse, model.PostToolUse, model.Notification, model.Stop, model.SessionEnd,
		sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: read activity events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]usage.ActivityEvent{}
	for rows.Next() {
		var id, kind, at string
		var tool, cmd any
		if err = rows.Scan(&id, &kind, &at, &tool, &cmd); err != nil {
			return nil, fmt.Errorf("store: read activity events: %w", err)
		}
		t, perr := time.Parse(timeLayout, at)
		if perr != nil {
			return nil, fmt.Errorf("store: read activity events: %w", perr)
		}
		ev := usage.ActivityEvent{Kind: model.EventKind(kind), At: t}
		ev.Tool, _ = tool.(string)
		ev.Command, _ = cmd.(string)
		out[id] = append(out[id], ev)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read activity events: %w", err)
	}
	return out, nil
}

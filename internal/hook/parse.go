// Package hook turns one Claude Code hook event into a session update: it
// parses the payload, folds in what the transcript says, applies the state
// machine and writes the result to the store.
//
// Payloads are untrusted. Parse tolerates missing and wrongly typed fields
// and rejects only what it cannot attribute to a session.
package hook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// maxToolTargetRunes caps Event.ToolTarget.
const maxToolTargetRunes = 60

// knownKinds are the events Parse accepts.
var knownKinds = map[model.EventKind]bool{
	model.SessionStart: true, model.UserPromptSubmit: true, model.PreToolUse: true,
	model.PostToolUse: true, model.Notification: true, model.Stop: true,
	model.SubagentStop: true, model.SessionEnd: true,
}

// Parse reads a hook payload into an Event of the given kind, in pane, at
// now. It reads session_id, cwd, transcript_path, tool_name, message and
// notification_type, on SessionStart model and source, on UserPromptSubmit
// prompt, on Stop last_assistant_message and the running subagents of
// background_tasks, and on SubagentStop whether agent_type is present and
// empty;
// ToolTarget is the first present of tool_input.file_path,
// tool_input.pattern or tool_input.command, cut to 60 runes. A field that is
// missing or not a string reads as empty. Parse fails on an unknown kind, a
// payload that is not a JSON object, or a missing session_id.
func Parse(kind model.EventKind, payload []byte, pane string, now time.Time) (model.Event, error) {
	if !knownKinds[kind] {
		return model.Event{}, fmt.Errorf("unknown hook event %q", kind)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return model.Event{}, fmt.Errorf("%s payload: %w", kind, err)
	}
	if fields == nil { // the payload was JSON null
		return model.Event{}, fmt.Errorf("%s payload is not a JSON object", kind)
	}
	ev := model.Event{
		Kind:             kind,
		SessionID:        str(fields["session_id"]),
		Pane:             pane,
		Cwd:              str(fields["cwd"]),
		TranscriptPath:   str(fields["transcript_path"]),
		ToolName:         str(fields["tool_name"]),
		ToolTarget:       toolTarget(fields["tool_input"]),
		Message:          str(fields["message"]),
		NotificationType: str(fields["notification_type"]),
		At:               now,
	}
	switch kind {
	case model.SessionStart:
		ev.Model, ev.Source = str(fields["model"]), str(fields["source"])
	case model.UserPromptSubmit:
		ev.Prompt = str(fields["prompt"])
	case model.Stop:
		ev.LastAssistantMessage = str(fields["last_assistant_message"])
		ev.RunningSubagents, ev.BackgroundKnown = runningSubagents(fields["background_tasks"])
	case model.SubagentStop:
		// Claude Code's helper agents stop with an empty agent_type; an
		// Agent or Task tool's subagent names its type (Ruling 87).
		raw, present := fields["agent_type"]
		ev.HelperAgent = present && str(raw) == ""
	}
	if ev.SessionID == "" {
		return model.Event{}, fmt.Errorf("%s payload has no session_id", kind)
	}
	return ev, nil
}

// runningSubagents counts the entries of a Stop's background_tasks whose
// type is "subagent" and status "running", and reports whether raw is such
// a list at all.
func runningSubagents(raw json.RawMessage) (int, bool) {
	var tasks []map[string]json.RawMessage
	if json.Unmarshal(raw, &tasks) != nil || tasks == nil { // absent, null or not a list
		return 0, false
	}
	n := 0
	for _, t := range tasks {
		if str(t["type"]) == "subagent" && str(t["status"]) == "running" {
			n++
		}
	}
	return n, true
}

// str returns raw as a string, or "" if it is absent or not a JSON string.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func toolTarget(raw json.RawMessage) string {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		return ""
	}
	for _, key := range []string{"file_path", "pattern", "command"} {
		if s := str(input[key]); s != "" {
			if r := []rune(s); len(r) > maxToolTargetRunes {
				return string(r[:maxToolTargetRunes])
			}
			return s
		}
	}
	return ""
}

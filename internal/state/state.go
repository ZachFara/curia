// Package state is the pure session state machine: one hook event in, the
// next session row out.
package state

import "github.com/ZachFara/curia/internal/model"

const maxLastUpdateRunes = 80

// sourceCompact is the SessionStart source Claude Code sends after a
// compaction.
const sourceCompact = "compact"

// Apply returns s after applying ev. openDecision reports whether the
// session has an open blocking decision ticket, which turns a Stop into
// needs_decision unless ev.StopBlocked (the hook is blocking the Stop to
// deliver answers), which makes it working.
// A SessionStart starts the session afresh, except a compaction's
// (Compaction) for a stored session, which keeps it as it was.
// It is pure: no I/O and no clock. An ended session ignores SubagentStop and
// SessionEnd and is returned unchanged (see Ignores); any other event is
// applied as usual and brings it back.
func Apply(s model.Session, ev model.Event, openDecision bool) model.Session {
	if Ignores(s, ev) {
		return s
	}
	if s.ID == "" && ev.Kind != model.SessionStart {
		start(&s, ev) // first seen mid-run; SessionStart starts below
	}
	s.UpdatedAt = ev.At

	switch ev.Kind {
	case model.SessionStart:
		sessionStart(&s, ev)
	case model.UserPromptSubmit:
		s.State = model.StateWorking
		s.SeenAt = ev.At
	case model.PreToolUse:
		s.State = model.StateWorking
		s.LastUpdate = toolUpdate(ev)
		countSubagent(&s, ev)
	case model.PostToolUse:
		s.State = model.StateWorking
		s.LastUpdate = toolUpdate(ev)
	case model.Notification:
		if informational(ev) {
			// Only for a session first seen now (Ignores covers stored
			// ones): it sits at its prompt.
			s.State = model.StateIdle
			break
		}
		s.State = model.StateWaiting
		s.LastUpdate = truncate(ev.Message)
	case model.Stop:
		stop(&s, ev, openDecision)
	case model.SubagentStop:
		if s.SubagentsActive > 0 {
			s.SubagentsActive--
		}
	case model.SessionEnd:
		s.State = model.StateEnded
	}
	return s
}

// Ignores reports whether Apply returns s unchanged for ev. An ended session
// ignores SubagentStop and SessionEnd; any other event means the session is
// alive (for example one ended by a nested session's SessionStart in its
// pane), so it is applied. A stored session also ignores an informational
// Notification (see informational).
func Ignores(s model.Session, ev model.Event) bool {
	if s.ID != "" && informational(ev) {
		return true
	}
	return s.State == model.StateEnded && (ev.Kind == model.SubagentStop || ev.Kind == model.SessionEnd)
}

// informational reports whether ev is a Notification that asks nothing of
// the user: idle_prompt, sent when the prompt has sat idle for about a
// minute, and auth_success (Ruling 31). Any other type, or none, means the
// session waits for input.
func informational(ev model.Event) bool {
	return ev.Kind == model.Notification && (ev.NotificationType == "idle_prompt" || ev.NotificationType == "auth_success")
}

func countSubagent(s *model.Session, ev model.Event) {
	if ev.ToolName == "Task" || ev.ToolName == "Agent" {
		s.SubagentsActive++
	}
}

// sessionStart starts s from ev. A new or /cleared session sits at an empty
// prompt the user just opened, so it is idle and seen (Ruling 37); its first
// prompt makes it working. A model ev names stands until a transcript read
// replaces it. A compaction's SessionStart for a stored session is not a
// start: see compacted.
func sessionStart(s *model.Session, ev model.Event) {
	if s.ID != "" && Compaction(ev) {
		compacted(s, ev)
		return
	}
	start(s, ev)
	s.State = model.StateIdle
	s.SeenAt = ev.At
	s.SubagentsActive = 0
	if ev.Model != "" {
		s.Model = ev.Model
	}
}

// stop ends a turn. A Stop the hook blocks to deliver answers does not
// end it: Claude goes on with the answers, so the session is working.
// Otherwise an open blocking ticket makes it needs_decision; with none it
// is idle.
func stop(s *model.Session, ev model.Event, openDecision bool) {
	switch {
	case ev.StopBlocked:
		s.State = model.StateWorking
	case openDecision:
		s.State = model.StateNeedsDecision
	default:
		s.State = model.StateIdle
	}
	if ev.Message != "" {
		s.LastUpdate = truncate(ev.Message)
	}
}

// start copies the session identity from ev, as SessionStart does. The
// transcript offset belongs to the old transcript, so it restarts at 0 when
// the path changes; a session with no path yet keeps it, because the hook
// read ev's transcript up to it.
func start(s *model.Session, ev model.Event) {
	if s.TranscriptPath != "" && s.TranscriptPath != ev.TranscriptPath {
		s.TranscriptOffset = 0
	}
	s.ID = ev.SessionID
	s.Agent = "claude"
	s.Pane = ev.Pane
	s.Cwd = ev.Cwd
	s.TranscriptPath = ev.TranscriptPath
	s.StartedAt = ev.At
}

func toolUpdate(ev model.Event) string {
	if ev.ToolTarget == "" {
		return truncate(ev.ToolName)
	}
	return truncate(ev.ToolName + " " + ev.ToolTarget)
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= maxLastUpdateRunes {
		return s
	}
	return string(r[:maxLastUpdateRunes])
}

// Compaction reports whether ev is the SessionStart Claude Code fires after
// compacting the conversation (source "compact"), which can come in the
// middle of a turn (Ruling 74).
func Compaction(ev model.Event) bool {
	return ev.Kind == model.SessionStart && ev.Source == sourceCompact
}

// compacted applies a compaction's SessionStart to a stored session: the
// session goes on as it was, so its state, seen time, start and subagent
// count stay. Only a changed transcript path (whose offset restarts) and a
// named model are taken; Apply has set UpdatedAt.
func compacted(s *model.Session, ev model.Event) {
	if ev.TranscriptPath != "" && ev.TranscriptPath != s.TranscriptPath {
		s.TranscriptPath, s.TranscriptOffset = ev.TranscriptPath, 0
	}
	if ev.Model != "" {
		s.Model = ev.Model
	}
}

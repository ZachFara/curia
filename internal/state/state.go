// Package state is the pure session state machine: one hook event in, the
// next session row out.
package state

import "github.com/ZachFara/curia/internal/model"

const maxLastUpdateRunes = 80

// Apply returns s after applying ev. openDecision reports whether the
// transcript ends in an open decision, which turns a Stop into needs_decision.
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
	case model.PermissionRequest:
		// The permission dialog is open; the permission_prompt Notification
		// follows about 6 s later.
		s.State = model.StateWaiting
		s.LastUpdate = toolUpdate(ev)
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
// replaces it.
func sessionStart(s *model.Session, ev model.Event) {
	start(s, ev)
	s.State = model.StateIdle
	s.SeenAt = ev.At
	s.SubagentsActive = 0
	if ev.Model != "" {
		s.Model = ev.Model
	}
}

func stop(s *model.Session, ev model.Event, openDecision bool) {
	s.State = model.StateIdle
	if openDecision {
		s.State = model.StateNeedsDecision
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

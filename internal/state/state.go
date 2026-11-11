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
	if Ignores(s, ev.Kind) {
		return s
	}
	if s.ID == "" {
		start(&s, ev)
	}
	s.UpdatedAt = ev.At

	switch ev.Kind {
	case model.SessionStart:
		start(&s, ev)
		s.State = model.StateWorking
		s.SubagentsActive = 0
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

// Ignores reports whether Apply returns s unchanged for an event of kind: an
// ended session ignores SubagentStop and SessionEnd. Any other event means
// the session is alive, for example one ended by a nested session's
// SessionStart in its pane, so it is applied.
func Ignores(s model.Session, kind model.EventKind) bool {
	return s.State == model.StateEnded && (kind == model.SubagentStop || kind == model.SessionEnd)
}

func countSubagent(s *model.Session, ev model.Event) {
	if ev.ToolName == "Task" || ev.ToolName == "Agent" {
		s.SubagentsActive++
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

// start copies the session identity from ev, as SessionStart does.
func start(s *model.Session, ev model.Event) {
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

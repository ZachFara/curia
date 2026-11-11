// Package state is the pure session state machine: one hook event in, the
// next session row out.
package state

import "github.com/ZachFara/curia/internal/model"

const maxLastUpdateRunes = 80

// Apply returns s after applying ev. openDecision reports whether the
// transcript ends in an open decision, which turns a Stop into needs_decision.
// It is pure: no I/O and no clock. An ended session ignores every event
// except SessionStart and is returned unchanged.
func Apply(s model.Session, ev model.Event, openDecision bool) model.Session {
	if s.State == model.StateEnded && ev.Kind != model.SessionStart {
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

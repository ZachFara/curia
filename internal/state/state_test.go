package state_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/state"
)

var (
	t0 = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Minute)
)

func base() model.Session {
	return model.Session{
		ID: "s1", Agent: "claude", Pane: "%1", Cwd: "/w", TranscriptPath: "/t",
		State: model.StateIdle, StartedAt: t0, UpdatedAt: t0, SeenAt: t0,
	}
}

func TestApplyTransitions(t *testing.T) {
	tests := []struct {
		name string
		in   model.Session
		ev   model.Event
		open bool
		want func(model.Session) model.Session
	}{
		{
			name: "SessionStart",
			in:   model.Session{SubagentsActive: 3},
			ev: model.Event{
				Kind: model.SessionStart, SessionID: "s1", Pane: "%1", Cwd: "/w",
				TranscriptPath: "/t", At: t1,
			},
			want: func(model.Session) model.Session {
				return model.Session{
					ID: "s1", Agent: "claude", Pane: "%1", Cwd: "/w", TranscriptPath: "/t",
					State: model.StateWorking, StartedAt: t1, UpdatedAt: t1,
				}
			},
		},
		{
			name: "UserPromptSubmit",
			ev:   model.Event{Kind: model.UserPromptSubmit, SessionID: "s1", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt, s.SeenAt = model.StateWorking, t1, t1
				return s
			},
		},
		{
			name: "PreToolUse",
			ev:   model.Event{Kind: model.PreToolUse, SessionID: "s1", ToolName: "Bash", ToolTarget: "ls", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt, s.LastUpdate = model.StateWorking, t1, "Bash ls"
				return s
			},
		},
		{
			name: "PreToolUse Task",
			ev:   model.Event{Kind: model.PreToolUse, SessionID: "s1", ToolName: "Task", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt, s.LastUpdate, s.SubagentsActive = model.StateWorking, t1, "Task", 1
				return s
			},
		},
		{
			name: "PreToolUse Agent",
			ev:   model.Event{Kind: model.PreToolUse, SessionID: "s1", ToolName: "Agent", ToolTarget: "x", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt, s.LastUpdate, s.SubagentsActive = model.StateWorking, t1, "Agent x", 1
				return s
			},
		},
		{
			name: "PostToolUse",
			ev:   model.Event{Kind: model.PostToolUse, SessionID: "s1", ToolName: "Read", ToolTarget: "a.go", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt, s.LastUpdate = model.StateWorking, t1, "Read a.go"
				return s
			},
		},
		{
			name: "Notification",
			ev:   model.Event{Kind: model.Notification, SessionID: "s1", Message: "needs input", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt, s.LastUpdate = model.StateWaiting, t1, "needs input"
				return s
			},
		},
		{
			name: "Stop with message",
			in:   func() model.Session { s := base(); s.State = model.StateWorking; s.LastUpdate = "old"; return s }(),
			ev:   model.Event{Kind: model.Stop, SessionID: "s1", Message: "done", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt, s.LastUpdate = model.StateIdle, t1, "done"
				return s
			},
		},
		{
			name: "Stop without message keeps LastUpdate",
			in:   func() model.Session { s := base(); s.State = model.StateWorking; s.LastUpdate = "old"; return s }(),
			ev:   model.Event{Kind: model.Stop, SessionID: "s1", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt = model.StateIdle, t1
				return s
			},
		},
		{
			name: "SubagentStop",
			in:   func() model.Session { s := base(); s.State = model.StateWorking; s.SubagentsActive = 2; return s }(),
			ev:   model.Event{Kind: model.SubagentStop, SessionID: "s1", At: t1},
			want: func(s model.Session) model.Session {
				s.UpdatedAt, s.SubagentsActive = t1, 1
				return s
			},
		},
		{
			name: "SessionEnd",
			ev:   model.Event{Kind: model.SessionEnd, SessionID: "s1", At: t1},
			want: func(s model.Session) model.Session {
				s.State, s.UpdatedAt = model.StateEnded, t1
				return s
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if in.ID == "" && tc.ev.Kind != model.SessionStart {
				in = base()
			}
			want := tc.want(in)
			if got := state.Apply(in, tc.ev, tc.open); got != want {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestEndedIgnoresEventsExceptSessionStart(t *testing.T) {
	ended := base()
	ended.State = model.StateEnded
	kinds := []model.EventKind{
		model.UserPromptSubmit, model.PreToolUse, model.PostToolUse,
		model.Notification, model.Stop, model.SubagentStop, model.SessionEnd,
	}
	for _, k := range kinds {
		ev := model.Event{Kind: k, SessionID: "s1", ToolName: "Task", Message: "m", At: t1}
		if got := state.Apply(ended, ev, true); got != ended {
			t.Errorf("%s changed an ended session: %+v", k, got)
		}
	}
	ev := model.Event{Kind: model.SessionStart, SessionID: "s1", Pane: "%2", At: t1}
	got := state.Apply(ended, ev, false)
	if got.State != model.StateWorking || got.Pane != "%2" || got.StartedAt != t1 {
		t.Errorf("SessionStart did not revive ended session: %+v", got)
	}
}

func TestSubagentCountNeverNegative(t *testing.T) {
	s := base()
	got := state.Apply(s, model.Event{Kind: model.SubagentStop, SessionID: "s1", At: t1}, false)
	if got.SubagentsActive != 0 {
		t.Errorf("SubagentsActive = %d, want 0", got.SubagentsActive)
	}
}

func TestStopWithOpenDecision(t *testing.T) {
	got := state.Apply(base(), model.Event{Kind: model.Stop, SessionID: "s1", At: t1}, true)
	if got.State != model.StateNeedsDecision {
		t.Errorf("State = %s, want needs_decision", got.State)
	}
}

func TestLastUpdateTruncatedTo80Runes(t *testing.T) {
	target := strings.Repeat("é世", 50) // 100 runes
	got := state.Apply(base(), model.Event{
		Kind: model.PreToolUse, SessionID: "s1", ToolName: "Bash", ToolTarget: target, At: t1,
	}, false)
	if n := len([]rune(got.LastUpdate)); n != 80 {
		t.Errorf("tool LastUpdate runes = %d, want 80", n)
	}
	got = state.Apply(base(), model.Event{
		Kind: model.Notification, SessionID: "s1", Message: target, At: t1,
	}, false)
	if n := len([]rune(got.LastUpdate)); n != 80 {
		t.Errorf("message LastUpdate runes = %d, want 80", n)
	}
}

func TestFirstSeenMidRunTakesIdentityFromEvent(t *testing.T) {
	ev := model.Event{
		Kind: model.PostToolUse, SessionID: "new", Pane: "%9", Cwd: "/c",
		TranscriptPath: "/tp", ToolName: "Read", At: t1,
	}
	got := state.Apply(model.Session{}, ev, false)
	want := model.Session{
		ID: "new", Agent: "claude", Pane: "%9", Cwd: "/c", TranscriptPath: "/tp",
		State: model.StateWorking, StartedAt: t1, UpdatedAt: t1, LastUpdate: "Read",
	}
	if got != want {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

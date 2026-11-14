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
			// Ruling 37: a new or /cleared session sits at an empty prompt,
			// and the user started it, so it is idle and seen.
			name: "SessionStart",
			in:   model.Session{SubagentsActive: 3},
			ev: model.Event{
				Kind: model.SessionStart, SessionID: "s1", Pane: "%1", Cwd: "/w",
				TranscriptPath: "/t", At: t1,
			},
			want: func(model.Session) model.Session {
				return model.Session{
					ID: "s1", Agent: "claude", Pane: "%1", Cwd: "/w", TranscriptPath: "/t",
					State: model.StateIdle, StartedAt: t1, UpdatedAt: t1, SeenAt: t1,
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

func TestEndedIgnoresSubagentStopAndSessionEnd(t *testing.T) {
	ended := base()
	ended.State = model.StateEnded
	for _, k := range []model.EventKind{model.SubagentStop, model.SessionEnd} {
		ev := model.Event{Kind: k, SessionID: "s1", At: t1}
		if got := state.Apply(ended, ev, true); got != ended {
			t.Errorf("%s changed an ended session: %+v", k, got)
		}
	}
	ev := model.Event{Kind: model.SessionStart, SessionID: "s1", Pane: "%2", At: t1}
	got := state.Apply(ended, ev, false)
	if got.State != model.StateIdle || got.Pane != "%2" || got.StartedAt != t1 || got.SeenAt != t1 {
		t.Errorf("SessionStart did not revive ended session as idle and seen: %+v", got)
	}
}

// TestEndedResumesOnActivity: a session ended by a nested session's start in
// its pane is alive again when it shows activity (Ruling 27).
func TestEndedResumesOnActivity(t *testing.T) {
	ended := base()
	ended.State = model.StateEnded
	for kind, want := range map[model.EventKind]model.State{
		model.UserPromptSubmit: model.StateWorking,
		model.PreToolUse:       model.StateWorking,
		model.PostToolUse:      model.StateWorking,
		model.Notification:     model.StateWaiting,
		model.Stop:             model.StateIdle,
	} {
		ev := model.Event{Kind: kind, SessionID: "s1", ToolName: "Bash", Message: "m", At: t1}
		got := state.Apply(ended, ev, false)
		live := ended
		live.State = model.StateWorking
		if got.State != want || !got.UpdatedAt.Equal(t1) || got != state.Apply(live, ev, false) {
			t.Errorf("%s on an ended session = %+v, want it applied normally (state %s)", kind, got, want)
		}
	}
}

func TestIgnores(t *testing.T) {
	ended, live := base(), base()
	ended.State = model.StateEnded
	kinds := []model.EventKind{
		model.SessionStart, model.UserPromptSubmit, model.PreToolUse, model.PostToolUse,
		model.Notification, model.Stop, model.SubagentStop, model.SessionEnd,
	}
	for _, k := range kinds {
		want := k == model.SubagentStop || k == model.SessionEnd
		if got := state.Ignores(ended, model.Event{Kind: k}); got != want {
			t.Errorf("Ignores(ended, %s) = %v, want %v", k, got, want)
		}
		if state.Ignores(live, model.Event{Kind: k}) {
			t.Errorf("Ignores(idle, %s) = true", k)
		}
	}
}

func TestSessionStartResetsOffsetForNewTranscript(t *testing.T) {
	s := base()
	s.TranscriptOffset = 500
	same := state.Apply(s, model.Event{Kind: model.SessionStart, SessionID: "s1", TranscriptPath: "/t", At: t1}, false)
	if same.TranscriptOffset != 500 {
		t.Errorf("same transcript: offset = %d, want 500 kept", same.TranscriptOffset)
	}
	moved := state.Apply(s, model.Event{Kind: model.SessionStart, SessionID: "s1", TranscriptPath: "/t2", At: t1}, false)
	if moved.TranscriptPath != "/t2" || moved.TranscriptOffset != 0 {
		t.Errorf("new transcript: path %q offset %d, want /t2 and 0", moved.TranscriptPath, moved.TranscriptOffset)
	}
	// First seen mid-run: the hook has already read ev's transcript up to the
	// offset, so the offset belongs to the path the session takes from ev.
	fresh := model.Session{TranscriptOffset: 300}
	got := state.Apply(fresh, model.Event{Kind: model.PostToolUse, SessionID: "n", TranscriptPath: "/tp", At: t1}, false)
	if got.TranscriptOffset != 300 {
		t.Errorf("first seen mid-run: offset = %d, want 300 kept", got.TranscriptOffset)
	}
}

func TestSessionStartTakesModelWhenNamed(t *testing.T) {
	s := base()
	s.Model = "claude-opus-5-5"
	named := state.Apply(s, model.Event{Kind: model.SessionStart, SessionID: "s1", Model: "claude-haiku-5-5", At: t1}, false)
	if named.Model != "claude-haiku-5-5" {
		t.Errorf("SessionStart naming a model: %q, want claude-haiku-5-5", named.Model)
	}
	unnamed := state.Apply(s, model.Event{Kind: model.SessionStart, SessionID: "s1", At: t1}, false)
	if unnamed.Model != "claude-opus-5-5" {
		t.Errorf("SessionStart naming none: %q, want claude-opus-5-5 kept", unnamed.Model)
	}
	if other := state.Apply(s, model.Event{Kind: model.UserPromptSubmit, SessionID: "s1", Model: "x", At: t1}, false); other.Model != "claude-opus-5-5" {
		t.Errorf("UserPromptSubmit changed the model to %q", other.Model)
	}
}

func TestPostToolUseTaskDoesNotCountSubagent(t *testing.T) {
	got := state.Apply(base(), model.Event{Kind: model.PostToolUse, SessionID: "s1", ToolName: "Task", At: t1}, false)
	if got.SubagentsActive != 0 {
		t.Errorf("SubagentsActive = %d, want 0 (only PreToolUse counts)", got.SubagentsActive)
	}
}

// Ruling 31: idle_prompt (the prompt has sat idle about a minute) and
// auth_success are not a request for input.
func TestInformationalNotificationChangesNothing(t *testing.T) {
	for _, typ := range []string{"idle_prompt", "auth_success"} {
		for _, st := range []model.State{model.StateIdle, model.StateWorking, model.StateEnded} {
			s := base()
			s.State, s.LastUpdate = st, "done"
			ev := model.Event{Kind: model.Notification, SessionID: "s1", Message: "waiting", NotificationType: typ, At: t1}
			if got := state.Apply(s, ev, false); got != s {
				t.Errorf("%s on %s changed the session: %+v", typ, st, got)
			}
			if !state.Ignores(s, ev) {
				t.Errorf("Ignores(%s, %s) = false", st, typ)
			}
		}
	}
	// A session first seen on one gets a row, at its prompt, with no message.
	ev := model.Event{Kind: model.Notification, SessionID: "n", Message: "waiting", NotificationType: "idle_prompt", At: t1}
	if got := state.Apply(model.Session{}, ev, false); got.ID != "n" || got.State != model.StateIdle || got.LastUpdate != "" {
		t.Errorf("first seen on idle_prompt = %+v, want idle with no LastUpdate", got)
	}
	for _, typ := range []string{"", "permission_prompt", "elicitation_dialog"} {
		ev := model.Event{Kind: model.Notification, SessionID: "s1", Message: "m", NotificationType: typ, At: t1}
		if got := state.Apply(base(), ev, false); got.State != model.StateWaiting {
			t.Errorf("Notification %q: state %s, want waiting", typ, got.State)
		}
	}
}

func TestNotificationWithEmptyMessage(t *testing.T) {
	s := base()
	s.LastUpdate = "Bash ls"
	got := state.Apply(s, model.Event{Kind: model.Notification, SessionID: "s1", At: t1}, false)
	if got.State != model.StateWaiting || got.LastUpdate != "" || !got.UpdatedAt.Equal(t1) {
		t.Errorf("got %+v, want waiting with the (empty) prompt text", got)
	}
}

func TestSubagentCountNeverNegative(t *testing.T) {
	s := base()
	got := state.Apply(s, model.Event{Kind: model.SubagentStop, SessionID: "s1", At: t1}, false)
	if got.SubagentsActive != 0 {
		t.Errorf("SubagentsActive = %d, want 0", got.SubagentsActive)
	}
}

// Ruling 87, defect 7: the event order of live check 2's two-subagent run
// (events 116 to 151) and of its run L3 (182 to 187). A helper agent's
// SubagentStop does not lower the count, and a Stop sets it to the
// background subagents its payload lists as running, so the session never
// reads as having no subagent while one still works.
func TestSubagentCountFollowsLiveRuns(t *testing.T) {
	ev := func(kind model.EventKind, f func(*model.Event)) model.Event {
		e := model.Event{Kind: kind, SessionID: "s1", At: t1}
		if f != nil {
			f(&e)
		}
		return e
	}
	agentTool := ev(model.PreToolUse, func(e *model.Event) { e.ToolName = "Agent" })
	stopWith := func(n int) model.Event {
		return ev(model.Stop, func(e *model.Event) { e.BackgroundKnown, e.RunningSubagents = true, n })
	}
	agentStop := ev(model.SubagentStop, nil)
	helperStop := ev(model.SubagentStop, func(e *model.Event) { e.HelperAgent = true })
	for name, run := range map[string]struct {
		events []model.Event
		counts []int
	}{
		"two background subagents": {
			[]model.Event{agentTool, agentTool, stopWith(2), agentStop, helperStop, agentStop},
			[]int{1, 2, 2, 1, 1, 0},
		},
		"Stop before the subagent's dialog": {
			[]model.Event{agentTool, stopWith(1), agentStop},
			[]int{1, 1, 0},
		},
		"Stop lists more than were counted": {
			[]model.Event{helperStop, stopWith(1), helperStop, agentStop},
			[]int{0, 1, 1, 0},
		},
		"Stop without a list keeps the count": {
			[]model.Event{agentTool, ev(model.Stop, nil), agentStop},
			[]int{1, 1, 0},
		},
	} {
		s := base()
		for k, e := range run.events {
			s = state.Apply(s, e, false)
			if s.SubagentsActive != run.counts[k] {
				t.Errorf("%s: after event %d (%s) SubagentsActive = %d, want %d", name, k+1, e.Kind, s.SubagentsActive, run.counts[k])
			}
		}
	}
}

func TestStopWithOpenDecision(t *testing.T) {
	got := state.Apply(base(), model.Event{Kind: model.Stop, SessionID: "s1", At: t1}, true)
	if got.State != model.StateNeedsDecision {
		t.Errorf("State = %s, want needs_decision", got.State)
	}
}

// A Stop the hook blocks to deliver answers is not the end of the turn:
// Claude goes on working with the answers (plan section 4.9).
func TestStopBlockedIsWorking(t *testing.T) {
	got := state.Apply(base(), model.Event{Kind: model.Stop, SessionID: "s1", Message: "done", StopBlocked: true, At: t1}, false)
	if got.State != model.StateWorking || got.LastUpdate != "done" || !got.UpdatedAt.Equal(t1) {
		t.Errorf("got state %s, LastUpdate %q, updated %v; want working, done, %v", got.State, got.LastUpdate, got.UpdatedAt, t1)
	}
}

func TestStopBlockedBeatsOpenDecision(t *testing.T) {
	got := state.Apply(base(), model.Event{Kind: model.Stop, SessionID: "s1", Message: "done", StopBlocked: true, At: t1}, true)
	if got.State != model.StateWorking || got.LastUpdate != "done" {
		t.Errorf("got state %s, LastUpdate %q; want working, done", got.State, got.LastUpdate)
	}
}

// StopBlocked means nothing on another event.
func TestStopBlockedOnlyOnStop(t *testing.T) {
	got := state.Apply(base(), model.Event{Kind: model.SessionEnd, SessionID: "s1", StopBlocked: true, At: t1}, true)
	if got.State != model.StateEnded {
		t.Errorf("SessionEnd with StopBlocked: state %s, want ended", got.State)
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
	got = state.Apply(base(), model.Event{
		Kind: model.Stop, SessionID: "s1", Message: target, At: t1,
	}, false)
	if got.LastUpdate != string([]rune(target)[:80]) {
		t.Errorf("Stop LastUpdate = %q, want the first 80 runes", got.LastUpdate)
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

// Ruling 74: a compaction fires SessionStart with source compact, often in
// the middle of a turn. A stored session keeps its state, seen time, start
// and subagent count; only the update time, a changed transcript path and a
// named model are taken.
func TestCompactSessionStartKeepsSession(t *testing.T) {
	s := base()
	s.State, s.SubagentsActive, s.LastUpdate = model.StateWorking, 2, "Edit a.go"
	s.Model, s.TranscriptOffset, s.Repo, s.TmuxWindow = "claude-opus-5-5", 500, "/src/curia", "@7"
	ev := model.Event{
		Kind: model.SessionStart, Source: "compact", SessionID: "s1", Pane: "%9", Cwd: "/elsewhere",
		TranscriptPath: "/t", At: t1,
	}
	got := state.Apply(s, ev, true)
	want := s
	want.UpdatedAt = t1
	if got != want {
		t.Errorf("compact on a working session:\n got %+v\nwant %+v", got, want)
	}

	ev.TranscriptPath, ev.Model = "/t2", "claude-haiku-5-5"
	got = state.Apply(s, ev, false)
	want.TranscriptPath, want.TranscriptOffset, want.Model = "/t2", 0, "claude-haiku-5-5"
	if got != want {
		t.Errorf("compact with a new transcript and model:\n got %+v\nwant %+v", got, want)
	}

	ev.TranscriptPath, ev.Model = "", ""
	if got = state.Apply(s, ev, false); got.TranscriptPath != "/t" || got.TranscriptOffset != 500 {
		t.Errorf("compact naming no transcript: path %q offset %d, want /t and 500 kept", got.TranscriptPath, got.TranscriptOffset)
	}

	for _, st := range []model.State{model.StateWaiting, model.StateIdle, model.StateNeedsDecision} {
		in := base()
		in.State = st
		if got := state.Apply(in, ev, false); got.State != st || got.SeenAt != in.SeenAt {
			t.Errorf("compact on %s: state %s seen %v, want both kept", st, got.State, got.SeenAt)
		}
	}
}

// For a session with no row yet, compact is a first event like any other.
func TestCompactSessionStartFirstSeen(t *testing.T) {
	ev := model.Event{Kind: model.SessionStart, Source: "compact", SessionID: "s1", Pane: "%1", Cwd: "/w", TranscriptPath: "/t", At: t1}
	got := state.Apply(model.Session{}, ev, false)
	want := model.Session{
		ID: "s1", Agent: "claude", Pane: "%1", Cwd: "/w", TranscriptPath: "/t",
		State: model.StateIdle, StartedAt: t1, UpdatedAt: t1, SeenAt: t1,
	}
	if got != want {
		t.Errorf("compact first seen:\n got %+v\nwant %+v", got, want)
	}
}

// Every other source starts the session afresh, as before.
func TestOtherSessionStartSourcesRestart(t *testing.T) {
	for _, source := range []string{"startup", "resume", "clear", "", "Compact"} {
		s := base()
		s.State, s.SubagentsActive = model.StateWorking, 2
		got := state.Apply(s, model.Event{Kind: model.SessionStart, Source: source, SessionID: "s1", At: t1}, false)
		if got.State != model.StateIdle || got.SeenAt != t1 || got.StartedAt != t1 || got.SubagentsActive != 0 {
			t.Errorf("source %q: %+v, want idle, seen, started at %v, no subagents", source, got, t1)
		}
	}
}

func TestCompaction(t *testing.T) {
	for _, tc := range []struct {
		ev   model.Event
		want bool
	}{
		{model.Event{Kind: model.SessionStart, Source: "compact"}, true},
		{model.Event{Kind: model.SessionStart, Source: "startup"}, false},
		{model.Event{Kind: model.SessionStart}, false},
		{model.Event{Kind: model.Stop, Source: "compact"}, false},
	} {
		if got := state.Compaction(tc.ev); got != tc.want {
			t.Errorf("Compaction(%+v) = %v, want %v", tc.ev, got, tc.want)
		}
	}
}

// Ruling 74a: an ended session is plainly alive again, so a compaction's
// SessionStart revives it as any SessionStart does: idle and seen.
func TestCompactSessionStartRevivesEnded(t *testing.T) {
	s := base()
	s.State, s.SubagentsActive = model.StateEnded, 2
	ev := model.Event{Kind: model.SessionStart, Source: "compact", SessionID: "s1", Pane: "%1", Cwd: "/w", TranscriptPath: "/t", At: t1}
	got := state.Apply(s, ev, false)
	if got.State != model.StateIdle || got.SeenAt != t1 || got.StartedAt != t1 || got.SubagentsActive != 0 {
		t.Errorf("compact on an ended session: %+v, want idle, seen and started at %v, no subagents", got, t1)
	}
}

package fleet_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
)

// A turn ended by a denial, in the shape Claude Code writes it. The marker
// is at 11:59:58.5, later than every UpdatedAt the tests settle (t0 is 12:00).
const deniedTranscript = `{"type":"user","message":{"role":"user","content":"Create out.txt"}}
{"type":"assistant","message":{"model":"m","content":[{"type":"tool_use","id":"t1","name":"Write","input":{}}],"usage":{"input_tokens":1}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"The user doesn't want to proceed with this tool use.","is_error":true,"tool_use_id":"t1"}]},"toolUseResult":"User rejected tool use","toolDenialKind":"user-rejected"}
{"type":"attachment","attachment":{"type":"x"}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]},"timestamp":"2026-11-11T11:59:58.500Z"}
{"type":"system","subtype":"turn_duration","durationMs":1996}
`

// settleDeniedFile writes deniedTranscript to a file under t.TempDir().
func settleDeniedFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(p, []byte(deniedTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// settleSession is a session in state, last updated at, whose transcript is
// at path.
func settleSession(id string, state model.State, updated time.Time, path string) model.Session {
	return model.Session{
		ID: id, Agent: "claude", State: state, Pane: "%1", TmuxWindow: "@1", Repo: "/r", Model: "m",
		LastUpdate: "Write out.txt", TranscriptPath: path, StartedAt: t0.Add(-time.Hour), UpdatedAt: updated,
	}
}

func TestSettleMovesInterruptedToIdleSeen(t *testing.T) {
	now := t0
	denied := settleDeniedFile(t)
	working := settleSession("working", model.StateWorking, now.Add(-10*time.Second), denied)
	working.TranscriptOffset = int64(strings.Index(deniedTranscript, `{"type":"assistant"`)) // read up to the tool_use
	waiting := settleSession("waiting", model.StateWaiting, now.Add(-time.Hour), denied)
	waiting.Pane, waiting.TmuxWindow = "", "" // outside tmux: the rule needs no pane
	st := openStore(t, working, waiting)

	settled, err := fleet.Settle(context.Background(), st, []model.Session{working, waiting}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"working", "waiting"}; !slices.Equal(settled, want) {
		t.Errorf("settled = %q, want %q", settled, want)
	}
	for _, before := range []model.Session{working, waiting} {
		want := before
		want.State, want.SeenAt, want.UpdatedAt, want.LastUpdate = model.StateIdle, now, now, "interrupted"
		if got := get(t, st, before.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("%s =\n%+v, want\n%+v", before.ID, got, want)
		}
	}
	// Settled sessions are idle now, so a second pass leaves them alone.
	again, err := fleet.Settle(context.Background(), st, []model.Session{get(t, st, "working")}, now.Add(time.Minute))
	if err != nil || len(again) != 0 {
		t.Errorf("second Settle = %q, %v, want none", again, err)
	}
}

func TestSettleLeavesRecentSessions(t *testing.T) {
	now := t0
	denied := settleDeniedFile(t)
	recent := settleSession("recent", model.StateWaiting, now.Add(-3*time.Second+time.Millisecond), denied)
	// A transcript that cannot be read proves a recent session is not read.
	unread := settleSession("unread", model.StateWorking, now.Add(-time.Second), t.TempDir())
	future := settleSession("future", model.StateWorking, now.Add(time.Minute), denied)
	edge := settleSession("edge", model.StateWorking, now.Add(-3*time.Second), denied)
	st := openStore(t, recent, unread, future, edge)

	settled, err := fleet.Settle(context.Background(), st, []model.Session{recent, unread, future, edge}, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"edge"}; !slices.Equal(settled, want) {
		t.Errorf("settled = %q, want %q (3 s old counts as settled)", settled, want)
	}
	for _, s := range []model.Session{recent, unread, future} {
		if got := get(t, st, s.ID); !reflect.DeepEqual(got, s) {
			t.Errorf("%s changed:\n%+v", s.ID, got)
		}
	}
}

func TestSettleLeavesIdleAndEnded(t *testing.T) {
	now := t0
	denied := settleDeniedFile(t)
	old := now.Add(-time.Hour)
	var sessions []model.Session
	for _, state := range []model.State{model.StateIdle, model.StateEnded, model.StateStale, model.StateNeedsDecision, "bogus"} {
		sessions = append(sessions, settleSession(string(state), state, old, denied))
	}
	// Neither is read: their transcripts are directories.
	sessions = append(sessions, settleSession("idle-unread", model.StateIdle, old, t.TempDir()))
	// No transcript at all: nothing to settle from, and no error.
	sessions = append(sessions, settleSession("no-transcript", model.StateWaiting, old, ""))
	st := openStore(t, sessions...)

	settled, err := fleet.Settle(context.Background(), st, sessions, now)
	if err != nil || len(settled) != 0 {
		t.Fatalf("Settle = %q, %v, want none", settled, err)
	}
	for _, s := range sessions {
		if got := get(t, st, s.ID); !reflect.DeepEqual(got, s) {
			t.Errorf("%s changed:\n%+v", s.ID, got)
		}
	}
}

func TestSettleSkipsWhenRowChanged(t *testing.T) {
	now := t0
	denied := settleDeniedFile(t)
	listed := settleSession("s", model.StateWaiting, now.Add(-time.Minute), denied)
	for name, stored := range map[string]model.Session{
		// A hook event (the next prompt) arrived after the list was read.
		"updated since": func() model.Session {
			s := listed
			s.State, s.UpdatedAt, s.LastUpdate = model.StateWorking, now.Add(-time.Second), "Bash ls"
			return s
		}(),
		"state changed": func() model.Session {
			s := listed
			s.State = model.StateWorking
			return s
		}(),
		"time changed": func() model.Session {
			s := listed
			s.UpdatedAt = listed.UpdatedAt.Add(time.Nanosecond)
			return s
		}(),
		"ended since": func() model.Session {
			s := listed
			s.State, s.UpdatedAt = model.StateEnded, now.Add(-time.Second)
			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			st := openStore(t, stored)
			settled, err := fleet.Settle(context.Background(), st, []model.Session{listed}, now)
			if err != nil || len(settled) != 0 {
				t.Errorf("Settle = %q, %v, want none", settled, err)
			}
			if got := get(t, st, "s"); !reflect.DeepEqual(got, stored) {
				t.Errorf("row =\n%+v, want unchanged\n%+v", got, stored)
			}
		})
	}
}

func TestSettleContinuesAfterOneFailure(t *testing.T) {
	now := t0
	denied := settleDeniedFile(t)
	old := now.Add(-time.Minute)
	broken := settleSession("a-broken", model.StateWorking, old, t.TempDir()) // a directory
	missing := settleSession("b-missing", model.StateWorking, old, denied)    // listed, never stored
	good := settleSession("c-good", model.StateWaiting, old, denied)
	st := openStore(t, broken, good)

	settled, err := fleet.Settle(context.Background(), st, []model.Session{broken, missing, good}, now)
	if want := []string{"c-good"}; !slices.Equal(settled, want) {
		t.Errorf("settled = %q, want %q", settled, want)
	}
	if err == nil || !strings.Contains(err.Error(), "a-broken") || !strings.Contains(err.Error(), "b-missing") {
		t.Errorf("err = %v, want one naming a-broken and b-missing", err)
	}
	if got := get(t, st, "c-good"); got.State != model.StateIdle {
		t.Errorf("c-good = %s, want idle", got.State)
	}
	if got := get(t, st, "a-broken"); !reflect.DeepEqual(got, broken) {
		t.Errorf("a-broken changed:\n%+v", got)
	}
}

// settleMarkerFile is a transcript whose last conversational line is an
// interrupt marker with the given timestamp field ("" for none), after a
// PreToolUse's tool_use.
func settleMarkerFile(t *testing.T, timestamp string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.jsonl")
	content := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}` + "\n" +
		`{"type":"user",` + timestamp + `"message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Ruling 79: the marker must be later than the session's last hook event.
func TestSettleOnlyMarkerAfterLastHookEvent(t *testing.T) {
	now := t0
	ts := func(s string) string { return `"timestamp":"` + s + `",` }
	for _, tt := range []struct {
		name    string
		state   model.State
		updated time.Time
		stamp   string
		want    bool
	}{
		// Denial: the permission_prompt Notification set waiting, then the
		// user denied.
		{"denial after notification", model.StateWaiting, now.Add(-time.Minute), ts("2026-11-11T11:59:01.329Z"), true},
		// Interrupt: PreToolUse set working, then Esc during the tool.
		{"interrupt after pretooluse", model.StateWorking, now.Add(-time.Minute), ts("2026-11-11T11:59:10.887Z"), true},
		// An old marker, then a new prompt: UserPromptSubmit set working
		// later, and Claude Code has not written the prompt line for more
		// than 3 s (a slow hook of another tool holds it).
		{"old marker then new prompt", model.StateWorking, now.Add(-10 * time.Second), ts("2026-11-11T11:59:40.000Z"), false},
		{"marker equal to UpdatedAt", model.StateWorking, now.Add(-time.Minute), ts("2026-11-11T11:59:00.000Z"), false},
		// Compared as times: 12:59:00+01:00 is 11:59:00Z, equal, not later.
		{"equal in another zone", model.StateWaiting, now.Add(-time.Minute), ts("2026-11-11T12:59:00+01:00"), false},
		{"later in another zone", model.StateWaiting, now.Add(-time.Minute), ts("2026-11-11T12:59:00.001+01:00"), true},
		// The stored time has nanoseconds; Claude Code's has milliseconds.
		{"a nanosecond later than the marker", model.StateWorking, now.Add(-time.Minute).Add(time.Nanosecond), ts("2026-11-11T11:59:00.000Z"), false},
		{"missing timestamp", model.StateWorking, now.Add(-time.Minute), "", false},
		{"malformed timestamp", model.StateWorking, now.Add(-time.Minute), ts("11:59:30 yesterday"), false},
		{"numeric timestamp", model.StateWorking, now.Add(-time.Minute), `"timestamp":1794453708329,`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := settleSession("s", tt.state, tt.updated, settleMarkerFile(t, tt.stamp))
			st := openStore(t, s)
			settled, err := fleet.Settle(context.Background(), st, []model.Session{s}, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(settled) == 1; got != tt.want {
				t.Errorf("settled = %q, want settled %v", settled, tt.want)
			}
			if got := get(t, st, "s"); tt.want != (got.State == model.StateIdle) || (!tt.want && !reflect.DeepEqual(got, s)) {
				t.Errorf("row = %+v", got)
			}
		})
	}
}

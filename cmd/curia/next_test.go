package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

func TestNextSwitchesToMostUrgentPane(t *testing.T) {
	_, fake := fleetHome(t, []string{"%1", "%2", "%3"},
		model.Session{ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1", UpdatedAt: at(11, 0)},
		model.Session{ID: "b", Agent: "claude", State: model.StateWaiting, Pane: "%2", UpdatedAt: at(11, 0)},
		model.Session{ID: "c", Agent: "claude", State: model.StateWaiting, Pane: "%3", UpdatedAt: at(10, 0)},
		model.Session{ID: "d", Agent: "claude", State: model.StateNeedsDecision, Pane: "", UpdatedAt: at(9, 0)},
	)
	out, err := runRoot(t, "next")
	if err != nil || out != "" {
		t.Errorf("next = %q, %v, want no output", out, err)
	}
	if want := []string{"ListPanes", "SwitchTo %3"}; !reflect.DeepEqual(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
	if log := readLog(t); log != nil {
		t.Errorf("log = %q, want none", log)
	}
}

// Ruling 20: next reconciles first, so it never jumps to a pane that is gone.
func TestNextSkipsAndMarksStaleSessionWhosePaneIsGone(t *testing.T) {
	db, fake := fleetHome(t, []string{"%2"},
		model.Session{ID: "gone", Agent: "claude", State: model.StateWaiting, Pane: "%1", UpdatedAt: at(10, 0)},
		model.Session{ID: "live", Agent: "claude", State: model.StateWaiting, Pane: "%2", UpdatedAt: at(11, 0)},
	)
	if out, err := runRoot(t, "next"); err != nil || out != "" {
		t.Errorf("next = %q, %v, want no output", out, err)
	}
	if want := []string{"ListPanes", "SwitchTo %2"}; !reflect.DeepEqual(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
	if s := getSession(t, db, "gone"); s.State != model.StateStale || !s.UpdatedAt.Equal(fleetNow) {
		t.Errorf("gone = %s at %v, want stale at %v", s.State, s.UpdatedAt, fleetNow)
	}
}

// After a tmux server restart the new server numbers panes from %0 again, so
// the dead session's pane id names an unrelated live pane.
func TestNextSkipsAndMarksStaleSessionFromBeforeServerRestart(t *testing.T) {
	db, fake := fleetHome(t, nil,
		model.Session{
			ID: "ghost", Agent: "claude", State: model.StateWaiting, Pane: "%1",
			StartedAt: at(9, 0), UpdatedAt: at(10, 0),
		},
		model.Session{
			ID: "fresh", Agent: "claude", State: model.StateWaiting, Pane: "%2",
			StartedAt: at(11, 30), UpdatedAt: at(11, 40),
		},
	)
	for _, id := range []string{"%1", "%2"} {
		fake.Panes = append(fake.Panes, tmux.Pane{ID: id, Session: "work", WindowID: "@1", ServerStart: at(11, 0)})
	}
	if out, err := runRoot(t, "next"); err != nil || out != "" {
		t.Errorf("next = %q, %v, want no output", out, err)
	}
	if want := []string{"ListPanes", "SwitchTo %2"}; !reflect.DeepEqual(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
	if s := getSession(t, db, "ghost"); s.State != model.StateStale || !s.UpdatedAt.Equal(fleetNow) {
		t.Errorf("ghost = %s at %v, want stale at %v", s.State, s.UpdatedAt, fleetNow)
	}
}

func TestNextSaysNothingNeedsYou(t *testing.T) {
	_, fake := fleetHome(t, []string{"%1"},
		model.Session{ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1", UpdatedAt: at(11, 0)},
	)
	if out, err := runRoot(t, "next"); err != nil || out != "" {
		t.Errorf("next = %q, %v, want no output", out, err)
	}
	if want := []string{"ListPanes", "DisplayMessage nothing needs you"}; !reflect.DeepEqual(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
}

func TestNextLogsErrorsAndExitsZero(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T)
		args  []string
		want  []string // substrings of each log line, in order
	}{
		{"tmux unreachable", func(t *testing.T) {
			fleetHome(t, nil,
				model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1", UpdatedAt: at(11, 0)})
			useDownTmux(t)
		}, []string{"next"}, []string{"not reconciling", "next failed"}},
		{"store fails", func(t *testing.T) {
			fleetHome(t, nil)
			t.Setenv("CURIA_DB", t.TempDir()) // a directory is not a database
		}, []string{"next"}, []string{"next failed"}},
		{"bad arguments", func(t *testing.T) { fleetHome(t, nil) }, []string{"next", "--bogus"}, []string{"next failed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			if out, err := runRoot(t, tc.args...); err != nil || out != "" {
				t.Errorf("next = %q, %v, want no output and exit 0", out, err)
			}
			log := readLog(t)
			if len(log) != len(tc.want) {
				t.Fatalf("log = %q, want %d lines", log, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(log[i], w) {
					t.Errorf("log line %d = %q, want it to contain %q", i, log[i], w)
				}
			}
		})
	}
}

// A session that raised a blocking ticket is needs_decision, which ranks
// before an older waiting session.
func TestNextJumpsToSessionWithBlockingTicket(t *testing.T) {
	db := decisionsHome(t, decisionsSpecs()[0]) // D-1: open, blocking, raised by s1
	fake := useFakeTmux(t)
	for _, id := range []string{"%1", "%2"} {
		fake.Panes = append(fake.Panes, tmux.Pane{ID: id, Session: "work", WindowID: "@1"})
	}
	seedSessions(t, db,
		model.Session{ID: "s0", Agent: "claude", State: model.StateWaiting, Pane: "%1", UpdatedAt: at(9, 0)},
		model.Session{ID: "s1", Agent: "claude", State: model.StateNeedsDecision, Pane: "%2", UpdatedAt: at(11, 0)},
	)
	if d := decisionsGet(t, db, "D-1"); d.SessionID != "s1" || d.Mode != model.ModeBlocking || d.Status != model.StatusOpen {
		t.Fatalf("D-1 = %+v, want an open blocking ticket of s1", d)
	}
	if out, err := runRoot(t, "next"); err != nil || out != "" {
		t.Errorf("next = %q, %v, want no output", out, err)
	}
	if want := []string{"ListPanes", "SwitchTo %2"}; !reflect.DeepEqual(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
}

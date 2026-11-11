package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

func TestNextSwitchesToMostUrgentPane(t *testing.T) {
	fake := livePanes("%1", "%2", "%3")
	fleetHome(t, fake,
		model.Session{ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1", UpdatedAt: at(11, 0)},
		model.Session{ID: "b", Agent: "claude", State: model.StateWaiting, Pane: "%2", UpdatedAt: at(11, 0)},
		model.Session{ID: "c", Agent: "claude", State: model.StateWaiting, Pane: "%3", UpdatedAt: at(10, 0)},
		model.Session{ID: "d", Agent: "claude", State: model.StateNeedsDecision, Pane: "", UpdatedAt: at(9, 0)},
	)
	out, err := runRoot(t, "next")
	if err != nil || out != "" {
		t.Errorf("next = %q, %v, want no output", out, err)
	}
	if want := []string{"SwitchTo %3"}; !reflect.DeepEqual(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
	if log := readLog(t); log != nil {
		t.Errorf("log = %q, want none", log)
	}
}

func TestNextSaysNothingNeedsYou(t *testing.T) {
	fake := livePanes("%1")
	fleetHome(t, fake,
		model.Session{ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1", UpdatedAt: at(11, 0)},
	)
	if out, err := runRoot(t, "next"); err != nil || out != "" {
		t.Errorf("next = %q, %v, want no output", out, err)
	}
	if want := []string{"DisplayMessage nothing needs you"}; !reflect.DeepEqual(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
}

func TestNextLogsErrorsAndExitsZero(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T)
		args  []string
	}{
		{"tmux fails", func(t *testing.T) {
			fleetHome(t, &downTmux{},
				model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1", UpdatedAt: at(11, 0)})
		}, []string{"next"}},
		{"store fails", func(t *testing.T) {
			fleetHome(t, livePanes())
			t.Setenv("CURIA_DB", t.TempDir()) // a directory is not a database
		}, []string{"next"}},
		{"bad arguments", func(t *testing.T) { fleetHome(t, livePanes()) }, []string{"next", "--bogus"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			if out, err := runRoot(t, tc.args...); err != nil || out != "" {
				t.Errorf("next = %q, %v, want no output and exit 0", out, err)
			}
			if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "next failed") {
				t.Errorf("log = %q, want one line reporting the failure", log)
			}
		})
	}
}

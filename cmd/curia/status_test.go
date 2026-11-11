package main

import (
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

func TestStatusPrintsBadge(t *testing.T) {
	s := func(id string, state model.State, pane string) model.Session {
		return model.Session{ID: id, Agent: "claude", State: state, Pane: pane, UpdatedAt: at(11, 0)}
	}
	fleetHome(t, livePanes(),
		s("a", model.StateNeedsDecision, "%1"),
		s("b", model.StateNeedsDecision, ""), // outside tmux still counts
		s("c", model.StateWaiting, "%3"),
		s("d", model.StateWorking, "%4"),
		s("e", model.StateIdle, "%5"),
		s("f", model.StateEnded, "%6"),
	)
	if out, err := runRoot(t, "status"); err != nil || out != "D2 W1" {
		t.Errorf("status = %q, %v, want \"D2 W1\" with no newline", out, err)
	}
}

func TestStatusPrintsNothingWhenQuiet(t *testing.T) {
	fleetHome(t, livePanes(), model.Session{ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1"})
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status = %q, %v, want empty output", out, err)
	}
}

func TestStatusPrintsNothingOnError(t *testing.T) {
	fleetHome(t, livePanes())
	t.Setenv("CURIA_DB", t.TempDir()) // a directory is not a database
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status with a broken store = %q, %v, want empty output and exit 0", out, err)
	}
	if out, err := runRoot(t, "status", "extra", "--bogus"); err != nil || out != "" {
		t.Errorf("status with bad arguments = %q, %v, want empty output and exit 0", out, err)
	}
}

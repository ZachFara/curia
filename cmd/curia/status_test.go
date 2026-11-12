package main

import (
	"database/sql"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

func TestStatusPrintsBadge(t *testing.T) {
	s := func(id string, state model.State, pane string) model.Session {
		return model.Session{ID: id, Agent: "claude", State: state, Pane: pane, UpdatedAt: at(11, 0)}
	}
	fleetHome(t, nil,
		s("a", model.StateNeedsDecision, "%1"),
		s("b", model.StateNeedsDecision, ""), // outside tmux still counts
		s("c", model.StateWaiting, "%3"),
		s("d", model.StateWorking, "%4"),
		s("e", model.StateIdle, "%5"),
		s("f", model.StateEnded, "%6"),
	)
	// needs_decision sessions are not counted: D counts tickets (section 4.10).
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Errorf("status = %q, %v, want \"W1\" with no newline", out, err)
	}
}

func TestStatusPrintsNothingWhenQuiet(t *testing.T) {
	fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1"})
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status = %q, %v, want empty output", out, err)
	}
}

func TestStatusStillSilentOnError(t *testing.T) {
	fleetHome(t, nil)
	t.Setenv("CURIA_DB", t.TempDir()) // a directory is not a database
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status with a broken store = %q, %v, want empty output and exit 0", out, err)
	}
	if out, err := runRoot(t, "status", "extra", "--bogus"); err != nil || out != "" {
		t.Errorf("status with bad arguments = %q, %v, want empty output and exit 0", out, err)
	}

	// The sessions read works but the ticket count fails.
	db, _ := fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("DROP TABLE decisions"); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status with a broken decisions table = %q, %v, want empty output and exit 0", out, err)
	}
}

// statusSession is session s1 in state, in live pane %1.
func statusSession(state model.State) model.Session {
	s := decideSessionS1()
	s.State = state
	return s
}

func TestBadgeCountsTicketsNotSessions(t *testing.T) {
	const z = ".000000000Z"
	db := decisionsHome(t,
		decisionsSpec{"first", model.ModeBlocking, "2026-11-11T08:00:00" + z, "", decisionsEnd{}},
		decisionsSpec{"second", model.ModeBlocking, "2026-11-11T09:00:00" + z, "", decisionsEnd{}},
	)
	seedSessions(t, db, statusSession(model.StateWorking))
	if out, err := runRoot(t, "status"); err != nil || out != "D2" {
		t.Errorf("status = %q, %v, want \"D2\"", out, err)
	}
}

func TestBadgeIgnoresAssumeAndPastDeadline(t *testing.T) {
	const z = ".000000000Z"
	for name, specs := range map[string][]decisionsSpec{
		"assume only": {
			{"live", model.ModeAssume, "2026-11-11T08:00:00" + z, "2026-11-12T08:00:00" + z, decisionsEnd{}},
			{"past deadline", model.ModeAssume, "2026-11-11T09:00:00" + z, "2026-11-11T11:00:00" + z, decisionsEnd{}},
		},
		"none open": decisionsSpecs()[2:],
	} {
		t.Run(name, func(t *testing.T) {
			db := decisionsHome(t, specs...)
			seedSessions(t, db, statusSession(model.StateNeedsDecision))
			if out, err := runRoot(t, "status"); err != nil || out != "" {
				t.Errorf("status = %q, %v, want no D part (and nothing else)", out, err)
			}
			if name != "assume only" {
				return
			}
			// The badge is a pure read: it never sweeps expiry.
			if d := decisionsGet(t, db, "D-2"); d.Status != model.StatusOpen {
				t.Errorf("D-2 = %s after status, want still open", d.Status)
			}
		})
	}
}

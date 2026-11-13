package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
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
		// D-4's answer is queued, which Q counts (Ruling 89).
		"none open": decisionsSpecs()[2:],
	} {
		t.Run(name, func(t *testing.T) {
			db := decisionsHome(t, specs...)
			seedSessions(t, db, statusSession(model.StateNeedsDecision))
			want := map[string]string{"assume only": "", "none open": "Q1"}[name]
			if out, err := runRoot(t, "status"); err != nil || out != want {
				t.Errorf("status = %q, %v, want %q: no D part", out, err, want)
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

// Ruling 77: a denied prompt fires no hook, so status settles the session
// from its transcript and the W it held goes away.
func TestStatusDropsWAfterDenial(t *testing.T) {
	path := settleTranscript(t, settleToolUse)
	db, fake := fleetHome(t, nil, model.Session{
		ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1", TmuxWindow: "@1",
		TranscriptPath: path, UpdatedAt: at(11, 0),
	})
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Fatalf("status before the denial = %q, %v, want \"W1\"", out, err)
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(settleDenial); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status after the denial = %q, %v, want empty output", out, err)
	}
	s := getSession(t, db, "a")
	if s.State != model.StateIdle || !s.SeenAt.Equal(fleetNow) || !s.UpdatedAt.Equal(fleetNow) || s.LastUpdate != "interrupted" {
		t.Errorf("a = %s seen %v updated %v %q, want idle, seen and updated at %v, \"interrupted\"",
			s.State, s.SeenAt, s.UpdatedAt, s.LastUpdate, fleetNow)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("status called tmux: %q", fake.Calls)
	}
}

// Settling is a correction: a transcript status cannot read, or a path
// that is not a regular file (one that never ends, a FIFO that would block
// an open), leaves that session as it is, and the badge is still printed.
func TestStatusIgnoresUnreadableTranscript(t *testing.T) {
	waiting := func(id, path string) model.Session {
		return model.Session{ID: id, Agent: "claude", State: model.StateWaiting, Pane: "%1", TranscriptPath: path, UpdatedAt: at(11, 0)}
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fleetHome(t, nil,
		waiting("a", settleUnreadable(t)), waiting("c", "/dev/zero"), waiting("d", t.TempDir()), waiting("e", fifo),
		settleDenied(t, "b", model.StateWaiting, "%2"),
	)
	if out, err := runRoot(t, "status"); err != nil || out != "W4" {
		t.Errorf("status = %q, %v, want \"W4\"", out, err)
	}
	if log := readLog(t); log != nil {
		t.Errorf("status logged %q, want nothing", log)
	}
}

// Ruling 79: a marker from before the session's last hook event does not
// settle it, so the W stays.
func TestStatusKeepsWForOlderMarker(t *testing.T) {
	s := settleDenied(t, "a", model.StateWaiting, "%1")
	s.UpdatedAt = at(11, 45) // the Notification came after the 11:30 marker
	db, _ := fleetHome(t, nil, s)
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Errorf("status = %q, %v, want \"W1\"", out, err)
	}
	if got := getSession(t, db, "a"); got.State != model.StateWaiting || !got.UpdatedAt.Equal(at(11, 45)) {
		t.Errorf("a = %s at %v, want waiting at 11:45", got.State, got.UpdatedAt)
	}
}

package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
)

// dashboardRun is what one fake run of the dashboard program saw: the
// number of program options and the view after the first refresh.
type dashboardRun struct {
	opts int
	view string
}

// useFakeProgram replaces the terminal program with one that runs the
// model's first refresh and records its view.
func useFakeProgram(t *testing.T) *dashboardRun {
	t.Helper()
	run := &dashboardRun{}
	prev := runProgram
	runProgram = func(m tea.Model, opts ...tea.ProgramOption) error {
		run.opts = len(opts)
		m, _ = m.Update(m.Init()())
		run.view = m.View()
		return nil
	}
	t.Cleanup(func() { runProgram = prev })
	return run
}

// dashboardSessions: a waiting and a working session in live panes, one
// whose pane is gone (the dashboard's Source marks it stale and still shows
// it), one stale long enough to be hidden, and an ended one.
func dashboardSessions() []model.Session {
	s := func(id string, state model.State, pane string, updated time.Time) model.Session {
		return model.Session{
			ID: id, Agent: "claude", Pane: pane, TmuxSession: "work", TmuxWindow: "@1", State: state,
			Model: "model-" + id, StartedAt: at(9, 0), UpdatedAt: updated,
		}
	}
	return []model.Session{
		s("working", model.StateWorking, "%1", at(11, 0)),
		s("gone", model.StateWorking, "%9", at(11, 10)),
		s("old-stale", model.StateStale, "%8", at(11, 0)),
		s("ended", model.StateEnded, "%7", at(11, 0)),
		s("waiting", model.StateWaiting, "%2", at(11, 30)),
	}
}

func TestUICommandShowsReconciledRankedSessions(t *testing.T) {
	db, fake := fleetHome(t, []string{"%1", "%2"}, dashboardSessions()...)
	for i := range fake.Panes { // every pane is in window @1
		fake.Panes[i].WindowName = "editor"
	}
	run := useFakeProgram(t)
	if out, err := runRoot(t, "ui"); err != nil || out != "" {
		t.Fatalf("ui = %q, %v", out, err)
	}
	if run.opts != 2 {
		t.Errorf("popup ran with %d program options, want 2 (alternate screen, mouse)", run.opts)
	}
	lines := strings.Split(run.view, "\n")
	if !strings.Contains(lines[0], "SESSION:WINDOW") {
		t.Errorf("header %q is not the popup's", lines[0])
	}
	for i, want := range []string{"model-waiting", "model-working", "model-gone"} {
		if !strings.Contains(lines[i+1], want) {
			t.Errorf("row %d = %q, want %s:\n%s", i+1, lines[i+1], want, run.view)
		}
	}
	if !strings.Contains(lines[2], "work:editor") {
		t.Errorf("row %q lacks the window name work:editor", lines[2])
	}
	for _, hidden := range []string{"model-old-stale", "model-ended"} {
		if strings.Contains(run.view, hidden) {
			t.Errorf("view shows %s:\n%s", hidden, run.view)
		}
	}
	if s := getSession(t, db, "gone"); s.State != model.StateStale || !s.UpdatedAt.Equal(fleetNow) {
		t.Errorf("gone = %s at %v, want stale at %v", s.State, s.UpdatedAt, fleetNow)
	}
}

func TestSidebarCommandShowsNarrowColumns(t *testing.T) {
	fleetHome(t, []string{"%1", "%2"}, dashboardSessions()...)
	run := useFakeProgram(t)
	if out, err := runRoot(t, "sidebar"); err != nil || out != "" {
		t.Fatalf("sidebar = %q, %v", out, err)
	}
	if run.opts != 1 {
		t.Errorf("sidebar ran with %d program options, want 1 (mouse)", run.opts)
	}
	if !strings.Contains(run.view, "work:@1") || strings.Contains(run.view, "model-waiting") {
		t.Errorf("sidebar view is not the narrow layout:\n%s", run.view)
	}
}

func TestUIReportsStoreError(t *testing.T) {
	hookHome(t)
	t.Setenv("CURIA_DB", t.TempDir()) // a directory, not a database file
	useFakeProgram(t)
	if _, err := runRoot(t, "ui"); err == nil {
		t.Error("ui with an unusable database returned no error")
	}
}

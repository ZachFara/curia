package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
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

// uiDriveProgram replaces the terminal program with one that runs the
// model's first refresh, then drive, and records the view drive leaves.
func uiDriveProgram(t *testing.T, drive func(m tea.Model) tea.Model) *dashboardRun {
	t.Helper()
	run := &dashboardRun{}
	prev := runProgram
	runProgram = func(m tea.Model, opts ...tea.ProgramOption) error {
		run.opts = len(opts)
		m, _ = m.Update(m.Init()())
		run.view = drive(m).View()
		return nil
	}
	t.Cleanup(func() { runProgram = prev })
	return run
}

// uiFeed runs cmd and feeds the messages it gives (each of a batch) to m,
// without running the commands they return (the one-second ticks).
func uiFeed(m tea.Model, cmd tea.Cmd) tea.Model {
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			m = uiFeed(m, c)
		}
	case nil:
	default:
		m, _ = m.Update(msg)
	}
	return m
}

// uiPress sends key k to m and feeds back what it starts.
func uiPress(m tea.Model, k string) tea.Model {
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return uiFeed(m, cmd)
}

func TestStoreSourceSweepsExpiry(t *testing.T) {
	const z = ".000000000Z"
	db, fake := decideHome(t,
		decisionsSpec{"due", model.ModeAssume, "2026-11-11T09:00:00" + z, "2026-11-11T11:00:00" + z, decisionsEnd{}},
		decisionsSpec{"live", model.ModeAssume, "2026-11-11T09:00:00" + z, "2026-11-12T11:00:00" + z, decisionsEnd{}},
		decisionsSpec{"blocking", model.ModeBlocking, "2026-11-11T09:00:00" + z, "", decisionsEnd{}},
	)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	src := storeSource{st: st, tm: fake, cfg: config.Default()}
	if _, serr := src.Sessions(context.Background()); serr != nil {
		t.Fatal(serr)
	}
	counts, err := src.OpenDecisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (model.OpenCount{Blocking: 1, Assume: 1}); len(counts) != 1 || counts["s1"] != want {
		t.Errorf("OpenDecisions = %+v, want s1: %+v", counts, want)
	}
	if d := decisionsGet(t, db, "D-1"); d.Status != model.StatusExpired {
		t.Errorf("D-1 = %s, want expired", d.Status)
	}
}

// d opens the session's tickets in the dashboard; an answer given there is
// recorded as given over SSH when SSH_CONNECTION is set, else in the popup.
func TestUIDKeyAnswersSessionTickets(t *testing.T) {
	for _, c := range []struct {
		ssh  string
		want model.AnsweredVia
	}{{"10.0.0.1 5000 10.0.0.2 22", model.AnsweredSSH}, {"", model.AnsweredPopup}} {
		db, _ := decideHome(t, decisionsSpecs()[1]) // D-1: an open, cheap assume ticket of s1
		t.Setenv("SSH_CONNECTION", c.ssh)
		clk := &decideClock{t: decisionsParse(t, decisionsNow)}
		clock = clk.now // decisionsHome restores it
		var queue string
		run := uiDriveProgram(t, func(m tea.Model) tea.Model {
			m = uiPress(m, "d")
			queue = m.View()
			clk.advance(time.Second)
			return uiPress(m, "2")
		})
		if _, err := runRoot(t, "ui"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(queue, "Run a && b?") {
			t.Errorf("d did not open s1's tickets:\n%s", queue)
		}
		if !strings.Contains(run.view, "D-1 answered") {
			t.Errorf("view after the answer:\n%s", run.view)
		}
		d := decisionsGet(t, db, "D-1")
		if d.Status != model.StatusOverridden || d.AnsweredVia != c.want {
			t.Errorf("SSH_CONNECTION %q: D-1 = %s via %s, want overridden via %s", c.ssh, d.Status, d.AnsweredVia, c.want)
		}
	}
}

// Inside an agent session the dashboard still lists sessions, but d cannot
// answer (spec section 7) and says why.
func TestUIInsideAgentCannotAnswer(t *testing.T) {
	db, _ := decideHome(t, decisionsSpecs()[1])
	t.Setenv("CLAUDECODE", "1")
	var list string
	run := uiDriveProgram(t, func(m tea.Model) tea.Model {
		list = m.View()
		return uiPress(m, "d")
	})
	if _, err := runRoot(t, "ui"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "work:@1") {
		t.Errorf("dashboard inside an agent session lists no session:\n%s", list)
	}
	if strings.Contains(run.view, "Run a && b?") {
		t.Errorf("d opened the queue inside an agent session:\n%s", run.view)
	}
	lines := strings.Split(run.view, "\n")
	if s := lines[len(lines)-1]; !strings.Contains(s, "agent session") {
		t.Errorf("status line = %q, want why d cannot answer", s)
	}
	if d := decisionsGet(t, db, "D-1"); d.Status != model.StatusOpen {
		t.Errorf("D-1 = %s, want still open", d.Status)
	}
}

// The dashboard's source settles as ls does.
func TestUISourceSettlesInterruptedSession(t *testing.T) {
	db, fake := fleetHome(t, []string{"%1"}, settleDenied(t, "a", model.StateWaiting, "%1"))
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	got, err := storeSource{st: st, tm: fake, cfg: config.Default()}.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != model.StateIdle || got[0].LastUpdate != "interrupted" {
		t.Errorf("Sessions = %+v, want a idle and interrupted", got)
	}
	if want := []string{"ListPanes", "SetWindowOption @1 @curia_marker "}; !slices.Equal(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
}

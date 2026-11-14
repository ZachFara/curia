package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
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
		if !strings.Contains(run.view, "D-1 overridden") { // the stored status word (M2)
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

// T16: without its log file the dashboard still starts, and d says why
// it cannot answer.
func TestUIStartsWithoutLogFile(t *testing.T) {
	db, _ := decideHome(t, decisionsSpecs()[1])
	logPath := config.Default().LogPath
	if err := os.MkdirAll(logPath, 0o700); err != nil { // a directory cannot be opened for append
		t.Fatal(err)
	}
	var list string
	run := uiDriveProgram(t, func(m tea.Model) tea.Model {
		list = m.View()
		return uiPress(m, "d")
	})
	if _, err := runRoot(t, "ui"); err != nil {
		t.Fatalf("curia ui without a log file = %v, want the dashboard", err)
	}
	if !strings.Contains(list, "work:@1") {
		t.Errorf("dashboard lists no session:\n%s", list)
	}
	lines := strings.Split(run.view, "\n")
	if s := lines[len(lines)-1]; !strings.Contains(s, "log file cannot be opened") {
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

func TestStoreSourceTasks(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Auth refactor", "")
	f.task("Fix login", "")
	f.update("T-2", func(tk *model.Task) { tk.Status = model.TaskDropped })
	got, err := storeSource{st: f.st, tm: f.fake, cfg: f.cfg}.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["T-1"].Title != "Auth refactor" || got["T-2"].Title != "Fix login" {
		t.Errorf("Tasks = %+v, want T-1 and T-2 by id (a dropped task too)", got)
	}
}

// uiBoardSource is the dashboard's board source over the fixture's world.
func uiBoardSource(f *spawnFixture) boardSource {
	return boardSource{st: f.st, tm: f.fake, cfg: f.cfg}
}

func TestBoardSourceSpawnDoesNotWait(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Now", "")
	// The board bounds an action by a few seconds; a spawn (git, an approved
	// setup script) runs to its own limits instead.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	line, err := uiBoardSource(f).Spawn(ctx, "T-1")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if f.sleeps != 0 {
		t.Errorf("Spawn slept %d times, want it not to wait for the agent", f.sleeps)
	}
	tk := f.get("T-1")
	i := slices.IndexFunc(f.fake.Panes, func(p tmux.Pane) bool { return p.ID == tk.Pane })
	if i < 0 {
		t.Fatalf("no window for T-1 (pane %q): %+v", tk.Pane, f.fake.Panes)
	}
	if want := spawnLine(spawnResult{Task: tk, Pane: f.fake.Panes[i]}); line != want {
		t.Errorf("line = %q, want spawnLine's %q", line, want)
	}
}

// A setup script that is not approved is skipped by spawn, and the board's
// line says so.
func TestBoardSourceSpawnShowsSkippedSetup(t *testing.T) {
	f := spawnNewFixture(t)
	f.commitSetup("#!/bin/sh\ntouch ran\n", true)
	f.task("Now", "")
	line, err := uiBoardSource(f).Spawn(context.Background(), "T-1")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.HasPrefix(line, "T-1 launched in ") || !strings.Contains(line, "skipped") ||
		!strings.Contains(line, "curia setup approve T-1") || strings.Contains(line, "\n") {
		t.Errorf("line = %q, want the launch and the skipped setup on one line", line)
	}
	if _, err := os.Stat(filepath.Join(f.worktree("T-1"), "ran")); err == nil {
		t.Error("the unapproved setup script ran")
	}
}

func TestBoardSourceSetStatusOnlyBacklogOrReady(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("One", "")
	src, ctx := uiBoardSource(f), context.Background()
	for _, to := range []model.TaskStatus{model.TaskReady, model.TaskBacklog, model.TaskReady} {
		if err := src.SetStatus(ctx, "T-1", to); err != nil {
			t.Fatalf("SetStatus %s: %v", to, err)
		}
		if got := f.get("T-1"); got.Status != to || !got.UpdatedAt.Equal(f.now) {
			t.Errorf("after SetStatus %s: %s at %v", to, got.Status, got.UpdatedAt)
		}
	}
	for _, to := range []model.TaskStatus{model.TaskDropped, model.TaskRunning, model.TaskDone, ""} {
		if err := src.SetStatus(ctx, "T-1", to); err == nil {
			t.Errorf("SetStatus %q succeeded, want a refusal", to)
		}
	}
	if got := f.get("T-1").Status; got != model.TaskReady {
		t.Errorf("after refusals T-1 is %s, want ready", got)
	}
	// A task dropped meanwhile is not brought back.
	f.update("T-1", func(tk *model.Task) { tk.Status = model.TaskDropped })
	if err := src.SetStatus(ctx, "T-1", model.TaskReady); err == nil || !strings.Contains(err.Error(), "dropped") {
		t.Errorf("SetStatus on a dropped task = %v, want a refusal naming dropped", err)
	}
	if got := f.get("T-1").Status; got != model.TaskDropped {
		t.Errorf("dropped task is now %s", got)
	}
	if err := src.SetStatus(ctx, "T-9", model.TaskReady); err == nil || !strings.Contains(err.Error(), "no such task") {
		t.Errorf("SetStatus on a missing task = %v", err)
	}
}

func TestDashboardOffersBoardInPopupOnly(t *testing.T) {
	fleetHome(t, []string{"%1", "%2"}, dashboardSessions()...)
	run := uiDriveProgram(t, func(m tea.Model) tea.Model { return uiPress(m, "t") })
	if _, err := runRoot(t, "ui"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.view, "no tasks; add one with curia task add") {
		t.Errorf("t in the popup did not open the board:\n%s", run.view)
	}
	run = uiDriveProgram(t, func(m tea.Model) tea.Model { return uiPress(m, "t") })
	if _, err := runRoot(t, "sidebar"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(run.view, "\n")
	if s := lines[len(lines)-1]; !strings.Contains(s, "curia ui") || strings.Contains(run.view, "no tasks") {
		t.Errorf("t in the sidebar:\n%s", run.view)
	}
}

// The popup's TASK column shows the session's task from the store.
func TestUIShowsTaskColumn(t *testing.T) {
	sessions := dashboardSessions()
	sessions[0].TaskID = "T-1"
	db, _ := fleetHome(t, []string{"%1", "%2"}, sessions...)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateTask(context.Background(), model.Task{
		Title: "Auth refactor", Repo: "/src/api", Status: model.TaskReady, CreatedAt: fleetNow,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	run := useFakeProgram(t)
	if _, err := runRoot(t, "ui"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.view, "T-1 Auth refactor") {
		t.Errorf("view lacks the task:\n%s", run.view)
	}
}

// The dashboard's refresh reads the team configs, also when tmux cannot be
// reached, and shows the team as one row that Space expands.
func TestUIGroupsTeamFromConfig(t *testing.T) {
	_, teams := teamsHome(t, teamsCapturedSessions()...) // tmux unreachable: the guard's client
	teamsWriteCaptured(t, teams)
	var collapsed string
	run := uiDriveProgram(t, func(m tea.Model) tea.Model {
		collapsed = m.View()
		return uiPress(m, " ")
	})
	if _, err := runRoot(t, "ui"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(collapsed, "▸ ") || !strings.Contains(collapsed, " +2") || strings.Contains(collapsed, "alpha") {
		t.Errorf("collapsed view, want one group row of the lead and 2 teammates:\n%s", collapsed)
	}
	for _, want := range []string{"▾ ", "  alpha", "  beta", "T-5"} {
		if !strings.Contains(run.view, want) {
			t.Errorf("expanded view lacks %q:\n%s", want, run.view)
		}
	}
}

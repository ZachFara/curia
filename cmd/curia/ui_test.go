package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
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
	userTerminalAs(t, true) // the user, at a terminal
	run := &dashboardRun{}
	prev := runProgram
	runProgram = func(m tea.Model, opts ...tea.ProgramOption) error {
		run.opts = len(opts)
		m = uiToSessionList(m)
		run.view = m.View()
		return nil
	}
	t.Cleanup(func() { runProgram = prev })
	return run
}

// uiToSessionList runs the popup's first messages: it opens on the inbox
// (Ruling 154), so Tab shows the session list and its first refresh.
func uiToSessionList(m tea.Model) tea.Model {
	m, _ = m.Update(m.Init()())
	if !strings.Contains(m.View(), "tab sessions") {
		return m // the sidebar, or a view without the inbox
	}
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m, cmd = m.Update(cmd()) // the inbox closes and the list refreshes
	m, _ = m.Update(cmd())
	return m
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
	if run.opts != 3 {
		t.Errorf("popup ran with %d program options, want 3 (alternate screen, mouse, the signal context)", run.opts)
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
	if run.opts != 2 {
		t.Errorf("sidebar ran with %d program options, want 2 (mouse, the signal context)", run.opts)
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
	userTerminalAs(t, true) // the user, at a terminal
	run := &dashboardRun{}
	prev := runProgram
	runProgram = func(m tea.Model, opts ...tea.ProgramOption) error {
		run.opts = len(opts)
		m = uiToSessionList(m)
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
		var next tea.Cmd
		m, next = m.Update(msg)
		if strings.Contains(fmt.Sprintf("%#v", msg), "ConfirmMsg") { // the taken y's action after burstGap; a child view wraps the message
			m = uiFeed(m, next)
		}
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

// The board's line carries the spawn's note (the cheaper model under a
// limit forecast), on the same one line.
func TestBoardSourceSpawnShowsNote(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Now", "")
	f.spawnWarning()
	line, err := uiBoardSource(f).Spawn(context.Background(), "T-1")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.HasPrefix(line, "T-1 launched in ") || !strings.HasSuffix(line, "; "+spawnWarningNote("T-1")) ||
		strings.Contains(line, "\n") {
		t.Errorf("line = %q, want the launch and then the note %q on one line", line, spawnWarningNote("T-1"))
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

// uiReviewStore seeds the store at db with tasks T-1 and T-2, each with a
// pending review (R-1 and R-2), and then stores T-2 dropped.
func uiReviewStore(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, title := range []string{"Auth refactor", "Fix login"} {
		tk, cerr := st.CreateTask(ctx, model.Task{Title: title, Repo: "/src/api", Status: model.TaskReady, CreatedAt: fleetNow})
		if cerr != nil {
			t.Fatal(cerr)
		}
		if _, _, err = st.CompleteTask(ctx, model.Review{
			TaskID: tk.ID, Summary: "Done.", Tests: model.TestsNotRun, CreatedAt: fleetNow,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.UpdateTask(ctx, "T-2", func(cur model.Task) (model.Task, error) {
		cur.Status = model.TaskDropped
		return cur, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreSourceReviews(t *testing.T) {
	db, fake := fleetHome(t, nil)
	uiReviewStore(t, db)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	got, err := storeSource{st: st, tm: fake, cfg: config.Default()}.Reviews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"T-1": "R-1"}; !maps.Equal(got, want) {
		t.Errorf("Reviews = %v, want %v (a dropped task's review absent)", got, want)
	}
}

// uiReviewKeyCase is one run of TestDashboardOffersReviewInPopupOnly: the
// command, CLAUDECODE, whether the log file can be opened, and what the
// view shows after r.
type uiReviewKeyCase struct {
	name, cmd, claudecode string
	noLog                 bool
	want                  string
}

// The popup opens the review of the selected session's task, inside an
// agent session too, where the view is read-only (its actions refuse with
// the user-only guard's reason: uiUserActions); the sidebar and the popup
// without its log file say why not.
func TestDashboardOffersReviewInPopupOnly(t *testing.T) {
	for _, c := range []uiReviewKeyCase{
		{name: "popup", cmd: "ui", want: "R-1"},
		{name: "sidebar", cmd: "sidebar", want: "open the popup to review"},
		{name: "agent", cmd: "ui", claudecode: "1", want: "R-1"},
		{name: "no log", cmd: "ui", noLog: true, want: "reviews cannot be opened here: the log file cannot be opened"},
	} {
		t.Run(c.name, func(t *testing.T) { uiReviewKey(t, c) })
	}
}

func uiReviewKey(t *testing.T, c uiReviewKeyCase) {
	t.Helper()
	sessions := dashboardSessions()
	sessions[4].TaskID = "T-1" // "waiting", the first row
	db, _ := fleetHome(t, []string{"%1", "%2"}, sessions...)
	uiReviewStore(t, db)
	t.Setenv("CLAUDECODE", c.claudecode)
	if c.noLog {
		if err := os.MkdirAll(config.Default().LogPath, 0o700); err != nil { // a directory cannot be opened for append
			t.Fatal(err)
		}
	}
	clk := &decideClock{t: fleetNow}
	clock = clk.now // fleetHome restores it
	var table string
	run := uiDriveProgram(t, func(m tea.Model) tea.Model {
		m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
		table = m.View()
		clk.advance(500 * time.Millisecond) // past the dwell
		m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
		return reviewPump(t, m, cmd)
	})
	if _, err := runRoot(t, c.cmd); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table, "work:@1") {
		t.Fatalf("the dashboard did not start:\n%s", table)
	}
	if !strings.Contains(run.view, c.want) {
		t.Errorf("after r, view lacks %q:\n%s", c.want, run.view)
	}
	if opened := !strings.Contains(run.view, "work:@1"); opened != (c.name == "popup" || c.name == "agent") {
		t.Errorf("after r, the review view is shown: %v:\n%s", opened, run.view)
	}
}

// uiUserActions are the dashboard's actions with a user's effect (re-review
// A, C1): each must do nothing, and say why, unless the user-only guard
// lets the dashboard's caller through. A new such action belongs here.
var uiUserActions = []struct {
	name string
	// world builds the fixture; nil is uiActionWorld. A review action's
	// world is uiReviewWorld, and its keys are run to the end of what they
	// start (the review view reads and acts in commands).
	world func(t *testing.T) *spawnFixture
	keys  []string                   // "enter" is the Enter key, anything else typed runes
	done  func(f *spawnFixture) bool // whether the action took effect
}{
	{"board: r marks a backlog task ready", nil, []string{"t", "r"}, func(f *spawnFixture) bool {
		return f.get("T-1").Status == model.TaskReady
	}},
	{"board: n then y spawns a backlog task", nil, []string{"t", "n", "y"}, func(f *spawnFixture) bool {
		return len(f.windows()) > 0
	}},
	{"x then y kills an agent's pane", nil, []string{"x", "y"}, func(f *spawnFixture) bool {
		return slices.Contains(f.fake.Calls, "KillPane %5")
	}},
	{"review: a then y approves (merges)", uiReviewWorld, []string{"r", "a", "y"}, uiReviewClosed},
	{"review: c, a note, Enter requests changes", uiReviewWorld, []string{"r", "c", "fix the tests", "enter"}, uiReviewClosed},
	{"review: x then y drops", uiReviewWorld, []string{"r", "x", "y"}, uiReviewClosed},
	{"settings: e, a value, Enter, y changes a setting", uiActionWorld, []string{"S", "e", "20m", "enter", "y"}, func(*spawnFixture) bool {
		home, _ := os.UserHomeDir()
		b, _ := os.ReadFile(filepath.Clean(configFile(home)))
		return strings.Contains(string(b), "20m")
	}},
	{"P then y pauses the suggested task", uiPauseWorld, []string{"P", "y"}, func(f *spawnFixture) bool {
		return !f.get("T-1").PausedAt.IsZero()
	}},
}

// uiPauseWorld is lifecycleWithLead (T-1 spawned, its lead idle in pane
// %100) under a limit forecast (spawnWarning), so the dashboard suggests
// pausing T-1.
func uiPauseWorld(t *testing.T) *spawnFixture {
	t.Helper()
	f := lifecycleWithLead(t)
	f.spawnWarning()
	return f
}

// uiReviewWorld is reviewWithLead: T-1 spawned with a commit on its branch
// and review R-1 pending, its lead idle in pane %100, the dashboard's only
// session.
func uiReviewWorld(t *testing.T) *spawnFixture {
	t.Helper()
	return reviewWithLead(t, model.StateIdle).spawnFixture
}

// uiReviewClosed reports whether review R-1 is no longer pending.
func uiReviewClosed(f *spawnFixture) bool {
	r, found, err := f.st.GetReview(context.Background(), "R-1")
	if err != nil || !found {
		f.t.Fatalf("GetReview(R-1): found %v, err %v", found, err)
	}
	return r.Status != model.ReviewPending
}

// uiRunActions runs curia ui on f and presses keys, two seconds apart (past
// the board's dwell and quiet periods), and returns the view after each key.
// With pump, each key's commands are run to the end, waiting for an action
// in flight (reviewPumpActions), as the review view and a pause need;
// otherwise only the messages they give are fed.
func uiRunActions(t *testing.T, f *spawnFixture, keys []string, pump bool) string {
	t.Helper()
	var view string
	prev := runProgram
	runProgram = func(m tea.Model, _ ...tea.ProgramOption) error {
		m = uiToSessionList(m)
		for _, k := range keys {
			f.now = f.now.Add(2 * time.Second)
			msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
			if k == "enter" {
				msg = tea.KeyMsg{Type: tea.KeyEnter}
			}
			var cmd tea.Cmd
			m, cmd = m.Update(msg)
			if pump {
				m = reviewPumpActions(t, m, cmd)
			} else {
				m = uiFeed(m, cmd)
			}
			view += m.View() + "\n"
		}
		return nil
	}
	t.Cleanup(func() { runProgram = prev })
	if _, err := runRoot(t, "ui"); err != nil {
		t.Fatal(err)
	}
	return view
}

// uiActionWorld is a spawn fixture with two backlog tasks and an idle
// agent session in pane %5.
func uiActionWorld(t *testing.T) *spawnFixture {
	t.Helper()
	f := spawnNewFixture(t)
	f.task("Backlog one", "")
	f.task("Backlog two", "")
	f.fake.Panes = []tmux.Pane{{ID: "%5", Session: "work", WindowID: "@5", ServerStart: f.now.Add(-time.Hour)}}
	if err := f.st.UpsertSession(context.Background(), model.Session{
		ID: "agent-x", Agent: "claude", State: model.StateIdle, Pane: "%5", TmuxSession: "work", TmuxWindow: "@5",
		StartedAt: f.now.Add(-time.Minute), UpdatedAt: f.now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// uiWorld builds the fixture of an action: world, or uiActionWorld.
func uiWorld(t *testing.T, world func(t *testing.T) *spawnFixture) *spawnFixture {
	t.Helper()
	if world == nil {
		return uiActionWorld(t)
	}
	return world(t)
}

// TestDashboardActionsNeedTheUser: with an agent marker set, and with no
// controlling terminal, every user action of the dashboard changes nothing
// and shows the guard's reason; as the user each one acts.
func TestDashboardActionsNeedTheUser(t *testing.T) {
	for _, a := range uiUserActions {
		for _, c := range []struct {
			name, why string
			set       func(t *testing.T)
		}{
			{"agent marker", "CLAUDECODE is set", func(t *testing.T) { t.Setenv("CLAUDECODE", "1") }},
			{"no terminal", "cannot run without a terminal", func(t *testing.T) { userTerminalAs(t, false) }},
		} {
			t.Run(a.name+"/"+c.name, func(t *testing.T) {
				f := uiWorld(t, a.world)
				c.set(t)
				view := uiRunActions(t, f, a.keys, a.world != nil)
				if a.done(f) {
					t.Errorf("%s took effect for a caller the guard refuses", a.name)
				}
				if !strings.Contains(strings.Join(strings.Fields(view), " "), c.why) { // the review view wraps it
					t.Errorf("view does not give the guard's reason %q:\n%s", c.why, view)
				}
				if got := f.get("T-1").Status; a.world == nil && got != model.TaskBacklog {
					t.Errorf("T-1 = %s, want backlog", got)
				}
			})
		}
		t.Run(a.name+"/the user", func(t *testing.T) {
			f := uiWorld(t, a.world)
			view := uiRunActions(t, f, a.keys, a.world != nil)
			if !a.done(f) {
				t.Errorf("%s did not take effect for the user:\n%s", a.name, view)
			}
		})
	}
}

// The board's source refuses by itself too: a spawn for a refused caller
// is not made, and would be ReadyOnly if it were (Ruling 113).
func TestBoardSourceRefusesForANonUser(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Backlog", "")
	b := uiBoardSource(f)
	b.refused = errors.New("curia ui cannot run inside an agent session (CLAUDECODE is set)")
	if _, err := b.Spawn(context.Background(), "T-1"); err == nil || len(f.windows()) != 0 {
		t.Errorf("Spawn = %v, windows %q: want the refusal and nothing spawned", err, f.windows())
	}
	if err := b.SetStatus(context.Background(), "T-1", model.TaskReady); err == nil || f.get("T-1").Status != model.TaskBacklog {
		t.Errorf("SetStatus = %v, T-1 %s: want the refusal and backlog", err, f.get("T-1").Status)
	}
}

// uiCost is one ingested reading of TestStoreSourceBudget's world: a
// session's running cost at a time before fleetNow, and the five-hour
// window's percentage when pct is not negative.
type uiCost struct {
	session string
	ago     time.Duration
	usd     float64
	pct     float64
}

// uiBudgetSource opens the store of a fleetHome world of
// dashboardSessions, ingests readings, and returns the dashboard's source.
func uiBudgetSource(t *testing.T, readings ...uiCost) storeSource {
	t.Helper()
	db, fake := fleetHome(t, []string{"%1", "%2"}, dashboardSessions()...)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, r := range readings {
		usd := r.usd
		in := store.Reading{SessionID: r.session, At: fleetNow.Add(-r.ago), CostUSD: &usd}
		if r.pct >= 0 {
			in.Limits = []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: r.pct, ResetsAt: fleetNow.Add(110 * time.Minute)}}
		}
		if _, err = st.Ingest(context.Background(), in, fleetNow); err != nil {
			t.Fatal(err)
		}
	}
	return storeSource{st: st, tm: fake, cfg: config.Default()}
}

func TestStoreSourceBudget(t *testing.T) {
	got, err := uiBudgetSource(t).Budget(context.Background())
	if err != nil || !reflect.DeepEqual(got, ui.Budget{Res: map[string]string{}}) {
		t.Errorf("Budget with no samples = %+v, %v, want the zero Budget", got, err)
	}
	// The five-hour window goes from 50% to 62% in 16 minutes (45%/h): the
	// limit in about 50 minutes, before the reset in 1h50m. working's cost
	// rises $1 in the window, waiting's $0.50 since its sample before it.
	src := uiBudgetSource(t,
		uiCost{"waiting", 25 * time.Minute, 0.5, -1},
		uiCost{"working", 16 * time.Minute, 1, 50},
		uiCost{"waiting", 5 * time.Minute, 1, -1},
		uiCost{"working", 0, 2, 62},
	)
	got, err = src.Budget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := ui.Budget{
		Status: "5h 62% → limit ~50m (resets 1h50m)",
		Burn:   map[string]float64{"working": 3, "waiting": 1.5},
		Res:    map[string]string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Budget = %+v, want %+v", got, want)
	}
}

func TestStoreSourceBudgetAPIMode(t *testing.T) {
	// No usage sample: API mode. working's cost rises $1.50 in 15 minutes,
	// $9.00 over the next two hours; ended's rise is not a live session's.
	readings := []uiCost{
		{"working", 15 * time.Minute, 1, -1},
		{"ended", 15 * time.Minute, 1, -1},
		{"working", 0, 2.5, -1},
		{"ended", 0, 6, -1},
	}
	got, err := uiBudgetSource(t, readings...).Budget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "next 2h ≈ $9.00 at the current rate"; got.Status != want {
		t.Errorf("Status = %q, want %q", got.Status, want)
	}
	if want := map[string]float64{"working": 4.5, "ended": 15}; !maps.Equal(got.Burn, want) {
		t.Errorf("Burn = %v, want %v", got.Burn, want)
	}
	// A usage sample in the window is a subscription: no projection, and
	// with no warning no status at all.
	readings = append(readings, uiCost{"waiting", time.Minute, 0, 10})
	got, err = uiBudgetSource(t, readings...).Budget(context.Background())
	if err != nil || got.Status != "" || len(got.Burn) != 3 {
		t.Errorf("Budget with a steady usage window = %+v, %v, want no status and three burns", got, err)
	}
}

func TestStoreSourceBudgetSuggestsPause(t *testing.T) {
	// The series of curia usage's test: 50, 56 and 62% over 14 minutes, the
	// limit in about 43 minutes, before the reset in 1h50m. T-1 (P2) and T-2
	// (P3) are running.
	db := usageWarningHome(t)
	usageTask(t, db, "/src/web", 2, "sa", "w1")
	usageTask(t, db, "/src/api", 3, "sb", "w2")
	usageCost(t, db, "sa", 11.5)
	usageCost(t, db, "sb", 10.3)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	src := storeSource{st: st, tm: &tmux.Fake{}, cfg: config.Default()}
	got, err := src.Budget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.PauseTask != "T-2" || got.PauseLabel != "T-2 (P3) api" || got.PauseTitle != "Fix sb" || got.PauseLimit != "limit ~43m" {
		t.Errorf("Budget = %+v, want T-2 (P3) api \"Fix sb\", limit ~43m", got)
	}
	if got.Status != "5h 62% → limit ~43m (resets 1h50m)" || len(got.Burn) != 2 {
		t.Errorf("Budget = %+v, want the warning and both burns as before", got)
	}
	// The same suggestion as curia usage's, from the same function.
	rep, err := usageBuild(context.Background(), st, clock())
	if err != nil || rep.PauseTask != got.PauseTask || rep.PauseLabel != got.PauseLabel || rep.PauseTitle != got.PauseTitle {
		t.Errorf("usageBuild = %+v, %v; Budget = %+v", rep, err, got)
	}
}

func TestStoreSourceBudgetNoPauseWithoutWarning(t *testing.T) {
	db, _ := fleetHome(t, nil)
	usageSeries(t, db, fleetNow.Add(20*time.Minute), 50, 56, 62) // resets first
	usageTask(t, db, "/src/api", 3, "sb", "w2")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	got, err := storeSource{st: st, tm: &tmux.Fake{}, cfg: config.Default()}.Budget(context.Background())
	if err != nil || got.PauseTask != "" || got.PauseLabel != "" || got.PauseTitle != "" || got.PauseLimit != "" || got.Status != "" {
		t.Errorf("Budget without a warning = %+v, %v, want no status and no suggestion", got, err)
	}
}

func TestStoreSourcePauseRunsPause(t *testing.T) {
	f := uiPauseWorld(t)
	src := storeSource{st: f.st, tm: f.fake, cfg: f.cfg}
	ctx := context.Background()
	if b, err := src.Budget(ctx); err != nil || b.PauseTask != "T-1" {
		t.Fatalf("Budget = %+v, %v, want T-1 suggested", b, err)
	}
	// Only the suggested task: another id is refused and nothing is done.
	if _, err := src.Pause(ctx, "T-2"); err == nil || !strings.Contains(err.Error(), "the suggestion changed") ||
		!strings.Contains(err.Error(), "T-1") {
		t.Errorf("Pause(T-2) = %v, want a refusal naming the suggestion T-1", err)
	}
	refused := src
	refused.refused = errors.New("curia ui cannot run inside an agent session (CLAUDECODE is set)")
	if _, err := refused.Pause(ctx, "T-1"); err == nil || !strings.Contains(err.Error(), "CLAUDECODE is set") {
		t.Errorf("Pause for a refused caller = %v, want the guard's reason", err)
	}
	lifecycleWantKept(f)
	if got := lifecycleKills(f); len(got) != 0 {
		t.Fatalf("a refused pause killed %q", got)
	}
	wt := f.worktree("T-1")
	line, err := src.Pause(ctx, "T-1")
	if err != nil {
		t.Fatalf("Pause(T-1): %v", err)
	}
	if want := "Paused T-1: curia resume T-1 brings it back (it continues session " + spawnShortID(lifecycleLead) + ")"; line != want {
		t.Errorf("line = %q, want the pause's last line %q", line, want)
	}
	if worktreeExists(wt) {
		t.Errorf("worktree %s still on disk", wt)
	}
	if got := worktreeGit(t, f.repo, "branch", "--list", lifecycleBranch); !strings.Contains(got, lifecycleBranch) {
		t.Errorf("branch %s is gone: %q", lifecycleBranch, got)
	}
	if tk := f.get("T-1"); !tk.PausedAt.Equal(f.now) {
		t.Errorf("paused_at = %v, want %v", tk.PausedAt, f.now)
	}
	if got := lifecycleKills(f); !slices.Equal(got, []string{"KillPane %100"}) {
		t.Errorf("kills = %q, want the task's pane", got)
	}
}

// Without a warning nothing is suggested, so nothing is paused, and pause's
// own refusals (a working agent, without --force) come back as the error.
func TestStoreSourcePauseRefusals(t *testing.T) {
	f := lifecycleWithLead(t)
	src := storeSource{st: f.st, tm: f.fake, cfg: f.cfg}
	if _, err := src.Pause(context.Background(), "T-1"); err == nil || !strings.Contains(err.Error(), "nothing is suggested") {
		t.Errorf("Pause without a warning = %v, want a refusal", err)
	}
	lifecycleWantKept(f)
	f.spawnWarning()
	lifecycleSession(f, lifecycleLead, "%100", model.StateWorking, f.now)
	_, err := src.Pause(context.Background(), "T-1")
	if err == nil || !strings.Contains(err.Error(), "is working") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("Pause of a working agent = %v, want pause's refusal", err)
	}
	lifecycleWantKept(f)
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("kills = %q, want none", got)
	}
}

// uiPauseHome is a fleetHome under curia usage's warning series with
// T-1 (P2, /src/web) and T-2 (P3, /src/api) running: their idle sessions
// sa and sb are in the live panes %1 and %2, so the dashboard's reconcile
// keeps them.
func uiPauseHome(t *testing.T) {
	t.Helper()
	db, _ := fleetHome(t, []string{"%1", "%2"})
	usageSeries(t, db, fleetNow.Add(110*time.Minute), 50, 56, 62)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i, c := range []struct {
		repo, sid string
		priority  int
	}{{"/src/web", "sa", 2}, {"/src/api", "sb", 3}} {
		tk, err := st.CreateTask(context.Background(), model.Task{
			Title: "Fix " + c.sid, Repo: c.repo, Status: model.TaskReady, Priority: c.priority, CreatedAt: fleetNow.Add(-time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.UpsertSession(context.Background(), model.Session{
			ID: c.sid, Agent: "claude", State: model.StateIdle, Pane: "%" + strconv.Itoa(i+1), TmuxSession: "work",
			TmuxWindow: "@1", TaskID: tk.ID, Repo: c.repo, StartedAt: fleetNow.Add(-time.Hour), UpdatedAt: fleetNow.Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// The popup run by the user offers P (its notice line names the suggestion
// and P opens the prompt); the sidebar and the popup inside an agent session
// do not.
func TestDashboardOffersPauseInPopupOnly(t *testing.T) {
	for _, c := range []struct {
		name, cmd, claudecode string
		notice                bool
		want                  string
	}{
		{"popup", "ui", "", true, `pause T-2 "Fix sb"?`},
		{"sidebar", "sidebar", "", false, "pausing is not available here"},
		{"agent", "ui", "1", false, "CLAUDECODE is set"},
	} {
		t.Run(c.name, func(t *testing.T) {
			uiPauseHome(t)
			t.Setenv("CLAUDECODE", c.claudecode)
			var table string
			run := uiDriveProgram(t, func(m tea.Model) tea.Model {
				m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
				table = m.View()
				return uiPress(m, "P")
			})
			if _, err := runRoot(t, c.cmd); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(table, "before reset: P pauses T-2 (P3) api"); got != c.notice {
				t.Errorf("notice line shown: %v, want %v:\n%s", got, c.notice, table)
			}
			lines := strings.Split(run.view, "\n")
			if s := lines[len(lines)-1]; !strings.Contains(s, c.want) {
				t.Errorf("after P the status line = %q, want %q", s, c.want)
			}
		})
	}
}

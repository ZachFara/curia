package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/team"
	"github.com/ZachFara/curia/internal/tmux"
)

// Paths the restore tests' seams return, and the socket of the fake server.
const (
	restoreTestSocket = "/private/tmp/tmux-501/default"
	restoreTestBin    = "/opt/curia/bin/curia"
	restoreTestClaude = "/usr/local/bin/claude"
)

// restoreUnexpectedLookPath and restoreUnexpectedBin are the seams under
// test: a test that reaches them without setting its own fails, so no test
// can find and start the real claude or this test binary in a window.
func restoreUnexpectedLookPath(file string) (string, error) {
	panic("restoreLookPath called without a test's own: " + file)
}

func restoreUnexpectedBin() (string, error) {
	panic("restoreBin called without a test's own")
}

// The stub claude of TestRestoreInRealTmux is this test binary, linked under
// restoreStubName: it records its arguments, working directory and pane in
// $HOME/restoreStubRecord and then waits to be killed with its server. It
// reads nothing from its terminal.
const (
	restoreStubName   = "restore-claude"
	restoreStubRecord = "restore-stub-record.json"
)

// restoreStubRun is what the stub recorded.
type restoreStubRun struct {
	Args []string `json:"args"`
	Cwd  string   `json:"cwd"`
	Pane string   `json:"pane"`
}

func init() {
	restoreLookPath = restoreUnexpectedLookPath
	restoreBin = restoreUnexpectedBin
	if filepath.Base(os.Args[0]) == restoreStubName {
		os.Exit(restoreStubMain())
	}
}

// restoreStubMain is the stub claude's main.
func restoreStubMain() int {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	b, _ := json.Marshal(restoreStubRun{Args: os.Args[1:], Cwd: cwd, Pane: os.Getenv("TMUX_PANE")})
	if os.WriteFile(filepath.Clean(filepath.Join(home, restoreStubRecord)), b, 0o600) != nil {
		return 3
	}
	time.Sleep(time.Minute)
	return 0
}

// restoreTmux is a tmux.Fake that keeps every WindowSpec it was given, fails
// the NewWindow calls named in failOn (by 0-based call number), and gives
// each pane it opens the server's start and socket.
type restoreTmux struct {
	*tmux.Fake
	specs  []tmux.WindowSpec
	failOn map[int]error
	start  time.Time
}

func (r *restoreTmux) NewWindow(ctx context.Context, w tmux.WindowSpec) (tmux.Pane, error) {
	n := len(r.specs)
	r.specs = append(r.specs, w)
	if err := r.failOn[n]; err != nil {
		return tmux.Pane{}, err
	}
	p, err := r.Fake.NewWindow(ctx, w)
	if err != nil {
		return p, err
	}
	p.ServerStart, p.Socket = r.start, restoreTestSocket
	r.Panes[len(r.Panes)-1] = p
	return p, nil
}

// removePane closes pane id, as when its process exits.
func (r *restoreTmux) removePane(id string) {
	r.Panes = slices.DeleteFunc(r.Panes, func(p tmux.Pane) bool { return p.ID == id })
}

// restoreFixture is one restore test's world: a store, a fake tmux server
// that started at start with one pane (%0) of its own, a fixed clock that
// restoreSleep advances, claude at restoreTestClaude and curia at
// restoreTestBin, no terminal.
type restoreFixture struct {
	t         *testing.T
	db        string
	st        *store.Store
	tm        *restoreTmux
	start     time.Time // the tmux server's start
	now       time.Time
	sleeps    int
	onSleep   func(n int) // called after the n-th fake sleep
	root      string      // directories and transcripts live here
	claudeErr error       // restoreLookPath's error, when set
	repo      string      // the git repository of linked tests ("" until restoreLinked)
}

func restoreNewFixture(t *testing.T) *restoreFixture {
	t.Helper()
	userTerminalAs(t, true) // the user, at a terminal: restore is user-only
	f := &restoreFixture{t: t, db: hookHome(t)}
	f.start = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	f.now = f.start.Add(time.Hour)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.root = root
	st, err := store.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	f.st = st
	f.tm = &restoreTmux{start: f.start, Fake: &tmux.Fake{Panes: []tmux.Pane{{
		ID: "%0", Session: "main", WindowID: "@0", WindowName: "zsh", ServerStart: f.start, Socket: restoreTestSocket,
	}}}}
	prevClient, prevClock, prevSleep := newTmuxClient, clock, restoreSleep
	prevLook, prevBin, prevTerm := restoreLookPath, restoreBin, restoreIsTerminal
	newTmuxClient = func() tmux.Client { return f.tm }
	clock = func() time.Time { return f.now }
	restoreSleep = func(d time.Duration) {
		f.now = f.now.Add(d)
		f.sleeps++
		if f.onSleep != nil {
			f.onSleep(f.sleeps)
		}
	}
	restoreLookPath = func(file string) (string, error) {
		if file != "claude" {
			return "", errors.New("unexpected lookup of " + file)
		}
		if f.claudeErr != nil {
			return "", f.claudeErr
		}
		return restoreTestClaude, nil
	}
	restoreBin = func() (string, error) { return restoreTestBin, nil }
	restoreIsTerminal = func() bool { return false }
	t.Cleanup(func() {
		newTmuxClient, clock, restoreSleep = prevClient, prevClock, prevSleep
		restoreLookPath, restoreBin, restoreIsTerminal = prevLook, prevBin, prevTerm
		restoreDryRun, restoreYes, restoreSince, restoreIncludeClosed = false, false, 0, false
		_ = st.Close()
	})
	return f
}

// restoreID is the n-th test session id, a valid Claude Code session id.
func restoreID(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", n, n) }

// touch writes a transcript at path, last modified age before now.
func (f *restoreFixture) touch(path string, age time.Duration) {
	f.t.Helper()
	worktreeWrite(f.t, path, "{}\n")
	at := f.now.Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		f.t.Fatal(err)
	}
}

// session builds a good candidate n: a Claude Code session killed in pane
// %7 of the previous server, in its own directory repo<n>, with a
// transcript last written 10 minutes ago; edit changes it. It stores
// nothing.
func (f *restoreFixture) session(n int, edit func(*model.Session)) model.Session {
	f.t.Helper()
	dir := filepath.Join(f.root, fmt.Sprintf("repo%d", n))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	tr := filepath.Join(f.root, "transcripts", fmt.Sprintf("t%d.jsonl", n))
	f.touch(tr, 10*time.Minute+time.Duration(n)*time.Second)
	s := model.Session{
		ID: restoreID(n), Agent: "claude", Pane: "%7", TmuxSession: "api-server", TmuxWindow: "@3",
		Cwd: dir, Repo: dir, Branch: "main", State: model.StateWorking, TranscriptPath: tr,
		StartedAt: f.start.Add(-2 * time.Hour), UpdatedAt: f.now.Add(-10 * time.Minute), Socket: restoreTestSocket,
	}
	if edit != nil {
		edit(&s)
	}
	return s
}

// seed stores session(n, edit) and returns it as stored.
func (f *restoreFixture) seed(n int, edit func(*model.Session)) model.Session {
	f.t.Helper()
	s := f.session(n, edit)
	if err := f.st.UpsertSession(context.Background(), s); err != nil {
		f.t.Fatal(err)
	}
	return f.get(s.ID)
}

func (f *restoreFixture) get(id string) model.Session {
	f.t.Helper()
	s, found, err := f.st.GetSession(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("GetSession(%s): found %v, err %v", id, found, err)
	}
	return s
}

// report acts out the resumed session's SessionStart: the row is live in
// pane, started now.
func (f *restoreFixture) report(id, pane string) {
	f.t.Helper()
	if err := f.st.UpdateSession(context.Background(), id, func(s model.Session, _ bool) model.Session {
		s.Pane, s.State, s.StartedAt, s.UpdatedAt, s.EndReason = pane, model.StateIdle, f.now, f.now, ""
		return s
	}); err != nil {
		f.t.Fatal(err)
	}
}

// reportAfter reports id in the first window opened (%100) after the n-th
// fake sleep.
func (f *restoreFixture) reportAfter(n int, id string) {
	f.onSleep = func(i int) {
		if i == n {
			f.report(id, "%100")
		}
	}
}

// run runs `curia restore <args>` with stdin and returns what it printed
// (stdout and stderr) and its error.
func (f *restoreFixture) run(stdin io.Reader, args ...string) (string, error) {
	f.t.Helper()
	restoreDryRun, restoreYes, restoreSince, restoreIncludeClosed = false, false, 0, false
	var out bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"restore"}, args...))
	f.t.Cleanup(func() {
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// claimFree reports whether another store handle can take the restore claim
// now (and gives it back).
func (f *restoreFixture) claimFree() bool {
	f.t.Helper()
	other, err := store.Open(f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	ok, err := other.ClaimRestore(context.Background(), f.now, restoreHold)
	if err != nil {
		f.t.Fatal(err)
	}
	if ok {
		if err := other.ReleaseRestore(context.Background()); err != nil {
			f.t.Fatal(err)
		}
	}
	return ok
}

// restoreLinked makes the fixture's git repository api-server (once) and a
// task id with a worktree on its branch, its window recorded as pane %7 two
// hours before the server started; edit changes the task before it is
// stored. It returns the task as stored.
func (f *restoreFixture) restoreLinked(id string, edit func(*model.Task)) model.Task {
	f.t.Helper()
	ctx := context.Background()
	if f.repo == "" {
		f.repo = worktreeRepo(f.t, "api-server")
	}
	tk, err := f.st.CreateTask(ctx, task.Normalize(model.Task{
		Title: "Task " + id, Repo: f.repo, BaseBranch: "main", Status: model.TaskReady, CreatedAt: f.now,
	}))
	if err != nil {
		f.t.Fatal(err)
	}
	if tk.ID != id {
		f.t.Fatalf("created %s, want %s", tk.ID, id)
	}
	branch := task.Branch(id, tk.Title)
	wt := task.WorktreePath(f.repo, id)
	gitRun(f.t, f.repo, "worktree", "add", "-q", "-b", branch, wt, "main")
	stored, err := f.st.UpdateTask(ctx, id, func(cur model.Task) (model.Task, error) {
		cur.Branch, cur.Worktree, cur.Pane, cur.LaunchedAt = branch, wt, "%7", f.start.Add(-2*time.Hour)
		if edit != nil {
			edit(&cur)
		}
		return cur, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return stored
}

// linkedSession seeds session n of task tk, in its worktree.
func (f *restoreFixture) linkedSession(n int, tk model.Task) model.Session {
	f.t.Helper()
	return f.seed(n, func(s *model.Session) {
		s.TaskID, s.Worktree, s.Cwd, s.Repo, s.Branch = tk.ID, tk.Worktree, tk.Worktree, tk.Repo, tk.Branch
	})
}

// setTeams writes team membership (store.SetTeams is the only writer).
func (f *restoreFixture) setTeams(assigned map[string]team.Membership) {
	f.t.Helper()
	if _, err := f.st.SetTeams(context.Background(), assigned); err != nil {
		f.t.Fatal(err)
	}
}

// specByName is the one window opened under name.
func (f *restoreFixture) specByName(name string) tmux.WindowSpec {
	f.t.Helper()
	var found []tmux.WindowSpec
	for _, w := range f.tm.specs {
		if w.Name == name {
			found = append(found, w)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("windows named %s: %+v, want exactly one", name, f.tm.specs)
	}
	return found[0]
}

// noTyping fails the test if anything was typed or a key sent.
func (f *restoreFixture) noTyping() {
	f.t.Helper()
	for _, c := range f.tm.Calls {
		if strings.HasPrefix(c, "SendKeys") {
			f.t.Errorf("restore typed into a pane: %s", c)
		}
	}
}

// restoreRowOf finds the row of session id.
func restoreRowOf(rows []restoreRow, id string) (restoreRow, bool) {
	i := slices.IndexFunc(rows, func(r restoreRow) bool { return r.Session.ID == id })
	if i < 0 {
		return restoreRow{}, false
	}
	return rows[i], true
}

func TestRestoreRefusesInsideAgent(t *testing.T) {
	f := restoreNewFixture(t)
	f.seed(1, nil)
	t.Setenv("CLAUDECODE", "1")
	out, err := f.run(nil, "--yes")
	if err == nil || !strings.Contains(err.Error(), "inside an agent session") {
		t.Fatalf("restore = %v, want a refusal inside an agent session\n%s", err, out)
	}
	if len(f.tm.Calls) != 0 || len(f.tm.specs) != 0 {
		t.Errorf("tmux was called: %q", f.tm.Calls)
	}
	if !f.claimFree() {
		t.Error("the refused restore holds the claim")
	}
}

func TestRestoreNeedsATmuxServer(t *testing.T) {
	db := hookHome(t)
	userTerminalAs(t, true)
	_, err := runRoot(t, "restore", "--yes")
	if err == nil || err.Error() != "no tmux server is running: start tmux, then run curia restore again" {
		t.Fatalf("restore = %v, want the no-server line", err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ok, err := st.ClaimRestore(context.Background(), time.Now(), restoreHold)
	if err != nil || !ok {
		t.Errorf("ClaimRestore after the failed restore = %v, %v; want the claim free (none taken)", ok, err)
	}
}

func TestPlanClassifiesEverySession(t *testing.T) {
	f := restoreNewFixture(t)
	wt := filepath.Join(f.root, "wt-T-1")
	if err := os.Mkdir(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]model.Task{
		"T-1": {ID: "T-1", Repo: "/src/api-server", Worktree: wt, Branch: "curia/T-1-x", Status: model.TaskReady},
		"T-2": {ID: "T-2", Repo: "/src/api-server", Worktree: wt, Status: model.TaskReady, PausedAt: f.now.Add(-time.Hour)},
	}
	inTask := func(id string) func(*model.Session) {
		return func(s *model.Session) { s.TaskID, s.Worktree, s.Cwd = id, wt, wt }
	}
	gone := f.session(8, nil)
	if err := os.Remove(gone.Cwd); err != nil {
		t.Fatal(err)
	}
	noTranscript := f.session(9, nil)
	if err := os.Remove(noTranscript.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	dirTranscript := f.session(10, func(s *model.Session) { s.TranscriptPath = filepath.Join(f.root, "repo10") })
	old := f.session(11, nil)
	f.touch(old.TranscriptPath, 72*time.Hour)
	sessions := []model.Session{
		f.session(1, func(s *model.Session) { s.Pane, s.StartedAt = "%0", f.start.Add(time.Minute) }), // live
		f.session(2, func(s *model.Session) { s.Socket = "/private/tmp/tmux-501/other" }),
		f.session(3, func(s *model.Session) { s.Pane = "" }),
		f.session(4, func(s *model.Session) { s.TeamRole, s.TeamID = model.RoleTeammate, "session-1" }),
		f.session(5, func(s *model.Session) { s.Agent = "codex" }),
		f.session(6, func(s *model.Session) { s.ID = "abc;kill-server" }),
		f.session(7, inTask("T-2")),
		gone, noTranscript, dirTranscript, old,
		f.session(12, inTask("T-1")), // more recent than 13
		f.session(13, inTask("T-1")),
		f.session(14, nil),
		f.session(15, func(s *model.Session) { s.State, s.EndReason = model.StateEnded, "other" }),
		f.session(16, func(s *model.Session) { s.State = model.StateEnded }),
		f.session(17, func(s *model.Session) { s.State, s.EndReason = model.StateEnded, "prompt_input_exit" }),
		// Pane %0 is listed, but on this server it is another pane.
		f.session(18, func(s *model.Session) {
			s.Pane, s.StartedAt, s.Socket = "%0", f.start.Add(time.Minute), "/private/tmp/tmux-501/other"
		}),
		// As 15, but started after this server did: its pane was closed while
		// tmux kept running (I5); and one that did not even end.
		f.session(19, func(s *model.Session) {
			s.State, s.EndReason, s.Pane, s.StartedAt = model.StateEnded, "other", "%9", f.start.Add(10*time.Minute)
		}),
		f.session(20, func(s *model.Session) { s.Pane, s.StartedAt = "%9", f.start.Add(5*time.Minute) }),
	}
	rows := restorePlan(sessions, f.tm.Panes, tasks, f.now, 24*time.Hour)
	want := map[string][2]string{
		restoreID(2):      {"other server", "on another tmux server (other)"},
		restoreID(3):      {"outside tmux", "it ran outside tmux"},
		restoreID(4):      {"teammate", "a teammate: its lead respawns its team"},
		restoreID(5):      {"not Claude Code", restoreCodexDetail},
		"abc;kill-server": {"bad id", "not a Claude Code session id"},
		restoreID(7):      {"paused", "paused; use curia resume T-2"},
		restoreID(8):      {"no directory", "its directory is gone or not a directory"},
		restoreID(9):      {"no transcript", "it has no transcript file"},
		restoreID(10):     {"no transcript", "it has no transcript file"},
		restoreID(11):     {"too old", "last active 3d ago; use --since"},
		restoreID(12):     {"restore", ""},
		restoreID(13):     {"superseded", "another session of T-1 is restored instead"},
		restoreID(14):     {"restore", ""},
		restoreID(15):     {"restore", ""},
		restoreID(16):     {"ended", "ended without a recorded reason (perhaps your own exit)"},
		restoreID(18):     {"other server", "on another tmux server (other)"},
		restoreID(19):     {"closed here", restoreClosedDetail(f, 19)},
		restoreID(20):     {"closed here", restoreClosedDetail(f, 20)},
	}
	restoreWantRows(t, rows, want, restoreID(1), restoreID(17))
	if r, _ := restoreRowOf(rows, restoreID(12)); !r.Linked || r.Task.ID != "T-1" || r.LastActive.IsZero() {
		t.Errorf("row 12 = %+v, want linked to T-1 with a last activity", r)
	}
	if r, _ := restoreRowOf(rows, restoreID(14)); r.Linked || r.Task.ID != "" {
		t.Errorf("row 14 = %+v, want unlinked", r)
	}
}

// restoreWantRows checks that rows are exactly want (session id to action
// and detail) and that the absent sessions are left out.
func restoreWantRows(t *testing.T, rows []restoreRow, want map[string][2]string, absent ...string) {
	t.Helper()
	for id, w := range want {
		r, ok := restoreRowOf(rows, id)
		if !ok {
			t.Errorf("no row for %s, want %q", id, w)
			continue
		}
		if string(r.Action) != w[0] || r.Detail != w[1] {
			t.Errorf("%s: action %q detail %q, want %q %q", id, r.Action, r.Detail, w[0], w[1])
		}
	}
	for _, id := range absent {
		if _, ok := restoreRowOf(rows, id); ok {
			t.Errorf("%s is in the plan, want it left out", id)
		}
	}
	if len(rows) != len(want) {
		t.Errorf("%d rows, want %d", len(rows), len(want))
	}
}

func TestPlanDroppedTaskIsUnlinked(t *testing.T) {
	f := restoreNewFixture(t)
	tasks := map[string]model.Task{"T-1": {ID: "T-1", Status: model.TaskDropped, Worktree: "/nowhere"}}
	rows := restorePlan([]model.Session{f.session(1, func(s *model.Session) { s.TaskID = "T-1" })}, f.tm.Panes, tasks, f.now, 24*time.Hour)
	if len(rows) != 1 || rows[0].Linked || rows[0].Task.ID != "" || rows[0].Action != restoreDo {
		t.Errorf("rows = %+v, want one unlinked restore in its cwd", rows)
	}
}

func TestPlanLinkedSessionOutsideItsWorktree(t *testing.T) {
	f := restoreNewFixture(t)
	wt := filepath.Join(f.root, "wt")
	if err := os.Mkdir(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]model.Task{"T-1": {ID: "T-1", Worktree: wt, Status: model.TaskReady}}
	rows := restorePlan([]model.Session{f.session(1, func(s *model.Session) { s.TaskID = "T-1" })}, f.tm.Panes, tasks, f.now, 24*time.Hour)
	if len(rows) != 1 || rows[0].Action != restoreNoDir || rows[0].Detail != "not in T-1's worktree" {
		t.Errorf("rows = %+v, want no directory: not in T-1's worktree", rows)
	}
}

func TestPlanPaneReusedAfterServerRestartIsNotLive(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.session(1, func(s *model.Session) { s.Pane, s.StartedAt = "%0", f.start.Add(-time.Minute) })
	rows := restorePlan([]model.Session{s}, f.tm.Panes, nil, f.now, 24*time.Hour)
	if len(rows) != 1 || rows[0].Action != restoreDo {
		t.Errorf("rows = %+v, want the session restored: pane %%0 is another pane on the new server", rows)
	}
}

func TestPlanSinceWindow(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.session(1, nil)
	f.touch(s.TranscriptPath, 30*time.Hour)
	if rows := restorePlan([]model.Session{s}, f.tm.Panes, nil, f.now, 24*time.Hour); len(rows) != 1 || rows[0].Action != restoreTooOld {
		t.Errorf("default window: %+v, want too old", rows)
	} else if rows[0].Detail != "last active 1d ago; use --since" {
		t.Errorf("detail %q", rows[0].Detail)
	}
	if rows := restorePlan([]model.Session{s}, f.tm.Panes, nil, f.now, 48*time.Hour); len(rows) != 1 || rows[0].Action != restoreDo {
		t.Errorf("--since 48h: %+v, want restore", rows)
	}
}

func TestPlanCapsAtThirtyTwo(t *testing.T) {
	f := restoreNewFixture(t)
	var sessions []model.Session
	for n := 1; n <= 40; n++ {
		s := f.session(n, nil)
		f.touch(s.TranscriptPath, time.Duration(n)*time.Minute) // 1 is the most recent
		sessions = append(sessions, s)
	}
	rows := restorePlan(sessions, f.tm.Panes, nil, f.now, 24*time.Hour)
	if len(rows) != 40 {
		t.Fatalf("%d rows, want 40", len(rows))
	}
	for n := 1; n <= 40; n++ {
		r, _ := restoreRowOf(rows, restoreID(n))
		want, detail := restoreDo, ""
		if n > restoreMax {
			want, detail = restoreOverLimit, "over the limit; run again"
		}
		if r.Action != want || r.Detail != detail {
			t.Errorf("session %d: %q %q, want %q %q", n, r.Action, r.Detail, want, detail)
		}
	}
}

func TestRestoreDryRunChangesNothing(t *testing.T) {
	f := restoreNewFixture(t)
	a := f.seed(1, nil)
	b := f.seed(2, func(s *model.Session) { s.Agent = "codex" })
	out, err := f.run(nil, "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"SESSION", "WINDOW", "TASK", "REPO@BRANCH", "LAST ACTIVE", "ACTION",
		restoreID(1)[:8], "restore", "skip: " + restoreCodexDetail, restoreCodexByHand(b.ID, b.Cwd),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(f.tm.specs) != 0 {
		t.Errorf("windows opened: %+v", f.tm.specs)
	}
	for _, s := range []model.Session{a, b} {
		if got := f.get(s.ID); got != s {
			t.Errorf("row changed:\n got %+v\nwant %+v", got, s)
		}
	}
	if !f.claimFree() {
		t.Error("the dry run kept the claim")
	}
}

func TestRestoreNeedsConfirmation(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	out, err := f.run(strings.NewReader("y\n"))
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("without a terminal: %v, want a refusal naming --yes\n%s", err, out)
	}
	restoreIsTerminal = func() bool { return true }
	for _, answer := range []string{"n\n", "\n", "yes please\n", ""} {
		out, err = f.run(strings.NewReader(answer))
		if err != nil || !strings.Contains(out, "restore 1 session? [y/N]") || !strings.Contains(out, "nothing restored") {
			t.Errorf("answer %q: %v\n%s", answer, err, out)
		}
	}
	if len(f.tm.specs) != 0 {
		t.Fatalf("windows opened before a yes: %+v", f.tm.specs)
	}
	f.reportAfter(1, s.ID)
	out, err = f.run(strings.NewReader("y\n"))
	if err != nil || len(f.tm.specs) != 1 {
		t.Errorf("answer y: %v, %d windows\n%s", err, len(f.tm.specs), out)
	}
}

func TestRestoreConfirmationOutlastingTheClaim(t *testing.T) {
	f := restoreNewFixture(t)
	f.seed(1, nil)
	restoreIsTerminal = func() bool { return true }
	slow := restoreReaderFunc(func() { f.now = f.now.Add(restoreHold + time.Second) })
	out, err := f.run(slow)
	if err == nil || !strings.Contains(err.Error(), "run curia restore again") {
		t.Fatalf("restore = %v, want a refusal after the claim ran out\n%s", err, out)
	}
	if len(f.tm.specs) != 0 {
		t.Errorf("windows opened: %+v", f.tm.specs)
	}
}

// restoreReaderFunc is a stdin that calls fn on its first read and then
// answers "y".
type restoreReaderFunc func()

func (fn restoreReaderFunc) Read(p []byte) (int, error) {
	if fn != nil {
		fn()
	}
	return copy(p, "y\n"), io.EOF
}

func TestRestoreSecondRunRefused(t *testing.T) {
	f := restoreNewFixture(t)
	f.seed(1, nil)
	other, err := store.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if ok, cerr := other.ClaimRestore(context.Background(), f.now, restoreHold); cerr != nil || !ok {
		t.Fatalf("claim: %v %v", ok, cerr)
	}
	out, err := f.run(nil, "--yes")
	if err == nil || err.Error() != "another curia restore is running" {
		t.Fatalf("restore = %v, want another curia restore is running\n%s", err, out)
	}
	if len(f.tm.specs) != 0 {
		t.Errorf("windows opened: %+v", f.tm.specs)
	}
	if err := other.ReleaseRestore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run(nil, "--dry-run"); err != nil {
		t.Errorf("after the other ended: %v\n%s", err, out)
	}
}

func TestRestoreSecondRunRightAfterRestoresNothing(t *testing.T) {
	f := restoreNewFixture(t)
	tk := f.restoreLinked("T-1", nil)
	linked := f.linkedSession(1, tk)
	plain := f.seed(2, nil)
	f.onSleep = func(n int) {
		if n == 1 {
			f.report(linked.ID, "%100") // the windows open most recent first
			f.report(plain.ID, "%101")
		}
	}
	out, err := f.run(nil, "--yes")
	if err != nil || len(f.tm.specs) != 2 {
		t.Fatalf("first run: %v, %d windows\n%s", err, len(f.tm.specs), out)
	}
	out, err = f.run(nil, "--yes")
	if err != nil || len(f.tm.specs) != 2 {
		t.Errorf("second run: %v, %d windows, want none more\n%s", err, len(f.tm.specs), out)
	}
	if !strings.Contains(out, "nothing to restore") {
		t.Errorf("second run said:\n%s", out)
	}
}

func TestRestoreLinkedWindowStillOpenIsSkipped(t *testing.T) {
	f := restoreNewFixture(t)
	// A previous restore opened %0 for T-1 (after the server started), and
	// its agent has not reported yet: its row still names the old pane.
	tk := f.restoreLinked("T-1", func(tk *model.Task) { tk.Pane, tk.LaunchedAt = "%0", f.start.Add(time.Minute) })
	f.linkedSession(1, tk)
	out, err := f.run(nil, "--yes")
	if err != nil || len(f.tm.specs) != 0 {
		t.Fatalf("restore: %v, %d windows\n%s", err, len(f.tm.specs), out)
	}
	if !strings.Contains(out, "skip: T-1's window %0 is still open") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRestoreLinkedTaskGoesThroughLaunch(t *testing.T) {
	f := restoreNewFixture(t)
	plain := f.restoreLinked("T-1", nil)
	rich := f.restoreLinked("T-2", func(tk *model.Task) { tk.AgentModel, tk.Team = "opus", true })
	a := f.linkedSession(1, plain)
	b := f.linkedSession(2, rich)
	opened := f.now
	f.onSleep = func(n int) {
		if n == 1 {
			f.report(a.ID, "%100")
			f.report(b.ID, "%101")
		}
	}
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	for _, c := range []struct {
		tk      model.Task
		s       model.Session
		pane    string
		command []string
	}{
		{plain, a, "%100", []string{restoreTestBin, "launch", "T-1", "--resume", a.ID}},
		{rich, b, "%101", []string{restoreTestBin, "launch", "T-2", "--resume", b.ID, "--model", "opus", "--team"}},
	} {
		w := f.specByName(c.tk.ID)
		want := tmux.WindowSpec{
			Session: task.SessionName(f.repo), Name: c.tk.ID, Dir: c.tk.Worktree,
			Env: []string{"CURIA_TASK_ID=" + c.tk.ID}, Command: c.command,
		}
		if w.Session != want.Session || w.Dir != want.Dir || !slices.Equal(w.Env, want.Env) || !slices.Equal(w.Command, want.Command) {
			t.Errorf("window:\n got %+v\nwant %+v", w, want)
		}
		got, _, err := f.st.GetTask(context.Background(), c.tk.ID)
		if err != nil || got.Pane != c.pane || !got.LaunchedAt.Equal(opened) {
			t.Errorf("%s: pane %q launched %v (%v), want %s at %v", c.tk.ID, got.Pane, got.LaunchedAt, err, c.pane, opened)
		}
		if !strings.Contains(out, "restored "+c.s.ID[:8]+" in api-server:"+c.tk.ID+" ("+c.pane+")") {
			t.Errorf("output lacks the restored line of %s:\n%s", c.tk.ID, out)
		}
	}
}

func TestRestoreUnlinkedSessionRunsClaudeResume(t *testing.T) {
	f := restoreNewFixture(t)
	repo := filepath.Join(f.root, "web.app")
	s := f.seed(1, func(s *model.Session) { s.TmuxSession, s.Repo = "my.repo:x", repo })
	f.reportAfter(1, s.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	w := f.specByName("web_app")
	if w.Session != "my_repo_x" || w.Dir != s.Cwd || len(w.Env) != 0 ||
		!slices.Equal(w.Command, []string{restoreTestClaude, "--resume", s.ID}) {
		t.Errorf("window = %+v", w)
	}
	if !strings.Contains(out, "restored "+s.ID[:8]+" in my_repo_x:web_app (%100)") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRestoreNamesFallBack(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.session(1, func(s *model.Session) { s.TmuxSession, s.Repo, s.Cwd = "", "", "/" })
	w, err := restoreSpec(restoreRow{Session: s, Action: restoreDo}, restoreTestBin, restoreTestClaude)
	if err != nil || w.Session != "curia" || w.Name != "agent" {
		t.Errorf("spec = %+v, %v; want session curia, window agent", w, err)
	}
	long := strings.Repeat("ab.", 30)
	s = f.session(2, func(s *model.Session) { s.TmuxSession = long })
	if w, err = restoreSpec(restoreRow{Session: s, Action: restoreDo}, restoreTestBin, restoreTestClaude); err != nil ||
		w.Session != strings.Repeat("ab_", 30)[:40] {
		t.Errorf("long name: %q, %v", w.Session, err)
	}
}

func TestRestoreSpecLeavesOutABadModel(t *testing.T) {
	f := restoreNewFixture(t)
	wt := filepath.Join(f.root, "wt")
	if err := os.Mkdir(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	row := restoreRow{
		Session: f.session(1, nil), Linked: true, Action: restoreDo,
		Task: model.Task{ID: "T-1", Repo: "/src/api-server", Worktree: wt, AgentModel: "opus --dangerously-skip-permissions"},
	}
	w, err := restoreSpec(row, restoreTestBin, restoreTestClaude)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{restoreTestBin, "launch", "T-1", "--resume", row.Session.ID}; !slices.Equal(w.Command, want) {
		t.Errorf("command %q, want %q: a stored model that fails task.ValidModel is left out", w.Command, want)
	}
	row.Task.ID = "T-1;x"
	if _, err := restoreSpec(row, restoreTestBin, restoreTestClaude); err == nil {
		t.Error("a bad task id gave a spec")
	}
}

// The patterns of Plan 3's tmux NewWindow (internal/tmux/exec.go).
var (
	restoreWindowNamePat = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	restoreWindowEnvPat  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}=[A-Za-z0-9._/:@-]{0,256}$`)
)

// restoreTmuxText is tmux's rule for one argument: no rune below 0x20 and
// no trailing ";".
func restoreTmuxText(s string) bool {
	return s != "" && !strings.HasSuffix(s, ";") && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 })
}

// restoreWantTmuxSafe checks every field of w against Plan 3's NewWindow
// rules.
func restoreWantTmuxSafe(t *testing.T, w tmux.WindowSpec) {
	t.Helper()
	if !restoreWindowNamePat.MatchString(w.Session) || !restoreWindowNamePat.MatchString(w.Name) {
		t.Errorf("names %q %q", w.Session, w.Name)
	}
	if !filepath.IsAbs(w.Dir) || filepath.Clean(w.Dir) != w.Dir || !restoreTmuxText(w.Dir) {
		t.Errorf("dir %q", w.Dir)
	}
	for _, e := range w.Env {
		if !restoreWindowEnvPat.MatchString(e) {
			t.Errorf("env %q", e)
		}
	}
	for _, a := range w.Command {
		if !restoreTmuxText(a) {
			t.Errorf("command argument %q", a)
		}
	}
	if len(w.Command) < 2 {
		t.Errorf("command %q: one argument would go to a shell", w.Command)
	}
}

func TestRestoreNeverPassesStoredTextToTmux(t *testing.T) {
	f := restoreNewFixture(t)
	hostile := filepath.Join(f.root, "d;$(touch x) `y` e")
	if err := os.Mkdir(hostile, 0o700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(f.root, "line\nbreak")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(f.root, "a-file")
	worktreeWrite(t, file, "x")
	evil := func(cwd string) func(*model.Session) {
		return func(s *model.Session) {
			s.TmuxSession, s.Repo, s.Cwd = "s;kill-server $(x)\nnext", "/x/$(rm -rf ~);\nkill-server;", cwd
		}
	}
	ok := f.seed(1, evil(hostile))
	f.seed(2, evil(broken))
	f.seed(3, evil(file))
	f.seed(4, evil(hostile+";"))
	f.reportAfter(1, ok.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if len(f.tm.specs) != 1 {
		t.Fatalf("windows %+v, want only the one in a real directory tmux can take", f.tm.specs)
	}
	for _, w := range f.tm.specs {
		restoreWantTmuxSafe(t, w)
		if w.Dir != hostile {
			t.Errorf("dir %q, want %q", w.Dir, hostile)
		}
	}
	if got := strings.Count(out, "skip: its directory"); got != 3 {
		t.Errorf("%d directory skips, want 3:\n%s", got, out)
	}
}

func TestRestoreNeverAddsPermissionFlags(t *testing.T) {
	f := restoreNewFixture(t)
	tk := f.restoreLinked("T-1", func(tk *model.Task) { tk.AgentModel, tk.Team = "sonnet", true })
	f.linkedSession(1, tk)
	f.seed(2, func(s *model.Session) { s.Model = "claude-haiku-5-5" })
	out, _ := f.run(nil, "--yes")
	if len(f.tm.specs) != 2 {
		t.Fatalf("windows %+v\n%s", f.tm.specs, out)
	}
	for _, w := range f.tm.specs {
		for _, a := range w.Command {
			if strings.HasPrefix(a, "--dangerously") || strings.HasPrefix(a, "--permission") || strings.HasPrefix(a, "--allow") {
				t.Errorf("window %s passes %q", w.Name, a)
			}
		}
		if w.Name != "T-1" && slices.Contains(w.Command, "--model") {
			t.Errorf("unlinked window %s passes --model: a resume keeps its own model (probe R2)", w.Name)
		}
	}
}

func TestRestoreWaitsForSessionToReport(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	f.reportAfter(3, s.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if !strings.Contains(out, "restored "+s.ID[:8]+" in api-server:repo1 (%100)") {
		t.Errorf("output:\n%s", out)
	}
	if f.sleeps != 3 {
		t.Errorf("%d sleeps, want 3 (it stops waiting once the session reported)", f.sleeps)
	}
	if !strings.Contains(out, "typed nothing") || !strings.Contains(out, "/rate-limit-options") {
		t.Errorf("output lacks the note on dialogs:\n%s", out)
	}
	f.noTyping()
}

func TestRestoreReportsSilentSession(t *testing.T) {
	f := restoreNewFixture(t)
	tk := f.restoreLinked("T-1", nil)
	s := f.linkedSession(1, tk)
	out, err := f.run(nil, "--yes")
	if err == nil {
		t.Fatalf("restore succeeded, want exit 1\n%s", out)
	}
	if !strings.Contains(out, s.ID[:8]+" did not report within 30s; look at api-server:T-1") {
		t.Errorf("output:\n%s", out)
	}
	if f.sleeps != int(restoreWait/restorePoll) {
		t.Errorf("%d sleeps, want %d", f.sleeps, restoreWait/restorePoll)
	}
	if len(f.tm.specs) != 1 {
		t.Errorf("windows %+v, want one: no retry", f.tm.specs)
	}
	f.noTyping()
}

func TestRestoreWindowClosedWithoutStarting(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	f.onSleep = func(n int) {
		if n == 2 {
			f.tm.removePane("%100") // claude --resume printed its error and exited
		}
	}
	out, err := f.run(nil, "--yes")
	if err == nil {
		t.Fatalf("restore succeeded, want exit 1\n%s", out)
	}
	if !strings.Contains(out, s.ID[:8]+": the window closed without the session starting") ||
		!strings.Contains(out, "claude --resume "+s.ID) {
		t.Errorf("output:\n%s", out)
	}
	if f.sleeps != 2 || len(f.tm.specs) != 1 {
		t.Errorf("%d sleeps, %d windows; want it to stop at once, with no retry", f.sleeps, len(f.tm.specs))
	}
}

func TestRestoreNewSessionIDEndsOldRow(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	newID := restoreID(99)
	f.onSleep = func(n int) {
		if n == 2 {
			if err := f.st.UpsertSession(context.Background(), model.Session{
				ID: newID, Agent: "claude", Pane: "%100", State: model.StateIdle, StartedAt: f.now, UpdatedAt: f.now,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if got := f.get(s.ID); got.State != model.StateEnded || got.EndReason != restoreEndReason {
		t.Errorf("old row %+v, want ended (%s)", got, restoreEndReason)
	}
	if !strings.Contains(out, "restored "+s.ID[:8]+" in api-server:repo1 (%100)") || !strings.Contains(out, newID[:8]) {
		t.Errorf("output:\n%s", out)
	}
}

func TestRestoreNewWindowFailureContinues(t *testing.T) {
	f := restoreNewFixture(t)
	first := f.seed(1, nil) // the most recent: opened first
	second := f.seed(2, nil)
	f.tm.failOn = map[int]error{0: errors.New("tmux said no")}
	f.reportAfter(1, second.ID)
	out, err := f.run(nil, "--yes")
	if err == nil {
		t.Fatalf("restore succeeded, want exit 1\n%s", out)
	}
	if !strings.Contains(out, "could not open a window for "+first.ID[:8]+": open the window: tmux said no") {
		t.Errorf("output lacks the failure:\n%s", out)
	}
	if len(f.tm.specs) != 2 || !strings.Contains(out, "restored "+second.ID[:8]) {
		t.Errorf("the second was not restored: %d windows\n%s", len(f.tm.specs), out)
	}
}

func TestRestoreTeamLead(t *testing.T) {
	f := restoreNewFixture(t)
	lead := f.seed(1, nil)
	mate1 := f.seed(2, func(s *model.Session) { s.Pane = "%8" })
	mate2 := f.seed(3, func(s *model.Session) { s.Pane = "%9" })
	f.setTeams(map[string]team.Membership{
		lead.ID:  {TeamID: "session-00000001", Role: model.RoleLead, Member: "team-lead"},
		mate1.ID: {TeamID: "session-00000001", Role: model.RoleTeammate, Member: "alpha"},
		mate2.ID: {TeamID: "session-00000001", Role: model.RoleTeammate, Member: "beta"},
	})
	// The server's loss ended one teammate (SessionEnd "other"); the other was killed outright.
	if err := f.st.UpdateSession(context.Background(), mate1.ID, func(s model.Session, _ bool) model.Session {
		s.State, s.EndReason = model.StateEnded, "other"
		return s
	}); err != nil {
		t.Fatal(err)
	}
	f.reportAfter(1, lead.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if len(f.tm.specs) != 1 || !slices.Contains(f.tm.specs[0].Command, lead.ID) {
		t.Fatalf("windows %+v, want only the lead's", f.tm.specs)
	}
	for _, id := range []string{mate1.ID, mate2.ID} {
		if got := f.get(id); got.State != model.StateEnded || got.EndReason != restoreEndReason {
			t.Errorf("teammate %s = %s (%q), want ended (%s)", id[:8], got.State, got.EndReason, restoreEndReason)
		}
	}
	restoreWantNotice(t, f.st, lead.ID, out)
	if !strings.Contains(out, "2 teammates") {
		t.Errorf("output lacks the teammates line:\n%s", out)
	}
	f.noTyping()
	// The next run offers the teammates no more.
	out, _ = f.run(nil, "--dry-run")
	if strings.Contains(out, mate1.ID[:8]) || strings.Contains(out, mate2.ID[:8]) {
		t.Errorf("a later plan still lists a teammate:\n%s", out)
	}
}

// restoreWantNotice checks the one stored message is the team notice for
// lead and that deliver.Message's outcome was printed.
func restoreWantNotice(t *testing.T, st *store.Store, lead, out string) {
	t.Helper()
	msgs, err := st.ListMessages(context.Background(), store.MessageFilter{})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages %+v (%v), want one", msgs, err)
	}
	m := msgs[0]
	if m.Text != restoreLeadText || m.TargetSession != lead || m.Kind != model.KindInstruction ||
		m.Sender != model.SenderUser || m.RoutedBy != model.RoutedAddress || m.Status != model.MsgQueued {
		t.Errorf("message %+v", m)
	}
	if !strings.Contains(out, "M-1 queued") || !strings.Contains(out, "check_messages") {
		t.Errorf("output lacks deliver.Message's outcome:\n%s", out)
	}
}

func TestRestoreLeavesLiveTeammateAlone(t *testing.T) {
	f := restoreNewFixture(t)
	lead := f.seed(1, nil)
	mate := f.seed(2, func(s *model.Session) { s.Pane, s.StartedAt = "%0", f.start.Add(time.Minute) })
	f.setTeams(map[string]team.Membership{
		lead.ID: {TeamID: "session-00000001", Role: model.RoleLead},
		mate.ID: {TeamID: "session-00000001", Role: model.RoleTeammate, Member: "alpha"},
	})
	before := f.get(mate.ID)
	f.reportAfter(1, lead.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if got := f.get(mate.ID); got != before {
		t.Errorf("live teammate changed:\n got %+v\nwant %+v", got, before)
	}
}

func TestRestoreTeammateBackDuringWaitIsNotEnded(t *testing.T) {
	f := restoreNewFixture(t)
	lead := f.seed(1, nil)
	mate := f.seed(2, func(s *model.Session) { s.Pane = "%8" })
	f.setTeams(map[string]team.Membership{
		lead.ID: {TeamID: "session-00000001", Role: model.RoleLead},
		mate.ID: {TeamID: "session-00000001", Role: model.RoleTeammate, Member: "alpha"},
	})
	// Planned as a lost teammate, it is live in %0 by the time the lead is back.
	f.onSleep = func(n int) {
		if n == 1 {
			f.report(mate.ID, "%0")
			f.report(lead.ID, "%100")
		}
	}
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if got := f.get(mate.ID); got.State == model.StateEnded {
		t.Errorf("a teammate live in a listed pane was ended: %+v", got)
	}
	if strings.Contains(out, "teammates did not survive") {
		t.Errorf("output counts a live teammate as lost:\n%s", out)
	}
}

func TestRestoreEndsSupersededRowsOfItsTask(t *testing.T) {
	f := restoreNewFixture(t)
	tk := f.restoreLinked("T-1", nil)
	newer := f.linkedSession(1, tk)
	older := f.linkedSession(2, tk)
	f.reportAfter(1, newer.ID)
	out, err := f.run(nil, "--yes")
	if err != nil || len(f.tm.specs) != 1 {
		t.Fatalf("restore: %v, %d windows\n%s", err, len(f.tm.specs), out)
	}
	if got := f.get(older.ID); got.State != model.StateEnded || got.EndReason != restoreEndReason {
		t.Errorf("older row = %+v, want ended", got)
	}
}

func TestRestorePrefersTheTaskPane(t *testing.T) {
	f := restoreNewFixture(t)
	wt := filepath.Join(f.root, "wt")
	if err := os.Mkdir(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	tasks := map[string]model.Task{"T-1": {ID: "T-1", Worktree: wt, Pane: "%7", Status: model.TaskReady}}
	in := func(pane string) func(*model.Session) {
		return func(s *model.Session) { s.TaskID, s.Worktree, s.Cwd, s.Pane = "T-1", wt, wt, pane }
	}
	// A teammate whose team columns a refresh cleared (it went stale) was
	// active after its lead, which ran in the task's own pane.
	mate, lead := f.session(1, in("%8")), f.session(2, in("%7"))
	rows := restorePlan([]model.Session{mate, lead}, f.tm.Panes, tasks, f.now, 24*time.Hour)
	if r, _ := restoreRowOf(rows, lead.ID); r.Action != restoreDo {
		t.Errorf("lead: %+v, want restore", r)
	}
	if r, _ := restoreRowOf(rows, mate.ID); r.Action != restoreSuperseded {
		t.Errorf("other pane: %+v, want superseded", r)
	}
}

func TestRestoreWithoutClaude(t *testing.T) {
	f := restoreNewFixture(t)
	f.claudeErr = exec.ErrNotFound
	tk := f.restoreLinked("T-1", nil)
	linked := f.linkedSession(1, tk)
	plain := f.seed(2, nil)
	f.reportAfter(1, linked.ID)
	out, err := f.run(nil, "--yes")
	if err == nil {
		t.Fatalf("restore succeeded, want exit 1\n%s", out)
	}
	if !strings.Contains(out, "could not open a window for "+plain.ID[:8]+": claude is not on PATH") {
		t.Errorf("output:\n%s", out)
	}
	if len(f.tm.specs) != 1 || f.tm.specs[0].Name != "T-1" || !strings.Contains(out, "restored "+linked.ID[:8]) {
		t.Errorf("windows %+v, want the linked one through curia launch\n%s", f.tm.specs, out)
	}
}

func TestRestoreOtherServerUntouched(t *testing.T) {
	f := restoreNewFixture(t)
	other := f.seed(1, func(s *model.Session) { s.Socket = "/private/tmp/tmux-501/work" })
	good := f.seed(2, nil)
	f.reportAfter(1, good.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if !strings.Contains(out, "skip: on another tmux server (work)") {
		t.Errorf("output:\n%s", out)
	}
	if got := f.get(other.ID); got != other {
		t.Errorf("row changed:\n got %+v\nwant %+v", got, other)
	}
	if len(f.tm.specs) != 1 {
		t.Errorf("windows %+v", f.tm.specs)
	}
}

func TestRestoreEndedSessions(t *testing.T) {
	f := restoreNewFixture(t)
	crashed := f.seed(1, func(s *model.Session) { s.State, s.EndReason = model.StateEnded, "other" })
	exited := f.seed(2, func(s *model.Session) { s.State, s.EndReason = model.StateEnded, "prompt_input_exit" })
	unknown := f.seed(3, func(s *model.Session) { s.State = model.StateEnded })
	out, err := f.run(nil, "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	lines := strings.Split(out, "\n")
	line := func(id string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, id[:8]) {
				return l
			}
		}
		return ""
	}
	if l := line(crashed.ID); !strings.HasSuffix(strings.TrimSpace(l), "restore") {
		t.Errorf("ended by a tmux crash: %q, want restore", l)
	}
	if l := line(exited.ID); l != "" {
		t.Errorf("your own exit is offered: %q", l)
	}
	if l := line(unknown.ID); !strings.Contains(l, "skip: ended without a recorded reason") {
		t.Errorf("ended with no reason: %q, want a skip", l)
	}
}

func TestRestoreDirectoryGoneBeforeWindow(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	restoreIsTerminal = func() bool { return true }
	out, err := f.run(restoreReaderFunc(func() {
		if err := os.Remove(s.Cwd); err != nil {
			t.Fatal(err)
		}
	}))
	if err == nil {
		t.Fatalf("restore succeeded, want exit 1\n%s", out)
	}
	if len(f.tm.specs) != 0 {
		t.Fatalf("a window was opened for a missing directory: %+v (tmux would start it in the home directory)", f.tm.specs)
	}
	if !strings.Contains(out, "could not open a window for "+s.ID[:8]+": "+s.Cwd+" is not a directory") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRestoreSessionBackDuringConfirmation(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	restoreIsTerminal = func() bool { return true }
	out, err := f.run(restoreReaderFunc(func() { f.report(s.ID, "%0") })) // the user resumed it by hand meanwhile
	if err == nil {
		t.Fatalf("restore succeeded, want exit 1\n%s", out)
	}
	if len(f.tm.specs) != 0 {
		t.Fatalf("a second window was opened for one session id: %+v", f.tm.specs)
	}
	if !strings.Contains(out, "could not open a window for "+s.ID[:8]+": it changed since the plan") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRestoreOutputIsSanitised(t *testing.T) {
	f := restoreNewFixture(t)
	f.seed(1, func(s *model.Session) {
		s.Repo, s.Branch, s.TmuxSession = "/x/\x1b[31mevil", "\x1b]0;title\x07main", "\x1b[2Jses"
	})
	f.seed(2, func(s *model.Session) { s.Agent, s.Branch = "\x1b[31mcodex", "\x1b[1mb" })
	out, _ := f.run(nil, "--dry-run")
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
		t.Errorf("escape on stdout: %q", out)
	}
}

// restoreRealSocket is the private tmux server of TestRestoreInRealTmux.
var restoreRealSocket = fmt.Sprintf("curia-test-%d-restore", os.Getpid())

func TestRestoreInRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := restoreNewFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), restoreStubName)
	if err = os.Symlink(exe, stub); err != nil {
		t.Fatal(err)
	}
	restoreLookPath = func(string) (string, error) { return stub, nil }
	raw := func(args ...string) (string, error) {
		out, rerr := exec.CommandContext(ctx, "tmux", append([]string{"-L", restoreRealSocket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), rerr
	}
	t.Cleanup(func() { spawnKillServer(restoreRealSocket) })
	_, _ = raw("kill-server")
	if out, nerr := raw("new-session", "-d", "-s", "keep"); nerr != nil {
		t.Fatalf("new-session: %v: %s", nerr, out)
	}
	newTmuxClient = func() tmux.Client { return tmux.NewExec(restoreRealSocket) }
	s := f.seed(1, func(s *model.Session) {
		s.TmuxSession, s.Socket, s.StartedAt = "old.sess", "", time.Now().Add(-time.Hour)
	})
	record := filepath.Join(filepath.Dir(f.db), restoreStubRecord) // hookHome's HOME, which the server passes on
	var run restoreStubRun
	f.onSleep = func(int) {
		time.Sleep(50 * time.Millisecond) // real time for the window's process to start
		b, rerr := os.ReadFile(filepath.Clean(record))
		if rerr != nil || run.Pane != "" || json.Unmarshal(b, &run) != nil {
			return
		}
		f.report(s.ID, run.Pane) // the stub's SessionStart, as the hook would record it
	}
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if !slices.Equal(run.Args, []string{"--resume", s.ID}) || run.Cwd != s.Cwd {
		t.Errorf("the window ran %q in %q, want [--resume %s] in %q", run.Args, run.Cwd, s.ID, s.Cwd)
	}
	if where, err := raw("display-message", "-p", "-t", run.Pane, "#{session_name}:#{window_name}"); err != nil || where != "old_sess:repo1" {
		t.Errorf("pane %s is in %q (%v), want old_sess:repo1", run.Pane, where, err)
	}
	if !strings.Contains(out, "restored "+s.ID[:8]+" in old_sess:repo1 ("+run.Pane+")") {
		t.Errorf("output:\n%s", out)
	}
}

// restoreClosedDetail is the skip detail of session n, in repo<n>, whose
// pane was closed while tmux kept running.
func restoreClosedDetail(f *restoreFixture, n int) string {
	return "its pane was closed while tmux kept running; reopen it with claude --resume " + restoreID(n) + " in " +
		filepath.Join(f.root, fmt.Sprintf("repo%d", n)) + ", or run curia restore --include-closed"
}

// TestPlanSkipsPaneClosedOnLiveServer: a session that started at or after
// the listed server's start and is not on another server lost its pane on
// that server, which is still running: it was closed, not crashed, and is
// skipped (Ruling P4-R7), ended with "other" or not ended at all. One that
// started before the server, on another server, or at an unknown time is
// classified as before; with --include-closed the closed ones are offered.
func TestPlanSkipsPaneClosedOnLiveServer(t *testing.T) {
	f := restoreNewFixture(t)
	after := f.start.Add(10 * time.Minute)
	sessions := []model.Session{
		f.session(1, func(s *model.Session) {
			s.State, s.EndReason, s.Pane, s.StartedAt = model.StateEnded, "other", "%9", after
		}),
		f.session(2, func(s *model.Session) { s.Pane, s.StartedAt = "%8", after }),
		f.session(3, func(s *model.Session) {
			s.State, s.EndReason, s.StartedAt = model.StateEnded, "other", f.start.Add(-time.Minute)
		}),
		f.session(4, func(s *model.Session) { s.Pane, s.StartedAt, s.Socket = "%8", after, "/private/tmp/tmux-501/other" }),
		f.session(5, func(s *model.Session) { s.StartedAt = f.start }), // at the server's start: started on it
		f.session(6, func(s *model.Session) { s.StartedAt = time.Time{} }),
	}
	rows := restorePlan(sessions, f.tm.Panes, nil, f.now, 24*time.Hour)
	restoreWantRows(t, rows, map[string][2]string{
		restoreID(1): {"closed here", restoreClosedDetail(f, 1)},
		restoreID(2): {"closed here", restoreClosedDetail(f, 2)},
		restoreID(3): {"restore", ""},
		restoreID(4): {"other server", "on another tmux server (other)"},
		restoreID(5): {"closed here", restoreClosedDetail(f, 5)},
		restoreID(6): {"restore", ""},
	})
	restoreIncludeClosed = true
	rows = restorePlan(sessions, f.tm.Panes, nil, f.now, 24*time.Hour)
	for _, n := range []int{1, 2, 5} {
		if r, _ := restoreRowOf(rows, restoreID(n)); r.Action != restoreDo {
			t.Errorf("--include-closed: %d is %q, want restore", n, r.Action)
		}
	}
}

// TestRestoreIncludeClosed: curia restore skips, and shows, a session closed
// on the running server, with the command to reopen it; with
// --include-closed it opens its window.
func TestRestoreIncludeClosed(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, func(s *model.Session) {
		s.State, s.EndReason, s.StartedAt = model.StateEnded, "other", f.start.Add(time.Minute)
	})
	out, err := f.run(nil, "--yes")
	if err != nil || len(f.tm.specs) != 0 || !strings.Contains(out, "nothing to restore") ||
		!strings.Contains(out, "skip: its pane was closed while tmux kept running; reopen it with claude --resume "+s.ID) {
		t.Fatalf("without --include-closed: %v, %d windows\n%s", err, len(f.tm.specs), out)
	}
	f.reportAfter(1, s.ID)
	out, err = f.run(nil, "--yes", "--include-closed")
	if err != nil || len(f.tm.specs) != 1 || !slices.Contains(f.tm.specs[0].Command, s.ID) {
		t.Fatalf("--include-closed: %v, windows %+v\n%s", err, f.tm.specs, out)
	}
}

// TestRestoreSecondRunOpensNoSecondWindow: a session with no task whose
// window sits at a dialog (it has not reported) is not offered again by a
// second restore: its row was ended (curia_restore) in the update that
// re-checked it before its window opened.
func TestRestoreSecondRunOpensNoSecondWindow(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	out, err := f.run(nil, "--yes")
	if err == nil || len(f.tm.specs) != 1 || !strings.Contains(out, "did not report within") {
		t.Fatalf("first run: %v, %d windows\n%s", err, len(f.tm.specs), out)
	}
	if got := f.get(s.ID); got.State != model.StateEnded || got.EndReason != restoreEndReason {
		t.Errorf("row after the first run = %s (%q), want ended (%s)", got.State, got.EndReason, restoreEndReason)
	}
	out, err = f.run(nil, "--yes")
	if err != nil || len(f.tm.specs) != 1 || !strings.Contains(out, "nothing to restore") {
		t.Errorf("second run: %v, %d windows, want no second window\n%s", err, len(f.tm.specs), out)
	}
	// The resumed session's SessionStart, when it comes, revives the row.
	f.report(s.ID, "%100")
	if got := f.get(s.ID); got.State != model.StateIdle || got.EndReason != "" {
		t.Errorf("after its SessionStart: %s (%q)", got.State, got.EndReason)
	}
}

// TestRestoreWindowFailurePutsRowBack: when its window cannot be opened, a
// row restore ended to open it is as it was, and offered again next time.
func TestRestoreWindowFailurePutsRowBack(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	f.tm.failOn = map[int]error{0: errors.New("tmux said no")}
	if out, err := f.run(nil, "--yes"); err == nil {
		t.Fatalf("restore succeeded\n%s", out)
	}
	// As planned: reconcile had marked it stale (its pane was gone).
	if got := f.get(s.ID); got.State != model.StateStale || got.EndReason != "" {
		t.Errorf("row = %s (%q), want it as planned: stale, no end reason", got.State, got.EndReason)
	}
	f.tm.failOn = nil
	out, err := f.run(nil, "--dry-run")
	if err != nil || !strings.Contains(out, lifecycle.SpawnShortID(s.ID)) || !strings.Contains(out, "restore\n") {
		t.Errorf("not offered again: %v\n%s", err, out)
	}
}

// TestRestoreClaimCoversExecute: the claim is held from the confirmation
// through the windows and the wait (10 minutes), not only the 2 minutes of
// the question, and is given up at the end.
func TestRestoreClaimCoversExecute(t *testing.T) {
	f := restoreNewFixture(t)
	s := f.seed(1, nil)
	var during []bool
	f.onSleep = func(n int) {
		if n == 1 {
			f.now = f.now.Add(restoreHold + time.Minute) // a long wait for the windows
			during = append(during, f.claimFree())
			f.now = f.now.Add(5 * time.Minute)
			during = append(during, f.claimFree())
			f.report(s.ID, "%100")
		}
	}
	if out, err := f.run(nil, "--yes"); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if !slices.Equal(during, []bool{false, false}) {
		t.Errorf("claim free during the run, 3 and 8 minutes in: %v, want held both times", during)
	}
	if !f.claimFree() {
		t.Error("the claim was not given up")
	}
}

// TestRestoreStaleTeamAfterPowerCut: after a power cut every row is stale
// and the team's config is gone with its lead; a refresh (SetTeams with
// nothing assigned) keeps the stale rows' membership (M8), so restore resumes
// the lead and ends its teammates' rows rather than offering each.
func TestRestoreStaleTeamAfterPowerCut(t *testing.T) {
	f := restoreNewFixture(t)
	lead := f.seed(1, nil)
	mate1 := f.seed(2, func(s *model.Session) { s.Pane = "%8" })
	mate2 := f.seed(3, func(s *model.Session) { s.Pane = "%9" })
	f.setTeams(map[string]team.Membership{
		lead.ID:  {TeamID: "session-00000001", Role: model.RoleLead, Member: "team-lead"},
		mate1.ID: {TeamID: "session-00000001", Role: model.RoleTeammate, Member: "alpha"},
		mate2.ID: {TeamID: "session-00000001", Role: model.RoleTeammate, Member: "beta"},
	})
	for _, id := range []string{lead.ID, mate1.ID, mate2.ID} {
		if err := f.st.UpdateSession(context.Background(), id, func(s model.Session, _ bool) model.Session {
			s.State = model.StateStale
			return s
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.setTeams(map[string]team.Membership{}) // the dashboard's refresh: no team config left
	f.reportAfter(1, lead.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if len(f.tm.specs) != 1 || !slices.Contains(f.tm.specs[0].Command, lead.ID) {
		t.Fatalf("windows %+v, want only the lead's\n%s", f.tm.specs, out)
	}
	for _, id := range []string{mate1.ID, mate2.ID} {
		if got := f.get(id); got.State != model.StateEnded || got.EndReason != restoreEndReason {
			t.Errorf("teammate %s = %s (%q), want ended (%s)", id[:8], got.State, got.EndReason, restoreEndReason)
		}
	}
}

// TestRestoreKeepsReviewMarker: a task's session that was unseen with a
// review waiting before the crash comes back unseen, so its window gets the
// blue review-ready marker again, though its SessionStart marked it seen;
// one that had been looked at stays seen.
func TestRestoreKeepsReviewMarker(t *testing.T) {
	for _, unseen := range []bool{true, false} {
		f := restoreNewFixture(t)
		tk := f.restoreLinked("T-1", nil)
		s := f.linkedSession(1, tk)
		if !unseen {
			if err := f.st.UpdateSession(context.Background(), s.ID, func(cur model.Session, _ bool) model.Session {
				cur.SeenAt = cur.UpdatedAt
				return cur
			}); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := f.st.CompleteTask(context.Background(), model.Review{
			TaskID: "T-1", SessionID: s.ID, Summary: "Done.",
			Tests: model.TestsPassed, CreatedAt: f.now,
		}); err != nil {
			t.Fatal(err)
		}
		f.onSleep = func(n int) {
			if n == 1 { // the resumed session's SessionStart: live in the new window, and seen
				if err := f.st.UpdateSession(context.Background(), s.ID, func(cur model.Session, _ bool) model.Session {
					cur.Pane, cur.TmuxWindow, cur.State, cur.EndReason = "%100", "@100", model.StateIdle, ""
					cur.StartedAt, cur.UpdatedAt, cur.SeenAt = f.now, f.now, f.now
					return cur
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		f.tm.Calls = nil
		if out, err := f.run(nil, "--yes"); err != nil {
			t.Fatalf("restore: %v\n%s", err, out)
		}
		blue := slices.Contains(f.tm.Calls, "SetWindowOption @100 @curia_marker blue")
		if got := f.get(s.ID); got.SeenAt.IsZero() != unseen || blue != unseen {
			t.Errorf("unseen before %v: seen at %v, blue marker %v (calls %q)", unseen, got.SeenAt, blue, f.tm.Calls)
		}
	}
}

// restoreCodexDetail is the plan's skip detail of a Codex session (Ruling
// P5-R8: Codex resume is unverified until probe X1).
const restoreCodexDetail = "a Codex session: restore does not resume Codex sessions yet (see below)"

// restoreCodexByHand is the line restore prints for a skipped Codex session.
func restoreCodexByHand(id, dir string) string {
	return "skipped " + id[:8] + ": a Codex session, not restored until Codex resume is verified; " +
		"to resume it by hand, run in " + dir + ": codex resume " + id
}

// TestRestoreSkipsCodexSessions: restore never resumes a Codex session with
// claude (Ruling P5-R8). Until Codex resume is verified it skips every
// Codex session, linked to a task or not, closed here or lost with tmux,
// says once per session what the user can run by hand, and does not count
// it as restored.
func TestRestoreSkipsCodexSessions(t *testing.T) {
	f := restoreNewFixture(t)
	codex := func(s *model.Session) { s.Agent = model.AgentCodex }
	unlinked := f.seed(1, codex)
	tk := f.restoreLinked("T-1", func(tk *model.Task) { tk.Agent, tk.AgentModel = model.AgentCodex, "gpt-5" })
	linked := f.seed(2, func(s *model.Session) {
		s.Agent, s.TaskID, s.Worktree, s.Cwd, s.Repo, s.Branch = model.AgentCodex, tk.ID, tk.Worktree, tk.Worktree, tk.Repo, tk.Branch
	})
	closed := f.seed(3, func(s *model.Session) {
		s.Agent, s.State, s.EndReason, s.StartedAt = model.AgentCodex, model.StateEnded, "other", f.start.Add(time.Minute)
	})
	plain := f.seed(4, nil)
	f.reportAfter(1, plain.ID)
	out, err := f.run(nil, "--yes")
	if err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if len(f.tm.specs) != 1 || !slices.Equal(f.tm.specs[0].Command, []string{restoreTestClaude, "--resume", plain.ID}) {
		t.Fatalf("windows %+v, want only the Claude Code session's", f.tm.specs)
	}
	for _, s := range []model.Session{unlinked, linked, closed} {
		dir := s.Cwd
		if line := restoreCodexByHand(s.ID, dir); strings.Count(out, line) != 1 {
			t.Errorf("output lacks %q once:\n%s", line, out)
		}
		if strings.Contains(out, "restored "+s.ID[:8]) || strings.Contains(out, "claude --resume "+s.ID) {
			t.Errorf("session %s counted as restored or offered to claude:\n%s", s.ID[:8], out)
		}
		if got := f.get(s.ID); got.EndReason == restoreEndReason {
			t.Errorf("skipped row %s was ended by restore: %+v", s.ID[:8], got)
		}
	}
	if got, _, _ := f.st.GetTask(context.Background(), tk.ID); got.Pane != tk.Pane || !got.LaunchedAt.Equal(tk.LaunchedAt) || got.Agent != model.AgentCodex {
		t.Errorf("T-1 changed: %+v", got)
	}
	if strings.Count("\n"+out, "\nrestored ") != 1 {
		t.Errorf("want one restored line:\n%s", out)
	}
}

// TestRestoreLinkedClaudeSessionOfCodexTask: a Claude Code session of a task
// whose stored agent is now codex is not resumed through curia launch,
// which would start codex on a Claude Code session id.
func TestRestoreLinkedClaudeSessionOfCodexTask(t *testing.T) {
	f := restoreNewFixture(t)
	tk := f.restoreLinked("T-1", func(tk *model.Task) { tk.Agent, tk.AgentModel = model.AgentCodex, "gpt-5" })
	s := f.linkedSession(1, tk)
	out, _ := f.run(nil, "--yes")
	if len(f.tm.specs) != 0 {
		t.Fatalf("windows %+v, want none\n%s", f.tm.specs, out)
	}
	if !strings.Contains(out, "skip: T-1 now runs codex, not this session's Claude Code") || strings.Contains(out, "restored "+s.ID[:8]) {
		t.Errorf("output:\n%s", out)
	}
}

// TestRestoreSpecRefusesOtherAgents: restoreSpec itself never builds a
// claude window for a session that is not Claude Code's, nor a launch of a
// task whose stored agent is not the session's.
func TestRestoreSpecRefusesOtherAgents(t *testing.T) {
	f := restoreNewFixture(t)
	wt := filepath.Join(f.root, "wt")
	if err := os.Mkdir(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	codex := f.session(1, func(s *model.Session) { s.Agent = model.AgentCodex })
	if w, err := restoreSpec(restoreRow{Session: codex, Action: restoreDo}, restoreTestBin, restoreTestClaude); err == nil {
		t.Errorf("unlinked Codex session: spec %+v, want an error", w)
	}
	for _, c := range []struct {
		session model.Session
		agent   string
	}{
		{codex, model.AgentCodex},
		{f.session(2, nil), model.AgentCodex},
		{codex, model.AgentClaude},
	} {
		row := restoreRow{
			Session: c.session, Linked: true, Action: restoreDo,
			Task: model.Task{ID: "T-1", Repo: "/src/api-server", Worktree: wt, Agent: c.agent, AgentModel: "gpt-5"},
		}
		if w, err := restoreSpec(row, restoreTestBin, restoreTestClaude); err == nil {
			t.Errorf("session agent %s, task agent %s: spec %+v, want an error", c.session.Agent, c.agent, w)
		}
	}
}

// A session in the same-numbered pane of another tmux server is not the
// window's agent coming back.
func TestRestoreCheckBackIgnoresOtherServer(t *testing.T) {
	opened := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	l := &restoreLaunch{pane: tmux.Pane{ID: "%100", Socket: "/tmp/tmux-501/first"}, opened: opened}
	other := model.Session{ID: "s1", Pane: "%100", Socket: "/tmp/tmux-501/second", StartedAt: opened.Add(time.Second)}
	restoreCheckBack(l, []model.Session{other}, nil, false)
	if l.state != restorePending {
		t.Errorf("state = %v, want pending", l.state)
	}
	other.Socket = "/tmp/tmux-501/first"
	restoreCheckBack(l, []model.Session{other}, nil, false)
	if l.state != restoreBack {
		t.Errorf("same server: state = %v, want back", l.state)
	}
}

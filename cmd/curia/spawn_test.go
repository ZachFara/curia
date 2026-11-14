package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
)

// spawnTestBin is the curia binary path spawn tests give the window.
const spawnTestBin = "/opt/curia/bin/curia"

// spawnFixture is one spawn test's world: a git repository named
// api-server, a store, a fake tmux, a fake clock that spawnSleep advances,
// and spawnBin fixed at spawnTestBin.
type spawnFixture struct {
	t       *testing.T
	db      string
	st      *store.Store
	cfg     config.Config
	fake    *tmux.Fake
	repo    string
	start   time.Time
	now     time.Time
	sleeps  int
	onSleep func(n int) // called after the n-th fake sleep
}

func spawnNewFixture(t *testing.T) *spawnFixture {
	t.Helper()
	f := &spawnFixture{t: t, db: hookHome(t), fake: useFakeTmux(t)}
	f.start = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	f.now = f.start
	f.repo = worktreeRepo(t, "api-server")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	st, err := store.Open(f.db)
	if err != nil {
		t.Fatal(err)
	}
	f.st = st
	prevClock, prevSleep, prevBin, prevLimit := clock, spawnSleep, spawnBin, spawnSetupLimit
	clock = func() time.Time { return f.now }
	spawnSleep = func(d time.Duration) {
		f.now = f.now.Add(d)
		f.sleeps++
		if f.onSleep != nil {
			f.onSleep(f.sleeps)
		}
	}
	spawnBin = func() (string, error) { return spawnTestBin, nil }
	t.Cleanup(func() {
		clock, spawnSleep, spawnBin, spawnSetupLimit = prevClock, prevSleep, prevBin, prevLimit
		_ = st.Close()
	})
	return f
}

// task creates a backlog task on the fixture's repository, based on main.
func (f *spawnFixture) task(title, body string) model.Task {
	f.t.Helper()
	tk, err := f.st.CreateTask(context.Background(), task.Normalize(model.Task{
		Title: title, Body: body, Repo: f.repo, BaseBranch: "main", Status: model.TaskBacklog, CreatedAt: f.now,
	}))
	if err != nil {
		f.t.Fatal(err)
	}
	return tk
}

// update changes task id in the store.
func (f *spawnFixture) update(id string, fn func(*model.Task)) {
	f.t.Helper()
	if _, err := f.st.UpdateTask(context.Background(), id, func(tk model.Task) (model.Task, error) {
		fn(&tk)
		return tk, nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *spawnFixture) get(id string) model.Task {
	f.t.Helper()
	tk, found, err := f.st.GetTask(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("GetTask(%s): found %v, err %v", id, found, err)
	}
	return tk
}

// spawn runs spawnTask and returns its result, what it printed and its error.
func (f *spawnFixture) spawn(id string, o spawnOptions) (spawnResult, string, error) {
	f.t.Helper()
	var out bytes.Buffer
	o.Out = &out
	r, err := spawnTask(context.Background(), f.st, f.fake, f.cfg, id, o)
	return r, out.String(), err
}

// windows returns the fake's NewWindow calls.
func (f *spawnFixture) windows() []string {
	return slices.DeleteFunc(slices.Clone(f.fake.Calls), func(c string) bool { return !strings.HasPrefix(c, "NewWindow") })
}

func (f *spawnFixture) worktree(id string) string { return task.WorktreePath(f.repo, id) }

// commitSetup commits .curia/setup.sh with content to the repository,
// executable when exe is set (through the index, so no test writes a file
// with an execute bit: the worktree checkout does). It is not approved.
func (f *spawnFixture) commitSetup(content string, exe bool) {
	f.t.Helper()
	worktreeWrite(f.t, filepath.Join(f.repo, ".curia", "setup.sh"), content)
	gitRun(f.t, f.repo, "add", "-A")
	if exe {
		gitRun(f.t, f.repo, "update-index", "--chmod=+x", ".curia/setup.sh")
	}
	gitRun(f.t, f.repo, "commit", "-q", "-m", "setup")
}

// setupScript commits .curia/setup.sh (see commitSetup) and approves its
// content for the repository.
func (f *spawnFixture) setupScript(content string, exe bool) {
	f.t.Helper()
	f.commitSetup(content, exe)
	f.approve(content)
}

// approve records content as the approved setup script of the repository.
func (f *spawnFixture) approve(content string) {
	f.t.Helper()
	sum := sha256.Sum256([]byte(content))
	if err := f.st.ApproveSetup(context.Background(), f.repo, hex.EncodeToString(sum[:])); err != nil {
		f.t.Fatal(err)
	}
}

// setupLog is the setup log of task T-1.
func (f *spawnFixture) setupLog() string {
	return filepath.Join(filepath.Dir(f.cfg.LogPath), "setup", "T-1.log")
}

// wantNothingStarted checks that a refused spawn of id opened no window,
// made no worktree and holds no launch claim.
func (f *spawnFixture) wantNothingStarted(id string) {
	f.t.Helper()
	if w := f.windows(); len(w) != 0 {
		f.t.Errorf("windows opened: %q", w)
	}
	if worktreeExists(f.worktree(id)) {
		f.t.Errorf("worktree %s was created", f.worktree(id))
	}
	if tk, found, err := f.st.GetTask(context.Background(), id); err == nil && found && !tk.LaunchedAt.IsZero() {
		f.t.Errorf("%s launched_at = %v, want the claim released", id, tk.LaunchedAt)
	}
}

// spawnRun runs `curia spawn args...` in-process and returns its output.
func spawnRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	defer func() { spawnAgent, spawnModel, spawnTeam = "claude", "", false }()
	return runRoot(t, append([]string{"spawn"}, args...)...)
}

func TestSpawnCreatesWorktreeWindowAndRecords(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Fix the auth refresh", "Make it refresh.")
	mainHead := worktreeGit(t, f.repo, "rev-parse", "main")

	out, err := spawnRun(t, "T-1")
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	wt := f.worktree("T-1")
	branch := "curia/T-1-fix-the-auth-refresh"
	if got := worktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
		t.Errorf("worktree HEAD = %q, want refs/heads/%s", got, branch)
	}
	if got := worktreeGit(t, wt, "rev-parse", "HEAD"); got != mainHead {
		t.Errorf("worktree starts at %s, want main's %s", got, mainHead)
	}
	want := []string{"NewWindow api-server T-1 " + wt + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1"}
	if got := f.windows(); !slices.Equal(got, want) {
		t.Errorf("windows = %q, want %q", got, want)
	}
	got := f.get("T-1")
	if got.Branch != branch || got.Worktree != wt || got.Pane != "%100" || !got.LaunchedAt.Equal(f.start) ||
		got.Status != model.TaskReady || got.Team || got.AgentModel != "" {
		t.Errorf("task = %+v", got)
	}
	for _, line := range []string{wt, branch, "T-1 launched in api-server:T-1 (%100); the agent has not reported yet"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}
	if f.sleeps != int(spawnWait/spawnPoll) {
		t.Errorf("waited %d polls, want %d (20 s)", f.sleeps, spawnWait/spawnPoll)
	}
}

func TestSpawnNeverPassesTaskTextToTmux(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("kill ; $(touch x) ;", "a;\n$(rm -rf ~) `id` ;\nkill-server ;\n#{pane_id} %H")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	w := f.windows()
	if len(w) != 1 {
		t.Fatalf("windows = %q, want one", w)
	}
	want := "NewWindow api-server T-1 " + f.worktree("T-1") + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1"
	if w[0] != want {
		t.Errorf("NewWindow call = %q, want %q", w[0], want)
	}
	for _, bad := range []string{";", "$", "`", "\n", "kill", "touch", "#{"} {
		if strings.Contains(w[0], bad) {
			t.Errorf("NewWindow call holds %q from the task text: %q", bad, w[0])
		}
	}
}

func TestSpawnModelAndTeamFlags(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("One", "")
	f.task("Two", "")
	if out, err := spawnRun(t, "T-1", "--model", "opus", "--team"); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	want := []string{"NewWindow api-server T-1 " + f.worktree("T-1") + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1 --model opus --team"}
	if got := f.windows(); !slices.Equal(got, want) {
		t.Errorf("windows = %q, want %q", got, want)
	}
	if got := f.get("T-1"); got.AgentModel != "opus" || !got.Team {
		t.Errorf("task model %q team %v, want opus, true", got.AgentModel, got.Team)
	}

	f.fake.Calls = nil
	if out, err := spawnRun(t, "T-2", "--model", "x;y"); err == nil {
		t.Fatalf("spawn --model x;y succeeded:\n%s", out)
	}
	f.wantNothingStarted("T-2")

	// A model stored on the task is used when no --model is given.
	f.update("T-2", func(tk *model.Task) { tk.AgentModel = "sonnet" })
	if _, out, err := f.spawn("T-2", spawnOptions{}); err != nil {
		t.Fatalf("spawn T-2: %v\n%s", err, out)
	}
	if w := f.windows(); len(w) != 1 || !strings.HasSuffix(w[0], " launch T-2 --model sonnet") {
		t.Errorf("windows = %q, want the stored model", w)
	}
	if got := f.get("T-2"); got.AgentModel != "sonnet" || got.Team {
		t.Errorf("T-2 model %q team %v, want sonnet, false", got.AgentModel, got.Team)
	}
}

func TestSpawnRefusals(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Dropped", "")
	f.update("T-1", func(tk *model.Task) { tk.Status = model.TaskDropped })
	f.task("Paused", "")
	f.update("T-2", func(tk *model.Task) { tk.PausedAt = f.now.Add(-time.Hour) })
	f.task("Plain", "")
	f.task("Bad base", "")
	f.update("T-4", func(tk *model.Task) { tk.BaseBranch = "no-such-base" })

	cases := []struct {
		args []string
		id   string
		want string
	}{
		{[]string{"T-9"}, "T-9", "no task T-9"},
		{[]string{"T-1;x"}, "T-1;x", "not a task id"},
		{[]string{"T-1"}, "T-1", "dropped"},
		{[]string{"T-2"}, "T-2", "use curia resume"},
		{[]string{"T-3", "--agent", "codex"}, "T-3", "only claude"},
		{[]string{"T-4"}, "T-4", "does not resolve"},
	}
	for _, c := range cases {
		out, err := spawnRun(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("spawn %q = %v, want an error containing %q\n%s", c.args, err, c.want, out)
		}
		if task.ValidID(c.id) {
			f.wantNothingStarted(c.id)
		}
	}
	if ok, err := refExists(context.Background(), f.repo, "refs/heads/"+task.Branch("T-4", "Bad base")); err != nil || ok {
		t.Errorf("branch of T-4: exists %v, err %v", ok, err)
	}
}

func TestSpawnRefusesWhenRunning(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Busy", "")
	f.fake.Panes = []tmux.Pane{{ID: "%5", Session: "api-server", WindowID: "@3", WindowName: "T-1"}}
	sess := model.Session{
		ID: "s-live", Agent: "claude", State: model.StateWorking, Pane: "%5", TmuxSession: "api-server",
		TmuxWindow: "@3", TaskID: "T-1", StartedAt: f.now.Add(-time.Minute), UpdatedAt: f.now.Add(-time.Minute),
	}
	if err := f.st.UpsertSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "api-server:T-1") {
		t.Fatalf("spawn with a live session = %v, want a refusal naming api-server:T-1", err)
	}
	f.wantNothingStarted("T-1")

	// The session ends; the pane recorded on the task is still open.
	sess.State, sess.UpdatedAt = model.StateEnded, f.now
	if err = f.st.UpsertSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	f.update("T-1", func(tk *model.Task) { tk.Pane = "%5" })
	_, _, err = f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "api-server:T-1") {
		t.Fatalf("spawn with the recorded pane open = %v, want a refusal naming api-server:T-1", err)
	}
	f.wantNothingStarted("T-1")

	// Once the pane is gone, the task spawns.
	f.fake.Panes = nil
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn after the pane closed: %v\n%s", err, out)
	}
}

func TestSpawnSecondClaimRefused(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Twice", "")
	first := f.now.Add(-time.Minute)
	if _, err := f.st.ClaimLaunch(context.Background(), "T-1", first, time.Minute); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "another spawn of T-1 started at "+first.Local().Format(time.TimeOnly)) {
		t.Fatalf("spawn under another claim = %v", err)
	}
	if w := f.windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if worktreeExists(f.worktree("T-1")) {
		t.Error("worktree created under another spawn's claim")
	}
	if got := f.get("T-1").LaunchedAt; !got.Equal(first) {
		t.Errorf("launched_at = %v, want the other spawn's claim %v kept", got, first)
	}

	// Eleven minutes on, the claim is taken as dead.
	f.now = first.Add(spawnSetupLimit + time.Minute)
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn after the hold: %v\n%s", err, out)
	}
}

func TestSpawnReusesWorktreeOnRetry(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Retry me", "")
	f.fake.NewWindowErr = errors.New("tmux broke")
	_, _, err := f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "tmux broke") {
		t.Fatalf("spawn with a failing NewWindow = %v", err)
	}
	wt := f.worktree("T-1")
	if !strings.Contains(err.Error(), wt) {
		t.Errorf("error does not say the worktree stays: %v", err)
	}
	if got := f.get("T-1"); !got.LaunchedAt.IsZero() || got.Worktree != wt || got.Branch == "" {
		t.Errorf("after the failure: %+v, want the claim released and the worktree recorded", got)
	}
	worktreeWrite(t, filepath.Join(wt, "work.txt"), "kept\n")

	f.fake.NewWindowErr = nil
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("retry: %v\n%s", err, out)
	}
	if !strings.Contains(out, "reusing") {
		t.Errorf("retry output does not say it reused the worktree:\n%s", out)
	}
	if !worktreeExists(filepath.Join(wt, "work.txt")) {
		t.Error("the retry replaced the worktree")
	}
	if n := len(strings.Split(worktreeGit(t, f.repo, "worktree", "list", "--porcelain"), "\n\n")); n != 2 {
		t.Errorf("git worktree list has %d entries, want 2 (main and T-1)", n)
	}
	if w := f.windows(); len(w) != 2 {
		t.Errorf("windows = %q, want the failed attempt and the retry", w)
	}
}

func TestSpawnRefusesForeignPath(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Foreign", "")
	wt := f.worktree("T-1")
	worktreeWrite(t, filepath.Join(wt, "mine.txt"), "the user's\n")
	_, _, err := f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "leaves it alone") {
		t.Fatalf("spawn onto an existing directory = %v", err)
	}
	if b, rerr := os.ReadFile(filepath.Clean(filepath.Join(wt, "mine.txt"))); rerr != nil || string(b) != "the user's\n" {
		t.Errorf("the user's file changed: %q, %v", b, rerr)
	}
	if w := f.windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released", got)
	}
}

func TestSetupScriptRules(t *testing.T) {
	for _, c := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"executable", spawnSetupExecutable},
		{"not executable", spawnSetupNotExecutable},
		{"symlink outside the worktree", spawnSetupOutside},
		{"absent", spawnSetupAbsent},
		{"exit 3", spawnSetupExit3},
		{"2 MiB of output", spawnSetupCapped},
		{"time limit", spawnSetupTimeLimit},
	} {
		t.Run(c.name, c.run)
	}
}

func spawnSetupExecutable(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("#!/bin/sh\n"+
		`echo "task=$CURIA_TASK_ID repo=$CURIA_REPO wt=$CURIA_WORKTREE pwd=$(pwd -P)"`+"\n"+
		"echo to-stderr >&2\n"+
		"read line && echo stdin-had-data\n"+
		"touch made-by-setup\n", true)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	wt := f.worktree("T-1")
	b, err := os.ReadFile(f.setupLog())
	if err != nil {
		t.Fatal(err)
	}
	want := "task=T-1 repo=" + f.repo + " wt=" + wt + " pwd=" + wt + "\nto-stderr\n"
	if string(b) != want {
		t.Errorf("setup log = %q, want %q", b, want)
	}
	if fi, err := os.Stat(f.setupLog()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("setup log mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if strings.Contains(out, "task=T-1") || strings.Contains(out, "to-stderr") {
		t.Errorf("script output reached spawn's output:\n%s", out)
	}
	if !worktreeExists(filepath.Join(wt, "made-by-setup")) {
		t.Error("the script did not run in the worktree")
	}
	if len(f.windows()) != 1 {
		t.Errorf("windows = %q", f.windows())
	}
}

func spawnSetupNotExecutable(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("#!/bin/sh\ntouch made-by-setup\n", false)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not executable") {
		t.Errorf("output does not say the script was skipped:\n%s", out)
	}
	if worktreeExists(filepath.Join(f.worktree("T-1"), "made-by-setup")) || worktreeExists(f.setupLog()) {
		t.Error("a script without an execute bit ran")
	}
}

func spawnSetupOutside(t *testing.T) {
	f := spawnNewFixture(t)
	if err := os.MkdirAll(filepath.Join(f.repo, ".curia"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(f.repo, ".curia", "setup.sh")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.repo, "add", "-A")
	gitRun(t, f.repo, "commit", "-q", "-m", "link")
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if !strings.Contains(out, "outside the worktree") {
		t.Errorf("output does not say the script was skipped:\n%s", out)
	}
	if worktreeExists(f.setupLog()) {
		t.Error("a script outside the worktree ran")
	}
}

func spawnSetupAbsent(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil || !strings.Contains(out, "no .curia/setup.sh") {
		t.Errorf("spawn = %v, output:\n%s", err, out)
	}
}

func spawnSetupExit3(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("#!/bin/sh\ni=1\nwhile [ $i -le 30 ]; do echo \"line $i\"; i=$((i+1)); done\n"+
		"printf 'evil \\033[31mred\\033]0;title\\007 end\\n'\nexit 3\n", true)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), f.setupLog()) {
		t.Fatalf("spawn = %v, want a failure naming the log", err)
	}
	if !strings.Contains(out, "line 30") || !strings.Contains(out, "line 12") || strings.Contains(out, "line 11\n") {
		t.Errorf("output does not hold exactly the last 20 lines:\n%s", out)
	}
	if !strings.Contains(out, "evil red end") || strings.ContainsFunc(out, func(r rune) bool { return r < 0x20 && r != '\n' }) {
		t.Errorf("output not cleaned: %q", out)
	}
	if !strings.Contains(out, f.setupLog()) {
		t.Errorf("output lacks the log path:\n%s", out)
	}
	if w := f.windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released", got)
	}
}

func spawnSetupCapped(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("#!/bin/sh\nhead -c 2097152 /dev/zero | tr '\\000' a\n", true)
	f.task("Setup", "")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	fi, err := os.Stat(f.setupLog())
	if err != nil || fi.Size() != 1<<20 {
		t.Errorf("setup log size = %v, %v; want 1 MiB", fi, err)
	}
}

func spawnSetupTimeLimit(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("#!/bin/sh\necho started\nsleep 30\necho never\n", true)
	f.task("Setup", "")
	spawnSetupLimit = 100 * time.Millisecond
	begin := time.Now()
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("spawn = %v, want a time-out\n%s", err, out)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("spawn took %v after the limit", d)
	}
	// sh may not even have echoed within 100 ms; it never got past the sleep.
	if b, _ := os.ReadFile(f.setupLog()); strings.Contains(string(b), "never") {
		t.Errorf("setup log = %q: the script ran past its limit", b)
	}
	if w := f.windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
}

func TestSpawnSetupFailureLeavesWorktree(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("#!/bin/sh\necho broken\nexit 1\n", true)
	f.task("Setup", "")
	if _, _, err := f.spawn("T-1", spawnOptions{}); err == nil {
		t.Fatal("spawn with a failing setup succeeded")
	}
	wt := f.worktree("T-1")
	if ok, err := worktreeIsOurs(context.Background(), f.repo, wt, f.get("T-1").Branch); err != nil || !ok {
		t.Fatalf("worktree after a failed setup: ours %v, err %v", ok, err)
	}
	if w := f.windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released", got)
	}
	// Fixing the script in the worktree (the file keeps its mode) lets a
	// second spawn through.
	worktreeWrite(t, filepath.Join(wt, ".curia", "setup.sh"), "#!/bin/sh\necho fixed\n")
	setupAnswer(t, setupSum("#!/bin/sh\necho fixed\n")[:8], nil)
	if out, err := runRoot(t, "setup", "approve", "T-1"); err != nil {
		t.Fatalf("setup approve: %v\n%s", err, out)
	}
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("second spawn: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(f.setupLog()); string(b) != "fixed\n" {
		t.Errorf("setup log = %q, want it truncated for the second run", b)
	}
	if w := f.windows(); len(w) != 1 {
		t.Errorf("windows = %q, want one", w)
	}
}

func TestSpawnRefusesMissingWorktreeBeforeWindow(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("#!/bin/sh\nrm -rf \"$CURIA_WORKTREE\"\n", true)
	f.task("Vanishing", "")
	_, _, err := f.spawn("T-1", spawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "no window opened") {
		t.Fatalf("spawn with the worktree gone = %v", err)
	}
	if w := f.windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released", got)
	}
}

func TestSpawnWaitsForLink(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Linked", "")
	f.onSleep = func(n int) {
		if n != 3 {
			return
		}
		done := make(chan error)
		go func() {
			done <- f.st.UpsertSession(context.Background(), model.Session{
				ID: "1a2b3c4d-0000-4000-8000-000000000000", Agent: "claude", State: model.StateIdle, Pane: "%100",
				TaskID: "T-1", StartedAt: f.now, UpdatedAt: f.now,
			})
		}()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	r, out, err := f.spawn("T-1", spawnOptions{Wait: spawnWait})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if r.Session.ID != "1a2b3c4d-0000-4000-8000-000000000000" || f.sleeps != 3 {
		t.Errorf("session %q after %d sleeps, want the linked one after 3", r.Session.ID, f.sleeps)
	}
	if got, want := spawnLine(r), "T-1 running in api-server:T-1 (%100), session 1a2b3c4d"; got != want {
		t.Errorf("spawnLine = %q, want %q", got, want)
	}

	f.onSleep = nil
	f.sleeps = 0
	f.task("Silent", "")
	r, out, err = f.spawn("T-2", spawnOptions{Wait: spawnWait})
	if err != nil {
		t.Fatalf("spawn T-2: %v\n%s", err, out)
	}
	if r.Session.ID != "" || f.sleeps != int(spawnWait/spawnPoll) {
		t.Errorf("session %q after %d sleeps, want none after 80", r.Session.ID, f.sleeps)
	}
	want := "T-2 launched in api-server:T-2 (%101); the agent has not reported yet (look at the window: it may be showing a startup dialog)"
	if got := spawnLine(r); got != want {
		t.Errorf("spawnLine = %q, want %q", got, want)
	}
}

func TestSpawnReportsAgentThatExited(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Untrusted", "")
	f.onSleep = func(int) { f.fake.Panes = nil } // the agent exited and its window closed
	_, _, err := f.spawn("T-1", spawnOptions{Wait: spawnWait})
	if err == nil || !strings.Contains(err.Error(), "exited before it reported") || !strings.Contains(err.Error(), "trust") {
		t.Fatalf("spawn = %v, want a plain report that the agent exited", err)
	}
	if got := f.get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released so a retry is not refused", got)
	}
}

func TestSpawnWaitZeroReturnsAtOnce(t *testing.T) {
	f := spawnNewFixture(t)
	f.task("Now", "")
	r, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if f.sleeps != 0 || r.Session.ID != "" || r.Pane.ID != "%100" || r.Task.Pane != "%100" {
		t.Errorf("result %+v after %d sleeps", r, f.sleeps)
	}
}

// spawnRealSocket is the private tmux server of TestSpawnInRealTmuxAndGit.
var spawnRealSocket = fmt.Sprintf("curia-test-%d-spawn", os.Getpid())

func TestSpawnInRealTmuxAndGit(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := spawnNewFixture(t)
	marker := filepath.Join(t.TempDir(), "marker")
	// bin is a symlink to /bin/sh, so the window's `<bin> launch T-1` runs
	// the script ./launch of the worktree with T-1 as $1.
	worktreeWrite(t, filepath.Join(f.repo, "launch"),
		"pwd -P > '"+marker+"'\nprintf '%s\\n' \"$1\" \"$CURIA_TASK_ID\" >> '"+marker+"'\nexec sleep 600\n")
	gitRun(t, f.repo, "add", "-A")
	gitRun(t, f.repo, "commit", "-q", "-m", "launch stub")
	bin := filepath.Join(t.TempDir(), "curia")
	if err := os.Symlink("/bin/sh", bin); err != nil {
		t.Fatal(err)
	}
	spawnBin = func() (string, error) { return bin, nil }

	raw := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", spawnRealSocket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() { spawnKillServer(spawnRealSocket) })
	_, _ = raw("kill-server")
	if out, err := raw("new-session", "-d", "-s", "keep"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	f.task("Real", "")
	r, err := spawnTask(ctx, f.st, tmux.NewExec(spawnRealSocket), f.cfg, "T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if r.Pane.Session != "api-server" || r.Pane.WindowName != "T-1" {
		t.Errorf("pane = %+v, want api-server:T-1", r.Pane)
	}
	wt := f.worktree("T-1")
	want := wt + "\nT-1\nT-1\n"
	var got []byte
	for range 100 {
		if got, _ = os.ReadFile(filepath.Clean(marker)); string(got) == want {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if string(got) != want {
		t.Errorf("the window ran with %q, want directory, argument and CURIA_TASK_ID %q", got, want)
	}
	if out, err := raw("display-message", "-p", "-t", r.Pane.ID, "#{session_name}:#{window_name}"); err != nil || out != "api-server:T-1" {
		t.Errorf("pane %s is in %q (%v), want api-server:T-1", r.Pane.ID, out, err)
	}
}

// spawnKillServer stops the private test server on socket and removes its
// socket file, which tmux leaves behind (see killServer in internal/tmux).
func spawnKillServer(socket string) {
	out, err := exec.Command("tmux", "-L", socket, "display-message", "-p", "#{socket_path}").Output()
	dir := filepath.Dir(strings.TrimSpace(string(out)))
	if err != nil || dir == "." {
		dir = filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d", os.Getuid()))
	}
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	if root, rerr := os.OpenRoot(dir); rerr == nil {
		_ = root.Remove(socket)
		_ = root.Close()
	}
}

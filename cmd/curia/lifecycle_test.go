package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

// lifecycleLead is the id of the agent session the lifecycle tests link to
// task T-1: a Claude Code session id, so pause keeps it for --resume.
const lifecycleLead = "1a2b3c4d-0000-4000-8000-000000000001"

// lifecycleBranch is task T-1's branch ("Fix auth").
const lifecycleBranch = "curia/T-1-fix-auth"

// lifecycleSpawned is a spawn fixture whose repository ignores node_modules/
// and .env, with task T-1 "Fix auth" spawned with o (window pane %100, no
// wait) and no session linked yet.
func lifecycleSpawned(t *testing.T, o spawnOptions) *spawnFixture {
	t.Helper()
	f := spawnNewFixture(t)
	worktreeWrite(t, filepath.Join(f.repo, ".gitignore"), "node_modules/\n.env\n")
	gitRun(t, f.repo, "add", "-A")
	gitRun(t, f.repo, "commit", "-q", "-m", "ignore")
	f.task("Fix auth", "Make it work.")
	if _, out, err := f.spawn("T-1", o); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	return f
}

// lifecycleWithLead is lifecycleSpawned with the lead session, idle in the
// task's window pane, started at the launch.
func lifecycleWithLead(t *testing.T) *spawnFixture {
	t.Helper()
	f := lifecycleSpawned(t, spawnOptions{})
	lifecycleSession(f, lifecycleLead, "%100", model.StateIdle, f.now)
	return f
}

// lifecycleSession links session id, in pane and state, to T-1, started at
// the launch and updated at updated.
func lifecycleSession(f *spawnFixture, id, pane string, state model.State, updated time.Time) {
	f.t.Helper()
	s := model.Session{
		ID: id, Agent: "claude", State: state, Pane: pane, TaskID: "T-1",
		StartedAt: f.start, UpdatedAt: updated,
	}
	if err := f.st.UpsertSession(context.Background(), s); err != nil {
		f.t.Fatal(err)
	}
}

// lifecycleRun runs `curia args...` in-process and resets the flags of the
// three commands afterwards.
func lifecycleRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	defer func() { pauseForce, pauseNoVerify, cleanForce = false, false, false }()
	return runRoot(t, args...)
}

// lifecycleKills returns the fake's KillPane calls.
func lifecycleKills(f *spawnFixture) []string {
	return slices.DeleteFunc(slices.Clone(f.fake.Calls), func(c string) bool { return !strings.HasPrefix(c, "KillPane") })
}

// lifecycleWantOrder checks that out holds each of lines, in that order.
func lifecycleWantOrder(t *testing.T, out string, lines ...string) {
	t.Helper()
	rest := out
	for _, l := range lines {
		i := strings.Index(rest, l)
		if i < 0 {
			t.Errorf("output lacks %q (in order):\n%s", l, out)
			return
		}
		rest = rest[i+len(l):]
	}
}

// lifecycleWantKept checks that T-1 is not paused and its worktree is still
// on disk on its branch.
func lifecycleWantKept(f *spawnFixture) {
	f.t.Helper()
	wt := f.worktree("T-1")
	if got := worktreeGit(f.t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		f.t.Errorf("worktree HEAD = %q, want refs/heads/%s", got, lifecycleBranch)
	}
	if tk := f.get("T-1"); !tk.PausedAt.IsZero() {
		f.t.Errorf("paused_at = %v, want not paused", tk.PausedAt)
	}
}

// lifecycleCommits is the subjects of the commits on T-1's branch that main
// does not have, newest first.
func lifecycleCommits(f *spawnFixture) []string {
	f.t.Helper()
	out := worktreeGit(f.t, f.repo, "log", "--format=%s", "main.."+lifecycleBranch)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// lifecycleRead returns the content of path.
func lifecycleRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// lifecycleHooks makes core.hooksPath of the fixture's repository a
// directory holding one executable hook, name, with content. The hook is
// made executable through the index and a checkout, so no test writes a
// file with an execute bit.
func lifecycleHooks(f *spawnFixture, name, content string) {
	f.t.Helper()
	dir := filepath.Join(filepath.Dir(f.repo), "hooks")
	if err := os.Mkdir(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	gitInit(f.t, dir, "main")
	worktreeWrite(f.t, filepath.Join(dir, name), content)
	gitRun(f.t, dir, "add", "-A")
	gitRun(f.t, dir, "update-index", "--chmod=+x", name)
	gitRun(f.t, dir, "commit", "-q", "-m", "hook")
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		f.t.Fatal(err)
	}
	gitRun(f.t, dir, "checkout", "--", name)
	gitRun(f.t, f.repo, "config", "core.hooksPath", dir)
}

func TestLifecycleCommandsRefuseInsideAgent(t *testing.T) {
	commands := [][]string{
		{"pause", "T-1"},
		{"pause", "T-1", "--force", "--no-verify"},
		{"resume", "T-1"},
		{"clean", "T-1"},
		{"clean", "T-1", "--force"},
	}
	// No database yet: refusing must not create one.
	db := hookHome(t)
	fake := useFakeTmux(t)
	t.Setenv("CLAUDECODE", "1")
	for _, args := range commands {
		if _, err := lifecycleRun(t, args...); err == nil || !strings.Contains(err.Error(), "inside an agent session") {
			t.Errorf("%v = %v, want the refusal", args, err)
		}
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused command opened the store: %v", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", fake.Calls)
	}

	// With a spawned task: nothing changes.
	t.Setenv("CLAUDECODE", "")
	f := lifecycleWithLead(t)
	worktreeWrite(t, filepath.Join(f.worktree("T-1"), "README"), "changed\n")
	calls := len(f.fake.Calls)
	t.Setenv("CLAUDECODE", "1")
	for _, args := range commands {
		if _, err := lifecycleRun(t, args...); err == nil || !strings.Contains(err.Error(), "inside an agent session") {
			t.Errorf("%v = %v, want the refusal", args, err)
		}
	}
	if got := f.fake.Calls[calls:]; len(got) != 0 {
		t.Errorf("tmux calls = %q, want none", got)
	}
	lifecycleWantKept(f)
	if c := lifecycleCommits(f); len(c) != 0 {
		t.Errorf("commits made: %q", c)
	}
	if got := worktreeGit(t, f.worktree("T-1"), "status", "--porcelain"); got != "M README" {
		t.Errorf("worktree status = %q, want the change untouched", got)
	}
	if tk := f.get("T-1"); tk.Branch != lifecycleBranch || !tk.CleanedAt.IsZero() || tk.Pane != "%100" {
		t.Errorf("task = %+v, want unchanged", tk)
	}
}

func TestPauseCommitsUntrackedThenRemoves(t *testing.T) {
	f := lifecycleWithLead(t)
	wt := f.worktree("T-1")
	worktreeWrite(t, filepath.Join(wt, "README"), "changed\n")
	worktreeWrite(t, filepath.Join(wt, "notes", "new.txt"), "new\n")

	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got, want := lifecycleCommits(f), []string{"curia: WIP T-1 (paused)"}; !slices.Equal(got, want) {
		t.Errorf("commits on the branch = %q, want %q", got, want)
	}
	files := worktreeGit(t, f.repo, "show", "--name-only", "--format=", lifecycleBranch)
	if files != "README\nnotes/new.txt" {
		t.Errorf("WIP commit holds %q, want README and notes/new.txt", files)
	}
	if worktreeExists(wt) {
		t.Errorf("worktree %s still on disk", wt)
	}
	if strings.Contains(worktreeGit(t, f.repo, "worktree", "list", "--porcelain"), wt) {
		t.Errorf("git still lists the worktree %s", wt)
	}
	tk := f.get("T-1")
	if !tk.PausedAt.Equal(f.now) || tk.ResumeSession != lifecycleLead || tk.Pane != "" || !tk.LaunchedAt.IsZero() ||
		tk.Branch != lifecycleBranch || tk.Worktree != wt {
		t.Errorf("task = %+v, want paused now, resume %s, no pane, no launch claim, branch and worktree kept", tk, lifecycleLead)
	}
	if got := lifecycleKills(f); !slices.Equal(got, []string{"KillPane %100"}) {
		t.Errorf("kills = %q, want the task's pane", got)
	}
	lifecycleWantOrder(t, out, "Closed pane api-server:T-1 (%100)", "Committed WIP", lifecycleBranch, "No ignored files",
		"Removed worktree "+wt, "Paused T-1", "curia resume T-1", "1a2b3c4d")
}

func TestPauseKeepsWorktreeWithIgnoredFiles(t *testing.T) {
	f := lifecycleWithLead(t)
	wt := f.worktree("T-1")
	worktreeWrite(t, filepath.Join(wt, "README"), "changed\n")
	worktreeWrite(t, filepath.Join(wt, "node_modules", "left-pad", "index.js"), "x\n")
	worktreeWrite(t, filepath.Join(wt, ".env"), "TOKEN=1\n")

	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause with ignored files: %v\n%s", err, out)
	}
	if got := lifecycleKills(f); !slices.Equal(got, []string{"KillPane %100"}) {
		t.Errorf("kills = %q, want the task's pane", got)
	}
	if got, want := lifecycleCommits(f), []string{"curia: WIP T-1 (paused)"}; !slices.Equal(got, want) {
		t.Errorf("commits on the branch = %q, want %q", got, want)
	}
	if files := worktreeGit(t, f.repo, "show", "--name-only", "--format=", lifecycleBranch); files != "README" {
		t.Errorf("WIP commit holds %q, want README only", files)
	}
	if got := lifecycleRead(t, filepath.Join(wt, "node_modules", "left-pad", "index.js")); got != "x\n" {
		t.Errorf("ignored file = %q, want it kept", got)
	}
	if got := lifecycleRead(t, filepath.Join(wt, ".env")); got != "TOKEN=1\n" {
		t.Errorf(".env = %q, want it kept", got)
	}
	if got := worktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("worktree HEAD = %q, want the task's branch", got)
	}
	tk := f.get("T-1")
	if !tk.PausedAt.Equal(f.now) || tk.ResumeSession != lifecycleLead || tk.Pane != "" || !tk.LaunchedAt.IsZero() || tk.Worktree != wt {
		t.Errorf("task = %+v, want paused now, resume %s, no pane, no launch claim, worktree kept", tk, lifecycleLead)
	}
	lifecycleWantOrder(t, out, "Closed pane api-server:T-1 (%100)", "Committed WIP", "Kept worktree "+wt, "2 ignored entries",
		".env", "node_modules/", "Paused T-1", "resume reuses it")
	if strings.Contains(out, "Removed worktree") {
		t.Errorf("pause says it removed the worktree:\n%s", out)
	}
	if tl := taskMust(t, "task", "ls"); !strings.Contains(tl, "(paused, worktree)") {
		t.Errorf("task ls does not say the pause kept the worktree:\n%s", tl)
	}
}

func TestResumeReusesKeptWorktree(t *testing.T) {
	f := lifecycleWithLead(t)
	wt := f.worktree("T-1")
	script := "#!/bin/sh\necho ran > \"$CURIA_WORKTREE/SETUP_RAN\"\n"
	worktreeWrite(t, filepath.Join(wt, ".curia", "setup.sh"), script)
	gitRun(t, wt, "add", ".curia/setup.sh")
	gitRun(t, wt, "update-index", "--chmod=+x", ".curia/setup.sh")
	gitRun(t, wt, "commit", "-q", "-m", "setup")
	if err := os.Remove(filepath.Join(wt, ".curia", "setup.sh")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "checkout", "--", ".curia/setup.sh") // executable through the checkout
	f.approve(script)
	worktreeWrite(t, filepath.Join(wt, "README"), "work in progress\n")
	worktreeWrite(t, filepath.Join(wt, ".env"), "TOKEN=1\n")
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	f.fake.Panes = nil
	out, err := lifecycleRun(t, "resume", "T-1")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Worktree: reusing") {
		t.Errorf("resume did not reuse the kept worktree:\n%s", out)
	}
	if got := lifecycleRead(t, filepath.Join(wt, ".env")); got != "TOKEN=1\n" {
		t.Errorf(".env = %q, want it kept", got)
	}
	if got := worktreeGit(t, wt, "log", "-1", "--format=%s"); got != "curia: WIP T-1 (paused)" {
		t.Errorf("worktree tip = %q, want the WIP commit", got)
	}
	if !worktreeExists(filepath.Join(wt, "SETUP_RAN")) {
		t.Errorf("the approved setup script did not run on resume:\n%s", out)
	}
	w := f.windows()
	want := "NewWindow api-server T-1 " + wt + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1 --resume " + lifecycleLead
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	if tk := f.get("T-1"); !tk.PausedAt.IsZero() || tk.Pane != "%101" {
		t.Errorf("task = %+v, want not paused, pane %%101", tk)
	}
}

func TestCleanNamesIgnoredFilesAndNeedsForce(t *testing.T) {
	f := lifecycleWithLead(t)
	worktreeWrite(t, filepath.Join(f.worktree("T-1"), ".env"), "TOKEN=1\n")
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	gitRun(t, f.repo, "merge", "-q", "--no-ff", "--no-edit", lifecycleBranch)
	lifecycleEnded(f)
	_, err := lifecycleRun(t, "clean", "T-1")
	if err == nil {
		t.Fatal("clean of a worktree with ignored files succeeded without --force")
	}
	for _, want := range []string{"1 ignored entry", ".env", "Nothing was removed", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if got := lifecycleRead(t, filepath.Join(f.worktree("T-1"), ".env")); got != "TOKEN=1\n" {
		t.Errorf(".env = %q, want it kept", got)
	}
	if ok, _ := refExists(context.Background(), f.repo, "refs/heads/"+lifecycleBranch); !ok {
		t.Error("branch deleted")
	}
	out, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err != nil {
		t.Fatalf("clean --force: %v\n%s", err, out)
	}
	lifecycleWantOrder(t, out, "Removed worktree", "1 ignored entry", ".env", "Deleted branch", "Cleaned T-1")
	if worktreeExists(f.worktree("T-1")) {
		t.Error("worktree still on disk")
	}
}

func TestPauseCleanWorktreeMakesNoCommit(t *testing.T) {
	f := lifecycleWithLead(t)
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if c := lifecycleCommits(f); len(c) != 0 {
		t.Errorf("commits = %q, want none", c)
	}
	if worktreeExists(f.worktree("T-1")) {
		t.Error("worktree still on disk")
	}
	if f.get("T-1").PausedAt.IsZero() {
		t.Error("not paused")
	}
	lifecycleWantOrder(t, out, "Closed pane api-server:T-1 (%100)", "Nothing to commit", "Removed worktree", "Paused T-1")
}

func TestPauseRefusesWorkingAgentWithoutForce(t *testing.T) {
	for _, state := range []model.State{model.StateWorking, model.StateWaiting} {
		t.Run(string(state), func(t *testing.T) {
			f := lifecycleSpawned(t, spawnOptions{})
			lifecycleSession(f, lifecycleLead, "%100", state, f.now)
			_, err := lifecycleRun(t, "pause", "T-1")
			if err == nil || !strings.Contains(err.Error(), string(state)) || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("pause = %v, want a refusal naming %s and --force", err, state)
			}
			if got := lifecycleKills(f); len(got) != 0 {
				t.Errorf("kills = %q, want none", got)
			}
			lifecycleWantKept(f)

			out, err := lifecycleRun(t, "pause", "T-1", "--force")
			if err != nil {
				t.Fatalf("pause --force: %v\n%s", err, out)
			}
			if got := lifecycleKills(f); !slices.Equal(got, []string{"KillPane %100"}) {
				t.Errorf("kills = %q, want the task's pane", got)
			}
			if f.get("T-1").PausedAt.IsZero() || worktreeExists(f.worktree("T-1")) {
				t.Error("pause --force did not pause")
			}
		})
	}
}

func TestPauseKillsOnlyTaskPanes(t *testing.T) {
	f := lifecycleWithLead(t)
	f.fake.Panes = append(f.fake.Panes,
		tmux.Pane{ID: "%101", Session: "api-server", WindowName: "T-1"}, // a teammate of T-1
		tmux.Pane{ID: "%1", Session: "api-server", WindowName: "T-2"},
		tmux.Pane{ID: "%2", Session: "other", WindowName: "T-1"},
		tmux.Pane{ID: "%3", Session: "home", WindowName: "zsh"},
	)
	lifecycleSession(f, "teammate-s", "%101", model.StateIdle, f.now)
	other := model.Session{ID: "other-task", Agent: "claude", State: model.StateIdle, Pane: "%1", TaskID: "T-2", StartedAt: f.start, UpdatedAt: f.now}
	if err := f.st.UpsertSession(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got, want := lifecycleKills(f), []string{"KillPane %100", "KillPane %101"}; !slices.Equal(got, want) {
		t.Errorf("kills = %q, want %q", got, want)
	}
	left := make([]string, 0, len(f.fake.Panes))
	for _, p := range f.fake.Panes {
		left = append(left, p.ID)
	}
	if !slices.Equal(left, []string{"%1", "%2", "%3"}) {
		t.Errorf("panes left = %q, want the three others", left)
	}
}

func TestPauseNeverKillsRecycledPane(t *testing.T) {
	f := lifecycleWithLead(t)
	// tmux restarted after the launch: %100 is now someone else's pane.
	f.fake.Panes = []tmux.Pane{{ID: "%100", Session: "home", WindowName: "zsh", ServerStart: f.start.Add(time.Hour)}}
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("kills = %q, want none: %%100 is another pane now", got)
	}
	if len(f.fake.Panes) != 1 {
		t.Errorf("panes = %+v, want the recycled one kept", f.fake.Panes)
	}
	if !strings.Contains(out, "No agent pane of T-1 is open") {
		t.Errorf("output lacks the no-pane line:\n%s", out)
	}
}

func TestPauseRefusesPaneItCannotIdentify(t *testing.T) {
	f := lifecycleSpawned(t, spawnOptions{})
	// The recorded pane is open, no session reported and the launch claim
	// has lapsed: nothing ties %100 to T-1 any more.
	f.update("T-1", func(tk *model.Task) { tk.LaunchedAt = time.Time{} })
	_, err := lifecycleRun(t, "pause", "T-1")
	if err == nil || !strings.Contains(err.Error(), "%100") || !strings.Contains(err.Error(), "Nothing was done") {
		t.Fatalf("pause = %v, want a refusal naming %%100", err)
	}
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("kills = %q, want none", got)
	}
	lifecycleWantKept(f)
}

func TestPauseCommitFailureKeepsWorktree(t *testing.T) {
	f := lifecycleWithLead(t)
	falseBin, err := exec.LookPath("false")
	if err != nil {
		t.Fatal("no false on PATH")
	}
	hooks := filepath.Join(filepath.Dir(f.repo), "hooks")
	if err = os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(falseBin, filepath.Join(hooks, "pre-commit")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, f.repo, "config", "core.hooksPath", hooks)
	wt := f.worktree("T-1")
	worktreeWrite(t, filepath.Join(wt, "README"), "staged\n")
	gitRun(t, wt, "add", "README")
	worktreeWrite(t, filepath.Join(wt, "new.txt"), "new\n")

	out, err := lifecycleRun(t, "pause", "T-1")
	if err == nil {
		t.Fatalf("pause through a failing pre-commit hook succeeded:\n%s", out)
	}
	for _, want := range []string{"WIP commit failed", "git commit", "--no-verify", "index is as it was"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	lifecycleWantKept(f)
	if c := lifecycleCommits(f); len(c) != 0 {
		t.Errorf("commits = %q, want none", c)
	}
	if got := worktreeGit(t, wt, "status", "--porcelain"); got != "M  README\n?? new.txt" {
		t.Errorf("worktree status = %q, want README staged and new.txt untracked as before", got)
	}
	if got := lifecycleRead(t, filepath.Join(wt, "new.txt")); got != "new\n" {
		t.Errorf("new.txt = %q", got)
	}

	out, err = lifecycleRun(t, "pause", "T-1", "--no-verify")
	if err != nil {
		t.Fatalf("pause --no-verify: %v\n%s", err, out)
	}
	if got, want := lifecycleCommits(f), []string{"curia: WIP T-1 (paused)"}; !slices.Equal(got, want) {
		t.Errorf("commits = %q, want %q", got, want)
	}
	if f.get("T-1").PausedAt.IsZero() || worktreeExists(wt) {
		t.Error("pause --no-verify did not pause")
	}
	if !strings.Contains(out, "--no-verify") {
		t.Errorf("output does not say the hooks were skipped:\n%s", out)
	}
}

func TestPauseNeverForcesRemoval(t *testing.T) {
	f := lifecycleWithLead(t)
	// A post-commit hook writes a file after the WIP commit.
	lifecycleHooks(f, "post-commit", "#!/bin/sh\necho late > late.txt\n")
	wt := f.worktree("T-1")
	worktreeWrite(t, filepath.Join(wt, "README"), "changed\n")

	out, err := lifecycleRun(t, "pause", "T-1")
	if err == nil {
		t.Fatalf("pause succeeded:\n%s", out)
	}
	for _, want := range []string{"never forces", "WIP commit", "curia pause T-1 again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	lifecycleWantKept(f)
	if got := lifecycleRead(t, filepath.Join(wt, "late.txt")); got != "late\n" {
		t.Errorf("late.txt = %q, want kept", got)
	}
	if got, want := lifecycleCommits(f), []string{"curia: WIP T-1 (paused)"}; !slices.Equal(got, want) {
		t.Errorf("commits = %q, want %q", got, want)
	}
}

func TestPauseKeepsWorktreeWhenIgnoredFileAppearsAfterCommit(t *testing.T) {
	f := lifecycleWithLead(t)
	// A post-commit hook writes an ignored file after the WIP commit: the
	// check after the commit sees it and keeps the worktree.
	lifecycleHooks(f, "post-commit", "#!/bin/sh\necho late > .env\n")
	wt := f.worktree("T-1")
	worktreeWrite(t, filepath.Join(wt, "README"), "changed\n")
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := lifecycleRead(t, filepath.Join(wt, ".env")); got != "late\n" {
		t.Errorf(".env = %q, want kept", got)
	}
	if !strings.Contains(out, "Kept worktree") || f.get("T-1").PausedAt.IsZero() {
		t.Errorf("want the worktree kept and the task paused:\n%s", out)
	}
}

func TestPausePicksLeadAsResumeSession(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	f := lifecycleSpawned(t, spawnOptions{})
	teamsWriteCaptured(t, filepath.Join(claude, "teams"))
	f.fake.Panes = append(f.fake.Panes, tmux.Pane{ID: "%17", Session: "api-server", WindowName: "T-1"})
	lifecycleSession(f, teamsCapturedLead, "%100", model.StateIdle, f.now.Add(-time.Minute))
	lifecycleSession(f, "alpha-s", "%17", model.StateIdle, f.now)

	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := f.get("T-1").ResumeSession; got != teamsCapturedLead {
		t.Errorf("resume_session = %q, want the lead %s", got, teamsCapturedLead)
	}
	got := lifecycleKills(f)
	slices.Sort(got)
	if want := []string{"KillPane %100", "KillPane %17"}; !slices.Equal(got, want) {
		t.Errorf("kills = %q, want %q", got, want)
	}
}

func TestPauseKeepsNoSessionIDThatIsNotOne(t *testing.T) {
	f := lifecycleSpawned(t, spawnOptions{})
	lifecycleSession(f, "not-a-claude-id", "%100", model.StateIdle, f.now)
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := f.get("T-1").ResumeSession; got != "" {
		t.Errorf("resume_session = %q, want none", got)
	}
	if !strings.Contains(out, "from the brief") {
		t.Errorf("output does not say resume starts from the brief:\n%s", out)
	}
}

func TestPauseRefusesWhenNotSpawnedOrAlreadyPaused(t *testing.T) {
	f := lifecycleWithLead(t)
	f.task("Never spawned", "")
	if _, err := lifecycleRun(t, "pause", "T-2"); err == nil || !strings.Contains(err.Error(), "not been spawned") {
		t.Errorf("pause T-2 = %v, want not spawned", err)
	}
	if _, err := lifecycleRun(t, "pause", "T-9"); err == nil || !strings.Contains(err.Error(), "no task T-9") {
		t.Errorf("pause T-9 = %v, want no task", err)
	}
	if _, err := lifecycleRun(t, "pause", "nope"); err == nil || !strings.Contains(err.Error(), "not a task id") {
		t.Errorf("pause nope = %v, want not a task id", err)
	}
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	kills := len(lifecycleKills(f))
	if _, err := lifecycleRun(t, "pause", "T-1"); err == nil || !strings.Contains(err.Error(), "already paused") {
		t.Errorf("second pause = %v, want already paused", err)
	}
	if got := len(lifecycleKills(f)); got != kills {
		t.Errorf("second pause killed %d panes", got-kills)
	}
}

func TestLifecycleRefusesMovedOrSymlinkedWorktree(t *testing.T) {
	f := lifecycleWithLead(t)
	wt := f.worktree("T-1")
	moved := filepath.Join(filepath.Dir(f.repo), "moved")
	gitRun(t, f.repo, "worktree", "move", wt, moved)
	if err := os.Symlink(moved, wt); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"pause", "T-1"}, {"pause", "T-1", "--force"}, {"clean", "T-1", "--force"}} {
		if _, err := lifecycleRun(t, args...); err == nil || !strings.Contains(err.Error(), "leaves it alone") {
			t.Errorf("%v = %v, want a refusal", args, err)
		}
	}
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("kills = %q, want none", got)
	}
	if fi, err := os.Lstat(wt); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink at %s was touched: %v", wt, err)
	}
	if got := worktreeGit(t, moved, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("moved worktree HEAD = %q", got)
	}
	tk := f.get("T-1")
	if !tk.PausedAt.IsZero() || tk.Branch != lifecycleBranch {
		t.Errorf("task = %+v, want unchanged", tk)
	}

	// A plain directory that is not a worktree is refused too.
	if err := os.Remove(wt); err != nil {
		t.Fatal(err)
	}
	worktreeWrite(t, filepath.Join(wt, "mine.txt"), "mine\n")
	if _, err := lifecycleRun(t, "clean", "T-1", "--force"); err == nil || !strings.Contains(err.Error(), "leaves it alone") {
		t.Errorf("clean over a foreign directory = %v, want a refusal", err)
	}
	if got := lifecycleRead(t, filepath.Join(wt, "mine.txt")); got != "mine\n" {
		t.Errorf("mine.txt = %q", got)
	}
}

func TestLifecycleRefusesWhileSpawnUnderWay(t *testing.T) {
	f := lifecycleSpawned(t, spawnOptions{})
	// The launch claim is fresh, no agent has reported and no window is
	// open: a spawn (its setup script) may be running in the worktree.
	f.fake.Panes = nil
	for _, args := range [][]string{{"pause", "T-1", "--force"}, {"clean", "T-1", "--force"}} {
		if _, err := lifecycleRun(t, args...); err == nil || !strings.Contains(err.Error(), "under way") {
			t.Errorf("%v = %v, want a refusal", args, err)
		}
	}
	lifecycleWantKept(f)

	f.now = f.now.Add(spawnSetupLimit + 2*time.Minute)
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause after the claim lapsed: %v\n%s", err, out)
	}
}

// lifecyclePaused is lifecycleWithLead after a change to README and a pause.
func lifecyclePaused(t *testing.T, o spawnOptions, lead bool) *spawnFixture {
	t.Helper()
	f := lifecycleSpawned(t, o)
	if lead {
		lifecycleSession(f, lifecycleLead, "%100", model.StateIdle, f.now)
	}
	worktreeWrite(t, filepath.Join(f.worktree("T-1"), "README"), "work in progress\n")
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	return f
}

func TestResumeRecreatesWorktreeAndResumes(t *testing.T) {
	f := lifecyclePaused(t, spawnOptions{}, true)
	wt := f.worktree("T-1")
	out, err := lifecycleRun(t, "resume", "T-1")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if got := worktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("worktree HEAD = %q", got)
	}
	if got := worktreeGit(t, wt, "log", "-1", "--format=%s"); got != "curia: WIP T-1 (paused)" {
		t.Errorf("worktree tip = %q, want the WIP commit", got)
	}
	if got := lifecycleRead(t, filepath.Join(wt, "README")); got != "work in progress\n" {
		t.Errorf("README = %q", got)
	}
	w := f.windows()
	want := "NewWindow api-server T-1 " + wt + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1 --resume " + lifecycleLead
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	tk := f.get("T-1")
	if !tk.PausedAt.IsZero() || tk.Pane != "%101" || tk.LaunchedAt.IsZero() {
		t.Errorf("task = %+v, want not paused, pane %%101, launched", tk)
	}
	lifecycleWantOrder(t, out, "session 1a2b3c4d", "Worktree: created", "T-1 launched in api-server:T-1 (%101)")
}

func TestResumeWithoutSessionStartsFresh(t *testing.T) {
	f := lifecyclePaused(t, spawnOptions{}, false)
	if got := f.get("T-1").ResumeSession; got != "" {
		t.Fatalf("resume_session = %q, want none", got)
	}
	out, err := lifecycleRun(t, "resume", "T-1")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	w := f.windows()
	want := "NewWindow api-server T-1 " + f.worktree("T-1") + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1"
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	if !strings.Contains(out, "from the brief") {
		t.Errorf("output does not say it starts from the brief:\n%s", out)
	}
	if !f.get("T-1").PausedAt.IsZero() {
		t.Error("still paused")
	}
}

func TestResumeKeepsModelAndTeam(t *testing.T) {
	f := lifecyclePaused(t, spawnOptions{Model: "opus", Team: true}, true)
	if out, err := lifecycleRun(t, "resume", "T-1"); err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	w := f.windows()
	want := "NewWindow api-server T-1 " + f.worktree("T-1") + " CURIA_TASK_ID=T-1 " + spawnTestBin +
		" launch T-1 --model opus --team --resume " + lifecycleLead
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	if tk := f.get("T-1"); tk.AgentModel != "opus" || !tk.Team {
		t.Errorf("task = %+v, want model opus and team kept", tk)
	}
}

func TestResumeRefusesWhenNotPaused(t *testing.T) {
	f := lifecycleWithLead(t)
	if _, err := lifecycleRun(t, "resume", "T-1"); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Errorf("resume = %v, want not paused", err)
	}
	if w := f.windows(); len(w) != 1 {
		t.Errorf("windows = %q, want only the spawn's", w)
	}
}

func TestResumeRefusesWhenBranchGone(t *testing.T) {
	f := lifecyclePaused(t, spawnOptions{}, true)
	gitRun(t, f.repo, "branch", "-D", lifecycleBranch)
	if _, err := lifecycleRun(t, "resume", "T-1"); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Errorf("resume = %v, want the branch is gone", err)
	}
	if worktreeExists(f.worktree("T-1")) || len(f.windows()) != 1 {
		t.Error("resume created a worktree or a window")
	}
	if f.get("T-1").PausedAt.IsZero() {
		t.Error("no longer paused")
	}
}

func TestResumeFailureStaysPaused(t *testing.T) {
	f := lifecyclePaused(t, spawnOptions{}, true)
	f.fake.NewWindowErr = errors.New("no server")
	_, err := lifecycleRun(t, "resume", "T-1")
	if err == nil || !strings.Contains(err.Error(), "paused again") {
		t.Fatalf("resume = %v, want a failure that says T-1 is paused again", err)
	}
	tk := f.get("T-1")
	if tk.PausedAt.IsZero() || tk.ResumeSession != lifecycleLead {
		t.Errorf("task = %+v, want still paused with its session", tk)
	}
	// The retry reuses the worktree the failed resume made.
	f.fake.NewWindowErr = nil
	out, err := lifecycleRun(t, "resume", "T-1")
	if err != nil {
		t.Fatalf("resume retry: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Worktree: reusing") {
		t.Errorf("retry did not reuse the worktree:\n%s", out)
	}
}

// lifecycleEnded ends T-1's lead session and closes its window.
func lifecycleEnded(f *spawnFixture) {
	f.t.Helper()
	lifecycleSession(f, lifecycleLead, "%100", model.StateEnded, f.now)
	f.fake.Panes = nil
}

// lifecycleCommitOnBranch commits a change to README in T-1's worktree.
func lifecycleCommitOnBranch(f *spawnFixture, subject string) {
	f.t.Helper()
	wt := f.worktree("T-1")
	worktreeWrite(f.t, filepath.Join(wt, "README"), subject+"\n")
	gitRun(f.t, wt, "commit", "-q", "-am", subject)
}

func TestCleanRemovesMergedBranchAndWorktree(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleCommitOnBranch(f, "the fix")
	gitRun(t, f.repo, "merge", "-q", "--no-ff", "--no-edit", lifecycleBranch)
	mainHead := worktreeGit(t, f.repo, "rev-parse", "main")
	worktreeWrite(t, filepath.Join(f.worktree("T-1"), "node_modules", "x.js"), "x\n")
	lifecycleEnded(f)

	// The ignored node_modules/ is named and kept without --force (Ruling 111).
	if _, err := lifecycleRun(t, "clean", "T-1"); err == nil || !strings.Contains(err.Error(), "node_modules/") {
		t.Fatalf("clean without --force = %v, want a refusal naming node_modules/", err)
	}
	out, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	if worktreeExists(f.worktree("T-1")) {
		t.Error("worktree still on disk")
	}
	if ok, err := refExists(context.Background(), f.repo, "refs/heads/"+lifecycleBranch); err != nil || ok {
		t.Errorf("branch exists %v (%v), want deleted", ok, err)
	}
	if got := worktreeGit(t, f.repo, "rev-parse", "main"); got != mainHead {
		t.Errorf("main moved to %s", got)
	}
	tk := f.get("T-1")
	if !tk.CleanedAt.Equal(f.now) || tk.Branch != "" || tk.Worktree != "" || tk.Pane != "" ||
		tk.ResumeSession != "" || !tk.PausedAt.IsZero() || tk.Status != model.TaskReady {
		t.Errorf("task = %+v, want cleaned, fields cleared, status ready", tk)
	}
	lifecycleWantOrder(t, out, "merged into main", "Removed worktree", "1 ignored", "node_modules/",
		"Deleted branch "+lifecycleBranch, "Cleaned T-1")
}

func TestCleanRefusesUnmerged(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleCommitOnBranch(f, "unmerged work")
	lifecycleEnded(f)

	_, err := lifecycleRun(t, "clean", "T-1")
	if err == nil {
		t.Fatal("clean of an unmerged branch succeeded")
	}
	for _, want := range []string{"not merged", "squash", "--force", "1 commit", "unmerged work", "Nothing was removed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if !worktreeExists(f.worktree("T-1")) {
		t.Error("worktree removed")
	}
	if ok, _ := refExists(context.Background(), f.repo, "refs/heads/"+lifecycleBranch); !ok {
		t.Error("branch deleted")
	}

	out, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err != nil {
		t.Fatalf("clean --force: %v\n%s", err, out)
	}
	if worktreeExists(f.worktree("T-1")) {
		t.Error("worktree still on disk")
	}
	if ok, _ := refExists(context.Background(), f.repo, "refs/heads/"+lifecycleBranch); ok {
		t.Error("branch still there")
	}
	lifecycleWantOrder(t, out, "not merged", "--force", "lost", "unmerged work", "Removed worktree", "Deleted branch")
}

func TestCleanForceNeverForcesWorktreeRemoval(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleEnded(f)
	worktreeWrite(t, filepath.Join(f.worktree("T-1"), "draft.txt"), "draft\n")
	_, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err == nil || !strings.Contains(err.Error(), "never forces") {
		t.Fatalf("clean --force of a dirty worktree = %v, want git's refusal", err)
	}
	if got := lifecycleRead(t, filepath.Join(f.worktree("T-1"), "draft.txt")); got != "draft\n" {
		t.Errorf("draft.txt = %q", got)
	}
	if ok, _ := refExists(context.Background(), f.repo, "refs/heads/"+lifecycleBranch); !ok {
		t.Error("branch deleted although the worktree stayed")
	}
	if !f.get("T-1").CleanedAt.IsZero() {
		t.Error("cleaned_at set")
	}
}

func TestCleanNeverTouchesBase(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleEnded(f)
	f.update("T-1", func(tk *model.Task) { tk.Branch = "main" })
	if _, err := lifecycleRun(t, "clean", "T-1", "--force"); err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Errorf("clean of the base = %v, want a refusal", err)
	}
	if ok, _ := refExists(context.Background(), f.repo, "refs/heads/main"); !ok {
		t.Error("main deleted")
	}
	if !worktreeExists(f.worktree("T-1")) {
		t.Error("worktree removed")
	}
}

func TestCleanRefusesWithLiveAgent(t *testing.T) {
	f := lifecycleWithLead(t)
	if _, err := lifecycleRun(t, "clean", "T-1"); err == nil || !strings.Contains(err.Error(), "live session") {
		t.Errorf("clean with a live session = %v, want a refusal", err)
	}
	// The session ended but its window is still open.
	lifecycleSession(f, lifecycleLead, "%100", model.StateEnded, f.now)
	if _, err := lifecycleRun(t, "clean", "T-1"); err == nil || !strings.Contains(err.Error(), "still open") {
		t.Errorf("clean with the window open = %v, want a refusal", err)
	}
	if !worktreeExists(f.worktree("T-1")) {
		t.Error("worktree removed")
	}
	if ok, _ := refExists(context.Background(), f.repo, "refs/heads/"+lifecycleBranch); !ok {
		t.Error("branch deleted")
	}
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("kills = %q, want none", got)
	}
}

func TestCleanAfterPause(t *testing.T) {
	f := lifecycleWithLead(t)
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	out, err := lifecycleRun(t, "clean", "T-1")
	if err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	if ok, _ := refExists(context.Background(), f.repo, "refs/heads/"+lifecycleBranch); ok {
		t.Error("branch still there")
	}
	tk := f.get("T-1")
	if tk.CleanedAt.IsZero() || !tk.PausedAt.IsZero() || tk.ResumeSession != "" || tk.Branch != "" {
		t.Errorf("task = %+v, want cleaned with pause fields cleared", tk)
	}
	lifecycleWantOrder(t, out, "No worktree on disk", "Deleted branch", "Cleaned T-1")
	if strings.Contains(out, "Removed worktree") {
		t.Errorf("clean says it removed a worktree:\n%s", out)
	}
}

func TestSpawnAfterCleanStartsOver(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleCommitOnBranch(f, "the fix")
	gitRun(t, f.repo, "merge", "-q", "--no-ff", "--no-edit", lifecycleBranch)
	lifecycleEnded(f)
	if out, err := lifecycleRun(t, "clean", "T-1"); err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn after clean: %v\n%s", err, out)
	}
	wt := f.worktree("T-1")
	if got, want := worktreeGit(t, wt, "rev-parse", "HEAD"), worktreeGit(t, f.repo, "rev-parse", "main"); got != want {
		t.Errorf("new worktree at %s, want main's %s", got, want)
	}
	if got := worktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("worktree HEAD = %q", got)
	}
	if tk := f.get("T-1"); tk.Branch != lifecycleBranch || tk.Worktree != wt {
		t.Errorf("task = %+v, want branch and worktree recorded again", tk)
	}
}

// lifecycleRealSocket is the private tmux server of TestPauseInRealTmuxAndGit.
var lifecycleRealSocket = fmt.Sprintf("curia-test-%d-lc", os.Getpid())

func TestPauseInRealTmuxAndGit(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := spawnNewFixture(t)
	// bin is a symlink to /bin/sh, so the window's `<bin> launch T-1` runs
	// the worktree's ./launch, which stands in for the agent.
	worktreeWrite(t, filepath.Join(f.repo, "launch"), "exec sleep 600\n")
	gitRun(t, f.repo, "add", "-A")
	gitRun(t, f.repo, "commit", "-q", "-m", "launch stub")
	bin := filepath.Join(t.TempDir(), "curia")
	if err := os.Symlink("/bin/sh", bin); err != nil {
		t.Fatal(err)
	}
	spawnBin = func() (string, error) { return bin, nil }
	raw := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", lifecycleRealSocket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() { spawnKillServer(lifecycleRealSocket) })
	_, _ = raw("kill-server")
	if out, err := raw("new-session", "-d", "-s", "keep"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	keep, err := raw("display-message", "-p", "-t", "=keep:", "#{pane_id}")
	if err != nil {
		t.Fatalf("keep pane: %v: %s", err, keep)
	}
	// The real server's start time is real, so the clock must be too.
	f.now = time.Now().UTC()
	f.start = f.now
	tm := tmux.NewExec(lifecycleRealSocket)
	f.task("Real", "")
	r, err := spawnTask(ctx, f.st, tm, f.cfg, "T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	lifecycleSession(f, lifecycleLead, r.Pane.ID, model.StateIdle, f.now)
	wt := f.worktree("T-1")
	worktreeWrite(t, filepath.Join(wt, "README"), "changed\n")

	var out strings.Builder
	if err = pauseTask(ctx, f.st, tm, f.cfg, "T-1", pauseOptions{}, &out); err != nil {
		t.Fatalf("pause: %v\n%s", err, out.String())
	}
	panes, err := raw("list-panes", "-a", "-F", "#{pane_id}")
	if err != nil || panes != keep {
		t.Errorf("panes left = %q (%v), want only the keep session's %s", panes, err, keep)
	}
	if worktreeExists(wt) {
		t.Error("worktree still on disk")
	}
	if got := f.get("T-1"); got.PausedAt.IsZero() || got.ResumeSession != lifecycleLead {
		t.Errorf("task = %+v, want paused with the lead's session", got)
	}
	if got := worktreeGit(t, f.repo, "log", "-1", "--format=%s", "curia/T-1-real"); got != "curia: WIP T-1 (paused)" {
		t.Errorf("branch tip = %q, want the WIP commit", got)
	}
}

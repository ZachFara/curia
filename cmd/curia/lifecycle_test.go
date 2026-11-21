package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/tmux"
)

// lifecycleBranch is task T-1's branch ("Fix auth").
const lifecycleBranch = testkit.LifecycleBranch

// lifecycleSpawned is a spawn fixture whose repository ignores node_modules/
// and .env, with task T-1 "Fix auth" spawned with o (window pane %100, no
// wait) and no session linked yet.
func lifecycleSpawned(t *testing.T, o lifecycle.SpawnOptions) *spawnFixture {
	t.Helper()
	f := spawnNewFixture(t)
	testkit.SpawnedFrom(f.SpawnFixture, o)
	return f
}

// lifecycleWithLead is lifecycleSpawned with the lead session, idle in the
// task's window pane, started at the launch.
func lifecycleWithLead(t *testing.T) *spawnFixture {
	t.Helper()
	f := lifecycleSpawned(t, lifecycle.SpawnOptions{})
	testkit.LifecycleSession(f.SpawnFixture, testkit.LifecycleLead, "%100", model.StateIdle, f.Now)
	return f
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
	return slices.DeleteFunc(slices.Clone(f.Fake.Calls), func(c string) bool { return !strings.HasPrefix(c, "KillPane") })
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
	f.T.Helper()
	wt := f.Worktree("T-1")
	if got := testkit.WorktreeGit(f.T, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		f.T.Errorf("worktree HEAD = %q, want refs/heads/%s", got, lifecycleBranch)
	}
	if tk := f.Get("T-1"); !tk.PausedAt.IsZero() {
		f.T.Errorf("paused_at = %v, want not paused", tk.PausedAt)
	}
}

// lifecycleCommits is the subjects of the commits on T-1's branch that main
// does not have, newest first.
func lifecycleCommits(f *spawnFixture) []string {
	f.T.Helper()
	out := testkit.WorktreeGit(f.T, f.Repo, "log", "--format=%s", "main.."+lifecycleBranch)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
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
	db := testkit.HookHome(t)
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
	testkit.WorktreeWrite(t, filepath.Join(f.Worktree("T-1"), "README"), "changed\n")
	calls := len(f.Fake.Calls)
	t.Setenv("CLAUDECODE", "1")
	for _, args := range commands {
		if _, err := lifecycleRun(t, args...); err == nil || !strings.Contains(err.Error(), "inside an agent session") {
			t.Errorf("%v = %v, want the refusal", args, err)
		}
	}
	if got := f.Fake.Calls[calls:]; len(got) != 0 {
		t.Errorf("tmux calls = %q, want none", got)
	}
	lifecycleWantKept(f)
	if c := lifecycleCommits(f); len(c) != 0 {
		t.Errorf("commits made: %q", c)
	}
	if got := testkit.WorktreeGit(t, f.Worktree("T-1"), "status", "--porcelain"); got != "M README" {
		t.Errorf("worktree status = %q, want the change untouched", got)
	}
	if tk := f.Get("T-1"); tk.Branch != lifecycleBranch || !tk.CleanedAt.IsZero() || tk.Pane != "%100" {
		t.Errorf("task = %+v, want unchanged", tk)
	}
}

func TestPauseCommitsUntrackedThenRemoves(t *testing.T) {
	f := lifecycleWithLead(t)
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(t, filepath.Join(wt, "README"), "changed\n")
	testkit.WorktreeWrite(t, filepath.Join(wt, "notes", "new.txt"), "new\n")

	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got, want := lifecycleCommits(f), []string{"curia: WIP T-1 (paused)"}; !slices.Equal(got, want) {
		t.Errorf("commits on the branch = %q, want %q", got, want)
	}
	files := testkit.WorktreeGit(t, f.Repo, "show", "--name-only", "--format=", lifecycleBranch)
	if files != "README\nnotes/new.txt" {
		t.Errorf("WIP commit holds %q, want README and notes/new.txt", files)
	}
	if testkit.WorktreeExists(wt) {
		t.Errorf("worktree %s still on disk", wt)
	}
	if strings.Contains(testkit.WorktreeGit(t, f.Repo, "worktree", "list", "--porcelain"), wt) {
		t.Errorf("git still lists the worktree %s", wt)
	}
	tk := f.Get("T-1")
	if !tk.PausedAt.Equal(f.Now) || tk.ResumeSession != testkit.LifecycleLead || tk.Pane != "" || !tk.LaunchedAt.IsZero() ||
		tk.Branch != lifecycleBranch || tk.Worktree != wt {
		t.Errorf("task = %+v, want paused now, resume %s, no pane, no launch claim, branch and worktree kept", tk, testkit.LifecycleLead)
	}
	if got := lifecycleKills(f); !slices.Equal(got, []string{"KillPane %100"}) {
		t.Errorf("kills = %q, want the task's pane", got)
	}
	lifecycleWantOrder(t, out, "Closed pane api-server:T-1 (%100)", "Committed WIP", lifecycleBranch, "No ignored files",
		"Removed worktree "+wt, "Paused T-1", "curia resume T-1", "1a2b3c4d")
}

func TestPauseKeepsWorktreeWithIgnoredFiles(t *testing.T) {
	f := lifecycleWithLead(t)
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(t, filepath.Join(wt, "README"), "changed\n")
	testkit.WorktreeWrite(t, filepath.Join(wt, "node_modules", "left-pad", "index.js"), "x\n")
	testkit.WorktreeWrite(t, filepath.Join(wt, ".env"), "TOKEN=1\n")

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
	if files := testkit.WorktreeGit(t, f.Repo, "show", "--name-only", "--format=", lifecycleBranch); files != "README" {
		t.Errorf("WIP commit holds %q, want README only", files)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(wt, "node_modules", "left-pad", "index.js")); got != "x\n" {
		t.Errorf("ignored file = %q, want it kept", got)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(wt, ".env")); got != "TOKEN=1\n" {
		t.Errorf(".env = %q, want it kept", got)
	}
	if got := testkit.WorktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("worktree HEAD = %q, want the task's branch", got)
	}
	tk := f.Get("T-1")
	if !tk.PausedAt.Equal(f.Now) || tk.ResumeSession != testkit.LifecycleLead || tk.Pane != "" || !tk.LaunchedAt.IsZero() || tk.Worktree != wt {
		t.Errorf("task = %+v, want paused now, resume %s, no pane, no launch claim, worktree kept", tk, testkit.LifecycleLead)
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
	wt := f.Worktree("T-1")
	script := "#!/bin/sh\necho ran > \"$CURIA_WORKTREE/SETUP_RAN\"\n"
	testkit.WorktreeWrite(t, filepath.Join(wt, ".curia", "setup.sh"), script)
	testkit.GitRun(t, wt, "add", ".curia/setup.sh")
	testkit.GitRun(t, wt, "update-index", "--chmod=+x", ".curia/setup.sh")
	testkit.GitRun(t, wt, "commit", "-q", "-m", "setup")
	if err := os.Remove(filepath.Join(wt, ".curia", "setup.sh")); err != nil {
		t.Fatal(err)
	}
	testkit.GitRun(t, wt, "checkout", "--", ".curia/setup.sh") // executable through the checkout
	f.Approve(script)
	testkit.WorktreeWrite(t, filepath.Join(wt, "README"), "work in progress\n")
	testkit.WorktreeWrite(t, filepath.Join(wt, ".env"), "TOKEN=1\n")
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	f.Fake.Panes = nil
	out, err := lifecycleRun(t, "resume", "T-1")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Worktree: reusing") {
		t.Errorf("resume did not reuse the kept worktree:\n%s", out)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(wt, ".env")); got != "TOKEN=1\n" {
		t.Errorf(".env = %q, want it kept", got)
	}
	if got := testkit.WorktreeGit(t, wt, "log", "-1", "--format=%s"); got != "curia: WIP T-1 (paused)" {
		t.Errorf("worktree tip = %q, want the WIP commit", got)
	}
	if !testkit.WorktreeExists(filepath.Join(wt, "SETUP_RAN")) {
		t.Errorf("the approved setup script did not run on resume:\n%s", out)
	}
	w := f.Windows()
	want := "NewWindow api-server T-1 " + wt + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1 --resume " + testkit.LifecycleLead + " --after-pause"
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	if tk := f.Get("T-1"); !tk.PausedAt.IsZero() || tk.Pane != "%101" {
		t.Errorf("task = %+v, want not paused, pane %%101", tk)
	}
}

func TestCleanNamesIgnoredFilesAndNeedsForce(t *testing.T) {
	f := lifecycleWithLead(t)
	testkit.WorktreeWrite(t, filepath.Join(f.Worktree("T-1"), ".env"), "TOKEN=1\n")
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	testkit.GitRun(t, f.Repo, "merge", "-q", "--no-ff", "--no-edit", lifecycleBranch)
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
	if got := testkit.LifecycleRead(t, filepath.Join(f.Worktree("T-1"), ".env")); got != "TOKEN=1\n" {
		t.Errorf(".env = %q, want it kept", got)
	}
	if ok, _ := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+lifecycleBranch); !ok {
		t.Error("branch deleted")
	}
	out, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err != nil {
		t.Fatalf("clean --force: %v\n%s", err, out)
	}
	lifecycleWantOrder(t, out, "Removed worktree", "1 ignored entry", ".env", "Deleted branch", "Cleaned T-1")
	if testkit.WorktreeExists(f.Worktree("T-1")) {
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
	if testkit.WorktreeExists(f.Worktree("T-1")) {
		t.Error("worktree still on disk")
	}
	if f.Get("T-1").PausedAt.IsZero() {
		t.Error("not paused")
	}
	lifecycleWantOrder(t, out, "Closed pane api-server:T-1 (%100)", "Nothing to commit", "Removed worktree", "Paused T-1")
}

func TestPauseRefusesWorkingAgentWithoutForce(t *testing.T) {
	for _, state := range []model.State{model.StateWorking, model.StateWaiting} {
		t.Run(string(state), func(t *testing.T) {
			f := lifecycleSpawned(t, lifecycle.SpawnOptions{})
			testkit.LifecycleSession(f.SpawnFixture, testkit.LifecycleLead, "%100", state, f.Now)
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
			if f.Get("T-1").PausedAt.IsZero() || testkit.WorktreeExists(f.Worktree("T-1")) {
				t.Error("pause --force did not pause")
			}
		})
	}
}

func TestPauseKillsOnlyTaskPanes(t *testing.T) {
	f := lifecycleWithLead(t)
	f.Fake.Panes = append(f.Fake.Panes,
		tmux.Pane{ID: "%101", Session: "api-server", WindowName: "T-1"}, // a teammate of T-1
		tmux.Pane{ID: "%1", Session: "api-server", WindowName: "T-2"},
		tmux.Pane{ID: "%2", Session: "other", WindowName: "T-1"},
		tmux.Pane{ID: "%3", Session: "home", WindowName: "zsh"},
	)
	testkit.LifecycleSession(f.SpawnFixture, "teammate-s", "%101", model.StateIdle, f.Now)
	other := model.Session{ID: "other-task", Agent: "claude", State: model.StateIdle, Pane: "%1", TaskID: "T-2", StartedAt: f.Start, UpdatedAt: f.Now}
	if err := f.St.UpsertSession(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got, want := lifecycleKills(f), []string{"KillPane %100", "KillPane %101"}; !slices.Equal(got, want) {
		t.Errorf("kills = %q, want %q", got, want)
	}
	left := make([]string, 0, len(f.Fake.Panes))
	for _, p := range f.Fake.Panes {
		left = append(left, p.ID)
	}
	if !slices.Equal(left, []string{"%1", "%2", "%3"}) {
		t.Errorf("panes left = %q, want the three others", left)
	}
}

func TestPauseNeverKillsRecycledPane(t *testing.T) {
	f := lifecycleWithLead(t)
	// tmux restarted after the launch: %100 is now someone else's pane.
	f.Fake.Panes = []tmux.Pane{{ID: "%100", Session: "home", WindowName: "zsh", ServerStart: f.Start.Add(time.Hour)}}
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("kills = %q, want none: %%100 is another pane now", got)
	}
	if len(f.Fake.Panes) != 1 {
		t.Errorf("panes = %+v, want the recycled one kept", f.Fake.Panes)
	}
	if !strings.Contains(out, "No agent pane of T-1 is open") {
		t.Errorf("output lacks the no-pane line:\n%s", out)
	}
}

func TestPauseRefusesPaneItCannotIdentify(t *testing.T) {
	f := lifecycleSpawned(t, lifecycle.SpawnOptions{})
	// The recorded pane is open, no session reported and the launch claim
	// has lapsed: nothing ties %100 to T-1 any more.
	f.Update("T-1", func(tk *model.Task) { tk.LaunchedAt = time.Time{} })
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
	hooks := filepath.Join(filepath.Dir(f.Repo), "hooks")
	if err = os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(falseBin, filepath.Join(hooks, "pre-commit")); err != nil {
		t.Fatal(err)
	}
	testkit.GitRun(t, f.Repo, "config", "core.hooksPath", hooks)
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(t, filepath.Join(wt, "README"), "staged\n")
	testkit.GitRun(t, wt, "add", "README")
	testkit.WorktreeWrite(t, filepath.Join(wt, "new.txt"), "new\n")

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
	if got := testkit.WorktreeGit(t, wt, "status", "--porcelain"); got != "M  README\n?? new.txt" {
		t.Errorf("worktree status = %q, want README staged and new.txt untracked as before", got)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(wt, "new.txt")); got != "new\n" {
		t.Errorf("new.txt = %q", got)
	}

	out, err = lifecycleRun(t, "pause", "T-1", "--no-verify")
	if err != nil {
		t.Fatalf("pause --no-verify: %v\n%s", err, out)
	}
	if got, want := lifecycleCommits(f), []string{"curia: WIP T-1 (paused)"}; !slices.Equal(got, want) {
		t.Errorf("commits = %q, want %q", got, want)
	}
	if f.Get("T-1").PausedAt.IsZero() || testkit.WorktreeExists(wt) {
		t.Error("pause --no-verify did not pause")
	}
	if !strings.Contains(out, "--no-verify") {
		t.Errorf("output does not say the hooks were skipped:\n%s", out)
	}
}

func TestPauseNeverForcesRemoval(t *testing.T) {
	f := lifecycleWithLead(t)
	// A post-commit hook writes a file after the WIP commit.
	testkit.LifecycleHooks(f.SpawnFixture, "post-commit", "#!/bin/sh\necho late > late.txt\n")
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(t, filepath.Join(wt, "README"), "changed\n")

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
	if got := testkit.LifecycleRead(t, filepath.Join(wt, "late.txt")); got != "late\n" {
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
	testkit.LifecycleHooks(f.SpawnFixture, "post-commit", "#!/bin/sh\necho late > .env\n")
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(t, filepath.Join(wt, "README"), "changed\n")
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(wt, ".env")); got != "late\n" {
		t.Errorf(".env = %q, want kept", got)
	}
	if !strings.Contains(out, "Kept worktree") || f.Get("T-1").PausedAt.IsZero() {
		t.Errorf("want the worktree kept and the task paused:\n%s", out)
	}
}

func TestPausePicksLeadAsResumeSession(t *testing.T) {
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	f := lifecycleSpawned(t, lifecycle.SpawnOptions{})
	teamsWriteCaptured(t, filepath.Join(claude, "teams"))
	f.Fake.Panes = append(f.Fake.Panes, tmux.Pane{ID: "%17", Session: "api-server", WindowName: "T-1"})
	testkit.LifecycleSession(f.SpawnFixture, teamsCapturedLead, "%100", model.StateIdle, f.Now.Add(-time.Minute))
	testkit.LifecycleSession(f.SpawnFixture, "alpha-s", "%17", model.StateIdle, f.Now)

	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := f.Get("T-1").ResumeSession; got != teamsCapturedLead {
		t.Errorf("resume_session = %q, want the lead %s", got, teamsCapturedLead)
	}
	got := lifecycleKills(f)
	slices.Sort(got)
	if want := []string{"KillPane %100", "KillPane %17"}; !slices.Equal(got, want) {
		t.Errorf("kills = %q, want %q", got, want)
	}
}

func TestPauseKeepsNoSessionIDThatIsNotOne(t *testing.T) {
	f := lifecycleSpawned(t, lifecycle.SpawnOptions{})
	testkit.LifecycleSession(f.SpawnFixture, "not-a-claude-id", "%100", model.StateIdle, f.Now)
	out, err := lifecycleRun(t, "pause", "T-1")
	if err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	if got := f.Get("T-1").ResumeSession; got != "" {
		t.Errorf("resume_session = %q, want none", got)
	}
	if !strings.Contains(out, "from the brief") {
		t.Errorf("output does not say resume starts from the brief:\n%s", out)
	}
}

func TestPauseRefusesWhenNotSpawnedOrAlreadyPaused(t *testing.T) {
	f := lifecycleWithLead(t)
	f.Task("Never spawned", "")
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
	wt := f.Worktree("T-1")
	moved := filepath.Join(filepath.Dir(f.Repo), "moved")
	testkit.GitRun(t, f.Repo, "worktree", "move", wt, moved)
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
	if got := testkit.WorktreeGit(t, moved, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("moved worktree HEAD = %q", got)
	}
	tk := f.Get("T-1")
	if !tk.PausedAt.IsZero() || tk.Branch != lifecycleBranch {
		t.Errorf("task = %+v, want unchanged", tk)
	}

	// A plain directory that is not a worktree is refused too.
	if err := os.Remove(wt); err != nil {
		t.Fatal(err)
	}
	testkit.WorktreeWrite(t, filepath.Join(wt, "mine.txt"), "mine\n")
	if _, err := lifecycleRun(t, "clean", "T-1", "--force"); err == nil || !strings.Contains(err.Error(), "leaves it alone") {
		t.Errorf("clean over a foreign directory = %v, want a refusal", err)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(wt, "mine.txt")); got != "mine\n" {
		t.Errorf("mine.txt = %q", got)
	}
}

func TestLifecycleRefusesWhileSpawnUnderWay(t *testing.T) {
	f := lifecycleSpawned(t, lifecycle.SpawnOptions{})
	// The launch claim is fresh, no agent has reported and no window is
	// open: a spawn (its setup script) may be running in the worktree.
	f.Fake.Panes = nil
	for _, args := range [][]string{{"pause", "T-1", "--force"}, {"clean", "T-1", "--force"}} {
		if _, err := lifecycleRun(t, args...); err == nil || !strings.Contains(err.Error(), "under way") {
			t.Errorf("%v = %v, want a refusal", args, err)
		}
	}
	lifecycleWantKept(f)

	f.Now = f.Now.Add(lifecycle.SpawnSetupLimit + 2*time.Minute)
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause after the claim lapsed: %v\n%s", err, out)
	}
}

// lifecyclePaused is lifecycleWithLead after a change to README and a pause.
func lifecyclePaused(t *testing.T, o lifecycle.SpawnOptions, lead bool) *spawnFixture {
	t.Helper()
	f := lifecycleSpawned(t, o)
	if lead {
		testkit.LifecycleSession(f.SpawnFixture, testkit.LifecycleLead, "%100", model.StateIdle, f.Now)
	}
	testkit.WorktreeWrite(t, filepath.Join(f.Worktree("T-1"), "README"), "work in progress\n")
	if out, err := lifecycleRun(t, "pause", "T-1"); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	}
	return f
}

func TestResumeRecreatesWorktreeAndResumes(t *testing.T) {
	f := lifecyclePaused(t, lifecycle.SpawnOptions{}, true)
	wt := f.Worktree("T-1")
	out, err := lifecycleRun(t, "resume", "T-1")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if got := testkit.WorktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("worktree HEAD = %q", got)
	}
	if got := testkit.WorktreeGit(t, wt, "log", "-1", "--format=%s"); got != "curia: WIP T-1 (paused)" {
		t.Errorf("worktree tip = %q, want the WIP commit", got)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(wt, "README")); got != "work in progress\n" {
		t.Errorf("README = %q", got)
	}
	w := f.Windows()
	want := "NewWindow api-server T-1 " + wt + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1 --resume " + testkit.LifecycleLead + " --after-pause"
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	tk := f.Get("T-1")
	if !tk.PausedAt.IsZero() || tk.Pane != "%101" || tk.LaunchedAt.IsZero() {
		t.Errorf("task = %+v, want not paused, pane %%101, launched", tk)
	}
	lifecycleWantOrder(t, out, "session 1a2b3c4d", "Worktree: added "+wt+" on branch "+lifecycleBranch+" again (the branch exists, with its commits)",
		"T-1 launched in api-server:T-1 (%101)")
}

func TestResumeWithoutSessionStartsFresh(t *testing.T) {
	f := lifecyclePaused(t, lifecycle.SpawnOptions{}, false)
	if got := f.Get("T-1").ResumeSession; got != "" {
		t.Fatalf("resume_session = %q, want none", got)
	}
	out, err := lifecycleRun(t, "resume", "T-1")
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	w := f.Windows()
	want := "NewWindow api-server T-1 " + f.Worktree("T-1") + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1 --after-pause"
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	if !strings.Contains(out, "from the brief") {
		t.Errorf("output does not say it starts from the brief:\n%s", out)
	}
	if !f.Get("T-1").PausedAt.IsZero() {
		t.Error("still paused")
	}
}

func TestResumeKeepsModelAndTeam(t *testing.T) {
	f := lifecyclePaused(t, lifecycle.SpawnOptions{Model: "opus", Team: true}, true)
	if out, err := lifecycleRun(t, "resume", "T-1"); err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	w := f.Windows()
	want := "NewWindow api-server T-1 " + f.Worktree("T-1") + " CURIA_TASK_ID=T-1 " + spawnTestBin +
		" launch T-1 --model opus --team --resume " + testkit.LifecycleLead + " --after-pause"
	if len(w) != 2 || w[1] != want {
		t.Errorf("windows = %q, want the second %q", w, want)
	}
	if tk := f.Get("T-1"); tk.AgentModel != "opus" || !tk.Team {
		t.Errorf("task = %+v, want model opus and team kept", tk)
	}
}

func TestResumeRefusesWhenNotPaused(t *testing.T) {
	f := lifecycleWithLead(t)
	if _, err := lifecycleRun(t, "resume", "T-1"); err == nil || !strings.Contains(err.Error(), "not paused") {
		t.Errorf("resume = %v, want not paused", err)
	}
	if w := f.Windows(); len(w) != 1 {
		t.Errorf("windows = %q, want only the spawn's", w)
	}
}

func TestResumeRefusesWhenBranchGone(t *testing.T) {
	f := lifecyclePaused(t, lifecycle.SpawnOptions{}, true)
	testkit.GitRun(t, f.Repo, "branch", "-D", lifecycleBranch)
	if _, err := lifecycleRun(t, "resume", "T-1"); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Errorf("resume = %v, want the branch is gone", err)
	}
	if testkit.WorktreeExists(f.Worktree("T-1")) || len(f.Windows()) != 1 {
		t.Error("resume created a worktree or a window")
	}
	if f.Get("T-1").PausedAt.IsZero() {
		t.Error("no longer paused")
	}
}

func TestResumeFailureStaysPaused(t *testing.T) {
	f := lifecyclePaused(t, lifecycle.SpawnOptions{}, true)
	f.Fake.NewWindowErr = errors.New("no server")
	_, err := lifecycleRun(t, "resume", "T-1")
	if err == nil || !strings.Contains(err.Error(), "paused again") {
		t.Fatalf("resume = %v, want a failure that says T-1 is paused again", err)
	}
	tk := f.Get("T-1")
	if tk.PausedAt.IsZero() || tk.ResumeSession != testkit.LifecycleLead {
		t.Errorf("task = %+v, want still paused with its session", tk)
	}
	// The retry reuses the worktree the failed resume made.
	f.Fake.NewWindowErr = nil
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
	f.T.Helper()
	testkit.LifecycleSession(f.SpawnFixture, testkit.LifecycleLead, "%100", model.StateEnded, f.Now)
	f.Fake.Panes = nil
}

// lifecycleCommitOnBranch commits a change to README in T-1's worktree.
func lifecycleCommitOnBranch(f *spawnFixture, subject string) {
	f.T.Helper()
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(f.T, filepath.Join(wt, "README"), subject+"\n")
	testkit.GitRun(f.T, wt, "commit", "-q", "-am", subject)
}

func TestCleanRemovesMergedBranchAndWorktree(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleCommitOnBranch(f, "the fix")
	testkit.GitRun(t, f.Repo, "merge", "-q", "--no-ff", "--no-edit", lifecycleBranch)
	mainHead := testkit.WorktreeGit(t, f.Repo, "rev-parse", "main")
	testkit.WorktreeWrite(t, filepath.Join(f.Worktree("T-1"), "node_modules", "x.js"), "x\n")
	lifecycleEnded(f)

	// The ignored node_modules/ is named and kept without --force (Ruling 111).
	if _, err := lifecycleRun(t, "clean", "T-1"); err == nil || !strings.Contains(err.Error(), "node_modules/") {
		t.Fatalf("clean without --force = %v, want a refusal naming node_modules/", err)
	}
	out, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	if testkit.WorktreeExists(f.Worktree("T-1")) {
		t.Error("worktree still on disk")
	}
	if ok, err := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+lifecycleBranch); err != nil || ok {
		t.Errorf("branch exists %v (%v), want deleted", ok, err)
	}
	if got := testkit.WorktreeGit(t, f.Repo, "rev-parse", "main"); got != mainHead {
		t.Errorf("main moved to %s", got)
	}
	tk := f.Get("T-1")
	if !tk.CleanedAt.Equal(f.Now) || tk.Branch != "" || tk.Worktree != "" || tk.Pane != "" ||
		tk.ResumeSession != "" || !tk.PausedAt.IsZero() || tk.Status != model.TaskDropped {
		t.Errorf("task = %+v, want cleaned, fields cleared, status dropped", tk)
	}
	lifecycleWantOrder(t, out, "merged into main", "Removed worktree", "1 ignored", "node_modules/",
		"Deleted branch "+lifecycleBranch, "Cleaned T-1")
}

// Live check, D4: cleaning a task that is done says its worktree was cleaned,
// not that the task was marked dropped as if it had been abandoned.
func TestCleanDoneTaskSaysCleaned(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleCommitOnBranch(f, "the fix")
	testkit.GitRun(t, f.Repo, "merge", "-q", "--no-ff", "--no-edit", lifecycleBranch)
	ctx := context.Background()
	if _, _, err := f.St.CompleteTask(ctx, model.Review{TaskID: "T-1", SessionID: "s1", Summary: "done", Tests: model.TestsPassed, CreatedAt: f.Now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.St.CloseReview(ctx, "R-1", store.ReviewClose{Status: model.ReviewMerged, How: model.ApproveMerge}, f.Now); err != nil {
		t.Fatal(err)
	}
	lifecycleEnded(f)
	out, err := lifecycleRun(t, "clean", "T-1")
	if err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	if strings.Contains(out, "marked dropped") || !strings.Contains(out, "Cleaned T-1 (done: its worktree and branch are removed") {
		t.Errorf("clean of a done task printed:\n%s\nwant it to say the done task's worktree was cleaned", out)
	}
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
	if !testkit.WorktreeExists(f.Worktree("T-1")) {
		t.Error("worktree removed")
	}
	if ok, _ := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+lifecycleBranch); !ok {
		t.Error("branch deleted")
	}

	out, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err != nil {
		t.Fatalf("clean --force: %v\n%s", err, out)
	}
	if testkit.WorktreeExists(f.Worktree("T-1")) {
		t.Error("worktree still on disk")
	}
	if ok, _ := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+lifecycleBranch); ok {
		t.Error("branch still there")
	}
	lifecycleWantOrder(t, out, "not merged", "--force", "lost", "unmerged work", "Removed worktree", "Deleted branch")
}

func TestCleanForceNeverForcesWorktreeRemoval(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleEnded(f)
	testkit.WorktreeWrite(t, filepath.Join(f.Worktree("T-1"), "draft.txt"), "draft\n")
	_, err := lifecycleRun(t, "clean", "T-1", "--force")
	if err == nil || !strings.Contains(err.Error(), "never forces") {
		t.Fatalf("clean --force of a dirty worktree = %v, want git's refusal", err)
	}
	if got := testkit.LifecycleRead(t, filepath.Join(f.Worktree("T-1"), "draft.txt")); got != "draft\n" {
		t.Errorf("draft.txt = %q", got)
	}
	if ok, _ := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+lifecycleBranch); !ok {
		t.Error("branch deleted although the worktree stayed")
	}
	if !f.Get("T-1").CleanedAt.IsZero() {
		t.Error("cleaned_at set")
	}
}

func TestCleanNeverTouchesBase(t *testing.T) {
	f := lifecycleWithLead(t)
	lifecycleEnded(f)
	f.Update("T-1", func(tk *model.Task) { tk.Branch = "main" })
	if _, err := lifecycleRun(t, "clean", "T-1", "--force"); err == nil || !strings.Contains(err.Error(), "base branch") {
		t.Errorf("clean of the base = %v, want a refusal", err)
	}
	if ok, _ := gitops.RefExists(context.Background(), f.Repo, "refs/heads/main"); !ok {
		t.Error("main deleted")
	}
	if !testkit.WorktreeExists(f.Worktree("T-1")) {
		t.Error("worktree removed")
	}
}

func TestCleanRefusesWithLiveAgent(t *testing.T) {
	f := lifecycleWithLead(t)
	if _, err := lifecycleRun(t, "clean", "T-1"); err == nil || !strings.Contains(err.Error(), "live session") {
		t.Errorf("clean with a live session = %v, want a refusal", err)
	}
	// The session ended but its window is still open.
	testkit.LifecycleSession(f.SpawnFixture, testkit.LifecycleLead, "%100", model.StateEnded, f.Now)
	if _, err := lifecycleRun(t, "clean", "T-1"); err == nil || !strings.Contains(err.Error(), "still open") {
		t.Errorf("clean with the window open = %v, want a refusal", err)
	}
	if !testkit.WorktreeExists(f.Worktree("T-1")) {
		t.Error("worktree removed")
	}
	if ok, _ := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+lifecycleBranch); !ok {
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
	if ok, _ := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+lifecycleBranch); ok {
		t.Error("branch still there")
	}
	tk := f.Get("T-1")
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
	testkit.GitRun(t, f.Repo, "merge", "-q", "--no-ff", "--no-edit", lifecycleBranch)
	lifecycleEnded(f)
	if out, err := lifecycleRun(t, "clean", "T-1"); err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	// Clean marks the task dropped: nothing spawns it again by accident.
	if _, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err == nil || !strings.Contains(err.Error(), "T-1 is dropped") {
		t.Fatalf("spawn right after clean = %v, want T-1 is dropped", err)
	}
	f.Update("T-1", func(tk *model.Task) { tk.Status = model.TaskReady }) // the user brings it back
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn after clean: %v\n%s", err, out)
	}
	wt := f.Worktree("T-1")
	if got, want := testkit.WorktreeGit(t, wt, "rev-parse", "HEAD"), testkit.WorktreeGit(t, f.Repo, "rev-parse", "main"); got != want {
		t.Errorf("new worktree at %s, want main's %s", got, want)
	}
	if got := testkit.WorktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+lifecycleBranch {
		t.Errorf("worktree HEAD = %q", got)
	}
	if tk := f.Get("T-1"); tk.Branch != lifecycleBranch || tk.Worktree != wt {
		t.Errorf("task = %+v, want branch and worktree recorded again", tk)
	}
}

func TestPauseNeverKillsOtherServersPane(t *testing.T) {
	f := lifecycleWithLead(t)
	lead, found, err := f.St.GetSession(context.Background(), testkit.LifecycleLead)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	lead.Socket = "/tmp/tmux-501/first"
	if err = f.St.UpsertSession(context.Background(), lead); err != nil {
		t.Fatal(err)
	}
	for i := range f.Fake.Panes {
		f.Fake.Panes[i].Socket = "/tmp/tmux-501/second"
	}
	_, err = lifecycleRun(t, "pause", "T-1")
	if err == nil || !strings.Contains(err.Error(), "of another tmux server (first): run curia pause from that server") ||
		!strings.Contains(err.Error(), "Nothing was done") {
		t.Fatalf("pause = %v, want the other-server refusal", err)
	}
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("kills = %q, want none", got)
	}
	lifecycleWantKept(f)
	out, _ := lifecycleRun(t, "pause", "T-1", "--force")
	if got := lifecycleKills(f); len(got) != 0 {
		t.Errorf("--force: kills = %q, want none\n%s", got, out)
	}
}

// The same with no live session yet: the task's own record of its window's
// server tells the panes apart. A task with no recorded server (an older
// row) is unknown, never "the same".
func TestPauseNeverKillsOtherServersPaneWithoutSession(t *testing.T) {
	for _, c := range []struct{ name, recorded, want string }{
		{"recorded", "", "of another tmux server (first)"},
		{"older row", "-", "may still be open on another server"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := lifecycleSpawned(t, lifecycle.SpawnOptions{})
			if c.recorded == "-" {
				f.Update("T-1", func(tk *model.Task) { tk.PaneSocket = "" })
			}
			for i := range f.Fake.Panes {
				f.Fake.Panes[i].Socket = "/tmp/tmux-501/second"
			}
			f.Now = f.Now.Add(time.Hour) // the launch claim has run out
			_, err := lifecycleRun(t, "pause", "T-1")
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "Nothing was done") {
				t.Fatalf("pause = %v, want a refusal with %q", err, c.want)
			}
			out, _ := lifecycleRun(t, "pause", "T-1", "--force")
			if got := lifecycleKills(f); len(got) != 0 {
				t.Errorf("kills = %q, want none\n%s", got, out)
			}
		})
	}
}

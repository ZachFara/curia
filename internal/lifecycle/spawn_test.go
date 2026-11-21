package lifecycle_test

import (
	"bytes"
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

	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/tmux"
)

func TestSpawnNeverPassesTaskTextToTmux(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("kill ; $(touch x) ;", "a;\n$(rm -rf ~) `id` ;\nkill-server ;\n#{pane_id} %H")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	w := f.Windows()
	if len(w) != 1 {
		t.Fatalf("windows = %q, want one", w)
	}
	want := "NewWindow api-server T-1 " + f.Worktree("T-1") + " CURIA_TASK_ID=T-1 " + testkit.SpawnTestBin + " launch T-1"
	if w[0] != want {
		t.Errorf("NewWindow call = %q, want %q", w[0], want)
	}
	for _, bad := range []string{";", "$", "`", "\n", "kill", "touch", "#{"} {
		if strings.Contains(w[0], bad) {
			t.Errorf("NewWindow call holds %q from the task text: %q", bad, w[0])
		}
	}
}

func TestSpawnRefusesWhenRunning(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Busy", "")
	f.Fake.Panes = []tmux.Pane{{ID: "%5", Session: "api-server", WindowID: "@3", WindowName: "T-1"}}
	sess := model.Session{
		ID: "s-live", Agent: "claude", State: model.StateWorking, Pane: "%5", TmuxSession: "api-server",
		TmuxWindow: "@3", TaskID: "T-1", StartedAt: f.Now.Add(-time.Minute), UpdatedAt: f.Now.Add(-time.Minute),
	}
	if err := f.St.UpsertSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "api-server:T-1") {
		t.Fatalf("spawn with a live session = %v, want a refusal naming api-server:T-1", err)
	}
	f.WantNothingStarted("T-1")

	// The session ends; the pane recorded on the task is still open.
	sess.State, sess.UpdatedAt = model.StateEnded, f.Now
	if err = f.St.UpsertSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	f.Update("T-1", func(tk *model.Task) { tk.Pane = "%5" })
	_, _, err = f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "api-server:T-1") {
		t.Fatalf("spawn with the recorded pane open = %v, want a refusal naming api-server:T-1", err)
	}
	f.WantNothingStarted("T-1")

	// Once the pane is gone, the task spawns.
	f.Fake.Panes = nil
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn after the pane closed: %v\n%s", err, out)
	}
}

func TestSpawnSecondClaimRefused(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Twice", "")
	first := f.Now.Add(-time.Minute)
	if _, err := f.St.ClaimLaunch(context.Background(), "T-1", "", first, time.Minute); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "another spawn of T-1 started at "+first.Local().Format(time.TimeOnly)) {
		t.Fatalf("spawn under another claim = %v", err)
	}
	if w := f.Windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if testkit.WorktreeExists(f.Worktree("T-1")) {
		t.Error("worktree created under another spawn's claim")
	}
	if got := f.Get("T-1").LaunchedAt; !got.Equal(first) {
		t.Errorf("launched_at = %v, want the other spawn's claim %v kept", got, first)
	}

	// Eleven minutes on, the claim is taken as dead.
	f.Now = first.Add(lifecycle.SpawnSetupLimit + time.Minute)
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn after the hold: %v\n%s", err, out)
	}
}

func TestSpawnReusesWorktreeOnRetry(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Retry me", "")
	f.Fake.NewWindowErr = errors.New("tmux broke")
	_, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "tmux broke") {
		t.Fatalf("spawn with a failing NewWindow = %v", err)
	}
	wt := f.Worktree("T-1")
	if !strings.Contains(err.Error(), wt) {
		t.Errorf("error does not say the worktree stays: %v", err)
	}
	if got := f.Get("T-1"); !got.LaunchedAt.IsZero() || got.Worktree != wt || got.Branch == "" {
		t.Errorf("after the failure: %+v, want the claim released and the worktree recorded", got)
	}
	testkit.WorktreeWrite(t, filepath.Join(wt, "work.txt"), "kept\n")

	f.Fake.NewWindowErr = nil
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("retry: %v\n%s", err, out)
	}
	if !strings.Contains(out, "reusing") {
		t.Errorf("retry output does not say it reused the worktree:\n%s", out)
	}
	if !testkit.WorktreeExists(filepath.Join(wt, "work.txt")) {
		t.Error("the retry replaced the worktree")
	}
	if n := len(strings.Split(testkit.WorktreeGit(t, f.Repo, "worktree", "list", "--porcelain"), "\n\n")); n != 2 {
		t.Errorf("git worktree list has %d entries, want 2 (main and T-1)", n)
	}
	if w := f.Windows(); len(w) != 2 {
		t.Errorf("windows = %q, want the failed attempt and the retry", w)
	}
}

func TestSpawnRefusesForeignPath(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Foreign", "")
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(t, filepath.Join(wt, "mine.txt"), "the user's\n")
	_, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "leaves it alone") {
		t.Fatalf("spawn onto an existing directory = %v", err)
	}
	if b, rerr := os.ReadFile(filepath.Clean(filepath.Join(wt, "mine.txt"))); rerr != nil || string(b) != "the user's\n" {
		t.Errorf("the user's file changed: %q, %v", b, rerr)
	}
	if w := f.Windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.Get("T-1").LaunchedAt; !got.IsZero() {
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
	f := testkit.NewSpawnFixture(t)
	f.SetupScript("#!/bin/sh\n"+
		`echo "task=$CURIA_TASK_ID repo=$CURIA_REPO wt=$CURIA_WORKTREE pwd=$(pwd -P)"`+"\n"+
		"echo to-stderr >&2\n"+
		"read line && echo stdin-had-data\n"+
		"touch made-by-setup\n", true)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	wt := f.Worktree("T-1")
	b, err := os.ReadFile(f.SetupLog())
	if err != nil {
		t.Fatal(err)
	}
	want := "task=T-1 repo=" + f.Repo + " wt=" + wt + " pwd=" + wt + "\nto-stderr\n"
	if string(b) != want {
		t.Errorf("setup log = %q, want %q", b, want)
	}
	if fi, err := os.Stat(f.SetupLog()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("setup log mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if strings.Contains(out, "task=T-1") || strings.Contains(out, "to-stderr") {
		t.Errorf("script output reached spawn's output:\n%s", out)
	}
	if !testkit.WorktreeExists(filepath.Join(wt, "made-by-setup")) {
		t.Error("the script did not run in the worktree")
	}
	if len(f.Windows()) != 1 {
		t.Errorf("windows = %q", f.Windows())
	}
}

func spawnSetupNotExecutable(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.SetupScript("#!/bin/sh\ntouch made-by-setup\n", false)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not executable") {
		t.Errorf("output does not say the script was skipped:\n%s", out)
	}
	if testkit.WorktreeExists(filepath.Join(f.Worktree("T-1"), "made-by-setup")) || testkit.WorktreeExists(f.SetupLog()) {
		t.Error("a script without an execute bit ran")
	}
}

func spawnSetupOutside(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	if err := os.MkdirAll(filepath.Join(f.Repo, ".curia"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(f.Repo, ".curia", "setup.sh")); err != nil {
		t.Fatal(err)
	}
	testkit.GitRun(t, f.Repo, "add", "-A")
	testkit.GitRun(t, f.Repo, "commit", "-q", "-m", "link")
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if !strings.Contains(out, "outside the worktree") {
		t.Errorf("output does not say the script was skipped:\n%s", out)
	}
	if testkit.WorktreeExists(f.SetupLog()) {
		t.Error("a script outside the worktree ran")
	}
}

func spawnSetupAbsent(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil || !strings.Contains(out, "no .curia/setup.sh") {
		t.Errorf("spawn = %v, output:\n%s", err, out)
	}
}

func spawnSetupExit3(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.SetupScript("#!/bin/sh\ni=1\nwhile [ $i -le 30 ]; do echo \"line $i\"; i=$((i+1)); done\n"+
		"printf 'evil \\033[31mred\\033]0;title\\007 end\\n'\nexit 3\n", true)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), f.SetupLog()) {
		t.Fatalf("spawn = %v, want a failure naming the log", err)
	}
	if !strings.Contains(out, "line 30") || !strings.Contains(out, "line 12") || strings.Contains(out, "line 11\n") {
		t.Errorf("output does not hold exactly the last 20 lines:\n%s", out)
	}
	if !strings.Contains(out, "evil red end") || strings.ContainsFunc(out, func(r rune) bool { return r < 0x20 && r != '\n' }) {
		t.Errorf("output not cleaned: %q", out)
	}
	if !strings.Contains(out, f.SetupLog()) {
		t.Errorf("output lacks the log path:\n%s", out)
	}
	if w := f.Windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.Get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released", got)
	}
}

func spawnSetupCapped(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.SetupScript("#!/bin/sh\nhead -c 2097152 /dev/zero | tr '\\000' a\n", true)
	f.Task("Setup", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	fi, err := os.Stat(f.SetupLog())
	if err != nil || fi.Size() != 1<<20 {
		t.Errorf("setup log size = %v, %v; want 1 MiB", fi, err)
	}
}

func spawnSetupTimeLimit(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.SetupScript("#!/bin/sh\necho started\nsleep 30\necho never\n", true)
	f.Task("Setup", "")
	lifecycle.SpawnSetupLimit = 100 * time.Millisecond
	begin := time.Now()
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("spawn = %v, want a time-out\n%s", err, out)
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("spawn took %v after the limit", d)
	}
	// sh may not even have echoed within 100 ms; it never got past the sleep.
	if b, _ := os.ReadFile(f.SetupLog()); strings.Contains(string(b), "never") {
		t.Errorf("setup log = %q: the script ran past its limit", b)
	}
	if w := f.Windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
}

func TestSpawnRefusesMissingWorktreeBeforeWindow(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.SetupScript("#!/bin/sh\nrm -rf \"$CURIA_WORKTREE\"\n", true)
	f.Task("Vanishing", "")
	_, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err == nil || !strings.Contains(err.Error(), "no window opened") {
		t.Fatalf("spawn with the worktree gone = %v", err)
	}
	if w := f.Windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.Get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released", got)
	}
}

func TestSpawnWaitsForLink(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Linked", "")
	f.OnSleep = func(n int) {
		if n != 3 {
			return
		}
		done := make(chan error)
		go func() {
			done <- f.St.UpsertSession(context.Background(), model.Session{
				ID: "1a2b3c4d-0000-4000-8000-000000000000", Agent: "claude", State: model.StateIdle, Pane: "%100",
				TaskID: "T-1", StartedAt: f.Now, UpdatedAt: f.Now,
			})
		}()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{Wait: lifecycle.SpawnWait})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if r.Session.ID != "1a2b3c4d-0000-4000-8000-000000000000" || f.Sleeps != 3 {
		t.Errorf("session %q after %d sleeps, want the linked one after 3", r.Session.ID, f.Sleeps)
	}
	if got, want := lifecycle.SpawnLine(r), "T-1 running in api-server:T-1 (%100), session 1a2b3c4d"; got != want {
		t.Errorf("lifecycle.SpawnLine = %q, want %q", got, want)
	}

	f.OnSleep = nil
	f.Sleeps = 0
	f.Task("Silent", "")
	r, out, err = f.Spawn("T-2", lifecycle.SpawnOptions{Wait: lifecycle.SpawnWait})
	if err != nil {
		t.Fatalf("spawn T-2: %v\n%s", err, out)
	}
	if r.Session.ID != "" || f.Sleeps != int(lifecycle.SpawnWait/lifecycle.SpawnPoll) {
		t.Errorf("session %q after %d sleeps, want none after 80", r.Session.ID, f.Sleeps)
	}
	want := "T-2 launched in api-server:T-2 (%101); the agent has not reported yet (look at the window: it may be showing a startup dialog)"
	if got := lifecycle.SpawnLine(r); got != want {
		t.Errorf("lifecycle.SpawnLine = %q, want %q", got, want)
	}
}

func TestSpawnReportsAgentThatExited(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Untrusted", "")
	f.OnSleep = func(int) { f.Fake.Panes = nil } // the agent exited and its window closed
	_, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{Wait: lifecycle.SpawnWait})
	if err == nil || !strings.Contains(err.Error(), "exited before it reported") || !strings.Contains(err.Error(), "trust") {
		t.Fatalf("spawn = %v, want a plain report that the agent exited", err)
	}
	if got := f.Get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released so a retry is not refused", got)
	}
}

func TestSpawnWaitZeroReturnsAtOnce(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Now", "")
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if f.Sleeps != 0 || r.Session.ID != "" || r.Pane.ID != "%100" || r.Task.Pane != "%100" {
		t.Errorf("result %+v after %d sleeps", r, f.Sleeps)
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
	f := testkit.NewSpawnFixture(t)
	marker := filepath.Join(t.TempDir(), "marker")
	// bin is a symlink to /bin/sh, so the window's `<bin> launch T-1` runs
	// the script ./launch of the worktree with T-1 as $1.
	testkit.WorktreeWrite(t, filepath.Join(f.Repo, "launch"),
		"pwd -P > '"+marker+"'\nprintf '%s\\n' \"$1\" \"$CURIA_TASK_ID\" >> '"+marker+"'\nexec sleep 600\n")
	testkit.GitRun(t, f.Repo, "add", "-A")
	testkit.GitRun(t, f.Repo, "commit", "-q", "-m", "launch stub")
	bin := filepath.Join(t.TempDir(), "curia")
	if err := os.Symlink("/bin/sh", bin); err != nil {
		t.Fatal(err)
	}
	lifecycle.SpawnBin = func() (string, error) { return bin, nil }

	raw := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", spawnRealSocket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() { testkit.SpawnKillServer(spawnRealSocket) })
	_, _ = raw("kill-server")
	if out, err := raw("new-session", "-d", "-s", "keep"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	f.Task("Real", "")
	r, err := lifecycle.SpawnTask(ctx, f.St, tmux.NewExec(spawnRealSocket), f.Cfg, "T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if r.Pane.Session != "api-server" || r.Pane.WindowName != "T-1" {
		t.Errorf("pane = %+v, want api-server:T-1", r.Pane)
	}
	wt := f.Worktree("T-1")
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

// testkit.SpawnKillServer stops the private test server on socket and removes its
// socket file, which tmux leaves behind (see killServer in internal/tmux).

func TestSpawnAgainAfterAgentExits(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Again", "")
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("first spawn: %v\n%s", err, out)
	}
	sess := model.Session{
		ID: "1a2b3c4d-0000-4000-8000-000000000000", Agent: "claude", State: model.StateIdle, Pane: r.Pane.ID,
		TmuxSession: "api-server", TaskID: "T-1", StartedAt: f.Now, UpdatedAt: f.Now,
	}
	if err = f.St.UpsertSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	// While the agent's window is open, a second spawn is refused for that.
	f.Now = f.Now.Add(time.Minute)
	if _, _, err = f.Spawn("T-1", lifecycle.SpawnOptions{}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("spawn while the agent runs = %v, want already running", err)
	}
	// The agent exits and its window closes: the task spawns again at once.
	f.Fake.Panes = nil
	f.Now = f.Now.Add(time.Minute)
	r2, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn after the agent exited: %v\n%s", err, out)
	}
	if r2.Pane.ID == r.Pane.ID || f.Get("T-1").Pane != r2.Pane.ID || !f.Get("T-1").LaunchedAt.Equal(f.Now) {
		t.Errorf("second spawn: pane %s, task %+v; want a new window recorded and launched at %v", r2.Pane.ID, f.Get("T-1"), f.Now)
	}
	if w := f.Windows(); len(w) != 2 {
		t.Errorf("windows = %q, want two", w)
	}
}

func TestSpawnIgnoresRecycledPaneID(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Recycled", "")
	launched := f.Now.Add(-time.Hour)
	f.Update("T-1", func(tk *model.Task) { tk.Pane, tk.LaunchedAt = "%0", launched })
	// tmux restarted after the launch, and %0 is now the user's own shell.
	f.Fake.Panes = []tmux.Pane{{ID: "%0", Session: "base", WindowName: "bash", ServerStart: launched.Add(time.Minute)}}
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn with a recycled pane id: %v\n%s", err, out)
	}
	if got := f.Get("T-1").Pane; got == "%0" {
		t.Errorf("pane = %s, want the new window", got)
	}
	// The same pane on a server that started before the launch is the task's.
	f.Task("Ours", "")
	f.Update("T-2", func(tk *model.Task) { tk.Pane, tk.LaunchedAt = "%0", launched })
	f.Fake.Panes = []tmux.Pane{{ID: "%0", Session: "api-server", WindowName: "T-2", ServerStart: launched.Add(-time.Minute)}}
	if _, _, err := f.Spawn("T-2", lifecycle.SpawnOptions{}); err == nil || !strings.Contains(err.Error(), "still open") {
		t.Fatalf("spawn with the task's own pane open = %v, want still open", err)
	}
}

// spawnAgentProbe is a tmux client that reads the task's stored agent at
// the moment the window opens: curia launch reads it from the store as
// soon as the window runs, so it must be stored by then.
type spawnAgentProbe struct {
	*tmux.Fake
	st     *store.Store
	seen   []string
	models []string // the stored model at the same moment
}

func TestSpawnStoresAgentBeforeWindow(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Racy", "")
	probe := &spawnAgentProbe{Fake: f.Fake, st: f.St}
	if _, err := lifecycle.SpawnTask(context.Background(), f.St, probe, f.Cfg, "T-1", lifecycle.SpawnOptions{Agent: model.AgentCodex}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if !slices.Equal(probe.seen, []string{model.AgentCodex}) {
		t.Errorf("agent stored when the window opened = %q, want codex", probe.seen)
	}
}

// TestSpawnRefusesOptionLikeValues: a model or session id that starts with
// a dash would reach the agent's argument list as an option.
func TestSpawnRefusesOptionLikeValues(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Flags", "")
	for _, o := range []lifecycle.SpawnOptions{
		{Model: "--dangerously-bypass-approvals-and-sandbox"},
		{Model: "-s", Agent: model.AgentCodex},
		{Model: "--sandbox", Agent: model.AgentCodex},
		{Resume: "------------------------------------"},
		{Resume: "-0f8fad5b-d9cb-469f-a165-70867728950"},
	} {
		if _, out, err := f.Spawn("T-1", o); err == nil {
			t.Errorf("spawn %+v succeeded:\n%s", o, out)
		}
		f.WantNothingStarted("T-1")
	}
	// A task whose stored model starts with a dash is refused too.
	f.Update("T-1", func(tk *model.Task) { tk.AgentModel = "-s" })
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err == nil {
		t.Errorf("spawn with a stored model -s succeeded:\n%s", out)
	}
	f.WantNothingStarted("T-1")
}

func TestSpawnStoredModelOnlyWithItsAgent(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Switch", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{Model: "sonnet"}); err != nil {
		t.Fatalf("first spawn: %v\n%s", err, out)
	}
	if tk := f.Get("T-1"); tk.AgentModel != "sonnet" || tk.Agent != model.AgentClaude {
		t.Fatalf("stored %q/%q, want claude/sonnet (a model the user chose is stored)", tk.Agent, tk.AgentModel)
	}
	f.Fake.Panes, f.Fake.Calls = nil, nil
	f.Now = f.Now.Add(time.Minute)
	probe := &spawnAgentProbe{Fake: f.Fake, st: f.St}
	var out bytes.Buffer
	r, err := lifecycle.SpawnTask(context.Background(), f.St, probe, f.Cfg, "T-1", lifecycle.SpawnOptions{Agent: model.AgentCodex, Out: &out})
	if err != nil {
		t.Fatalf("codex spawn: %v\n%s", err, out.String())
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) {
		t.Errorf("windows = %q, want no --model for codex", got)
	}
	note := testkit.SpawnOtherAgentNote("T-1", "sonnet", "claude", "codex")
	if r.Note != note || strings.Count(out.String(), note) != 1 {
		t.Errorf("note %q, output:\n%s\nwant %q once", r.Note, out.String(), note)
	}
	if !slices.Equal(probe.seen, []string{model.AgentCodex}) || !slices.Equal(probe.models, []string{""}) {
		t.Errorf("stored when the window opened: agent %q model %q; want codex and no model", probe.seen, probe.models)
	}
	if tk := f.Get("T-1"); tk.Agent != model.AgentCodex || tk.AgentModel != "" {
		t.Errorf("stored %q/%q, want codex and no model", tk.Agent, tk.AgentModel)
	}
}

// TestSpawnCodexModelNeverReachesClaude: a model the user chose for codex is
// stored for codex, and a later claude spawn under a warning gets the
// cheaper model, not codex's, and stores none.

// TestSpawnHostileStoredModelOfOtherAgent: a stored model for the other
// agent never reaches the window, whatever it holds, and the line that
// names it is cleaned.
func TestSpawnHostileStoredModelOfOtherAgent(t *testing.T) {
	for _, bad := range []string{"-s", "--dangerously-bypass-approvals-and-sandbox", "\x1b[31mopus\nnext", "x;rm -rf ~", "sonnet"} {
		t.Run(bad, func(t *testing.T) {
			f := testkit.NewSpawnFixture(t)
			f.Task("Mixed", "")
			f.Update("T-1", func(tk *model.Task) { tk.Agent, tk.AgentModel = model.AgentClaude, bad })
			r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{Agent: model.AgentCodex})
			if err != nil {
				t.Fatalf("codex spawn: %v\n%s", err, out)
			}
			if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) {
				t.Errorf("windows = %q, want [<bin> launch T-1] and nothing of the stored model", got)
			}
			if strings.ContainsFunc(out+r.Note, func(c rune) bool { return c < 0x20 && c != '\n' }) || strings.Contains(r.Note, "\n") {
				t.Errorf("note not cleaned: %q", r.Note)
			}
			if tk := f.Get("T-1"); tk.Agent != model.AgentCodex || tk.AgentModel != "" {
				t.Errorf("stored %q/%q, want codex and no model", tk.Agent, tk.AgentModel)
			}
		})
	}
}

// An agent may spawn a ready task but not choose how it runs (Ruling 113).

func TestSpawnByAgentRefusesHowItRuns(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Fix", "Body")
	f.Update("T-1", func(tk *model.Task) { tk.Status = model.TaskReady })
	before := f.Get("T-1")
	for opt, o := range map[string]lifecycle.SpawnOptions{
		"--agent":  {Agent: model.AgentCodex},
		"--model":  {Model: "gpt-expensive"},
		"--team":   {Team: true},
		"--resume": {Resume: "0b9a2c1e-0000-4000-8000-000000000000"},
	} {
		o.ReadyOnly = true
		f.Fake.Calls = nil
		_, _, err := f.Spawn("T-1", o)
		if err == nil || !strings.Contains(err.Error(), opt+" is the user's choice") || !strings.Contains(err.Error(), "stored values apply") {
			t.Errorf("%s: err = %v, want a refusal naming it and the stored values", opt, err)
		}
		f.WantNothingStarted("T-1")
	}
	if tk := f.Get("T-1"); tk.Agent != before.Agent || tk.AgentModel != before.AgentModel {
		t.Errorf("stored agent %q model %q, want unchanged", tk.Agent, tk.AgentModel)
	}
}

// --agent codex without codex on PATH is refused before anything is stored:
// the task keeps its agent and its model.

func TestSpawnCodexNotInstalledStoresNothing(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Task("Fix", "Body")
	f.Update("T-1", func(tk *model.Task) { tk.AgentModel = "opus" })
	lifecycle.LaunchLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	_, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{Agent: model.AgentCodex})
	if err == nil || !strings.Contains(err.Error(), "codex is not installed") || !strings.Contains(err.Error(), "--agent claude") {
		t.Fatalf("err = %v, want codex is not installed, with --agent claude", err)
	}
	f.WantNothingStarted("T-1")
	if tk := f.Get("T-1"); tk.Agent == model.AgentCodex || tk.AgentModel != "opus" {
		t.Errorf("stored agent %q model %q, want unchanged", tk.Agent, tk.AgentModel)
	}
}

func (p *spawnAgentProbe) NewWindow(ctx context.Context, w tmux.WindowSpec) (tmux.Pane, error) {
	tk, _, err := p.st.GetTask(ctx, w.Name)
	if err != nil {
		return tmux.Pane{}, err
	}
	p.seen = append(p.seen, tk.Agent)
	p.models = append(p.models, tk.AgentModel)
	return p.Fake.NewWindow(ctx, w)
}

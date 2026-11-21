package lifecycle_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/tmux"
)

// lifecycleRealSocket is the private tmux server of TestPauseInRealTmuxAndGit.
var lifecycleRealSocket = fmt.Sprintf("curia-test-%d-lc", os.Getpid())

func TestPauseInRealTmuxAndGit(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := testkit.NewSpawnFixture(t)
	// bin is a symlink to /bin/sh, so the window's `<bin> launch T-1` runs
	// the worktree's ./launch, which stands in for the agent.
	testkit.WorktreeWrite(t, filepath.Join(f.Repo, "launch"), "exec sleep 600\n")
	testkit.GitRun(t, f.Repo, "add", "-A")
	testkit.GitRun(t, f.Repo, "commit", "-q", "-m", "launch stub")
	bin := filepath.Join(t.TempDir(), "curia")
	if err := os.Symlink("/bin/sh", bin); err != nil {
		t.Fatal(err)
	}
	lifecycle.SpawnBin = func() (string, error) { return bin, nil }
	raw := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", lifecycleRealSocket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	t.Cleanup(func() { testkit.SpawnKillServer(lifecycleRealSocket) })
	_, _ = raw("kill-server")
	if out, err := raw("new-session", "-d", "-s", "keep"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	keep, err := raw("display-message", "-p", "-t", "=keep:", "#{pane_id}")
	if err != nil {
		t.Fatalf("keep pane: %v: %s", err, keep)
	}
	// The real server's start time is real, so the clock must be too.
	f.Now = time.Now().UTC()
	f.Start = f.Now
	tm := tmux.NewExec(lifecycleRealSocket)
	f.Task("Real", "")
	r, err := lifecycle.SpawnTask(ctx, f.St, tm, f.Cfg, "T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	testkit.LifecycleSession(f, testkit.LifecycleLead, r.Pane.ID, model.StateIdle, f.Now)
	wt := f.Worktree("T-1")
	testkit.WorktreeWrite(t, filepath.Join(wt, "README"), "changed\n")

	var out strings.Builder
	if err = lifecycle.PauseTask(ctx, f.St, tm, f.Cfg, "T-1", lifecycle.PauseOptions{}, &out); err != nil {
		t.Fatalf("pause: %v\n%s", err, out.String())
	}
	panes, err := raw("list-panes", "-a", "-F", "#{pane_id}")
	if err != nil || panes != keep {
		t.Errorf("panes left = %q (%v), want only the keep session's %s", panes, err, keep)
	}
	if testkit.WorktreeExists(wt) {
		t.Error("worktree still on disk")
	}
	if got := f.Get("T-1"); got.PausedAt.IsZero() || got.ResumeSession != testkit.LifecycleLead {
		t.Errorf("task = %+v, want paused with the lead's session", got)
	}
	if got := testkit.WorktreeGit(t, f.Repo, "log", "-1", "--format=%s", "curia/T-1-real"); got != "curia: WIP T-1 (paused)" {
		t.Errorf("branch tip = %q, want the WIP commit", got)
	}
}

// TestPauseNeverKillsOtherServersPane: pause run from a second tmux server
// whose %100 has the id of the agent's pane on the first server neither
// kills that pane nor takes the agent for stopped: it refuses, naming the
// other server, and with --force it still kills nothing.

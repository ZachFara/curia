package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realSocket is per run, so concurrent test runs (several worktrees running
// make check) never kill each other's servers.
var realSocket = fmt.Sprintf("curia-test-%d", os.Getpid())

// realTmux starts a throwaway server with no configuration on its own socket
// (never the default server), with dir as the working directory of every
// tmux command, and returns a helper that runs tmux commands against it.
func realTmux(ctx context.Context, t *testing.T, dir string) func(args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tm := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", realSocket}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// Cleanup runs after the caller's deferred cancel, so it must not use ctx.
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", realSocket, "kill-server").Run() })
	_, _ = tm("kill-server") // clear a leftover server from an aborted run
	if out, err := tm("-f", os.DevNull, "new-session", "-d", "-s", "t"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	return tm
}

// sourceSnippet writes the snippet for bin into dir and sources it times
// times, failing on any error or output.
func sourceSnippet(t *testing.T, tm func(...string) (string, error), dir, bin string, times int) {
	t.Helper()
	snippet := filepath.Join(dir, "curia.tmux")
	if err := WriteTmux(snippet, filepath.Join(dir, ".tmux.conf"), bin); err != nil {
		t.Fatal(err)
	}
	for range times {
		if out, err := tm("source-file", snippet); err != nil || out != "" {
			t.Fatalf("source-file: %v: %q\n%s", err, out, readFile(t, snippet))
		}
	}
}

func TestSnippetInRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	if out, err := tm("set", "-g", "focus-events", "off"); err != nil {
		t.Fatalf("set focus-events: %v: %s", err, out)
	}

	// A user re-sources ~/.tmux.conf after every edit.
	sourceSnippet(t, tm, dir, testBin, 2)

	if out, err := tm("show", "-gv", "focus-events"); err != nil || strings.TrimSpace(out) != "on" {
		t.Errorf("focus-events = %q, %v; want on", out, err)
	}
	right, err := tm("show", "-gv", "status-right")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(right, "#("+testBin+" status)"); n != 1 {
		t.Errorf("status-right has %d badges after two sources, want 1: %q", n, right)
	}
}

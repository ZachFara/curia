package install

import (
	"cmp"
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
	t.Cleanup(func() { killServer(realSocket) })
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

// TestSnippetMakesRoomForBadge: tmux's default status-right already fills the
// default status-right-length of 40, which would cut the badge off. The
// snippet raises a smaller length to 80 and keeps a larger one, including
// one that is larger only as a number ("120" sorts before "80" as text).
func TestSnippetMakesRoomForBadge(t *testing.T) {
	for _, tc := range []struct{ preset, want string }{
		{"", "80"}, // tmux's default, 40
		{"79", "80"},
		{"80", "80"},
		{"120", "120"},
	} {
		t.Run("preset "+cmp.Or(tc.preset, "default"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			dir := t.TempDir()
			tm := realTmux(ctx, t, dir)
			if tc.preset != "" {
				if out, err := tm("set", "-g", "status-right-length", tc.preset); err != nil {
					t.Fatalf("set status-right-length: %v: %s", err, out)
				}
			}
			sourceSnippet(t, tm, dir, testBin, 2)
			if out, err := tm("show", "-gv", "status-right-length"); err != nil || strings.TrimSpace(out) != tc.want {
				t.Errorf("status-right-length = %q, %v; want %s", out, err, tc.want)
			}
		})
	}
}

// TestSnippetQuotedBinaryInRealTmux sources the snippet for a binary path with
// a space, a dollar and a single quote, then runs the commands that reach the shell
// through every quoting layer. The "binary" is a symlink to /bin/sh, so
// `<bin> seen %0` runs the script file ./seen, which the test writes; tmux runs
// its commands in dir, the directory the server was started from.
func TestSnippetQuotedBinaryInRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	binDir := filepath.Join(dir, "my $HOME tools", "it's")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "curia")
	if err := os.Symlink("/bin/sh", bin); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	for _, name := range []string{"seen", "status"} {
		script := "printf '%s\\n' \"" + name + " $*\" >> '" + calls + "'\n"
		writeFile(t, filepath.Join(dir, name), script)
	}

	sourceSnippet(t, tm, dir, bin, 2)

	// The focus hook: set-hook, then run-shell, then the shell.
	if out, err := tm("set-hook", "-R", "pane-focus-in"); err != nil {
		t.Fatalf("set-hook -R: %v: %s", err, out)
	}
	waitForLine(t, calls, "seen %0")

	// The badge: if -F, then set -ag, then the #() job. Only an attached
	// client starts status jobs, so the stored job runs in sh by hand.
	right, err := tm("show", "-gv", "status-right")
	if err != nil {
		t.Fatal(err)
	}
	quoted := `'` + dir + `/my $HOME tools/it'\''s/curia'`
	if want := " #{@curia_badge}#(" + quoted + " status)\n"; !strings.HasSuffix(right, want) {
		t.Fatalf("status-right = %q, want suffix %q", right, want)
	}
	job := strings.TrimSuffix(right[strings.LastIndex(right, "#(")+2:], ")\n")
	sh := exec.CommandContext(ctx, "/bin/sh", "-c", job)
	sh.Dir = dir
	if out, err := sh.CombinedOutput(); err != nil {
		t.Fatalf("status job %q: %v: %s", job, err, out)
	}
	waitForLine(t, calls, "status ")

	for _, key := range []string{"a", "N", "A"} {
		if out, err := tm("list-keys", "-T", "prefix", key); err != nil || !strings.Contains(out, "curia") {
			t.Errorf("binding %s = %q, %v", key, out, err)
		}
	}
	if out, err := tm("list-keys", "-T", "prefix", "A"); err != nil || !strings.Contains(out, "decide") {
		t.Errorf("binding A = %q, %v; want the decide popup", out, err)
	}
}

// waitForLine waits a few seconds for the file at path to hold line.
func waitForLine(t *testing.T, path, line string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Clean(path))
		if strings.Contains(string(b), line+"\n") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %q, want line %q", path, b, line)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killServer stops the private test server on socket and removes its
// socket file, which tmux leaves behind. The directory is asked of the
// server before it goes, or else built from TMUX_TMPDIR as tmux builds it,
// and only the entry named socket in it is removed (os.Root keeps the
// removal inside that directory).
func killServer(socket string) {
	out, err := exec.Command("tmux", "-L", socket, "display-message", "-p", "#{socket_path}").Output()
	dir := filepath.Dir(strings.TrimSpace(string(out)))
	if err != nil || dir == "." {
		tmp := os.Getenv("TMUX_TMPDIR")
		if tmp == "" {
			tmp = "/tmp"
		}
		dir = filepath.Join(tmp, fmt.Sprintf("tmux-%d", os.Getuid()))
	}
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	if root, rerr := os.OpenRoot(dir); rerr == nil {
		_ = root.Remove(socket)
		_ = root.Close()
	}
}

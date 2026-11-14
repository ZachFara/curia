package install

import (
	"cmp"
	"context"
	"fmt"
	"maps"
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
	return realTmuxConf(ctx, t, dir, os.DevNull)
}

// realTmuxConf is realTmux with the server started from the configuration
// file conf, which the test wrote.
func realTmuxConf(ctx context.Context, t *testing.T, dir, conf string) func(args ...string) (string, error) {
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
	if out, err := tm("-f", conf, "new-session", "-d", "-s", "t"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	return tm
}

// sourceSnippet writes the snippet for bin into dir and sources it times
// times, failing on any error or output.
func sourceSnippet(t *testing.T, tm func(...string) (string, error), dir, bin string, times int) {
	t.Helper()
	snippet := filepath.Join(dir, "curia.tmux")
	if _, err := WriteTmux(snippet, filepath.Join(dir, ".tmux.conf"), bin, defaultKeys); err != nil {
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

// markerFragment is what the snippet appends to each window format.
const markerFragment = "#{E:@curia_marker_format}"

// A themed configuration modeled on a powerline theme loaded before the
// curia block: window formats with colors, separators, a conditional,
// commas and both quote characters, a status-right whose last segment
// carries its own colors, and a prefix key the user bound to another tool.
var (
	themeWindow = "#[fg=#282c34,bg=#3e4452,nobold]#[fg=#abb2bf,bg=#3e4452] #I  #W" +
		`#{?window_zoomed_flag, (Z),} "q" 'x', y #[fg=#3e4452,bg=#282c34]` + ""
	themeCurrent = "#[fg=#282c34,bg=#61afef]#[fg=#282c34,bg=#61afef,bold] #I  #W" +
		"#{?window_zoomed_flag,#[fg=red] Z,} #[fg=#61afef,bg=#282c34,nobold]"
	themeRight = "#[fg=#abb2bf,bg=#282c34] %H:%M #[fg=#e5c07b,bg=#282c34]#[fg=#e06c75,bg=#e5c07b]"
)

// themedConf writes the themed configuration to dir/.tmux.conf, with the
// curia block after it as curia install leaves it, and returns its path.
func themedConf(t *testing.T, dir string) string {
	t.Helper()
	conf := filepath.Join(dir, ".tmux.conf")
	writeFile(t, conf, strings.Join([]string{
		"set -g status-interval 15",
		"set -g window-status-format " + tmuxQuote(themeWindow),
		"set -g window-status-current-format " + tmuxQuote(themeCurrent),
		"set -g status-right " + tmuxQuote(themeRight),
		"bind a display-popup -E recon",
	}, "\n")+"\n")
	if _, err := WriteTmux(filepath.Join(dir, "curia.tmux"), conf, testBin, defaultKeys); err != nil {
		t.Fatal(err)
	}
	return conf
}

// tmuxState is what the snippet may touch, read from a real server.
func tmuxState(t *testing.T, tm func(...string) (string, error)) map[string]string {
	t.Helper()
	state := map[string]string{}
	for _, opt := range []string{
		"window-status-format", "window-status-current-format", "status-right",
		"status-interval", "status-right-length", "focus-events",
	} {
		out, err := tm("show", "-gv", opt)
		if err != nil {
			t.Fatalf("show %s: %v: %s", opt, err, out)
		}
		state[opt] = strings.TrimSuffix(out, "\n")
	}
	for _, key := range []string{"a", "A", "N"} {
		out, _ := tm("list-keys", "-T", "prefix", key) // an unbound key exits 1
		state["key "+key] = strings.TrimSpace(out)
	}
	return state
}

// TestSnippetRespectsThemedConf: a server started from a themed
// configuration with the curia block last keeps the theme's tabs (gaining
// the marker once), the user's own key, and gets the badge once after the
// theme's status-right, behind a style reset; sourcing the configuration or
// the snippet again changes nothing.
func TestSnippetRespectsThemedConf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	conf := themedConf(t, dir)
	tm := realTmuxConf(ctx, t, dir, conf)

	want := map[string]string{
		"window-status-format":         themeWindow + markerFragment,
		"window-status-current-format": themeCurrent + markerFragment,
		"status-right":                 themeRight + "#[default] #{@curia_badge}#(" + testBin + " status)",
		"status-interval":              "2",
		"status-right-length":          "80",
		"focus-events":                 "on",
	}
	got := tmuxState(t, tm)
	for opt, w := range want {
		if got[opt] != w {
			t.Errorf("%s =\n  %q\nwant\n  %q", opt, got[opt], w)
		}
	}
	if !strings.Contains(got["key a"], "recon") || strings.Contains(got["key a"], "curia") {
		t.Errorf("key a = %q, want the user's recon binding", got["key a"])
	}
	for _, key := range []string{"A", "N"} {
		if !strings.Contains(got["key "+key], testBin) {
			t.Errorf("key %s = %q, want Curia's binding", key, got["key "+key])
		}
	}

	for _, file := range []string{conf, filepath.Join(dir, "curia.tmux")} {
		if out, err := tm("source-file", file); err != nil || out != "" {
			t.Fatalf("source-file %s: %v: %q", file, err, out)
		}
		if again := tmuxState(t, tm); !maps.Equal(again, got) {
			t.Errorf("after sourcing %s again:\n%v\nwant\n%v", filepath.Base(file), again, got)
		}
	}
}

// TestMarkerRendersAfterThemedTab expands the window formats as the status
// line does: with no marker a tab renders exactly as the theme's own format,
// and with @curia_marker red it renders the same followed by the red dot.
func TestMarkerRendersAfterThemedTab(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmuxConf(ctx, t, dir, themedConf(t, dir))
	display := func(format string) string {
		t.Helper()
		out, err := tm("display-message", "-p", "-t", "t:", format)
		if err != nil {
			t.Fatalf("display %q: %v: %s", format, err, out)
		}
		return strings.TrimSuffix(out, "\n")
	}
	for opt, theme := range map[string]string{
		"window-status-format":         themeWindow,
		"window-status-current-format": themeCurrent,
	} {
		plain := display(theme)
		if got := display("#{E:" + opt + "}"); got != plain {
			t.Errorf("%s without marker = %q, want the theme's %q", opt, got, plain)
		}
		if out, err := tm("set", "-w", "-t", "t:", "@curia_marker", "red"); err != nil {
			t.Fatalf("set @curia_marker: %v: %s", err, out)
		}
		if got, want := display("#{E:"+opt+"}"), plain+"#[fg=red] ●"; got != want {
			t.Errorf("%s with marker = %q, want %q", opt, got, want)
		}
		if out, err := tm("set", "-uw", "-t", "t:", "@curia_marker"); err != nil {
			t.Fatalf("unset @curia_marker: %v: %s", err, out)
		}
	}
}

// TestSnippetOnBareServer: on a server with no configuration all three keys
// are Curia's and tmux's default window formats gain the marker once.
func TestSnippetOnBareServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	before := tmuxState(t, tm)

	sourceSnippet(t, tm, dir, testBin, 2)

	got := tmuxState(t, tm)
	for _, opt := range []string{"window-status-format", "window-status-current-format"} {
		if want := before[opt] + markerFragment; got[opt] != want {
			t.Errorf("%s = %q, want %q", opt, got[opt], want)
		}
	}
	for key, sub := range map[string]string{"a": " ui", "A": " decide", "N": " next"} {
		if !strings.Contains(got["key "+key], testBin+sub) {
			t.Errorf("key %s = %q, want Curia's%s", key, got["key "+key], sub)
		}
	}
}

// TestSnippetStatusInterval: the interval is lowered to 2 only from 0 (no
// refresh) or a larger value; a smaller one is kept.
func TestSnippetStatusInterval(t *testing.T) {
	for _, tc := range []struct{ preset, want string }{
		{"0", "2"}, {"1", "1"}, {"2", "2"}, {"15", "2"},
	} {
		t.Run("preset "+tc.preset, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			dir := t.TempDir()
			tm := realTmux(ctx, t, dir)
			if out, err := tm("set", "-g", "status-interval", tc.preset); err != nil {
				t.Fatalf("set status-interval: %v: %s", err, out)
			}
			sourceSnippet(t, tm, dir, testBin, 2)
			if out, err := tm("show", "-gv", "status-interval"); err != nil || strings.TrimSpace(out) != tc.want {
				t.Errorf("status-interval = %q, %v; want %s", out, err, tc.want)
			}
		})
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

// TestSnippetConfiguredKeys: the keys come from the configuration, and an
// empty one binds nothing.
func TestSnippetConfiguredKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	snippet := filepath.Join(dir, "curia.tmux")
	keys := TmuxKeys{Dashboard: "F5", Decisions: "", Next: "C-y"}
	if _, err := WriteTmux(snippet, filepath.Join(dir, ".tmux.conf"), testBin, keys); err != nil {
		t.Fatal(err)
	}
	if out, err := tm("source-file", snippet); err != nil || out != "" {
		t.Fatalf("source-file: %v: %q", err, out)
	}
	for key, want := range map[string]string{"F5": testBin + " ui", "C-y": testBin + " next"} {
		if out, err := tm("list-keys", "-T", "prefix", key); err != nil || !strings.Contains(out, want) {
			t.Errorf("key %s = %q, %v; want %q", key, out, err, want)
		}
	}
	all, err := tm("list-keys", "-T", "prefix")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all, " decide") {
		t.Errorf("decide is bound with key_decisions empty:\n%s", all)
	}
	for _, key := range []string{"a", "A", "N"} {
		if out, _ := tm("list-keys", "-T", "prefix", key); strings.Contains(out, "curia") {
			t.Errorf("key %s = %q, want no Curia binding", key, out)
		}
	}
}

// TestSnippetRebindsCuriasOwnKeys: a key bound to Curia by an earlier
// snippet, here for a binary that has since moved, counts as Curia's and is
// bound again for this binary.
func TestSnippetRebindsCuriasOwnKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	if out, err := tm("bind", "a", "display-popup", "-E", "-w", "90%", "-h", "80%", "/old/curia ui"); err != nil {
		t.Fatalf("bind: %v: %s", err, out)
	}
	sourceSnippet(t, tm, dir, testBin, 1)
	if out, err := tm("list-keys", "-T", "prefix", "a"); err != nil || !strings.Contains(out, testBin+" ui") {
		t.Errorf("key a = %q, %v; want %s ui", out, err, testBin)
	}
}

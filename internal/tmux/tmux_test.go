package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseListPanes(t *testing.T) {
	out := "%0\tmain\t@0\t1794437573\tzsh\n%1\tmain\t@1\t1794437573\tmy long window\n%2\twork\t@2\t1794437573\ta\tb\n"
	got, err := parsePanes(out)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1794437573, 0)
	want := []Pane{
		{ID: "%0", Session: "main", WindowID: "@0", ServerStart: start, WindowName: "zsh"},
		{ID: "%1", Session: "main", WindowID: "@1", ServerStart: start, WindowName: "my long window"},
		{ID: "%2", Session: "work", WindowID: "@2", ServerStart: start, WindowName: "a\tb"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestParsePanesMalformed(t *testing.T) {
	if _, err := parsePanes("%0\tonly-two\n"); err == nil {
		t.Fatal("want error for malformed line")
	}
	if _, err := parsePanes("\t\t\t\t\n"); err == nil {
		t.Fatal("want error for empty pane id")
	}
	// The old four-field format, or a start time that is not unix seconds.
	for _, line := range []string{"%0\tmain\t@0\tzsh\n", "%0\tmain\t@0\tsoon\tzsh\n", "%0\tmain\t@0\t\tzsh\n"} {
		if _, err := parsePanes(line); err == nil {
			t.Errorf("parsePanes(%q): want error for a bad start time", line)
		}
	}
}

func TestEscapeMessage(t *testing.T) {
	for in, want := range map[string]string{
		"plain":        "plain",
		"#{pane_id}":   "##{pane_id}",
		"%H:%M 100%":   "%%H:%%M 100%%",
		"#[fg=red] -x": "##[fg=red] -x",
		"":             "",
	} {
		if got := escapeMessage(in); got != want {
			t.Errorf("escapeMessage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := &Fake{Panes: []Pane{
		{ID: "%0", Session: "s", WindowID: "@0", WindowName: "w"},
		{ID: "%1", Session: "s", WindowID: "@1", WindowName: "x"},
	}}
	var c Client = f

	ps, err := c.ListPanes(ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("ListPanes = %v, %v", ps, err)
	}
	p, err := c.PaneInfo(ctx, "%1")
	if err != nil || p.WindowName != "x" {
		t.Fatalf("PaneInfo = %v, %v", p, err)
	}
	if _, err := c.PaneInfo(ctx, "%9"); err == nil {
		t.Fatal("want error for unknown pane")
	}
	if focused, err := c.PaneFocused(ctx, "%0"); err != nil || focused {
		t.Fatalf("PaneFocused = %v, %v; want the unset answer, false", focused, err)
	}
	f.Focused = true
	if focused, _ := c.PaneFocused(ctx, "%1"); !focused {
		t.Fatal("PaneFocused = false after setting Focused")
	}
	_ = c.SwitchTo(ctx, "%0")
	_ = c.SetWindowOption(ctx, "@0", "@curia", "busy")
	_ = c.DisplayMessage(ctx, "hi")
	_ = c.KillPane(ctx, "%0")
	if len(f.Panes) != 1 || f.Panes[0].ID != "%1" {
		t.Fatalf("Panes after kill = %v", f.Panes)
	}
	want := []string{
		"ListPanes", "PaneInfo %1", "PaneInfo %9", "PaneFocused %0", "PaneFocused %1", "SwitchTo %0",
		"SetWindowOption @0 @curia busy", "DisplayMessage hi", "KillPane %0",
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("Calls = %v", f.Calls)
	}
}

// realSocket is per run, so concurrent test runs (several worktrees running
// make check) never kill each other's servers.
var realSocket = fmt.Sprintf("curia-test-%d", os.Getpid())

// realTmux starts a throwaway server with no configuration on its own socket
// (never the default server) and returns a helper that runs raw tmux commands
// against it. -f /dev/null keeps the user's ~/.tmux.conf, which may hold
// Curia's own hooks, out of the test server.
func realTmux(ctx context.Context, t *testing.T) func(args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tm := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", realSocket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return string(out), err
	}
	// Cleanup runs after the caller's deferred cancel, so it must not use ctx.
	t.Cleanup(func() { killServer(realSocket) })
	_, _ = tm("kill-server") // clear a leftover server from an aborted run
	if out, err := tm("new-session", "-d", "-s", "t", "-n", "win dow"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	return tm
}

// TestRealTmuxReadsNoConfig: the test server must not read the user's tmux
// configuration, which may hold Curia's own hooks pointing at the real store.
// HOME and XDG_CONFIG_HOME point at a temp dir whose config would set an
// option; it must stay unset.
func TestRealTmuxReadsNoConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	for _, p := range []string{filepath.Join(home, ".tmux.conf"), filepath.Join(home, "tmux", "tmux.conf")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("set -g @curia_conf_read yes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	if out, _ := tm("show", "-gqv", "@curia_conf_read"); strings.TrimSpace(out) != "" {
		t.Errorf("test server read a tmux config: @curia_conf_read = %q", out)
	}
}

func TestExecAgainstRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	c := NewExec(realSocket)

	panes, err := c.ListPanes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 1 || panes[0].Session != "t" || panes[0].WindowName != "win dow" {
		t.Fatalf("panes = %+v", panes)
	}
	// The server was started by realTmux, moments ago.
	if d := time.Since(panes[0].ServerStart); d < 0 || d > time.Minute {
		t.Fatalf("ServerStart = %v, want within the last minute", panes[0].ServerStart)
	}
	p := panes[0]

	info, err := c.PaneInfo(ctx, p.ID)
	if err != nil || info != p {
		t.Fatalf("PaneInfo = %+v, %v; want %+v", info, err, p)
	}
	// tmux expands an unknown target to empty fields with exit 0; we must error.
	if _, err = c.PaneInfo(ctx, "%999"); err == nil {
		t.Fatal("want error for missing pane")
	}

	if err = c.SetWindowOption(ctx, p.WindowID, "@curia", "-needs input"); err != nil {
		t.Fatal(err)
	}
	out, err := tm("show-options", "-w", "-t", p.WindowID, "-v", "@curia")
	if err != nil || strings.TrimSpace(out) != "-needs input" {
		t.Fatalf("show-options = %q, %v", out, err)
	}

	checkNoWindow(ctx, t, c, p)
	checkHeadlessAndKill(ctx, t, c, tm, p)
}

// checkNoWindow: a window that is gone is told apart from other failures.
func checkNoWindow(ctx context.Context, t *testing.T, c Client, p Pane) {
	t.Helper()
	if err := c.SetWindowOption(ctx, "@999", "@curia", "x"); !errors.Is(err, ErrNoWindow) {
		t.Errorf("SetWindowOption on a missing window = %v, want ErrNoWindow", err)
	}
	if err := c.SetWindowOption(ctx, p.WindowID, "not-an-option", "x"); err == nil || errors.Is(err, ErrNoWindow) {
		t.Errorf("SetWindowOption with a bad option = %v, want another error", err)
	}
}

// checkHeadlessAndKill covers the calls that need no attached client.
func checkHeadlessAndKill(ctx context.Context, t *testing.T, c Client, tm func(...string) (string, error), p Pane) {
	t.Helper()
	// No client is attached (headless CI): switch-client fails cleanly, and
	// display-message with a leading "-" and a format is accepted as text.
	if err := c.SwitchTo(ctx, p.ID); err == nil {
		t.Fatal("SwitchTo without a client should fail")
	}
	const text = "-x #{pane_id} %H:%M %1 100% #"
	if err := c.DisplayMessage(ctx, text); err != nil {
		t.Fatalf("DisplayMessage: %v", err)
	}
	// (-p leaves #[style] sequences unprocessed, so they are only unit-tested.)
	// Round trip: the escaped text must render back as the original.
	out, err := tm("display-message", "-p", "--", escapeMessage(text))
	if err != nil || strings.TrimSuffix(out, "\n") != text {
		t.Fatalf("rendered %q (%v), want %q", out, err, text)
	}

	// A second window so killing its pane leaves the server alive.
	if out, err = tm("new-window", "-d", "-t", "t"); err != nil {
		t.Fatalf("new-window: %v: %s", err, out)
	}
	panes, err := c.ListPanes(ctx)
	if err != nil || len(panes) != 2 {
		t.Fatalf("panes = %+v, %v", panes, err)
	}
	if err = c.KillPane(ctx, panes[1].ID); err != nil {
		t.Fatal(err)
	}
	if panes, _ = c.ListPanes(ctx); len(panes) != 1 {
		t.Fatalf("after kill panes = %+v", panes)
	}
}

// focusServers starts a server with no configuration on its own socket and,
// when attach is called, a second one whose only pane runs a client attached
// to the first, so the client has a real pty. Neither reads ~/.tmux.conf, so
// no user hook (such as Curia's own pane-focus-in) runs. It returns a helper
// for raw tmux commands against the first server and the attach func.
func focusServers(ctx context.Context, t *testing.T) (tm func(args ...string) (string, error), attach func()) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	inner, outer := realSocket+"-focus", realSocket+"-outer"
	on := func(socket string, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", socket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() {
		for _, s := range []string{outer, inner} {
			killServer(s)
		}
	})
	for _, s := range []string{outer, inner} {
		_, _ = on(s, "kill-server") // a leftover from an aborted run
	}
	if out, err := on(inner, "new-session", "-d", "-s", "t"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	tm = func(args ...string) (string, error) { return on(inner, args...) }
	attach = func() {
		t.Helper()
		// The outer pane sets TMUX, which makes tmux refuse to attach.
		client := "env -u TMUX tmux -L " + inner + " attach -t t"
		if out, err := on(outer, "new-session", "-d", "-x", "80", "-y", "24", client); err != nil {
			t.Fatalf("outer new-session: %v: %s", err, out)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if out, _ := tm("list-clients", "-F", "#{client_name}"); strings.TrimSpace(out) != "" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("no client attached after 5s")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return tm, attach
}

// TestPaneFocusedAgainstRealTmux: a pane is focused when it is the active
// pane of the active window of a session with an attached client.
func TestPaneFocusedAgainstRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm, attach := focusServers(ctx, t)
	c := NewExec(realSocket + "-focus")
	first := paneID(t, tm, "t")
	if out, err := tm("new-window", "-d", "-t", "t:"); err != nil {
		t.Fatalf("new-window: %v: %s", err, out)
	}
	background := paneID(t, tm, "t:1")
	focused := func(pane string, want bool) {
		t.Helper()
		if got, err := c.PaneFocused(ctx, pane); err != nil || got != want {
			t.Errorf("PaneFocused(%s) = %v, %v; want %v", pane, got, err, want)
		}
	}

	focused(first, false) // no client attached
	attach()
	focused(first, true)
	focused(background, false) // not the active window
	if out, err := tm("split-window", "-t", first); err != nil {
		t.Fatalf("split-window: %v: %s", err, out)
	}
	focused(first, false) // no longer the active pane
	focused(paneID(t, tm, "t:0"), true)
	if _, err := c.PaneFocused(ctx, "%999"); err == nil {
		t.Error("PaneFocused(%999): want an error for a missing pane")
	}
}

// paneID returns the id of the active pane of target.
func paneID(t *testing.T, tm func(...string) (string, error), target string) string {
	t.Helper()
	out, err := tm("display-message", "-p", "-t", target, "#{pane_id}")
	if id := strings.TrimSpace(out); err == nil && id != "" {
		return id
	}
	t.Fatalf("pane id of %s: %q, %v", target, out, err)
	return ""
}

// recorder is a runner for execClient that records each tmux invocation
// instead of running it and answers each with out, except that call number
// failAt (counting from 1; 0 for none) fails with err.
//
// It is also Bell's tty opener: it records each path and opens it with
// openFile, or refuses when openFile is nil.
type recorder struct {
	runs     [][]string
	out      string
	failAt   int
	err      error
	opened   []string
	openFile func(name string) (*os.File, error)
}

func (r *recorder) open(name string) (*os.File, error) {
	r.opened = append(r.opened, name)
	if r.openFile == nil {
		return nil, errors.New("recorder: opening is refused")
	}
	return r.openFile(name)
}

func (r *recorder) run(_ context.Context, args ...string) (string, error) {
	r.runs = append(r.runs, slices.Clone(args))
	if len(r.runs) == r.failAt {
		return "", r.err
	}
	return r.out, nil
}

func recordingClient() (*execClient, *recorder) {
	r := &recorder{}
	return &execClient{runner: r.run, openTTY: r.open}, r
}

func TestSendKeysRefusesBadInput(t *testing.T) {
	ctx := context.Background()
	for _, pane := range []string{"", "%", "1", "%1;kill-server", "t:0", "%1 ", "%1\n", "%-1", "%%1"} {
		c, r := recordingClient()
		if err := c.SendKeys(ctx, pane, "hello"); err == nil {
			t.Errorf("SendKeys(%q, hello): want an error for a bad pane", pane)
		}
		if len(r.runs) != 0 {
			t.Errorf("SendKeys(%q, hello) ran tmux: %v", pane, r.runs)
		}
	}
	texts := []string{
		"", "a\nb", "a\x1bb", "a\x7f", "a\u009bb", "\xff", "x", strings.Repeat("a", 801),
		"a\rb", "a\tb", "a\x00b", "a\u0080", "ok\xffok", "é", ";",
	}
	for _, text := range texts {
		c, r := recordingClient()
		if err := c.SendKeys(ctx, "%5", text); err == nil {
			t.Errorf("SendKeys(%%5, %q): want an error", text)
		}
		if len(r.runs) != 0 {
			t.Errorf("SendKeys(%%5, %q) ran tmux: %v", text, r.runs)
		}
	}
}

func TestSendKeysIsThreeSeparateInvocations(t *testing.T) {
	c, r := recordingClient()
	if err := c.SendKeys(context.Background(), "%5", "use pg; then ship;"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"copy-mode", "-q", "-t", "%5"},
		{"send-keys", "-t", "%5", "-l", "--", `use pg; then ship\;`},
		{"send-keys", "-t", "%5", "Enter"},
	}
	if !reflect.DeepEqual(r.runs, want) {
		t.Fatalf("runs = %q\nwant   %q", r.runs, want)
	}
	for _, run := range r.runs {
		if slices.Contains(run, ";") || slices.Contains(run, `\;`) {
			t.Errorf("run %q carries a command separator of its own", run)
		}
	}
}

// TestSendKeysStopsAtFirstFailure: SendKeys stops at the step that fails.
// Only a failed Enter, after the text was typed, is a partial send (Ruling 54).
func TestSendKeysStopsAtFirstFailure(t *testing.T) {
	for _, tc := range []struct {
		failAt  int
		partial bool
	}{
		{failAt: 1},                // copy-mode -q failed: nothing typed
		{failAt: 2},                // the text step failed: no Enter
		{failAt: 3, partial: true}, // Enter failed: the text sits unsubmitted
	} {
		c, r := recordingClient()
		r.failAt, r.err = tc.failAt, errors.New("can't find pane: %5")
		err := c.SendKeys(context.Background(), "%5", "hello")
		if !errors.Is(err, r.err) {
			t.Errorf("failAt %d: err = %v, want the runner's error", tc.failAt, err)
		}
		if got := errors.Is(err, ErrPartialSend); got != tc.partial {
			t.Errorf("failAt %d: errors.Is(err, ErrPartialSend) = %v, want %v", tc.failAt, got, tc.partial)
		}
		if len(r.runs) != tc.failAt {
			t.Errorf("failAt %d: runs = %q, want %d", tc.failAt, r.runs, tc.failAt)
		}
	}
}

// TestSendKeysWhenContextEnds: the caller's context ends during step
// cancelAt (1 copy-mode, 2 text, 3 Enter). The runner records every step
// SendKeys asks for and acts like exec.CommandContext: it refuses to start one
// once the step's context is done, and with killed set, the step running when
// its context ends fails as a killed process would, after tmux may already
// have typed the text (Ruling 58). SendKeys itself must not ask for the text
// step once the context is done; once the text step has succeeded, Enter runs
// whatever happens to the caller's context (Ruling 81).
func TestSendKeysWhenContextEnds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cancelAt int
		killed   bool
		runs     int
		partial  bool
		canceled bool // the error wraps context.Canceled; false: no error
	}{
		{name: "during copy-mode, which fails", cancelAt: 1, killed: true, runs: 1, canceled: true},
		{name: "after copy-mode, before the text step starts", cancelAt: 1, runs: 1, canceled: true},
		{name: "during the text step, which is killed", cancelAt: 2, killed: true, runs: 2, partial: true, canceled: true},
		{name: "after the text step, so Enter still runs", cancelAt: 2, runs: 3},
		{name: "during Enter, which is not killed", cancelAt: 3, killed: true, runs: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &cancellingRunner{cancel: cancel, cancelAt: tc.cancelAt, killed: tc.killed}
			err := (&execClient{runner: r.run}).SendKeys(ctx, "%5", "hello")
			if tc.canceled && !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled", err)
			}
			if !tc.canceled && err != nil {
				t.Errorf("err = %v, want nil: Enter ran after the text step", err)
			}
			if got := errors.Is(err, ErrPartialSend); got != tc.partial {
				t.Errorf("errors.Is(err, ErrPartialSend) = %v, want %v (err %v)", got, tc.partial, err)
			}
			if len(r.runs) != tc.runs {
				t.Errorf("asked for %d steps %q, want %d", len(r.runs), r.runs, tc.runs)
			}
		})
	}
}

// cancellingRunner is the runner TestSendKeysWhenContextEnds describes.
type cancellingRunner struct {
	cancel   context.CancelFunc
	cancelAt int
	killed   bool
	runs     [][]string
}

func (r *cancellingRunner) run(ctx context.Context, args ...string) (string, error) {
	r.runs = append(r.runs, args)
	if err := ctx.Err(); err != nil {
		return "", err // never started
	}
	if len(r.runs) == r.cancelAt {
		r.cancel()
		if r.killed && ctx.Err() != nil {
			return "", fmt.Errorf("tmux %s: signal: killed: %w", args[0], ctx.Err())
		}
	}
	return "", nil
}

// TestSendKeysStepBudgets: each of SendKeys's three tmux invocations gets
// its own deadline, the copy-mode and text steps at most keysStepTimeout away
// and Enter at most enterTimeout away, whatever the caller's deadline; a
// shorter caller deadline shortens the first two steps but not Enter, and a
// caller canceled after the text step leaves Enter's context live (Ruling 81).
func TestSendKeysStepBudgets(t *testing.T) {
	caller, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	steps := budgetSteps(caller, t, cancel)
	for k, s := range steps[:2] {
		s.within(t, fmt.Sprintf("step %d", k+1), 0, 400*time.Millisecond)
	}
	if steps[2].err != nil {
		t.Errorf("Enter ran on a done context: %v", steps[2].err)
	}
	steps[2].within(t, "Enter", time.Second, enterTimeout)

	for k, s := range budgetSteps(context.Background(), t, func() {})[:2] {
		s.within(t, fmt.Sprintf("no caller deadline: step %d", k+1), 0, keysStepTimeout)
	}
}

// budgetStep is what one tmux invocation of SendKeys saw of its context.
type budgetStep struct {
	left time.Duration // until the deadline
	has  bool          // a deadline is set
	err  error         // ctx.Err() when the step started
}

// within fails unless the step had a deadline more than lo and at most hi away.
func (s budgetStep) within(t *testing.T, what string, lo, hi time.Duration) {
	t.Helper()
	if !s.has || s.left <= lo || s.left > hi {
		t.Errorf("%s: deadline in %v (set %v), want in (%v, %v]", what, s.left, s.has, lo, hi)
	}
}

// budgetSteps runs SendKeys on ctx, calling afterText once the text step is
// done, and returns what each of its three steps saw.
func budgetSteps(ctx context.Context, t *testing.T, afterText func()) []budgetStep {
	t.Helper()
	var steps []budgetStep
	runner := func(sctx context.Context, _ ...string) (string, error) {
		d, ok := sctx.Deadline()
		steps = append(steps, budgetStep{time.Until(d), ok, sctx.Err()})
		if len(steps) == 2 {
			afterText()
		}
		return "", nil
	}
	if err := (&execClient{runner: runner}).SendKeys(ctx, "%5", "hello"); err != nil {
		t.Fatalf("SendKeys = %v, want nil", err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	return steps
}

func TestSendKeysAcceptsEightHundredRunes(t *testing.T) {
	ctx := context.Background()
	c, r := recordingClient()
	text := strings.Repeat("é", 800)
	if len(text) != 1600 {
		t.Fatalf("len = %d, want 1600 bytes", len(text))
	}
	if err := c.SendKeys(ctx, "%5", text); err != nil {
		t.Fatalf("800 runes refused: %v", err)
	}
	if len(r.runs) != 3 || r.runs[1][5] != text {
		t.Fatalf("runs = %d, want 3 with the text intact", len(r.runs))
	}
	c, r = recordingClient()
	if err := c.SendKeys(ctx, "%5", text+"é"); err == nil {
		t.Fatal("801 runes accepted")
	}
	if len(r.runs) != 0 {
		t.Fatalf("801 runes ran tmux: %d runs", len(r.runs))
	}
}

func TestBellRefusesBadPane(t *testing.T) {
	for _, pane := range []string{"", "%", "1", "%1;kill-server", "t:0"} {
		c, r := recordingClient()
		if err := c.Bell(context.Background(), pane); err == nil {
			t.Errorf("Bell(%q): want an error", pane)
		}
		if len(r.runs) != 0 {
			t.Errorf("Bell(%q) ran tmux: %v", pane, r.runs)
		}
	}
}

// TestBellOpensOnlyDevPaths: whatever tmux answers, Bell opens nothing but a
// pty path, /dev/ttysN or /dev/pts/N, and writes only to a character device
// (Ruling 55).
func TestBellOpensOnlyDevPaths(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	answers := []string{
		"", "\n", victim + "\n", "/dev/../" + victim + "\n", "/dev/ttys001 x\n", "/dev/\n", "/dev/tty\x07\n",
		"/dev/fd/1\n", "/dev/stdout\n", "/dev/tty\n", "/dev/null\n", "/dev/ttys\n", "/dev/pts/\n",
		"/dev/pts/1/2\n", "/dev/ttys1a\n", "/dev/disk0\n", "/dev/ttys001\n\n", "/dev/pts/../ttys001\n",
	}
	want := [][]string{{"display-message", "-p", "-t", "%5", "#{pane_tty}"}}
	for _, out := range answers {
		c, r := recordingClient()
		r.out = out
		if err := c.Bell(context.Background(), "%5"); err == nil {
			t.Errorf("Bell with tty answer %q: want an error", out)
		}
		if !reflect.DeepEqual(r.runs, want) {
			t.Errorf("runs = %q, want %q", r.runs, want)
		}
		if len(r.opened) != 0 {
			t.Errorf("Bell with tty answer %q opened %q", out, r.opened)
		}
	}
	// A regular file at an accepted-looking path is opened but not written.
	for _, tty := range []string{"/dev/ttys999", "/dev/pts/3"} {
		c, r := recordingClient()
		r.out = tty + "\n"
		r.openFile = func(string) (*os.File, error) { return os.OpenFile(filepath.Clean(victim), os.O_WRONLY, 0) }
		if err := c.Bell(context.Background(), "%5"); err == nil {
			t.Errorf("Bell onto a regular file at %s: want an error", tty)
		}
		if !reflect.DeepEqual(r.opened, []string{tty}) {
			t.Errorf("opened = %q, want [%s]", r.opened, tty)
		}
	}
	if b, err := os.ReadFile(filepath.Clean(victim)); err != nil || string(b) != "untouched" {
		t.Errorf("victim file = %q, %v; want it untouched", b, err)
	}
}

// TestBellHonoursContextDeadline: the write to the pane's tty carries the
// context's deadline, so a flow-stopped pane cannot block the caller. With a
// deadline already passed, nothing is written to a real pane's tty.
func TestBellHonoursContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	pane := catPane(t, tm, filepath.Join(t.TempDir(), "out"))
	tty, err := tm("display-message", "-p", "-t", pane, "#{pane_tty}")
	if err != nil {
		t.Fatalf("pane_tty: %v: %s", err, tty)
	}
	r := &recorder{out: tty}
	c := &execClient{runner: r.run, openTTY: openTTY}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	// A done context is caught before the write.
	if err := c.Bell(expired, pane); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Bell with a done context = %v, want context.DeadlineExceeded", err)
	}
	// A context not yet done whose deadline has passed reaches the write,
	// which the write deadline stops.
	if err := c.Bell(pastDeadline{Context: context.Background()}, pane); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Bell past the write deadline = %v, want os.ErrDeadlineExceeded", err)
	}
	time.Sleep(200 * time.Millisecond)
	if out, _ := tm("display-message", "-p", "-t", pane, "#{window_bell_flag}"); strings.TrimSpace(out) != "0" {
		t.Fatalf("window_bell_flag = %q after Bells that should not ring, want 0", out)
	}
}

// pastDeadline is a context that is not done but reports a deadline already
// passed, as a live context does in the moment its deadline arrives.
type pastDeadline struct{ context.Context }

func (pastDeadline) Deadline() (time.Time, bool) { return time.Now().Add(-time.Second), true }

func TestFakeRecordsSendKeysAndBell(t *testing.T) {
	ctx := context.Background()
	f := &Fake{}
	var c Client = f
	if err := c.SendKeys(ctx, "%5", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := c.Bell(ctx, "%5"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"SendKeys %5 hi", "Bell %5"}; !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("Calls = %q, want %q", f.Calls, want)
	}
	f.SendKeysErr = errors.New("boom")
	if err := c.SendKeys(ctx, "%6", "again"); !errors.Is(err, f.SendKeysErr) {
		t.Fatalf("SendKeys = %v, want SendKeysErr", err)
	}
	if last := f.Calls[len(f.Calls)-1]; last != "SendKeys %6 again" {
		t.Fatalf("failed SendKeys not recorded: last call %q", last)
	}
	f.SendKeysErr = fmt.Errorf("%w: enter failed", ErrPartialSend)
	if err := c.SendKeys(ctx, "%6", "again"); !errors.Is(err, ErrPartialSend) {
		t.Fatalf("SendKeys = %v, want ErrPartialSend", err)
	}
}

// catPane opens a background window on the test server whose pane runs
// `cat > out` (directly, with no shell configuration) and returns its id.
func catPane(t *testing.T, tm func(...string) (string, error), out string) string {
	t.Helper()
	id, err := tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "t:", "/bin/sh", "-c", "exec cat > '"+out+"'")
	if err != nil {
		t.Fatalf("new-window: %v: %s", err, id)
	}
	return strings.TrimSpace(id)
}

// closeCat ends cat's input with C-d and waits for its pane to close, so
// everything typed is in the file.
func closeCat(t *testing.T, tm func(...string) (string, error), pane string) {
	t.Helper()
	if out, err := tm("send-keys", "-t", pane, "C-d"); err != nil {
		t.Fatalf("send-keys C-d: %v: %s", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := tm("list-panes", "-a", "-F", "#{pane_id}")
		if err != nil {
			t.Fatalf("list-panes: %v: %s", err, out)
		}
		if !slices.Contains(strings.Fields(out), pane) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s still open 5s after C-d", pane)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSendKeysLiteralInRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	c := NewExec(realSocket)
	out := filepath.Join(t.TempDir(), "out")
	pane := catPane(t, tm, out)
	lines := []string{
		"plain text", "ends with semi;", `ends with backslash-semi\;`, "-l Enter C-c",
		"#{pane_id} %H $(id) `id`", ";;", "Postgrés ≈ 16",
	}
	for _, line := range lines {
		if err := c.SendKeys(ctx, pane, line); err != nil {
			t.Fatalf("SendKeys(%q): %v", line, err)
		}
	}
	closeCat(t, tm, pane)
	got, err := os.ReadFile(filepath.Clean(out))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(lines, "\n") + "\n"; string(got) != want {
		t.Fatalf("cat got %q\nwant    %q", got, want)
	}
}

func TestSendKeysUnknownPaneFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	realTmux(ctx, t)
	if err := NewExec(realSocket).SendKeys(ctx, "%99", "hello"); err == nil {
		t.Fatal("SendKeys(%99): want an error for a missing pane")
	}
}

func TestSendKeysLeavesCopyMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	c := NewExec(realSocket)
	out := filepath.Join(t.TempDir(), "out")
	pane := catPane(t, tm, out)
	inMode := func() string {
		t.Helper()
		v, err := tm("display-message", "-p", "-t", pane, "#{pane_in_mode}")
		if err != nil {
			t.Fatalf("pane_in_mode: %v: %s", err, v)
		}
		return strings.TrimSpace(v)
	}
	if v, err := tm("copy-mode", "-t", pane); err != nil {
		t.Fatalf("copy-mode: %v: %s", err, v)
	}
	if m := inMode(); m != "1" {
		t.Fatalf("pane_in_mode = %q before SendKeys, want 1", m)
	}
	if err := c.SendKeys(ctx, pane, "after copy mode"); err != nil {
		t.Fatal(err)
	}
	if m := inMode(); m != "0" {
		t.Fatalf("pane_in_mode = %q after SendKeys, want 0", m)
	}
	closeCat(t, tm, pane)
	if got, err := os.ReadFile(filepath.Clean(out)); err != nil || string(got) != "after copy mode\n" {
		t.Fatalf("cat got %q, %v; want the line", got, err)
	}
}

func TestBellInRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	pane := catPane(t, tm, filepath.Join(t.TempDir(), "out"))
	flag := func() string {
		t.Helper()
		v, err := tm("display-message", "-p", "-t", pane, "#{window_bell_flag}")
		if err != nil {
			t.Fatalf("window_bell_flag: %v: %s", err, v)
		}
		return strings.TrimSpace(v)
	}
	if f := flag(); f != "0" {
		t.Fatalf("window_bell_flag = %q before Bell, want 0", f)
	}
	if err := NewExec(realSocket).Bell(ctx, pane); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for flag() != "1" {
		if time.Now().After(deadline) {
			t.Fatal("window_bell_flag not set 2s after Bell")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBellUnknownPaneFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	realTmux(ctx, t)
	if err := NewExec(realSocket).Bell(ctx, "%99"); err == nil {
		t.Fatal("Bell(%99): want an error for a missing pane")
	}
}

func TestCapturePaneRefusesBadPane(t *testing.T) {
	for _, pane := range []string{"", "%", "1", "%1;kill-server", "t:0"} {
		c, r := recordingClient()
		if _, err := c.CapturePane(context.Background(), pane); err == nil {
			t.Errorf("CapturePane(%q): want an error", pane)
		}
		if len(r.runs) != 0 {
			t.Errorf("CapturePane(%q) ran tmux: %v", pane, r.runs)
		}
	}
}

func TestCapturePaneInRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	id, err := tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "t:", "/bin/sh", "-c", `printf '%s\n' "$1"; exec cat`, "sh", "❯ draft #{pane_id} %H")
	if err != nil {
		t.Fatalf("new-window: %v: %s", err, id)
	}
	pane := strings.TrimSpace(id)
	c := NewExec(realSocket)
	var capt Capture
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(capt.Screen, "❯ draft"); time.Sleep(20 * time.Millisecond) {
		if capt, err = c.CapturePane(ctx, pane); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("CapturePane = %q, want the printed line", capt.Screen)
		}
	}
	if !strings.Contains(capt.Screen, "❯ draft #{pane_id} %H\n") || strings.Contains(capt.Screen, "\x1b") {
		t.Errorf("CapturePane = %q, want the line as printed, with no escapes", capt.Screen)
	}
	if capt.Synchronized {
		t.Error("CapturePane: Synchronized set for a window without synchronize-panes")
	}
	if _, err := c.CapturePane(ctx, "%99"); err == nil {
		t.Error("CapturePane(%99): want an error for a missing pane")
	}
}

// Re-review N5: with synchronize-panes on, send-keys to one pane reaches
// every pane of the window, so CapturePane says so.
func TestCapturePaneReportsSynchronizePanes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	id, err := tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "t:", "/bin/sh", "-c", "exec cat")
	if err != nil {
		t.Fatalf("new-window: %v: %s", err, id)
	}
	pane := strings.TrimSpace(id)
	for _, args := range [][]string{
		{"split-window", "-d", "-t", pane, "/bin/sh", "-c", "exec cat"},
		{"set-window-option", "-t", pane, "synchronize-panes", "on"},
	} {
		if out, terr := tm(args...); terr != nil {
			t.Fatalf("%s: %v: %s", args[0], terr, out)
		}
	}
	capt, err := NewExec(realSocket).CapturePane(ctx, pane)
	if err != nil || !capt.Synchronized {
		t.Errorf("CapturePane = synchronized %v, %v; want true", capt.Synchronized, err)
	}
}

// CapturePane refuses an answer that does not name the pane asked about,
// as tmux gives for an unknown target, before capturing anything.
func TestCapturePaneRefusesUnknownPaneAnswer(t *testing.T) {
	for _, out := range []string{"", "\t\n", "%6\t0\n", "%5\n"} {
		c, r := recordingClient()
		r.out = out
		if _, err := c.CapturePane(context.Background(), "%5"); err == nil {
			t.Errorf("answer %q: want an error", out)
		}
		if len(r.runs) != 1 {
			t.Errorf("answer %q: runs = %q, want only the display-message", out, r.runs)
		}
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

// goodWindow is a WindowSpec NewWindow accepts; each refusal case below
// spoils one field of it.
func goodWindow() WindowSpec {
	return WindowSpec{
		Session: "api", Name: "T-1", Dir: "/tmp/wt", Env: []string{"CURIA_TASK_ID=T-1"},
		Command: []string{"/usr/local/bin/curia", "launch", "T-1"},
	}
}

func TestNewWindowRefusesBadInput(t *testing.T) {
	many := func(n int, s string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", s, i)
		}
		return out
	}
	cases := map[string]func(*WindowSpec){
		"session empty":           func(w *WindowSpec) { w.Session = "" },
		"session a.b":             func(w *WindowSpec) { w.Session = "a.b" },
		"session a:b":             func(w *WindowSpec) { w.Session = "a:b" },
		"session a b":             func(w *WindowSpec) { w.Session = "a b" },
		"session 41 runes":        func(w *WindowSpec) { w.Session = strings.Repeat("a", 41) },
		"session ends with ;":     func(w *WindowSpec) { w.Session = "api;" },
		"name T 1":                func(w *WindowSpec) { w.Name = "T 1" },
		"name empty":              func(w *WindowSpec) { w.Name = "" },
		"name 41 runes":           func(w *WindowSpec) { w.Name = strings.Repeat("T", 41) },
		"name #{pane_id}":         func(w *WindowSpec) { w.Name = "#{pane_id}" },
		"dir rel":                 func(w *WindowSpec) { w.Dir = "rel" },
		"dir empty":               func(w *WindowSpec) { w.Dir = "" },
		"dir newline":             func(w *WindowSpec) { w.Dir = "/a\nb" },
		"dir tab":                 func(w *WindowSpec) { w.Dir = "/a\tb" },
		"dir ends with ;":         func(w *WindowSpec) { w.Dir = "/a;" },
		"dir not cleaned":         func(w *WindowSpec) { w.Dir = "/a/../b" },
		"dir trailing slash":      func(w *WindowSpec) { w.Dir = "/a/" },
		"dir not UTF-8":           func(w *WindowSpec) { w.Dir = "/a\xffb" },
		"env lower=1":             func(w *WindowSpec) { w.Env = []string{"lower=1"} },
		"env A=$(id)":             func(w *WindowSpec) { w.Env = []string{"A=$(id)"} },
		"env A=b c":               func(w *WindowSpec) { w.Env = []string{"A=b c"} },
		"env A=b;":                func(w *WindowSpec) { w.Env = []string{"A=b;"} },
		"env no =":                func(w *WindowSpec) { w.Env = []string{"A"} },
		"env name 65":             func(w *WindowSpec) { w.Env = []string{strings.Repeat("A", 65) + "=1"} },
		"env value 257":           func(w *WindowSpec) { w.Env = []string{"A=" + strings.Repeat("v", 257)} },
		"env 9 entries":           func(w *WindowSpec) { w.Env = many(9, "A=") },
		"command nil":             func(w *WindowSpec) { w.Command = nil },
		"command empty arg":       func(w *WindowSpec) { w.Command = []string{""} },
		"command ;":               func(w *WindowSpec) { w.Command = []string{";"} },
		"command x;":              func(w *WindowSpec) { w.Command = []string{"x;"} },
		"command a\\nb":           func(w *WindowSpec) { w.Command = []string{"a\nb"} },
		"command 17 args":         func(w *WindowSpec) { w.Command = many(17, "a") },
		"command later ;":         func(w *WindowSpec) { w.Command = []string{"/bin/echo", "x", ";"} },
		"command later x;":        func(w *WindowSpec) { w.Command = []string{"/bin/echo", "x;"} },
		"command later empty":     func(w *WindowSpec) { w.Command = []string{"/bin/echo", ""} },
		"command arg 4097 bytes":  func(w *WindowSpec) { w.Command = []string{"/bin/echo", strings.Repeat("a", 4097)} },
		"command arg not UTF-8":   func(w *WindowSpec) { w.Command = []string{"/bin/echo", "a\xff"} },
		"command arg escape":      func(w *WindowSpec) { w.Command = []string{"/bin/echo", "a\x1bb"} },
		"one arg with shell text": func(w *WindowSpec) { w.Command = []string{"echo $HOME"} },
		"one arg with space":      func(w *WindowSpec) { w.Command = []string{"sleep 9"} },
		"one arg like a flag":     func(w *WindowSpec) { w.Command = []string{"-x"} },
		"one arg assignment":      func(w *WindowSpec) { w.Command = []string{"A=b"} },
	}
	for name, spoil := range cases {
		w := goodWindow()
		spoil(&w)
		c, r := recordingClient()
		if _, err := c.NewWindow(context.Background(), w); err == nil {
			t.Errorf("%s: NewWindow(%+v): want an error", name, w)
		}
		if len(r.runs) != 0 {
			t.Errorf("%s: NewWindow ran tmux: %q", name, r.runs)
		}
	}
}

// TestNewWindowAcceptsLimits: the largest values the rules allow are
// accepted and reach tmux unchanged.
func TestNewWindowAcceptsLimits(t *testing.T) {
	w := goodWindow()
	w.Session, w.Name = strings.Repeat("s", 40), strings.Repeat("n", 40)
	w.Dir = "/a b/é/#{x}%H"
	w.Env = nil
	for i := range 8 {
		// 64-character names, the longest allowed.
		w.Env = append(w.Env, fmt.Sprintf("%s%c=%s", strings.Repeat("V", 63), 'A'+i, strings.Repeat("v", 256)))
	}
	w.Command = []string{"/bin/echo"}
	for range 15 {
		w.Command = append(w.Command, strings.Repeat("é", 2048))
	}
	c, r := recordingClient()
	r.out = "%3\t" + w.Session + "\t@3\t1794437573\t" + w.Name + "\n"
	if _, err := c.NewWindow(context.Background(), w); err != nil {
		t.Fatalf("NewWindow at the limits: %v", err)
	}
	if len(r.runs) != 2 || !slices.Equal(r.runs[1][len(r.runs[1])-16:], w.Command) {
		t.Fatalf("runs = %d, want 2 with the command intact", len(r.runs))
	}
	if !slices.Contains(r.runs[1], "/a b/é/##{x}%H") {
		t.Errorf("new-window = %q, want the dir with # doubled (tmux expands -c as a format)", r.runs[1])
	}
	for _, one := range []string{"true", "/usr/bin/true", "a-b_c.d@e:f+g"} {
		w.Command = []string{one}
		c, r = recordingClient()
		r.out = "%3\t" + w.Session + "\t@3\t1794437573\t" + w.Name + "\n"
		if _, err := c.NewWindow(context.Background(), w); err != nil || len(r.runs) != 2 {
			t.Errorf("NewWindow with command [%q] = %v after %d runs, want it accepted", one, err, len(r.runs))
		}
	}
}

// windowRunner answers each tmux invocation of NewWindow with errs[i] (nil
// when past the end) and out, and records each invocation and its deadline.
type windowRunner struct {
	errs      []error
	out       string
	runs      [][]string
	deadlines []time.Duration
}

func (r *windowRunner) run(ctx context.Context, args ...string) (string, error) {
	i := len(r.runs)
	r.runs = append(r.runs, slices.Clone(args))
	d, ok := ctx.Deadline()
	if !ok {
		d = time.Now().Add(time.Hour)
	}
	r.deadlines = append(r.deadlines, time.Until(d))
	if i < len(r.errs) && r.errs[i] != nil {
		return "", r.errs[i]
	}
	return r.out, nil
}

// TestNewWindowInvocations: has-session decides between new-window and
// new-session; a new-session that lost the race to another process is
// retried once as new-window; nothing else is retried; the whole call has
// 5 s.
func TestNewWindowInvocations(t *testing.T) {
	w := goodWindow()
	w.Env = append(w.Env, "CURIA_DB=/d/x.db")
	tail := []string{
		"-n", "T-1", "-c", "/tmp/wt", "-e", "CURIA_TASK_ID=T-1", "-e", "CURIA_DB=/d/x.db",
		"--", "/usr/local/bin/curia", "launch", "T-1",
	}
	has := []string{"has-session", "-t", "=api"}
	newWindow := append([]string{"new-window", "-d", "-P", "-F", paneFormat, "-t", "=api:"}, tail...)
	newSession := append([]string{"new-session", "-d", "-P", "-F", paneFormat, "-s", "api"}, tail...)
	absent := errors.New("tmux has-session: exit status 1: can't find session: api")
	dup := errors.New("tmux new-session: exit status 1: duplicate session: api")
	other := errors.New("tmux new-session: exit status 1: create window failed")
	for _, tc := range []struct {
		name string
		errs []error
		want [][]string
		ok   bool
	}{
		{name: "session present", want: [][]string{has, newWindow}, ok: true},
		{name: "session absent", errs: []error{absent}, want: [][]string{has, newSession}, ok: true},
		{name: "lost the race", errs: []error{absent, dup}, want: [][]string{has, newSession, newWindow}, ok: true},
		{name: "lost the race, retry fails", errs: []error{absent, dup, other}, want: [][]string{has, newSession, newWindow}},
		{name: "new-session fails otherwise", errs: []error{absent, other}, want: [][]string{has, newSession}},
		{name: "new-window fails", errs: []error{nil, other}, want: [][]string{has, newWindow}},
	} {
		r := &windowRunner{errs: tc.errs, out: "%7\tapi\t@4\t1794437573\tT-1\n"}
		p, err := (&execClient{runner: r.run}).NewWindow(context.Background(), w)
		if !reflect.DeepEqual(r.runs, tc.want) {
			t.Errorf("%s: runs = %q\nwant %q", tc.name, r.runs, tc.want)
		}
		if tc.ok && (err != nil || p != (Pane{ID: "%7", Session: "api", WindowID: "@4", ServerStart: time.Unix(1794437573, 0), WindowName: "T-1"})) {
			t.Errorf("%s: NewWindow = %+v, %v", tc.name, p, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: NewWindow = %+v, want an error", tc.name, p)
		}
		for _, d := range r.deadlines {
			if d <= 4*time.Second || d > windowTimeout || windowTimeout != 5*time.Second {
				t.Errorf("%s: deadline in %v, want the 5 s budget", tc.name, d)
			}
		}
	}
	// The printed line must be one pane.
	for _, out := range []string{"", "\t\t\t\t\n", "%7\tapi\t@4\t1794437573\tT-1\n%8\tapi\t@5\t1794437573\tT-1\n"} {
		r := &windowRunner{out: out}
		if p, err := (&execClient{runner: r.run}).NewWindow(context.Background(), w); err == nil {
			t.Errorf("printed %q: NewWindow = %+v, want an error", out, p)
		}
	}
}

// waitFile waits up to 5 s for path to exist and returns its contents.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Clean(path)); err == nil {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not written within 5s", path)
		}
	}
}

// TestNewWindowArgvIsDirect: the command's arguments reach the program
// exactly as given, with no shell or tmux reading them. The program is
// /bin/sh running a script file (an executable script would need a file
// mode gosec refuses in tests); it writes its arguments, its working
// directory and $CURIA_TASK_ID, then moves the file into place.
func TestNewWindowArgvIsDirect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	realTmux(ctx, t)
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "work #{pane_id} %H")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	script, out := filepath.Join(tmp, "argv.sh"), filepath.Join(tmp, "argv")
	body := `out=$1; shift
{ for a in "$@"; do printf '%s\n' "$a"; done; pwd -P; printf '%s\n' "$CURIA_TASK_ID"; } > "$out.tmp"
mv "$out.tmp" "$out"
`
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"two words", "$(id) `id` #{pane_id}", "a;b"}
	_, err := NewExec(realSocket).NewWindow(ctx, WindowSpec{
		Session: "t", Name: "argv", Dir: dir, Env: []string{"CURIA_TASK_ID=T-7"},
		Command: append([]string{"/bin/sh", script, out}, args...),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(append(args, resolved, "T-7"), "\n") + "\n"
	if got := waitFile(t, out); got != want {
		t.Fatalf("the program saw %q\nwant %q", got, want)
	}
}

// sleepCommand is a pane command that outlives the test.
var sleepCommand = []string{"sleep", "600"}

func TestNewWindowCreatesSessionThenReusesIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	if out, err := tm("new-session", "-d", "-s", "api-server", "-n", "keep"); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	c := NewExec(realSocket)
	dir := t.TempDir()
	open := func(name string) Pane {
		t.Helper()
		p, err := c.NewWindow(ctx, WindowSpec{Session: "api", Name: name, Dir: dir, Command: sleepCommand})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first, second := open("T-1"), open("T-2")
	if first.Session != "api" || first.WindowName != "T-1" || second.Session != "api" || second.WindowName != "T-2" {
		t.Fatalf("panes = %+v, %+v", first, second)
	}
	if first.ID == second.ID || first.WindowID == second.WindowID {
		t.Fatalf("both calls returned the same pane or window: %+v, %+v", first, second)
	}
	panes, err := c.ListPanes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bySession := map[string][]string{}
	for _, p := range panes {
		bySession[p.Session] = append(bySession[p.Session], p.WindowName)
	}
	if !slices.Contains(panes, first) || !slices.Contains(panes, second) {
		t.Errorf("ListPanes = %+v, want both new panes as returned", panes)
	}
	if got := bySession["api"]; !slices.Equal(got, []string{"T-1", "T-2"}) {
		t.Errorf("windows of api = %q, want [T-1 T-2]", got)
	}
	if got := bySession["api-server"]; !slices.Equal(got, []string{"keep"}) {
		t.Errorf("windows of api-server = %q, want it untouched", got)
	}
}

func TestNewWindowConcurrentFirstUse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	c := NewExec(realSocket)
	dir := t.TempDir()
	errs := make(chan error, 5)
	for i := range 5 {
		go func() {
			_, err := c.NewWindow(ctx, WindowSpec{Session: "swarm", Name: fmt.Sprintf("T-%d", i), Dir: dir, Command: sleepCommand})
			errs <- err
		}()
	}
	for range 5 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	out, err := tm("list-windows", "-a", "-F", "#{session_name} #{window_name}")
	if err != nil {
		t.Fatalf("list-windows: %v: %s", err, out)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if name, ok := strings.CutPrefix(line, "swarm "); ok {
			got = append(got, name)
		}
	}
	slices.Sort(got)
	if want := []string{"T-0", "T-1", "T-2", "T-3", "T-4"}; !slices.Equal(got, want) {
		t.Errorf("windows of swarm = %q, want %q (all of %q)", got, want, out)
	}
	sessions, _ := tm("list-sessions", "-F", "#{session_name}")
	if n := strings.Count(sessions, "swarm"); n != 1 {
		t.Errorf("sessions = %q, want swarm once", sessions)
	}
}

func TestNewWindowPaneEndsWithCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	realTmux(ctx, t)
	c := NewExec(realSocket)
	p, err := c.NewWindow(ctx, WindowSpec{Session: "t", Name: "brief", Dir: t.TempDir(), Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		panes, err := c.ListPanes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(panes, func(q Pane) bool { return q.ID == p.ID }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s still listed 2s after its command ended", p.ID)
		}
	}
}

func TestCaptureTailRefusesBadInput(t *testing.T) {
	for _, tc := range []struct {
		pane  string
		lines int
	}{
		{"x", 5}, {"", 5}, {"%1;kill-server", 5}, {"t:0", 5}, {"%5", 0}, {"%5", -1}, {"%5", 2001},
	} {
		c, r := recordingClient()
		if _, err := c.CaptureTail(context.Background(), tc.pane, tc.lines); err == nil {
			t.Errorf("CaptureTail(%q, %d): want an error", tc.pane, tc.lines)
		}
		if len(r.runs) != 0 {
			t.Errorf("CaptureTail(%q, %d) ran tmux: %q", tc.pane, tc.lines, r.runs)
		}
	}
	c, r := recordingClient()
	r.out = "a\nb\n"
	if _, err := c.CaptureTail(context.Background(), "%5", 2000); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"capture-pane", "-p", "-J", "-t", "%5", "-S", "-2000"}}; !reflect.DeepEqual(r.runs, want) {
		t.Errorf("runs = %q, want %q", r.runs, want)
	}
}

// TestCaptureTailLastLines: the tail is the last lines lines that are not
// blank rows at the bottom of the screen.
func TestCaptureTailLastLines(t *testing.T) {
	for _, tc := range []struct {
		out   string
		lines int
		want  string
	}{
		{"1\n2\n3\n4\n\n\n", 2, "3\n4\n"},
		{"1\n2\n", 5, "1\n2\n"},
		{"1\n  \n2\n   \n\n", 2, "  \n2\n"},
		{"\n\n\n", 3, ""},
		{"", 3, ""},
		{"only", 1, "only\n"},
	} {
		c, r := recordingClient()
		r.out = tc.out
		got, err := c.CaptureTail(context.Background(), "%5", tc.lines)
		if err != nil || got != tc.want {
			t.Errorf("CaptureTail of %q, %d = %q, %v; want %q", tc.out, tc.lines, got, err, tc.want)
		}
	}
}

func TestCaptureTailInRealTmux(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm := realTmux(ctx, t)
	id, err := tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "t:", "/bin/sh", "-c",
		`i=1; while [ $i -le 200 ]; do echo $i; i=$((i+1)); done; exec sleep 600`)
	if err != nil {
		t.Fatalf("new-window: %v: %s", err, id)
	}
	pane := strings.TrimSpace(id)
	c := NewExec(realSocket)
	const want = "196\n197\n198\n199\n200\n"
	var got string
	for deadline := time.Now().Add(5 * time.Second); got != want; time.Sleep(20 * time.Millisecond) {
		if got, err = c.CaptureTail(ctx, pane, 5); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("CaptureTail(%s, 5) = %q, want %q", pane, got, want)
		}
	}
	// History and screen together: 150 lines reach back past the screen.
	all, err := c.CaptureTail(ctx, pane, 150)
	if err != nil || !strings.HasPrefix(all, "51\n52\n") || !strings.HasSuffix(all, "\n200\n") || strings.Count(all, "\n") != 150 {
		t.Errorf("CaptureTail(%s, 150) = %q, %v; want lines 51 to 200", pane, all, err)
	}
	for _, tc := range []struct {
		pane  string
		lines int
	}{{pane, 0}, {pane, 2001}, {"x", 5}, {"%999", 5}} {
		if _, err := c.CaptureTail(ctx, tc.pane, tc.lines); err == nil {
			t.Errorf("CaptureTail(%q, %d): want an error", tc.pane, tc.lines)
		}
	}
}

func TestFakeRecordsNewWindowAndCaptureTail(t *testing.T) {
	ctx := context.Background()
	f := &Fake{Panes: []Pane{{ID: "%0", Session: "main", WindowName: "zsh"}}, Tail: "196\n200\n"}
	var c Client = f
	w := WindowSpec{Session: "api", Name: "T-1", Dir: "/w/t1", Env: []string{"A=1", "B=2"}, Command: []string{"curia", "launch", "T-1"}}
	p, err := c.NewWindow(ctx, w)
	if want := (Pane{ID: "%100", Session: "api", WindowName: "T-1"}); err != nil || p != want {
		t.Fatalf("NewWindow = %+v, %v; want %+v", p, err, want)
	}
	w.Name, w.Env = "T-2", nil
	if p, err = c.NewWindow(ctx, w); err != nil || p.ID != "%101" || p.WindowName != "T-2" {
		t.Fatalf("second NewWindow = %+v, %v; want %%101", p, err)
	}
	f.NewWindowErr = errors.New("boom")
	w.Name = "T-3"
	if _, err = c.NewWindow(ctx, w); !errors.Is(err, f.NewWindowErr) {
		t.Fatalf("NewWindow = %v, want NewWindowErr", err)
	}
	if got := []string{f.Panes[1].ID, f.Panes[2].ID}; len(f.Panes) != 3 || !slices.Equal(got, []string{"%100", "%101"}) {
		t.Fatalf("Panes = %+v, want the two opened windows after %%0", f.Panes)
	}
	tail, err := c.CaptureTail(ctx, "%100", 5)
	if err != nil || tail != "196\n200\n" {
		t.Fatalf("CaptureTail = %q, %v; want Tail", tail, err)
	}
	want := []string{
		"NewWindow api T-1 /w/t1 A=1,B=2 curia launch T-1",
		"NewWindow api T-2 /w/t1  curia launch T-1",
		"NewWindow api T-3 /w/t1  curia launch T-1",
		"CaptureTail %100 5",
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("Calls = %q\nwant %q", f.Calls, want)
	}
}

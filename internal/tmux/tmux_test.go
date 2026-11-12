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
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", realSocket, "kill-server").Run() })
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
			_ = exec.Command("tmux", "-L", s, "kill-server").Run()
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

package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
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
	_ = c.SwitchTo(ctx, "%0")
	_ = c.SetWindowOption(ctx, "@0", "@curia", "busy")
	_ = c.DisplayMessage(ctx, "hi")
	_ = c.KillPane(ctx, "%0")
	if len(f.Panes) != 1 || f.Panes[0].ID != "%1" {
		t.Fatalf("Panes after kill = %v", f.Panes)
	}
	want := []string{
		"ListPanes", "PaneInfo %1", "PaneInfo %9", "SwitchTo %0",
		"SetWindowOption @0 @curia busy", "DisplayMessage hi", "KillPane %0",
	}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Fatalf("Calls = %v", f.Calls)
	}
}

// realSocket is per run, so concurrent test runs (several worktrees running
// make check) never kill each other's servers.
var realSocket = fmt.Sprintf("curia-test-%d", os.Getpid())

// realTmux starts a throwaway server on its own socket (never the default
// server) and returns a helper that runs raw tmux commands against it.
func realTmux(ctx context.Context, t *testing.T) func(args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tm := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-L", realSocket}, args...)...).CombinedOutput()
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

	checkHeadlessAndKill(ctx, t, c, tm, p)
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

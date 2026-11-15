package main

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

// readSession is session id in state st in pane, in window @<pane number>,
// started after the fake server.
func readSession(id string, st model.State, pane string) model.Session {
	return model.Session{
		ID: id, Agent: "claude", State: st, Pane: pane, TmuxSession: "work",
		TmuxWindow: "@" + strings.TrimPrefix(pane, "%"), StartedAt: at(9, 0), UpdatedAt: at(11, 0),
	}
}

// readHome isolates HOME and CURIA_DB, fixes the clock, seeds sessions and
// makes the commands use a tmux.Fake whose live panes are panes, showing
// Claude Code's idle prompt. The caller is the user at a terminal unless
// the test says otherwise (userTerminalAs). It returns the database path and
// the fake.
func readHome(t *testing.T, panes []string, sessions ...model.Session) (string, *tmux.Fake) {
	t.Helper()
	db := hookHome(t)
	userTerminalAs(t, true)
	prevClock := clock
	clock = func() time.Time { return at(12, 0) }
	t.Cleanup(func() { clock = prevClock; readLines = readDefaultLines })
	seedSessions(t, db, sessions...)
	fake := useFakeTmux(t)
	for _, p := range panes {
		fake.Panes = append(fake.Panes, tmux.Pane{
			ID: p, Session: "work", WindowID: "@" + strings.TrimPrefix(p, "%"), ServerStart: at(8, 0),
		})
	}
	fake.Screen = decideIdleScreen
	return db, fake
}

// readRefusalHome is the fleet every refusal test runs against: %9 is a
// live pane with no session; %2 holds an ended session, %3 a stale one; %4's
// session is live in the store but its pane is gone, so reconciling marks
// it stale.
func readRefusalHome(t *testing.T) (string, *tmux.Fake) {
	t.Helper()
	return readHome(t, []string{"%2", "%3", "%9"},
		readSession("ended", model.StateEnded, "%2"),
		readSession("stale", model.StateStale, "%3"),
		readSession("gone", model.StateIdle, "%4"))
}

// readRefusedPanes are the panes neither command accepts.
var readRefusedPanes = []string{"%9", "%2", "%3", "%4", "abc", "%1;x", "", "%", "1", "%1 ", "%-1"}

// readRun runs `curia <args>` in-process with stdin and returns stdout;
// stderr is discarded.
func readRun(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetIn(strings.NewReader(stdin))
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// readCaptured reports whether fake was asked for any pane's scrollback.
func readCaptured(fake *tmux.Fake) bool {
	return slices.ContainsFunc(fake.Calls, func(c string) bool { return strings.HasPrefix(c, "CaptureTail") })
}

func TestReadPrintsTailOfAgentPane(t *testing.T) {
	_, fake := readHome(t, []string{"%5"}, readSession("s5", model.StateWorking, "%5"))
	var tail strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&tail, "line %d\n", i)
	}
	fake.Tail = tail.String()

	out, err := readRun(t, "", "read", "%5", "--lines", "5")
	if err != nil {
		t.Fatal(err)
	}
	if want := "line 56\nline 57\nline 58\nline 59\nline 60\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if !slices.Contains(fake.Calls, "CaptureTail %5 5") {
		t.Errorf("calls = %q, want CaptureTail %%5 5", fake.Calls)
	}
	if i := slices.Index(fake.Calls, "ListPanes"); i < 0 || i > slices.Index(fake.Calls, "CaptureTail %5 5") {
		t.Errorf("calls = %q, want ListPanes (reconcile) before the capture", fake.Calls)
	}
}

func TestReadDefaultsToFiftyLines(t *testing.T) {
	_, fake := readHome(t, []string{"%5"}, readSession("s5", model.StateIdle, "%5"))
	fake.Tail = "only\n"
	out, err := readRun(t, "", "read", "%5")
	if err != nil {
		t.Fatal(err)
	}
	if out != "only\n" || !slices.Contains(fake.Calls, "CaptureTail %5 50") {
		t.Errorf("stdout %q, calls %q; want %q and CaptureTail %%5 50", out, fake.Calls, "only\n")
	}
}

func TestReadRefusesNonAgentPane(t *testing.T) {
	for _, pane := range readRefusedPanes {
		t.Run(pane, func(t *testing.T) {
			_, fake := readRefusalHome(t)
			fake.Tail = "secret\n"
			out, err := readRun(t, "", "read", pane)
			if err == nil {
				t.Errorf("read %q: no error, stdout %q", pane, out)
			}
			if out != "" || readCaptured(fake) {
				t.Errorf("read %q: stdout %q, calls %q; want nothing read", pane, out, fake.Calls)
			}
		})
	}
}

func TestReadAcceptsSharedPane(t *testing.T) {
	_, fake := readHome(t, []string{"%1"},
		readSession("a", model.StateIdle, "%1"), readSession("b", model.StateWorking, "%1"))
	fake.Tail = "shared\n"
	out, err := readRun(t, "", "read", "%1")
	if err != nil || out != "shared\n" {
		t.Errorf("read of a shared pane = %q, %v; want %q", out, err, "shared\n")
	}
}

func TestReadOutputIsSanitised(t *testing.T) {
	_, fake := readHome(t, []string{"%5"}, readSession("s5", model.StateIdle, "%5"))
	long := strings.Repeat("é", 2000)
	fake.Tail = "    indented \x1b[31mred\x1b[0m and\x07 bell\n" +
		"\x1b]0;evil title\x07after \u202erevered\n" +
		"\x1b[2J\x1bPdcs\x1b\\ok\r\n" +
		long + "\n"

	out, err := readRun(t, "", "read", "%5", "--lines", "10")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\x1b", "\x07", "\u202e", "\r", "evil title", "dcs"} {
		if strings.Contains(out, bad) {
			t.Errorf("stdout holds %q: %q", bad, out)
		}
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("stdout has %d lines, want 4: %q", len(lines), out)
	}
	if want := "    indented red and bell"; lines[0] != want {
		t.Errorf("line 1 = %q, want %q (indentation kept)", lines[0], want)
	}
	if want := "after revered"; lines[1] != want {
		t.Errorf("line 2 = %q, want %q", lines[1], want)
	}
	if lines[2] != "ok" {
		t.Errorf("line 3 = %q, want %q", lines[2], "ok")
	}
	if n := utf8.RuneCountInString(lines[3]); n != readLineRunes || !utf8.ValidString(lines[3]) {
		t.Errorf("line 4 has %d runes (valid UTF-8 %v), want %d", n, utf8.ValidString(lines[3]), readLineRunes)
	}
}

func TestReadLinesBounds(t *testing.T) {
	for _, n := range []string{"0", "2001", "-1", "x"} {
		_, fake := readHome(t, []string{"%5"}, readSession("s5", model.StateIdle, "%5"))
		if _, err := readRun(t, "", "read", "%5", "--lines", n); err == nil {
			t.Errorf("read --lines %s: no error", n)
		}
		if readCaptured(fake) {
			t.Errorf("read --lines %s: calls %q, want no capture", n, fake.Calls)
		}
	}
	for _, n := range []string{"1", "2000"} {
		_, fake := readHome(t, []string{"%5"}, readSession("s5", model.StateIdle, "%5"))
		fake.Tail = "x\n"
		if _, err := readRun(t, "", "read", "%5", "--lines", n); err != nil {
			t.Errorf("read --lines %s: %v", n, err)
		}
		if !slices.Contains(fake.Calls, "CaptureTail %5 "+n) {
			t.Errorf("read --lines %s: calls %q", n, fake.Calls)
		}
	}
}

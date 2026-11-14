package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/tmux"
)

// The test guard (plan section 6.8). cmd/curia tests run commands
// in-process, and they are often run inside a real tmux session and a real
// Claude Code session. TestMain makes the safe environment the default, so a
// test that forgets its helpers cannot reach the developer's home, database,
// tmux server or agent session.

// guardUnset are the variables TestMain removes: the tmux client, Claude Code
// and Curia caller variables a developer's shell may carry.
var guardUnset = []string{
	"TMUX", "TMUX_PANE", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID",
	"CLAUDE_PROJECT_DIR", "CLAUDE_CONFIG_DIR", "CURIA_SESSION_ID", "SSH_CONNECTION",
}

var (
	// guardDir is the temp directory holding the guarded HOME and CURIA_DB.
	guardDir string
	// guardOuterHome is HOME as it was before TestMain replaced it.
	guardOuterHome string
)

func TestMain(m *testing.M) {
	os.Exit(runGuarded(m))
}

// runGuarded sets up the guard, runs the tests and removes the temp dirs.
func runGuarded(m *testing.M) int {
	dir, err := os.MkdirTemp("", "curia-cmd-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test guard:", err)
		return 1
	}
	defer removeGuardDir(dir)
	// A short path: a unix socket path is limited to about 100 bytes.
	tmuxDir, err := os.MkdirTemp("", "ct")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test guard:", err)
		return 1
	}
	defer removeGuardDir(tmuxDir)
	if err := guardEnv(dir, tmuxDir); err != nil {
		fmt.Fprintln(os.Stderr, "test guard:", err)
		return 1
	}
	newTmuxClient = func() tmux.Client { return unreachableTmux{} }
	return m.Run()
}

// removeGuardDir removes one of the guard's temp dirs, reporting a failure.
func removeGuardDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "test guard: cleanup:", err)
	}
}

// guardEnv points HOME and CURIA_DB into dir, TMUX_TMPDIR at the empty
// tmuxDir, and unsets guardUnset.
func guardEnv(dir, tmuxDir string) error {
	guardDir, guardOuterHome = dir, os.Getenv("HOME")
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return err
	}
	set := [][2]string{
		{"HOME", home},
		{"CURIA_DB", filepath.Join(dir, "curia.db")},
		{"TMUX_TMPDIR", tmuxDir},
	}
	for _, kv := range set {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			return err
		}
	}
	for _, name := range guardUnset {
		if err := os.Unsetenv(name); err != nil {
			return err
		}
	}
	return nil
}

// errTmuxUnreachable is what unreachableTmux returns from every method.
var errTmuxUnreachable = errors.New("tmux is unreachable in cmd/curia tests (TestMain guard); use useFakeTmux")

// unreachableTmux is the default tmux client under the test guard: every
// method fails, so no test can reach a real tmux server by accident. A new
// tmux.Client method needs one more line here.
type unreachableTmux struct{}

var _ tmux.Client = unreachableTmux{}

func (unreachableTmux) ListPanes(context.Context) ([]tmux.Pane, error) {
	return nil, errTmuxUnreachable
}

func (unreachableTmux) PaneInfo(context.Context, string) (tmux.Pane, error) {
	return tmux.Pane{}, errTmuxUnreachable
}

func (unreachableTmux) PaneFocused(context.Context, string) (bool, error) {
	return false, errTmuxUnreachable
}

func (unreachableTmux) SwitchTo(context.Context, string) error { return errTmuxUnreachable }

func (unreachableTmux) SetWindowOption(context.Context, string, string, string) error {
	return errTmuxUnreachable
}

func (unreachableTmux) DisplayMessage(context.Context, string) error { return errTmuxUnreachable }

func (unreachableTmux) KillPane(context.Context, string) error { return errTmuxUnreachable }

func (unreachableTmux) SendKeys(context.Context, string, string) error { return errTmuxUnreachable }

func (unreachableTmux) CapturePane(context.Context, string) (tmux.Capture, error) {
	return tmux.Capture{}, errTmuxUnreachable
}

func (unreachableTmux) Bell(context.Context, string) error { return errTmuxUnreachable }

func TestGuardIsolatesEnvironment(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" || !strings.HasPrefix(home, guardDir+string(os.PathSeparator)) {
		t.Errorf("HOME = %q, want a directory inside the guard's %q", home, guardDir)
	}
	if guardOuterHome != "" && home == guardOuterHome {
		t.Errorf("HOME is the home the tests were started with (%q)", home)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" && home == u.HomeDir {
		t.Errorf("HOME is the real home of %s (%q)", u.Username, home)
	}
	if db := os.Getenv("CURIA_DB"); !strings.HasPrefix(db, guardDir+string(os.PathSeparator)) {
		t.Errorf("CURIA_DB = %q, want a path inside %q", db, guardDir)
	}
	for _, name := range guardUnset {
		if v, ok := os.LookupEnv(name); ok {
			t.Errorf("%s is set (%q); the guard unsets it", name, v)
		}
	}
	if _, err := newTmuxClient().ListPanes(context.Background()); !errors.Is(err, errTmuxUnreachable) {
		t.Errorf("default tmux client ListPanes error = %v, want errTmuxUnreachable", err)
	}
}

func TestVersionFlagPrintsVersion(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"--version"})
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("Execute(--version) returned error: %v", err)
	}

	want := "curia version " + version + "\n"
	if got := out.String(); got != want {
		t.Fatalf("--version output = %q, want %q", got, want)
	}
}

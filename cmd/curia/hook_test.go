package main

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/hook"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// hookResult is what one in-process run of `curia hook` produced.
type hookResult struct {
	exitCode, exits int
	output          string
	log             []string
}

// hookHome points HOME (and so the default log and config paths) and
// CURIA_DB at a temp dir, outside tmux, and returns the database path.
func hookHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMUX_PANE", "")
	db := filepath.Join(home, "curia.db")
	t.Setenv("CURIA_DB", db)
	return db
}

// runHookCmd runs `curia <args>` in-process with stdin and a fake exit func.
func runHookCmd(t *testing.T, stdin io.Reader, args ...string) hookResult {
	t.Helper()
	res := hookResult{exitCode: -1}
	prev := hookExit
	hookExit = func(code int) { res.exitCode = code; res.exits++ }
	var out bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		hookExit = prev
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	if err := rootCmd.Execute(); err != nil {
		t.Errorf("Execute returned %v", err)
	}
	res.output = out.String()
	res.log = readLog(t)
	return res
}

func readLog(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(config.Default().LogPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// assertQuietExitZero checks the hook exited 0 exactly once and printed nothing.
func assertQuietExitZero(t *testing.T, res hookResult) {
	t.Helper()
	if res.exitCode != 0 || res.exits != 1 {
		t.Errorf("exit code %d after %d exit calls, want one exit(0)", res.exitCode, res.exits)
	}
	if res.output != "" {
		t.Errorf("hook wrote to stdout/stderr: %q", res.output)
	}
}

func TestMalformedPayloadExitsZero(t *testing.T) {
	hookHome(t)
	res := runHookCmd(t, strings.NewReader("{"), "hook", "PostToolUse")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 {
		t.Fatalf("log has %d lines, want 1: %q", len(res.log), res.log)
	}
	if !strings.Contains(res.log[0], "PostToolUse") {
		t.Errorf("log line does not name the event: %q", res.log[0])
	}
}

func TestHookExitsZeroWhenStoreLocked(t *testing.T) {
	db := hookHome(t)
	st, err := store.Open(db) // create the file in WAL mode first
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()

	start := time.Now()
	res := runHookCmd(t, strings.NewReader(`{"session_id":"A","cwd":"/repo"}`), "hook", "PostToolUse")
	elapsed := time.Since(start)

	assertQuietExitZero(t, res)
	if elapsed > 3*time.Second {
		t.Errorf("hook took %v with the store locked, want under 3s", elapsed)
	}
	if len(res.log) != 1 || !strings.Contains(res.log[0], "locked") {
		t.Errorf("log = %q, want one line reporting the lock", res.log)
	}
}

func TestHookRecordsSession(t *testing.T) {
	db := hookHome(t)
	body := `{"session_id":"A","transcript_path":"/nonexistent/t.jsonl","cwd":"` + t.TempDir() +
		`","hook_event_name":"SessionStart","source":"startup"}`
	res := runHookCmd(t, strings.NewReader(body), "hook", "SessionStart")
	assertQuietExitZero(t, res)
	if res.log != nil {
		t.Errorf("log after a good event: %q", res.log)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sess, found, err := st.GetSession(context.Background(), "A")
	if err != nil || !found {
		t.Fatalf("GetSession = found %v, err %v", found, err)
	}
	if sess.State != model.StateWorking || sess.Pane != "" {
		t.Errorf("session = %+v, want working with no pane", sess)
	}
}

func TestHookBadInvocationsExitZero(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"unknown event", `{"session_id":"A"}`, []string{"hook", "PreCompact"}},
		{"no event", `{"session_id":"A"}`, []string{"hook"}},
		{"two events", `{"session_id":"A"}`, []string{"hook", "Stop", "Stop"}},
		{"flag-like event", `{"session_id":"A"}`, []string{"hook", "--bogus"}},
		{
			"oversized payload", `{"session_id":"A","pad":"` + strings.Repeat("x", maxPayloadBytes) + `"}`,
			[]string{"hook", "Stop"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := hookHome(t)
			res := runHookCmd(t, strings.NewReader(tc.stdin), tc.args...)
			assertQuietExitZero(t, res)
			if len(res.log) != 1 {
				t.Errorf("log = %q, want one line", res.log)
			}
			if _, err := os.Stat(db); err == nil {
				st, err := store.Open(db)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = st.Close() }()
				if all, _ := st.ListSessions(context.Background(), store.ListFilter{IncludeEnded: true}); len(all) != 0 {
					t.Errorf("sessions recorded: %+v", all)
				}
			}
		})
	}
}

func TestHookPanicExitsZero(t *testing.T) {
	hookHome(t)
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (model.Session, error) {
		panic("boom\nforged line")
	}
	t.Cleanup(func() { hookRun = prev })

	res := runHookCmd(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Stop")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 || !strings.Contains(res.log[0], "boom") {
		t.Errorf("log = %q, want one line with the panic", res.log)
	}
}

func TestHookLogCannotBeForged(t *testing.T) {
	hookHome(t)
	res := runHookCmd(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Bogus\ntime=2026 level=INFO msg=forged")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 {
		t.Errorf("log = %q, want the untrusted text kept on one line", res.log)
	}
}

func TestGitInfo(t *testing.T) {
	// A git hook (lefthook's pre-commit runs these tests) exports GIT_DIR and
	// friends; gitInfo must look at cwd regardless.
	other := t.TempDir()
	gitInit(t, other, "main")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)

	dir := t.TempDir()
	if repo, branch := gitInfo(context.Background(), dir); repo != "" || branch != "" {
		t.Errorf("outside a repo: %q, %q", repo, branch)
	}
	if repo, branch := gitInfo(context.Background(), ""); repo != "" || branch != "" {
		t.Errorf("empty cwd: %q, %q", repo, branch)
	}
	gitInit(t, dir, "feat/x")
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	repo, branch := gitInfo(context.Background(), sub)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	if repo != wantRepo || branch != "feat/x" {
		t.Errorf("gitInfo = %q, %q, want %q, feat/x", repo, branch, wantRepo)
	}
}

// gitInit makes dir a git repository on branch. It runs git with gitEnv, so
// a GIT_DIR inherited from a git hook cannot make it reinitialise (and mark
// bare) the repository running the tests.
func gitInit(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", "-b", branch, dir)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

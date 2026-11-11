package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useFakeGit puts a "git" on PATH that is a symlink to /bin/sh and returns
// the cwd to pass to gitInfo: a shell script file. gitInfo runs
// `git -C <cwd> <args>`, which /bin/sh reads as -C (noclobber) and then runs
// the script with <args> as $1, $2. (A real executable would need a file
// mode gosec rejects in tests.)
func useFakeGit(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("/bin/sh", filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(dir, "fake-git.sh")
	if err := os.WriteFile(cwd, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return cwd
}

// hermeticGit keeps the real git from reading the user's config or finding
// a repository above the test's temp dirs. It returns a fresh temp dir.
func hermeticGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return dir
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

func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func logLines(buf *bytes.Buffer) []string {
	if buf.Len() == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
}

func TestGitInfo(t *testing.T) {
	dir := hermeticGit(t)
	// A git hook (lefthook's pre-commit runs these tests) exports GIT_DIR and
	// friends; gitInfo must look at cwd regardless.
	other := t.TempDir()
	gitInit(t, other, "main")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	logger, buf := testLogger()

	if repo, branch := gitInfo(context.Background(), dir, logger); repo != "" || branch != "" {
		t.Errorf("outside a repo: %q, %q", repo, branch)
	}
	if repo, branch := gitInfo(context.Background(), "", logger); repo != "" || branch != "" {
		t.Errorf("empty cwd: %q, %q", repo, branch)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("logged for a directory outside a repo: %q", lines)
	}

	gitInit(t, dir, "feat/x")
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	repo, branch := gitInfo(context.Background(), sub, logger)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	if repo != wantRepo || branch != "feat/x" {
		t.Errorf("gitInfo = %q, %q, want %q, feat/x", repo, branch, wantRepo)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("logged for a good repo: %q", lines)
	}
}

func TestGitInfoLogsMissingGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	logger, buf := testLogger()
	if repo, branch := gitInfo(context.Background(), t.TempDir(), logger); repo != "" || branch != "" {
		t.Errorf("without git: %q, %q", repo, branch)
	}
	if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], "git failed") {
		t.Errorf("log = %q, want one git failure", lines)
	}
}

func TestGitInfoLogsTimeout(t *testing.T) {
	// sleep is a grandchild holding stdout after git (sh) is killed, as a
	// hook or pager git started might be.
	cwd := useFakeGit(t, "/bin/sleep 3\nexit 0\n")
	logger, buf := testLogger()
	start := time.Now()
	repo, branch := gitInfo(context.Background(), cwd, logger)
	// The grandchild keeps stdout open for 3s; gitWaitDelay stops the wait.
	if elapsed := time.Since(start); elapsed > subprocessTimeout+time.Second {
		t.Errorf("gitInfo took %v, want about %v", elapsed, subprocessTimeout)
	}
	if repo != "" || branch != "" {
		t.Errorf("after a timeout: %q, %q", repo, branch)
	}
	if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], "timed out") {
		t.Errorf("log = %q, want one timeout line", lines)
	}
}

func TestGitInfoLogsBranchFailure(t *testing.T) {
	// A git too old for branch --show-current.
	cwd := useFakeGit(t, `case "$1" in
rev-parse) echo /fake/repo ;;
*) echo 'error: unknown option show-current' >&2; exit 129 ;;
esac
`)
	logger, buf := testLogger()
	repo, branch := gitInfo(context.Background(), cwd, logger)
	if repo != "/fake/repo" || branch != "" {
		t.Errorf("gitInfo = %q, %q, want /fake/repo and no branch", repo, branch)
	}
	if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], "show-current") {
		t.Errorf("log = %q, want one line about the branch call", lines)
	}
}

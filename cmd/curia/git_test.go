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

// fakeGitArgs is the argv gitInfo passes after `-C <cwd>`.
const fakeGitArgs = "rev-parse --show-toplevel --absolute-git-dir"

// useFakeGit puts a "git" on PATH that is a symlink to /bin/sh and returns
// the cwd to pass to gitInfo: a shell script file. gitInfo runs
// `git -C <cwd> <args>`, which /bin/sh reads as -C (noclobber) and then runs
// the script with <args> as $1, $2. (A real executable would need a file
// mode gosec rejects in tests.) This depends on gitInfo's argv: should it
// change, sh rejects the options, or the script exits 97 naming the
// arguments, and the test sees that failure in the log rather than passing.
// Every run appends a line to the returned calls file.
func useFakeGit(t *testing.T, script string) (cwd, calls string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("/bin/sh", filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	cwd = filepath.Join(dir, "fake-git.sh")
	calls = filepath.Join(dir, "calls")
	guard := "echo run >> '" + calls + "'\n" +
		`[ "$*" = "` + fakeGitArgs + `" ] || { echo "fake git: unexpected arguments: $*" >&2; exit 97; }` + "\n"
	if err := os.WriteFile(cwd, []byte(guard+script), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return cwd, calls
}

// gitRun runs git in dir with gitEnv and a fixed identity.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// fakeGitDir makes a directory holding a HEAD file with head as content,
// for the fake git to print as its git dir.
func fakeGitDir(t *testing.T, head string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte(head), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
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

	gitInit(t, dir, "feat/x") // no commits yet: HEAD names an unborn branch
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
	cwd, _ := useFakeGit(t, "/bin/sleep 5\nexit 0\n")
	logger, buf := testLogger()
	start := time.Now()
	repo, branch := gitInfo(context.Background(), cwd, logger)
	// The grandchild keeps stdout open for 5s; gitWaitDelay stops the wait
	// soon after the timeout, well before the grandchild exits.
	elapsed := time.Since(start)
	if elapsed < subprocessTimeout || elapsed > subprocessTimeout+time.Second {
		t.Errorf("gitInfo took %v, want about %v", elapsed, subprocessTimeout)
	}
	if repo != "" || branch != "" {
		t.Errorf("after a timeout: %q, %q", repo, branch)
	}
	if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], "timed out") {
		t.Errorf("log = %q, want one timeout line", lines)
	}
}

// Ruling 30: one git process per SessionStart; the branch comes from the HEAD
// file in the git dir it reports.
func TestGitInfoRunsOneGitProcess(t *testing.T) {
	gitDir := fakeGitDir(t, "ref: refs/heads/feat/one\n")
	cwd, calls := useFakeGit(t, "printf '/fake/repo\\n%s\\n' '"+gitDir+"'\n")
	logger, buf := testLogger()
	repo, branch := gitInfo(context.Background(), cwd, logger)
	if repo != "/fake/repo" || branch != "feat/one" {
		t.Errorf("gitInfo = %q, %q, want /fake/repo, feat/one", repo, branch)
	}
	if b, _ := os.ReadFile(filepath.Clean(calls)); string(b) != "run\n" {
		t.Errorf("git ran %d times, want once", strings.Count(string(b), "run"))
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("log = %q, want none", lines)
	}
}

func TestGitInfoLinkedWorktree(t *testing.T) {
	dir := hermeticGit(t)
	gitInit(t, dir, "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, dir, "worktree", "add", "-q", "-b", "feat/wt", wt)
	logger, buf := testLogger()
	repo, branch := gitInfo(context.Background(), wt, logger)
	wantRepo, _ := filepath.EvalSymlinks(wt)
	if repo != wantRepo || branch != "feat/wt" {
		t.Errorf("gitInfo = %q, %q, want %q, feat/wt", repo, branch, wantRepo)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("log = %q, want none", lines)
	}
}

func TestGitInfoDetachedHead(t *testing.T) {
	dir := hermeticGit(t)
	gitInit(t, dir, "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	gitRun(t, dir, "checkout", "-q", "--detach")
	logger, buf := testLogger()
	repo, branch := gitInfo(context.Background(), dir, logger)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	if repo != wantRepo || branch != "" {
		t.Errorf("gitInfo = %q, %q, want %q and no branch", repo, branch, wantRepo)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("log = %q, want none", lines)
	}
}

// The HEAD file is untrusted: an oversized one is not read whole and gives
// no branch; a missing one is logged.
func TestGitInfoBadHeadFile(t *testing.T) {
	for name, tc := range map[string]struct {
		gitDir, wantLog string
	}{
		"oversized": {fakeGitDir(t, "ref: refs/heads/"+strings.Repeat("x", 1<<20)), "HEAD"},
		"missing":   {t.TempDir(), "HEAD"},
	} {
		t.Run(name, func(t *testing.T) {
			cwd, _ := useFakeGit(t, "printf '/fake/repo\\n%s\\n' '"+tc.gitDir+"'\n")
			logger, buf := testLogger()
			repo, branch := gitInfo(context.Background(), cwd, logger)
			if repo != "/fake/repo" || branch != "" {
				t.Errorf("gitInfo = %q, %q, want /fake/repo and no branch", repo, branch)
			}
			if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], tc.wantLog) {
				t.Errorf("log = %q, want one line about %s", lines, tc.wantLog)
			}
		})
	}
}

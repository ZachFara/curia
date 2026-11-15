package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeGitArgs is the argv gitInfo passes after `-C <cwd>`.
const fakeGitArgs = "rev-parse --path-format=absolute --show-toplevel --absolute-git-dir --git-common-dir"

// useFakeGit puts a "git" on PATH that is a symlink to /bin/sh and returns
// the cwd to pass to gitInfo: a shell script file. gitInfo runs
// `git -C <cwd> <args>`, which /bin/sh reads as -C (noclobber) and then runs
// the script with <args> as $1, $2, ... (A real executable would need a file
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

	if repo, branch, wt := gitInfo(context.Background(), dir, logger); repo != "" || branch != "" || wt != "" {
		t.Errorf("outside a repo: %q, %q, %q", repo, branch, wt)
	}
	if repo, branch, wt := gitInfo(context.Background(), "", logger); repo != "" || branch != "" || wt != "" {
		t.Errorf("empty cwd: %q, %q, %q", repo, branch, wt)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("logged for a directory outside a repo: %q", lines)
	}

	gitInit(t, dir, "feat/x") // no commits yet: HEAD names an unborn branch
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	repo, branch, wt := gitInfo(context.Background(), sub, logger)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	if repo != wantRepo || branch != "feat/x" || wt != "" {
		t.Errorf("gitInfo = %q, %q, %q, want %q, feat/x and no worktree", repo, branch, wt, wantRepo)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("logged for a good repo: %q", lines)
	}
}

func TestGitInfoLogsMissingGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	logger, buf := testLogger()
	if repo, branch, wt := gitInfo(context.Background(), t.TempDir(), logger); repo != "" || branch != "" || wt != "" {
		t.Errorf("without git: %q, %q, %q", repo, branch, wt)
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
	repo, branch, wt := gitInfo(context.Background(), cwd, logger)
	// The grandchild keeps stdout open for 5s; gitWaitDelay stops the wait
	// soon after the timeout, well before the grandchild exits.
	elapsed := time.Since(start)
	if elapsed < subprocessTimeout || elapsed > subprocessTimeout+time.Second {
		t.Errorf("gitInfo took %v, want about %v", elapsed, subprocessTimeout)
	}
	if repo != "" || branch != "" || wt != "" {
		t.Errorf("after a timeout: %q, %q, %q", repo, branch, wt)
	}
	if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], "timed out") {
		t.Errorf("log = %q, want one timeout line", lines)
	}
}

// Ruling 30 and plan section 4.5: one git process per SessionStart gives the
// top level, the git dir and the common dir; the branch comes from the HEAD
// file in the git dir it reports. Here the common dir names a linked
// worktree of /fake/repo.
func TestGitInfoIsOneProcess(t *testing.T) {
	gitDir := fakeGitDir(t, "ref: refs/heads/feat/one\n")
	cwd, calls := useFakeGit(t, "printf '/fake/wt\\n%s\\n/fake/repo/.git\\n' '"+gitDir+"'\n")
	logger, buf := testLogger()
	repo, branch, wt := gitInfo(context.Background(), cwd, logger)
	if repo != "/fake/repo" || branch != "feat/one" || wt != "/fake/wt" {
		t.Errorf("gitInfo = %q, %q, %q, want /fake/repo, feat/one, /fake/wt", repo, branch, wt)
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
	repo, branch, worktree := gitInfo(context.Background(), wt, logger)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	wantWorktree, _ := filepath.EvalSymlinks(wt)
	if repo != wantRepo || branch != "feat/wt" || worktree != wantWorktree {
		t.Errorf("gitInfo = %q, %q, %q, want %q, feat/wt, %q", repo, branch, worktree, wantRepo, wantWorktree)
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
	repo, branch, wt := gitInfo(context.Background(), dir, logger)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	if repo != wantRepo || branch != "" || wt != "" {
		t.Errorf("gitInfo = %q, %q, %q, want %q and no branch or worktree", repo, branch, wt, wantRepo)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("log = %q, want none", lines)
	}
}

// A HEAD that is a FIFO (a hostile or broken checkout) must not block the
// hook: opening or reading it would wait for a writer that never comes.
func TestGitInfoHeadFIFODoesNotBlock(t *testing.T) {
	gitDir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(gitDir, "HEAD"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, _ := useFakeGit(t, "printf '/fake/repo\\n%s\\n/fake/repo/.git\\n' '"+gitDir+"'\n")
	logger, buf := testLogger()
	start := time.Now()
	done := make(chan [3]string, 1)
	go func() {
		repo, branch, wt := gitInfo(context.Background(), cwd, logger)
		done <- [3]string{repo, branch, wt}
	}()
	select {
	case got := <-done:
		if got != [3]string{"/fake/repo", "", ""} {
			t.Errorf("gitInfo = %q, want /fake/repo and no branch", got)
		}
	case <-time.After(subprocessTimeout + time.Second):
		t.Fatalf("gitInfo still blocked on a FIFO HEAD after %v", time.Since(start))
	}
	if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], "HEAD") {
		t.Errorf("log = %q, want one line about HEAD", lines)
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
			cwd, _ := useFakeGit(t, "printf '/fake/repo\\n%s\\n/fake/repo/.git\\n' '"+tc.gitDir+"'\n")
			logger, buf := testLogger()
			repo, branch, wt := gitInfo(context.Background(), cwd, logger)
			if repo != "/fake/repo" || branch != "" || wt != "" {
				t.Errorf("gitInfo = %q, %q, %q, want /fake/repo and no branch or worktree", repo, branch, wt)
			}
			if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], tc.wantLog) {
				t.Errorf("log = %q, want one line about %s", lines, tc.wantLog)
			}
		})
	}
}

func TestGitInfoMainWorktree(t *testing.T) {
	dir := hermeticGit(t)
	gitInit(t, dir, "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	// A linked worktree elsewhere does not change what the main one reports.
	gitRun(t, dir, "worktree", "add", "-q", "-b", "feat/wt", filepath.Join(t.TempDir(), "wt"))
	logger, buf := testLogger()
	repo, branch, wt := gitInfo(context.Background(), dir, logger)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	if repo != wantRepo || branch != "main" || wt != "" {
		t.Errorf("gitInfo = %q, %q, %q, want %q, main and no worktree", repo, branch, wt, wantRepo)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("log = %q, want none", lines)
	}
}

func TestGitInfoSubdirectoryOfWorktree(t *testing.T) {
	dir := hermeticGit(t)
	gitInit(t, dir, "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, dir, "worktree", "add", "-q", "-b", "curia/T-7-x", wt)
	sub := filepath.Join(wt, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	logger, buf := testLogger()
	repo, branch, worktree := gitInfo(context.Background(), sub, logger)
	wantRepo, _ := filepath.EvalSymlinks(dir)
	wantWorktree, _ := filepath.EvalSymlinks(wt)
	if repo != wantRepo || branch != "curia/T-7-x" || worktree != wantWorktree {
		t.Errorf("gitInfo = %q, %q, %q, want %q, curia/T-7-x, %q", repo, branch, worktree, wantRepo, wantWorktree)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("log = %q, want none", lines)
	}
}

// Only a common dir named .git outside the top level marks a linked
// worktree; any other (a submodule's, under .git/modules) leaves the top
// level as the repository.
func TestGitInfoRepoFromCommonDir(t *testing.T) {
	gitDir := fakeGitDir(t, "ref: refs/heads/main\n")
	for common, want := range map[string][2]string{
		"/fake/top/.git":             {"/fake/top", ""},
		"/fake/main/.git":            {"/fake/main", "/fake/top"},
		"/fake/super/.git/modules/x": {"/fake/top", ""},
	} {
		cwd, _ := useFakeGit(t, "printf '/fake/top\\n%s\\n%s\\n' '"+gitDir+"' '"+common+"'\n")
		logger, buf := testLogger()
		repo, branch, wt := gitInfo(context.Background(), cwd, logger)
		if repo != want[0] || branch != "main" || wt != want[1] {
			t.Errorf("common dir %s: gitInfo = %q, %q, %q, want %q, main, %q", common, repo, branch, wt, want[0], want[1])
		}
		if lines := logLines(buf); lines != nil {
			t.Errorf("common dir %s: log = %q, want none", common, lines)
		}
	}
}

// Output that is not three absolute paths is logged once and yields
// nothing.
func TestGitInfoUnexpectedOutput(t *testing.T) {
	gitDir := fakeGitDir(t, "ref: refs/heads/main\n")
	for name, out := range map[string]string{
		"two lines":     "/fake/top\n" + gitDir + "\n",
		"four lines":    "/fake/top\n" + gitDir + "\n/fake/top/.git\n/extra\n",
		"empty line":    "/fake/top\n\n/fake/top/.git\n",
		"relative path": "fake/top\n" + gitDir + "\n/fake/top/.git\n",
	} {
		t.Run(name, func(t *testing.T) {
			cwd, _ := useFakeGit(t, "printf '%s' '"+out+"'\n")
			logger, buf := testLogger()
			if repo, branch, wt := gitInfo(context.Background(), cwd, logger); repo != "" || branch != "" || wt != "" {
				t.Errorf("gitInfo = %q, %q, %q, want nothing", repo, branch, wt)
			}
			if lines := logLines(buf); len(lines) != 1 || !strings.Contains(lines[0], "unexpected output") {
				t.Errorf("log = %q, want one line about unexpected output", lines)
			}
		})
	}
}

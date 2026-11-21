package main

// Git fixtures shared with internal/gitops's tests (copied: test helpers are
// not importable across packages).

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/gitops"
)

// gitRun runs git in dir with Env and a fixed identity.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Env = gitops.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
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

// gitInit makes dir a git repository on branch. It runs git with Env, so
// a GIT_DIR inherited from a git hook cannot make it reinitialise (and mark
// bare) the repository running the tests.
func gitInit(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", "-b", branch, dir)
	cmd.Env = gitops.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

// worktreeRepo makes a git repository named name in a fresh hermetic temp
// dir (see hermeticGit), with one commit on main, and returns its path with
// symlinks resolved (macOS temp dirs sit behind /var -> /private/var, and git
// reports resolved paths). It also gives git an identity for commits.
func worktreeRepo(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(hermeticGit(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	repo := filepath.Join(dir, name)
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitInit(t, repo, "main")
	worktreeWrite(t, filepath.Join(repo, "README"), "hello\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	return repo
}

// worktreeWrite writes content to path (mode 0600), creating its directory.
func worktreeWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// worktreeGit runs git in dir with Env and returns its trimmed stdout.
func worktreeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitops.Env()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// worktreeExists reports whether path exists.
func worktreeExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
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

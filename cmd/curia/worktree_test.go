package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

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

// worktreeGit runs git in dir with gitEnv and returns its trimmed stdout.
func worktreeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
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

// worktreePathFor is where a test puts worktree name of repo.
func worktreePathFor(repo, name string) string {
	return filepath.Join(filepath.Dir(repo), ".curia-worktrees", name)
}

func TestWorktreeAddNewAndExistingBranch(t *testing.T) {
	ctx := context.Background()
	repo := worktreeRepo(t, "api")
	mainHead := worktreeGit(t, repo, "rev-parse", "main")

	path := worktreePathFor(repo, "api-T-1")
	if err := worktreeAdd(ctx, repo, path, "curia/T-1-x", "main"); err != nil {
		t.Fatalf("worktreeAdd (new branch): %v", err)
	}
	if got := worktreeGit(t, path, "symbolic-ref", "HEAD"); got != "refs/heads/curia/T-1-x" {
		t.Errorf("new worktree HEAD = %q, want refs/heads/curia/T-1-x", got)
	}
	if got := worktreeGit(t, path, "rev-parse", "HEAD"); got != mainHead {
		t.Errorf("new branch starts at %s, want main's %s", got, mainHead)
	}

	gitRun(t, repo, "branch", "curia/T-2-y", "main")
	path2 := worktreePathFor(repo, "api-T-2")
	// The base is not used for a branch that exists.
	if err := worktreeAdd(ctx, repo, path2, "curia/T-2-y", "no-such-base"); err != nil {
		t.Fatalf("worktreeAdd (existing branch): %v", err)
	}
	if got := worktreeGit(t, path2, "symbolic-ref", "HEAD"); got != "refs/heads/curia/T-2-y" {
		t.Errorf("existing-branch worktree HEAD = %q", got)
	}

	path3 := worktreePathFor(repo, "api-T-3")
	err := worktreeAdd(ctx, repo, path3, "curia/T-3-z", "no-such-base")
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Errorf("worktreeAdd with a base that does not resolve = %v, want a 'does not resolve' error", err)
	}
	for _, bad := range [][2]string{{"-x", "main"}, {"curia/T-3-z", "-main"}, {"a..b", "main"}} {
		if err := worktreeAdd(ctx, repo, path3, bad[0], bad[1]); err == nil {
			t.Errorf("worktreeAdd(branch %q, base %q) succeeded", bad[0], bad[1])
		}
	}
	if err := worktreeAdd(ctx, repo, "relative/path", "curia/T-3-z", "main"); err == nil {
		t.Error("worktreeAdd with a relative path succeeded")
	}
	if worktreeExists(path3) {
		t.Errorf("%s exists after failed adds", path3)
	}
	if ok, err := refExists(ctx, repo, "refs/heads/curia/T-3-z"); err != nil || ok {
		t.Errorf("branch of the failed add: exists %v, err %v", ok, err)
	}
}

func TestWorktreeIsOurs(t *testing.T) {
	ctx := context.Background()
	repo := worktreeRepo(t, "api")
	path := worktreePathFor(repo, "api-T-1")
	if err := worktreeAdd(ctx, repo, path, "curia/T-1-x", "main"); err != nil {
		t.Fatal(err)
	}
	other := worktreeRepo(t, "other")
	otherPath := worktreePathFor(other, "other-T-1")
	if err := worktreeAdd(ctx, other, otherPath, "curia/T-1-x", "main"); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(filepath.Dir(repo), "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, repo, path, branch string
		want                     bool
	}{
		{"right path and branch", repo, path, "curia/T-1-x", true},
		{"another branch", repo, path, "curia/T-1-y", false},
		{"a plain directory", repo, plain, "curia/T-1-x", false},
		{"a worktree of another repo", repo, otherPath, "curia/T-1-x", false},
		{"the main worktree", repo, repo, "main", false},
		{"a missing path", repo, worktreePathFor(repo, "api-T-9"), "curia/T-1-x", false},
	}
	for _, c := range cases {
		got, err := worktreeIsOurs(ctx, c.repo, c.path, c.branch)
		if err != nil || got != c.want {
			t.Errorf("%s: worktreeIsOurs = %v, %v; want %v, nil", c.name, got, err, c.want)
		}
	}
	// A symlink to the worktree is not the worktree.
	link := filepath.Join(filepath.Dir(repo), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if got, err := worktreeIsOurs(ctx, repo, link, "curia/T-1-x"); err != nil || got {
		t.Errorf("a symlink to the worktree: worktreeIsOurs = %v, %v; want false", got, err)
	}
}

func TestWorktreeRemoveRefusesDirtyWithoutForce(t *testing.T) {
	ctx := context.Background()
	repo := worktreeRepo(t, "api")
	path := worktreePathFor(repo, "api-T-1")
	if err := worktreeAdd(ctx, repo, path, "curia/T-1-x", "main"); err != nil {
		t.Fatal(err)
	}
	if dirty, err := worktreeDirty(ctx, path); err != nil || dirty {
		t.Fatalf("fresh worktree: dirty %v, err %v", dirty, err)
	}
	worktreeWrite(t, filepath.Join(path, "new.txt"), "x\n")
	if dirty, err := worktreeDirty(ctx, path); err != nil || !dirty {
		t.Fatalf("worktree with an untracked file: dirty %v, err %v", dirty, err)
	}
	if err := worktreeRemove(ctx, repo, path, false); err == nil {
		t.Fatal("worktreeRemove of a dirty worktree without force succeeded")
	}
	if !worktreeExists(filepath.Join(path, "new.txt")) {
		t.Fatal("the refused remove deleted files")
	}
	if err := worktreeRemove(ctx, repo, path, true); err != nil {
		t.Fatalf("worktreeRemove with force: %v", err)
	}
	if worktreeExists(path) {
		t.Errorf("%s still exists after a forced remove", path)
	}
	if _, err := worktreeDirty(ctx, path); err == nil {
		t.Error("worktreeDirty of a removed worktree succeeded")
	}
}

func TestWorktreeCommitAllTakesUntracked(t *testing.T) {
	ctx := context.Background()
	repo := worktreeRepo(t, "api")
	path := worktreePathFor(repo, "api-T-1")
	if err := worktreeAdd(ctx, repo, path, "curia/T-1-x", "main"); err != nil {
		t.Fatal(err)
	}
	worktreeWrite(t, filepath.Join(path, ".gitignore"), "*.log\n")
	worktreeWrite(t, filepath.Join(path, "a.txt"), "a\n")
	worktreeWrite(t, filepath.Join(path, "sub", "b.txt"), "b\n")
	worktreeWrite(t, filepath.Join(path, "debug.log"), "noise\n")
	if err := worktreeCommitAll(ctx, path, "wip: T-1", false); err != nil {
		t.Fatalf("worktreeCommitAll: %v", err)
	}
	files := strings.Fields(worktreeGit(t, path, "ls-tree", "-r", "--name-only", "HEAD"))
	for _, want := range []string{".gitignore", "README", "a.txt", "sub/b.txt"} {
		if !slices.Contains(files, want) {
			t.Errorf("HEAD lacks %s: %q", want, files)
		}
	}
	if slices.Contains(files, "debug.log") {
		t.Errorf("HEAD holds the ignored debug.log: %q", files)
	}
	if got := worktreeGit(t, path, "log", "-1", "--format=%s"); got != "wip: T-1" {
		t.Errorf("commit subject = %q", got)
	}
	if dirty, err := worktreeDirty(ctx, path); err != nil || dirty {
		t.Errorf("after commit: dirty %v, err %v", dirty, err)
	}
	if got := worktreeGit(t, repo, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("main repository HEAD moved to %q", got)
	}

	worktreeCheckCommitHooks(t, repo, path)
}

// worktreeCheckCommitHooks checks that a failing pre-commit hook fails
// worktreeCommitAll and that noVerify skips it.
func worktreeCheckCommitHooks(t *testing.T, repo, path string) {
	t.Helper()
	ctx := context.Background()
	// A failing pre-commit hook fails the commit; noVerify skips it.
	falseBin, err := exec.LookPath("false")
	if err != nil {
		t.Skip("no false on PATH")
	}
	hooks := filepath.Join(filepath.Dir(repo), "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(falseBin, filepath.Join(hooks, "pre-commit")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "config", "core.hooksPath", hooks)
	worktreeWrite(t, filepath.Join(path, "c.txt"), "c\n")
	if err := worktreeCommitAll(ctx, path, "wip 2", false); err == nil {
		t.Fatal("worktreeCommitAll succeeded through a failing pre-commit hook")
	}
	if err := worktreeCommitAll(ctx, path, "wip 2", true); err != nil {
		t.Fatalf("worktreeCommitAll with noVerify: %v", err)
	}
	if got := worktreeGit(t, path, "log", "-1", "--format=%s"); got != "wip 2" {
		t.Errorf("commit subject = %q, want wip 2", got)
	}
}

func TestBranchMergedAndDelete(t *testing.T) {
	ctx := context.Background()
	repo := worktreeRepo(t, "api")
	gitRun(t, repo, "branch", "feat/a", "main")
	if merged, err := branchMerged(ctx, repo, "feat/a", "main"); err != nil || !merged {
		t.Fatalf("feat/a: merged %v, err %v; want true", merged, err)
	}
	if err := branchDelete(ctx, repo, "feat/a", false); err != nil {
		t.Fatalf("branchDelete(feat/a): %v", err)
	}
	if ok, err := refExists(ctx, repo, "refs/heads/feat/a"); err != nil || ok {
		t.Errorf("feat/a after delete: exists %v, err %v", ok, err)
	}

	gitRun(t, repo, "checkout", "-q", "-b", "feat/b")
	worktreeWrite(t, filepath.Join(repo, "b.txt"), "b\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "b")
	gitRun(t, repo, "checkout", "-q", "main")
	if merged, err := branchMerged(ctx, repo, "feat/b", "main"); err != nil || merged {
		t.Fatalf("feat/b: merged %v, err %v; want false", merged, err)
	}
	if err := branchDelete(ctx, repo, "feat/b", false); err == nil {
		t.Fatal("branchDelete of an unmerged branch without force succeeded")
	}
	if ok, err := refExists(ctx, repo, "refs/heads/feat/b"); err != nil || !ok {
		t.Fatalf("feat/b after refused delete: exists %v, err %v", ok, err)
	}
	if err := branchDelete(ctx, repo, "feat/b", true); err != nil {
		t.Fatalf("branchDelete with force: %v", err)
	}
	if ok, err := refExists(ctx, repo, "refs/heads/feat/b"); err != nil || ok {
		t.Errorf("feat/b after forced delete: exists %v, err %v", ok, err)
	}
	worktreeCheckBadNames(t, repo)
}

// worktreeCheckBadNames checks that a missing branch or a name that is no
// valid branch or ref is an error.
func worktreeCheckBadNames(t *testing.T, repo string) {
	t.Helper()
	ctx := context.Background()
	if _, err := branchMerged(ctx, repo, "no-such", "main"); err == nil {
		t.Error("branchMerged of a missing branch succeeded")
	}
	for _, bad := range []string{"-D", "a..b", ""} {
		if err := branchDelete(ctx, repo, bad, true); err == nil {
			t.Errorf("branchDelete(%q) succeeded", bad)
		}
	}
	if _, err := refExists(ctx, repo, "--all"); err == nil {
		t.Error("refExists of an option succeeded")
	}
}

func TestGitErrorsAreCleaned(t *testing.T) {
	ctx := context.Background()
	repo := worktreeRepo(t, "api")
	hostile := filepath.Join(filepath.Dir(repo), "x\x1b[31mred\x07\nline")
	err := worktreeRemove(ctx, repo, hostile, false)
	if err == nil {
		t.Fatal("worktreeRemove of a path that is no worktree succeeded")
	}
	msg := err.Error()
	if strings.ContainsFunc(msg, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		t.Errorf("error holds a control rune: %q", msg)
	}
	if !strings.Contains(msg, "red") {
		t.Errorf("error lacks git's message: %q", msg)
	}
}

func TestWorktreeFunctionsIgnoreGitEnv(t *testing.T) {
	ctx := context.Background()
	repo := worktreeRepo(t, "api")
	other := worktreeRepo(t, "other")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	path := worktreePathFor(repo, "api-T-1")
	if err := worktreeAdd(ctx, repo, path, "curia/T-1-x", "main"); err != nil {
		t.Fatal(err)
	}
	if ok, err := worktreeIsOurs(ctx, repo, path, "curia/T-1-x"); err != nil || !ok {
		t.Errorf("worktreeIsOurs = %v, %v; want the worktree to belong to repo", ok, err)
	}
	if ok, err := refExists(ctx, other, "refs/heads/curia/T-1-x"); err != nil || ok {
		t.Errorf("the branch went to the GIT_DIR repository: exists %v, err %v", ok, err)
	}
}

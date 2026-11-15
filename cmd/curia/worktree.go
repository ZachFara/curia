package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

// Limits of the git calls in this file. A worktree add checks out a whole
// tree (plan section 4.6: 60 s); a commit runs the repository's hooks,
// which may run its checks.
const (
	worktreeAddLimit    = 60 * time.Second
	worktreeCommitLimit = 10 * time.Minute
	worktreeGitLimit    = 30 * time.Second
)

// worktreeGitErrMax bounds git's cleaned stderr in an error.
const worktreeGitErrMax = 400

// worktreeRun runs git -C dir args through runGit within limit and returns
// its stdout, or an error holding git's stderr cleaned to one line.
func worktreeRun(ctx context.Context, limit time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	res := runGit(ctx, dir, args...)
	if res.err == nil {
		return res.out, nil
	}
	return res.out, worktreeGitError(res, limit)
}

// worktreeGitError describes a failed git call: the subcommand, the exit
// status and git's stderr, cleaned, since it may quote untrusted names.
func worktreeGitError(res gitResult, limit time.Duration) error {
	what := res.err.Error()
	if res.timedOut {
		what = "timed out after " + limit.String()
	}
	msg := sanitize.Line(res.stderr, worktreeGitErrMax)
	if msg == "" {
		return fmt.Errorf("git %s: %s", res.args[0], what)
	}
	return fmt.Errorf("git %s: %s: %s", res.args[0], what, msg)
}

// worktreeExitCode is git's exit code, or -1 when git did not run to an
// exit (not found, killed, timed out).
func worktreeExitCode(res gitResult) int {
	var exit *exec.ExitError
	if res.timedOut || !errors.As(res.err, &exit) {
		return -1
	}
	return exit.ExitCode()
}

// worktreeCheckPath refuses a path that is not absolute and clean, so it can
// never be read as an option or wander through "..".
func worktreeCheckPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("refusing path %q: not absolute and clean", sanitize.Line(path, 200))
	}
	return nil
}

// worktreeCheckBranch refuses a branch or base name task.ValidBranch does
// not accept, before git sees it.
func worktreeCheckBranch(what, name string) error {
	if !task.ValidBranch(name) {
		return fmt.Errorf("refusing %s %q: not a valid branch name", what, sanitize.Line(name, 200))
	}
	return nil
}

// refExists reports whether ref resolves in repo (git rev-parse --verify).
// A ref that starts with "-" is refused.
func refExists(ctx context.Context, repo, ref string) (bool, error) {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return false, fmt.Errorf("refusing ref %q", sanitize.Line(ref, 200))
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", ref)
	switch {
	case res.err == nil:
		return true, nil
	case worktreeExitCode(res) == 1 && res.stderr == "":
		return false, nil
	}
	return false, worktreeGitError(res, worktreeGitLimit)
}

// worktreeAdd adds a worktree of repo at path on branch: a new branch from
// base (which must resolve to a commit) when branch does not exist, else the
// existing branch, whose base is then unused. Every name is checked before
// git sees it and passed after the fixed options.
func worktreeAdd(ctx context.Context, repo, path, branch, base string) error {
	if err := errors.Join(worktreeCheckPath(repo), worktreeCheckPath(path),
		worktreeCheckBranch("branch", branch), worktreeCheckBranch("base", base)); err != nil {
		return err
	}
	exists, err := refExists(ctx, repo, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if exists {
		_, err = worktreeRun(ctx, worktreeAddLimit, repo, "worktree", "add", "--quiet", path, branch)
		return err
	}
	ok, err := refExists(ctx, repo, base+"^{commit}")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("base %q does not resolve to a commit in %s", base, sanitize.Line(repo, 200))
	}
	_, err = worktreeRun(ctx, worktreeAddLimit, repo, "worktree", "add", "--quiet", "-b", branch, path, base)
	return err
}

// worktreeCheckRepo checks that repo is the top level of a main (not
// linked) worktree, so git -C repo cannot act on a repository above it.
func worktreeCheckRepo(ctx context.Context, repo string) error {
	if err := worktreeCheckPath(repo); err != nil {
		return err
	}
	out, err := worktreeRun(ctx, worktreeGitLimit, repo,
		"rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return err
	}
	top, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return err
	}
	if out != top+"\n"+filepath.Join(top, ".git") {
		return fmt.Errorf("%s is not the top level of a main git worktree", sanitize.Line(repo, 200))
	}
	return nil
}

// worktreeIsOurs reports whether path is a worktree of repo with branch
// checked out: a directory (not a symlink) that is the top level of a linked
// worktree whose common git dir is repo's. A missing path, or one in no
// repository, is not ours and no error.
func worktreeIsOurs(ctx context.Context, repo, path, branch string) (bool, error) {
	if err := errors.Join(worktreeCheckPath(repo), worktreeCheckPath(path), worktreeCheckBranch("branch", branch)); err != nil {
		return false, err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !fi.IsDir() {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, path, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir",
		"--symbolic-full-name", "HEAD")
	if res.notARepository() {
		return false, nil
	}
	if res.err != nil {
		return false, worktreeGitError(res, worktreeGitLimit)
	}
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return false, err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	want := realPath + "\n" + filepath.Join(realRepo, ".git") + "\nrefs/heads/" + branch
	return realPath != realRepo && res.out == want, nil
}

// worktreeRemove removes the worktree at path from repo; git refuses a
// worktree with modified or untracked files unless force is set.
func worktreeRemove(ctx context.Context, repo, path string, force bool) error {
	if err := errors.Join(worktreeCheckPath(repo), worktreeCheckPath(path)); err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	_, err := worktreeRun(ctx, worktreeGitLimit, repo, append(args, path)...)
	return err
}

// worktreeDirty reports whether the worktree at path has modified, staged
// or untracked files (ignored files do not count).
func worktreeDirty(ctx context.Context, path string) (bool, error) {
	if err := worktreeCheckPath(path); err != nil {
		return false, err
	}
	out, err := worktreeRun(ctx, worktreeGitLimit, path, "status", "--porcelain", "--untracked-files=all")
	return out != "", err
}

// worktreeCommitAll stages everything in the worktree at path, untracked
// files included and ignored ones not, and commits it with message;
// noVerify skips the repository's commit hooks.
func worktreeCommitAll(ctx context.Context, path, message string, noVerify bool) error {
	if err := worktreeCheckPath(path); err != nil {
		return err
	}
	if _, err := worktreeRun(ctx, worktreeGitLimit, path, "add", "--all"); err != nil {
		return err
	}
	args := []string{"commit", "--quiet"}
	if noVerify {
		args = append(args, "--no-verify")
	}
	_, err := worktreeRun(ctx, worktreeCommitLimit, path, append(args, "-m", message)...)
	return err
}

// branchMerged reports whether branch is merged into base: its tip is an
// ancestor of base's.
func branchMerged(ctx context.Context, repo, branch, base string) (bool, error) {
	if err := errors.Join(worktreeCheckPath(repo), worktreeCheckBranch("branch", branch),
		worktreeCheckBranch("base", base)); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/"+branch, base)
	switch {
	case res.err == nil:
		return true, nil
	case worktreeExitCode(res) == 1 && res.stderr == "":
		return false, nil
	}
	return false, worktreeGitError(res, worktreeGitLimit)
}

// branchDelete deletes branch from repo: git branch -d, which refuses an
// unmerged branch, or -D when force is set.
func branchDelete(ctx context.Context, repo, branch string, force bool) error {
	if err := errors.Join(worktreeCheckPath(repo), worktreeCheckBranch("branch", branch)); err != nil {
		return err
	}
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := worktreeRun(ctx, worktreeGitLimit, repo, "branch", flag, branch)
	return err
}

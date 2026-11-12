package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// gitWaitDelay bounds how long a git call waits for its output pipes once
// git has exited or been killed, so a grandchild still holding them cannot
// keep the hook past its timeout.
const gitWaitDelay = 100 * time.Millisecond

// maxHeadBytes bounds the read of a git dir's HEAD file, which is untrusted.
// A symbolic ref is far shorter.
const maxHeadBytes = 4096

// gitInfo returns the repository top level and current branch for cwd, or
// empty strings when cwd is empty or not in a repository. It runs one git
// process, bounded by subprocessTimeout, for the top level and the git dir,
// then reads the branch from <git dir>/HEAD: "ref: refs/heads/<branch>", and
// anything else (a detached HEAD) is no branch. This is right in a linked
// worktree, whose git dir has its own HEAD, and before the first commit. Any
// failure other than "not a git repository" (git missing, a timeout, an
// unreadable HEAD) is logged on logger as one warning.
func gitInfo(ctx context.Context, cwd string, logger *slog.Logger) (repo, branch string) {
	if cwd == "" { // git -C "" would use this process's directory
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	res := runGit(ctx, cwd, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if res.err != nil {
		if !res.notARepository() {
			res.log(logger, cwd)
		}
		return "", ""
	}
	repo, gitDir, ok := strings.Cut(res.out, "\n")
	if !ok || strings.Contains(gitDir, "\n") {
		logger.Warn("git failed", "cwd", cwd, "args", strings.Join(res.args, " "), "err", "unexpected output", "stdout", res.out)
		return "", ""
	}
	branch, err := headBranch(gitDir)
	if err != nil {
		logger.Warn("git HEAD unreadable", "cwd", cwd, "err", err.Error())
	}
	return repo, branch
}

// headBranch returns the branch the HEAD file in gitDir names, or "" when
// HEAD is detached. HEAD is untrusted: anything but a regular file (a FIFO
// would block the open or the read forever) is an error, checked before the
// open and again on the open handle, and the open does not block.
func headBranch(gitDir string) (string, error) {
	path := filepath.Clean(filepath.Join(gitDir, "HEAD"))
	if err := requireRegular(os.Stat(path)); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if err = requireRegular(f.Stat()); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxHeadBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxHeadBytes {
		return "", fmt.Errorf("%s exceeds %d bytes", path, maxHeadBytes)
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/")
	if !ok {
		return "", nil
	}
	return ref, nil
}

// requireRegular passes on a stat error, or reports one when fi is not a
// regular file.
func requireRegular(fi os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (%v)", fi.Name(), fi.Mode().Type())
	}
	return nil
}

// gitResult is the outcome of one git call.
type gitResult struct {
	args        []string
	out, stderr string
	err         error
	timedOut    bool
}

// notARepository reports whether git ran and said cwd is not in a
// repository, which is normal and not worth logging.
func (r gitResult) notARepository() bool {
	var exit *exec.ExitError
	return !r.timedOut && errors.As(r.err, &exit) && strings.Contains(r.stderr, "not a git repository")
}

func (r gitResult) log(logger *slog.Logger, cwd string) {
	msg := r.err.Error()
	if r.timedOut {
		msg = "timed out after " + subprocessTimeout.String() + ": " + msg
	}
	logger.Warn("git failed", "cwd", cwd, "args", strings.Join(r.args, " "), "err", msg, "stderr", r.stderr)
}

// runGit runs git -C cwd args and waits for it. Its stdout and stderr are
// captured, never passed through.
func runGit(ctx context.Context, cwd string, args ...string) gitResult {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = gitEnv()
	cmd.WaitDelay = gitWaitDelay
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	res := gitResult{args: args}
	res.err = cmd.Run()
	res.timedOut = ctx.Err() != nil
	res.out, res.stderr = strings.TrimSpace(out.String()), strings.TrimSpace(stderr.String())
	return res
}

// gitRepoVars are the variables `git rev-parse --local-env-vars` lists: set
// in this process (for example because Claude Code was started from a git
// hook) they would point git at another repository than cwd's.
var gitRepoVars = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
	"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
}

// gitEnv is this process's environment without gitRepoVars, so git finds
// the repository from -C alone, and with LC_ALL=C, so its messages are the
// English ones notARepository matches.
func gitEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitRepoVars, name)
	})
	return append(env, "LC_ALL=C")
}

package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// gitWaitDelay bounds how long a git call waits for its output pipes once
// git has exited or been killed, so a grandchild still holding them cannot
// keep the hook past its timeout.
const gitWaitDelay = 100 * time.Millisecond

// gitInfo returns the repository top level and current branch for cwd, or
// empty strings when cwd is empty or not in a repository. The branch is
// empty on a detached HEAD. The two git commands run at the same time,
// bounded by subprocessTimeout. Any failure other than "not a git
// repository" (git missing, a timeout, a git too old for --show-current) is
// logged on logger as one warning.
func gitInfo(ctx context.Context, cwd string, logger *slog.Logger) (repo, branch string) {
	if cwd == "" { // git -C "" would use this process's directory
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	waitRepo := startGit(ctx, cwd, "rev-parse", "--show-toplevel")
	waitBranch := startGit(ctx, cwd, "branch", "--show-current")
	repoRes, branchRes := waitRepo(), waitBranch()
	if repoRes.err != nil {
		// The branch call fails for the same reason; one line is enough.
		if !repoRes.notARepository() {
			repoRes.log(logger, cwd)
		}
		return "", ""
	}
	if branchRes.err != nil {
		branchRes.log(logger, cwd)
		return repoRes.out, ""
	}
	return repoRes.out, branchRes.out
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

// startGit starts git -C cwd args; the returned func waits for it. Its
// stdout and stderr are captured, never passed through.
func startGit(ctx context.Context, cwd string, args ...string) func() gitResult {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = gitEnv()
	cmd.WaitDelay = gitWaitDelay
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	res := gitResult{args: args}
	if res.err = cmd.Start(); res.err != nil {
		return func() gitResult { return res }
	}
	return func() gitResult {
		res.err = cmd.Wait()
		res.timedOut = ctx.Err() != nil
		res.out, res.stderr = strings.TrimSpace(out.String()), strings.TrimSpace(stderr.String())
		return res
	}
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

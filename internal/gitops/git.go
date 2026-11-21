// Package gitops runs git for Curia: the repository and worktree calls of
// task spawn, pause and clean, and the merge and diff of a review approval.
// It runs git and nothing else; printing and commands stay in cmd/curia.
package gitops

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

// Info returns, for cwd, the main repository's top level, the current
// branch and, in a linked worktree, that worktree's top level (else empty),
// or empty strings when cwd is empty or not in a repository. It runs one git
// process, bounded by timeout, for the top level, the git dir and
// the common dir (plan section 4.5), then reads the branch from
// <git dir>/HEAD: "ref: refs/heads/<branch>", and anything else (a detached
// HEAD) is no branch. This is right in a linked worktree, whose git dir has
// its own HEAD, and before the first commit. Output other than three
// absolute paths, and any failure other than "not a git repository" (git
// missing, a timeout, an unreadable HEAD), is logged on logger as one
// warning.
func Info(ctx context.Context, cwd string, logger *slog.Logger, timeout time.Duration) (repo, branch, worktree string) {
	if cwd == "" { // git -C "" would use this process's directory
		return "", "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := Run(ctx, cwd, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if res.err != nil {
		if !res.notARepository() {
			res.log(logger, cwd, timeout)
		}
		return "", "", ""
	}
	lines := strings.Split(res.out, "\n")
	if len(lines) != 3 || slices.ContainsFunc(lines, func(l string) bool { return !filepath.IsAbs(l) }) {
		logger.Warn("git failed", "cwd", cwd, "args", strings.Join(res.args, " "), "err", "unexpected output", "stdout", res.out)
		return "", "", ""
	}
	top, gitDir, common := lines[0], lines[1], lines[2]
	branch, err := headBranch(gitDir)
	if err != nil {
		logger.Warn("git HEAD unreadable", "cwd", cwd, "err", err.Error())
	}
	repo, worktree = mainRepo(top, common)
	return repo, branch, worktree
}

// mainRepo returns the main repository and the linked worktree (or "") for
// a top level and common dir from git: a common dir named .git whose parent
// is not the top level belongs to a linked worktree of that parent.
// Anything else (a main worktree, a submodule's common dir) is its own top
// level.
func mainRepo(top, common string) (repo, worktree string) {
	if parent := filepath.Dir(common); filepath.Base(common) == ".git" && parent != top {
		return parent, top
	}
	return top, ""
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

// Result is the outcome of one git call. out and stderr are trimmed;
// raw is stdout as git wrote it (Run only), for output whose leading
// spaces mean something, such as a diff stat's column alignment.
type Result struct {
	args        []string
	out, stderr string
	raw         string
	err         error
	timedOut    bool
}

// Err is the error of the call, nil when git exited 0.
func (r Result) Err() error { return r.err }

// Stderr is git's trimmed stderr.
func (r Result) Stderr() string { return r.stderr }

// notARepository reports whether git ran and said cwd is not in a
// repository, which is normal and not worth logging.
func (r Result) notARepository() bool {
	var exit *exec.ExitError
	return !r.timedOut && errors.As(r.err, &exit) && strings.Contains(r.stderr, "not a git repository")
}

func (r Result) log(logger *slog.Logger, cwd string, timeout time.Duration) {
	msg := r.err.Error()
	if r.timedOut {
		msg = "timed out after " + timeout.String() + ": " + msg
	}
	logger.Warn("git failed", "cwd", cwd, "args", strings.Join(r.args, " "), "err", msg, "stderr", r.stderr)
}

// Run runs git -C cwd args and waits for it. Its stdout and stderr are
// captured, never passed through.
func Run(ctx context.Context, cwd string, args ...string) Result {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = Env()
	cmd.WaitDelay = gitWaitDelay
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	res := Result{args: args}
	res.err = cmd.Run()
	res.timedOut = ctx.Err() != nil
	res.raw = out.String()
	res.out, res.stderr = strings.TrimSpace(res.raw), strings.TrimSpace(stderr.String())
	return res
}

// groupGrace is how long a child run by RunProcessGroup has to stop after SIGTERM
// before its process group is killed. Tests shorten it.
var groupGrace = 5 * time.Second

// groupMaxOut bounds what RunProcessGroup keeps of a child's stdout and of its
// stderr, each.
const groupMaxOut = 64 << 10

// RunProcessGroup runs the program at path with args in dir and env, for the git
// and gh calls of an approval, which change the user's repository or a
// remote and must not be cut short by accident. The child runs in a process
// group of its own, so a hang-up or a Ctrl-C meant for Curia's group never
// reaches it, with stdin from the null device and stdout and stderr in
// unlinked temporary files rather than pipes, so should Curia die the child
// finishes instead of dying on its next write. When ctx ends, the group gets
// SIGTERM, and SIGKILL after groupGrace; once a stopped child has exited,
// whatever is left of its group (a hook that ignored SIGTERM) is killed too.
// stdout and stderr are kept up to groupMaxOut bytes each.
func RunProcessGroup(ctx context.Context, dir string, env []string, path string, args ...string) (stdout, stderr string, err error) {
	outF, err := groupTempFile()
	if err != nil {
		return "", "", err
	}
	defer func() { _ = outF.Close() }()
	errF, err := groupTempFile()
	if err != nil {
		return "", "", err
	}
	defer func() { _ = errF.Close() }()
	cmd := exec.Command(path, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = outF, errF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return "", "", err
	}
	err = groupWait(ctx, cmd)
	return groupRead(outF), groupRead(errF), err
}

// groupWait waits for cmd, which leads its own process group, and stops the
// group when ctx ends first: SIGTERM, then SIGKILL after groupGrace.
func groupWait(ctx context.Context, cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(groupGrace)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		err = <-done
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // what is left of the group; ESRCH when nothing is
	return err
}

// groupTempFile is a new temporary file, already unlinked, so that nothing
// is left behind whatever happens to Curia.
func groupTempFile() (*os.File, error) {
	f, err := os.CreateTemp("", "curia-out-")
	if err != nil {
		return nil, err
	}
	if err = os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// groupRead is what the child wrote to f, at most groupMaxOut bytes,
// trimmed.
func groupRead(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, groupMaxOut))
	return strings.TrimSpace(string(b))
}

// RunGroup is Run through RunProcessGroup: git -C cwd args, in a process
// group of its own, stopped with SIGTERM and then killed when ctx ends.
func RunGroup(ctx context.Context, cwd string, args ...string) Result {
	return runGitGroupEnv(ctx, cwd, nil, args...)
}

// runGitGroupEnv is RunGroup with extra added to git's environment.
func runGitGroupEnv(ctx context.Context, cwd string, extra []string, args ...string) Result {
	res := Result{args: args}
	path, err := exec.LookPath("git")
	if err != nil {
		res.err = err
		return res
	}
	res.out, res.stderr, res.err = RunProcessGroup(ctx, "", append(Env(), extra...), path, append([]string{"-C", cwd}, args...)...)
	res.timedOut = ctx.Err() != nil
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

// Env is this process's environment without gitRepoVars, so git finds
// the repository from -C alone, and with LC_ALL=C, so its messages are the
// English ones notARepository matches.
func Env() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitRepoVars, name)
	})
	return append(env, "LC_ALL=C")
}

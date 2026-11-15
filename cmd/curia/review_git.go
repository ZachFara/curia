package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// Limits of the git and gh calls of an approval (plan section 4.6). Every
// other git call of this file has worktreeGitLimit.
const (
	reviewMergeLimit = 2 * time.Minute
	reviewPushLimit  = 2 * time.Minute
	reviewGHLimit    = time.Minute
)

// Bounds of what git and gh print (plan section 4.13).
const (
	reviewStatMaxBytes = 64 << 10 // the diff stat
	reviewStatMaxLines = 200
	reviewGitErrMax    = 300      // git's stderr in an error, in runes
	reviewGHMaxOut     = 64 << 10 // gh's stdout and stderr, each
	reviewStatWidth    = 100      // the width of the stat shown and recorded
)

var (
	// reviewHeadPattern is a full commit id: SHA-1 or SHA-256.
	reviewHeadPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	// reviewRemotePattern is the only form of remote name pushed to.
	reviewRemotePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// reviewURLPattern is the only form of pull-request URL kept from gh.
	reviewURLPattern = regexp.MustCompile(`^https://[A-Za-z0-9._/-]{1,200}$`)
)

// errReviewBranchChanged refuses an approval whose branch is no longer at
// the commit the user was shown (Ruling 118).
var errReviewBranchChanged = errors.New("the branch changed since you looked; showing the new state")

// reviewGH runs gh in dir and returns its stdout (at most 64 KiB). Tests replace it.
var reviewGH = func(ctx context.Context, dir string, args ...string) (stdout []byte, err error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(), "GH_PROMPT_DISABLED=1")
	cmd.WaitDelay = gitWaitDelay
	out, errOut := &reviewCapped{max: reviewGHMaxOut}, &reviewCapped{max: reviewGHMaxOut}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err = cmd.Run(); err != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(errOut.buf.String()), "\n")
		if first = sanitize.Line(first, reviewGitErrMax); first != "" {
			return out.buf.Bytes(), fmt.Errorf("%w: %s", err, first)
		}
		return out.buf.Bytes(), err
	}
	return out.buf.Bytes(), nil
}

// reviewGHLookPath finds gh before anything is pushed. Tests replace it.
var reviewGHLookPath = exec.LookPath

// reviewDiffLookPath finds a diff tool. Tests replace it.
var reviewDiffLookPath = exec.LookPath

// reviewCapped keeps the first max bytes written to it and drops the rest,
// so a chatty child neither grows memory nor blocks on a full pipe.
type reviewCapped struct {
	buf bytes.Buffer
	max int
}

func (c *reviewCapped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// reviewCheckNames checks t's repository path, branch and base before git
// sees them.
func reviewCheckNames(t model.Task) error {
	return errors.Join(worktreeCheckPath(t.Repo), worktreeCheckBranch("branch", t.Branch),
		worktreeCheckBranch("base", t.BaseBranch))
}

// reviewHead is the full commit id t's branch is at now, or "" when the
// branch does not exist.
func reviewHead(ctx context.Context, t model.Task) (string, error) {
	if err := reviewCheckNames(t); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, t.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+t.Branch+"^{commit}")
	switch {
	case res.err == nil && reviewHeadPattern.MatchString(res.out):
		return res.out, nil
	case res.err == nil:
		return "", fmt.Errorf("git rev-parse printed %q, not a commit id", sanitize.Line(res.out, 80))
	case worktreeExitCode(res) == 1 && res.stderr == "":
		return "", nil
	}
	return "", worktreeGitError(res, worktreeGitLimit)
}

// reviewShort is the short form of commit id head.
func reviewShort(head string) string {
	return head[:min(7, len(head))]
}

// reviewDiffStat is section 4.5's diff stat for t, cleaned and capped, and the warning line that goes with it ("" when none).
// It also returns the full commit id of the branch the stat was taken at ("" when there is none), which the
// view shows and passes back on approval.
func reviewDiffStat(ctx context.Context, t model.Task, width int) (stat, note, head string) {
	if t.Branch == "" {
		return "", "the task has no branch (never spawned, or cleaned)", ""
	}
	path, onDisk, err := lifecycleWorktree(ctx, t)
	if err == nil {
		head, err = reviewHead(ctx, t)
	}
	if err != nil {
		return "", sanitize.Line(err.Error(), reviewGitErrMax), ""
	}
	if head == "" {
		return "", "branch " + t.Branch + " does not exist", ""
	}
	if stat, err = reviewStatAt(ctx, t, head, width); err != nil {
		return "", sanitize.Line(err.Error(), reviewGitErrMax), head
	}
	return stat, reviewDiffNote(ctx, t, head, path, onDisk), head
}

// reviewDiffNote is the warning under the stat of t's branch at head: the
// worktree's uncommitted changes, else no commits beyond the base.
func reviewDiffNote(ctx context.Context, t model.Task, head, path string, onDisk bool) string {
	if onDisk {
		dirty, err := worktreeDirty(ctx, path)
		switch {
		case err != nil:
			return sanitize.Line("cannot read the worktree: "+err.Error(), reviewGitErrMax)
		case dirty:
			return "the worktree has uncommitted changes that are not in this diff"
		}
	}
	n, err := reviewAhead(ctx, t, head)
	switch {
	case err != nil:
		return sanitize.Line(err.Error(), reviewGitErrMax)
	case n == 0:
		return "the branch has no commits beyond " + t.BaseBranch
	}
	return ""
}

// reviewStatAt is git diff --stat of t's base...head (three dots: the
// changes since the branch left its base), cleaned by sanitize.Block and
// cut at 200 lines and 64 KiB with a line saying how many more there were.
// No external diff or text conversion runs.
func reviewStatAt(ctx context.Context, t model.Task, head string, width int) (string, error) {
	if !reviewHeadPattern.MatchString(head) {
		return "", fmt.Errorf("refusing commit %q", sanitize.Line(head, 80))
	}
	out, err := worktreeRun(ctx, worktreeGitLimit, t.Repo, "diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"--stat="+strconv.Itoa(width), t.BaseBranch+"..."+head, "--")
	if err != nil {
		return "", err
	}
	s := sanitize.Block(out, 0)
	if s == "" {
		return "", nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines) - reviewStatMaxLines; n > 0 {
		lines = append(lines[:reviewStatMaxLines:reviewStatMaxLines], fmt.Sprintf("… %d more lines", n))
	}
	return sanitize.Block(strings.Join(lines, "\n"), reviewStatMaxBytes), nil
}

// reviewAhead counts the commits of head that t's base does not have.
func reviewAhead(ctx context.Context, t model.Task, head string) (int, error) {
	if !reviewHeadPattern.MatchString(head) {
		return 0, fmt.Errorf("refusing commit %q", sanitize.Line(head, 80))
	}
	out, err := worktreeRun(ctx, worktreeGitLimit, t.Repo, "rev-list", "--count", t.BaseBranch+".."+head, "--")
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count printed %q", sanitize.Line(out, 40))
	}
	return n, nil
}

// reviewCheck runs steps 1 to 5 of section 4.6's approve order and returns the diff stat and the branch's commit.
// The stat is taken at that commit, so it is the record of what was approved.
func reviewCheck(ctx context.Context, st *store.Store, t model.Task) (stat, head string, err error) {
	if t.Branch == "" {
		return "", "", fmt.Errorf("%s has no branch: nothing to approve (never spawned, or cleaned)", t.ID)
	}
	path, onDisk, err := lifecycleWorktree(ctx, t)
	if err != nil {
		return "", "", err
	}
	if head, err = reviewHead(ctx, t); err != nil {
		return "", "", err
	}
	if head == "" {
		return "", "", fmt.Errorf("branch %s does not exist in %s", t.Branch, sanitize.Line(t.Repo, 300))
	}
	ok, err := refExists(ctx, t.Repo, t.BaseBranch+"^{commit}")
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", fmt.Errorf("base %s does not resolve to a commit in %s", t.BaseBranch, sanitize.Line(t.Repo, 300))
	}
	if err = reviewCheckAgents(ctx, st, t); err != nil {
		return "", "", err
	}
	if err = reviewCheckClean(ctx, path, onDisk); err != nil {
		return "", "", err
	}
	n, err := reviewAhead(ctx, t, head)
	if err != nil {
		return "", "", err
	}
	if n == 0 {
		return "", "", fmt.Errorf("nothing to merge: %s has no commits beyond %s", t.Branch, t.BaseBranch)
	}
	if stat, err = reviewStatAt(ctx, t, head, reviewStatWidth); err != nil {
		return "", "", err
	}
	return stat, head, nil
}

// reviewCheckAgents is step 2: no live linked session is working or waiting.
func reviewCheckAgents(ctx context.Context, st *store.Store, t model.Task) error {
	live, err := st.TaskSessions(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, s := range live {
		if s.State == model.StateWorking || s.State == model.StateWaiting {
			return errors.New("the agent is still working; wait for it to stop")
		}
	}
	return nil
}

// reviewCheckClean is step 3: the worktree, when it is on disk, is clean.
func reviewCheckClean(ctx context.Context, path string, onDisk bool) error {
	if !onDisk {
		return nil
	}
	dirty, err := worktreeDirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("uncommitted changes in %s: ask the agent to commit them, or commit them yourself", sanitize.Line(path, 300))
	}
	return nil
}

// reviewMerge is step 6: it merges t's branch into its base in t.Repo, and aborts a merge that did not complete.
// It merges exactly commit head, the one approved, and refuses when the branch has moved since; when the user's
// checkout does not have the base checked out or is in the middle of a merge, rebase, cherry-pick, revert or
// bisect; and leaves git to refuse a local change the merge would overwrite. The repository's hooks run as git
// runs them. After a failure the repository is as it was, or the error says how it is not.
func reviewMerge(ctx context.Context, t model.Task, head string) error {
	if !reviewHeadPattern.MatchString(head) {
		return fmt.Errorf("refusing commit %q", sanitize.Line(head, 80))
	}
	if err := reviewCheckNames(t); err != nil {
		return err
	}
	if err := worktreeCheckRepo(ctx, t.Repo); err != nil {
		return err
	}
	if err := reviewCheckCheckout(ctx, t); err != nil {
		return err
	}
	before, err := reviewRepoState(ctx, t.Repo)
	if err != nil {
		return err
	}
	now, err := reviewHead(ctx, t)
	if err != nil {
		return err
	}
	if now != head {
		return errReviewBranchChanged
	}
	mctx, cancel := context.WithTimeout(ctx, reviewMergeLimit)
	defer cancel()
	res := runGit(mctx, t.Repo, "merge", "--no-ff", "--no-edit", "--no-autostash",
		"-m", "Merge branch '"+t.Branch+"' into "+t.BaseBranch, head)
	if res.err == nil {
		return nil
	}
	return reviewMergeFailed(ctx, t, res, before)
}

// reviewCheckCheckout refuses a merge unless the user's checkout has t's
// base checked out and no operation under way.
func reviewCheckCheckout(ctx context.Context, t model.Task) error {
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, t.Repo, "symbolic-ref", "--short", "-q", "HEAD")
	cur := res.out
	switch {
	case res.err != nil && worktreeExitCode(res) == 1 && res.stderr == "":
		cur = "a detached HEAD"
	case res.err != nil:
		return worktreeGitError(res, worktreeGitLimit)
	}
	if cur != t.BaseBranch {
		return fmt.Errorf("%s has %s checked out, not %s: check out %s there, or approve with --pr",
			sanitize.Line(t.Repo, 300), sanitize.Line(cur, 200), t.BaseBranch, t.BaseBranch)
	}
	op, err := reviewInProgress(ctx, t.Repo)
	if err != nil {
		return err
	}
	if op != "" {
		return fmt.Errorf("%s is in the middle of %s: finish or abort it there, then approve again. Nothing was merged",
			sanitize.Line(t.Repo, 300), op)
	}
	return nil
}

// reviewOperations are the files in a git dir that say an operation is
// under way, and its name.
var reviewOperations = []struct{ file, name string }{
	{"MERGE_HEAD", "a merge"},
	{"rebase-merge", "a rebase"},
	{"rebase-apply", "a rebase or am"},
	{"CHERRY_PICK_HEAD", "a cherry-pick"},
	{"REVERT_HEAD", "a revert"},
	{"BISECT_LOG", "a bisect"},
}

// reviewInProgress names the operation under way in repo, or "".
func reviewInProgress(ctx context.Context, repo string) (string, error) {
	dir, err := worktreeRun(ctx, worktreeGitLimit, repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("git printed %q as the git dir", sanitize.Line(dir, 200))
	}
	for _, op := range reviewOperations {
		_, err := os.Lstat(filepath.Join(dir, op.file))
		switch {
		case err == nil:
			return op.name, nil
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
	}
	return "", nil
}

// reviewState is what a failed merge must leave as it found: HEAD's commit
// and git status.
type reviewState struct{ head, status string }

func reviewRepoState(ctx context.Context, repo string) (reviewState, error) {
	head, err := worktreeRun(ctx, worktreeGitLimit, repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return reviewState{}, err
	}
	status, err := worktreeRun(ctx, worktreeGitLimit, repo, "status", "--porcelain", "--untracked-files=all")
	return reviewState{head: head, status: status}, err
}

// reviewMergeFailed aborts the merge res left under way (MERGE_HEAD exists:
// a conflict, or a hook that refused the merge commit), then checks the
// repository is as before. The error is git's message, cleaned, and what
// state the repository is in.
func reviewMergeFailed(ctx context.Context, t model.Task, res gitResult, before reviewState) error {
	said := res.stderr
	if said == "" {
		said = res.out // a conflict is reported on stdout
	}
	msg := sanitize.Line(said, reviewGitErrMax)
	if res.timedOut {
		msg = "git merge timed out after " + reviewMergeLimit.String() + ": " + msg
	}
	// The caller's context may be what ended the merge; cleaning up must still run.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeGitLimit)
	defer cancel()
	repo := sanitize.Line(t.Repo, 300)
	if ok, _ := refExists(actx, t.Repo, "MERGE_HEAD"); ok {
		if _, err := worktreeRun(actx, worktreeGitLimit, t.Repo, "merge", "--abort"); err != nil {
			return fmt.Errorf("%s; the merge is still under way in %s and could not be aborted (%w): run git -C %s merge --abort there",
				msg, repo, err, repo)
		}
	}
	after, err := reviewRepoState(actx, t.Repo)
	switch {
	case err != nil:
		return fmt.Errorf("%s; afterwards the state of %s could not be read (%w): check git status there", msg, repo, err)
	case after.head != before.head:
		return fmt.Errorf("%s; %s moved from %s to %s: check git -C %s log", msg, t.BaseBranch,
			reviewShort(before.head), sanitize.Line(reviewShort(after.head), 20), repo)
	case after.status != before.status:
		return fmt.Errorf("%s; nothing was merged, but git status in %s differs from before the merge: check it", msg, repo)
	}
	return fmt.Errorf("%s; nothing was merged", msg)
}

// reviewOpenPR is step 7: push, then gh pr create; url is "" when gh printed none.
// It pushes exactly commit head, the one approved, to the task's branch on remote (never forced, never the
// base) and refuses when the branch has moved since, before the push and again before gh runs. The task's
// title and the agent's summary reach gh only as the values of --title and --body (task.PRTitle, task.PRBody).
func reviewOpenPR(ctx context.Context, t model.Task, r model.Review, decided []model.Decision, remote, head string) (url string, err error) {
	if !reviewHeadPattern.MatchString(head) {
		return "", fmt.Errorf("refusing commit %q", sanitize.Line(head, 80))
	}
	if !reviewRemotePattern.MatchString(remote) || strings.HasPrefix(remote, "-") {
		return "", fmt.Errorf("refusing remote %q", sanitize.Line(remote, 80))
	}
	if err = reviewCheckNames(t); err != nil {
		return "", err
	}
	if _, err = reviewGHLookPath("gh"); err != nil {
		return "", errors.New("gh is not on PATH: install the GitHub CLI and log in, or approve with --merge. Nothing was pushed")
	}
	if err = reviewSameHead(ctx, t, head); err != nil {
		return "", err
	}
	if _, err = worktreeRun(ctx, reviewPushLimit, t.Repo, "push", remote, head+":refs/heads/"+t.Branch); err != nil {
		return "", fmt.Errorf("%w; %s was not pushed and no pull request was opened", err, t.Branch)
	}
	pushed := fmt.Sprintf("%s is pushed to %s at %s", t.Branch, remote, reviewShort(head))
	if err = reviewSameHead(ctx, t, head); err != nil {
		return "", fmt.Errorf("%s, but %w", pushed, err)
	}
	gctx, cancel := context.WithTimeout(ctx, reviewGHLimit)
	defer cancel()
	out, err := reviewGH(gctx, t.Repo, "pr", "create", "--base", t.BaseBranch, "--head", t.Branch,
		"--title", task.PRTitle(t), "--body", task.PRBody(t, r, decided))
	if err != nil {
		return "", fmt.Errorf("%s, but gh pr create failed: %s. The review stays pending: approve again to retry, or open the pull request yourself",
			pushed, sanitize.Line(err.Error(), reviewGitErrMax))
	}
	return reviewPRURL(out), nil
}

// reviewSameHead refuses when t's branch is no longer at head.
func reviewSameHead(ctx context.Context, t model.Task, head string) error {
	now, err := reviewHead(ctx, t)
	if err != nil {
		return err
	}
	if now != head {
		return errReviewBranchChanged
	}
	return nil
}

// reviewPRURL is the last line gh printed when it is a plain https URL,
// else "".
func reviewPRURL(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if reviewURLPattern.MatchString(last) {
		return last
	}
	return ""
}

// reviewDiffCmd is the command of section 4.6's diff table for tool.
// It is not bound to a context: it runs as long as the user keeps the tool open.
func reviewDiffCmd(t model.Task, tool string) (*exec.Cmd, error) {
	if err := reviewCheckNames(t); err != nil {
		return nil, err
	}
	rng := t.BaseBranch + "..." + t.Branch
	var bin string
	var args, needs []string
	switch tool {
	case "git":
		bin, args = "git", []string{"-C", t.Repo, "diff", rng}
	case "delta":
		bin, args, needs = "git", []string{"-C", t.Repo, "-c", "core.pager=delta", "diff", rng}, []string{"delta"}
	case "lazygit":
		bin, args = "lazygit", []string{"-p", reviewDiffDir(t)}
	case "nvim":
		bin, args, needs = "git", []string{"-C", t.Repo, "difftool", "--no-prompt", "--tool=nvimdiff", rng}, []string{"nvim"}
	default:
		return nil, fmt.Errorf("unknown diff tool %q: review.diff_tool is git, delta, lazygit or nvim", sanitize.Line(tool, 40))
	}
	for _, n := range needs {
		if _, err := reviewDiffLookPath(n); err != nil {
			return nil, fmt.Errorf("the diff tool %s is not on PATH: install it, or set review.diff_tool", n)
		}
	}
	path, err := reviewDiffLookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("the diff tool %s is not on PATH: install it, or set review.diff_tool", bin)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = gitEnv()
	return cmd, nil
}

// reviewDiffDir is where lazygit opens: the task's worktree when it is on
// disk as a directory, else the main repository.
func reviewDiffDir(t model.Task) string {
	path, err := spawnWorktreePath(t.Repo, t.ID)
	if err != nil {
		return t.Repo
	}
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		return path
	}
	return t.Repo
}

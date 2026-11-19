package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	reviewGitErrMax    = 300 // git's stderr in an error, in runes
	reviewStatWidth    = 100 // the width of the stat shown and recorded
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

// reviewGH runs gh in dir, in a process group of its own (runGroup), and
// returns its stdout (at most 64 KiB). Tests replace it.
var reviewGH = func(ctx context.Context, dir string, args ...string) (stdout []byte, err error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, err
	}
	out, errOut, err := runGroup(ctx, dir, append(gitEnv(), "GH_PROMPT_DISABLED=1"), path, args...)
	if err != nil {
		first, _, _ := strings.Cut(errOut, "\n")
		if first = sanitize.Line(first, reviewGitErrMax); first != "" {
			return []byte(out), fmt.Errorf("%w: %s", err, first)
		}
		return []byte(out), err
	}
	return []byte(out), nil
}

// reviewGHLookPath finds gh before anything is pushed. Tests replace it.
var reviewGHLookPath = exec.LookPath

// reviewDiffLookPath finds a diff tool. Tests replace it.
var reviewDiffLookPath = exec.LookPath

// reviewRunGroup is worktreeRun for the git calls of an approval that
// change something (merge, push, and undoing a merge): git runs in a
// process group of its own and is stopped with SIGTERM, then killed, when
// limit or ctx ends (runGroup).
func reviewRunGroup(ctx context.Context, limit time.Duration, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if res := runGitGroup(ctx, dir, args...); res.err != nil {
		return worktreeGitError(res, limit)
	}
	return nil
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

// reviewResolveHead is t's branch head when given (--head) is exactly its full commit id: lower-case hex of
// the length the repository's object format gives (40 for SHA-1, 64 for SHA-256, asked of git). A prefix is
// refused, however unique: being unique now does not prove it names the commit the user looked at. Any other
// value is refused with the full head and the command to run; a full id that is not the branch's head now is
// "the branch changed since you looked".
func reviewResolveHead(ctx context.Context, t model.Task, given string) (string, error) {
	head, err := reviewHead(ctx, t)
	switch {
	case err != nil:
		return "", err
	case head == "":
		return "", fmt.Errorf("branch %s does not exist in %s", t.Branch, sanitize.Line(t.Repo, 300))
	}
	size, err := reviewObjectIDLen(ctx, t.Repo)
	if err != nil {
		return "", err
	}
	if len(given) != size || strings.Trim(given, "0123456789abcdef") != "" {
		return "", fmt.Errorf("--head %q: give the full commit id you looked at (%d lower-case hex digits; a prefix is not "+
			"enough): %s is at %s", sanitize.Line(given, 80), size, t.Branch, head)
	}
	if given != head {
		return "", fmt.Errorf("the branch changed since you looked: %s is now at %s, not %s. Look at it again with curia review %s",
			t.Branch, head, given, t.ID)
	}
	return head, nil
}

// reviewObjectIDLen is the length, in hex digits, of a commit id in repo: git rev-parse --show-object-format.
func reviewObjectIDLen(ctx context.Context, repo string) (int, error) {
	format, err := worktreeRun(ctx, worktreeGitLimit, repo, "rev-parse", "--show-object-format")
	switch {
	case err != nil:
		return 0, err
	case format == "sha1":
		return 40, nil
	case format == "sha256":
		return 64, nil
	}
	return 0, fmt.Errorf("git rev-parse --show-object-format printed %q", sanitize.Line(format, 40))
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
// changes since the branch left its base), its leading spaces kept, cleaned by sanitize.Block and
// cut at 200 lines and 64 KiB with a line saying how many more there were.
// No external diff or text conversion runs.
func reviewStatAt(ctx context.Context, t model.Task, head string, width int) (string, error) {
	if !reviewHeadPattern.MatchString(head) {
		return "", fmt.Errorf("refusing commit %q", sanitize.Line(head, 80))
	}
	sctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(sctx, t.Repo, "diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"--stat="+strconv.Itoa(width), "refs/heads/"+t.BaseBranch+"..."+head, "--")
	if res.err != nil {
		return "", worktreeGitError(res, worktreeGitLimit)
	}
	// The untrimmed output: git pads every file name, the first one's too,
	// to line the columns up (live check defect 3). Block keeps leading
	// spaces and drops trailing ones and the last line break.
	s := sanitize.Block(res.raw, 0)
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
	out, err := worktreeRun(ctx, worktreeGitLimit, t.Repo, "rev-list", "--count", "refs/heads/"+t.BaseBranch+".."+head, "--")
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
	ok, err := refExists(ctx, t.Repo, "refs/heads/"+t.BaseBranch+"^{commit}")
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
// runs them. It refuses a checkout whose index holds staged changes, so that a merge cut short can be undone
// without touching the user's own work (reviewUndoProven). After a failure the repository is as it was, or
// the error says how it is not.
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
	if before.staged {
		return fmt.Errorf("staged changes in %s: commit or unstage them first. Nothing was merged", sanitize.Line(t.Repo, 300))
	}
	now, err := reviewHead(ctx, t)
	if err != nil {
		return err
	}
	if now != head {
		return errReviewBranchChanged
	}
	// The tree the merge will give, computed before anything changes: a
	// conflict is refused here, and an undo later must find exactly it.
	tree, err := reviewMergeTree(ctx, t, head)
	if err != nil {
		return err
	}
	if err = reviewCheckOnDisk(ctx, t, tree, head); err != nil {
		return err
	}
	mctx, cancel := context.WithTimeout(ctx, reviewMergeLimit)
	defer cancel()
	res := runGitGroupEnv(mctx, t.Repo, reviewMergeEnv(t, tree), "merge", "--no-ff", "--commit", "--no-edit", "--no-autostash",
		"-m", "Merge branch '"+t.Branch+"' into "+t.BaseBranch, head)
	if res.err == nil {
		return reviewMergeDone(ctx, t, head, before)
	}
	return reviewMergeFailed(ctx, t, res, before, tree)
}

// reviewMergeEnv is the configuration the approval's git merge runs with, given in the environment (as git -c
// would, read after every config file; git 2.31 or later) so that the developer's merge configuration cannot
// make it a different merge (C-1): branch.<base>.mergeOptions emptied (--no-commit, --squash, -s, -X and the
// rest are not applied), and, when git merge-tree computed the merge's tree (git 2.38 or later, whose default
// strategy is ort), pull.twohead set to ort, the strategy merge-tree uses. The command line adds --no-ff and
// --commit, which win over merge.ff and any --no-commit.
func reviewMergeEnv(t model.Task, tree string) []string {
	env := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=branch." + t.BaseBranch + ".mergeOptions", "GIT_CONFIG_VALUE_0="}
	if tree != "" {
		env[0] = "GIT_CONFIG_COUNT=2"
		env = append(env, "GIT_CONFIG_KEY_1=pull.twohead", "GIT_CONFIG_VALUE_1=ort")
	}
	return env
}

// reviewMergeDone is the check after git merge exited 0: the base must then contain head, or the merge is not
// recorded (C-1). On a context detached from the caller's; when the base does not contain head, Curia runs
// nothing and the error, wrapping errReviewLeft, says what is there and what the user can run.
func reviewMergeDone(ctx context.Context, t model.Task, head string, before reviewState) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*worktreeGitLimit)
	defer cancel()
	n, err := reviewAhead(actx, t, head)
	if err == nil && n == 0 {
		return nil
	}
	repo := sanitize.Line(t.Repo, 300)
	found := "its commits could not be counted: " + sanitize.Line(fmt.Sprint(err), 200)
	if err == nil {
		after, serr := reviewRepoState(actx, t.Repo)
		found = reviewStateDiff(before, after)
		switch {
		case serr != nil:
			found = "git status could not be read"
		case found == "":
			found = "git status as before"
		}
		if sit, ierr := reviewInspect(actx, t.Repo); ierr == nil && sit.underWay {
			found += "; a merge is under way (MERGE_HEAD)"
		}
	}
	return fmt.Errorf("git merge reported success, but %s does not contain %s (%s): nothing is recorded merged and Curia ran "+
		"nothing there. Look with git -C %s status and git -C %s log; while a merge is under way there, git -C %s commit "+
		"completes it (approving again then records it) and git -C %s merge --abort ends it. The review stays pending: %w",
		t.BaseBranch, reviewShort(head), found, repo, repo, repo, repo, errReviewLeft)
}

// reviewConflictsShown bounds the conflicting paths a refusal names.
const reviewConflictsShown = 5

// reviewMergeTree is the tree id git merge of head into the checkout's HEAD will produce, computed with git
// merge-tree --write-tree (it writes objects only, never the working tree, the index or a ref). A merge that
// would conflict is refused here, before anything changes. A git too old for --write-tree (before 2.38) gives
// "" and no error: the merge then runs, but nothing can prove an interrupted one is only Curia's, so it is never
// undone except after a conflict (reviewMergeFailed).
func reviewMergeTree(ctx context.Context, t model.Task, head string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, t.Repo, "merge-tree", "--write-tree", "--name-only", "--no-messages", "HEAD", head)
	lines := strings.Split(res.out, "\n")
	switch code := worktreeExitCode(res); {
	case res.err == nil && reviewHeadPattern.MatchString(lines[0]):
		return lines[0], nil
	case code == 1 && reviewHeadPattern.MatchString(lines[0]):
		paths := lines[1:]
		if n := len(paths) - reviewConflictsShown; n > 0 {
			paths = append(paths[:reviewConflictsShown], fmt.Sprintf("%d more", n))
		}
		return "", fmt.Errorf("merging %s into %s would conflict in %s: resolve it on the branch first (merge %s into it in its "+
			"worktree). Nothing was merged", t.Branch, t.BaseBranch, sanitize.Line(strings.Join(paths, ", "), reviewGitErrMax),
			t.BaseBranch)
	case code == 129:
		return "", nil // no --write-tree
	}
	return "", worktreeGitError(res, worktreeGitLimit)
}

// reviewCheckOnDisk refuses a merge that would add a path that already exists in t's checkout, untracked or
// ignored (I-2): git merge replaces an ignored file without a word, and an undo of a merge cut short would then
// delete it. The paths added are those of tree (the merge's result; head itself on a git without merge-tree
// --write-tree) that HEAD does not have; one is in the way when it, or a part of its path that is not a directory,
// exists on disk and git ls-files --others (which lists ignored files too) lists it, so a tracked file that only
// differs in case on a case-insensitive disk is not.
func reviewCheckOnDisk(ctx context.Context, t model.Task, tree, head string) error {
	to := tree
	if to == "" {
		to = head
	}
	gctx, cancel := context.WithTimeout(ctx, 2*worktreeGitLimit)
	defer cancel()
	res := runGit(gctx, t.Repo, "diff-tree", "-r", "-z", "--name-only", "--no-renames", "--diff-filter=A", "HEAD", to, "--")
	if res.err != nil {
		return worktreeGitError(res, worktreeGitLimit)
	}
	var near []string
	for p := range strings.SplitSeq(strings.TrimSuffix(res.raw, "\x00"), "\x00") {
		if q := reviewInTheWay(t.Repo, p); q != "" && !slices.Contains(near, q) {
			near = append(near, q)
		}
	}
	if len(near) == 0 {
		return nil
	}
	args := []string{"ls-files", "-z", "--others", "--"}
	for _, q := range near {
		args = append(args, ":(literal)"+q)
	}
	res = runGit(gctx, t.Repo, args...)
	if res.err != nil {
		return worktreeGitError(res, worktreeGitLimit)
	}
	others := strings.Split(strings.TrimSuffix(res.raw, "\x00"), "\x00")
	found := slices.DeleteFunc(near, func(q string) bool {
		return !slices.ContainsFunc(others, func(o string) bool { return o == q || strings.HasPrefix(o, q+"/") })
	})
	if len(found) == 0 {
		return nil
	}
	exist, it := "exists", "it"
	if len(found) > 1 {
		exist, it = "exist", "them"
	}
	if n := len(found) - reviewConflictsShown; n > 0 {
		found = append(found[:reviewConflictsShown:reviewConflictsShown], fmt.Sprintf("%d more", n))
	}
	repo := sanitize.Line(t.Repo, 300)
	return fmt.Errorf("merging %s into %s would add %s, which already %s in %s, untracked or ignored, and not in %s: git would "+
		"replace %s. Move %s out of the way there, then approve again. Nothing was merged", t.Branch, t.BaseBranch,
		sanitize.Line(strings.Join(found, ", "), reviewGitErrMax), exist, repo, t.BaseBranch, it, it)
}

// reviewInTheWay is the part of path p (p itself, or a leading part of it that is not a directory) that exists
// in repo's working tree, or "".
func reviewInTheWay(repo, p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	for i := 1; i <= len(parts); i++ {
		q := strings.Join(parts[:i], "/")
		fi, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(q)))
		switch {
		case errors.Is(err, os.ErrNotExist):
			return ""
		case err != nil || i == len(parts) || !fi.IsDir():
			return q
		}
	}
	return ""
}

// reviewCheckCheckout refuses a merge unless the user's checkout has t's
// base checked out and no operation under way.
func reviewCheckCheckout(ctx context.Context, t model.Task) error {
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	// The full name, not --short: a tag named like the base makes --short
	// print heads/<base> (M-4).
	res := runGit(ctx, t.Repo, "symbolic-ref", "-q", "HEAD")
	cur := strings.TrimPrefix(res.out, "refs/heads/")
	switch {
	case res.err != nil && worktreeExitCode(res) == 1 && res.stderr == "":
		cur = "a detached HEAD"
	case res.err != nil:
		return worktreeGitError(res, worktreeGitLimit)
	case cur == res.out:
		cur = res.out // not a branch: shown as it is
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

// reviewState is what an interrupted merge must leave as it found: HEAD's commit, git status, whether the
// index held staged changes, and the tracked paths whose working tree differed from the index, by what each holds.
type reviewState struct {
	head, status string
	staged       bool
	modified     map[string]string // path -> reviewWorkingPath's account of it; nil with more than reviewModifiedMax
}

func reviewRepoState(ctx context.Context, repo string) (reviewState, error) {
	head, err := worktreeRun(ctx, worktreeGitLimit, repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return reviewState{}, err
	}
	status, err := worktreeRun(ctx, worktreeGitLimit, repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return reviewState{}, err
	}
	staged, err := reviewStaged(ctx, repo, "HEAD")
	if err != nil {
		return reviewState{}, err
	}
	modified, err := reviewModified(ctx, repo)
	if errors.Is(err, errReviewTooManyModified) {
		err = nil // the merge may run; only an undo needs the comparison (reviewUnproven)
	}
	return reviewState{head: head, status: status, staged: staged, modified: modified}, err
}

// reviewStaged reports whether repo's index differs from tree (a tree-ish: HEAD, or a tree id).
func reviewStaged(ctx context.Context, repo, tree string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, repo, "diff-index", "--cached", "--quiet", tree, "--")
	switch {
	case res.err == nil:
		return false, nil
	case worktreeExitCode(res) == 1 && res.stderr == "":
		return true, nil
	}
	return false, worktreeGitError(res, worktreeGitLimit)
}

// reviewModifiedMax bounds the paths reviewModified reads; with more, an interrupted merge is never undone.
const reviewModifiedMax = 500

// errReviewTooManyModified is reviewModified's answer for a working tree with more than reviewModifiedMax
// modified paths.
var errReviewTooManyModified = fmt.Errorf("more than %d modified files", reviewModifiedMax)

// reviewTooMany is why an undo is not attempted in a checkout with more than reviewModifiedMax modified paths.
var reviewTooMany = fmt.Sprintf("the checkout has more than %d modified files, too many for Curia to prove that undoing "+
	"the merge touches nothing else", reviewModifiedMax)

// reviewUnread starts reviewWorkingPath's account of a path it could not read; such a path proves nothing.
const reviewUnread = "unreadable: "

// reviewModified maps each tracked path of repo whose working tree differs from the index (git diff --raw) to
// what the working tree holds there, by kind (reviewWorkingPath), so that an edit made while a merge ran is seen
// even in a file that was modified before (I-1: a deleted file, a symlink or a gitlink is no error).
func reviewModified(ctx context.Context, repo string) (map[string]string, error) {
	gctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(gctx, repo, "diff", "--raw", "-z", "--no-abbrev", "--no-renames", "--no-ext-diff")
	if res.err != nil {
		return nil, worktreeGitError(res, worktreeGitLimit)
	}
	fields := strings.Split(strings.TrimSuffix(res.raw, "\x00"), "\x00")
	if res.raw == "" {
		fields = nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("git diff --raw printed %d fields", len(fields))
	}
	if len(fields)/2 > reviewModifiedMax {
		return nil, errReviewTooManyModified
	}
	m := make(map[string]string, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 || fields[i+1] == "" {
			return nil, fmt.Errorf("git diff --raw printed %q", sanitize.Line(fields[i], 80))
		}
		m[fields[i+1]] = reviewWorkingPath(ctx, repo, fields[i+1], meta[0])
	}
	return m, nil
}

// reviewWorkingPath says what repo's working tree holds at path p, whose index entry has mode: "deleted", the
// target of a symlink, the commit a gitlink's repository has checked out, or a regular file's mode and the
// SHA-256 of its content. Anything else, or a read that fails, is reviewUnread and a reason.
func reviewWorkingPath(ctx context.Context, repo, p, mode string) string {
	full := filepath.Join(repo, filepath.FromSlash(p))
	fi, err := os.Lstat(full)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "deleted"
	case err != nil:
		return reviewUnread + err.Error()
	case fi.Mode()&os.ModeSymlink != 0:
		target, rerr := os.Readlink(full)
		if rerr != nil {
			return reviewUnread + rerr.Error()
		}
		return "symlink to " + target
	case fi.IsDir() && mode == "160000":
		commit, gerr := worktreeRun(ctx, worktreeGitLimit, full, "rev-parse", "--verify", "HEAD")
		if gerr != nil {
			return reviewUnread + "its repository's HEAD: " + gerr.Error()
		}
		return "gitlink at " + commit
	case !fi.Mode().IsRegular():
		return reviewUnread + "not a regular file (" + fi.Mode().Type().String() + ")"
	}
	f, err := os.Open(filepath.Clean(full))
	if err != nil {
		return reviewUnread + err.Error()
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return reviewUnread + err.Error()
	}
	return fmt.Sprintf("file %v %s", fi.Mode().Perm()&0o111 != 0, hex.EncodeToString(h.Sum(nil)))
}

// reviewReadable reports whether every path of m was read (none is reviewUnread).
func reviewReadable(m map[string]string) bool {
	for _, v := range m {
		if strings.HasPrefix(v, reviewUnread) {
			return false
		}
	}
	return true
}

// errReviewLeft marks the outcome of an interrupted merge that Curia left as it found it, because it could not
// prove the checkout held only its own merge: the review stays pending with its intent kept.
var errReviewLeft = errors.New("left as it is")

// reviewSituation is what reviewInspect reads of a checkout after a merge failed.
type reviewSituation struct {
	gitDir             string
	head               string
	underWay, unmerged bool // MERGE_HEAD exists; the index has conflict entries
	locked             bool // index.lock exists
}

// reviewInspect reads repo's situation after a merge failed.
func reviewInspect(ctx context.Context, repo string) (reviewSituation, error) {
	var s reviewSituation
	dir, err := worktreeRun(ctx, worktreeGitLimit, repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return s, err
	}
	if !filepath.IsAbs(dir) {
		return s, fmt.Errorf("git printed %q as the git dir", sanitize.Line(dir, 200))
	}
	s.gitDir = dir
	if s.head, err = worktreeRun(ctx, worktreeGitLimit, repo, "rev-parse", "--verify", "HEAD"); err != nil {
		return s, err
	}
	if s.underWay, err = refExists(ctx, repo, "MERGE_HEAD"); err != nil {
		return s, err
	}
	unmerged, err := worktreeRun(ctx, worktreeGitLimit, repo, "ls-files", "--unmerged")
	if err != nil {
		return s, err
	}
	s.unmerged = unmerged != ""
	_, lerr := os.Lstat(filepath.Join(dir, "index.lock"))
	s.locked = lerr == nil
	return s, nil
}

// reviewMergeMsg is git's message for failed merge res, cleaned, saying why git stopped; never empty.
func reviewMergeMsg(res gitResult) string {
	said := res.stderr
	if said == "" {
		said = res.out // a conflict is reported on stdout
	}
	msg := sanitize.Line(said, reviewGitErrMax)
	switch {
	case res.timedOut:
		msg = strings.TrimSuffix("git merge was stopped (its limit of "+reviewMergeLimit.String()+", or the action's, ran out): "+msg, ": ")
	case msg == "":
		msg = "git merge failed (" + sanitize.Line(res.err.Error(), 80) + ")"
	}
	return msg
}

// reviewMergeFailed follows merge res that did not succeed, on a context detached from the caller's. Only an
// interruption Curia can prove is its own merge and nothing else is undone (reviewUndo); a conflict (git stops
// before any hook runs) is aborted as git leaves it; in every other case nothing is run and the error, wrapping
// errReviewLeft, says what is there and what the user can run.
func reviewMergeFailed(ctx context.Context, t model.Task, res gitResult, before reviewState, tree string) error {
	msg := reviewMergeMsg(res)
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*worktreeGitLimit)
	defer cancel()
	repo := sanitize.Line(t.Repo, 300)
	sit, err := reviewInspect(actx, t.Repo)
	if err != nil {
		return fmt.Errorf("%s; afterwards the state of %s could not be read (%w): check git status there. %w", msg, repo, err, errReviewLeft)
	}
	switch {
	case sit.underWay && sit.unmerged: // a conflict: git wrote nothing but the merge, and ran no hook
		return reviewUndo(actx, t, msg, before, "merge", "--abort")
	case sit.underWay:
		// A hook refused the merge commit (HEAD unmoved), or the merge commit was made and git stopped in
		// post-merge (HEAD moved): the index must be exactly the merge's result.
		target := tree
		if sit.head != before.head {
			target = sit.head + "^{tree}"
		}
		return reviewUndoProven(actx, t, msg, before, sit, target, "merge", "--abort")
	case sit.head != before.head:
		return fmt.Errorf("%s; %s moved from %s to %s: check git -C %s log", msg, t.BaseBranch,
			reviewShort(before.head), sanitize.Line(reviewShort(sit.head), 20), repo)
	}
	after, err := reviewRepoState(actx, t.Repo)
	switch {
	case err != nil:
		return fmt.Errorf("%s; afterwards the state of %s could not be read (%w): check git status there. %w", msg, repo, err, errReviewLeft)
	case reviewSame(before, after):
		return fmt.Errorf("%s; nothing was merged%s", msg, reviewLeftovers(actx, t.Repo))
	case reviewCutShort(res, before):
		return reviewUndoProven(actx, t, msg+"; git merge was cut short before it recorded the merge", before, sit, tree, "reset", "--merge")
	}
	return reviewLeft(t, msg, before, after, "git stopped on its own and Curia cannot tell what it changed")
}

// reviewCutShort reports whether merge res was stopped before it ended on
// its own (Curia stopped it, or a signal killed it) in a checkout whose
// index was clean before.
func reviewCutShort(res gitResult, before reviewState) bool {
	if before.staged {
		return false
	}
	var exit *exec.ExitError
	if res.timedOut {
		return true
	}
	if !errors.As(res.err, &exit) {
		return false
	}
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// reviewSame reports whether after is before in every way an undo must restore: HEAD, the index equal to
// HEAD, git status with untracked files, and the content of every modified file.
func reviewSame(before, after reviewState) bool {
	return after.head == before.head && after.staged == before.staged && after.status == before.status &&
		before.modified != nil && after.modified != nil && reviewReadable(before.modified) && maps.Equal(before.modified, after.modified)
}

// reviewUndoProven runs the undo (git merge --abort, or git reset --merge) only when the checkout provably
// holds Curia's merge and nothing else: no index.lock; the index exactly target, the tree the merge produced
// (from git merge-tree before it ran, or the merge commit's); the working tree different from the index only in
// files that differed before the merge, with the same content. Anything that cannot be proven runs nothing.
func reviewUndoProven(ctx context.Context, t model.Task, msg string, before reviewState, sit reviewSituation, target string, undo ...string) error {
	if why := reviewUnproven(ctx, t, before, sit, target); why != "" {
		after, err := reviewRepoState(ctx, t.Repo)
		if err != nil {
			after = reviewState{status: "(git status could not be read)"}
		}
		return reviewLeft(t, msg, before, after, why)
	}
	return reviewUndo(ctx, t, msg, before, undo...)
}

// reviewUnproven says why the checkout cannot be shown to hold only Curia's merge, or "" when it can.
func reviewUnproven(ctx context.Context, t model.Task, before reviewState, sit reviewSituation, target string) string {
	switch {
	case target == "":
		return "this git cannot compute a merge's result beforehand (git merge-tree --write-tree needs git 2.38)"
	case sit.locked:
		return sanitize.Line(filepath.Join(sit.gitDir, "index.lock"), 300) + " exists: a git was killed, or one still runs"
	case before.modified == nil:
		return reviewTooMany
	}
	if differs, err := reviewStaged(ctx, t.Repo, target); err != nil || differs {
		return "the index holds changes that are not the merge's (something was staged while the merge ran)"
	}
	now, err := reviewModified(ctx, t.Repo)
	switch {
	case errors.Is(err, errReviewTooManyModified):
		return reviewTooMany
	case err != nil:
		return "the working tree could not be read: " + sanitize.Line(err.Error(), 200)
	}
	for _, p := range slices.Sorted(maps.Keys(now)) {
		switch id := now[p]; {
		case strings.HasPrefix(id, reviewUnread):
			return "Curia could not read " + sanitize.Line(p, 200) + " in the working tree (" + sanitize.Line(strings.TrimPrefix(id, reviewUnread), 200) + ")"
		case before.modified[p] != id:
			return "files changed in the working tree while the merge ran (" + sanitize.Line(p, 200) + ")"
		}
	}
	return ""
}

// reviewUndo runs git undo... (merge --abort, or reset --merge: the only two Curia runs, never --hard,
// checkout or clean) and reports whether the checkout is then as it was before the merge (reviewSame).
func reviewUndo(ctx context.Context, t model.Task, msg string, before reviewState, undo ...string) error {
	repo := sanitize.Line(t.Repo, 300)
	cmd := strings.Join(undo, " ")
	if err := reviewRunGroup(ctx, worktreeGitLimit, t.Repo, undo...); err != nil {
		return fmt.Errorf("%s; git %s failed (%w). Nothing was merged, but %s holds what the merge left: look with git -C %s status. %w",
			msg, cmd, err, repo, repo, errReviewLeft)
	}
	after, err := reviewRepoState(ctx, t.Repo)
	switch {
	case err != nil:
		return fmt.Errorf("%s; Curia ran git %s, then the state of %s could not be read (%w): check git status there", msg, cmd, repo, err)
	case !reviewSame(before, after):
		return fmt.Errorf("%s; after git %s, %s still differs from before the merge (%s): check it there", msg, cmd, repo,
			reviewStateDiff(before, after))
	}
	if undo[0] == "reset" {
		return fmt.Errorf("%s; Curia undid what it had staged (git reset --merge) and %s is as it was. Nothing was merged%s",
			msg, repo, reviewLeftovers(ctx, t.Repo))
	}
	return fmt.Errorf("%s; nothing was merged%s", msg, reviewLeftovers(ctx, t.Repo))
}

// reviewLeft is the outcome when Curia runs nothing after an interrupted merge: what it found, why it cannot
// undo it, and what the user can run. It wraps errReviewLeft: the review stays pending with its intent kept.
func reviewLeft(t model.Task, msg string, before, after reviewState, why string) error {
	repo := sanitize.Line(t.Repo, 300)
	diff := reviewStateDiff(before, after)
	if diff == "" {
		diff = "git status as before"
	}
	return fmt.Errorf("%s; the merge was interrupted and %s holds changes Curia cannot attribute to its own merge (%s), so it "+
		"ran nothing there (%s). Look with git -C %s status; to undo the merge yourself, git -C %s merge --abort while a merge "+
		"is under way, or git -C %s reset --merge, either of which also undoes anything staged since. The review stays pending: %w",
		msg, repo, why, diff, repo, repo, repo, errReviewLeft)
}

// reviewLeftoverFiles are the files git may leave in its directory after a merge that did not complete:
// pointers to commits (where HEAD was, the attempted merge's tree) that change nothing in the checkout.
var reviewLeftoverFiles = []string{"ORIG_HEAD", "AUTO_MERGE"}

// reviewLeftovers is the sentence that names the reviewLeftoverFiles in repo's git directory, "" when none
// is there (live check defect 9): HEAD and git status are as before, so they are no difference to report,
// but a user who looks finds them.
func reviewLeftovers(ctx context.Context, repo string) string {
	dir, err := worktreeRun(ctx, worktreeGitLimit, repo, "rev-parse", "--absolute-git-dir")
	if err != nil || !filepath.IsAbs(dir) {
		return ""
	}
	var found []string
	for _, f := range reviewLeftoverFiles {
		if fi, err := os.Lstat(filepath.Join(dir, f)); err == nil && fi.Mode().IsRegular() {
			found = append(found, f)
		}
	}
	if len(found) == 0 {
		return ""
	}
	return ". git may have left " + strings.Join(found, " and ") + " in " + sanitize.Line(dir, 300) +
		": they are harmless and need nothing"
}

// reviewStateMax bounds the status lines reviewStateDiff names, each way.
const reviewStateMax = 5

// reviewStateDiff says how after differs from before: HEAD, and the git
// status lines found only before or only after (cleaned, at most
// reviewStateMax each way).
func reviewStateDiff(before, after reviewState) string {
	var parts []string
	if before.head != after.head {
		parts = append(parts, "HEAD was "+reviewShort(before.head)+", is "+sanitize.Line(reviewShort(after.head), 20))
	}
	was, is := strings.Split(before.status, "\n"), strings.Split(after.status, "\n")
	only := func(of, in []string) []string {
		var out []string
		for _, l := range of {
			if l != "" && !slices.Contains(in, l) {
				out = append(out, l)
			}
		}
		if n := len(out) - reviewStateMax; n > 0 {
			out = append(out[:reviewStateMax], fmt.Sprintf("%d more", n))
		}
		return out
	}
	if gone := only(was, is); len(gone) > 0 {
		parts = append(parts, "no longer in git status: "+strings.Join(gone, ", "))
	}
	if added := only(is, was); len(added) > 0 {
		parts = append(parts, "new in git status: "+strings.Join(added, ", "))
	}
	if (before.modified == nil || after.modified == nil) && after.head != "" { // after.head "": after could not be read
		parts = append(parts, "the content of modified files not compared ("+errReviewTooManyModified.Error()+")")
	}
	return sanitize.Line(strings.Join(parts, "; "), reviewGitErrMax)
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
	if err = reviewRunGroup(ctx, reviewPushLimit, t.Repo, "push", remote, head+":refs/heads/"+t.Branch); err != nil {
		return "", fmt.Errorf("%w; %s was not pushed and no pull request was opened", err, t.Branch)
	}
	pushed := fmt.Sprintf("%s is pushed to %s at %s", t.Branch, remote, reviewShort(head))
	if err = reviewSameHead(ctx, t, head); err != nil {
		return "", fmt.Errorf("%s, but %w", pushed, err)
	}
	title, body := task.PRTitle(t), task.PRBody(t, r, decided)
	gctx, cancel := context.WithTimeout(ctx, reviewGHLimit)
	defer cancel()
	out, err := reviewGH(gctx, t.Repo, "pr", "create", "--base", t.BaseBranch, "--head", t.Branch, "--title", title, "--body", body)
	if err == nil {
		return reviewPRURL(out), nil
	}
	// The pull request may exist already, opened by an earlier approve whose gh failed late: it is adopted
	// only when it is provably this approval's (reviewExistingPR).
	url, other := reviewExistingPR(context.WithoutCancel(ctx), t, remote, head, title, body)
	switch {
	case other != nil:
		return "", fmt.Errorf("%s, but %w", pushed, other)
	case url != "":
		return url, nil
	}
	return "", fmt.Errorf("%s, but gh pr create failed: %s. The review stays pending: approve again to retry",
		pushed, sanitize.Line(err.Error(), reviewGitErrMax))
}

// reviewPRView is what reviewExistingPR reads of gh pr view's JSON. It is untrusted: every field is
// compared with what this approval expects, or validated, before use.
type reviewPRView struct {
	URL               string `json:"url"`
	State             string `json:"state"`
	BaseRefName       string `json:"baseRefName"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	Title             string `json:"title"`
	Body              string `json:"body"`
}

// reviewPRFields are the fields reviewExistingPR asks gh pr view for.
const reviewPRFields = "url,state,baseRefName,headRefName,headRefOid,isCrossRepository,title,body"

// reviewExistingPR looks for the pull request of t's branch when gh pr create failed, and returns its URL
// when it is provably the one this approval would have opened: open, from t's branch of the same repository
// (not a fork) into t's base, at exactly commit head, with exactly title and body, at the URL of a pull
// request of the repository remote names. One that matches in all but its title or text was not opened by
// Curia for this review: other says so and names its URL, and the review stays pending; Curia never edits
// it. Anything else, a gh that fails included, is "" and nil.
func reviewExistingPR(ctx context.Context, t model.Task, remote, head, title, body string) (url string, other error) {
	pr, ok := reviewViewPR(ctx, t)
	if !ok || pr.State != "OPEN" || pr.IsCrossRepository || pr.BaseRefName != t.BaseBranch ||
		pr.HeadRefName != t.Branch || pr.HeadRefOid != head || !reviewRepoPRURL(ctx, t, remote, pr.URL) {
		return "", nil
	}
	if pr.Title != title || pr.Body != body {
		return "", fmt.Errorf("a pull request for %s already exists with another title or text than Curia gives this review: %s. "+
			"Curia did not open it, so it is neither recorded nor changed; the review stays pending. Look at it, then close it "+
			"and approve again, or approve with --merge", t.Branch, pr.URL)
	}
	return pr.URL, nil
}

// reviewViewPR is gh pr view's JSON for t's branch (argv, bounded by reviewGHLimit; gh's output is capped),
// and whether gh gave one.
func reviewViewPR(ctx context.Context, t model.Task) (reviewPRView, bool) {
	gctx, cancel := context.WithTimeout(ctx, reviewGHLimit)
	defer cancel()
	out, err := reviewGH(gctx, t.Repo, "pr", "view", t.Branch, "--json", reviewPRFields)
	var pr reviewPRView
	if err != nil || json.Unmarshal(out, &pr) != nil {
		return reviewPRView{}, false
	}
	return pr, true
}

// reviewRemoteURL is the form of a remote's URL whose host and repository path a pull request's URL must
// share: https://host/owner/repo, git@host:owner/repo or ssh://git@host/owner/repo, with or without .git.
var reviewRemoteURL = regexp.MustCompile(`^(?:https://|ssh://git@|git@)([A-Za-z0-9.-]{1,100})[/:]([A-Za-z0-9._-]{1,100})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?/?$`)

// reviewRepoPRURL reports whether url is the URL of a pull request of the repository t's remote names:
// https://<its host>/<owner>/<repo>/pull/<number>. A remote whose URL has no such form (a local path) has
// no pull request URL that can be checked, so none is accepted.
func reviewRepoPRURL(ctx context.Context, t model.Task, remote, url string) bool {
	got, err := worktreeRun(ctx, worktreeGitLimit, t.Repo, "remote", "get-url", "--", remote)
	if err != nil {
		return false
	}
	m := reviewRemoteURL.FindStringSubmatch(got)
	if m == nil {
		return false
	}
	want := regexp.MustCompile(`^https://` + regexp.QuoteMeta(m[1]) + `/` + regexp.QuoteMeta(m[2]) + `/` +
		regexp.QuoteMeta(m[3]) + `/pull/[1-9][0-9]{0,9}$`)
	return want.MatchString(url)
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

// reviewPagerVars are the variables a pager may come from, which the diff
// command of the git and delta tools does not inherit: it sets its own.
var reviewPagerVars = []string{"GIT_PAGER", "PAGER", "LESS", "DELTA_PAGER", "BAT_PAGER", "LV"}

// reviewPagerEnv is the fixed pager of the git and delta diff tools: less, without -F (git's default LESS is
// FRX, whose -F quits at once when the diff fits one screen, so the view came back before the diff was seen
// and the next q, meant for the pager, closed the view), with -R (git's and delta's colors) and -X (no screen
// clearing on exit, so the last page stays readable). Constants only: nothing here comes from the config or
// the caller's environment. GIT_PAGER names the pager for the git tool; the delta tool's pager is delta,
// named by -c core.pager, which pages through DELTA_PAGER.
const (
	reviewLess       = "-R -X"
	reviewGitPager   = "less"
	reviewDeltaPager = "less -R -X"
)

// reviewDiffEnv is gitEnv without reviewPagerVars, plus the fixed pager of
// tool (reviewPagerEnv).
func reviewDiffEnv(tool string) []string {
	env := slices.DeleteFunc(gitEnv(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(reviewPagerVars, name)
	})
	env = append(env, "LESS="+reviewLess)
	switch tool {
	case "git":
		env = append(env, "GIT_PAGER="+reviewGitPager)
	case "delta":
		env = append(env, "DELTA_PAGER="+reviewDeltaPager)
	}
	return env
}

// reviewDiffCmd is the command of section 4.6's diff table for tool.
// It is not bound to a context: it runs as long as the user keeps the tool open; the git and delta tools
// page through a less that stays until the user leaves it, whatever the diff's length (reviewDiffEnv).
func reviewDiffCmd(t model.Task, tool string) (*exec.Cmd, error) {
	if err := reviewCheckNames(t); err != nil {
		return nil, err
	}
	rng := "refs/heads/" + t.BaseBranch + "...refs/heads/" + t.Branch
	var bin string
	var args, needs []string
	switch tool {
	case "git":
		bin, args = "git", []string{"-C", t.Repo, "diff", rng}
	case "delta":
		bin, args, needs = "git", []string{"-C", t.Repo, "-c", "core.pager=delta --paging=always", "diff", rng}, []string{"delta"}
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
	cmd.Env = reviewDiffEnv(tool)
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

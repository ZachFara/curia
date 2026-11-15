package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		"--stat="+strconv.Itoa(width), t.BaseBranch+"..."+head, "--")
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
// runs them. It refuses a checkout whose index holds staged changes, so that a merge cut short can be undone
// without touching the user's own work (reviewUndoCutShort). After a failure the repository is as it was, or
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
	mctx, cancel := context.WithTimeout(ctx, reviewMergeLimit)
	defer cancel()
	res := runGitGroup(mctx, t.Repo, "merge", "--no-ff", "--no-edit", "--no-autostash",
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
// and git status, and whether the index held staged changes.
type reviewState struct {
	head, status string
	staged       bool
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
	staged, err := reviewStaged(ctx, repo)
	return reviewState{head: head, status: status, staged: staged}, err
}

// reviewStaged reports whether repo's index differs from HEAD.
func reviewStaged(ctx context.Context, repo string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, worktreeGitLimit)
	defer cancel()
	res := runGit(ctx, repo, "diff-index", "--cached", "--quiet", "HEAD", "--")
	switch {
	case res.err == nil:
		return false, nil
	case worktreeExitCode(res) == 1 && res.stderr == "":
		return true, nil
	}
	return false, worktreeGitError(res, worktreeGitLimit)
}

// reviewMergeFailed aborts the merge res left under way (MERGE_HEAD exists:
// a conflict, or a hook that refused the merge commit), or undoes one that
// was cut short before git recorded it (reviewUndoCutShort), then checks the
// repository is as before. The error is git's message, cleaned, and what
// state the repository is in.
func reviewMergeFailed(ctx context.Context, t model.Task, res gitResult, before reviewState) error {
	said := res.stderr
	if said == "" {
		said = res.out // a conflict is reported on stdout
	}
	msg := sanitize.Line(said, reviewGitErrMax)
	if res.timedOut {
		msg = strings.TrimSuffix("git merge was stopped (its limit of "+reviewMergeLimit.String()+", or the action's, ran out): "+msg, ": ")
	}
	// The caller's context may be what ended the merge; cleaning up must still run.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*worktreeGitLimit)
	defer cancel()
	repo := sanitize.Line(t.Repo, 300)
	underWay, _ := refExists(actx, t.Repo, "MERGE_HEAD")
	if underWay {
		if err := reviewRunGroup(actx, worktreeGitLimit, t.Repo, "merge", "--abort"); err != nil {
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
	case after.status == before.status:
		return fmt.Errorf("%s; nothing was merged%s", msg, reviewLeftovers(actx, t.Repo))
	case !underWay && reviewCutShort(res, before):
		return reviewUndoCutShort(actx, t, msg, before)
	case underWay:
		return fmt.Errorf("%s; the merge was aborted, but git status in %s differs from before the merge (%s): check it there",
			msg, repo, reviewStateDiff(before, after))
	}
	return fmt.Errorf("%s; nothing was merged, but git status in %s differs from before the merge (%s). Curia left it as it is: "+
		"if what differs is what the merge staged, git -C %s reset --merge undoes it; check git status there first",
		msg, repo, reviewStateDiff(before, after), repo)
}

// reviewCutShort reports whether merge res was stopped before it ended on
// its own (Curia stopped it, or a signal killed it) in a checkout whose
// index was clean before: the one case where Curia undoes it itself.
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

// reviewUndoCutShort undoes Curia's own merge that was cut short before git wrote MERGE_HEAD (a hook still
// ran): HEAD has not moved, the index was clean before (reviewMerge refuses staged changes), and what differs
// is what the merge staged. git reset --merge, what git merge --abort runs, resets the index and the files
// the merge changed and keeps unstaged and untracked work. The checkout is then compared with the state before
// the merge (HEAD and git status) and the error says exactly what differs, if anything; nothing more is tried.
// It is the only reset Curia ever runs (Ruling P4-R5): never another form, --hard, checkout or clean.
func reviewUndoCutShort(ctx context.Context, t model.Task, msg string, before reviewState) error {
	repo := sanitize.Line(t.Repo, 300)
	cut := msg + "; git merge was cut short before it recorded the merge"
	if err := reviewRunGroup(ctx, worktreeGitLimit, t.Repo, "reset", "--merge"); err != nil {
		return fmt.Errorf("%s, and undoing what it had staged failed (%w). Nothing was merged, but %s holds what the merge "+
			"staged: run git -C %s reset --merge there once no git runs in it", cut, err, repo, repo)
	}
	after, err := reviewRepoState(ctx, t.Repo)
	switch {
	case err != nil:
		return fmt.Errorf("%s; Curia ran git reset --merge, then the state of %s could not be read (%w): check git status there",
			cut, repo, err)
	case after.head != before.head || after.status != before.status:
		return fmt.Errorf("%s; after git reset --merge, %s still differs from before the merge (%s): check it there",
			cut, repo, reviewStateDiff(before, after))
	}
	return fmt.Errorf("%s; Curia undid what it had staged (git reset --merge) and %s is as it was. Nothing was merged%s",
		cut, repo, reviewLeftovers(ctx, t.Repo))
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
	rng := t.BaseBranch + "..." + t.Branch
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

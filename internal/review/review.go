package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
)

// Resolve turns a task id or a review id into the pending review it names.
func Resolve(ctx context.Context, st *store.Store, arg string) (model.Review, error) {
	switch {
	case task.ValidReviewID(arg):
		r, _, err := Pending(ctx, st, arg)
		return r, err
	case task.ValidID(arg):
		rs, err := st.ListReviews(ctx, store.ReviewFilter{TaskID: arg, Statuses: []model.ReviewStatus{model.ReviewPending}})
		if err != nil {
			return model.Review{}, err
		}
		if len(rs) == 0 {
			return model.Review{}, fmt.Errorf("%s has no review waiting", arg)
		}
		return rs[0], nil
	}
	return model.Review{}, fmt.Errorf("%q is not a task id (T-<n>) or a review id (R-<n>)", sanitize.Line(arg, 40))
}

// Pending reads review id, which must be pending, and its task. The
// ids read back from the store are checked again.
func Pending(ctx context.Context, st *store.Store, id string) (model.Review, model.Task, error) {
	if !task.ValidReviewID(id) {
		return model.Review{}, model.Task{}, fmt.Errorf("%q is not a review id (R-<n>)", sanitize.Line(id, 40))
	}
	r, found, err := st.GetReview(ctx, id)
	switch {
	case err != nil:
		return model.Review{}, model.Task{}, err
	case !found:
		return model.Review{}, model.Task{}, fmt.Errorf("no review %s", id)
	case r.Status != model.ReviewPending:
		return model.Review{}, model.Task{}, fmt.Errorf("%s is %s", id, task.ReviewStatusWord(r.Status))
	case !task.ValidID(r.TaskID):
		return model.Review{}, model.Task{}, fmt.Errorf("%s names task %q, which is not a task id", id, sanitize.Line(r.TaskID, 40))
	}
	t, found, err := st.GetTask(ctx, r.TaskID)
	if err == nil && !found {
		err = fmt.Errorf("%s's task %s does not exist", id, r.TaskID)
	}
	return r, t, err
}

// Approval is what Approve is asked: the store and the configuration, the review, how to approve it ("" takes
// review.approve), the commit the user was shown, who is asking and when.
type Approval struct {
	Store    *store.Store
	Config   config.Config
	ReviewID string
	How      model.ApproveHow
	Seen     string
	Via      model.AnsweredVia
	Now      time.Time
}

// Approve approves pending review reviewID as how says ("" takes review.approve): a merge, or a pull
// request. seen is the full commit id of the branch head the user was shown (the view's, or the flag form's
// --head), and the only commit approved: when the branch is elsewhere it refuses. Before git runs, the approval's intent (how, the head, the stat) is
// recorded on the review, which is also its claim: a second approval is refused while it stands. An intent
// to merge whose head is already in the base, left by an approval that was killed after git merged, is
// recorded as merged. A refusal or failure leaves the review pending. Approve, RequestChanges and Drop are
// the three actions of section 4.6; each returns its outcome line.
func Approve(ctx context.Context, p Approval) (string, error) {
	st, cfg, how, seen, via, now := p.Store, p.Config, p.How, p.Seen, p.Via, p.Now
	r, t, err := Pending(ctx, st, p.ReviewID)
	if err != nil {
		return "", err
	}
	if t.Status == model.TaskDropped {
		return "", fmt.Errorf("%s is dropped: nothing to approve", t.ID)
	}
	if how == "" {
		how = model.ApproveHow(cfg.Review.Approve)
	}
	if how != model.ApproveMerge && how != model.ApprovePR {
		return "", fmt.Errorf("unknown approve method %q: want merge or pr", sanitize.Line(string(how), 20))
	}
	if line, done, ferr := reviewFoundDone(ctx, st, r, t, seen, via, now); done {
		return line, ferr
	}
	stat, head, err := Check(ctx, st, t)
	if err != nil {
		return "", err
	}
	if seen != head {
		if via == model.AnsweredCLI { // the flag form shows nothing: say where to look
			return "", fmt.Errorf("the branch changed since you looked: %s is now at %s. Look at it again with curia review %s",
				t.Branch, head, r.ID)
		}
		return "", gitops.ErrBranchChanged
	}
	if err = reviewApproveReady(ctx, r, t, how, now); err != nil {
		return "", err
	}
	prior := r // its intent, if any, is what the claim may replace
	if r, err = st.ClaimReviewIntent(ctx, r.ID, store.ReviewIntent{How: how, Head: head, DiffStat: stat}, now, IntentHold); err != nil {
		return "", reviewBusyError(r, err)
	}
	claimed := claimedApproval{st: st, cfg: cfg, r: r, prior: prior, t: t, stat: stat, head: head, via: via, now: now}
	if how == model.ApproveMerge {
		return approveMerge(ctx, claimed)
	}
	return approvePR(ctx, claimed)
}

// reviewApproveReady runs an approval's refusals that come before its claim, so that a refusal touches no
// intent (C-B, C-2): an earlier approval's merge that may still run, then, for a merge, the checkout's state.
func reviewApproveReady(ctx context.Context, r model.Review, t model.Task, how model.ApproveHow, now time.Time) error {
	if err := reviewEarlierMerge(ctx, r, t, now, "approve"); err != nil || how != model.ApproveMerge {
		return err
	}
	return reviewMergeReady(ctx, r, t)
}

// reviewMergeReady runs the checks of the user's checkout that refuse a merge (the base checked out, no
// operation under way, no staged change) before the approval claims the review (C-B); an earlier approval's
// git that may still run there is refused before, by reviewEarlierMerge.
func reviewMergeReady(ctx context.Context, r model.Review, t model.Task) error {
	if err := gitops.CheckNames(t); err != nil {
		return err
	}
	if err := gitops.CheckCheckout(ctx, t); err != nil {
		return reviewEarlierApproval(r, t, err)
	}
	staged, err := gitops.Staged(ctx, t.Repo, "HEAD")
	switch {
	case err != nil:
		return err
	case staged:
		return fmt.Errorf("staged changes in %s: commit or unstage them first. Nothing was merged", sanitize.Line(t.Repo, 300))
	}
	return nil
}

// reviewEarlierMerge refuses action verb ("approve", "drop", "request changes") on review r while an earlier
// approval's intent to merge stands on r and its git may still be running in t's checkout (C-2): while the intent
// is younger than the hold, as busy, as the claim would; once older, when a merge is under way there or the index
// holds staged changes (that approval's process may have been killed with its git still in a hook). Every action
// that claims or closes a review calls it first, so no outcome replaces an intent whose merge may still complete.
func reviewEarlierMerge(ctx context.Context, r model.Review, t model.Task, now time.Time, verb string) error {
	if r.IntentAt.IsZero() || r.ApprovedHow != model.ApproveMerge {
		return nil
	}
	if r.IntentAt.After(now.Add(-IntentHold)) {
		return reviewBusyAgain(r, store.ErrReviewBusy, verb)
	}
	if !reviewHasCheckout(t) {
		return nil
	}
	op, err := gitops.InProgress(ctx, t.Repo)
	if err != nil {
		return err
	}
	staged, err := gitops.Staged(ctx, t.Repo, "HEAD")
	if err != nil {
		return err
	}
	if op != "a merge" && !staged {
		return nil
	}
	then := "; if its merge reached " + t.BaseBranch + " by then, approve instead, which records the review merged"
	if verb == "approve" {
		then = ", which records the review merged once its merge is in " + t.BaseBranch
	}
	repo := sanitize.Line(t.Repo, 300)
	return fmt.Errorf("%s holds a merge under way or staged changes, and an earlier approval of %s (merge at %s, since %s) may still "+
		"be running git there. Nothing was done: wait for it to finish, then %s again%s. If no git runs there, what is staged "+
		"is what that approval's merge left: look with git -C %s status",
		repo, r.ID, sanitize.Line(gitops.Short(r.Head), 10), r.IntentAt.Local().Format("15:04:05"), verb, then, repo)
}

// reviewEarlierApproval is refusal err of the checkout, with a sentence added when an earlier approval's
// intent to merge stands on r (M-a). reviewEarlierMerge has refused before while that approval's git may still
// run there, so err is the reason, kept as it is; the sentence says what approving again then does.
func reviewEarlierApproval(r model.Review, t model.Task, err error) error {
	if r.IntentAt.IsZero() || r.ApprovedHow != model.ApproveMerge {
		return err
	}
	return fmt.Errorf("%w. An earlier approval of %s (merge at %s, since %s) did not finish: once its merge is in %s, approving "+
		"again records it", err, r.ID, sanitize.Line(gitops.Short(r.Head), 10), r.IntentAt.Local().Format("15:04:05"), t.BaseBranch)
}

// reviewHasCheckout reports whether t names a checkout an approval could have run git in: its names are valid
// and its repository path exists.
func reviewHasCheckout(t model.Task) bool {
	if gitops.CheckNames(t) != nil {
		return false
	}
	_, err := os.Stat(t.Repo)
	return !errors.Is(err, os.ErrNotExist)
}

// reviewRelease gives up claim r after its approval failed with nothing done: it puts back the earlier
// intent the claim replaced (prior's), whose approval may still have git running (C-B), or clears r's.
func reviewRelease(ctx context.Context, st *store.Store, r, prior model.Review) error {
	return st.RestoreReviewIntent(ctx, r.ID, r.IntentAt,
		store.ReviewIntent{How: prior.ApprovedHow, Head: prior.Head, DiffStat: prior.DiffStat}, prior.IntentAt)
}

// reviewBusyError says that review r is being approved elsewhere when err is
// store.ErrReviewBusy, else is err.
func reviewBusyError(r model.Review, err error) error {
	return reviewBusyAgain(r, err, "approve")
}

// reviewBusyAgain is reviewBusyError for an action named by verb ("approve", "drop", "request changes"),
// which the refusal says to try again.
func reviewBusyAgain(r model.Review, err error, verb string) error {
	if !errors.Is(err, store.ErrReviewBusy) {
		return err
	}
	since := r.IntentAt.Local()
	return fmt.Errorf("%s is being approved (%s at %s) by another curia since %s: wait for it to finish. "+
		"If that curia was killed, %s again after %s", r.ID, sanitize.Line(string(r.ApprovedHow), 10),
		sanitize.Line(gitops.Short(r.Head), 10), since.Format("15:04:05"), verb, since.Add(IntentHold).Format("15:04:05"))
}

// reviewBusyNow is reviewBusyError with review r read again, so that the
// refusal names the approval in flight as it is stored now.
func reviewBusyNow(ctx context.Context, st *store.Store, r model.Review, err error, verb string) error {
	if errors.Is(err, store.ErrReviewBusy) {
		if cur, found, gerr := st.GetReview(ctx, r.ID); gerr == nil && found {
			r = cur
		}
	}
	return reviewBusyAgain(r, err, verb)
}

// reviewFoundDone records review r merged, and reports done, when an approval recorded the intent to merge
// r's head, the branch is still at that head, which is seen, and the base already
// contains it: an approval whose process was killed after git merged. Otherwise it does nothing.
func reviewFoundDone(ctx context.Context, st *store.Store, r model.Review, t model.Task, seen string,
	via model.AnsweredVia, now time.Time,
) (line string, done bool, err error) {
	if r.IntentAt.IsZero() || r.ApprovedHow != model.ApproveMerge || !gitops.HeadPattern.MatchString(r.Head) ||
		(seen != "" && seen != r.Head) || !mergedAtHead(ctx, t, r.Head) {
		return "", false, nil
	}
	what := fmt.Sprintf("%s (%s) is already merged into %s; recorded", t.Branch, gitops.Short(r.Head), t.BaseBranch)
	_, expired, err := st.CloseReview(ctx, r.ID, store.ReviewClose{
		Status: model.ReviewMerged, How: model.ApproveMerge, DiffStat: r.DiffStat, Head: r.Head, Via: via,
	}, now)
	if err != nil {
		return "", true, reviewRecordError(ctx, st, r, what, r.Head, err)
	}
	return fmt.Sprintf("%s approved: %s.%s Remove the worktree and branch with: curia clean %s",
		r.ID, what, reviewExpired(expired), t.ID), true, nil
}

// mergedAtHead reports whether t's branch is at head and t's base contains
// it. Anything it cannot read is false.
func mergedAtHead(ctx context.Context, t model.Task, head string) bool {
	if gitops.CheckNames(t) != nil {
		return false
	}
	cur, err := gitops.BranchHead(ctx, t)
	if err != nil || cur != head {
		return false
	}
	n, err := gitops.Ahead(ctx, t, head)
	return err == nil && n == 0
}

// claimedApproval is an approval that holds its claim: the review as claimed (r) and as it was before
// (prior, for the rollback), its task, the diff stat and head the user saw, who approves and when.
type claimedApproval struct {
	st         *store.Store
	cfg        config.Config
	r, prior   model.Review
	t          model.Task
	stat, head string
	via        model.AnsweredVia
	now        time.Time
}

// approveMerge merges and records the review merged. When the merge fails its intent is cleared, unless
// the base holds the head after all, so that approving again records it.
func approveMerge(ctx context.Context, a claimedApproval) (string, error) {
	st, r, prior, t, stat, head, via, now := a.st, a.r, a.prior, a.t, a.stat, a.head, a.via, a.now
	what := fmt.Sprintf("merged %s into %s (%s)", t.Branch, t.BaseBranch, gitops.Short(head))
	if err := gitops.Merge(ctx, t, head); err != nil {
		if !baseContainsHead(ctx, t, head) {
			return "", reviewMergeRelease(ctx, st, r, prior, err)
		}
		// git was stopped after it made the merge commit (in post-merge): the merge is done and is recorded now
		// (M-9), whatever ended the caller's context.
		what += ", though git was stopped after the merge commit (" + sanitize.Line(err.Error(), reviewLineMax) + ")"
		ctx = context.WithoutCancel(ctx)
	}
	_, expired, err := st.CloseReview(ctx, r.ID, store.ReviewClose{
		Status: model.ReviewMerged, How: model.ApproveMerge, DiffStat: stat, Head: head, Via: via,
	}, now)
	if err != nil {
		return "", reviewRecordError(ctx, st, r, what, head, err)
	}
	return fmt.Sprintf("%s approved: %s.%s Remove the worktree and branch with: curia clean %s",
		r.ID, what, reviewExpired(expired), t.ID), nil
}

// baseContainsHead reports whether t's base contains head after a merge failed (git stopped after the merge
// commit), on a context detached from ctx's end; a read that fails is false.
func baseContainsHead(ctx context.Context, t model.Task, head string) bool {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitops.GitLimit)
	defer cancel()
	n, err := gitops.Ahead(actx, t, head)
	return err == nil && n == 0
}

// reviewMergeRelease follows a failed merge of head that left it outside the base: it keeps r's intent when
// Curia left the checkout as it found it (gitops.ErrLeft), else clears it (or puts back the earlier intent it
// replaced: reviewRelease), and returns the merge's error.
func reviewMergeRelease(ctx context.Context, st *store.Store, r, prior model.Review, failed error) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitops.GitLimit)
	defer cancel()
	if errors.Is(failed, gitops.ErrLeft) { // what git left is unknown: the intent stays, the claim with it
		return failed
	}
	if err := reviewRelease(actx, st, r, prior); err != nil {
		return fmt.Errorf("%w (and clearing the approval's claim failed: %s)", failed, sanitize.Line(err.Error(), gitops.ErrMax))
	}
	return failed
}

// approvePR opens the pull request and records the review approved; on a failure its intent is cleared.
func approvePR(ctx context.Context, a claimedApproval) (string, error) {
	st, cfg, r, prior, t, stat, head, via, now := a.st, a.cfg, a.r, a.prior, a.t, a.stat, a.head, a.via, a.now
	ds, err := st.TaskDecisions(ctx, t.ID)
	var url string
	if err == nil {
		_, _, decided := Split(ds, now)
		url, err = reviewOpenPR(ctx, t, r, decided, cfg.Review.Remote, head)
	}
	if err != nil {
		if rerr := reviewRelease(context.WithoutCancel(ctx), st, r, prior); rerr != nil {
			err = fmt.Errorf("%w (and clearing the approval's claim failed: %s)", err, sanitize.Line(rerr.Error(), gitops.ErrMax))
		}
		return "", err
	}
	what := "pull request " + url + " opened for " + t.Branch
	if url == "" {
		what = "pull request opened for " + t.Branch + " (gh printed no URL)"
	}
	_, expired, err := st.CloseReview(ctx, r.ID, store.ReviewClose{
		Status: model.ReviewApproved, How: model.ApprovePR, DiffStat: stat, Head: head, PRURL: url, Via: via,
	}, now)
	if err != nil {
		return "", reviewRecordError(ctx, st, r, what, head, err)
	}
	return fmt.Sprintf("%s approved: %s.%s", r.ID, what, reviewExpired(expired)), nil
}

// reviewRecordError says that the git work (what, at head) was done but closing review r failed. It never
// advises dropping the task: the intent stays recorded, and approving again records the work. When someone
// else closed the review meanwhile, it says how.
func reviewRecordError(ctx context.Context, st *store.Store, r model.Review, what, head string, err error) error {
	if errors.Is(err, store.ErrReviewClosed) {
		now, found, gerr := st.GetReview(context.WithoutCancel(ctx), r.ID)
		if gerr == nil && found && (now.Status == model.ReviewMerged || now.Status == model.ReviewApproved) && now.Head == head {
			return fmt.Errorf("%s: %s, and another curia recorded it meanwhile (%s)", r.ID, what, task.ReviewStatusWord(now.Status))
		}
		return fmt.Errorf("%s: %s, but the review was closed by someone else meanwhile, so it is not recorded as approved",
			r.ID, what)
	}
	return fmt.Errorf("%s: %s, but recording it failed (%w): the review stays pending; run curia review %s --approve --head %s "+
		"again to record it", r.ID, what, err, r.ID, head)
}

// reviewExpired is the sentence about the assumptions an approval expired.
func reviewExpired(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return " 1 unconfirmed assumption expired."
	}
	return fmt.Sprintf(" %d unconfirmed assumptions expired.", n)
}

// RequestChanges closes pending review reviewID as changes_requested with note (cleaned by
// task.NormalizeNote) and sends the note to the task's agent as a message: the first live linked session
// that is not a teammate, else the review's session. With neither, the note is only saved.
func RequestChanges(ctx context.Context, st *store.Store, tm tmux.Client, logger *slog.Logger, reviewID, note string,
	via model.AnsweredVia, now time.Time,
) (string, error) {
	note, err := task.NormalizeNote(strings.TrimSpace(note)) // a note from stdin ends with a line break
	if err != nil {
		return "", fmt.Errorf("the note must be 1 to %d characters once cleaned", model.MaxReviewNote)
	}
	r, t, err := Pending(ctx, st, reviewID)
	if err != nil {
		return "", err
	}
	if err = reviewEarlierMerge(ctx, r, t, now, "request changes"); err != nil {
		return "", err
	}
	target, err := reviewTarget(ctx, st, r)
	if err != nil {
		return "", err
	}
	if _, _, err = st.CloseReview(ctx, r.ID, store.ReviewClose{
		Status: model.ReviewChanges, Notes: note, Via: via, IntentHold: IntentHold,
	}, now); err != nil {
		return "", reviewBusyNow(ctx, st, r, err, "request changes")
	}
	done := r.ID + ": changes requested; "
	if target == "" {
		return done + "the note is saved but " + t.ID + " has no agent session to send it to", nil
	}
	m, err := st.CreateMessage(ctx, model.Message{
		Text: task.NoteMessage(r.ID, t.ID, note), Kind: model.KindInstruction, Sender: model.SenderUser,
		TargetSession: target, TargetTask: t.ID, TargetRepo: t.Repo, RoutedBy: model.RoutedAddress,
		Status: model.MsgQueued, CreatedAt: now,
	})
	if err == nil {
		err = st.SetReviewNoteMessage(ctx, r.ID, m.ID)
	}
	var out model.MessageOutcome
	if err == nil {
		out, err = deliver.Message(ctx, st, tm, logger, m.ID, now)
	}
	if err != nil {
		return "", fmt.Errorf("%sthe note is saved but could not be sent: %s", done, sanitize.Line(err.Error(), gitops.ErrMax))
	}
	return done + OutcomeLine(out), nil
}

// reviewTarget is the session a change request goes to: the task's first
// live linked session that is not a teammate, else the review's session,
// else "".
func reviewTarget(ctx context.Context, st *store.Store, r model.Review) (string, error) {
	live, err := st.TaskSessions(ctx, r.TaskID)
	if err != nil {
		return "", err
	}
	for _, s := range live {
		if s.TeamRole != model.RoleTeammate {
			return s.ID, nil
		}
	}
	return r.SessionID, nil
}

// Drop closes pending review reviewID as dropped, which stores its task dropped too.
func Drop(ctx context.Context, st *store.Store, reviewID string, via model.AnsweredVia, now time.Time) (string, error) {
	r, t, err := Pending(ctx, st, reviewID)
	if err != nil {
		return "", err
	}
	if err = reviewEarlierMerge(ctx, r, t, now, "drop"); err != nil {
		return "", err
	}
	if _, _, err = st.CloseReview(ctx, r.ID, store.ReviewClose{Status: model.ReviewDropped, Via: via, IntentHold: IntentHold}, now); err != nil {
		return "", reviewBusyNow(ctx, st, r, err, "drop")
	}
	return fmt.Sprintf("%s dropped; %s is dropped. Its agent is not stopped.", r.ID, r.TaskID), nil
}

// Split sorts a task's tickets into the open assume tickets (open at
// now), the open blocking tickets and the decided ones.
func Split(ds []model.Decision, now time.Time) (assumptions, blocking, decided []model.Decision) {
	for _, d := range ds {
		switch {
		case d.Status == model.StatusOpen && d.Mode == model.ModeAssume && (d.ExpiresAt.IsZero() || d.ExpiresAt.After(now)):
			assumptions = append(assumptions, d)
		case d.Status == model.StatusOpen && d.Mode == model.ModeBlocking:
			blocking = append(blocking, d)
		case d.Status == model.StatusAnswered || d.Status == model.StatusConfirmed || d.Status == model.StatusOverridden:
			decided = append(decided, d)
		}
	}
	return assumptions, blocking, decided
}

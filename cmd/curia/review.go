package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia review`; tests reset them.
var (
	reviewApproveFlag, reviewMergeFlag, reviewPRFlag  bool
	reviewChangesFlag, reviewDropFlag, reviewJSONFlag bool
	reviewNoteFlag                                    string
)

// reviewMaxStdin is the most `--note -` reads from stdin.
const reviewMaxStdin = 64 << 10

// reviewWhat is what the user-only guard says only the user may do here.
const reviewWhat = "review work"

// reviewLineMax bounds a printed outcome line, in runes.
const reviewLineMax = 600

// reviewIntentHold is how long the intent an approval records before it runs
// git (store.ClaimReviewIntent) refuses another approval, a change request
// and a drop of the same review: longer than an approval can run (the
// view's six minutes at most), so it outlives only an approval whose process
// was killed.
const reviewIntentHold = 10 * time.Minute

// reviewFlightWait bounds how long a command waits, before it exits, for
// the review actions still in flight: each is bounded by its own limits (the
// view's six minutes at most).
const reviewFlightWait = 7 * time.Minute

// reviewFlight counts the review actions (approve, request changes, drop)
// in flight in this process, so that curia review and curia ui wait for
// them to finish and be recorded before they exit, whatever ended the view.
var reviewFlight = &reviewInFlight{}

// reviewInFlight is a count of actions in flight that can be waited on.
type reviewInFlight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n drops to 0; nil while nobody waits
}

// begin counts one more action in flight; the func it returns ends it.
func (f *reviewInFlight) begin() (end func()) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.n--; f.n == 0 && f.idle != nil {
			close(f.idle)
			f.idle = nil
		}
	}
}

// wait waits until no action is in flight, at most limit, and reports
// whether none is.
func (f *reviewInFlight) wait(limit time.Duration) bool {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return true
	}
	if f.idle == nil {
		f.idle = make(chan struct{})
	}
	idle := f.idle
	f.mu.Unlock()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

// reviewWaitFlight waits for the review actions in flight (reviewFlight)
// once a view has ended, whatever ended it.
func reviewWaitFlight() error {
	if !reviewFlight.wait(reviewFlightWait) {
		return fmt.Errorf("a review action was still running after %s; check it with curia reviews ls", reviewFlightWait)
	}
	return nil
}

// reviewHoldSignals makes a hang-up and SIGTERM (and, with interrupt, SIGINT)
// no longer end the process: an approval in flight runs to its end and is
// recorded first (C1). The context is canceled when one arrives, so a view
// can quit; stop restores the default behavior.
func reviewHoldSignals(interrupt bool) (ctx context.Context, stop context.CancelFunc) {
	sigs := []os.Signal{syscall.SIGHUP, syscall.SIGTERM}
	if interrupt {
		sigs = append(sigs, os.Interrupt)
	}
	return signal.NotifyContext(context.Background(), sigs...)
}

// reviewProgramErr is err from a view's program, except that a program
// ended by a held signal (reviewHoldSignals) is no error.
func reviewProgramErr(sig context.Context, err error) error {
	if sig.Err() != nil && errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

// reviewIsTerminal reports whether stdin and stdout are terminals. Tests
// replace it.
var reviewIsTerminal = func() bool { return taskIsTerminal(os.Stdin) && taskIsTerminal(os.Stdout) }

func init() {
	cmd := &cobra.Command{
		Use:   "review [<T-id>|<R-id>]",
		Short: "Approve, send back or drop finished work (the user only)",
		Long: "With no action flag, opens the review view (all waiting reviews, or the one\n" +
			"named): a approves, c requests changes, x drops, o opens the diff tool.\n" +
			"--approve merges the task's branch into its base in your checkout (git merge\n" +
			"--no-ff, which must have the base checked out), or with --pr pushes it and\n" +
			"opens a pull request with gh; the default is review.approve. --request-changes\n" +
			"--note <text> (or - for stdin) sends the note to the task's agent as a\n" +
			"message. --drop drops the task; its agent is not stopped. --json prints the\n" +
			"review's detail and acts on nothing. Refuses to run inside an agent session.",
		Annotations: userOnlyMark(),
		Args:        cobra.MaximumNArgs(1),
		RunE:        runReview,
	}
	f := cmd.Flags()
	f.BoolVar(&reviewApproveFlag, "approve", false, "approve the review: merge, or open a pull request")
	f.BoolVar(&reviewMergeFlag, "merge", false, "with --approve: merge into the base branch in your checkout")
	f.BoolVar(&reviewPRFlag, "pr", false, "with --approve: push the branch and open a pull request")
	f.BoolVar(&reviewChangesFlag, "request-changes", false, "send the task back to its agent with --note")
	f.StringVar(&reviewNoteFlag, "note", "", "the change request (- reads it from stdin)")
	f.BoolVar(&reviewDropFlag, "drop", false, "drop the task (its agent is not stopped)")
	f.BoolVar(&reviewJSONFlag, "json", false, "print the review's detail as one JSON object")
	rootCmd.AddCommand(cmd)
}

// reviewAction is what the flags ask for.
type reviewAction int

const (
	reviewView reviewAction = iota
	reviewPrint
	reviewDoApprove
	reviewDoChanges
	reviewDoDrop
)

func runReview(cmd *cobra.Command, args []string) error {
	// Only the user approves, sends back or drops work (spec section 7).
	if err := userOnly("review", reviewWhat); err != nil {
		return err
	}
	act, err := reviewFlags(args)
	if err != nil {
		return err
	}
	arg := ""
	if len(args) == 1 {
		arg = args[0]
	}
	if act == reviewView {
		return reviewRunView(cmd, arg)
	}
	how := model.ApproveHow("")
	switch {
	case reviewMergeFlag:
		how = model.ApproveMerge
	case reviewPRFlag:
		how = model.ApprovePR
	}
	note := reviewNoteFlag
	if act == reviewDoChanges && note == "-" {
		if note, err = reviewReadNote(cmd.InOrStdin()); err != nil {
			return err
		}
	}
	return reviewRunAction(cmd, act, arg, how, note)
}

// reviewFlags checks the flag combination and the id, and says what to do.
func reviewFlags(args []string) (reviewAction, error) {
	act, err := reviewFlagAction()
	if err != nil {
		return 0, err
	}
	if len(args) == 0 {
		if act != reviewView {
			return 0, errors.New("--approve, --request-changes, --drop and --json need a task or review id")
		}
		return act, nil
	}
	if !task.ValidID(args[0]) && !task.ValidReviewID(args[0]) {
		return 0, fmt.Errorf("%q is not a task id (T-<n>) or a review id (R-<n>)", sanitize.Line(args[0], 40))
	}
	return act, nil
}

// reviewActionFlags is the action flag set, and how many are set.
func reviewActionFlags() (act reviewAction, n int) {
	for _, f := range []struct {
		set bool
		act reviewAction
	}{{reviewApproveFlag, reviewDoApprove}, {reviewChangesFlag, reviewDoChanges}, {reviewDropFlag, reviewDoDrop}} {
		if f.set {
			act, n = f.act, n+1
		}
	}
	return act, n
}

// reviewFlagAction checks the flag combination and says what it asks for.
func reviewFlagAction() (reviewAction, error) {
	act, n := reviewActionFlags()
	if n > 1 {
		return 0, errors.New("give at most one of --approve, --request-changes and --drop")
	}
	if reviewJSONFlag && n > 0 {
		return 0, errors.New("--json acts on nothing: give it without --approve, --request-changes or --drop")
	}
	switch {
	case (reviewMergeFlag || reviewPRFlag) && !reviewApproveFlag:
		return 0, errors.New("--merge and --pr go with --approve")
	case reviewMergeFlag && reviewPRFlag:
		return 0, errors.New("give --merge or --pr, not both")
	case reviewNoteFlag != "" && !reviewChangesFlag:
		return 0, errors.New("--note goes with --request-changes")
	case reviewChangesFlag && reviewNoteFlag == "":
		return 0, errors.New("--request-changes needs --note <text> (or --note - to read it from stdin)")
	case reviewJSONFlag:
		return reviewPrint, nil
	}
	return act, nil
}

// reviewReadNote reads `--note -` from stdin, at most reviewMaxStdin bytes.
func reviewReadNote(stdin io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(stdin, reviewMaxStdin+1))
	if err != nil {
		return "", fmt.Errorf("read the note from stdin: %w", err)
	}
	if len(b) > reviewMaxStdin {
		return "", errors.New("the note on stdin is over 64 KiB; a note holds at most 600 characters")
	}
	return string(b), nil
}

// reviewOpen loads the config, the store and the log for one run.
func reviewOpen() (cfg config.Config, st *store.Store, logger *slog.Logger, done func(), err error) {
	if cfg, err = loadConfig(); err != nil {
		return cfg, nil, nil, nil, err
	}
	if st, err = openStore(cfg); err != nil {
		return cfg, nil, nil, nil, err
	}
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		_ = st.Close()
		return cfg, nil, nil, nil, err
	}
	return cfg, st, logger, func() { _ = closeLog(); _ = st.Close() }, nil
}

// reviewRunAction runs the flag form of an action, or --json, on the
// pending review arg names, and prints its outcome line.
func reviewRunAction(cmd *cobra.Command, act reviewAction, arg string, how model.ApproveHow, note string) error {
	cfg, st, logger, done, err := reviewOpen()
	if err != nil {
		return err
	}
	defer done()
	ctx, tm, out := cmd.Context(), newTmuxClient(), cmd.OutOrStdout()
	r, err := reviewResolve(ctx, st, arg)
	if err != nil {
		return err
	}
	if act == reviewPrint {
		return reviewPrintJSON(ctx, out, st, cfg, r)
	}
	// A hang-up, SIGTERM or Ctrl-C waits for the action to be done and recorded.
	_, stop := reviewHoldSignals(true)
	defer stop()
	var line string
	switch act {
	case reviewDoApprove:
		if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
			return err
		}
		line, err = reviewApprove(ctx, st, cfg, r.ID, how, "", model.AnsweredCLI, clock())
	case reviewDoChanges:
		line, err = reviewRequestChanges(ctx, st, tm, logger, r.ID, note, model.AnsweredCLI, clock())
	default:
		line, err = reviewDrop(ctx, st, r.ID, model.AnsweredCLI, clock())
	}
	if err != nil {
		return err
	}
	reviewMarkers(ctx, st, tm, cfg.LogPath, r.TaskID)
	_, err = fmt.Fprintln(out, sanitize.Line(line, reviewLineMax))
	return err
}

// reviewRunView runs the review view (all pending reviews, or those of the
// task arg names) in the alternate screen, then prints the outcome of each
// action taken in it.
func reviewRunView(cmd *cobra.Command, arg string) error {
	if !reviewIsTerminal() {
		return errors.New("curia review needs a terminal: list reviews with curia reviews ls, or act on one with " +
			"curia review <id> --approve, --request-changes --note <text> or --drop")
	}
	cfg, st, logger, done, err := reviewOpen()
	if err != nil {
		return err
	}
	defer done()
	opts := ui.ReviewOptions{TaskID: arg}
	if task.ValidReviewID(arg) {
		r, rerr := reviewResolve(cmd.Context(), st, arg)
		if rerr != nil {
			return rerr
		}
		opts.TaskID = r.TaskID
	}
	src := newReviewSource(st, newTmuxClient(), cfg, logger, decideVia(), nil)
	last := ui.NewReview(src, clock, opts)
	sig, stop := reviewHoldSignals(false)
	defer stop()
	err = reviewProgramErr(sig, runProgram(reviewCapture{m: last, last: &last}, tea.WithAltScreen(), tea.WithContext(sig)))
	if werr := reviewWaitFlight(); err == nil {
		err = werr
	}
	if err != nil {
		return err
	}
	for _, line := range last.Result() {
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), sanitize.Line(line, reviewLineMax)); err != nil {
			return err
		}
	}
	return nil
}

// reviewCapture runs the review view and keeps its latest state in last,
// so its Result can be read once the program has quit.
type reviewCapture struct {
	m    ui.ReviewModel
	last *ui.ReviewModel
}

func (c reviewCapture) Init() tea.Cmd { return c.m.Init() }

func (c reviewCapture) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := c.m.Update(msg)
	if m, ok := next.(ui.ReviewModel); ok {
		c.m, *c.last = m, m
	}
	return c, cmd
}

func (c reviewCapture) View() string { return c.m.View() }

// reviewStatusWord is a review status as a sentence says it.
func reviewStatusWord(s model.ReviewStatus) string {
	return strings.ReplaceAll(sanitize.Line(string(s), 30), "_", " ")
}

// reviewResolve turns a task id or a review id into the pending review it names.
func reviewResolve(ctx context.Context, st *store.Store, arg string) (model.Review, error) {
	switch {
	case task.ValidReviewID(arg):
		r, _, err := reviewPending(ctx, st, arg)
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

// reviewPending reads review id, which must be pending, and its task. The
// ids read back from the store are checked again.
func reviewPending(ctx context.Context, st *store.Store, id string) (model.Review, model.Task, error) {
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
		return model.Review{}, model.Task{}, fmt.Errorf("%s is %s", id, reviewStatusWord(r.Status))
	case !task.ValidID(r.TaskID):
		return model.Review{}, model.Task{}, fmt.Errorf("%s names task %q, which is not a task id", id, sanitize.Line(r.TaskID, 40))
	}
	t, found, err := st.GetTask(ctx, r.TaskID)
	if err == nil && !found {
		err = fmt.Errorf("%s's task %s does not exist", id, r.TaskID)
	}
	return r, t, err
}

// reviewApprove, reviewRequestChanges and reviewDrop are the three actions of section 4.6; each returns its outcome line.
//
// reviewApprove approves pending review reviewID as how says ("" takes review.approve): a merge, or a pull
// request. seen is the branch head the user was shown (the view's), or "" for the flag form, where the head
// taken by the checks is the one approved. Before git runs, the approval's intent (how, the head, the stat) is
// recorded on the review, which is also its claim: a second approval is refused while it stands. An intent
// to merge whose head is already in the base, left by an approval that was killed after git merged, is
// recorded as merged. A refusal or failure leaves the review pending.
func reviewApprove(ctx context.Context, st *store.Store, cfg config.Config, reviewID string, how model.ApproveHow,
	seen string, via model.AnsweredVia, now time.Time,
) (string, error) {
	if err := userOnly("review", reviewWhat); err != nil {
		return "", err
	}
	r, t, err := reviewPending(ctx, st, reviewID)
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
	stat, head, err := reviewCheck(ctx, st, t)
	if err != nil {
		return "", err
	}
	if seen != "" && seen != head {
		return "", errReviewBranchChanged
	}
	if r, err = st.ClaimReviewIntent(ctx, r.ID, store.ReviewIntent{How: how, Head: head, DiffStat: stat}, now, reviewIntentHold); err != nil {
		return "", reviewBusyError(r, err)
	}
	if how == model.ApproveMerge {
		return reviewApproveMerge(ctx, st, r, t, stat, head, via, now)
	}
	return reviewApprovePR(ctx, st, cfg, r, t, stat, head, via, now)
}

// reviewBusyError says that review r is being approved elsewhere when err is
// store.ErrReviewBusy, else is err.
func reviewBusyError(r model.Review, err error) error {
	if !errors.Is(err, store.ErrReviewBusy) {
		return err
	}
	since := r.IntentAt.Local()
	return fmt.Errorf("%s is being approved (%s at %s) by another curia since %s: wait for it to finish. "+
		"If that curia was killed, approve again after %s", r.ID, sanitize.Line(string(r.ApprovedHow), 10),
		sanitize.Line(reviewShort(r.Head), 10), since.Format("15:04:05"), since.Add(reviewIntentHold).Format("15:04:05"))
}

// reviewBusyNow is reviewBusyError with review r read again, so that the
// refusal names the approval in flight as it is stored now.
func reviewBusyNow(ctx context.Context, st *store.Store, r model.Review, err error) error {
	if errors.Is(err, store.ErrReviewBusy) {
		if cur, found, gerr := st.GetReview(ctx, r.ID); gerr == nil && found {
			r = cur
		}
	}
	return reviewBusyError(r, err)
}

// reviewFoundDone records review r merged, and reports done, when an approval recorded the intent to merge
// r's head, the branch is still at that head (and it is seen, when seen is given), and the base already
// contains it: an approval whose process was killed after git merged. Otherwise it does nothing.
func reviewFoundDone(ctx context.Context, st *store.Store, r model.Review, t model.Task, seen string,
	via model.AnsweredVia, now time.Time,
) (line string, done bool, err error) {
	if r.IntentAt.IsZero() || r.ApprovedHow != model.ApproveMerge || !reviewHeadPattern.MatchString(r.Head) ||
		(seen != "" && seen != r.Head) || !reviewInBase(ctx, t, r.Head) {
		return "", false, nil
	}
	what := fmt.Sprintf("%s (%s) is already merged into %s; recorded", t.Branch, reviewShort(r.Head), t.BaseBranch)
	_, expired, err := st.CloseReview(ctx, r.ID, store.ReviewClose{
		Status: model.ReviewMerged, How: model.ApproveMerge, DiffStat: r.DiffStat, Head: r.Head, Via: via,
	}, now)
	if err != nil {
		return "", true, reviewRecordError(ctx, st, r, what, r.Head, err)
	}
	return fmt.Sprintf("%s approved: %s.%s Remove the worktree and branch with: curia clean %s",
		r.ID, what, reviewExpired(expired), t.ID), true, nil
}

// reviewInBase reports whether t's branch is at head and t's base contains
// it. Anything it cannot read is false.
func reviewInBase(ctx context.Context, t model.Task, head string) bool {
	if reviewCheckNames(t) != nil {
		return false
	}
	cur, err := reviewHead(ctx, t)
	if err != nil || cur != head {
		return false
	}
	n, err := reviewAhead(ctx, t, head)
	return err == nil && n == 0
}

// reviewApproveMerge merges and records the review merged. When the merge fails its intent is cleared, unless
// the base holds the head after all, so that approving again records it.
func reviewApproveMerge(ctx context.Context, st *store.Store, r model.Review, t model.Task, stat, head string,
	via model.AnsweredVia, now time.Time,
) (string, error) {
	if err := reviewMerge(ctx, t, head); err != nil {
		return "", reviewMergeRelease(ctx, st, r, t, head, err)
	}
	what := fmt.Sprintf("merged %s into %s (%s)", t.Branch, t.BaseBranch, reviewShort(head))
	_, expired, err := st.CloseReview(ctx, r.ID, store.ReviewClose{
		Status: model.ReviewMerged, How: model.ApproveMerge, DiffStat: stat, Head: head, Via: via,
	}, now)
	if err != nil {
		return "", reviewRecordError(ctx, st, r, what, head, err)
	}
	return fmt.Sprintf("%s approved: %s.%s Remove the worktree and branch with: curia clean %s",
		r.ID, what, reviewExpired(expired), t.ID), nil
}

// reviewMergeRelease follows a failed merge of head: it keeps r's intent when the base contains head after
// all (and says to approve again to record it), else clears it, and returns the merge's error.
func reviewMergeRelease(ctx context.Context, st *store.Store, r model.Review, t model.Task, head string, failed error) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeGitLimit)
	defer cancel()
	if n, err := reviewAhead(actx, t, head); err == nil && n == 0 {
		return fmt.Errorf("%w. %s holds %s all the same: record it with curia review %s --approve --head %s",
			failed, t.BaseBranch, reviewShort(head), r.ID, head)
	}
	if err := st.ReleaseReviewIntent(actx, r.ID, r.IntentAt); err != nil {
		return fmt.Errorf("%w (and clearing the approval's claim failed: %s)", failed, sanitize.Line(err.Error(), reviewGitErrMax))
	}
	return failed
}

// reviewApprovePR opens the pull request and records the review approved; on a failure its intent is cleared.
func reviewApprovePR(ctx context.Context, st *store.Store, cfg config.Config, r model.Review, t model.Task, stat, head string,
	via model.AnsweredVia, now time.Time,
) (string, error) {
	ds, err := st.TaskDecisions(ctx, t.ID)
	var url string
	if err == nil {
		_, _, decided := reviewSplit(ds, now)
		url, err = reviewOpenPR(ctx, t, r, decided, cfg.Review.Remote, head)
	}
	if err != nil {
		if rerr := st.ReleaseReviewIntent(context.WithoutCancel(ctx), r.ID, r.IntentAt); rerr != nil {
			err = fmt.Errorf("%w (and clearing the approval's claim failed: %s)", err, sanitize.Line(rerr.Error(), reviewGitErrMax))
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
			return fmt.Errorf("%s: %s, and another curia recorded it meanwhile (%s)", r.ID, what, reviewStatusWord(now.Status))
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

// reviewRequestChanges closes pending review reviewID as changes_requested with note (cleaned by
// task.NormalizeNote) and sends the note to the task's agent as a message: the first live linked session
// that is not a teammate, else the review's session. With neither, the note is only saved.
func reviewRequestChanges(ctx context.Context, st *store.Store, tm tmux.Client, logger *slog.Logger, reviewID, note string,
	via model.AnsweredVia, now time.Time,
) (string, error) {
	if err := userOnly("review", reviewWhat); err != nil {
		return "", err
	}
	note, err := task.NormalizeNote(strings.TrimSpace(note)) // a note from stdin ends with a line break
	if err != nil {
		return "", fmt.Errorf("the note must be 1 to %d characters once cleaned", model.MaxReviewNote)
	}
	r, t, err := reviewPending(ctx, st, reviewID)
	if err != nil {
		return "", err
	}
	target, err := reviewTarget(ctx, st, r)
	if err != nil {
		return "", err
	}
	if _, _, err = st.CloseReview(ctx, r.ID, store.ReviewClose{
		Status: model.ReviewChanges, Notes: note, Via: via, IntentHold: reviewIntentHold,
	}, now); err != nil {
		return "", reviewBusyNow(ctx, st, r, err)
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
		return "", fmt.Errorf("%sthe note is saved but could not be sent: %s", done, sanitize.Line(err.Error(), reviewGitErrMax))
	}
	return done + sayOutcomeLine(out), nil
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

// reviewDrop closes pending review reviewID as dropped, which stores its task dropped too.
func reviewDrop(ctx context.Context, st *store.Store, reviewID string, via model.AnsweredVia, now time.Time) (string, error) {
	if err := userOnly("review", reviewWhat); err != nil {
		return "", err
	}
	r, _, err := reviewPending(ctx, st, reviewID)
	if err != nil {
		return "", err
	}
	if _, _, err = st.CloseReview(ctx, r.ID, store.ReviewClose{Status: model.ReviewDropped, Via: via, IntentHold: reviewIntentHold}, now); err != nil {
		return "", reviewBusyNow(ctx, st, r, err)
	}
	return fmt.Sprintf("%s dropped; %s is dropped. Its agent is not stopped.", r.ID, r.TaskID), nil
}

// reviewMarkers re-applies the window markers of the task's live linked
// sessions, so a review's dot goes when it is closed. A failure is logged.
func reviewMarkers(ctx context.Context, st *store.Store, tm tmux.Client, logPath, taskID string) {
	live, err := st.TaskSessions(ctx, taskID)
	if err != nil {
		logWarn(logPath, "review attention marker", "task", taskID, "err", err.Error())
		return
	}
	var windows []string
	for _, s := range live {
		if s.TmuxWindow != "" && !slices.Contains(windows, s.TmuxWindow) {
			windows = append(windows, s.TmuxWindow)
		}
	}
	applyMarkers(ctx, tm, st, logPath, "review attention marker", windows)
}

// reviewSplit sorts a task's tickets into the open assume tickets (open at
// now), the open blocking tickets and the decided ones.
func reviewSplit(ds []model.Decision, now time.Time) (assumptions, blocking, decided []model.Decision) {
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

// reviewDetail builds section 4.5's detail of review r of task t.
func reviewDetail(ctx context.Context, st *store.Store, cfg config.Config, r model.Review, t model.Task) (ui.ReviewDetail, error) {
	ds, err := st.TaskDecisions(ctx, t.ID)
	if err != nil {
		return ui.ReviewDetail{}, err
	}
	run, err := st.TaskRun(ctx, t.ID)
	if err != nil {
		return ui.ReviewDetail{}, err
	}
	d := ui.ReviewDetail{
		Review: r, Task: t, Sessions: run.Sessions, Teammates: run.Teammates,
		CostUSD: run.CostUSD, CostKnown: run.CostKnown, Approve: model.ApproveHow(cfg.Review.Approve),
	}
	d.Assumptions, d.Blocking, d.Decided = reviewSplit(ds, clock())
	if !run.Started.IsZero() && r.CreatedAt.After(run.Started) {
		d.Duration = r.CreatedAt.Sub(run.Started)
	}
	d.DiffStat, d.DiffNote, d.Head = reviewDiffStat(ctx, t, reviewStatWidth)
	return d, nil
}

// reviewSource is the review view's ui.ReviewSource over the store and git.
type reviewSource struct {
	st  *store.Store
	tm  tmux.Client
	cfg config.Config
	log *slog.Logger
	via model.AnsweredVia
	// refused is the user-only guard's refusal of the caller, or nil: with
	// it the view is read-only, and approve, request changes and drop do
	// nothing and say why (the dashboard's caller, as the board's).
	refused error
}

// newReviewSource returns the ui.ReviewSource of curia review. Task 20 reuses it for the dashboard,
// with refused the guard's answer for the dashboard's caller (nil for curia review, which has passed it).
func newReviewSource(st *store.Store, tm tmux.Client, cfg config.Config, logger *slog.Logger, via model.AnsweredVia,
	refused error,
) ui.ReviewSource {
	return reviewSource{st: st, tm: tm, cfg: cfg, log: logger, via: via, refused: refused}
}

// Queue is the pending reviews of tasks not stored dropped, oldest first.
// It runs no git.
func (s reviewSource) Queue(ctx context.Context) ([]ui.ReviewRow, error) {
	rs, err := s.st.ListReviews(ctx, store.ReviewFilter{Statuses: []model.ReviewStatus{model.ReviewPending}})
	if err != nil {
		return nil, err
	}
	rows := make([]ui.ReviewRow, 0, len(rs))
	for _, r := range rs {
		t, found, err := s.st.GetTask(ctx, r.TaskID)
		if err != nil {
			return nil, err
		}
		if found && t.Status != model.TaskDropped {
			rows = append(rows, ui.ReviewRow{Review: r, Task: t})
		}
	}
	return rows, nil
}

// Detail reads review id and builds its detail.
func (s reviewSource) Detail(ctx context.Context, id string) (ui.ReviewDetail, error) {
	r, found, err := s.st.GetReview(ctx, id)
	if err == nil && !found {
		err = fmt.Errorf("no review %s", id)
	}
	if err != nil {
		return ui.ReviewDetail{}, err
	}
	t, found, err := s.st.GetTask(ctx, r.TaskID)
	if err == nil && !found {
		err = fmt.Errorf("%s's task %s does not exist", id, sanitize.Line(r.TaskID, 40))
	}
	if err != nil {
		return ui.ReviewDetail{}, err
	}
	return reviewDetail(ctx, s.st, s.cfg, r, t)
}

// Approve approves review id at head, the branch head that was shown, as
// review.approve says.
func (s reviewSource) Approve(ctx context.Context, id, head string) (string, error) {
	if s.refused != nil {
		return "", s.refused
	}
	defer reviewFlight.begin()()
	if err := reconcile(ctx, s.st, s.tm, s.cfg.LogPath, clock()); err != nil {
		return "", err
	}
	if head == "" {
		return "", errors.New("the review shows no branch head to approve")
	}
	return s.after(ctx, id)(reviewApprove(ctx, s.st, s.cfg, id, "", head, s.via, clock()))
}

// RequestChanges sends review id back with note.
func (s reviewSource) RequestChanges(ctx context.Context, id, note string) (string, error) {
	if s.refused != nil {
		return "", s.refused
	}
	defer reviewFlight.begin()()
	return s.after(ctx, id)(reviewRequestChanges(ctx, s.st, s.tm, s.log, id, note, s.via, clock()))
}

// Drop drops review id and its task.
func (s reviewSource) Drop(ctx context.Context, id string) (string, error) {
	if s.refused != nil {
		return "", s.refused
	}
	defer reviewFlight.begin()()
	return s.after(ctx, id)(reviewDrop(ctx, s.st, id, s.via, clock()))
}

// after re-applies the markers of review id's task once an action has
// closed it, and passes the action's outcome on.
func (s reviewSource) after(ctx context.Context, id string) func(string, error) (string, error) {
	return func(line string, err error) (string, error) {
		if err == nil {
			if r, found, gerr := s.st.GetReview(ctx, id); gerr == nil && found {
				reviewMarkers(ctx, s.st, s.tm, s.cfg.LogPath, r.TaskID)
			}
		}
		return line, err
	}
}

// DiffCmd is the configured diff tool on review id's task.
func (s reviewSource) DiffCmd(ctx context.Context, id string) (*exec.Cmd, error) {
	r, found, err := s.st.GetReview(ctx, id)
	if err == nil && !found {
		err = fmt.Errorf("no review %s", id)
	}
	if err != nil {
		return nil, err
	}
	t, found, err := s.st.GetTask(ctx, r.TaskID)
	if err == nil && !found {
		err = fmt.Errorf("%s's task does not exist", id)
	}
	if err != nil {
		return nil, err
	}
	return reviewDiffCmd(t, s.cfg.Review.DiffTool)
}

// reviewJSONTask is the task as `curia review --json` prints it.
type reviewJSONTask struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Repo       string `json:"repo"`
	BaseBranch string `json:"base_branch"`
	Branch     string `json:"branch"`
	Worktree   string `json:"worktree"`
}

// reviewJSON is the detail as `curia review --json` prints it: the review's
// columns (diff_stat and head as they are now, for a pending review records
// neither), then the detail.
type reviewJSON struct {
	N               int64          `json:"n"`
	ID              string         `json:"id"`
	TaskID          string         `json:"task_id"`
	SessionID       string         `json:"session_id"`
	Summary         string         `json:"summary"`
	TestsStatus     string         `json:"tests_status"`
	TestsDetail     string         `json:"tests_detail"`
	Status          string         `json:"status"`
	ApprovedHow     string         `json:"approved_how"`
	DiffStat        string         `json:"diff_stat"`
	Head            string         `json:"head"`
	PRURL           string         `json:"pr_url"`
	MyNotes         string         `json:"my_notes"`
	NoteMessage     string         `json:"note_message"`
	ClosedVia       string         `json:"closed_via"`
	CreatedAt       *time.Time     `json:"created_at"`
	UpdatedAt       *time.Time     `json:"updated_at"`
	ClosedAt        *time.Time     `json:"closed_at"`
	Task            reviewJSONTask `json:"task"`
	DiffNote        string         `json:"diff_note"`
	OpenAssumptions []string       `json:"open_assumptions"`
	OpenBlocking    []string       `json:"open_blocking"`
	Decided         []string       `json:"decided"`
	Sessions        int            `json:"sessions"`
	Teammates       int            `json:"teammates"`
	DurationSeconds int64          `json:"duration_seconds"`
	CostUSD         *float64       `json:"cost_usd"`
}

// reviewIDs is the ids of ds, never nil, cleaned.
func reviewIDs(ds []model.Decision) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, sanitize.Line(d.ID, 20))
	}
	return out
}

// reviewPrintJSON prints review r's detail as one JSON object. It acts on
// nothing.
func reviewPrintJSON(ctx context.Context, w io.Writer, st *store.Store, cfg config.Config, r model.Review) error {
	_, t, err := reviewPending(ctx, st, r.ID)
	if err != nil {
		return err
	}
	d, err := reviewDetail(ctx, st, cfg, r, t)
	if err != nil {
		return err
	}
	l := sanitize.Line
	j := reviewJSON{
		N: r.N, ID: l(r.ID, 20), TaskID: l(r.TaskID, 20), SessionID: l(r.SessionID, 100),
		Summary: sanitize.Text(r.Summary, task.MaxSummary), TestsStatus: l(string(r.Tests), 20),
		TestsDetail: l(r.TestsDetail, task.MaxTestsDetail), Status: l(string(r.Status), 30),
		ApprovedHow: l(string(r.ApprovedHow), 20), DiffStat: d.DiffStat, Head: l(d.Head, 64),
		PRURL: l(r.PRURL, 300), MyNotes: sanitize.Text(r.Notes, model.MaxReviewNote), NoteMessage: l(r.NoteMessage, 20),
		ClosedVia: l(string(r.ClosedVia), 20),
		CreatedAt: jsonTime(r.CreatedAt), UpdatedAt: jsonTime(r.UpdatedAt), ClosedAt: jsonTime(r.ClosedAt),
		Task: reviewJSONTask{
			ID: l(t.ID, 20), Title: l(t.Title, task.MaxTitle), Repo: l(t.Repo, 300),
			BaseBranch: l(t.BaseBranch, 200), Branch: l(t.Branch, 200), Worktree: l(t.Worktree, 300),
		},
		DiffNote: d.DiffNote, OpenAssumptions: reviewIDs(d.Assumptions), OpenBlocking: reviewIDs(d.Blocking),
		Decided: reviewIDs(d.Decided), Sessions: d.Sessions, Teammates: d.Teammates,
		DurationSeconds: int64(d.Duration / time.Second),
	}
	if d.CostKnown {
		j.CostUSD = &d.CostUSD
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(j); err != nil {
		return err
	}
	_, err = io.WriteString(w, escapeUnprintable(buf.String()))
	return err
}

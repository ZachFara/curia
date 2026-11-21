package main

import (
	"context"
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
	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/review"
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
	reviewNoteFlag, reviewHeadFlag                    string
)

// reviewMaxStdin is the most `--note -` reads from stdin.
const reviewMaxStdin = 64 << 10

// reviewWhat is what the user-only guard says only the user may do here.
const reviewWhat = "review work"

// reviewLineMax bounds a printed outcome line, in runes.
const reviewLineMax = 600

// reviewFlightWait bounds how long a command waits, before it exits, for
// the review actions still in flight: each is bounded by its own limits (the
// view's six minutes at most).
const reviewFlightWait = 7 * time.Minute

// reviewFlight counts the review actions (approve, request changes, drop)
// and the dashboard's pauses in flight in this process, so that curia review and curia ui wait for
// them to finish and be recorded before they exit, whatever ended the view.
var reviewFlight = &reviewInFlight{}

// reviewInFlight is a count of actions in flight that can be waited on.
type reviewInFlight struct {
	mu   sync.Mutex
	n    int
	p    int           // of n, the pauses
	idle chan struct{} // closed when n drops to 0; nil while nobody waits
}

// begin counts one more action in flight; the func it returns ends it.
func (f *reviewInFlight) begin() (end func()) { return f.count(0) }

// beginPause counts one more pause in flight; the func it returns ends it.
func (f *reviewInFlight) beginPause() (end func()) { return f.count(1) }

func (f *reviewInFlight) count(pause int) (end func()) {
	f.mu.Lock()
	f.n++
	f.p += pause
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.p -= pause
		if f.n--; f.n == 0 && f.idle != nil {
			close(f.idle)
			f.idle = nil
		}
	}
}

// busy reports whether an action is in flight.
func (f *reviewInFlight) busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n > 0
}

// words says what is in flight and which command shows its outcome.
func (f *reviewInFlight) words() (what, follow string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.p {
	case 0:
		return "a review action", "curia reviews ls"
	case f.n:
		return "a pause", "curia task ls"
	}
	return "a pause and a review action", "curia task ls and curia reviews ls"
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
	what, follow := reviewFlight.words() // before the wait: the action ends it
	if !reviewFlight.wait(reviewFlightWait) {
		return fmt.Errorf("%s was still running after %s; check it with %s", what, reviewFlightWait, follow)
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

// reviewWaitingLine is what curia prints when a held signal came while an
// action was in flight (M-2).
func reviewWaitingLine() string {
	what, _ := reviewFlight.words()
	return "curia: " + what + " is still running; waiting for it to finish and be recorded (its own limits bound it)"
}

// reviewSayWaiting prints reviewWaitingLine on w once, when sig ends before the returned func is called (the
// flag form's action ends).
func reviewSayWaiting(sig context.Context, w io.Writer) (done func()) {
	end := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-sig.Done():
			_, _ = fmt.Fprintln(w, reviewWaitingLine())
		case <-end:
		}
	}()
	return func() { close(end); <-finished }
}

// reviewAfterProgram waits for the review actions in flight once a view's program has returned, saying so on
// w when a held signal ended it while one runs.
func reviewAfterProgram(sig context.Context, w io.Writer) error {
	if sig.Err() != nil && reviewFlight.busy() {
		_, _ = fmt.Fprintln(w, reviewWaitingLine())
	}
	return reviewWaitFlight()
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
	review.OutcomeLine = sayOutcomeLine
	cmd := &cobra.Command{
		Use:   "review [<T-id>|<R-id>]",
		Short: "Approve, send back or drop finished work (the user only)",
		Long: "With no action flag, opens the review view (all waiting reviews, or the one\n" +
			"named): a approves, c requests changes, x drops, o opens the diff tool.\n" +
			"--approve merges the task's branch into its base in your checkout (git merge\n" +
			"--no-ff, which must have the base checked out), or with --pr pushes it and\n" +
			"opens a pull request with gh; the default is review.approve. It needs --head\n" +
			"<commit>, the full id of the branch head you looked at (curia review <id>\n" +
			"--json prints it; a prefix is refused), and refuses when the branch is no\n" +
			"longer there. Without --head it prints the head and the command to run.\n" +
			"--request-changes --note <text> (or - for stdin) sends the note to the task's\n" +
			"agent as a message. --drop drops the task; its agent is not stopped. --json\n" +
			"prints the review's detail and acts on nothing. Refuses to run inside an\n" +
			"agent session.",
		Annotations: userOnlyMark(),
		Args:        cobra.MaximumNArgs(1),
		RunE:        runReview,
	}
	f := cmd.Flags()
	f.BoolVar(&reviewApproveFlag, "approve", false, "approve the review: merge, or open a pull request")
	f.BoolVar(&reviewMergeFlag, "merge", false, "with --approve: merge into the base branch in your checkout")
	f.BoolVar(&reviewPRFlag, "pr", false, "with --approve: push the branch and open a pull request")
	f.StringVar(&reviewHeadFlag, "head", "", "with --approve: the full commit id of the branch head you looked at")
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
	case (reviewMergeFlag || reviewPRFlag || reviewHeadFlag != "") && !reviewApproveFlag:
		return 0, errors.New("--merge, --pr and --head go with --approve")
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
	r, err := review.Resolve(ctx, st, arg)
	if err != nil {
		return err
	}
	if act == reviewPrint {
		return reviewPrintJSON(ctx, out, st, cfg, r)
	}
	// A hang-up, SIGTERM or Ctrl-C waits for the action to be done and recorded.
	sig, stop := reviewHoldSignals(true)
	defer stop()
	defer reviewSayWaiting(sig, cmd.ErrOrStderr())()
	var line string
	switch act {
	case reviewDoApprove:
		var seen string
		if seen, err = reviewFlagHead(ctx, st, r, how); err != nil {
			return err
		}
		if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
			return err
		}
		line, err = reviewApprove(ctx, review.Approval{Store: st, Config: cfg, ReviewID: r.ID, How: how, Seen: seen, Via: model.AnsweredCLI, Now: clock()})
	case reviewDoChanges:
		line, err = reviewRequestChanges(ctx, st, tm, logger, r.ID, note, model.AnsweredCLI, clock())
	default:
		line, err = reviewDrop(ctx, st, r.ID, model.AnsweredCLI, clock())
	}
	reviewLogOutcome(logger, r.ID, line, err)
	if err != nil {
		return err
	}
	reviewMarkers(ctx, st, tm, cfg.LogPath, r.TaskID)
	_, err = fmt.Fprintln(out, sanitize.Line(line, reviewLineMax))
	return err
}

// reviewFlagHead is the full commit id --head names on review r's branch (Ruling 121). Without --head it
// says what the branch is at now and the exact command that approves that commit; a --head that is not
// the branch's head now is "the branch changed since you looked".
func reviewFlagHead(ctx context.Context, st *store.Store, r model.Review, how model.ApproveHow) (string, error) {
	_, t, err := review.Pending(ctx, st, r.ID)
	if err != nil {
		return "", err
	}
	if reviewHeadFlag != "" {
		return gitops.ResolveHead(ctx, t, reviewHeadFlag)
	}
	head, err := gitops.BranchHead(ctx, t)
	switch {
	case err != nil:
		return "", err
	case head == "":
		return "", fmt.Errorf("--approve needs --head <commit>, and branch %s does not exist", t.Branch)
	}
	what := ""
	if stat, serr := gitops.StatAt(ctx, t, head, gitops.StatWidth); serr == nil && stat != "" {
		lines := strings.Split(stat, "\n")
		what = " (" + strings.TrimSpace(lines[len(lines)-1]) + ")"
	}
	flag := ""
	if how != "" {
		flag = " --" + string(how)
	}
	return "", fmt.Errorf("--approve needs --head <commit>, the branch head you looked at: %s is at %s%s. "+
		"Look at it with curia review %s (or --json), then approve exactly that commit with:\n  curia review %s --approve%s --head %s",
		t.Branch, head, what, r.ID, r.ID, flag, head)
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
		r, rerr := review.Resolve(cmd.Context(), st, arg)
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
	if werr := reviewAfterProgram(sig, cmd.ErrOrStderr()); err == nil {
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

// The three review actions check the user guard here, then call the review package.

func reviewApprove(ctx context.Context, a review.Approval) (string, error) {
	if err := userOnly("review", reviewWhat); err != nil {
		return "", err
	}
	return review.Approve(ctx, a)
}

func reviewRequestChanges(ctx context.Context, st *store.Store, tm tmux.Client, logger *slog.Logger, reviewID, note string,
	via model.AnsweredVia, now time.Time,
) (string, error) {
	if err := userOnly("review", reviewWhat); err != nil {
		return "", err
	}
	return review.RequestChanges(ctx, st, tm, logger, reviewID, note, via, now)
}

func reviewDrop(ctx context.Context, st *store.Store, reviewID string, via model.AnsweredVia, now time.Time) (string, error) {
	if err := userOnly("review", reviewWhat); err != nil {
		return "", err
	}
	return review.Drop(ctx, st, reviewID, via, now)
}

// reviewLogOutcome logs a review action's outcome line or error, cleaned.
func reviewLogOutcome(logger *slog.Logger, id, line string, err error) {
	if logger == nil {
		return
	}
	if err != nil {
		logger.Info("review action failed", "review", sanitize.Line(id, 20), "err", sanitize.Line(err.Error(), reviewLineMax))
		return
	}
	logger.Info("review action done", "review", sanitize.Line(id, 20), "outcome", sanitize.Line(line, reviewLineMax))
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
	applyMarkers(ctx, tm, st, logPath, "review attention marker", "", windows)
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
	d.Assumptions, d.Blocking, d.Decided = review.Split(ds, clock())
	if !run.Started.IsZero() && r.CreatedAt.After(run.Started) {
		d.Duration = r.CreatedAt.Sub(run.Started)
	}
	d.DiffStat, d.DiffNote, d.Head = review.DiffStat(ctx, t, gitops.StatWidth)
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
	d, err := reviewDetail(ctx, s.st, s.cfg, r, t)
	if err == nil { // the branch cost never waits: the first ask starts the analyzer (costlens.go)
		d.BranchCostUSD, d.BranchCostKnown = costlensTaskCost(s.cfg, t)
	}
	return d, err
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
	return s.after(ctx, id)(reviewApprove(ctx, review.Approval{Store: s.st, Config: s.cfg, ReviewID: id, Seen: head, Via: s.via, Now: clock()}))
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
		reviewLogOutcome(s.log, id, line, err) // also when the view that started it has gone
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
	return review.DiffCmd(t, s.cfg.Review.DiffTool)
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
	BranchCostUSD   *float64       `json:"branch_cost_usd"`
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
	_, t, err := review.Pending(ctx, st, r.ID)
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
	if usd, ok := costlensTaskCostWait(ctx, cfg, t); ok {
		j.BranchCostUSD = &usd
	}
	return writeJSON(w, j, "")
}

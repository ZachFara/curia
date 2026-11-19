package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
	"github.com/ZachFara/curia/internal/usage"
)

// runProgram runs the dashboard in the terminal; tests replace it, so no
// test needs a terminal.
var runProgram = func(m tea.Model, opts ...tea.ProgramOption) error {
	_, err := tea.NewProgram(m, opts...).Run()
	return err
}

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "ui",
		Short: "Show every session and jump to one (run in a tmux popup)",
		Long: "Lists every session, most in need of you first, refreshed every second, with\n" +
			"each session's task and open decisions (DEC, red when one blocks); an agent\n" +
			"team is one row, which Space expands. j/k or the arrows move, Enter or a\n" +
			"click jumps to the session's pane (a team's: the member most in need of you)\n" +
			"and closes the popup, d opens the session's decisions to answer them (not\n" +
			"inside an agent session) and r the review waiting for its task (read-only\n" +
			"inside an agent session), t opens\n" +
			"the task board (n there spawns a task), / filters on repo, branch, last\n" +
			"update and member name, x then y kills the pane, q or Esc quits. From 100\n" +
			"columns COST and CACHE show each session's cost and prompt cache; b sorts\n" +
			"by burn (dollars an hour) and back, and the status line shows a usage\n" +
			"limit warning or, on the API, the spend projected for the next two hours.\n" +
			"While the limit is forecast to land before the reset, a line suggests the\n" +
			"running task to pause (the lowest priority, then the highest burn): P then\n" +
			"y pauses it as curia pause does (not inside an agent session).",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runDashboard(ui.ModePopup, tea.WithAltScreen(), tea.WithMouseCellMotion())
		},
	}, &cobra.Command{
		Use:   "sidebar",
		Short: "Show every session in a narrow, always-on pane",
		Long: "The dashboard of `curia ui` with narrow columns (state, window, context,\n" +
			"age), for a pane kept open beside your work. A jump leaves it open.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runDashboard(ui.ModeSidebar, tea.WithMouseCellMotion())
		},
	})
}

// runDashboard runs the dashboard in mode over the store until it quits.
func runDashboard(mode ui.Mode, opts ...tea.ProgramOption) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	client := newTmuxClient()
	// The dashboard's actions with a user's effect (the board's ready and
	// spawn keys, d, x y, P y, and the review view's a, c and x) are the
	// user-only commands' (spec section 7, re-review A, C1): the guard is
	// asked once, and when it refuses each of those keys shows its reason
	// and does nothing. Jumps and views stay.
	refused := userOnly("ui", "change, spawn, pause or kill from the dashboard, or answer decisions and reviews")
	src := storeSource{st: st, tm: client, cfg: cfg, refused: refused}
	m := ui.New(src, client, cfg, clock, mode)
	if mode == ui.ModePopup {
		bsrc := boardSource{st: st, tm: client, cfg: cfg, refused: refused}
		m = m.WithBoard(func() tea.Model { return ui.NewBoard(bsrc, client, clock, true) })
		if refused == nil {
			m = m.WithPause()
		}
	}
	if refused != nil {
		m = m.WithoutKill(refused.Error())
		if mode == ui.ModePopup {
			m = m.WithoutDecide(refused.Error()).WithoutPause(refused.Error())
		}
	}
	// d answers through curia decide's path, so it opens only for the user
	// (above). r opens the review view for anyone, read-only when the guard
	// refuses: its approve, change request and drop return the guard's
	// reason (reviewSource.refused), as the board's keys do.
	if mode == ui.ModePopup {
		var closeLog func() error
		m, closeLog = dashboardPopupViews(m, st, client, cfg, refused)
		defer func() { _ = closeLog() }()
	}
	// A hang-up or SIGTERM ends the dashboard, but an approval (or another
	// review action) in flight finishes and is recorded first (C1).
	sig, stop := reviewHoldSignals(false)
	defer stop()
	err = reviewProgramErr(sig, runProgram(m, append(opts, tea.WithContext(sig))...))
	if werr := reviewAfterProgram(sig, os.Stderr); err == nil {
		err = werr
	}
	return err
}

// dashboardPopupViews gives the popup's m its decide view (only when the
// guard let the caller through: refused is nil) and its review view
// (read-only when refused is set), over the log file, and returns the
// function that closes the log. T16: without its log file the dashboard
// still starts, without d or r.
func dashboardPopupViews(m ui.Model, st *store.Store, client tmux.Client, cfg config.Config, refused error) (ui.Model, func() error) {
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		if refused == nil {
			m = m.WithoutDecide("decisions cannot be answered here: the log file cannot be opened")
		}
		return m.WithoutReview("reviews cannot be opened here: the log file cannot be opened"), func() error { return nil }
	}
	dsrc, via := newDecideSource(st, client, logger), decideVia()
	if refused == nil {
		m = m.WithDecide(func(id string) tea.Model {
			return ui.NewDecide(dsrc, clock, ui.DecideOptions{SessionID: id, Embedded: true, Via: via})
		})
	}
	rsrc := newReviewSource(st, client, cfg, logger, via, refused)
	m = m.WithReview(func(id string) tea.Model {
		return ui.NewReview(rsrc, clock, ui.ReviewOptions{TaskID: id, Embedded: true})
	})
	return m, closeLog
}

// storeSource is the dashboard's ui.Source: on every read it reconciles
// with tmux, settles and lists the sessions as ls does and ranks them.
type storeSource struct {
	st  *store.Store
	tm  tmux.Client
	cfg config.Config
	// refused is the user-only guard's refusal of the dashboard's caller, or
	// nil: with it, Pause does nothing and says why.
	refused error
}

// Sessions reconciles and settles, then returns every session that has not
// ended and is not stale for longer than cfg.StaleHideAfter, ranked.
func (s storeSource) Sessions(ctx context.Context) ([]model.Session, error) {
	now := clock()
	if err := reconcile(ctx, s.st, s.tm, s.cfg.LogPath, now); err != nil {
		return nil, err
	}
	sessions, err := listSettledMarked(ctx, s.st, s.tm, store.ListFilter{HideStaleBefore: now.Add(-s.cfg.StaleHideAfter)}, s.cfg.LogPath, now)
	if err != nil {
		return nil, err
	}
	return fleet.Rank(sessions), nil
}

// OpenDecisions expires the assume tickets that are due, then counts the
// open tickets by session and mode.
func (s storeSource) OpenDecisions(ctx context.Context) (map[string]model.OpenCount, error) {
	now := clock()
	if _, err := s.st.ExpireDecisions(ctx, now); err != nil {
		return nil, err
	}
	return s.st.OpenCounts(ctx, now)
}

// Tasks returns every task by id, for the TASK column.
func (s storeSource) Tasks(ctx context.Context) (map[string]model.Task, error) {
	tasks, err := s.st.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.Task, len(tasks))
	for _, t := range tasks {
		out[t.ID] = t
	}
	return out, nil
}

// Reviews returns the id of the pending review of each task not stored
// dropped, by task id, for the r key.
func (s storeSource) Reviews(ctx context.Context) (map[string]string, error) {
	return s.st.PendingReviewTasks(ctx)
}

// Budget reads the usage and cost samples of the burn window: Status is
// the forecast warning's badge text, else in API mode (no usage sample in
// the window) the projection of the live sessions' spend, and Burn each
// session's burn; a zero Budget when nothing was ingested in the window.
// While a warning is in force the pause suggestion is curia usage's
// (usageBuild), computed nowhere else.
func (s storeSource) Budget(ctx context.Context) (ui.Budget, error) {
	now := clock()
	since := now.Add(-usage.BurnWindow)
	samples, err := s.st.UsageSince(ctx, since)
	if err != nil {
		return ui.Budget{}, err
	}
	cost, err := s.st.CostSince(ctx, since)
	if err != nil {
		return ui.Budget{}, err
	}
	var b ui.Budget
	if burn := usage.Burns(cost, now); len(burn) > 0 {
		b.Burn = burn
	}
	if w, ok := usage.Warning(usage.Forecasts(samples, now), now); ok {
		b.Status = usage.BadgeText(w, now)
		return b, uiSuggestPause(ctx, s.st, &b, now)
	}
	if len(cost) == 0 {
		return b, nil
	}
	live, err := uiLiveSessions(ctx, s.st)
	if err != nil {
		return ui.Budget{}, err
	}
	if usd, ok := usage.APIProjection(samples, cost, live, now); ok {
		b.Status = "next 2h ≈ " + usage.CostCell(usd, true) + " at the current rate"
	}
	return b, nil
}

// uiSuggestPause fills b's pause suggestion from usageBuild, when its
// warning is in force and a task qualifies.
func uiSuggestPause(ctx context.Context, st *store.Store, b *ui.Budget, now time.Time) error {
	rep, err := usageBuild(ctx, st, now)
	if err != nil || !rep.Warn || rep.PauseTask == "" {
		return err
	}
	b.PauseTask, b.PauseLabel, b.PauseTitle = rep.PauseTask, rep.PauseLabel, rep.PauseTitle
	b.PauseLimit = "at limit"
	if rep.Warning.LimitAt.After(now) {
		b.PauseLimit = "limit ~" + usage.Short(rep.Warning.LimitAt.Sub(now))
	}
	return nil
}

// Pause pauses task id as curia pause does, without --force, and returns
// the pause's last line. Only the task usageBuild suggests at this moment
// is paused: the dashboard's prompt named the suggestion of its last
// refresh, and anything else is refused with what is suggested now. The
// pause runs to its own limits (git's, the repository's hooks), not cut
// short by the view, and counts as an action in flight (reviewFlight), so
// curia ui waits for it to finish and be recorded before it exits.
func (s storeSource) Pause(ctx context.Context, id string) (string, error) {
	if s.refused != nil {
		return "", s.refused
	}
	defer reviewFlight.begin()()
	ctx = context.WithoutCancel(ctx)
	rep, err := usageBuild(ctx, s.st, clock())
	if err != nil {
		return "", err
	}
	if rep.PauseTask != id {
		now := "nothing is suggested now"
		if rep.PauseTask != "" {
			now = "the suggestion is now " + rep.PauseLabel
		}
		return "", fmt.Errorf("%s not paused: the suggestion changed; %s. Nothing was done", sanitize.Line(id, 40), now)
	}
	var out bytes.Buffer
	if err = pauseTask(ctx, s.st, s.tm, s.cfg, id, pauseOptions{}, &out); err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	return lines[len(lines)-1], nil
}

// uiLiveSessions are the ids of the sessions that have not ended.
func uiLiveSessions(ctx context.Context, st *store.Store) (map[string]bool, error) {
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		live[s.ID] = true
	}
	return live, nil
}

// boardSource is the task board's ui.BoardSource over the store.
type boardSource struct {
	st  *store.Store
	tm  tmux.Client
	cfg config.Config
	// refused is the user-only guard's refusal of the dashboard's caller, or
	// nil: with it, the board's ready and spawn keys do nothing and say why.
	refused error
}

// Cards reconciles with tmux, as the dashboard's refresh does (the board
// replaces it while shown), and derives every task's card.
func (b boardSource) Cards(ctx context.Context) ([]task.Card, error) {
	now := clock()
	if err := reconcile(ctx, b.st, b.tm, b.cfg.LogPath, now); err != nil {
		return nil, err
	}
	return taskCards(ctx, b.st, now)
}

// SetStatus moves task id between backlog and ready, in one write
// transaction: any other status is refused, and so is a task that is no
// longer in backlog or ready (dropped meanwhile).
func (b boardSource) SetStatus(ctx context.Context, id string, to model.TaskStatus) error {
	if b.refused != nil {
		return b.refused
	}
	if !uiBacklogOrReady(to) {
		return fmt.Errorf("the board moves a task only between backlog and ready, not to %q", sanitize.Line(string(to), 40))
	}
	_, err := b.st.UpdateTask(ctx, id, func(cur model.Task) (model.Task, error) {
		if !uiBacklogOrReady(cur.Status) {
			return model.Task{}, fmt.Errorf("%s is %s; the board moves a task only between backlog and ready", cur.ID, cur.Status)
		}
		cur.Status, cur.UpdatedAt = to, clock()
		return cur, nil
	})
	if errors.Is(err, store.ErrNoTask) {
		return fmt.Errorf("%s: no such task", sanitize.Line(id, 40))
	}
	return err
}

func uiBacklogOrReady(s model.TaskStatus) bool {
	return s == model.TaskBacklog || s == model.TaskReady
}

// Spawn starts task id as curia spawn does, without waiting for the agent
// to report, and returns spawnLine's line with any setup step that was
// skipped (an unapproved script). The board bounds an action by a few
// seconds; a spawn runs to its own limits instead (git's, an approved setup
// script's), so a slow checkout or setup is not cut short. The board runs
// it off its update loop and refuses another action until it returns.
func (b boardSource) Spawn(ctx context.Context, id string) (string, error) {
	if b.refused != nil {
		return "", b.refused
	}
	var out bytes.Buffer
	// ReadyOnly unless the guard passed (Ruling 113), should the source be
	// wired without the refusal elsewhere.
	r, err := spawnTask(context.WithoutCancel(ctx), b.st, b.tm, b.cfg, id, spawnOptions{Wait: 0, Out: &out, ReadyOnly: b.refused != nil})
	if err != nil {
		return "", err
	}
	parts := []string{spawnLine(r)}
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "Setup: ") && strings.Contains(l, "skipped") {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, "; "), nil
}

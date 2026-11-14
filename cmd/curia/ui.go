package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

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
			"inside an agent session), t opens the task board (n there spawns a task), /\n" +
			"filters on repo, branch, last update and member name, x then y kills the\n" +
			"pane, q or Esc quits.",
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
	src := storeSource{st: st, tm: client, cfg: cfg}
	m := ui.New(src, client, cfg, clock, mode)
	if mode == ui.ModePopup {
		bsrc := boardSource{st: st, tm: client, cfg: cfg}
		m = m.WithBoard(func() tea.Model { return ui.NewBoard(bsrc, client, clock, true) })
	}
	// d answers through curia decide's path, so it refuses inside an agent
	// session as curia decide does (spec section 7): no factory is set,
	// and the dashboard says why when d is pressed.
	// T16: without its log file the dashboard still starts, without d.
	if mode == ui.ModePopup && refuseInsideAgent() == nil {
		logger, closeLog, err := openLog(cfg.LogPath)
		if err != nil {
			return runProgram(m.WithoutDecide("decisions cannot be answered here: the log file cannot be opened"), opts...)
		}
		defer func() { _ = closeLog() }()
		dsrc, via := newDecideSource(st, client, logger), decideVia()
		m = m.WithDecide(func(id string) tea.Model {
			return ui.NewDecide(dsrc, clock, ui.DecideOptions{SessionID: id, Embedded: true, Via: via})
		})
	}
	return runProgram(m, opts...)
}

// storeSource is the dashboard's ui.Source: on every read it reconciles
// with tmux, settles and lists the sessions as ls does and ranks them.
type storeSource struct {
	st  *store.Store
	tm  tmux.Client
	cfg config.Config
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

// boardSource is the task board's ui.BoardSource over the store.
type boardSource struct {
	st  *store.Store
	tm  tmux.Client
	cfg config.Config
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
	var out bytes.Buffer
	r, err := spawnTask(context.WithoutCancel(ctx), b.st, b.tm, b.cfg, id, spawnOptions{Wait: 0, Out: &out})
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

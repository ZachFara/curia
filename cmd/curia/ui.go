package main

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
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
		Long: "Lists every session, most in need of you first, refreshed every second.\n" +
			"j/k or the arrows move, Enter or a click jumps to the session's pane and\n" +
			"closes the popup, / filters on repo, branch and last update, x then y kills\n" +
			"the pane, q or Esc quits.",
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
	return runProgram(ui.New(src, client, cfg, clock, mode), opts...)
}

// storeSource is the dashboard's ui.Source: on every read it reconciles
// with tmux, lists the sessions as ls does and ranks them.
type storeSource struct {
	st  *store.Store
	tm  tmux.Client
	cfg config.Config
}

// Sessions reconciles, then returns every session that has not ended and
// is not stale for longer than cfg.StaleHideAfter, ranked.
func (s storeSource) Sessions(ctx context.Context) ([]model.Session, error) {
	now := clock()
	if err := reconcile(ctx, s.st, s.tm, s.cfg.LogPath, now); err != nil {
		return nil, err
	}
	sessions, err := s.st.ListSessions(ctx, store.ListFilter{HideStaleBefore: now.Add(-s.cfg.StaleHideAfter)})
	if err != nil {
		return nil, err
	}
	return fleet.Rank(sessions), nil
}

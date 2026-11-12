package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/store"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "next",
		Short: "Switch to the session that needs you most (run from a tmux key binding)",
		Long: "Marks sessions whose pane is gone as stale and settles interrupted ones (as\n" +
			"ls does), then switches the tmux client to the pane of the most urgent session\n" +
			"that needs a decision, is waiting, or went idle since you last looked;\n" +
			"otherwise shows \"nothing needs you\". Always exits 0; errors go to the log file.",
		Args: cobra.ArbitraryArgs,
		// Bad flags or arguments reach runNext, so they are logged, not printed.
		DisableFlagParsing: true,
		Run:                runNext,
	})
}

// runNext reconciles as ls does, jumps to fleet.Next and always exits 0,
// logging any error: tmux runs it from a key binding, where output and exit
// codes go unseen.
func runNext(cmd *cobra.Command, args []string) {
	cfg, cfgErr := loadConfig()
	if cfgErr != nil {
		cfg = config.Default() // still log to the default log file
	}
	err := cfgErr
	if err == nil {
		err = jumpNext(cmd.Context(), cfg, args)
	}
	if err == nil {
		return
	}
	if logger, closeLog, lerr := openLog(cfg.LogPath); lerr == nil {
		logger.Error("next failed", "err", err.Error())
		_ = closeLog()
	}
}

func jumpNext(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("next takes no arguments, got %q", args)
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	client := newTmuxClient()
	// Reconcile first, so a session whose pane is gone is never the target.
	now := clock()
	if err = reconcile(ctx, st, client, cfg.LogPath, now); err != nil {
		return err
	}
	// Settle next, so a session a denial left waiting is not the target.
	sessions, err := listSettledMarked(ctx, st, client, store.ListFilter{}, cfg.LogPath, now)
	if err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	s, ok := fleet.Next(sessions)
	if !ok {
		return client.DisplayMessage(tctx, "nothing needs you")
	}
	if err := client.SwitchTo(tctx, s.Pane); err != nil {
		return fmt.Errorf("switch to session %s: %w", s.ID, err)
	}
	return nil
}

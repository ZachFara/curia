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
		Long: "Switches the tmux client to the pane of the most urgent session that needs a\n" +
			"decision, is waiting, or went idle since you last looked; otherwise shows\n" +
			"\"nothing needs you\". Always exits 0; errors go to the log file.",
		Args: cobra.ArbitraryArgs,
		// Bad flags or arguments reach runNext, so they are logged, not printed.
		DisableFlagParsing: true,
		Run:                runNext,
	})
}

// runNext jumps to fleet.Next and always exits 0, logging any error: tmux
// runs it from a key binding, where output and exit codes go unseen.
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
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	client := newTmux()
	s, ok := fleet.Next(sessions)
	if !ok {
		return client.DisplayMessage(tctx, "nothing needs you")
	}
	if err := client.SwitchTo(tctx, s.Pane); err != nil {
		return fmt.Errorf("switch to session %s: %w", s.ID, err)
	}
	return nil
}

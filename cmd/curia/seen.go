package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/attention"
	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "seen <pane>",
		Short: "Mark the sessions in a pane as looked at (run by tmux on pane focus)",
		Long: "Marks the pane's sessions seen and clears the attention marker of its window.\n" +
			"Always exits 0 and prints nothing; errors go to the log file.",
		Args: cobra.ArbitraryArgs,
		Run:  runSeen,
	})
}

// runSeen logs every failure and returns normally, so the exit code is 0.
func runSeen(cmd *cobra.Command, args []string) {
	logger := slog.New(slog.DiscardHandler)
	cfg, err := loadConfig()
	if err != nil {
		cfg = config.Default()
	}
	if l, c, lerr := openLog(cfg.LogPath); lerr == nil {
		logger = l
		defer func() { _ = c() }()
	}
	if err == nil {
		err = markSeen(cmd.Context(), args, cfg)
	}
	if err != nil {
		logger.Error("seen failed", "args", fmt.Sprint(args), "err", err.Error())
	}
}

// markSeen marks the pane's sessions seen, then refreshes the marker of each
// window they are in. A pane with no session does nothing.
func markSeen(ctx context.Context, args []string, cfg config.Config) error {
	if len(args) != 1 {
		return fmt.Errorf("want one pane id, got %d arguments", len(args))
	}
	pane := args[0]
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return err
	}
	windows := paneWindows(sessions, pane)
	if len(windows) == 0 {
		return nil
	}
	if err = st.MarkSeen(ctx, pane, time.Now()); err != nil {
		return err
	}
	client := newTmuxClient()
	for _, w := range windows {
		actx, cancel := context.WithTimeout(ctx, subprocessTimeout)
		err = attention.Apply(actx, client, st, w)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// paneWindows returns the distinct non-empty windows of the sessions in pane.
func paneWindows(sessions []model.Session, pane string) []string {
	var windows []string
	for _, s := range sessions {
		if s.Pane == pane && s.TmuxWindow != "" && !slices.Contains(windows, s.TmuxWindow) {
			windows = append(windows, s.TmuxWindow)
		}
	}
	return windows
}

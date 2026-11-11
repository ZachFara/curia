package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/store"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Print the status-bar badge (run by tmux every few seconds)",
		Long: "Prints D<n> W<n> R<n> (decisions, waiting, reviews) with zero parts left out\n" +
			"and no newline. Prints nothing when nothing needs you or on any error.",
		Args: cobra.ArbitraryArgs,
		// Bad flags or arguments are errors, which print nothing.
		DisableFlagParsing: true,
		Run:                runStatus,
	})
}

// runStatus prints the badge, or nothing on any error, and always exits 0:
// whatever it prints lands in the tmux status bar.
func runStatus(cmd *cobra.Command, args []string) {
	if badge, err := statusBadge(cmd.Context(), args); err == nil {
		_, _ = fmt.Fprint(cmd.OutOrStdout(), badge)
	}
}

func statusBadge(ctx context.Context, args []string) (string, error) {
	if len(args) != 0 {
		return "", fmt.Errorf("status takes no arguments, got %q", args)
	}
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	st, err := openStore(cfg)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return "", err
	}
	return fleet.Badge(fleet.Count(sessions)), nil
}

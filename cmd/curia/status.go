package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/usage"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Print the status-bar badge (run by tmux every few seconds)",
		Long: "Prints D<n> Q<n> W<n> R<n> (open blocking decisions, answers and\n" +
			"instructions not yet delivered to their agent, waiting sessions, reviews)\n" +
			"with zero parts left out, then \"auto-off\" while auto-send is tripped off,\n" +
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
	// No reconcile (no tmux call) and no expiry sweep: an assume ticket past
	// its deadline is not open, so it is not counted either way. The only
	// write is settling a session a denial or an interrupt left working or
	// waiting, which costs nothing while no session is either.
	now := clock()
	sessions, _, err := listSettled(ctx, st, store.ListFilter{}, "", now)
	if err != nil {
		return "", err
	}
	openBlocking, undelivered, err := st.BadgeCounts(ctx, now)
	if err != nil {
		return "", err
	}
	messages, autoOff, err := st.BadgeExtras(ctx)
	if err != nil {
		return "", err
	}
	reviews, err := st.PendingReviews(ctx)
	if err != nil {
		return "", err
	}
	counts := fleet.Count(sessions, openBlocking, undelivered+messages)
	counts.Reviews = reviews
	counts.AutoOff = autoOff
	counts.Forecast = statusForecast(ctx, st, now)
	badge := fleet.Badge(counts)
	if settingsExternalPending(ctx, st) { // C!: the user file was edited outside Curia (F28)
		badge = strings.TrimSpace(badge + " C!")
	}
	for _, s := range sessions { // F20: a live bypass is always visible
		if modeNow(s, cfg, now) == model.ModeBypass {
			return strings.TrimSpace(badge + " BYPASS"), nil
		}
	}
	return badge, nil
}

// statusForecast is the badge's limit warning, already rendered: one read of
// the last 20 minutes of samples from a covering index (UsageReadings), no
// network, no process. Empty when no window warns or when the read fails
// (the counts still print).
func statusForecast(ctx context.Context, st *store.Store, now time.Time) string {
	samples, err := st.UsageReadings(ctx, now.Add(-usage.BurnWindow))
	if err != nil {
		return ""
	}
	if w, ok := usage.Warning(usage.Forecasts(samples, now), now); ok {
		return usage.BadgeText(w, now)
	}
	return ""
}

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/route"
)

// routingJSON is the flag of `curia routing stats`; tests reset it.
var routingJSON bool

func init() {
	parent := &cobra.Command{
		Use:   "routing",
		Short: "Show and reset the auto-send gate",
	}
	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show routing accuracy and the auto-send gate",
		Args:  cobra.NoArgs,
		RunE:  runRoutingStats,
	}
	stats.Flags().BoolVar(&routingJSON, "json", false, "print a JSON object instead of text")
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Clear the earned and tripped flags (the user only)",
		Args:  cobra.NoArgs,
		RunE:  runRoutingReset,
	}
	parent.AddCommand(stats, reset)
	rootCmd.AddCommand(parent)
}

// routingStats is `curia routing stats --json`.
type routingStats struct {
	Threshold  float64    `json:"threshold"`
	Configured bool       `json:"configured"`
	Earned     bool       `json:"earned"`
	EarnedAt   *time.Time `json:"earned_at"`
	Tripped    bool       `json:"tripped"`
	TrippedAt  *time.Time `json:"tripped_at"`
	Labeled    int        `json:"labeled"`
	Accepted   int        `json:"accepted"`
}

func runRoutingStats(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	earnedAt, trippedAt, err := st.RoutingState(cmd.Context())
	if err != nil {
		return err
	}
	labels, err := st.RouteLabels(cmd.Context(), cfg.Routing.AutoThreshold, route.Window)
	if err != nil {
		return err
	}
	g := route.Gate(labels)
	s := routingStats{
		Threshold: cfg.Routing.AutoThreshold, Configured: cfg.Routing.AutoSend,
		Earned: !earnedAt.IsZero(), EarnedAt: jsonTime(earnedAt),
		Tripped: !trippedAt.IsZero(), TrippedAt: jsonTime(trippedAt),
		Labeled: g.Labeled, Accepted: g.Accepted,
	}
	return routingPrint(cmd.OutOrStdout(), s)
}

func routingPrint(w io.Writer, s routingStats) error {
	if routingJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	pct := 0
	if s.Labeled > 0 {
		pct = (s.Accepted*100 + s.Labeled/2) / s.Labeled
	}
	_, _ = fmt.Fprintf(w, "threshold:   %g\n", s.Threshold)
	_, _ = fmt.Fprintf(w, "labeled:     %d/%d at or above the threshold\n", s.Labeled, route.Window)
	_, _ = fmt.Fprintf(w, "accuracy:    %d/%d accepted (%d%%)\n", s.Accepted, s.Labeled, pct)
	_, _ = fmt.Fprintf(w, "configured:  %s\n", routingYesNo(s.Configured, nil))
	_, _ = fmt.Fprintf(w, "earned:      %s\n", routingYesNo(s.Earned, s.EarnedAt))
	_, _ = fmt.Fprintf(w, "tripped:     %s\n", routingYesNo(s.Tripped, s.TrippedAt))
	return nil
}

// routingYesNo is "yes" (with the time, if any) or "no".
func routingYesNo(v bool, at *time.Time) string {
	switch {
	case !v:
		return "no"
	case at != nil:
		return "yes (" + at.Format(time.RFC3339) + ")"
	}
	return "yes"
}

func runRoutingReset(cmd *cobra.Command, _ []string) error {
	if err := refuseInsideAgent(); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if err := st.ResetRouting(cmd.Context()); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "routing reset: auto-send must be earned again")
	return nil
}

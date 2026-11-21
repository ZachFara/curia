package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia timeline`; tests reset them.
var (
	timelineSinceFlag, timelineRepo, timelineTask string
	timelineJSON                                  bool
	timelineLimit                                 int
)

// timelineDefaultSince bounds a read that names no --since, so a large
// store is never read from its start.
const timelineDefaultSince = 24 * time.Hour

// timelineLineLimit bounds the text of one line.
const timelineLineLimit = 120

func init() {
	c := &cobra.Command{
		Use:   "timeline",
		Short: "What happened across all agents, in order",
		Long: "Prints a union over existing rows, oldest first: sessions started and ended,\n" +
			"turns ended (with their last line), tickets raised and answered, reviews\n" +
			"opened and closed, messages delivered, tasks filed and pushes sent. --since\n" +
			"takes a duration (6h, 2d) or a date (default 24h); --repo and --task keep\n" +
			"one repository's or task's events; --limit keeps the newest N. --json prints\n" +
			"a JSON array.",
		Args: cobra.NoArgs,
		RunE: runTimeline,
	}
	c.Flags().StringVar(&timelineSinceFlag, "since", "", "how far back: 6h, 2d or 2026-11-19 (default 24h)")
	c.Flags().StringVar(&timelineRepo, "repo", "", "only this repository (its top-level path)")
	c.Flags().StringVar(&timelineTask, "task", "", "only this task, e.g. T-42")
	c.Flags().IntVar(&timelineLimit, "limit", 200, "show at most the newest N events")
	c.Flags().BoolVar(&timelineJSON, "json", false, "print a JSON array instead of a table")
	rootCmd.AddCommand(c)
}

// timelineSince is the time a --since value names: a duration back from now
// ("6h"), whole days ("2d") or a date. Empty is def back from now.
func timelineSince(v string, now time.Time, def time.Duration) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return now.Add(-def), nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, now.Location()); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: want a duration (6h), days (2d) or a date (2026-11-19)", sanitize.Line(v, 40))
}

func runTimeline(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := clock()
	since, err := timelineSince(timelineSinceFlag, now, timelineDefaultSince)
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	evs, err := st.Timeline(cmd.Context(), store.TimelineQuery{Since: since, Repo: timelineRepo, TaskID: timelineTask, Limit: timelineLimit})
	if err != nil {
		return err
	}
	slices.Reverse(evs) // newest first from the store; printed oldest first
	if timelineJSON {
		return timelinePrintJSON(cmd.OutOrStdout(), evs)
	}
	return timelinePrintTable(cmd.OutOrStdout(), evs)
}

// timelineWho is the task, or the session's first eight characters.
func timelineWho(e model.TimelineEvent) string {
	if e.TaskID != "" {
		return e.TaskID
	}
	return e.SessionID[:min(8, len(e.SessionID))]
}

func timelinePrintTable(w io.Writer, evs []model.TimelineEvent) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tWHO\tKIND\tLINE")
	for _, e := range evs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.At.Local().Format("01-02 15:04:05"),
			ui.Cell(sanitize.Line(timelineWho(e), 12)), ui.Cell(e.Kind), ui.Cell(sanitize.Line(e.Line, timelineLineLimit)))
	}
	return tw.Flush()
}

func timelinePrintJSON(w io.Writer, evs []model.TimelineEvent) error {
	type row struct {
		At      time.Time `json:"at"`
		Session string    `json:"session"`
		Task    string    `json:"task"`
		Kind    string    `json:"kind"`
		Line    string    `json:"line"`
	}
	rows := make([]row, 0, len(evs)) // never nil, so no events is []
	for _, e := range evs {
		rows = append(rows, row{e.At.UTC(), sanitize.Line(e.SessionID, 80), sanitize.Line(e.TaskID, 80), e.Kind, sanitize.Line(e.Line, timelineLineLimit)})
	}
	return writeJSON(w, rows, "  ")
}

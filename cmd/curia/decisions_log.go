package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia decisions log`; tests reset them.
var decisionlogRepo, decisionlogScope, decisionlogStatus, decisionlogSince string

// Cell limits of the log.
const (
	decisionlogQuestionLimit = 60
	decisionlogAnswerLimit   = 30
)

func init() {
	c := &cobra.Command{
		Use:   "log [query]",
		Short: "Search the decision history with how often each was reused",
		Long: "Lists decided tickets, newest first, matching the words of query (the same\n" +
			"full-text index precedent search uses). REUSE is the count of tickets that\n" +
			"cited the decision as their precedent and the date of the last; ASKED is the\n" +
			"count of later tickets raised although it was returned as their near match.\n" +
			"A superseded decision is marked and not counted among the reuse targets of\n" +
			"the summary line. --status shows another status instead; --since takes a\n" +
			"duration (6h, 2d) or a date.",
		Args: cobra.MaximumNArgs(1),
		RunE: runDecisionLog,
	}
	c.Flags().StringVar(&decisionlogRepo, "repo", "", "only this repository (its top-level path)")
	c.Flags().StringVar(&decisionlogScope, "scope", "", "only this scope: task, repo or global")
	c.Flags().StringVar(&decisionlogStatus, "status", "", "only this status (default: answered, confirmed, overridden)")
	c.Flags().StringVar(&decisionlogSince, "since", "", "only tickets raised since: 6h, 2d or 2026-11-19")
	decisionsCmd().AddCommand(c)
}

// decisionsCmd is the `decisions` parent registered by decisions.go.
func decisionsCmd() *cobra.Command {
	for _, c := range rootCmd.Commands() {
		if c.Name() == "decisions" {
			return c
		}
	}
	panic("decisions command is not registered before decisionlog.go's init")
}

func runDecisionLog(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := clock()
	q := store.DecisionLogQuery{Repo: decisionlogRepo, Scope: decisionlogScope, Status: model.DecisionStatus(decisionlogStatus)}
	if len(args) > 0 {
		q.Text = args[0]
	}
	if decisionlogSince != "" {
		if q.Since, err = timelineSince(decisionlogSince, now, 0); err != nil {
			return err
		}
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	rows, sum, err := st.DecisionLog(cmd.Context(), q)
	if err != nil {
		return err
	}
	return decisionlogPrint(cmd.OutOrStdout(), rows, sum, now)
}

func decisionlogCell(s string, limit int) string { return ui.Cell(sanitize.Line(s, limit)) }

func decisionlogPrint(w io.Writer, rows []model.DecisionLogRow, sum model.DecisionLogSummary, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tSCOPE\tDATE\tREUSE\tASKED\tQUESTION\tANSWER")
	for _, r := range rows {
		status := string(r.Status)
		if r.Superseded {
			status += " (superseded)"
		}
		reuse := fmt.Sprint(r.Reuse)
		if r.Reuse > 0 {
			reuse += " (last " + ui.Age(r.LastReuse, now) + " ago)"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			decisionlogCell(r.ID, 20), decisionlogCell(status, 30), decisionlogCell(r.Scope, 10),
			r.CreatedAt.Local().Format("2006-01-02"), reuse, r.AskedAgain,
			decisionlogCell(r.Question, decisionlogQuestionLimit), decisionlogCell(r.Answer, decisionlogAnswerLimit))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, decisionlogSummaryLine(sum))
	return err
}

// decisionlogSummaryLine is the share of answered decisions reused at least once.
func decisionlogSummaryLine(s model.DecisionLogSummary) string {
	if s.Answered == 0 {
		return "no answered decisions"
	}
	return strings.TrimSpace(fmt.Sprintf("%d of %d answered decisions (%d%%) were reused at least once",
		s.Reused, s.Answered, s.Reused*100/s.Answered))
}

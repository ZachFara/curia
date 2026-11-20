package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
	"github.com/ZachFara/curia/internal/usage"
)

// curia time's flags; tests reset them.
var (
	activitySince time.Duration
	activityRepo  string
	activityTask  string
	activityBy    string
	activityJSON  bool
)

func init() {
	c := &cobra.Command{
		Use:   "time",
		Short: "Show where agent time goes, and how much is waiting on me",
		Long: "Classifies each session's time (coding, reading, testing, building, waiting on CI,\n" +
			"git, thinking, waiting on me, other) from the stored hook events and prints agent\n" +
			"time (summed over sessions) and wall time, with the share of agent time spent\n" +
			"waiting on me. --by groups by class (default), task, type (the branch prefix),\n" +
			"repo or session. Read-only; no model is called.",
		Args: cobra.NoArgs,
		RunE: runActivity,
	}
	c.Flags().DurationVar(&activitySince, "since", 24*time.Hour, "how far back to look")
	c.Flags().StringVar(&activityRepo, "repo", "", "only this repository (name or path)")
	c.Flags().StringVar(&activityTask, "task", "", "only this task, e.g. T-42")
	c.Flags().StringVar(&activityBy, "by", "class", "group by class, task, type, repo or session")
	c.Flags().BoolVar(&activityJSON, "json", false, "print one JSON object instead of text")
	rootCmd.AddCommand(c)
}

// activityRow is one group of the report.
type activityRow struct {
	Key         string  `json:"key"`
	AgentSecs   float64 `json:"agent_seconds"`
	WallSecs    float64 `json:"wall_seconds"`
	WaitingSecs float64 `json:"waiting_on_me_seconds"`
}

// activitySession is one session of the report.
type activitySession struct {
	ID        string             `json:"session_id"`
	TaskID    string             `json:"task_id"`
	Repo      string             `json:"repo"`
	Type      string             `json:"type"`
	AgentSecs float64            `json:"agent_seconds"`
	WallSecs  float64            `json:"wall_seconds"`
	ByClass   map[string]float64 `json:"by_class"`
}

type activityReport struct {
	Since     string             `json:"since"`
	By        string             `json:"by"`
	AgentSecs float64            `json:"agent_seconds"`
	WallSecs  float64            `json:"wall_seconds"`
	WaitShare *float64           `json:"waiting_on_me_share"` // null without agent time
	Headline  string             `json:"headline"`
	ByClass   map[string]float64 `json:"by_class"`
	Rows      []activityRow      `json:"rows"`
	Sessions  []activitySession  `json:"sessions"`
}

// activityItem is one classified session.
type activityItem struct {
	s     model.Session
	typ   string
	t     usage.ActivityTime
	agent time.Duration
}

// activityBuild classifies every session with events in the window.
// rulesFor gives the rules table for a repository: today the user's rules
// for every repository; the repository's own layer (.curia/config.toml,
// F28) is to be connected when F28 merges, by passing a func that adds them.
func activityBuild(ctx context.Context, st *store.Store, now time.Time, rulesFor func(repo string) *usage.ActivityRules, idleCap time.Duration, types []string) (activityReport, error) {
	since := now.Add(-activitySince)
	evs, err := st.ActivityEvents(ctx, since, "")
	if err != nil {
		return activityReport{}, err
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{IncludeEnded: true})
	if err != nil {
		return activityReport{}, err
	}
	var items []activityItem
	for _, s := range sessions {
		if len(evs[s.ID]) == 0 || activityRepo != "" && s.Repo != activityRepo && filepath.Base(s.Repo) != activityRepo || activityTask != "" && s.TaskID != activityTask {
			continue
		}
		live := s.State != model.StateEnded && s.State != model.StateStale
		it := activityItem{s: s, typ: usage.ActivityTaskType(s.Branch, types)}
		it.t = usage.ClassifyActivity(evs[s.ID], rulesFor(s.Repo), usage.ActivityOpts{IdleCap: idleCap, Since: since, Now: now, Live: live})
		for _, d := range it.t.ByClass {
			it.agent += d
		}
		items = append(items, it)
	}
	return activitySummarize(items, since), nil
}

// activitySummarize totals the items and groups them by activityBy.
func activitySummarize(items []activityItem, since time.Time) activityReport {
	rep := activityReport{Since: since.UTC().Format(time.RFC3339), By: activityBy, ByClass: map[string]float64{}, Rows: []activityRow{}, Sessions: []activitySession{}}
	byClass := map[string]time.Duration{}
	var all []usage.ActivityTime
	var agent time.Duration
	for _, it := range items {
		sec := activitySession{
			ID: it.s.ID, TaskID: it.s.TaskID, Repo: it.s.Repo, Type: it.typ, ByClass: map[string]float64{},
			AgentSecs: it.agent.Seconds(), WallSecs: usage.WallTime(it.t).Seconds(),
		}
		for c, d := range it.t.ByClass {
			byClass[c] += d
			sec.ByClass[c] = d.Seconds()
		}
		rep.Sessions = append(rep.Sessions, sec)
		all = append(all, it.t)
		agent += it.agent
	}
	wall := usage.WallTime(all...)
	rep.AgentSecs, rep.WallSecs = agent.Seconds(), wall.Seconds()
	if agent > 0 {
		share := byClass[usage.ClassWaitingMe].Seconds() / agent.Seconds()
		rep.WaitShare = &share
	}
	rep.Headline = usage.ActivityHeadline(byClass, wall)
	for _, c := range usage.ActivityClasses {
		if d := byClass[c]; d > 0 {
			rep.ByClass[c] = d.Seconds()
			if activityBy == "class" {
				rep.Rows = append(rep.Rows, activityRow{Key: c, AgentSecs: d.Seconds()})
			}
		}
	}
	if activityBy != "class" {
		rep.Rows = activityGroups(items)
	}
	return rep
}

// activityGroups groups the items by activityBy, largest agent time first.
func activityGroups(items []activityItem) []activityRow {
	groups := map[string][]usage.ActivityTime{}
	rows := map[string]*activityRow{}
	for _, it := range items {
		key := map[string]string{"task": it.s.TaskID, "type": it.typ, "repo": it.s.Repo, "session": it.s.ID}[activityBy]
		if key == "" {
			key = "-"
		}
		if rows[key] == nil {
			rows[key] = &activityRow{Key: key}
		}
		rows[key].AgentSecs += it.agent.Seconds()
		rows[key].WaitingSecs += it.t.ByClass[usage.ClassWaitingMe].Seconds()
		groups[key] = append(groups[key], it.t)
	}
	out := make([]activityRow, 0, len(rows))
	for k, r := range rows {
		r.WallSecs = usage.WallTime(groups[k]...).Seconds()
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b activityRow) int {
		return cmp.Or(cmp.Compare(b.AgentSecs, a.AgentSecs), cmp.Compare(a.Key, b.Key))
	})
	return out
}

func runActivity(cmd *cobra.Command, _ []string) error {
	if !slices.Contains([]string{"class", "task", "type", "repo", "session"}, activityBy) {
		return fmt.Errorf("--by %q: want class, task, type, repo or session", activityBy)
	}
	if activitySince <= 0 {
		return fmt.Errorf("--since %v: must be above zero", activitySince)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	rules, err := usage.NewActivityRules(cfg.Activity.Rules)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	rep, err := activityBuild(cmd.Context(), st, clock(), func(string) *usage.ActivityRules { return rules }, max(cfg.Activity.IdleCap, time.Minute), cfg.Activity.TaskTypes)
	if err != nil {
		return err
	}
	if activityJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return activityPrint(cmd.OutOrStdout(), rep)
}

func activityPrint(w io.Writer, rep activityReport) error {
	_, _ = fmt.Fprintln(w, rep.Headline)
	if len(rep.Rows) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "%s\tAGENT\tWALL\tWAITING ON ME\n", map[string]string{"class": "CLASS", "task": "TASK", "type": "TYPE", "repo": "REPO", "session": "SESSION"}[rep.By])
	for _, r := range rep.Rows {
		wall, waiting := "", ""
		if rep.By != "class" {
			wall = usage.ActivityDuration(time.Duration(r.WallSecs * float64(time.Second)))
			waiting = fmt.Sprintf("%.0f%%", r.WaitingSecs*100/max(r.AgentSecs, 1))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ui.Cell(r.Key), usage.ActivityDuration(time.Duration(r.AgentSecs*float64(time.Second))), wall, waiting)
	}
	return tw.Flush()
}

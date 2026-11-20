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
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/ui"
	"github.com/ZachFara/curia/internal/usage"
)

// usageJSON is `curia usage --json`; tests reset it.
var usageJSON bool

const (
	usageBurnLimit  = 10             // rows of the burn table
	usageLabelLimit = 60             // runes of the pause label and title
	usageAnyWindow  = 24 * time.Hour // how far back "something was ingested" looks
	usageNothing    = "no usage has been ingested; see curia install (status line) or cost-lens"
	usageNoRecent   = "nothing was ingested in the last 20 minutes"
)

func init() {
	c := &cobra.Command{
		Use:   "usage",
		Short: "Show the usage windows, the burn forecast and who burns it",
		Long: "Prints one line per usage window with a reading in the last 20 minutes, in\n" +
			"API mode the projected spend, the sessions that burn most, and while a\n" +
			"limit forecast is in force the task it suggests pausing. Read-only: it\n" +
			"pauses nothing. --json prints one object instead.",
		Args: cobra.NoArgs,
		RunE: runUsage,
	}
	c.Flags().BoolVar(&usageJSON, "json", false, "print one JSON object instead of text")
	rootCmd.AddCommand(c)
}

// usageReport is everything curia usage shows, computed once.
type usageReport struct {
	Forecasts  []usage.Forecast
	Warning    usage.Forecast
	Warn       bool
	APIUSD     float64
	API        bool
	Burn       []usageBurnRow // highest first
	PauseTask  string         // "" when there is no suggestion
	PauseLabel string         // "T-7 (P3) api-server"
	PauseTitle string
	PauseBusy  bool // the agent is mid-turn: pause would refuse it now
	Any        bool // something was ingested in the last 24 hours
}

// usageBurnRow is one session of the burn table.
type usageBurnRow struct {
	SessionID, Window, TaskID string
	USDPerHour, CostUSD       float64
	CostKnown                 bool
}

// usageBuild reads the samples, the sessions and the task cards and applies
// section 4.4. It writes nothing.
func usageBuild(ctx context.Context, st *store.Store, now time.Time) (usageReport, error) {
	var rep usageReport
	since := now.Add(-usage.BurnWindow)
	samples, err := st.UsageSince(ctx, since)
	if err != nil {
		return rep, err
	}
	cost, err := st.CostSince(ctx, since)
	if err != nil {
		return rep, err
	}
	if rep.Any, err = usageAny(ctx, st, now, samples, cost); err != nil || !rep.Any {
		return rep, err
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return rep, err
	}
	rep.Forecasts = usage.Forecasts(samples, now)
	rep.Warning, rep.Warn = usage.Warning(rep.Forecasts, now)
	burns := usage.Burns(cost, now)
	live := map[string]bool{}
	for _, s := range sessions {
		live[s.ID] = s.State != model.StateEnded && s.State != model.StateStale
	}
	rep.APIUSD, rep.API = usage.APIProjection(samples, cost, live, now)
	rep.Burn = usageBurnRows(burns, sessions)
	if rep.Warn {
		cards, err := taskCards(ctx, st, now)
		if err != nil {
			return rep, err
		}
		usagePause(&rep, cards, burns)
	}
	return rep, nil
}

// usageAny reports whether anything was ingested in the last 24 hours.
func usageAny(ctx context.Context, st *store.Store, now time.Time, samples []model.UsageSample, cost []model.CostSample) (bool, error) {
	recent := now.Add(-usage.BurnWindow)
	if len(samples) > 0 || slices.ContainsFunc(cost, func(c model.CostSample) bool { return !c.At.Before(recent) }) {
		return true, nil
	}
	day := now.Add(-usageAnyWindow)
	u, err := st.UsageSince(ctx, day)
	if err != nil || len(u) > 0 {
		return len(u) > 0, err
	}
	c, err := st.CostSince(ctx, day)
	return slices.ContainsFunc(c, func(c model.CostSample) bool { return !c.At.Before(day) }), err
}

// usageBurnRows is the burn table: every session with a cost sample in the
// window, highest burn first, at most usageBurnRows.
func usageBurnRows(burns map[string]float64, sessions []model.Session) []usageBurnRow {
	by := map[string]model.Session{}
	for _, s := range sessions {
		by[s.ID] = s
	}
	rows := make([]usageBurnRow, 0, len(burns))
	for id, b := range burns {
		s := by[id]
		rows = append(rows, usageBurnRow{
			SessionID: id, Window: s.TmuxWindow, TaskID: s.TaskID,
			USDPerHour: b, CostUSD: s.CostUSD, CostKnown: s.CostKnown,
		})
	}
	slices.SortFunc(rows, func(a, b usageBurnRow) int {
		if c := cmp.Compare(b.USDPerHour, a.USDPerHour); c != 0 {
			return c
		}
		return cmp.Compare(a.SessionID, b.SessionID)
	})
	return rows[:min(len(rows), usageBurnLimit)]
}

// usagePause fills the report's suggestion from the running tasks. A task's
// burn is the sum over its live linked sessions.
func usagePause(rep *usageReport, cards []task.Card, burns map[string]float64) {
	var running []usage.Running
	byID, busyIDs := map[string]task.Card{}, map[string]bool{}
	for _, c := range cards {
		if c.Status != model.TaskRunning {
			continue
		}
		sum := 0.0
		for _, s := range c.Sessions {
			sum += burns[s.ID]
		}
		// The test curia pause applies without --force.
		busy := pauseCheckStates(c.Task, c.Sessions) != nil
		running = append(running, usage.Running{TaskID: c.Task.ID, N: c.Task.N, Priority: c.Task.Priority, BurnUSD: sum, Busy: busy})
		if busy {
			busyIDs[c.Task.ID] = true
		}
		byID[c.Task.ID] = c
	}
	id, ok := usage.SuggestPause(running)
	if !ok {
		return
	}
	t := byID[id].Task
	p := t.Priority
	if p < model.PriorityHigh || p > model.PriorityLow {
		p = model.PriorityNormal
	}
	label := fmt.Sprintf("%s (P%d)", t.ID, p)
	if repo := filepath.Base(t.Repo); t.Repo != "" && repo != "." && repo != "/" {
		label += " " + repo
	}
	rep.PauseTask, rep.PauseBusy = id, busyIDs[id]
	rep.PauseLabel = sanitize.Line(label, usageLabelLimit)
	rep.PauseTitle = sanitize.Line(t.Title, usageLabelLimit)
}

func runUsage(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	now := clock()
	rep, err := usageBuild(cmd.Context(), st, now)
	if err != nil {
		return err
	}
	if usageJSON {
		return usagePrintJSON(cmd.OutOrStdout(), rep)
	}
	return usagePrint(cmd.OutOrStdout(), rep, now)
}

// usagePrint is curia usage's text: the nothing-ingested line, or the
// window lines, the projection, the burn table and the suggestion.
func usagePrint(w io.Writer, rep usageReport, now time.Time) error {
	if !rep.Any {
		_, err := fmt.Fprintln(w, usageNothing)
		return err
	}
	if len(rep.Forecasts) == 0 && !rep.API && len(rep.Burn) == 0 {
		_, err := fmt.Fprintln(w, usageNoRecent)
		return err
	}
	for _, f := range rep.Forecasts {
		_, _ = fmt.Fprintln(w, usage.Line(f, now))
	}
	if rep.API {
		_, _ = fmt.Fprintf(w, "next 2h ≈ %s at the current rate\n", usage.CostCell(rep.APIUSD, true))
	}
	if len(rep.Burn) > 0 {
		_, _ = fmt.Fprintln(w, "Burn by session")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "SESSION:WINDOW\tTASK\t$/h\tCOST")
		for _, r := range rep.Burn {
			session := r.SessionID
			if r.Window != "" {
				session += ":" + r.Window
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", ui.Cell(session), ui.Cell(r.TaskID),
				ui.Cell(usage.CostCell(r.USDPerHour, true)), ui.Cell(usage.CostCell(r.CostUSD, r.CostKnown)))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	switch {
	case rep.PauseBusy:
		_, _ = fmt.Fprintf(w, "suggestion: pause %s when its agent is idle\n", ui.Cell(rep.PauseLabel))
	case rep.PauseTask != "":
		_, _ = fmt.Fprintf(w, "suggestion: pause %s: curia pause %s\n", ui.Cell(rep.PauseLabel), rep.PauseTask)
	}
	return nil
}

// usageJSONWindow and the types beside it are --json's shape (plan section 4.5).
type usageJSONWindow struct {
	Window   string  `json:"window"`
	UsedPct  float64 `json:"used_pct"`
	ResetsAt *string `json:"resets_at"`
	LimitAt  *string `json:"limit_at"`
	Warn     bool    `json:"warn"`
}

type usageJSONBurn struct {
	SessionID  string   `json:"session_id"`
	TaskID     string   `json:"task_id"`
	USDPerHour float64  `json:"usd_per_hour"`
	CostUSD    *float64 `json:"cost_usd"`
}

type usageJSONReport struct {
	Windows []usageJSONWindow `json:"windows"`
	API     *float64          `json:"api_projection_usd"`
	Burn    []usageJSONBurn   `json:"burn"`
	Suggest *string           `json:"suggest_pause"`
}

func usageTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func usagePrintJSON(w io.Writer, rep usageReport) error {
	out := usageJSONReport{Windows: []usageJSONWindow{}, Burn: []usageJSONBurn{}}
	for _, f := range rep.Forecasts {
		out.Windows = append(out.Windows, usageJSONWindow{
			Window: string(f.Window), UsedPct: f.UsedPct, ResetsAt: usageTime(f.ResetsAt), LimitAt: usageTime(f.LimitAt), Warn: f.Warn,
		})
	}
	if rep.API {
		out.API = &rep.APIUSD
	}
	for _, r := range rep.Burn {
		b := usageJSONBurn{SessionID: r.SessionID, TaskID: r.TaskID, USDPerHour: r.USDPerHour}
		if r.CostKnown {
			b.CostUSD = &r.CostUSD
		}
		out.Burn = append(out.Burn, b)
	}
	if rep.PauseTask != "" && !rep.PauseBusy {
		out.Suggest = &rep.PauseTask
	}
	return json.NewEncoder(w).Encode(out)
}

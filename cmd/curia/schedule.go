package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// `curia schedule` (F22, Ruling 151): a message sent later or repeatedly. The
// monitor fires it (schedule_fire.go). Creating, canceling, pausing and
// resuming are the user's only (userOnly); creating always confirms, with
// Enter, on a terminal, and there is no --yes.

// Flags of the create form; tests reset them.
var (
	scheduleAt, scheduleDaily, scheduleOn, scheduleRepo, scheduleTask, scheduleOnly string
	scheduleAll, scheduleIfIdle, scheduleJSON                                       bool
)

func scheduleResetFlags() {
	scheduleAt, scheduleDaily, scheduleOn, scheduleRepo, scheduleTask, scheduleOnly = "", "", "", "", "", ""
	scheduleAll, scheduleIfIdle, scheduleJSON = false, false, false
}

// scheduleShown bounds the recipients listed before the question.
const scheduleShown = 10

func init() {
	c := &cobra.Command{
		Use:   `schedule "<text>" (--at <time> | --daily HH:MM | --on reset) (--all | --repo <name> | --task T-42)`,
		Short: "Send a message later or repeatedly (the user only)",
		Long: "Creates a scheduled message. It prints the text, the trigger and the targets as they\n" +
			"resolve now, and waits for Enter; there is no --yes. Recipients are resolved again\n" +
			"when it fires, and it fires only while `curia monitor` runs. --at takes HH:MM (the next\n" +
			"one), YYYY-MM-DDTHH:MM or +2h (local time); --if-idle skips sessions that are not idle;\n" +
			"--only limited keeps sessions the usage limit stopped. Also: ls, show, cancel, pause, resume.",
		Args:        cobra.ExactArgs(1),
		Annotations: userOnlyMark(),
		RunE:        runScheduleCreate,
	}
	f := c.Flags()
	f.StringVar(&scheduleAt, "at", "", "fire once: HH:MM, YYYY-MM-DDTHH:MM or +2h")
	f.StringVar(&scheduleDaily, "daily", "", "fire every day at HH:MM")
	f.StringVar(&scheduleOn, "on", "", "fire when the usage window resets (the value is: reset)")
	f.BoolVar(&scheduleAll, "all", false, "target every live agent session")
	f.StringVar(&scheduleRepo, "repo", "", "target one repository's live sessions")
	f.StringVar(&scheduleTask, "task", "", "target one task's live session, e.g. T-42")
	f.BoolVar(&scheduleIfIdle, "if-idle", false, "deliver only to sessions that are idle when it fires")
	f.StringVar(&scheduleOnly, "only", "", "limited: only sessions the usage limit stopped")
	ls := &cobra.Command{Use: "ls", Short: "List the schedules", Args: cobra.NoArgs, RunE: runScheduleLs}
	ls.Flags().BoolVar(&scheduleJSON, "json", false, "print a JSON array instead of a table")
	show := &cobra.Command{Use: "show S-3", Short: "Show a schedule and its runs", Args: cobra.ExactArgs(1), RunE: runScheduleShow}
	c.AddCommand(ls, show,
		scheduleChange("cancel", "Cancel a schedule (the user only)", []string{"active", "paused"}, "canceled"),
		scheduleChange("pause", "Pause an active schedule (the user only)", []string{"active"}, "paused"),
		scheduleChange("resume", "Resume a paused schedule (the user only)", []string{"paused"}, "active"))
	rootCmd.AddCommand(c)
}

func scheduleChange(verb, short string, from []string, to string) *cobra.Command {
	return &cobra.Command{
		Use: verb + " S-3", Short: short, Args: cobra.ExactArgs(1), Annotations: userOnlyMark(),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := userOnly("schedule "+verb, verb+" a schedule"); err != nil {
				return err
			}
			return scheduleSet(cmd, args[0], verb, from, to)
		},
	}
}

// scheduleID reports whether id is S-<n>.
func scheduleID(id string) bool {
	n, ok := strings.CutPrefix(id, "S-")
	if !ok || n == "" || len(n) > 9 || n[0] == '0' {
		return false
	}
	return strings.Trim(n, "0123456789") == ""
}

func scheduleSet(cmd *cobra.Command, id, verb string, from []string, to string) error {
	if !scheduleID(id) {
		return fmt.Errorf("%q is not a schedule id (S-3)", sanitize.Line(id, 40))
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
	sc, found, err := st.GetSchedule(cmd.Context(), id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no schedule %s", id)
	}
	var next time.Time
	switch {
	case to == "active" && sc.Kind == "daily":
		next = scheduleNextDaily(sc.Spec, clock())
	case to == "active":
		next = sc.NextRun // an overdue one then follows the late rule at the next tick
	case to == "paused":
		next = sc.NextRun
	}
	if err = st.UpdateScheduleStatus(cmd.Context(), id, from, to, next); err != nil {
		if errors.Is(err, store.ErrScheduleState) {
			return fmt.Errorf("%s is %s: it cannot be %s", id, sc.Status, strings.TrimSuffix(to, "d")+"d")
		}
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", id, map[string]string{"cancel": "canceled", "pause": "paused", "resume": "resumed"}[verb])
	return err
}

// scheduleDraft checks the text, the target and the filters of the create form.
func scheduleDraft(raw string, now time.Time) (store.Schedule, error) {
	text, err := message.Normalize(raw)
	if err != nil {
		return store.Schedule{}, err
	}
	if n := len([]rune(text)); n > message.MaxText-40 { // room for the "(scheduled S-n) " label
		return store.Schedule{}, fmt.Errorf("a scheduled message is at most %d characters", message.MaxText-40)
	}
	target, err := scheduleParseTarget(scheduleAll, scheduleRepo, scheduleTask)
	if err != nil {
		return store.Schedule{}, err
	}
	if scheduleOnly != "" && scheduleOnly != "limited" {
		return store.Schedule{}, errors.New(`--only takes "limited"`)
	}
	return store.Schedule{Text: text, Target: target, IfIdle: scheduleIfIdle, OnlyLimited: scheduleOnly == "limited", CreatedVia: "cli", CreatedAt: now}, nil
}

func runScheduleCreate(cmd *cobra.Command, args []string) error {
	if err := userOnly("schedule", "schedule a message"); err != nil {
		return err
	}
	sc, err := scheduleDraft(args[0], clock())
	if err != nil {
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
	ctx := cmd.Context()
	when, err := scheduleTrigger(ctx, st, &sc, sc.CreatedAt, cfg.Schedule.ResetGrace)
	if err != nil {
		return err
	}
	if !sayIsTerminal() {
		return errors.New("a schedule always needs your confirmation, and there is no terminal to ask on; run it in a terminal")
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return err
	}
	rcpt := scheduleRecipients(sessions, sc.Target, cfg.Routing.Aliases)
	w := cmd.OutOrStdout()
	if !scheduleConfirm(cmd, sc, when, rcpt) {
		_, err = fmt.Fprintln(w, "\nnothing scheduled")
		return err
	}
	stored, err := st.CreateSchedule(ctx, sc, scheduleMaxActive)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "%s created; first run %s\n", stored.ID, stored.NextRun.Local().Format("2006-01-02 15:04"))
	if running, _ := monitorRunning(monitorLockPath(cfg)); !running {
		_, _ = fmt.Fprintln(w, "curia monitor is not running: nothing fires until it is (curia monitor start)")
	}
	return nil
}

// scheduleConfirm prints what is about to be scheduled, the recipients as
// they resolve now, and waits for Enter; anything else, or end of input,
// is a no.
func scheduleConfirm(cmd *cobra.Command, sc store.Schedule, when string, rcpt []model.Session) bool {
	w := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "message:  %s\nwhen:     %s\nto:       %s, %d sessions now (recipients are resolved again when it fires)\n",
		sanitize.Line(sc.Text, 200), when, sc.Target, len(rcpt))
	for _, s := range rcpt[:min(len(rcpt), scheduleShown)] {
		_, _ = fmt.Fprintf(w, "  %s\n", ui.Cell(sanitize.Line(cmp.Or(s.TaskID, s.ID)+" "+s.Repo, 120)))
	}
	if more := len(rcpt) - scheduleShown; more > 0 {
		_, _ = fmt.Fprintf(w, "  … and %d more\n", more)
	}
	if sc.IfIdle || sc.OnlyLimited {
		_, _ = fmt.Fprintf(w, "filters:  if-idle=%v only-limited=%v (checked when it fires)\n", sc.IfIdle, sc.OnlyLimited)
	}
	_, _ = fmt.Fprint(w, "Press Enter to create it; anything else cancels: ")
	line, err := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 64)).ReadString('\n')
	return err == nil && strings.TrimSpace(line) == ""
}

// scheduleTrigger reads the one trigger flag into sc (kind, spec, next run)
// and returns it in words.
func scheduleTrigger(ctx context.Context, st *store.Store, sc *store.Schedule, now time.Time, grace time.Duration) (string, error) {
	n := 0
	for _, set := range []bool{scheduleAt != "", scheduleDaily != "", scheduleOn != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return "", errors.New("name exactly one trigger: --at, --daily or --on reset")
	}
	var err error
	switch {
	case scheduleAt != "":
		sc.Kind = "at"
		if sc.NextRun, err = scheduleParseAt(scheduleAt, now); err != nil {
			return "", err
		}
		sc.Spec = sc.NextRun.Format(time.RFC3339)
		return "once at " + sc.NextRun.Local().Format("2006-01-02 15:04"), nil
	case scheduleDaily != "":
		sc.Kind = "daily"
		if sc.Spec, err = scheduleParseDaily(scheduleDaily); err != nil {
			return "", err
		}
		sc.NextRun = scheduleNextDaily(sc.Spec, now)
		return "every day at " + sc.Spec + " (next " + sc.NextRun.Local().Format("2006-01-02 15:04") + ")", nil
	}
	if scheduleOn != "reset" {
		return "", errors.New(`--on takes "reset"`)
	}
	usage, err := st.LatestUsage(ctx)
	if err != nil {
		return "", err
	}
	// The earliest reset still ahead, of the two windows' latest samples.
	for _, u := range usage {
		if u.ResetsAt.After(now) && (sc.NextRun.IsZero() || u.ResetsAt.Add(grace).Before(sc.NextRun)) {
			sc.NextRun = u.ResetsAt.Add(grace)
		}
	}
	if sc.NextRun.IsZero() {
		return "", errors.New("--on reset: no usage window with a reset time ahead is known yet (the status line reports it)")
	}
	sc.Kind = "on_reset"
	return "once, " + grace.String() + " after the usage window resets (" + sc.NextRun.Local().Format("2006-01-02 15:04") + ")", nil
}

// scheduleRow is one schedule as `ls --json` prints it.
type scheduleRow struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	Kind        string `json:"kind"`
	Spec        string `json:"spec"`
	Target      string `json:"target"`
	IfIdle      bool   `json:"if_idle"`
	OnlyLimited bool   `json:"only_limited"`
	Status      string `json:"status"`
	NextRunAt   string `json:"next_run_at"`
	LastOutcome string `json:"last_outcome"`
	CreatedVia  string `json:"created_via"`
	CreatedAt   string `json:"created_at"`
}

func scheduleRowOf(s store.Schedule) scheduleRow {
	next := ""
	if !s.NextRun.IsZero() {
		next = s.NextRun.UTC().Format(time.RFC3339)
	}
	return scheduleRow{
		ID: sanitize.Line(s.ID, 20), Text: sanitize.Line(s.Text, 200), Kind: s.Kind, Spec: sanitize.Line(s.Spec, 40),
		Target: sanitize.Line(s.Target, 80), IfIdle: s.IfIdle, OnlyLimited: s.OnlyLimited, Status: s.Status,
		NextRunAt: next, LastOutcome: s.LastOutcome, CreatedVia: s.CreatedVia, CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func runScheduleLs(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	list, err := st.ListSchedules(cmd.Context(), 200)
	if err != nil {
		return err
	}
	rows := make([]scheduleRow, 0, len(list)) // never nil: none is []
	for _, s := range list {
		rows = append(rows, scheduleRowOf(s))
	}
	w := cmd.OutOrStdout()
	if scheduleJSON {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err = enc.Encode(rows); err != nil {
			return err
		}
		_, err = io.WriteString(w, escapeUnprintable(buf.String()))
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tNEXT\tTARGET\tLAST\tTEXT")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Status, ui.Cell(r.NextRunAt), ui.Cell(r.Target), ui.Cell(r.LastOutcome), ui.Cell(sanitize.Line(r.Text, 60)))
	}
	return tw.Flush()
}

func runScheduleShow(cmd *cobra.Command, args []string) error {
	if !scheduleID(args[0]) {
		return fmt.Errorf("%q is not a schedule id (S-3)", sanitize.Line(args[0], 40))
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
	s, found, err := st.GetSchedule(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no schedule %s", args[0])
	}
	runs, err := st.ScheduleRuns(cmd.Context(), s.ID, 20)
	if err != nil {
		return err
	}
	r, w := scheduleRowOf(s), cmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "%s  %s\nmessage: %s\ntrigger: %s %s\ntarget:  %s (if-idle=%v only-limited=%v)\nnext:    %s\ncreated: %s via %s\n",
		r.ID, r.Status, r.Text, r.Kind, ui.Cell(r.Spec), r.Target, r.IfIdle, r.OnlyLimited, ui.Cell(r.NextRunAt), r.CreatedAt, r.CreatedVia)
	for _, run := range runs {
		_, _ = fmt.Fprintf(w, "run %s (due %s): %s %s\n", run.RanAt.UTC().Format(time.RFC3339), run.DueAt.UTC().Format(time.RFC3339), run.Outcome, sanitize.Line(run.Results, 300))
	}
	return nil
}

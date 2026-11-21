package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/ui"
	"github.com/ZachFara/curia/internal/usage"
)

// F29 auto-spawn (spec F29, Ruling 159, owner decisions D-7 and D-8): the
// monitor starts the next ready task, within caps, through the same
// lifecycle.SpawnTask as `curia spawn`.
//
// Why starting sessions from the monitor does not let an agent start agents:
// the monitor is the user's own background process, and everything it needs
// is something only the user can write. An agent cannot enable the setting
// (the key lives in the user file, which the hook denies agents to edit,
// `curia auto-spawn on` is user-only, and a repository file cannot enable
// it); cannot mark a task ready with a valid hash (ready_hash is written
// only by `curia task ready`, the board key, and the user's own
// level = ready setting through file_task: a web move, an agent's
// `curia spawn`/MCP call and every edit leave none); and cannot approve a
// setup script (`curia setup approve` is user-only, and an unapproved
// script makes the task not eligible). The monitor never types into a
// pane: lifecycle.SpawnTask only creates a window.

const (
	// autoEvery is the job's own tick (F29: 30 seconds), apart from the monitor's.
	autoEvery = 30 * time.Second
	// autoUsageLook bounds the usage samples read for the "window full" check.
	autoUsageLook = 8 * 24 * time.Hour
)

var (
	// autoSpawnFn is the spawn path; tests count its calls.
	autoSpawnFn = lifecycle.SpawnTask
	// autoRepoCfg is the auto-spawn setting in effect for a repository: the
	// user file tightened by the repository's own file. A resolution that
	// fails is "off".
	autoRepoCfg = func(repo string) config.AutoSpawn {
		home, err := os.UserHomeDir()
		if err != nil {
			return config.AutoSpawn{}
		}
		c, _, err := config.Resolve(config.Options{UserPath: configFile(home), RepoRoot: repo})
		if err != nil {
			return config.AutoSpawn{}
		}
		return c.Tasks.AutoSpawn
	}
)

// autoShared is what one pass reads once for every candidate.
type autoShared struct {
	live       int
	bypass     bool
	today      int
	last       time.Time
	warn, full bool
	memPct     float64
	memKnown   bool
}

func autoReadShared(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) (autoShared, error) {
	var s autoShared
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return s, err
	}
	for _, x := range sessions {
		switch x.State {
		case model.StateWorking, model.StateWaiting, model.StateNeedsDecision, model.StateIdle:
			s.live++
		}
	}
	if s.bypass, err = st.BypassActive(ctx, now); err != nil {
		return s, err
	}
	if s.today, s.last, err = st.AutoStats(ctx, now); err != nil {
		return s, err
	}
	samples, err := st.UsageReadings(ctx, now.Add(-autoUsageLook))
	if err != nil {
		return s, err
	}
	s.warn, s.full = autoUsage(samples, now, cfg.Schedule.LimitedPct)
	// Check 6 reads the monitor's newest VM sample; a stale or missing one, or the key at 0, skips it.
	if vm, ok, verr := st.LatestDockerVM(ctx); verr != nil {
		return s, verr
	} else if ok && vm.LimitMB > 0 && cfg.Resources.DockerMemoryRefusePct > 0 && usage.ResourceFresh(vm.At, now, cfg.Resources.Interval) {
		s.memPct, s.memKnown = float64(vm.UsedMB)*100/float64(vm.LimitMB), true
	}
	return s, nil
}

// autoUsage is check 5: a forecast warning, or a window whose latest sample
// is at or above limitedPct (schedule.limited_pct). The latest sample of a window whose
// resets_at has passed counts as 0.
func autoUsage(samples []model.UsageSample, now time.Time, limitedPct float64) (warn, full bool) {
	_, warn = usage.Warning(usage.Forecasts(samples, now), now)
	latest := map[model.UsageWindow]model.UsageSample{}
	for _, s := range samples {
		if p, ok := latest[s.Window]; !ok || s.At.After(p.At) || (s.At.Equal(p.At) && s.N > p.N) {
			latest[s.Window] = s
		}
	}
	for _, s := range latest {
		if (s.ResetsAt.IsZero() || s.ResetsAt.After(now)) && s.UsedPct >= limitedPct {
			full = true
		}
	}
	return warn, full
}

// autoRow is one ready task and why it may or may not start.
type autoRow struct {
	Card task.Card
	Cfg  config.AutoSpawn
	V    task.AutoVerdict
}

// autoRows judges every task whose derived status is ready, in the board's
// order for auto-spawn (priority, then age), with AutoEligible, the one rule.
func autoRows(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) ([]autoRow, autoShared, error) {
	cards, err := taskCards(ctx, st, now)
	if err != nil {
		return nil, autoShared{}, err
	}
	cards = slices.DeleteFunc(cards, func(c task.Card) bool { return c.Status != model.TaskReady })
	slices.SortStableFunc(cards, func(a, b task.Card) int {
		if pa, pb := autoPriority(a.Task), autoPriority(b.Task); pa != pb {
			return pa - pb
		}
		if c := a.Task.CreatedAt.Compare(b.Task.CreatedAt); c != 0 {
			return c
		}
		return int(a.Task.N - b.Task.N)
	})
	sh, err := autoReadShared(ctx, st, cfg, now)
	if err != nil {
		return nil, sh, err
	}
	blocking, err := st.OpenBlockingByTask(ctx, now)
	if err != nil {
		return nil, sh, err
	}
	rows := make([]autoRow, 0, len(cards))
	for _, c := range cards {
		rc := autoRepoCfg(c.Task.Repo)
		cand := task.AutoCand{Task: c.Task, Status: c.Status, OpenBlocking: blocking[c.Task.ID]}
		if c.Task.ReadyHash != "" {
			if cand.BudgetOver, cand.SetupUnapproved, err = autoTaskSkips(ctx, st, cfg, c.Task); err != nil {
				return nil, sh, err
			}
		}
		g := task.AutoGate{
			Now: now, Enabled: rc.Enabled && cfg.Tasks.AutoSpawn.Enabled, Bypass: sh.bypass,
			Live: sh.live, MaxAgents: rc.MaxAgents, StartedToday: sh.today, MaxPerDay: rc.MaxPerDay,
			LastStart: sh.last, Interval: rc.Interval, ForecastWarn: sh.warn, WindowFull: sh.full,
			MemKnown: sh.memKnown, MemPct: sh.memPct, MemRefusePct: float64(cfg.Resources.DockerMemoryRefusePct),
		}
		rows = append(rows, autoRow{c, rc, task.AutoEligible(g, cand)})
	}
	return rows, sh, nil
}

func autoPriority(t model.Task) int {
	if t.Priority == 0 {
		return model.PriorityNormal
	}
	return t.Priority
}

// autoTaskSkips reads the two per-task skips: a spent budget, and a
// repository whose .curia/setup.sh is not approved in its exact content (an
// unreadable one counts as not approved). Nothing is run or approved here.
func autoTaskSkips(ctx context.Context, st *store.Store, cfg config.Config, t model.Task) (over, unapproved bool, err error) {
	b, err := st.TaskBudget(ctx, t.ID, cfg.Cost.TaskBudgetDefault)
	if err != nil {
		return false, false, err
	}
	over = b.Level() == model.BudgetOver
	s, rerr := lifecycle.SetupRead(t.Repo)
	if rerr != nil {
		return over, !errors.Is(rerr, lifecycle.ErrSetupAbsent), nil // unreadable counts as not approved
	}
	approved, err := st.SetupApproval(ctx, t.Repo)
	return over, approved != s.Sum, err
}

// autoSpawn is the monitor job: on its own 30 second tick, and only when
// the setting is on in the file as it is now (read afresh, so `curia
// auto-spawn off` stops it at the next pass), it starts at most one task.
func (e *monitorEnv) autoSpawn(ctx context.Context) error {
	now := e.now()
	if now.Sub(e.lastAuto) < autoEvery {
		return nil
	}
	e.lastAuto = now
	cfg, err := loadConfig()
	if err != nil || !cfg.Tasks.AutoSpawn.Enabled {
		return err
	}
	rows, _, err := autoRows(ctx, e.st, cfg, now)
	if err != nil {
		return err
	}
	var refused *autoRow
	for i := range rows {
		switch r := &rows[i]; {
		case r.V.OK:
			return e.autoStart(ctx, cfg, *r, now)
		case r.V.Candidate && refused == nil:
			refused = r
		}
	}
	if refused != nil {
		_, err = e.st.LogAuto(ctx, store.AutoRun{
			At: now, Outcome: "refused", Check: refused.V.Check, TaskID: refused.Card.Task.ID,
			Detail: autoDetail(refused.V.Reason, refused.Card.Task),
		})
	}
	return err
}

// autoDetail is a log line: the reason, then the task's cleaned title.
func autoDetail(reason string, t model.Task) string {
	return reason + " (" + sanitize.Line(t.Title, 60) + ")"
}

// autoStart claims r's task with one UPDATE (so two monitors, or a restart,
// start it once), starts it through lifecycle.SpawnTask (a new session, own worktree
// and window, never a pane that exists), and logs the result. A failed
// spawn gives the claim back and is retried after the interval.
func (e *monitorEnv) autoStart(ctx context.Context, cfg config.Config, r autoRow, now time.Time) error {
	t := r.Card.Task
	ok, err := e.st.AutoClaim(ctx, t, now, store.AutoLimits{MaxPerDay: r.Cfg.MaxPerDay, Interval: r.Cfg.Interval})
	if err != nil || !ok {
		return err
	}
	// shortcut: a crash between the claim and the spawn leaves the task ready
	// with no hash and a start counted (nothing starts twice, nothing is logged),
	// and a long spawn (up to lifecycle.SpawnSetupLimit+2m) holds the monitor pass and the
	// jobs behind it; fix both with a claim row finished by the spawn if they bite.
	// A spawn may run an approved setup script for minutes: it gets its own
	// deadline, not the tick's.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycle.SpawnSetupLimit+2*time.Minute)
	defer cancel()
	res, err := autoSpawnFn(sctx, e.st, newTmuxClient(), cfg, t.ID, lifecycle.SpawnOptions{Wait: lifecycle.SpawnWait})
	if err != nil {
		rerr := e.st.AutoRelease(sctx, t.ID, now, t.AutoSpawnedAt, t.ReadyHash)
		_, lerr := e.st.LogAuto(sctx, store.AutoRun{
			At: now, Outcome: "refused", Check: "spawn_failed", TaskID: t.ID,
			Detail: autoDetail("the spawn failed: "+sanitize.Line(err.Error(), 200), t),
		})
		e.log.Warn("monitor: auto-spawn failed", "task", t.ID, "err", sanitize.Line(err.Error(), 300))
		if rerr != nil {
			return rerr
		}
		return lerr
	}
	_, err = e.st.LogAuto(sctx, store.AutoRun{
		At: now, Outcome: "fired", TaskID: t.ID, SessionID: res.Session.ID,
		Detail: autoDetail("started automatically: marked ready by you, text unchanged since, "+task.PriorityLabel(t.Priority), t),
	})
	return err
}

// autoHeader is the dashboard header's text and `status`'s first line.
func autoHeader(enabled bool, cfg config.AutoSpawn, live, today int, last *store.AutoRun) string {
	if !enabled {
		return "auto: off"
	}
	if last != nil && last.Outcome == "refused" {
		return "auto: waiting (" + last.Check + ")"
	}
	return fmt.Sprintf("auto: on %d/%d agents, %d/%d today", live, cfg.MaxAgents, today, cfg.MaxPerDay)
}

// ---- curia auto-spawn on | off | status ----

func init() {
	parent := &cobra.Command{
		Use:   "auto-spawn",
		Short: "Let the monitor start ready tasks by itself: on, off or status",
		Long: "curia auto-spawn status shows whether the monitor may start ready tasks, its caps, the last\n" +
			"ten starts and refusals, and for each ready task the first rule that keeps it from starting.\n" +
			"on and off set tasks.auto_spawn.enabled in your user file (with a revision); only the user may.",
	}
	on := &cobra.Command{
		Use: "on", Short: "Allow the monitor to start ready tasks (the user only)", Args: cobra.NoArgs,
		Annotations: userOnlyMark(), RunE: userOnlyRunE("auto-spawn on", "allow auto-spawn", func(c *cobra.Command, _ []string) error { return runAutoSet(c, "true") }),
	}
	off := &cobra.Command{
		Use: "off", Short: "Stop the monitor starting tasks; takes effect at its next pass (the user only)", Args: cobra.NoArgs,
		Annotations: userOnlyMark(), RunE: userOnlyRunE("auto-spawn off", "stop auto-spawn", func(c *cobra.Command, _ []string) error { return runAutoSet(c, "false") }),
	}
	status := &cobra.Command{Use: "status", Short: "The setting, the caps, the log and why each ready task waits", Args: cobra.NoArgs, RunE: runAutoStatus}
	parent.AddCommand(on, off, status)
	rootCmd.AddCommand(parent)
}

func runAutoSet(cmd *cobra.Command, value string) error {
	path, err := configUserPath()
	if err != nil {
		return err
	}
	st, err := configOpen(cmd.Context(), path, true)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	_, n, err := configSetKey(cmd.Context(), st, path, "tasks.auto_spawn.enabled", value, "cli")
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	a := cfg.Tasks.AutoSpawn
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "tasks.auto_spawn.enabled = %s (revision %d); at most %d agents at once, %d starts a day, %v apart\n",
		value, n, a.MaxAgents, a.MaxPerDay, a.Interval)
	return err
}

func runAutoStatus(cmd *cobra.Command, _ []string) error {
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
	rows, sh, err := autoRows(cmd.Context(), st, cfg, now)
	if err != nil {
		return err
	}
	runs, err := st.AutoRuns(cmd.Context(), 10)
	if err != nil {
		return err
	}
	var last *store.AutoRun
	if len(runs) > 0 {
		last = &runs[0]
	}
	return autoPrintStatus(cmd.OutOrStdout(), cfg.Tasks.AutoSpawn, sh, last, runs, rows)
}

func autoPrintStatus(w io.Writer, a config.AutoSpawn, sh autoShared, last *store.AutoRun, runs []store.AutoRun, rows []autoRow) error {
	var b strings.Builder
	b.WriteString(autoHeader(a.Enabled, a, sh.live, sh.today, last) + "\n")
	for _, r := range runs {
		fmt.Fprintf(&b, "  %s  %s %s %s: %s\n", r.At.Local().Format("01-02 15:04"), r.Outcome, ui.Cell(r.Check), ui.Cell(r.TaskID), ui.Cell(sanitize.Line(r.Detail, 200)))
	}
	if len(rows) > 0 {
		b.WriteString("ready tasks:\n")
	}
	for _, r := range rows {
		state := "waits: " + r.V.Reason
		if r.V.OK {
			state = "would start at the next pass"
		}
		fmt.Fprintf(&b, "  %s  %s  %s\n", ui.Cell(r.Card.Task.ID), ui.Cell(sanitize.Line(r.Card.Task.Title, 60)), state)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

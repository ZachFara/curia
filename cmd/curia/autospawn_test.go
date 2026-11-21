package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/tmux"
)

// autoEnable writes the user file with auto-spawn on (plus extra TOML under [tasks.auto_spawn]).
func autoEnable(t *testing.T, extra string) {
	t.Helper()
	autoUserFile(t, "[tasks.auto_spawn]\nenabled = true\n"+extra)
}

func autoUserFile(t *testing.T, body string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	p := configFile(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// autoEnv is a monitor environment on the fixture's store and clock.
func autoEnv(f *spawnFixture) *monitorEnv {
	return &monitorEnv{st: f.St, cfg: f.Cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return f.Now }}
}

// autoReady is a task the user marked ready at a terminal (with the hash).
func autoReady(f *spawnFixture, title string) model.Task {
	f.T.Helper()
	tk := f.Task(title, "body of "+title)
	f.Update(tk.ID, func(x *model.Task) { x.Status = model.TaskReady })
	if err := f.St.MarkReady(context.Background(), tk.ID); err != nil {
		f.T.Fatal(err)
	}
	return f.Get(tk.ID)
}

// autoPass runs one job pass, 31 seconds after the last, and returns the NewWindow calls so far.
func autoPass(t *testing.T, f *spawnFixture, e *monitorEnv) []string {
	t.Helper()
	f.Now = f.Now.Add(31 * time.Second)
	if err := e.autoSpawn(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f.Windows()
}

func autoRuns(t *testing.T, f *spawnFixture) []store.AutoRun {
	t.Helper()
	rs, err := f.St.AutoRuns(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestAutoSpawnDisabledByDefaultDoesNothing(t *testing.T) {
	f := spawnNewFixture(t)
	autoReady(f, "a")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows opened with the default setting: %q", w)
	}
	if rs := autoRuns(t, f); len(rs) != 0 {
		t.Errorf("log rows with auto-spawn off: %+v", rs)
	}
}

func TestAutoSpawnRepoFileCannotEnable(t *testing.T) {
	f := spawnNewFixture(t)
	autoReady(f, "a")
	testkit.WorktreeWrite(t, filepath.Join(f.Repo, ".curia", "config.toml"), "[tasks.auto_spawn]\nenabled = true\nmax_agents = 9\n")
	if got := autoRepoCfg(f.Repo); got.Enabled || got.MaxAgents != 3 {
		t.Fatalf("a repository file changed the setting: %+v", got)
	}
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows opened: %q", w)
	}
}

// With the setting on in the user file, a repository's own file turns it off for that repository.
func TestAutoSpawnRepoFileMayDisableItsRepo(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	autoReady(f, "a")
	testkit.WorktreeWrite(t, filepath.Join(f.Repo, ".curia", "config.toml"), "[tasks.auto_spawn]\nenabled = false\n")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows opened in a repository that turned it off: %q", w)
	}
}

func TestAutoSpawnStartsFirstByPriorityThenAgeThroughSpawn(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	a := autoReady(f, "older normal")
	f.Now = f.Now.Add(time.Minute)
	b := autoReady(f, "newer urgent")
	f.Update(b.ID, func(x *model.Task) { x.Priority = model.PriorityHigh })
	if err := f.St.MarkReady(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	web := f.Task("moved on the web", "x") // ready, no hash
	f.Update(web.ID, func(x *model.Task) { x.Status = model.TaskReady; x.Priority = model.PriorityHigh })

	w := autoPass(t, f, autoEnv(f))
	if len(w) != 1 || !strings.Contains(w[0], " "+b.ID+" ") {
		t.Fatalf("windows = %q, want exactly one, for %s", w, b.ID)
	}
	got := f.Get(b.ID)
	if got.AutoSpawnedAt.IsZero() || got.Worktree == "" {
		t.Errorf("task after start = %+v, want auto_spawned_at and a worktree (the spawn path)", got)
	}
	if f.Get(a.ID).AutoSpawnedAt != (time.Time{}) || f.Get(web.ID).AutoSpawnedAt != (time.Time{}) {
		t.Error("a task that was not chosen carries auto_spawned_at")
	}
	rs := autoRuns(t, f)
	if len(rs) != 1 || rs[0].Outcome != "fired" || rs[0].TaskID != b.ID || !strings.Contains(rs[0].Detail, "marked ready by you") {
		t.Errorf("log = %+v, want one fired row naming the task and why it was eligible", rs)
	}
}

// Two claimers of one task (two monitors, a restart) start it once.
func TestAutoSpawnTwoClaimersStartOnce(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	autoReady(f, "a")
	e1, e2 := autoEnv(f), autoEnv(f)
	cfg, _ := loadConfig()
	f.Now = f.Now.Add(time.Minute)
	rows, _, err := autoRows(context.Background(), f.St, cfg, f.Now)
	if err != nil || len(rows) != 1 || !rows[0].V.OK {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if err = e1.autoStart(context.Background(), cfg, rows[0], f.Now); err != nil {
		t.Fatal(err)
	}
	if err = e2.autoStart(context.Background(), cfg, rows[0], f.Now); err != nil {
		t.Fatal(err)
	}
	if w := f.Windows(); len(w) != 1 {
		t.Fatalf("windows = %q, want one start", w)
	}
}

// An agent-filed task (filed ready by the user's level) starts in a NEW session, and Curia types into no existing pane.
func TestAutoSpawnAgentFiledStartsNewSessionNeverTypesIntoAnotherPane(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "max_agents = 5\n")
	ctx := context.Background()
	for _, s := range []model.Session{
		{ID: "filer", Pane: "%1", State: model.StateIdle, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now, Repo: f.Repo},
		{ID: "lead", Pane: "%2", State: model.StateIdle, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now, Repo: f.Repo, TeamRole: model.RoleLead},
		{ID: "mate", Pane: "%3", State: model.StateIdle, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now, Repo: f.Repo, TeamRole: model.RoleTeammate},
	} {
		if err := f.St.UpsertSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	tk := model.Task{Title: "filed ready", Body: "b", Repo: f.Repo, BaseBranch: "main", Status: model.TaskReady, CreatedAt: f.Now, FiledBy: "filer", FiledReason: "found"}
	filed, err := f.St.FileTask(ctx, tk, 5)
	if err != nil {
		t.Fatal(err)
	}
	w := autoPass(t, f, autoEnv(f))
	if len(w) != 1 || !strings.Contains(w[0], " "+filed.ID+" ") {
		t.Fatalf("windows = %q, want one for %s", w, filed.ID)
	}
	for _, c := range f.Fake.Calls {
		if strings.HasPrefix(c, "SendKeys") {
			t.Errorf("auto-spawn typed into a pane: %q", c)
		}
	}
	if got := f.Get(filed.ID); got.Pane == "%1" || got.Pane == "%2" || got.Pane == "%3" {
		t.Errorf("the task was assigned to an existing pane %s", got.Pane)
	}
}

// An agent-filed task moved ready from anywhere but the user's terminal or level has no hash and is skipped.
func TestAutoSpawnWebMovedAndEditedAreSkippedAndNextStarts(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	ctx := context.Background()
	web := f.Task("web", "x")
	f.Update(web.ID, func(x *model.Task) { x.Status = model.TaskReady })
	edited := autoReady(f, "edited")
	f.Update(edited.ID, func(x *model.Task) { x.Body = "changed after ready" })
	good := autoReady(f, "good")
	w := autoPass(t, f, autoEnv(f))
	if len(w) != 1 || !strings.Contains(w[0], " "+good.ID+" ") {
		t.Fatalf("windows = %q, want only %s", w, good.ID)
	}
	cfg, _ := loadConfig()
	rows, _, err := autoRows(ctx, f.St, cfg, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	why := map[string]string{}
	for _, r := range rows {
		why[r.Card.Task.ID] = r.V.Check
	}
	if why[web.ID] != "no_hash" || why[edited.ID] != "edited" {
		t.Errorf("checks = %v, want no_hash and edited", why)
	}
}

func TestAutoSpawnUnapprovedSetupScriptIsSkippedAndNeverRun(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	f.CommitSetup("#!/bin/sh\ntouch RAN\n", true)
	testkit.GitRun(t, f.Repo, "checkout", "--", ".curia/setup.sh") // the checkout has the bit the index records
	tk := autoReady(f, "a")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows opened with an unapproved script: %q", w)
	}
	if _, err := os.Stat(f.SetupLog()); err == nil {
		t.Error("a setup log exists: the script ran")
	}
	rs := autoRuns(t, f)
	if len(rs) != 1 || rs[0].Check != "setup_unapproved" || rs[0].TaskID != tk.ID {
		t.Fatalf("log = %+v, want one refusal naming setup_unapproved", rs)
	}
	f.Approve("#!/bin/sh\ntouch RAN\n")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 1 {
		t.Fatalf("windows = %q after approval, want the task started", w)
	}
}

func TestAutoSpawnCapsPerTickIntervalAndDay(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "max_per_day = 2\n")
	for _, n := range []string{"a", "b", "c"} {
		autoReady(f, n)
	}
	e := autoEnv(f)
	if w := autoPass(t, f, e); len(w) != 1 {
		t.Fatalf("one pass opened %d windows, want 1 (one task per tick)", len(w))
	}
	// Within the 5 minute interval: refused once, and the same cause again writes no second row.
	for range 3 {
		if w := autoPass(t, f, e); len(w) != 1 {
			t.Fatalf("a start inside the interval: %q", w)
		}
	}
	rs := autoRuns(t, f)
	if len(rs) != 2 || rs[0].Check != "interval" || rs[1].Outcome != "fired" {
		t.Fatalf("log = %+v, want fired then one interval refusal", rs)
	}
	f.Now = f.Now.Add(6 * time.Minute)
	if w := autoPass(t, f, e); len(w) != 2 {
		t.Fatalf("windows = %q after the interval, want the second start", w)
	}
	f.Now = f.Now.Add(6 * time.Minute)
	if w := autoPass(t, f, e); len(w) != 2 {
		t.Fatalf("windows = %q, want no third start at max_per_day = 2", w)
	}
	if rs = autoRuns(t, f); rs[0].Check != "max_per_day" {
		t.Errorf("newest row = %+v, want a max_per_day refusal", rs[0])
	}
}

func TestAutoSpawnMaxAgentsCountsLiveSessions(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "max_agents = 2\n")
	for _, id := range []string{"s1", "s2"} {
		if err := f.St.UpsertSession(context.Background(), model.Session{ID: id, State: model.StateWorking, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now}); err != nil {
			t.Fatal(err)
		}
	}
	autoReady(f, "a")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows = %q at max_agents", w)
	}
	if rs := autoRuns(t, f); len(rs) != 1 || rs[0].Check != "max_agents" {
		t.Errorf("log = %+v, want one max_agents refusal", rs)
	}
}

func TestAutoSpawnBypassBlocks(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	if err := f.St.UpsertSession(context.Background(), model.Session{ID: "s1", State: model.StateEnded, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now}); err != nil {
		t.Fatal(err)
	}
	if err := f.St.UpsertSession(context.Background(), model.Session{ID: "s2", State: model.StateIdle, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now, Repo: "/other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.St.SetModes(context.Background(), store.ModeScope{Repo: "/other"}, model.ModeBypass, time.Time{}); err != nil {
		t.Fatal(err)
	}
	autoReady(f, "a")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows = %q while a session of any repository is in bypass", w)
	}
	if rs := autoRuns(t, f); len(rs) != 1 || rs[0].Check != "bypass" {
		t.Errorf("log = %+v, want one bypass refusal", rs)
	}
}

func TestAutoSpawnForecastAndResourcePressure(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	autoReady(f, "a")
	e := autoEnv(f)
	t.Run("a full window", func(t *testing.T) {
		statusExec(t, f.DB, `INSERT INTO usage_samples (at, window, used_pct, resets_at, session_id) VALUES (?, 'five_hour', 100, ?, 'x')`,
			f.Now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z"), f.Now.Add(2*time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z"))
		if w := autoPass(t, f, e); len(w) != 0 {
			t.Fatalf("windows = %q with a window at 100%%", w)
		}
		if rs := autoRuns(t, f); len(rs) != 1 || rs[0].Check != "forecast" {
			t.Fatalf("log = %+v, want one forecast refusal", rs)
		}
	})
	statusExec(t, f.DB, `DELETE FROM usage_samples`)
	// The Docker VM reading comes from the store, the threshold from resources.docker_memory_refuse_pct.
	vm := func(age time.Duration, usedMB int64) {
		if err := f.St.AddDockerVM(context.Background(), f.Now.Add(31*time.Second-age), usedMB, 1000); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("docker memory over the threshold with a fresh sample", func(t *testing.T) {
		vm(time.Second, 900)
		if w := autoPass(t, f, e); len(w) != 0 {
			t.Fatalf("windows = %q over the memory threshold", w)
		}
		if rs := autoRuns(t, f); rs[0].Check != "memory" {
			t.Fatalf("newest = %+v, want memory", rs[0])
		}
	})
	t.Run("key at 0 skips the check", func(t *testing.T) {
		autoUserFile(t, "[tasks.auto_spawn]\nenabled = true\n[resources]\ndocker_memory_refuse_pct = 0\n")
		vm(time.Second, 990)
		if w := autoPass(t, f, e); len(w) != 1 {
			t.Fatalf("windows = %q, want the start with the check off", w)
		}
	})
}

func TestAutoSpawnStaleDockerSampleSkipsTheCheck(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	autoReady(f, "a")
	if err := f.St.AddDockerVM(context.Background(), f.Now.Add(-time.Hour), 990, 1000); err != nil {
		t.Fatal(err)
	}
	if w := autoPass(t, f, autoEnv(f)); len(w) != 1 {
		t.Fatalf("windows = %q, want the start with a stale sample", w)
	}
}

// The latest sample of a window whose reset has passed counts as 0.
func TestAutoUsageWindowRules(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	full := model.UsageSample{N: 1, At: now.Add(-time.Hour), Window: model.WindowFiveHour, UsedPct: 100, ResetsAt: now.Add(time.Hour)}
	if _, f := autoUsage([]model.UsageSample{full}, now, 100); !f {
		t.Error("a window at 100% before its reset is not full")
	}
	past := full
	past.ResetsAt = now.Add(-time.Minute)
	if _, f := autoUsage([]model.UsageSample{past}, now, 100); f {
		t.Error("a sample of a window that has reset still counts")
	}
	half := full
	half.UsedPct = 60
	if _, f := autoUsage([]model.UsageSample{half}, now, 50); !f {
		t.Error("schedule.limited_pct = 50 ignored: 60% is not full")
	}
	if _, f := autoUsage([]model.UsageSample{half}, now, 100); f {
		t.Error("60% counts as full at limited_pct = 100")
	}
	older := full
	older.At, older.UsedPct = now.Add(-2*time.Hour), 10
	newer := full
	newer.N, newer.At, newer.UsedPct = 2, now.Add(-30*time.Minute), 40
	if _, f := autoUsage([]model.UsageSample{full, older, newer}, now, 100); f {
		t.Error("an older reading beat the newest")
	}
}

func TestAutoSpawnBudgetSpentIsSkippedAndNextStarts(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	spent := autoReady(f, "spent")
	f.Update(spent.ID, func(x *model.Task) { x.BudgetUSD = 1 })
	if err := f.St.MarkReady(context.Background(), spent.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.St.UpsertSession(context.Background(), model.Session{ID: "old", State: model.StateEnded, TaskID: spent.ID, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now}); err != nil {
		t.Fatal(err)
	}
	statusExec(t, f.DB, `UPDATE sessions SET cost_usd = 2 WHERE id = 'old'`)
	next := autoReady(f, "next")
	w := autoPass(t, f, autoEnv(f))
	if len(w) != 1 || !strings.Contains(w[0], " "+next.ID+" ") {
		t.Fatalf("windows = %q, want only %s", w, next.ID)
	}
}

// `curia auto-spawn off` stops it at the next pass; hostile task text never reaches the output raw.
func TestAutoSpawnKillSwitchAndCleanedOutput(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	hostile := "evil \x1b]0;pwn\x07\nFORGED: auto-spawn fired T-99"
	tk := f.Task(hostile, "b")
	f.Update(tk.ID, func(x *model.Task) { x.Status = model.TaskReady })
	if err := f.St.MarkReady(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	f.Update(tk.ID, func(x *model.Task) { x.Title += " edited" }) // refused as edited: it shows in status
	out, err := runRoot(t, "auto-spawn", "off")
	if err != nil || !strings.Contains(out, "tasks.auto_spawn.enabled = false") {
		t.Fatalf("auto-spawn off = %q, %v", out, err)
	}
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows = %q after auto-spawn off", w)
	}
	out, err = runRoot(t, "auto-spawn", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "auto: off\n") {
		t.Errorf("status = %q, want it to start with auto: off", out)
	}
	if strings.ContainsAny(out, "\x1b\x07") || strings.Contains(out, "\nFORGED") {
		t.Errorf("status shows raw task text: %q", out)
	}
	if !strings.Contains(out, "waits:") {
		t.Errorf("status = %q, want the reason a ready task waits", out)
	}
}

func TestAutoSpawnStatusExplainsAndHeaderShowsCounts(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	edited := autoReady(f, "edited")
	f.Update(edited.ID, func(x *model.Task) { x.Body = "other" })
	out, err := runRoot(t, "auto-spawn", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "auto: on 0/3 agents, 0/5 today\n") || !strings.Contains(out, "changed after it was marked ready") {
		t.Errorf("status = %q", out)
	}
	if got := autoHeader(true, config.AutoSpawn{MaxAgents: 3, MaxPerDay: 5}, 2, 1, &store.AutoRun{Outcome: "refused", Check: "forecast"}); got != "auto: waiting (forecast)" {
		t.Errorf("header = %q", got)
	}
}

// A failed spawn changes nothing and is retried only after the interval.
func TestAutoSpawnFailedSpawnGivesClaimBackAndWaitsTheInterval(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	tk := autoReady(f, "a")
	calls := 0
	prev := autoSpawnFn
	autoSpawnFn = func(context.Context, *store.Store, tmux.Client, config.Config, string, lifecycle.SpawnOptions) (lifecycle.SpawnResult, error) {
		calls++
		return lifecycle.SpawnResult{}, os.ErrPermission
	}
	t.Cleanup(func() { autoSpawnFn = prev })
	e := autoEnv(f)
	autoPass(t, f, e)
	got := f.Get(tk.ID)
	if calls != 1 || got.ReadyHash != tk.ReadyHash || !got.AutoSpawnedAt.IsZero() {
		t.Fatalf("after a failed spawn: calls %d, hash %q, auto_spawned_at %v; want the claim given back", calls, got.ReadyHash, got.AutoSpawnedAt)
	}
	autoPass(t, f, e)
	if calls != 1 {
		t.Errorf("retried inside the interval (%d calls)", calls)
	}
	f.Now = f.Now.Add(6 * time.Minute)
	autoPass(t, f, e)
	if calls != 2 {
		t.Errorf("calls = %d, want a retry after the interval", calls)
	}
	if rs := autoRuns(t, f); len(rs) == 0 || rs[0].Check != "spawn_failed" {
		t.Errorf("log = %+v, want spawn_failed", rs)
	}
}

// An agent cannot enable the key, mark a task ready, or approve a setup script.
func TestAutoSpawnAgentCannotEnableMarkReadyOrApproveSetup(t *testing.T) {
	for _, argv := range [][]string{{"auto-spawn", "on"}, {"task", "ready", "T-1"}, {"setup", "approve", "T-1"}} {
		userOnlyRefused(t, argv, "CLAUDECODE", "only the user may")
	}
	f := spawnNewFixture(t)
	tk := f.Task("a", "b")
	userTerminalAs(t, false)
	t.Setenv("CLAUDECODE", "1")
	for _, argv := range [][]string{{"auto-spawn", "on"}, {"task", "ready", tk.ID}} {
		if _, err := runRoot(t, argv...); err == nil {
			t.Errorf("%v ran for an agent", argv)
		}
	}
	if got := f.Get(tk.ID); got.ReadyHash != "" || got.Status != model.TaskBacklog {
		t.Errorf("an agent changed the task: %+v", got)
	}
	if cfg, _ := loadConfig(); cfg.Tasks.AutoSpawn.Enabled {
		t.Error("an agent enabled auto-spawn")
	}
}

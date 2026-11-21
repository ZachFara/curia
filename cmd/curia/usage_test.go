package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// usageSeries stores five_hour samples (the spec example when pcts is
// 50, 56, 62) at 15, 8 and 1 minutes before fleetNow, all resetting at
// reset, through store.Ingest, as an ingest would.
func usageSeries(t *testing.T, db string, reset time.Time, pcts ...float64) {
	t.Helper()
	agos := []time.Duration{15 * time.Minute, 8 * time.Minute, time.Minute}
	if len(pcts) != len(agos) {
		t.Fatalf("usageSeries wants %d percentages", len(agos))
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	usageFeedSession(t, st, "feed")
	for i, p := range pcts {
		at := fleetNow.Add(-agos[i])
		r := store.Reading{SessionID: "feed", At: at, Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: p, ResetsAt: reset}}}
		if _, err := st.Ingest(context.Background(), r, at); err != nil {
			t.Fatal(err)
		}
	}
}

// usageFeedSession stores the ended session whose ingests carry a test's
// usage readings: a reading is stored only for a session that exists.
func usageFeedSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	sess := model.Session{ID: id, State: model.StateEnded, UpdatedAt: fleetNow.Add(-24 * time.Hour)}
	if err := st.UpsertSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
}

// usageCost stores cost readings for session id: 10 dollars 20 minutes before
// fleetNow and cost1 one minute before.
func usageCost(t *testing.T, db, id string, cost1 float64) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, c := range []struct {
		ago  time.Duration
		cost float64
	}{{20 * time.Minute, 10}, {time.Minute, cost1}} {
		at, cost := fleetNow.Add(-c.ago), c.cost
		if _, err := st.Ingest(context.Background(), store.Reading{SessionID: id, At: at, CostUSD: &cost}, at); err != nil {
			t.Fatal(err)
		}
	}
}

// usageTask stores a ready task of repo with priority and links a working
// session sid (tmux window win) to it, so the task derives as running.
func usageTask(t *testing.T, db, repo string, priority int, sid, win string) string {
	t.Helper()
	return usageTaskState(t, db, repo, priority, sid, win, model.StateIdle)
}

func usageTaskState(t *testing.T, db, repo string, priority int, sid, win string, state model.State) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	tk, err := st.CreateTask(context.Background(), model.Task{
		Title: "Fix " + sid, Repo: repo, Status: model.TaskReady, Priority: priority, CreatedAt: fleetNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	seedSessions(t, db, model.Session{
		ID: sid, Agent: "claude", State: state, Pane: "%1", TmuxWindow: win, TaskID: tk.ID,
		Repo: repo, UpdatedAt: fleetNow.Add(-time.Minute),
	})
	return tk.ID
}

func usageWarningHome(t *testing.T) string {
	t.Helper()
	db, _ := fleetHome(t, nil)
	usageSeries(t, db, fleetNow.Add(110*time.Minute), 50, 56, 62)
	return db
}

func TestUsageSubscription(t *testing.T) {
	db := usageWarningHome(t)
	t1 := usageTask(t, db, "/src/web", 2, "sa", "w1")
	t2 := usageTask(t, db, "/src/api", 3, "sb", "w2")
	usageCost(t, db, "sa", 11.5)
	usageCost(t, db, "sb", 10.3)
	if t1 != "T-1" || t2 != "T-2" {
		t.Fatalf("task ids %s %s", t1, t2)
	}
	out, err := runRoot(t, "usage")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"5h 62% · resets 1h50m · burning 51%/h → limit ~43m",
		"Burn by session",
		"suggestion: pause T-2 (P3) api: curia pause T-2",
	}
	last := -1
	for _, w := range want {
		i := strings.Index(out, w)
		if i < 0 || i < last {
			t.Fatalf("output lacks %q in order:\n%s", w, out)
		}
		last = i
	}
	if strings.Index(out, "sa:w1") > strings.Index(out, "sb:w2") || !strings.Contains(out, "$4.50") || !strings.Contains(out, "$0.90") {
		t.Errorf("burn table is not highest first with $/h:\n%s", out)
	}
	if strings.Contains(out, "next 2h") {
		t.Errorf("a subscription shows no API projection:\n%s", out)
	}
}

func TestUsageNoSuggestionWithoutWarning(t *testing.T) {
	db, _ := fleetHome(t, nil)
	usageSeries(t, db, fleetNow.Add(20*time.Minute), 50, 56, 62) // resets first
	usageTask(t, db, "/src/api", 3, "sb", "w2")
	usageCost(t, db, "sb", 12)
	out, err := runRoot(t, "usage")
	if err != nil || strings.Contains(out, "suggestion") || !strings.Contains(out, "5h 62% · resets 20m · burning 51%/h") {
		t.Errorf("usage = %q, %v, want the window and no suggestion", out, err)
	}
	if strings.Contains(out, "limit ~") {
		t.Errorf("a burn that resets first names no limit: %q", out)
	}
}

func TestUsageSteady(t *testing.T) {
	db, _ := fleetHome(t, nil)
	usageSeries(t, db, fleetNow.Add(time.Hour), 40, 40, 40)
	if out, err := runRoot(t, "usage"); err != nil || !strings.Contains(out, "5h 40% · resets 1h0m · steady") {
		t.Errorf("usage = %q, %v, want a steady line", out, err)
	}
}

func TestUsageAPIMode(t *testing.T) {
	db, _ := fleetHome(t, nil)
	usageTask(t, db, "/src/api", 2, "sb", "w2")
	usageCost(t, db, "sb", 11.5) // 1.5 in 20 minutes, x6 = 9
	out, err := runRoot(t, "usage")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "next 2h ≈ $9.00 at the current rate") || strings.Contains(out, "5h ") {
		t.Errorf("usage = %q, want the projection and no window line", out)
	}
	if !strings.Contains(out, "Burn by session") || strings.Contains(out, "suggestion") {
		t.Errorf("usage = %q, want the burn table and no suggestion", out)
	}
}

func TestUsageNothingIngested(t *testing.T) {
	fleetHome(t, nil)
	out, err := runRoot(t, "usage")
	want := "no usage has been ingested; see curia install (status line) or cost-lens\n"
	if err != nil || out != want {
		t.Errorf("usage = %q, %v, want %q", out, err, want)
	}
}

func TestUsageNothingRecent(t *testing.T) {
	db, _ := fleetHome(t, nil)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	at := fleetNow.Add(-3 * time.Hour)
	usageFeedSession(t, st, "feed")
	r := store.Reading{SessionID: "feed", At: at, Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: 5}}}
	if _, err = st.Ingest(context.Background(), r, at); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	out, err := runRoot(t, "usage")
	if err != nil || strings.Contains(out, "no usage has been ingested") || !strings.Contains(out, "last 20 minutes") {
		t.Errorf("usage = %q, %v, want a line saying nothing came in the last 20 minutes", out, err)
	}
}

func TestUsageJSON(t *testing.T) {
	db := usageWarningHome(t)
	usageTask(t, db, "/src/api", 3, "sb", "w2")
	usageCost(t, db, "sb", 12)
	out, err := runRoot(t, "usage", "--json")
	t.Cleanup(func() { usageJSON = false })
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Windows []map[string]any `json:"windows"`
		API     *float64         `json:"api_projection_usd"`
		Burn    []map[string]any `json:"burn"`
		Suggest *string          `json:"suggest_pause"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	usageCheckWindow(t, got.Windows)
	if got.API != nil || got.Suggest == nil || *got.Suggest != "T-1" {
		t.Errorf("api %v suggest %v", got.API, got.Suggest)
	}
	if len(got.Burn) != 1 || got.Burn[0]["session_id"] != "sb" || got.Burn[0]["task_id"] != "T-1" ||
		got.Burn[0]["usd_per_hour"] != 6.0 || got.Burn[0]["cost_usd"] != 12.0 {
		t.Errorf("burn = %v", got.Burn)
	}
}

func TestUsageJSONNulls(t *testing.T) {
	db, _ := fleetHome(t, nil)
	usageSeries(t, db, time.Time{}, 40, 40, 40) // no reset known
	out, err := runRoot(t, "usage", "--json")
	t.Cleanup(func() { usageJSON = false })
	want := `{"windows":[{"window":"five_hour","used_pct":40,"resets_at":null,"limit_at":null,"warn":false}],` +
		`"api_projection_usd":null,"burn":[],"suggest_pause":null}`
	if err != nil || strings.TrimSpace(out) != want {
		t.Errorf("usage --json = %q, %v, want %q", out, err, want)
	}
}

func TestUsageJSONEmpty(t *testing.T) {
	fleetHome(t, nil)
	out, err := runRoot(t, "usage", "--json")
	t.Cleanup(func() { usageJSON = false })
	want := `{"windows":[],"api_projection_usd":null,"burn":[],"suggest_pause":null}`
	if err != nil || strings.TrimSpace(out) != want {
		t.Errorf("usage --json = %q, %v, want %q", out, err, want)
	}
}

func TestUsageBurnTableHasTenRows(t *testing.T) {
	db, _ := fleetHome(t, nil)
	for i := range 12 {
		id := "s" + string(rune('a'+i))
		seedSessions(t, db, model.Session{ID: id, State: model.StateWorking, UpdatedAt: fleetNow})
		usageCost(t, db, id, 10+float64(i+1))
	}
	out, err := runRoot(t, "usage")
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "s") {
			rows++
		}
	}
	if rows != 10 {
		t.Errorf("%d rows, want 10:\n%s", rows, out)
	}
	if !strings.Contains(out, "sl") || strings.Contains(out, "sa ") {
		t.Errorf("not the ten highest burns:\n%s", out)
	}
}

func TestUsageTableIsSanitized(t *testing.T) {
	db, _ := fleetHome(t, nil)
	usageSeries(t, db, fleetNow.Add(110*time.Minute), 50, 56, 62)
	usageTask(t, db, "/src/\x1b[31mevil\x1b]0;pwned\x07", 3, "sb", "win\x1b[2Jdow")
	usageCost(t, db, "sb", 12)
	out, err := runRoot(t, "usage")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\x07") || strings.Contains(out, "pwned") && strings.Contains(out, "]0;") {
		t.Errorf("an escape sequence reached stdout: %q", out)
	}
	if !strings.Contains(out, "suggestion: pause T-1 (P3) ") {
		t.Errorf("usage = %q", out)
	}
}

func TestUsageAllowedInsideAgent(t *testing.T) {
	usageWarningHome(t)
	fake := useFakeTmux(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("TMUX_PANE", "%1")
	userTerminalAs(t, false)
	out, err := runRoot(t, "usage")
	if err != nil || !strings.Contains(out, "5h 62%") {
		t.Errorf("usage inside an agent = %q, %v, want it printed", out, err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls %q, want none", fake.Calls)
	}
}

func TestUsageBuildWritesNothing(t *testing.T) {
	db := usageWarningHome(t)
	usageTask(t, db, "/src/api", 3, "sb", "w2")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	before, _ := st.UsageSince(context.Background(), time.Time{})
	rep, err := usageBuild(context.Background(), st, fleetNow)
	if err != nil || !rep.Any || !rep.Warn || rep.Warning.Window != model.WindowFiveHour {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if after, _ := st.UsageSince(context.Background(), time.Time{}); len(after) != len(before) {
		t.Errorf("usageBuild changed the samples")
	}
}

func usageCheckWindow(t *testing.T, windows []map[string]any) {
	t.Helper()
	if len(windows) != 1 || windows[0]["window"] != "five_hour" || windows[0]["used_pct"] != 62.0 ||
		windows[0]["warn"] != true || windows[0]["resets_at"] != "2026-11-11T13:50:00Z" {
		t.Fatalf("windows = %v", windows)
	}
	if limit, _ := windows[0]["limit_at"].(string); !strings.HasPrefix(limit, "2026-11-11T12:43:") {
		t.Errorf("limit_at = %v", windows[0]["limit_at"])
	}
}

// The suggestion names a task pause can act on now (not mid-turn) among the
// lowest-priority candidates; when none can be paused yet it still names the
// one it would suggest, marked busy, and P and the pause command are not
// offered. A stale id is still refused as before.
func TestUsagePauseSuggestionNeedsIdleAgent(t *testing.T) {
	db := usageWarningHome(t)
	usageTaskState(t, db, "/src/web", 3, "sa", "w1", model.StateIdle)
	usageTaskState(t, db, "/src/api", 3, "sb", "w2", model.StateWorking)
	usageCost(t, db, "sa", 10.3)
	usageCost(t, db, "sb", 11.5) // the higher burn, but mid-turn
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	rep, err := usageBuild(ctx, st, clock())
	if err != nil || rep.PauseTask != "T-1" || rep.PauseBusy {
		t.Fatalf("suggestion = %q busy %v, %v; want the idle T-1", rep.PauseTask, rep.PauseBusy, err)
	}
	src := storeSource{st: st, tm: &tmux.Fake{}, cfg: config.Default()}
	if _, err = src.Pause(ctx, "T-2"); err == nil || !strings.Contains(err.Error(), "suggestion changed") {
		t.Errorf("Pause of a task no longer suggested = %v, want the refusal", err)
	}

	db = usageWarningHome(t)
	usageTaskState(t, db, "/src/web", 3, "sa", "w1", model.StateWorking)
	usageTaskState(t, db, "/src/api", 3, "sb", "w2", model.StateWorking)
	usageCost(t, db, "sa", 10.3)
	usageCost(t, db, "sb", 11.5)
	out, err := runRoot(t, "usage")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "T-2 (P3) api") || !strings.Contains(out, "when its agent is idle") || strings.Contains(out, "curia pause") {
		t.Errorf("usage with every candidate mid-turn:\n%s\nwant the would-be T-2, the idle wait and no pause command", out)
	}
	st2, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	b, err := storeSource{st: st2, tm: &tmux.Fake{}, cfg: config.Default()}.Budget(ctx)
	if err != nil || b.PauseTask != "T-2" || !b.PauseBusy {
		t.Errorf("Budget = %+v, %v; want T-2 marked busy", b, err)
	}
}

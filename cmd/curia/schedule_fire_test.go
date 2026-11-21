package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/team"
	"github.com/ZachFara/curia/internal/tmux"
)

// schedNow is the clock of the firing tests (readHome's clock is at(12, 0)).
var schedNow = at(12, 0)

// schedFixture is a monitor env over a store with the given sessions, each
// live in its own pane of fake. A second env on the same database (env2) has
// a fake of its own.
func schedFixture(t *testing.T, sessions ...model.Session) (env, env2 *monitorEnv, fake *tmux.Fake) {
	t.Helper()
	panes := make([]string, len(sessions))
	for i, s := range sessions {
		panes[i] = s.Pane
	}
	db, fake := readHome(t, panes, sessions...)
	mk := func(f *tmux.Fake) *monitorEnv {
		st, err := store.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return &monitorEnv{
			st: st, cfg: config.Default(), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			now: func() time.Time { return schedNow }, tmuxFor: func(string) tmux.Client { return f },
		}
	}
	other := &tmux.Fake{Panes: fake.Panes, Screen: fake.Screen}
	return mk(fake), mk(other), fake
}

func schedAdd(t *testing.T, st *store.Store, mut func(*store.Schedule)) store.Schedule {
	t.Helper()
	sc := store.Schedule{Text: "continue", Kind: "at", Target: "all", NextRun: schedNow.Add(-time.Minute), CreatedAt: at(9, 0)}
	sc.Spec = sc.NextRun.Format(time.RFC3339)
	if mut != nil {
		mut(&sc)
	}
	out, err := st.CreateSchedule(context.Background(), sc, 50)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func schedMessages(t *testing.T, st *store.Store) []model.Message {
	t.Helper()
	ms, err := st.ListMessages(context.Background(), store.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func schedTargets(ms []model.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.TargetSession)
	}
	return out
}

func schedResults(t *testing.T, st *store.Store, id string) map[string]string {
	t.Helper()
	runs, err := st.ScheduleRuns(context.Background(), id, 5)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v; want one", runs, err)
	}
	var r struct{ Sessions map[string]string }
	if err = json.Unmarshal([]byte(runs[0].Results), &r); err != nil {
		t.Fatal(err)
	}
	if r.Sessions == nil {
		r.Sessions = map[string]string{}
	}
	r.Sessions["outcome"] = runs[0].Outcome
	return r.Sessions
}

func TestScheduleFiresOnceWithTwoMonitors(t *testing.T) {
	e1, e2, fake := schedFixture(t, readSession("a", model.StateIdle, "%1"), readSession("b", model.StateIdle, "%2"))
	sc := schedAdd(t, e1.st, nil)
	var wg sync.WaitGroup
	for _, e := range []*monitorEnv{e1, e2} {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.fireSchedules(context.Background()) }()
	}
	wg.Wait()
	ms := schedMessages(t, e1.st)
	if len(ms) != 2 {
		t.Fatalf("messages = %d, want 2 (one per session, once)", len(ms))
	}
	for _, m := range ms {
		if m.Sender != model.SenderUser || m.Kind != model.KindInstruction || !strings.Contains(m.Text, sc.ID) || m.BroadcastID == "" {
			t.Errorf("message = %+v, want a user instruction naming %s in one broadcast", m, sc.ID)
		}
	}
	if typed := sendTyped(fake); len(typed) > 2 {
		t.Errorf("typed %v, want at most one line per session", typed)
	}
	e1.tickAt(t)
	if n := len(schedMessages(t, e1.st)); n != 2 {
		t.Errorf("a second tick fired again: %d messages", n)
	}
}

// tickAt runs the schedule job once more (a later tick of the same monitor).
func (e *monitorEnv) tickAt(t *testing.T) {
	t.Helper()
	if err := e.fireSchedules(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestScheduleLateWindow(t *testing.T) {
	for _, c := range []struct {
		late    time.Duration
		outcome string
		msgs    int
	}{{29 * time.Minute, "fired", 1}, {31 * time.Minute, "skipped_late", 0}} {
		t.Run(c.outcome, func(t *testing.T) {
			e, _, _ := schedFixture(t, readSession("a", model.StateIdle, "%1"))
			sc := schedAdd(t, e.st, func(s *store.Schedule) { s.NextRun = schedNow.Add(-c.late) })
			e.tickAt(t)
			if got := len(schedMessages(t, e.st)); got != c.msgs {
				t.Errorf("messages = %d, want %d", got, c.msgs)
			}
			if r := schedResults(t, e.st, sc.ID); r["outcome"] != c.outcome {
				t.Errorf("outcome = %q, want %q", r["outcome"], c.outcome)
			}
			if got, _, _ := e.st.GetSchedule(context.Background(), sc.ID); got.Status != "done" {
				t.Errorf("a one-time schedule is %q after its run, want done", got.Status)
			}
		})
	}
}

func TestDailyScheduleMissedWaitsForTomorrow(t *testing.T) {
	e, _, _ := schedFixture(t, readSession("a", model.StateIdle, "%1"))
	sc := schedAdd(t, e.st, func(s *store.Schedule) {
		s.Kind, s.Spec, s.NextRun = "daily", "02:00", schedNow.Add(-5*time.Hour)
	})
	e.tickAt(t)
	got, _, _ := e.st.GetSchedule(context.Background(), sc.ID)
	if got.Status != "active" || !got.NextRun.After(schedNow) || got.LastOutcome != "skipped_late" || len(schedMessages(t, e.st)) != 0 {
		t.Errorf("schedule = %+v, want active, next run in the future, skipped_late, nothing sent", got)
	}
}

func TestCanceledSchedulesDoNotFire(t *testing.T) {
	e, _, _ := schedFixture(t, readSession("a", model.StateIdle, "%1"))
	sc := schedAdd(t, e.st, nil)
	if err := e.st.UpdateScheduleStatus(context.Background(), sc.ID, []string{"active"}, "canceled", time.Time{}); err != nil {
		t.Fatal(err)
	}
	e.tickAt(t)
	if n := len(schedMessages(t, e.st)); n != 0 {
		t.Errorf("a canceled schedule sent %d messages", n)
	}
}

// Recipients are resolved when the schedule fires: a session that ended since
// creation is skipped, one that started since is included.
func TestScheduleResolvesRecipientsAtFiring(t *testing.T) {
	ended := readSession("old", model.StateEnded, "%1")
	e, _, _ := schedFixture(t, readSession("a", model.StateIdle, "%2"), ended, readSession("new", model.StateIdle, "%3"))
	mate := readSession("mate", model.StateIdle, "%4")
	if err := e.st.UpsertSession(context.Background(), mate); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.SetTeams(context.Background(), map[string]team.Membership{"mate": {TeamID: "tm", Role: model.RoleTeammate, Member: "m"}}); err != nil {
		t.Fatal(err)
	}
	schedAdd(t, e.st, nil)
	e.tickAt(t)
	got := schedTargets(schedMessages(t, e.st))
	slices.Sort(got)
	if !slices.Equal(got, []string{"a", "new"}) {
		t.Errorf("targets = %v, want a and new only", got)
	}
}

func TestScheduleTargetsRepoAndTask(t *testing.T) {
	api, web, tk := readSession("api", model.StateIdle, "%1"), readSession("web", model.StateIdle, "%2"), readSession("tk", model.StateIdle, "%3")
	api.Repo, web.Repo, tk.Repo, tk.TaskID = "/work/api-server", "/work/web", "/work/web", "T-7"
	e, _, _ := schedFixture(t, api, web, tk)
	schedAdd(t, e.st, func(s *store.Schedule) { s.Target = "repo:API-Server" })
	schedAdd(t, e.st, func(s *store.Schedule) { s.Target = "task:T-7" })
	e.tickAt(t)
	got := schedTargets(schedMessages(t, e.st))
	if len(got) != 2 || got[0] != "api" || got[1] != "tk" {
		t.Errorf("targets = %v, want api (repo) then tk (task)", got)
	}
}

func TestScheduleNoTargetIsRecorded(t *testing.T) {
	e, _, _ := schedFixture(t, readSession("old", model.StateEnded, "%1"))
	sc := schedAdd(t, e.st, nil)
	e.tickAt(t)
	if r := schedResults(t, e.st, sc.ID); r["outcome"] != "skipped_no_target" {
		t.Errorf("outcome = %q, want skipped_no_target", r["outcome"])
	}
}

func TestScheduleIfIdleSkipsBusy(t *testing.T) {
	e, _, _ := schedFixture(t, readSession("idle", model.StateIdle, "%1"), readSession("busy", model.StateWorking, "%2"))
	sc := schedAdd(t, e.st, func(s *store.Schedule) { s.IfIdle = true })
	e.tickAt(t)
	if got := schedTargets(schedMessages(t, e.st)); len(got) != 1 || got[0] != "idle" {
		t.Errorf("targets = %v, want idle only", got)
	}
	if r := schedResults(t, e.st, sc.ID); r["busy"] != "skipped_busy" || r["outcome"] != "fired" {
		t.Errorf("results = %v", r)
	}
}

// --only limited is the derived rule: a window's latest sample is at or above
// schedule.limited_pct and the session went idle after it.
func TestScheduleOnlyLimitedFromUsageSamples(t *testing.T) {
	for _, c := range []struct {
		pct  float64
		want int
	}{{100, 1}, {99, 0}} {
		t.Run(fmt.Sprintf("pct%v", c.pct), func(t *testing.T) {
			stopped := readSession("stopped", model.StateIdle, "%1") // idle since 11:00
			working := readSession("working", model.StateWorking, "%2")
			e, _, _ := schedFixture(t, stopped, working)
			reset := schedNow.Add(time.Hour)
			if _, err := e.st.Ingest(context.Background(), store.Reading{
				SessionID: "stopped", At: at(10, 30),
				Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: c.pct, ResetsAt: reset}},
			}, schedNow); err != nil {
				t.Fatal(err)
			}
			sc := schedAdd(t, e.st, func(s *store.Schedule) { s.OnlyLimited = true })
			e.tickAt(t)
			if got := len(schedMessages(t, e.st)); got != c.want {
				t.Errorf("messages = %d, want %d", got, c.want)
			}
			if r := schedResults(t, e.st, sc.ID); c.want == 0 && (r["stopped"] != "skipped_not_limited" || r["working"] != "skipped_not_limited") {
				t.Errorf("results = %v, want skipped_not_limited", r)
			}
		})
	}
}

// A session that went idle BEFORE the sample was not stopped by it.
func TestScheduleOnlyLimitedNeedsIdleAfterTheSample(t *testing.T) {
	idleEarly := readSession("early", model.StateIdle, "%1")
	idleEarly.UpdatedAt = at(9, 0)
	e, _, _ := schedFixture(t, idleEarly)
	if _, err := e.st.Ingest(context.Background(), store.Reading{
		SessionID: "early", At: at(10, 30),
		Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: 100, ResetsAt: schedNow.Add(time.Hour)}},
	}, schedNow); err != nil {
		t.Fatal(err)
	}
	schedAdd(t, e.st, func(s *store.Schedule) { s.OnlyLimited = true })
	e.tickAt(t)
	if n := len(schedMessages(t, e.st)); n != 0 {
		t.Errorf("sent %d, want 0 (idle before the limit sample)", n)
	}
}

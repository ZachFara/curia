package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/store"
)

// An agent controls the denial rate: one session's rows stop at the cap, other sessions are unaffected, and the summary read stays small.
func TestPermissionEventsCappedPerSession(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	ev := func(sess string) store.PermissionEvent {
		return store.PermissionEvent{SessionID: sess, Mode: "away", Tool: "Bash", Summary: "rm -rf x", Verdict: "deny", At: autoT}
	}
	for range 10000 {
		if err := s.AddPermissionEvent(ctx, ev("flood")); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := s.AddPermissionEvent(ctx, ev("calm")); err != nil {
			t.Fatal(err)
		}
	}
	flood, _ := s.PermissionEvents(ctx, "flood")
	calm, _ := s.PermissionEvents(ctx, "calm")
	if len(flood) != store.PermissionEventCap || len(calm) != 3 {
		t.Fatalf("rows: flood %d (want %d), calm %d (want 3)", len(flood), store.PermissionEventCap, len(calm))
	}
	start := time.Now()
	groups, total, capped, err := s.Denials(ctx, autoT.Add(-time.Hour), autoT.Add(time.Hour))
	t.Logf("Denials over %d rows took %v", total, time.Since(start))
	if err != nil || len(groups) != 2 || total != store.PermissionEventCap+3 || !capped {
		t.Fatalf("groups %+v total %d capped %v err %v", groups, total, capped, err)
	}
	if groups[0].SessionID != "flood" || groups[0].Count != store.PermissionEventCap || groups[1].Count != 3 {
		t.Errorf("groups = %+v, want the flood group first at the cap", groups)
	}
}

// Rows older than the retention go; recent rows stay.
func TestPrunePermissionEventsByAge(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for _, at := range []time.Time{autoT.Add(-store.PermissionEventKeep - time.Hour), autoT} {
		if err := s.AddPermissionEvent(ctx, store.PermissionEvent{SessionID: "a", Mode: "away", Tool: "Bash", Summary: "x", Verdict: "deny", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PruneHistory(ctx, autoT); err != nil {
		t.Fatal(err)
	}
	if evs, _ := s.PermissionEvents(ctx, "a"); len(evs) != 1 {
		t.Fatalf("rows after prune = %d, want 1", len(evs))
	}
}

// The auto log and the schedule runs drop rows past their age.
func TestPruneAutoLogAndScheduleRunsByAge(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	old := autoT.Add(-store.HistoryKeep - time.Hour)
	for _, at := range []time.Time{old, autoT} {
		if _, err := s.LogAuto(ctx, store.AutoRun{At: at, Outcome: "fired", TaskID: "T", Detail: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	sc := newSchedule(t, s, old)
	if _, ok, err := s.ClaimSchedule(ctx, sc, autoT, "fired", old); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	sc.NextRun = autoT
	if _, ok, err := s.ClaimSchedule(ctx, sc, time.Time{}, "fired", autoT); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := s.PruneHistory(ctx, autoT); err != nil {
		t.Fatal(err)
	}
	if runs, _ := s.AutoRuns(ctx, 10); len(runs) != 1 {
		t.Errorf("auto runs after prune = %d, want 1", len(runs))
	}
	if runs, _ := s.ScheduleRuns(ctx, sc.ID, 10); len(runs) != 1 {
		t.Errorf("schedule runs after prune = %d, want 1", len(runs))
	}
}

func TestPruneConfigRevisionsKeepsBaselineAndNewest(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	old := autoT.Add(-store.HistoryKeep - time.Hour)
	var err error
	if _, err = s.AddConfigRevision(ctx, store.ConfigRevision{At: old, Actor: "baseline", Key: "*", File: "# mine\n"}); err != nil {
		t.Fatal(err)
	}
	for range store.ConfigRevisionsKeep + 5 {
		if _, err = s.AddConfigRevision(ctx, store.ConfigRevision{At: autoT, Actor: "cli", Key: "k", File: "x = 1\n"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.PruneHistory(ctx, autoT); err != nil {
		t.Fatal(err)
	}
	revs, _ := s.ConfigRevisions(ctx, 1000)
	base := 0
	for _, r := range revs {
		if r.Actor == "baseline" {
			base++
		}
	}
	if len(revs) != store.ConfigRevisionsKeep+1 || base != 1 {
		t.Errorf("revisions after prune = %d (baseline %d), want the newest %d plus the baseline", len(revs), base, store.ConfigRevisionsKeep)
	}
}

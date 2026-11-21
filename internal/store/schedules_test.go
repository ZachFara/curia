package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/store"
)

var schedBase = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func newSchedule(t *testing.T, s *store.Store, due time.Time) store.Schedule {
	t.Helper()
	sc, err := s.CreateSchedule(context.Background(), store.Schedule{
		Text: "continue", Kind: "at", Spec: due.Format(time.RFC3339), Target: "all", NextRun: due, CreatedAt: schedBase,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// Two claimers (two monitors, or a restart) of one due schedule: exactly one wins.
func TestClaimScheduleOnceWithTwoClaimers(t *testing.T) {
	s := open(t, dbPath(t))
	sc := newSchedule(t, s, schedBase)
	var wins, errs int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.ClaimSchedule(context.Background(), sc, time.Time{}, "fired", schedBase)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
			} else if ok {
				wins++
			}
		}()
	}
	wg.Wait()
	if wins != 1 || errs != 0 {
		t.Fatalf("wins = %d, errors = %d; want one winner and no error", wins, errs)
	}
	got, _, _ := s.GetSchedule(context.Background(), sc.ID)
	if got.Status != "done" || !got.NextRun.IsZero() {
		t.Errorf("after a one-time fire: %+v, want done with no next run", got)
	}
	if due, _ := s.DueSchedules(context.Background(), schedBase.Add(time.Hour)); len(due) != 0 {
		t.Errorf("due after the claim = %v", due)
	}
}

func TestScheduleLifecycleAndCap(t *testing.T) {
	ctx := context.Background()
	s := open(t, dbPath(t))
	a := newSchedule(t, s, schedBase.Add(time.Hour))
	if a.ID != "S-1" || a.Status != "active" {
		t.Fatalf("first schedule = %+v", a)
	}
	if due, _ := s.DueSchedules(ctx, schedBase); len(due) != 0 {
		t.Errorf("not due yet, got %v", due)
	}
	if err := s.UpdateScheduleStatus(ctx, a.ID, []string{"active"}, "paused", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueSchedules(ctx, schedBase.Add(2*time.Hour)); len(due) != 0 {
		t.Errorf("a paused schedule is due: %v", due)
	}
	if err := s.UpdateScheduleStatus(ctx, a.ID, []string{"paused", "active"}, "canceled", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateScheduleStatus(ctx, a.ID, []string{"paused", "active"}, "canceled", time.Time{}); !errors.Is(err, store.ErrScheduleState) {
		t.Errorf("canceling a canceled schedule = %v, want ErrScheduleState", err)
	}
	if due, _ := s.DueSchedules(ctx, schedBase.Add(48*time.Hour)); len(due) != 0 {
		t.Errorf("a canceled schedule is due: %v", due)
	}
	// The cap counts active and paused ones, not canceled.
	for range 2 {
		newSchedule(t, s, schedBase.Add(time.Hour))
	}
	if _, err := s.CreateSchedule(ctx, store.Schedule{Text: "x", Kind: "at", Target: "all", NextRun: schedBase, CreatedAt: schedBase}, 2); !errors.Is(err, store.ErrTooManySchedules) {
		t.Errorf("third active schedule under cap 2 = %v", err)
	}
}

func TestScheduleRunsAndUsage(t *testing.T) {
	ctx := context.Background()
	s := open(t, dbPath(t))
	sc := newSchedule(t, s, schedBase)
	id, ok, err := s.ClaimSchedule(ctx, sc, time.Time{}, "fired", schedBase.Add(time.Minute))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = s.FinishScheduleRun(ctx, id, "skipped_no_target", `{"sessions":{}}`); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ScheduleRuns(ctx, sc.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].Outcome != "skipped_no_target" || !runs[0].DueAt.Equal(schedBase) || !runs[0].RanAt.Equal(schedBase.Add(time.Minute)) {
		t.Errorf("runs = %+v, %v", runs, err)
	}
	list, _ := s.ListSchedules(ctx, 10)
	if len(list) != 1 || list[0].LastOutcome != "skipped_no_target" {
		t.Errorf("list = %+v", list)
	}
}

func TestScheduleRunsAreOnTheTimeline(t *testing.T) {
	ctx := context.Background()
	s := open(t, dbPath(t))
	sc := newSchedule(t, s, schedBase)
	if _, ok, err := s.ClaimSchedule(ctx, sc, time.Time{}, "skipped_late", schedBase.Add(time.Minute)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	evs, err := s.Timeline(ctx, store.TimelineQuery{})
	if err != nil || len(evs) != 1 || evs[0].Kind != "schedule_run" || evs[0].Line != "S-1 skipped_late" {
		t.Errorf("timeline = %+v, %v", evs, err)
	}
}

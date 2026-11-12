package store_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestClaimNotifySlotOnePerWindow(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	ctx := context.Background()
	const every = 30 * time.Second
	const goroutines, rounds = 20, 10
	for round := range rounds {
		now := t0.Add(time.Duration(round) * time.Hour)
		got := make([]bool, goroutines)
		errs := make([]error, goroutines)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := range goroutines {
			wg.Go(func() {
				<-start
				// One statement: SQLITE_BUSY means it wrote nothing.
				errs[g] = retryBusy(func() error {
					var err error
					got[g], err = hs[g%len(hs)].ClaimNotifySlot(ctx, now, every)
					return err
				}, isBusy)
			})
		}
		close(start)
		wg.Wait()
		won := 0
		for g := range goroutines {
			if errs[g] != nil {
				t.Fatalf("round %d goroutine %d: %v", round, g, errs[g])
			}
			if got[g] {
				won++
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d claims won the slot at one instant, want 1", round, won)
		}
	}

	now := t0.Add(time.Duration(rounds) * time.Hour)
	for _, c := range []struct {
		at    time.Time
		every time.Duration
		want  bool
	}{
		{now, every, true},
		{now.Add(29 * time.Second), every, false},
		{now.Add(30 * time.Second), every, true},
		{now.Add(59 * time.Second), every, false},
		{now.Add(60 * time.Second), every, true},
		// The stored time (now+60s) is in the future of the next call.
		{now.Add(80 * time.Second), every, false},
		// With every <= 0 every call claims, but not before a future stored time.
		{now.Add(time.Minute), 0, true},
		{now.Add(time.Minute), 0, true},
		{now.Add(30 * time.Second), 0, false},
		{now.Add(30 * time.Second), -time.Minute, true},
	} {
		got, err := hs[0].ClaimNotifySlot(ctx, c.at, c.every)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("ClaimNotifySlot(now+%v, every %v) = %v, want %v", c.at.Sub(now), c.every, got, c.want)
		}
	}
}

func TestPruneEventsBatches(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	db := rawDB(t, path)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	old := t0.Add(-48 * time.Hour)
	for i := range 2510 {
		at := old.Add(time.Duration(i) * time.Second)
		if i >= 2500 {
			at = t0.Add(time.Duration(i) * time.Second)
		}
		if _, err := tx.Exec(`INSERT INTO events (session_id, kind, at) VALUES ('s1', 'Stop', ?)`,
			at.UTC().Format("2006-01-02T15:04:05.000000000Z")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cutoff := t0.Add(-time.Hour)
	if n, err := s.PruneEvents(ctx, cutoff, 0); err != nil || n != 0 {
		t.Errorf("PruneEvents(limit 0) = %d, %v; want 0, nil", n, err)
	}
	var got []int64
	for range 4 {
		n, err := s.PruneEvents(ctx, cutoff, 1000)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if want := []int64{1000, 1000, 500, 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
	var left, newest int
	if err := db.QueryRow(`SELECT count(*), COALESCE(SUM(at >= ?), 0) FROM events`,
		t0.UTC().Format("2006-01-02T15:04:05.000000000Z")).Scan(&left, &newest); err != nil {
		t.Fatal(err)
	}
	if left != 10 || newest != 10 {
		t.Errorf("%d events left, %d of them new; want the 10 new ones", left, newest)
	}
}

package store_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
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

// restoreClaim runs ClaimRestore and requires it not to fail.
func restoreClaim(t *testing.T, s *store.Store, now time.Time, hold time.Duration) bool {
	t.Helper()
	got, err := s.ClaimRestore(context.Background(), now, hold)
	if err != nil {
		t.Fatalf("ClaimRestore(%v): %v", now, err)
	}
	return got
}

func TestClaimRestoreOneHolder(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 3)
	const hold = 2 * time.Minute
	if !restoreClaim(t, hs[0], t0, hold) {
		t.Fatal("first claim = false, want true")
	}
	if restoreClaim(t, hs[1], t0.Add(time.Second), hold) {
		t.Error("second claim while held = true, want false")
	}
	if restoreClaim(t, hs[1], t0.Add(hold-1), hold) {
		t.Error("claim just before hold has passed = true, want false")
	}
	// The first holder crashed: after hold its claim is taken over.
	if !restoreClaim(t, hs[1], t0.Add(hold), hold) {
		t.Error("claim after hold has passed = false, want true")
	}
	if restoreClaim(t, hs[2], t0.Add(hold+time.Second), hold) {
		t.Error("claim right after a takeover = true, want false")
	}
	if err := hs[1].ReleaseRestore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !restoreClaim(t, hs[2], t0.Add(hold+2*time.Second), hold) {
		t.Error("claim right after ReleaseRestore = false, want true")
	}
	// A clock set back cannot steal the claim.
	if restoreClaim(t, hs[0], t0, hold) {
		t.Error("claim at an earlier time = true, want false")
	}
}

func TestRestoreReleaseKeepsNewerHolder(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 3)
	ctx := context.Background()
	const hold = 2 * time.Minute
	if !restoreClaim(t, hs[0], t0, hold) {
		t.Fatal("first claim = false")
	}
	if !restoreClaim(t, hs[1], t0.Add(hold), hold) {
		t.Fatal("takeover = false")
	}
	// The slow first holder finishes late: its release must not free the
	// claim the second holder now has.
	if err := hs[0].ReleaseRestore(ctx); err != nil {
		t.Fatal(err)
	}
	if restoreClaim(t, hs[2], t0.Add(hold+time.Second), hold) {
		t.Error("a stale holder's release freed the newer claim")
	}
	// A handle that never held the claim releases nothing either.
	if err := hs[2].ReleaseRestore(ctx); err != nil {
		t.Fatal(err)
	}
	if restoreClaim(t, hs[2], t0.Add(hold+2*time.Second), hold) {
		t.Error("a non-holder's release freed the claim")
	}
	if err := hs[1].ReleaseRestore(ctx); err != nil {
		t.Fatal(err)
	}
	if !restoreClaim(t, hs[2], t0.Add(hold+3*time.Second), hold) {
		t.Error("the holder's own release did not free the claim")
	}
	// Releasing twice is harmless.
	if err := hs[1].ReleaseRestore(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreClaimRace(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	ctx := context.Background()
	const goroutines = 20
	for round := range 10 {
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
					got[g], err = hs[g%len(hs)].ClaimRestore(ctx, now, 2*time.Minute)
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
			t.Fatalf("round %d: %d restores claimed at one instant, want 1", round, won)
		}
	}
}

func TestRestoreClaimRefusesNoHold(t *testing.T) {
	s := open(t, dbPath(t))
	for _, hold := range []time.Duration{0, -time.Minute} {
		if got, err := s.ClaimRestore(context.Background(), t0, hold); err == nil || got {
			t.Errorf("ClaimRestore(hold %v) = %v, %v; want false and an error", hold, got, err)
		}
	}
	if !restoreClaim(t, s, t0, time.Minute) {
		t.Error("a refused claim left something behind")
	}
}

func TestRestoreCandidatesRule(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	since := t0.Add(-24 * time.Hour)
	for _, c := range []struct {
		id     string
		state  model.State
		reason string
		at     time.Time
	}{
		{"working", model.StateWorking, "", t0},
		{"idle", model.StateIdle, "", t0.Add(-time.Hour)},
		{"stale", model.StateStale, "", t0.Add(-2 * time.Hour)},
		{"waiting", model.StateWaiting, "", t0.Add(-3 * time.Hour)},
		{"needs", model.StateNeedsDecision, "", t0.Add(-4 * time.Hour)},
		{"tmux-killed", model.StateEnded, "other", t0.Add(-30 * time.Minute)},
		{"odd-reason", model.StateEnded, "Not A Reason!", t0.Add(-40 * time.Minute)}, // stored as other
		{"user-exit", model.StateEnded, "prompt_input_exit", t0.Add(-time.Minute)},
		{"logout", model.StateEnded, "logout", t0.Add(-time.Minute)},
		{"clear", model.StateEnded, "clear", t0.Add(-time.Minute)},
		{"no-reason", model.StateEnded, "", t0.Add(-time.Minute)},
		{"old-working", model.StateWorking, "", since.Add(-time.Nanosecond)},
		{"old-killed", model.StateEnded, "other", since.Add(-time.Hour)},
		{"edge", model.StateIdle, "", since},
		{"no-time", model.StateIdle, "", time.Time{}},
	} {
		upsert(t, s, model.Session{ID: c.id, Agent: "claude", State: c.state, EndReason: c.reason, UpdatedAt: c.at})
	}
	ids := func(limit int) []string {
		t.Helper()
		got, err := s.RestoreCandidates(ctx, since, limit)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got))
		for _, sess := range got {
			out = append(out, sess.ID)
		}
		return out
	}
	want := []string{"working", "tmux-killed", "odd-reason", "idle", "stale", "waiting", "needs", "edge"}
	if got := ids(100); !reflect.DeepEqual(got, want) {
		t.Errorf("RestoreCandidates = %v\nwant %v (most recently active first)", got, want)
	}
	if got := ids(3); !reflect.DeepEqual(got, want[:3]) {
		t.Errorf("RestoreCandidates(limit 3) = %v, want %v", got, want[:3])
	}
	if got := ids(0); len(got) != 0 {
		t.Errorf("RestoreCandidates(limit 0) = %v, want none", got)
	}
	// A crashed session that came back is a candidate as what it is now.
	if err := s.UpdateSession(ctx, "tmux-killed", func(cur model.Session, _ bool) model.Session {
		cur.State = model.StateIdle
		return cur
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RestoreCandidates(ctx, since, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range got {
		if sess.ID == "tmux-killed" && (sess.State != model.StateIdle || sess.EndReason != "") {
			t.Errorf("revived session read back as %s / %q", sess.State, sess.EndReason)
		}
	}
	t.Logf("RestoreCandidates plan:\n%s", queryPlan(t, rawDB(t, path), store.RestoreCandidatesQuery,
		"2026-11-14T09:00:00.000000000Z", 32))
}

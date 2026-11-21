package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

var autoT = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

var autoLim = store.AutoLimits{MaxPerDay: 5, Interval: 5 * time.Minute}

func readyTask(t *testing.T, s *store.Store, title string) model.Task {
	t.Helper()
	tk := newTask(title)
	tk.Status = model.TaskReady
	tk = createTask(t, s, tk)
	if err := s.MarkReady(context.Background(), tk.ID); err != nil {
		t.Fatal(err)
	}
	return getTask(t, s, tk.ID)
}

// MarkReady stores the hash of the text; leaving ready clears it; a task
// that became ready any other way has none.
func TestMarkReadyHashLifecycle(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	tk := readyTask(t, s, "a")
	if tk.ReadyHash != model.ReadyHash(tk.Title, tk.Body) {
		t.Fatalf("ReadyHash = %q, want the hash of the text", tk.ReadyHash)
	}
	plain := newTask("moved by hand")
	plain.Status = model.TaskReady
	if got := createTask(t, s, plain); got.ReadyHash != "" {
		t.Errorf("a task created ready has hash %q, want none", got.ReadyHash)
	}
	if err := s.MarkReady(ctx, createTask(t, s, newTask("backlog")).ID); err == nil {
		t.Error("MarkReady of a backlog task succeeded")
	}
	if _, err := s.UpdateTask(ctx, tk.ID, func(c model.Task) (model.Task, error) { c.Status = model.TaskBacklog; return c, nil }); err != nil {
		t.Fatal(err)
	}
	if got := getTask(t, s, tk.ID); got.ReadyHash != "" {
		t.Errorf("hash after leaving ready = %q, want cleared", got.ReadyHash)
	}
}

// A launch (any spawn) consumes the readiness: an ended session must not be restarted by the monitor.
func TestLaunchClearsReadyHash(t *testing.T) {
	s := open(t, dbPath(t))
	tk := readyTask(t, s, "a")
	if _, err := s.ClaimLaunch(context.Background(), tk.ID, "", autoT, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := getTask(t, s, tk.ID); got.ReadyHash != "" {
		t.Errorf("hash after a launch claim = %q, want cleared", got.ReadyHash)
	}
}

func TestFileTaskReadyCarriesHash(t *testing.T) {
	s := open(t, dbPath(t))
	f := filed("filed ready")
	f.Status = model.TaskReady
	got, err := s.FileTask(context.Background(), f, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReadyHash != model.ReadyHash(got.Title, got.Body) {
		t.Errorf("a task filed ready (level = ready) has hash %q, want the hash of its text", got.ReadyHash)
	}
	b := filed("filed backlog")
	if got, err = s.FileTask(context.Background(), b, 5); err != nil || got.ReadyHash != "" {
		t.Errorf("a task filed to backlog: hash %q err %v, want none", got.ReadyHash, err)
	}
}

// Two claimers (two monitors, or a restart) start a task once.
func TestAutoClaimOnce(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	tk := readyTask(t, s, "a")
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.AutoClaim(ctx, tk.ID, tk.ReadyHash, autoT, autoLim)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d claimers won, want 1", won)
	}
	if got := getTask(t, s, tk.ID); got.AutoSpawnedAt.IsZero() {
		t.Error("the claim did not record auto_spawned_at")
	}
}

// The claim itself holds the spacing and the daily cap, so a second monitor cannot slip past them.
func TestAutoClaimHoldsIntervalAndDailyCap(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	a, b := readyTask(t, s, "a"), readyTask(t, s, "b")
	if ok, _ := s.AutoClaim(ctx, a.ID, a.ReadyHash, autoT, autoLim); !ok {
		t.Fatal("first claim refused")
	}
	if ok, _ := s.AutoClaim(ctx, b.ID, b.ReadyHash, autoT.Add(4*time.Minute), autoLim); ok {
		t.Error("a claim inside the interval won")
	}
	if ok, _ := s.AutoClaim(ctx, b.ID, "stale-hash", autoT.Add(6*time.Minute), autoLim); ok {
		t.Error("a claim with a hash that is not the stored one won")
	}
	if ok, _ := s.AutoClaim(ctx, b.ID, b.ReadyHash, autoT.Add(6*time.Minute), store.AutoLimits{MaxPerDay: 1, Interval: time.Minute}); ok {
		t.Error("a claim at the daily cap won")
	}
	if ok, _ := s.AutoClaim(ctx, b.ID, b.ReadyHash, autoT.Add(6*time.Minute), autoLim); !ok {
		t.Error("a claim after the interval and under the cap was refused")
	}
}

// A failed spawn changes nothing: release restores the hash and clears the claim.
func TestAutoReleaseRestores(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	tk := readyTask(t, s, "a")
	if ok, _ := s.AutoClaim(ctx, tk.ID, tk.ReadyHash, autoT, autoLim); !ok {
		t.Fatal("claim refused")
	}
	if err := s.AutoRelease(ctx, tk.ID, autoT, time.Time{}, tk.ReadyHash); err != nil {
		t.Fatal(err)
	}
	got := getTask(t, s, tk.ID)
	if got.ReadyHash != tk.ReadyHash || !got.AutoSpawnedAt.IsZero() {
		t.Errorf("after release: hash %q at %v, want the original and none", got.ReadyHash, got.AutoSpawnedAt)
	}
}

// A refusal is logged only when it differs from the previous row; a start always is.
func TestLogAutoDedupesRefusals(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	ref := store.AutoRun{At: autoT, Outcome: "refused", Check: "forecast", TaskID: "T-1", Detail: "x"}
	for i, want := range []bool{true, false, true} {
		if i == 2 {
			ref.Check = "memory"
		}
		ref.At = autoT.Add(time.Duration(i) * time.Minute)
		got, err := s.LogAuto(ctx, ref)
		if err != nil || got != want {
			t.Fatalf("LogAuto #%d = %v, %v; want %v", i, got, err, want)
		}
	}
	fired := store.AutoRun{At: autoT.Add(time.Hour), Outcome: "fired", TaskID: "T-1", SessionID: "s1"}
	for range 2 {
		if ok, _ := s.LogAuto(ctx, fired); !ok {
			t.Error("a fired row was deduplicated")
		}
	}
	rows, err := s.AutoRuns(ctx, 10)
	if err != nil || len(rows) != 4 || rows[0].Outcome != "fired" {
		t.Fatalf("AutoRuns = %+v, %v; want 4 rows, newest first", rows, err)
	}
}

func TestBypassActive(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	upsert(t, s, model.Session{ID: "a", State: model.StateIdle, Agent: "claude", StartedAt: autoT, Repo: "/r"})
	if on, _ := s.BypassActive(ctx, autoT); on {
		t.Fatal("bypass active with no mode set")
	}
	if _, err := s.SetModes(ctx, store.ModeScope{All: true}, model.ModeBypass, autoT.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.BypassActive(ctx, autoT); !on {
		t.Error("bypass not seen")
	}
	if on, _ := s.BypassActive(ctx, autoT.Add(2*time.Hour)); on {
		t.Error("a lapsed bypass still counts")
	}
}

func TestAutoRunsOnTimeline(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	if _, err := s.LogAuto(ctx, store.AutoRun{At: autoT, Outcome: "fired", TaskID: "T-1", SessionID: "s1", Detail: "ready by you"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Timeline(ctx, store.TimelineQuery{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		found = found || (e.Kind == "auto_spawn" && e.TaskID == "T-1")
	}
	if !found {
		t.Errorf("no auto_spawn event in the timeline: %+v", evs)
	}
}

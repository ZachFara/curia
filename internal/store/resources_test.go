package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/store"
)

func TestResourceSamplesLatestPerSession(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	base := time.Date(2026, 11, 20, 12, 0, 0, 5, time.UTC)
	if err := s.AddResourceSamples(ctx, []store.ResourceSample{
		{SessionID: "a", At: base, CPU: 1, RSSMB: 10}, {SessionID: "b", At: base, CPU: 2, RSSMB: 20},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddResourceSamples(ctx, []store.ResourceSample{{SessionID: "a", At: base.Add(10 * time.Second), CPU: 7.5, RSSMB: 99}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"].CPU != 7.5 || got["a"].RSSMB != 99 || !got["a"].At.Equal(base.Add(10*time.Second)) || got["b"].RSSMB != 20 {
		t.Errorf("latest = %+v, want a's second sample and b's only one", got)
	}
}

// TestPruneResources: age and the row cap both delete, and AddResourceSamples itself never deletes (the
// cap check is off the insert's path).
func TestPruneResources(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-store.ResourceKeep - time.Hour)
	if err := s.AddResourceSamples(ctx, []store.ResourceSample{
		{SessionID: "a", At: old}, {SessionID: "a", At: old.Add(time.Minute)}, {SessionID: "a", At: now},
	}); err != nil {
		t.Fatal(err)
	}
	if n := resourceRows(t, path); n != 3 {
		t.Fatalf("rows after insert = %d, want 3: the insert must not prune", n)
	}
	if n, err := s.PruneResources(ctx, now); err != nil || n != 2 {
		t.Fatalf("PruneResources = %d, %v, want the two old rows", n, err)
	}
	if n := resourceRows(t, path); n != 1 {
		t.Errorf("rows = %d, want the fresh one", n)
	}
	// One bulk statement: 100 000 single-row inserts take ~20 s under -race.
	if _, err := rawDB(t, path).Exec(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < ?)
INSERT INTO resource_samples (at, session_id, cpu_pct, rss_mb) SELECT ?, 'b', 0, 0 FROM c`,
		store.ResourceMaxRows+30, now.UTC().Format("2006-01-02T15:04:05.000000000Z")); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := s.PruneResources(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	if n := resourceRows(t, path); n > store.ResourceMaxRows {
		t.Errorf("rows = %d, want at most the cap %d", n, store.ResourceMaxRows)
	}
}

func resourceRows(t *testing.T, path string) int {
	t.Helper()
	var n int
	if err := rawDB(t, path).QueryRow(`SELECT COUNT(*) FROM resource_samples`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestContainersReplacedWholeAndVMKept(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if err := s.ReplaceContainers(ctx, []store.ContainerRow{
		{ID: "aaaaaaaaaaaa", SessionID: "S1", Name: "web", By: "label", CPU: 1.5, MemMB: 100},
		{ID: "bbbbbbbbbbbb", Name: "db", By: "none", MemMB: 5},
	}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceContainers(ctx, []store.ContainerRow{{ID: "cccccccccccc", SessionID: "S1", Name: "x", By: "compose"}}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, seen, err := s.LatestContainers(ctx)
	if err != nil || len(got) != 1 || got[0].ID != "cccccccccccc" || got[0].SessionID != "S1" || !seen.Equal(at.Add(time.Minute)) {
		t.Errorf("containers = %+v %v %v, want only the second set", got, seen, err)
	}
}

func TestDockerVMKeptOffTheSessionSamples(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if _, ok, _ := s.LatestDockerVM(ctx); ok {
		t.Error("a VM reading before any was stored")
	}
	if err := s.AddDockerVM(ctx, at, 300, 8000); err != nil {
		t.Fatal(err)
	}
	vm, ok, err := s.LatestDockerVM(ctx)
	if err != nil || !ok || vm.UsedMB != 300 || vm.LimitMB != 8000 || !vm.At.Equal(at) {
		t.Errorf("vm = %+v %v %v", vm, ok, err)
	}
	if latest, _ := s.LatestResources(ctx); len(latest) != 0 {
		t.Errorf("the VM row leaked into the per-session samples: %+v", latest)
	}
}

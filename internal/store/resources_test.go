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
	var batch []store.ResourceSample
	for range store.ResourceMaxRows + 30 {
		batch = append(batch, store.ResourceSample{SessionID: "b", At: now})
	}
	if err := s.AddResourceSamples(ctx, batch); err != nil {
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

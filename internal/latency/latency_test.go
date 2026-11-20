package latency_test

import (
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/latency"
)

func median(t *testing.T, p95s ...time.Duration) (time.Duration, int) {
	t.Helper()
	calls := 0
	got := latency.MedianP95(t.Logf, func() time.Duration { calls++; return p95s[calls-1] })
	return got, calls
}

func TestMedianP95OneWildBatchPasses(t *testing.T) {
	const budget = 20 * time.Millisecond
	for _, p := range [][]time.Duration{{5, 400, 6}, {400, 5, 6}, {5, 6, 400}} {
		got, calls := median(t, p[0]*time.Millisecond, p[1]*time.Millisecond, p[2]*time.Millisecond)
		if got >= budget || calls != 3 {
			t.Errorf("batches %v: median %v after %d batches, want under %v after 3", p, got, calls, budget)
		}
	}
}

func TestMedianP95TwoWildBatchesFail(t *testing.T) {
	got, calls := median(t, 5*time.Millisecond, 400*time.Millisecond, 300*time.Millisecond)
	if got != 300*time.Millisecond || calls != 3 {
		t.Errorf("median %v after %d batches, want 300ms after 3", got, calls)
	}
}

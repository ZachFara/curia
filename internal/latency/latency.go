// Package latency is the one way a test gates a percentile: over the median
// of three batches, so one runner-noise episode in one batch cannot fail it.
package latency

import (
	"slices"
	"time"
)

// Batches is how many batches a latency gate runs and takes the median of.
const Batches = 3

// MedianP95 runs batch Batches times, always all of them, and returns the
// median of the p95 values it returned. batch measures one batch with the
// gate's own sample count, logs its own p50, p95 and max, and returns its p95.
// Owner's next step if flakes continue (ticket D-1): gate on the ratio to a
// reference workload measured in the same job.
func MedianP95(logf func(format string, args ...any), batch func() time.Duration) time.Duration {
	p95s := make([]time.Duration, Batches)
	for i := range p95s {
		logf("latency batch %d of %d", i+1, Batches)
		p95s[i] = batch()
	}
	logf("batch p95 values %v", p95s)
	slices.Sort(p95s)
	logf("median of %d batch p95 values: %v", Batches, p95s[Batches/2])
	return p95s[Batches/2]
}

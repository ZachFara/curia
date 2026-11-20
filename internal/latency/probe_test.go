package latency_test

import (
	"testing"

	"github.com/ZachFara/curia/internal/latency"
)

// Temporary: prints the reference workload on CI (fails on purpose so go test shows the log).
func TestProbeReferenceLatency(t *testing.T) {
	for range 6 {
		t.Logf("REFERENCE %v", latency.Reference(t))
	}
	t.Error("probe only")
}

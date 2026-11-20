package latency_test

import (
	"testing"

	"github.com/ZachFara/curia/internal/latency"
)

func TestProbeReferenceLatency(t *testing.T) {
	for range 8 {
		p50, p95 := latency.Reference(t)
		t.Logf("REFERENCE p50 %v p95 %v", p50, p95)
	}
	t.Error("probe only")
}

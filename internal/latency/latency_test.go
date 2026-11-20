package latency_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/latency"
)

const (
	ms        = time.Millisecond
	threshold = 20 * ms
)

func TestVerdictScale(t *testing.T) {
	n := latency.Nominal
	for _, c := range []struct {
		ref   time.Duration
		scale float64
	}{{n / 2, 1}, {n, 1}, {n * 3 / 2, 1.5}, {n * 2, 2}} {
		if _, scale, fail := latency.Verdict(0, threshold, c.ref); scale != c.scale || fail != "" {
			t.Errorf("ref %v: scale %v fail %q, want %v and no failure", c.ref, scale, fail, c.scale)
		}
	}
}

func TestVerdictBelowNominalKeepsTheAbsoluteThreshold(t *testing.T) {
	limit, _, _ := latency.Verdict(0, threshold, latency.Nominal/4)
	if limit != threshold {
		t.Errorf("limit %v, want %v", limit, threshold)
	}
}

func TestVerdictOverTwiceNominalFailsEvenWhenFast(t *testing.T) {
	_, scale, fail := latency.Verdict(1*ms, threshold, latency.Nominal*21/10)
	if scale != 2 || !strings.Contains(fail, "too slow") {
		t.Errorf("scale %v, fail %q, want 2 and a too-slow failure", scale, fail)
	}
}

func TestVerdictProductOverThresholdTimesScaleFails(t *testing.T) {
	ref := latency.Nominal * 3 / 2 // scale 1.5: limit 30 ms
	if _, _, fail := latency.Verdict(29*ms, threshold, ref); fail != "" {
		t.Errorf("29 ms under a 30 ms limit failed: %s", fail)
	}
	if _, _, fail := latency.Verdict(30*ms, threshold, ref); fail == "" {
		t.Error("30 ms against a 30 ms limit passed")
	}
}

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

// A runner whose commit tail is inflated (a slow fsync) is excused up to the
// clamp; a product that sleeps is not excused by it.
func TestVerdictInflatedReferenceTailStillCatchesAProductSleep(t *testing.T) {
	ref := latency.Nominal * 3 / 2 // scale 1.5, limit 30 ms
	if _, _, fail := latency.Verdict(25*ms, threshold, ref); fail != "" {
		t.Errorf("a slow-commit runner at 25 ms (limit 30 ms) failed: %s", fail)
	}
	if _, _, fail := latency.Verdict(25*ms+30*ms, threshold, ref); fail == "" {
		t.Error("a 30 ms sleep in the product passed on an inflated reference tail")
	}
}

func batches(gate string, p95s ...time.Duration) []latency.Result {
	var rs []latency.Result
	for _, p := range p95s {
		rs = append(rs, latency.Result{Gate: gate, Threshold: threshold, P95: p, Ref95: latency.Nominal})
	}
	return rs
}

func judge(t *testing.T, rs []latency.Result) (string, bool) {
	t.Helper()
	table, ok := latency.Judge(rs)
	t.Log(table)
	return table, ok
}

func TestJudgeOneWildBatchPassesTwoFail(t *testing.T) {
	for _, p := range [][]time.Duration{{5 * ms, 400 * ms, 6 * ms}, {400 * ms, 5 * ms, 6 * ms}, {5 * ms, 6 * ms, 400 * ms}} {
		if table, ok := judge(t, batches("g", p...)); !ok {
			t.Errorf("batches %v failed:\n%s", p, table)
		}
	}
	if table, ok := judge(t, batches("g", 5*ms, 400*ms, 300*ms)); ok || !strings.Contains(table, "g") {
		t.Errorf("two wild batches passed or the table omits the gate:\n%s", table)
	}
}

func TestJudgeGateMissingFromAPassFails(t *testing.T) {
	rs := append(batches("a", 5*ms, 5*ms, 5*ms), batches("b", 5*ms, 5*ms)...)
	table, ok := judge(t, rs)
	if ok || !strings.Contains(table, "2 batch results, want 3") {
		t.Errorf("a gate in two passes: ok %v, table:\n%s", ok, table)
	}
	if _, ok = judge(t, nil); ok {
		t.Error("no results at all passed")
	}
}

func TestJudgeFailedAssertionInOnePassFails(t *testing.T) {
	rs := batches("g", 5*ms, 5*ms, 5*ms)
	rs[1].Failed = true
	if table, ok := judge(t, rs); ok || !strings.Contains(table, "assertion") {
		t.Errorf("a failed assertion in one pass: ok %v, table:\n%s", ok, table)
	}
}

func TestJudgeScaleIsTheMedianOfTheThreeClampedAndApplied(t *testing.T) {
	rs := batches("g", 25*ms, 25*ms, 25*ms)
	for i, r := range []time.Duration{latency.Nominal, latency.Nominal * 3 / 2, latency.Nominal * 5} { // median 1.5: limit 30 ms
		rs[i].Ref95 = r
	}
	if table, ok := judge(t, rs); !ok {
		t.Errorf("25 ms under a 30 ms limit (median scale 1.5; one wild reference ignored) failed:\n%s", table)
	}
	for i := range rs {
		rs[i].P95 = 31 * ms
	}
	if table, ok := judge(t, rs); ok {
		t.Errorf("31 ms over the 30 ms limit passed:\n%s", table)
	}
	for i := range rs { // median reference over 2x nominal: fails whatever the batches
		rs[i].P95, rs[i].Ref95 = 1*ms, latency.Nominal*3
	}
	if table, ok := judge(t, rs); ok || !strings.Contains(table, "too slow") {
		t.Errorf("a reference over 2x nominal passed:\n%s", table)
	}
}

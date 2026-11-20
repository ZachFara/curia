package e2e

// The latency gate: a copy of internal/latency (e2e may import no internal
// package). Keep the two identical but for names.

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// latencyBatches is how many batches a latency gate runs and takes the median of.
const latencyBatches = 3

// latencyNominal is the reference workload's time on the machine class the thresholds
// were set for (race detector on, median of 5 repetitions, 2026-11-19).
// Measured: this Mac, 88 to 94 ms (load average 4 to 6, so a quiet one is no
// slower); GitHub ubuntu-latest in the latency job, alone on its runner: 172 ms
// on one runner and 105 ms on another (two runs of the same commit); 260 to
// 355 ms when other test packages run beside it (not the latency phase). Chosen
// as 100 ms so a developer machine scales at 1 and a fast runner at about 1.05.
const latencyNominal = 2 * time.Millisecond

// latencyMaxScale is the most a slow machine is excused: a reference more than
// latencyMaxScale times latencyNominal fails the gate instead of stretching it further.
const latencyMaxScale = 2

// latencyGate runs batch latencyBatches times, always all of them (batch measures one batch
// with the gate's own sample count, logs its own p50, p95 and max, and
// returns its p95), and fails t unless the median p95 is under p95Budget times
// the machine's scale. The reference is timed before and after the batches and
// the smaller is used, so a slowdown that starts mid-test does not excuse it.
func latencyGate(t testing.TB, what string, batch func() time.Duration) {
	t.Helper()
	before50, before95 := latencyReference(t)
	p95s := make([]time.Duration, latencyBatches)
	for i := range p95s {
		t.Logf("latency batch %d of %d", i+1, len(p95s))
		p95s[i] = batch()
	}
	after50, after95 := latencyReference(t)
	ref50, ref95 := min(before50, after50), min(before95, after95)
	median := slices.Sorted(slices.Values(p95s))[len(p95s)/2]
	limit, scale, fail := latencyVerdict(median, p95Budget, ref95)
	t.Logf("%s: batch p95 values %v, median %v; reference commit p50 %v p95 %v (before %v, %v), scale %.2f, effective threshold %v", what, p95s, median, ref50, ref95, before50, before95, scale, limit)
	if fail != "" {
		t.Errorf("%s: %s", what, fail)
	}
}

// latencyVerdict returns the effective threshold, the scale (the reference p95 over
// latencyNominal, clamped to [1, latencyMaxScale]) and, when the gate fails, why.
func latencyVerdict(median, threshold, ref time.Duration) (limit time.Duration, scale float64, fail string) {
	raw := float64(ref) / float64(latencyNominal)
	scale = min(max(raw, 1), latencyMaxScale)
	limit = time.Duration(float64(threshold) * scale)
	switch {
	case raw > latencyMaxScale:
		fail = fmt.Sprintf("reference commit p95 took %v, %.2fx the nominal %v, over the %vx allowance: the machine is too slow for this gate, which fails rather than excuses it", ref, raw, latencyNominal, latencyMaxScale)
	case median >= limit:
		fail = fmt.Sprintf("median p95 = %v, want < %v (threshold %v x scale %.2f; reference p95 %v, nominal %v)", median, limit, threshold, scale, ref, latencyNominal)
	}
	return limit, scale, fail
}

// latencyReference times individual durable write transactions (three inserts and a
// read, 1000 of them, so past the WAL checkpoint several times) on a fresh
// temp-file store opened as the product opens it (WAL, default synchronous,
// immediate transactions), and returns their p50 and p95. The gates fail on an
// occasional slow commit (an fsync or checkpoint on a shared runner's disk), not
// on slow CPU, so the scale comes from this tail. It uses the driver and a
// private schema, never internal/store, so a product slowdown cannot raise it.
func latencyReference(t testing.TB) (p50, p95 time.Duration) {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "ref.db"), RawQuery: q.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.Exec(`CREATE TABLE ref (id INTEGER PRIMARY KEY, k TEXT NOT NULL, v INTEGER NOT NULL); CREATE INDEX ref_k ON ref (k)`); err != nil {
		t.Fatal(err)
	}
	const txns = 1000
	took := make([]time.Duration, txns)
	for i := range took {
		start := time.Now()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for j := range 3 {
			if _, err = tx.Exec(`INSERT INTO ref (k, v) VALUES (?, ?)`, fmt.Sprintf("k%d", (i*3+j)%17), i); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err = tx.QueryRow(`SELECT count(*) FROM ref WHERE k = ?`, "k3").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		took[i] = time.Since(start)
	}
	slices.Sort(took)
	return took[txns/2], took[txns*95/100]
}

const (
	ms        = time.Millisecond
	threshold = 20 * ms
)

func TestLatencyVerdictScale(t *testing.T) {
	n := latencyNominal
	for _, c := range []struct {
		ref   time.Duration
		scale float64
	}{{n / 2, 1}, {n, 1}, {n * 3 / 2, 1.5}, {n * 2, 2}} {
		if _, scale, fail := latencyVerdict(0, threshold, c.ref); scale != c.scale || fail != "" {
			t.Errorf("ref %v: scale %v fail %q, want %v and no failure", c.ref, scale, fail, c.scale)
		}
	}
}

func TestLatencyVerdictBelowNominalKeepsTheAbsoluteThreshold(t *testing.T) {
	limit, _, _ := latencyVerdict(0, threshold, latencyNominal/4)
	if limit != threshold {
		t.Errorf("limit %v, want %v", limit, threshold)
	}
}

func TestLatencyVerdictOverTwiceNominalFailsEvenWhenFast(t *testing.T) {
	_, scale, fail := latencyVerdict(1*ms, threshold, latencyNominal*21/10)
	if scale != 2 || !strings.Contains(fail, "too slow") {
		t.Errorf("scale %v, fail %q, want 2 and a too-slow failure", scale, fail)
	}
}

func TestLatencyVerdictProductOverThresholdTimesScaleFails(t *testing.T) {
	ref := latencyNominal * 3 / 2 // scale 1.5: limit 30 ms
	if _, _, fail := latencyVerdict(29*ms, threshold, ref); fail != "" {
		t.Errorf("29 ms under a 30 ms limit failed: %s", fail)
	}
	if _, _, fail := latencyVerdict(30*ms, threshold, ref); fail == "" {
		t.Error("30 ms against a 30 ms limit passed")
	}
}

// A runner whose commit tail is inflated (a slow fsync) is excused up to the
// clamp; a product that sleeps is not excused by it.
func TestLatencyVerdictInflatedReferenceTailStillCatchesAProductSleep(t *testing.T) {
	ref := latencyNominal * 3 / 2 // scale 1.5, limit 30 ms
	if _, _, fail := latencyVerdict(25*ms, threshold, ref); fail != "" {
		t.Errorf("a slow-commit runner at 25 ms (limit 30 ms) failed: %s", fail)
	}
	if _, _, fail := latencyVerdict(25*ms+30*ms, threshold, ref); fail == "" {
		t.Error("a 30 ms sleep in the product passed on an inflated reference tail")
	}
}

// Package latency is the one way a test gates a percentile: over the median
// of three batches (so one runner-noise episode in one batch cannot fail it),
// scaled by how slow the machine is today compared with the one the thresholds
// were set for (so a slow GitHub runner is not mistaken for a regression).
package latency

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver the product uses; the reference deliberately bypasses internal/store
)

// Batches is how many batches a latency gate runs and takes the median of.
const Batches = 3

// Nominal is the p95 of the reference workload (individual durable write
// transactions on a fresh WAL store, 1000 per run, race detector on) on the
// machine class the thresholds were set for. The gates fail on an occasional
// slow commit, so the scale follows the commit tail, not the median. Measured
// 2026-11-19: this Mac (load average 5), 8 runs, p50 1.1 to 1.3 ms, p95 1.4 to
// 2.4 ms (usually 1.6 to 1.7); GitHub ubuntu-latest, latency job: p50 1.5 ms,
// p95 1.8 to 2.1 ms on one runner and 2.3 to 2.7 ms (p50 1.9) on another, 8 runs each. Chosen as 2 ms so a
// quiet developer machine scales at 1 and these runners at 1.0 to 1.35.
const Nominal = 2 * time.Millisecond

// MaxScale is the most a slow machine is excused: a reference more than
// MaxScale times Nominal means the gate could not be measured (owner decision
// 2026-11-20): not a pass, and not a failure of the code.
const MaxScale = 2

// UnmeasuredPrefix starts the verdict text of a gate whose machine was too slow.
const UnmeasuredPrefix = "could not measure: "

// CouldNotMeasureMarker is the last line the verdict program prints for that outcome.
const CouldNotMeasureMarker = "LATENCY-COULD-NOT-MEASURE: no gate failed, but the machine was too slow to measure at least one; not a code failure, rerun on another machine"

// Outcome is the result of judging the gates of a run.
type Outcome int

// Pass: every gate measured and under its threshold. CouldNotMeasure: no
// failure, but at least one gate ran on a machine over the allowance. Fail: a
// real failure anywhere (it wins). The order is the precedence.
const (
	Pass Outcome = iota
	CouldNotMeasure
	Fail
)

// Gate runs batch Batches times, always all of them (batch measures one batch
// with the gate's own sample count, logs its own p50, p95 and max, and
// returns its p95), and fails t unless the median p95 is under threshold times
// the machine's scale. The reference is timed before and after the batches and
// the smaller is used, so a slowdown that starts mid-test does not excuse it.
//
// With ResultsEnv set (make test-latency: three passes, each running every gate
// with ONE batch, spaced by the time the other gates take) Gate runs one batch,
// prints it for Judge and does not fail on the p95 itself.
func Gate(t testing.TB, what string, threshold time.Duration, batch func() time.Duration) {
	t.Helper()
	if os.Getenv(ResultsEnv) != "" {
		spaced(t, what, threshold, batch)
		return
	}
	before50, before95 := Reference(t)
	p95s := make([]time.Duration, Batches)
	for i := range p95s {
		t.Logf("latency batch %d of %d", i+1, len(p95s))
		p95s[i] = batch()
	}
	after50, after95 := Reference(t)
	ref50, ref95 := min(before50, after50), min(before95, after95)
	median := slices.Sorted(slices.Values(p95s))[len(p95s)/2]
	limit, scale, fail := Verdict(median, threshold, ref95)
	t.Logf("%s: batch p95 values %v, median %v; reference commit p50 %v p95 %v (before %v, %v), scale %.2f, effective threshold %v", what, p95s, median, ref50, ref95, before50, before95, scale, limit)
	if fail != "" {
		t.Errorf("%s: %s", what, fail)
	}
}

// Verdict returns the effective threshold, the scale (the reference p95 over
// Nominal, clamped to [1, MaxScale]) and, when the gate fails or cannot be
// measured (fail starts with UnmeasuredPrefix), why.
func Verdict(median, threshold, ref time.Duration) (limit time.Duration, scale float64, fail string) {
	raw := float64(ref) / float64(Nominal)
	scale = min(max(raw, 1), MaxScale)
	limit = time.Duration(float64(threshold) * scale)
	switch {
	case raw > MaxScale:
		fail = fmt.Sprintf(UnmeasuredPrefix+"the machine is too slow for this gate (reference p95 %v, %.2f times the nominal %v, over the %vx allowance)", ref, raw, Nominal, MaxScale)
	case median >= limit:
		fail = fmt.Sprintf("median p95 = %v, want < %v (threshold %v x scale %.2f; reference p95 %v, nominal %v)", median, limit, threshold, scale, ref, Nominal)
	}
	return limit, scale, fail
}

// Reference times individual durable write transactions (three inserts and a
// read, 1000 of them, so past the WAL checkpoint several times) on a fresh
// temp-file store opened as the product opens it (WAL, default synchronous,
// immediate transactions), and returns their p50 and p95. The gates fail on an
// occasional slow commit (an fsync or checkpoint on a shared runner's disk), not
// on slow CPU, so the scale comes from this tail. It uses the driver and a
// private schema, never internal/store, so a product slowdown cannot raise it.
func Reference(t testing.TB) (p50, p95 time.Duration) {
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

// ResultsEnv is the variable that makes every gate run ONE batch and print it as a
// ResultPrefix line (make test-latency runs three such passes, with -v).
const ResultsEnv = "CURIA_LATENCY_SPACED"

// ResultPrefix starts a result line in the test output.
const ResultPrefix = "LATENCY-RESULT "

// Result is one gate's one batch in one pass.
type Result struct {
	Gate      string        `json:"gate"`
	At        time.Time     `json:"at"`
	Threshold time.Duration `json:"threshold"`
	P95       time.Duration `json:"p95"`
	Ref50     time.Duration `json:"ref_p50"`
	Ref95     time.Duration `json:"ref_p95"`
	Failed    bool          `json:"failed"` // a non-latency assertion of the test failed in this pass
}

// spaced runs one batch and records it when the test ends, so Failed covers
// the test's other assertions. The p95 is judged by Judge, over the passes.
func spaced(t testing.TB, what string, threshold time.Duration, batch func() time.Duration) {
	t.Helper()
	before50, before95 := Reference(t)
	p95 := batch()
	after50, after95 := Reference(t)
	r := Result{Gate: t.Name() + ": " + what, At: time.Now(), Threshold: threshold, P95: p95, Ref50: min(before50, after50), Ref95: min(before95, after95)}
	t.Logf("%s: spaced batch p95 %v, reference commit p50 %v p95 %v", r.Gate, p95, r.Ref50, r.Ref95)
	t.Cleanup(func() {
		r.Failed = t.Failed()
		line, err := json.Marshal(r)
		if err != nil {
			t.Errorf("record latency result: %v", err)
			return
		}
		fmt.Printf("%s%s\n", ResultPrefix, line) // stdout, shown by go test -v, read by ReadResults
	})
}

// ReadResults picks the results out of the `go test -v` output of the passes.
func ReadResults(r io.Reader) ([]Result, error) {
	var rs []Result
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), ResultPrefix); ok {
			var x Result
			if err := json.Unmarshal([]byte(line), &x); err != nil {
				return nil, fmt.Errorf("bad result line %q: %w", line, err)
			}
			rs = append(rs, x)
		}
	}
	return rs, sc.Err()
}

// Judge applies the owner's rule to the spaced passes: per gate, exactly Batches
// results (a crashed or skipped test is a failure), no failed non-latency
// assertion in any pass, and the median p95 under the threshold times the scale
// of the MEDIAN reference p95 (clamped as Verdict does: a single pass's inflated
// reference cannot excuse the gate, and a reference over MaxScale x Nominal
// is CouldNotMeasure, not a failure). It returns the table, always, and the
// outcome: Fail if anything really failed, else CouldNotMeasure if any gate
// was over the allowance, else Pass.
func Judge(rs []Result) (table string, outcome Outcome) {
	var b strings.Builder
	if len(rs) == 0 {
		return "no latency results: no gate ran in this shard, which fails\n", Fail
	}
	var order []string
	by := map[string][]Result{}
	for _, r := range rs {
		if _, seen := by[r.Gate]; !seen {
			order = append(order, r.Gate)
		}
		by[r.Gate] = append(by[r.Gate], r)
	}
	for _, g := range order {
		got := by[g]
		var p95s, refs []time.Duration
		var at []string
		failedAssertion := false
		for _, r := range got {
			p95s, refs = append(p95s, r.P95), append(refs, r.Ref95)
			at = append(at, r.At.Format("15:04:05"))
			failedAssertion = failedAssertion || r.Failed
		}
		median := func(d []time.Duration) time.Duration { return slices.Sorted(slices.Values(d))[len(d)/2] }
		fail := ""
		limit, scale := time.Duration(0), 0.0
		switch {
		case len(got) != Batches:
			fail = fmt.Sprintf("%d batch results, want %d", len(got), Batches)
		case failedAssertion:
			fail = "a non-latency assertion failed in a pass"
		default:
			limit, scale, fail = Verdict(median(p95s), got[0].Threshold, median(refs))
		}
		verdict, o := "ok", Pass
		if strings.HasPrefix(fail, UnmeasuredPrefix) {
			verdict, o = fail, CouldNotMeasure
		} else if fail != "" {
			verdict, o = "FAIL "+fail, Fail
		}
		outcome = max(outcome, o) // Fail outranks CouldNotMeasure outranks Pass
		var m time.Duration
		if len(p95s) > 0 {
			m = median(p95s)
		}
		fmt.Fprintf(&b, "%s: batch p95 %v at %v, median %v, reference p95 %v, scale %.2f, effective threshold %v: %s\n", g, p95s, at, m, refs, scale, limit, verdict)
	}
	return b.String(), outcome
}

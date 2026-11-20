// Package latency is the one way a test gates a percentile: over the median
// of three batches (so one runner-noise episode in one batch cannot fail it),
// scaled by how slow the machine is today compared with the one the thresholds
// were set for (so a slow GitHub runner is not mistaken for a regression).
package latency

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver the product uses; the reference deliberately bypasses internal/store
)

// Batches is how many batches a latency gate runs and takes the median of.
const Batches = 3

// Nominal is the reference workload's time on the machine class the thresholds
// were set for (race detector on, median of 5 repetitions, 2026-11-19).
// Measured: this Mac, 88 to 94 ms (load average 4 to 6, so a quiet one is no
// slower); GitHub ubuntu-latest in the latency job, alone on its runner: 172 ms
// on one runner and 105 ms on another (two runs of the same commit); 260 to
// 355 ms when other test packages run beside it (not the latency phase). Chosen
// as 100 ms so a developer machine scales at 1 and a fast runner at about 1.05.
const Nominal = 100 * time.Millisecond

// MaxScale is the most a slow machine is excused: a reference more than
// MaxScale times Nominal fails the gate instead of stretching it further.
const MaxScale = 2

// Gate runs batch Batches times, always all of them (batch measures one batch
// with the gate's own sample count, logs its own p50, p95 and max, and
// returns its p95), and fails t unless the median p95 is under threshold times
// the machine's scale. The reference is timed before and after the batches and
// the smaller is used, so a slowdown that starts mid-test does not excuse it.
func Gate(t testing.TB, what string, threshold time.Duration, batch func() time.Duration) {
	t.Helper()
	before := Reference(t)
	p95s := make([]time.Duration, Batches)
	for i := range p95s {
		t.Logf("latency batch %d of %d", i+1, Batches)
		p95s[i] = batch()
	}
	ref := min(before, Reference(t))
	median := slices.Sorted(slices.Values(p95s))[Batches/2]
	limit, scale, fail := Verdict(median, threshold, ref)
	t.Logf("%s: batch p95 values %v, median %v, reference %v (before %v), scale %.2f, effective threshold %v", what, p95s, median, ref, before, scale, limit)
	if fail != "" {
		t.Errorf("%s: %s", what, fail)
	}
}

// Verdict returns the effective threshold, the scale (the reference over
// Nominal, clamped to [1, MaxScale]) and, when the gate fails, why.
func Verdict(median, threshold, ref time.Duration) (limit time.Duration, scale float64, fail string) {
	raw := float64(ref) / float64(Nominal)
	scale = min(max(raw, 1), MaxScale)
	limit = time.Duration(float64(threshold) * scale)
	switch {
	case raw > MaxScale:
		fail = fmt.Sprintf("reference workload took %v, %.2fx the nominal %v, over the %vx allowance: the machine is too slow for this gate, which fails rather than excuses it", ref, raw, Nominal, MaxScale)
	case median >= limit:
		fail = fmt.Sprintf("median p95 = %v, want < %v (threshold %v x scale %.2f; reference %v, nominal %v)", median, limit, threshold, scale, ref, Nominal)
	}
	return limit, scale, fail
}

// Reference times a fixed piece of work of the gates' character (SQLite
// through the pure-Go driver, opened as the product opens it): the median of
// 5 repetitions of open, then 100 transactions of three inserts and a read.
// It uses the driver and a private schema, never internal/store, so a product
// slowdown cannot raise it.
func Reference(t testing.TB) time.Duration {
	t.Helper()
	var reps [5]time.Duration
	for i := range reps {
		start := time.Now()
		referenceOnce(t, filepath.Join(t.TempDir(), "ref.db"))
		reps[i] = time.Since(start)
	}
	slices.Sort(reps[:])
	return reps[len(reps)/2]
}

func referenceOnce(t testing.TB, path string) {
	t.Helper()
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.Exec(`CREATE TABLE ref (id INTEGER PRIMARY KEY, k TEXT NOT NULL, v INTEGER NOT NULL); CREATE INDEX ref_k ON ref (k)`); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
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
	}
}

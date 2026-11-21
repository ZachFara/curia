package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	latgate "github.com/ZachFara/curia/internal/latency"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/testkit"
)

// ingestSpecNow sits inside the windows of the spec's example document
// (internal/usage/testdata/doc-spec.json).
var ingestSpecNow = time.Date(2026, 11, 11, 13, 41, 0, 0, time.UTC)

// ingestFixture reads one of internal/usage's payload fixtures.
func ingestFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("..", "..", "internal", "usage", "testdata", name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ingestFixClock makes clock return now for the rest of the test.
func ingestFixClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = prev })
}

// ingestExec runs `curia <args>` in-process with stdin, failing t when
// Execute returns an error, and returns what went to stdout and stderr.
func ingestExec(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	defer func() {
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()
	if err := rootCmd.Execute(); err != nil {
		t.Errorf("Execute returned %v", err)
	}
	return out.String(), errOut.String()
}

// ingestSnapshot is what an ingest can change in the store at db: one
// session's cost and cache columns and the samples.
type ingestSnapshot struct {
	found       bool
	sess        model.Session
	usage, cost int
}

func ingestSnap(t *testing.T, db, id string) ingestSnapshot {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	s, found, err := st.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.UsageSince(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.CostSince(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return ingestSnapshot{
		found: found,
		sess: model.Session{
			CostUSD: s.CostUSD, CostKnown: s.CostKnown, CacheExpiresAt: s.CacheExpiresAt,
			CacheRebuildUSD: s.CacheRebuildUSD, CacheRecacheTokens: s.CacheRecacheTokens,
			State: s.State, UpdatedAt: s.UpdatedAt, SeenAt: s.SeenAt,
		},
		usage: len(u), cost: len(c),
	}
}

// ingestSeed stores an idle session id in the store at db.
func ingestSeed(t *testing.T, db string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		seedSessions(t, db, model.Session{ID: id, State: model.StateIdle, UpdatedAt: time.Now().Add(-time.Minute)})
	}
}

// ingestSpecDoc is the spec's example with session_id set to id.
func ingestSpecDoc(t *testing.T, id string) string {
	t.Helper()
	return strings.Replace(string(ingestFixture(t, "doc-spec.json")), "7c1e5a52-0f1b-4c1e-9a53-5d3f0c1a2b44", id, 1)
}

// ingestLogWithoutTime drops slog's time= attribute, whose date holds
// digits and dashes of its own.
func ingestLogWithoutTime(lines []string) string {
	re := regexp.MustCompile(`time=\S+ `)
	return re.ReplaceAllString(strings.Join(lines, "\n"), "")
}

func TestIngestStoresDocument(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)

	stdout, stderr := ingestExec(t, strings.NewReader(ingestSpecDoc(t, "s1")), "ingest")
	if stdout != "" || stderr != "" {
		t.Errorf("stdout %q stderr %q, want both empty", stdout, stderr)
	}
	got := ingestSnap(t, db, "s1")
	if !got.sess.CostKnown || got.sess.CostUSD != 4.12 {
		t.Errorf("cost = %v (known %v), want 4.12", got.sess.CostUSD, got.sess.CostKnown)
	}
	if want := time.Date(2026, 11, 11, 14, 5, 0, 0, time.UTC); !got.sess.CacheExpiresAt.Equal(want) {
		t.Errorf("cache expiry = %v, want %v", got.sess.CacheExpiresAt, want)
	}
	if got.sess.CacheRebuildUSD != 1.68 {
		t.Errorf("rebuild = %v, want 1.68", got.sess.CacheRebuildUSD)
	}
	if got.usage != 2 || got.cost != 1 {
		t.Errorf("usage samples %d cost samples %d, want 2 and 1", got.usage, got.cost)
	}
	if log := readLog(t); log != nil {
		t.Errorf("log = %q, want none", log)
	}
}

// ingestBadConfig writes a config file that does not parse into the HOME
// that hookHome made for db.
func ingestBadConfig(t *testing.T, db string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(db), ".config", "curia")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("not = [toml"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ingestMissingStore points CURIA_DB into a directory that does not exist
// and returns that directory.
func ingestMissingStore(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "missing")
	t.Setenv("CURIA_DB", filepath.Join(dir, "curia.db"))
	return dir
}

func TestIngestNeverFails(t *testing.T) {
	good := func(t *testing.T) string { return ingestSpecDoc(t, "s1") }
	for _, tc := range []struct {
		name  string
		stdin func(t *testing.T) string
		setup func(t *testing.T, db string) (mustNotExist string)
	}{
		{"empty", func(*testing.T) string { return "" }, nil},
		{"garbage", func(*testing.T) string { return "garbage" }, nil},
		{"one MiB", func(*testing.T) string { return strings.Repeat("a", 1<<20) }, nil},
		{"array", func(*testing.T) string { return "[]" }, nil},
		{"schema version 2", func(t *testing.T) string {
			return strings.Replace(good(t), `"schema_version": 1`, `"schema_version": 2`, 1)
		}, nil},
		{"store directory missing", good, func(t *testing.T, _ string) string { return ingestMissingStore(t) }},
		{"config does not parse", good, func(t *testing.T, db string) string { ingestBadConfig(t, db); return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.HookHome(t)
			ingestSeed(t, db, "s1")
			ingestFixClock(t, ingestSpecNow)
			before := ingestSnap(t, db, "s1")
			mustNotExist := ""
			if tc.setup != nil {
				mustNotExist = tc.setup(t, db)
			}
			stdout, stderr := ingestExec(t, strings.NewReader(tc.stdin(t)), "ingest")
			if stdout != "" || stderr != "" {
				t.Errorf("stdout %q stderr %q, want both empty", stdout, stderr)
			}
			if after := ingestSnap(t, db, "s1"); after != before {
				t.Errorf("store changed: %+v, want %+v", after, before)
			}
			ingestWantMissing(t, mustNotExist)
		})
	}
}

// ingestWantMissing fails t when path is set and exists.
func ingestWantMissing(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		return
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s was created (stat: %v)", path, err)
	}
}

func TestIngestPrintsNothing(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)
	for _, doc := range []string{ingestSpecDoc(t, "s1"), `{"schema_version":1,"session_id":"bad id"}`} {
		stdout, stderr := ingestExec(t, strings.NewReader(doc), "ingest")
		if stdout != "" || stderr != "" {
			t.Errorf("stdout %q stderr %q, want both empty", stdout, stderr)
		}
	}
}

func TestIngestLogsNamesNotValues(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)

	ingestExec(t, strings.NewReader(`{"schema_version":1,"session_id":"s1","cost_usd":-1,"cache_rebuild_usd":98765.4321}`), "ingest")
	log := readLog(t)
	text := ingestLogWithoutTime(log)
	if len(log) != 1 || !strings.Contains(text, "cost_usd") || !strings.Contains(text, "cache_rebuild_usd") {
		t.Fatalf("log = %q, want one line naming cost_usd and cache_rebuild_usd", log)
	}
	for _, v := range []string{"-1", "98765"} {
		if strings.Contains(text, v) {
			t.Errorf("log %q holds the value %q", text, v)
		}
	}

	// Refused documents: the line gives the reason, never a piece of the document.
	for _, doc := range []string{
		`{"schema_version":2,"session_id":"QQdocQQ","cost_usd":31337.5}`,
		`XYZZY-garbage-PLUGH`,
		`{"schema_version":1,"session_id":"has space QQdocQQ","cost_usd":31337.5}`,
	} {
		logPath := filepath.Join(filepath.Dir(db), ".local", "state", "curia", "curia.log")
		if err := os.Remove(logPath); err != nil {
			t.Fatal(err)
		}
		// Each document is a new problem, as if a minute had passed: the
		// rate limit's marker goes too.
		if err := os.Remove(logPath + ingestThrottleFile); err != nil {
			t.Fatal(err)
		}
		ingestExec(t, strings.NewReader(doc), "ingest")
		log := readLog(t)
		text := ingestLogWithoutTime(log)
		if len(log) != 1 || !strings.Contains(text, "refused") {
			t.Errorf("log = %q, want one line saying the document was refused", log)
		}
		for _, v := range []string{"QQdocQQ", "31337", "XYZZY", "PLUGH"} {
			if strings.Contains(text, v) {
				t.Errorf("log %q holds %q from the document", text, v)
			}
		}
	}
	if s := ingestSnap(t, db, "s1"); s.sess.CostKnown {
		t.Errorf("cost stored from a bad document: %v", s.sess.CostUSD)
	}
}

func TestIngestStatuslineFlag(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "built-session-1")
	ingestFixClock(t, statuslineNow)

	stdout, stderr := ingestExec(t, bytes.NewReader(ingestFixture(t, "statusline-built.json")), "ingest", "--statusline")
	if stdout != "" || stderr != "" {
		t.Errorf("stdout %q stderr %q, want both empty: the chained form must not disturb the user's own line", stdout, stderr)
	}
	got := ingestSnap(t, db, "built-session-1")
	if !got.sess.CostKnown || got.sess.CostUSD != 4.12 {
		t.Errorf("cost = %v (known %v), want 4.12", got.sess.CostUSD, got.sess.CostKnown)
	}
	if want := time.Unix(1794771246, 0); !got.sess.CacheExpiresAt.Equal(want) || got.sess.CacheRecacheTokens != 42211 {
		t.Errorf("cache = %v, %d tokens; want %v, 42211 (Ruling P5-R1)", got.sess.CacheExpiresAt, got.sess.CacheRecacheTokens, want)
	}
	if got.usage != 2 || got.cost != 1 {
		t.Errorf("usage samples %d cost samples %d, want 2 and 1", got.usage, got.cost)
	}
	if log := readLog(t); log != nil {
		t.Errorf("log = %q, want none", log)
	}
}

func TestIngestAllowedInsideAgent(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)
	fake := useFakeTmux(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("TMUX_PANE", "%1")
	t.Setenv("CURIA_TASK_ID", "T-1")

	ingestExec(t, strings.NewReader(ingestSpecDoc(t, "s1")), "ingest")
	if got := ingestSnap(t, db, "s1"); got.sess.CostUSD != 4.12 || got.usage != 2 || got.cost != 1 {
		t.Errorf("got %+v, want stored as usual", got)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls %q, want none", fake.Calls)
	}
}

func TestIngestStdinDeadline(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	prev := ingestLimit
	ingestLimit = 100 * time.Millisecond
	t.Cleanup(func() { ingestLimit = prev })
	before := ingestSnap(t, db, "s1")

	stdin, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	go func() { _, _ = writer.Write([]byte(`{"schema_version":1,"session_id":"s1","cost_usd":`)) }()
	start := time.Now()
	stdout, stderr := ingestExec(t, stdin, "ingest")
	if d := time.Since(start); d > time.Second {
		t.Errorf("returned after %v, want within 1s", d)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("stdout %q stderr %q, want both empty", stdout, stderr)
	}
	if after := ingestSnap(t, db, "s1"); after != before {
		t.Errorf("store changed: %+v", after)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "deadline") {
		t.Errorf("log = %q, want one line about the deadline", log)
	}
}

// ingestPanicReader panics on Read.
type ingestPanicReader struct{}

func (ingestPanicReader) Read([]byte) (int, error) { panic("ingest test: reader panicked") }

func TestIngestRecoversPanic(t *testing.T) {
	testkit.HookHome(t)
	stdout, stderr := ingestExec(t, ingestPanicReader{}, "ingest")
	if stdout != "" || stderr != "" {
		t.Errorf("stdout %q stderr %q, want both empty", stdout, stderr)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "panic") {
		t.Errorf("log = %q, want one line about the panic", log)
	}
	if _, err := ingestRun(context.Background(), ingestPanicReader{}, false); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Errorf("ingestRun = %v, want the panic as an error", err)
	}
}

// TestIngestPassesParserNowToStore: the store refuses a whole reading the
// parser would keep unless both check it against one now. The clock moves
// on with every call and the document's at is exactly 24 hours before the
// first, the oldest the parser takes.
func TestIngestPassesParserNowToStore(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	base := ingestSpecNow
	calls := 0
	prev := clock
	clock = func() time.Time { calls++; return base.Add(time.Duration(calls-1) * time.Millisecond) }
	t.Cleanup(func() { clock = prev })

	doc := fmt.Sprintf(`{"schema_version":1,"session_id":"s1","at":%q,"cost_usd":2.5}`, base.Add(-24*time.Hour).Format(time.RFC3339))
	ingestExec(t, strings.NewReader(doc), "ingest")
	if got := ingestSnap(t, db, "s1"); got.sess.CostUSD != 2.5 || got.cost != 1 {
		t.Errorf("got %+v, want the cost stored (log %q)", got, readLog(t))
	}
}

// TestIngestUnknownSessionStoresNothing: an agent may name any session id.
// A reading is stored only for a session a hook created: for another id
// nothing is stored (no row, no cost, no usage sample) and the log says
// "unknown session" once. Once the session exists the same reading stores.
func TestIngestUnknownSessionStoresNothing(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)
	before := ingestSnap(t, db, "s1")

	ingestExec(t, strings.NewReader(ingestSpecDoc(t, "ghost")), "ingest")
	got := ingestSnap(t, db, "ghost")
	if got.found || got.cost != 0 || got.usage != 0 {
		t.Errorf("ghost: found %v, %d cost, %d usage samples; want nothing", got.found, got.cost, got.usage)
	}
	if s1 := ingestSnap(t, db, "s1"); s1 != before {
		t.Errorf("s1 changed: %+v", s1)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "unknown session") {
		t.Errorf("log = %q, want one line about an unknown session", log)
	}
	ingestSeed(t, db, "ghost") // the first hook arrives
	ingestExec(t, strings.NewReader(ingestSpecDoc(t, "ghost")), "ingest")
	if got = ingestSnap(t, db, "ghost"); got.usage != 2 || got.cost != 1 {
		t.Errorf("after the session exists: %d usage, %d cost samples, want 2 and 1", got.usage, got.cost)
	}
}

// TestIngestDoesNotCreateStore: a store that does not exist yet is not
// created by ingest (the status line runs before the first hook).
func TestIngestDoesNotCreateStore(t *testing.T) {
	testkit.HookHome(t)
	db := filepath.Join(t.TempDir(), "curia.db")
	t.Setenv("CURIA_DB", db)
	ingestFixClock(t, ingestSpecNow)
	ingestExec(t, strings.NewReader(ingestSpecDoc(t, "s1")), "ingest")
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("store file: %v, want it not created", err)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "store") {
		t.Errorf("log = %q, want one line about the missing store", log)
	}
}

func TestIngestArgumentsLoggedNotPrinted(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)
	stdout, stderr := ingestExec(t, strings.NewReader(ingestSpecDoc(t, "s1")), "ingest", "--bogus", "x", "--help")
	if stdout != "" || stderr != "" {
		t.Errorf("stdout %q stderr %q, want both empty", stdout, stderr)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "argument") {
		t.Errorf("log = %q, want one line about the ignored arguments", log)
	}
	if got := ingestSnap(t, db, "s1"); got.sess.CostUSD != 4.12 {
		t.Errorf("cost %v, want the document stored", got.sess.CostUSD)
	}
}

// TestIngestLatency is TestHookLatency's method: sequential in-process runs
// on a database with 50 sessions and 5000 samples, the store opened once as
// TestHookLatency opens it, 1000 runs in one pass (Ruling 114), p95 under
// 20 ms, no retry. Each run reads,
// parses and stores a full reading (both windows, cost and cache) that
// changes a value, so every step writes. It also reports, ungated, runs
// that open and close the store themselves, as a process does; the
// process gate is TestStatuslineProcessLatency.
func TestIngestLatency(t *testing.T) {
	db := testkit.HookHome(t)
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i)
	}
	ingestSeed(t, db, ids...)
	now := time.Now().UTC().Truncate(time.Second)
	ingestFillSamples(t, db, ids, now)
	if got := ingestSnap(t, db, "s0"); got.usage+got.cost != 5000 {
		t.Fatalf("filled %d samples, want 5000", got.usage+got.cost)
	}
	// The captured status-line payload carries context_window, as every real one does.
	doc := func(i int) string {
		return string(statuslineLivePayload(t, "statusline-captured.json", ids[i%len(ids)], time.Now(), 10+float64(i)/100))
	}
	const runs, gatedRuns = 200, 1000 // reported; gated (Ruling 114)
	timed := func(from, runs int) []time.Duration {
		d := make([]time.Duration, 0, runs)
		for i := range runs {
			in := strings.NewReader(doc(from + i)) // built before the clock starts
			start := time.Now()
			if _, err := ingestRun(context.Background(), in, true); err != nil {
				t.Fatal(err)
			}
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		return d
	}
	opening := timed(0, runs)

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prev := ingestOpen
	ingestOpen = func() (*store.Store, func(), error) { return st, func() {}, nil }
	t.Cleanup(func() { ingestOpen = prev })
	from := runs
	latgate.Gate(t, "ingest", 20*time.Millisecond, func() time.Duration {
		d := timed(from, gatedRuns)
		from += gatedRuns
		t.Logf("ingest on an open store over %d runs: p50 %v, p95 %v, max %v", gatedRuns, d[gatedRuns/2], d[gatedRuns*95/100], d[gatedRuns-1])
		return d[gatedRuns*95/100]
	})

	t.Logf("ingest opening the store each run, over %d runs (not gated): p50 %v, p95 %v, max %v",
		runs, opening[runs/2], opening[runs*95/100], opening[runs-1])
	if got := ingestSnap(t, db, "s0"); got.cost != 5000/2+from {
		t.Errorf("%d cost samples, want every run stored (log %q)", got.cost, readLog(t))
	}
}

// ingestFillSamples writes, in one transaction, 2500 cost samples and 2500
// usage samples spread over the last 20 hours, as store.Ingest stores them.
func ingestFillSamples(t *testing.T, db string, ids []string, now time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	tx, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	const layout = "2006-01-02T15:04:05.000000000Z" // the store's
	for i := range 2500 {
		at := now.Add(-20 * time.Hour).Add(time.Duration(i) * 28 * time.Second).Format(layout)
		w := model.WindowFiveHour
		if i%2 == 1 {
			w = model.WindowSevenDay
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO cost_samples (at, session_id, cost_usd) VALUES (?, ?, ?)`,
			at, ids[i%len(ids)], float64(i)/10); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_samples (at, window, used_pct, resets_at, session_id) VALUES (?, ?, ?, ?, ?)`,
			at, string(w), float64(i%100), now.Add(time.Hour).Format(layout), ids[i%len(ids)]); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestIngestStatuslineSetsContextWindow(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC))
	doc := strings.ReplaceAll(string(ingestFixture(t, "statusline-captured.json")), "5cd98763-d94d-417d-b2ee-d172b8f63e81", "s1")
	ingestExec(t, strings.NewReader(doc), "ingest", "--statusline")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s, _, _ := st.GetSession(context.Background(), "s1")
	if s.ContextMax != 1000000 || s.ContextUsed*100/s.ContextMax != 4 {
		t.Errorf("used %d max %d, want 4%% of 1000000", s.ContextUsed, s.ContextMax)
	}
}

// ingestLogAt makes the log's rate limit see now, which the test moves.
func ingestLogAt(t *testing.T, now *time.Time) {
	t.Helper()
	prev := ingestLogNow
	ingestLogNow = func() time.Time { return *now }
	t.Cleanup(func() { ingestLogNow = prev })
}

// TestIngestLogBoundedPerKind: a problem that persists is logged once per
// interval per kind, across runs (each run is a process, so the state is a
// file), the first time always, and again once it has been gone.
func TestIngestLogBoundedPerKind(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ingestLogAt(t, &now)
	run := func(doc string) { ingestExec(t, strings.NewReader(doc), "ingest") }

	for range 6 { // three a second for two seconds
		run("SECRET-PAYLOAD garbage")
		now = now.Add(300 * time.Millisecond)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "refused") {
		t.Fatalf("after 6 refused documents: log = %q, want one line", log)
	}
	run(ingestSpecDoc(t, "ghost")) // another kind: logged at once
	if log := readLog(t); len(log) != 2 || !strings.Contains(log[1], "unknown session") {
		t.Fatalf("unknown session: log = %q, want a second line naming it", log)
	}
	run(ingestSpecDoc(t, "ghost"))
	run("SECRET-PAYLOAD garbage")
	if log := readLog(t); len(log) != 2 {
		t.Fatalf("repeats inside the interval: log = %q, want still two lines", log)
	}
	now = now.Add(time.Minute)
	run("SECRET-PAYLOAD garbage")
	if log := readLog(t); len(log) != 3 {
		t.Fatalf("after the interval: log = %q, want a third line", log)
	}
	if strings.Contains(strings.Join(readLog(t), "\n"), "SECRET") {
		t.Errorf("the log holds payload content: %q", readLog(t))
	}
}

// TestIngestLogAgainWhenProblemReturns: a kind that was gone for a while
// is logged when it returns, inside the interval; one that only alternates
// with good runs (several sessions share the log) is not.
func TestIngestLogAgainWhenProblemReturns(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "s1")
	ingestFixClock(t, ingestSpecNow)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ingestLogAt(t, &now)
	run := func(doc string) { ingestExec(t, strings.NewReader(doc), "ingest") }

	run("garbage")
	now = now.Add(time.Second)
	run(ingestSpecDoc(t, "s1")) // a good run in between
	now = now.Add(time.Second)
	run("garbage")
	if log := readLog(t); len(log) != 1 {
		t.Fatalf("alternating with a good run: log = %q, want one line", log)
	}
	now = now.Add(30 * time.Second) // gone for half a minute
	run(ingestSpecDoc(t, "s1"))
	now = now.Add(time.Second)
	run("garbage")
	if log := readLog(t); len(log) != 2 {
		t.Fatalf("problem returned after it cleared: log = %q, want a second line", log)
	}
}

// TestIngestLogMarkerCannotFailTheRun: a marker that cannot be read or
// written changes nothing but the bound.
func TestIngestLogMarkerCannotFailTheRun(t *testing.T) {
	testkit.HookHome(t)
	ingestFixClock(t, ingestSpecNow)
	marker := config.Default().LogPath + ".throttle"
	if err := os.MkdirAll(filepath.Join(marker, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := ingestExec(t, strings.NewReader("garbage"), "ingest")
	if stdout != "" || stderr != "" {
		t.Errorf("stdout %q stderr %q, want both empty", stdout, stderr)
	}
	if log := readLog(t); len(log) != 1 {
		t.Errorf("log = %q, want the line", log)
	}
}

// TestIngestLogMarkerStaysSmall: the marker holds one entry per kind, never
// anything from the payload.
func TestIngestLogMarkerStaysSmall(t *testing.T) {
	testkit.HookHome(t)
	ingestFixClock(t, ingestSpecNow)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ingestLogAt(t, &now)
	for i := range 50 {
		ingestExec(t, strings.NewReader(fmt.Sprintf("garbage %d", i)), "ingest", fmt.Sprintf("--x%d", i))
		now = now.Add(7 * time.Second)
	}
	b, err := os.ReadFile(config.Default().LogPath + ".throttle")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 512 || strings.Contains(string(b), "garbage") {
		t.Errorf("marker %q: too large or holds payload", b)
	}
}

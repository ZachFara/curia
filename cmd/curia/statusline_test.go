package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/install"
	latgate "github.com/ZachFara/curia/internal/latency"
	"github.com/ZachFara/curia/internal/testkit"
)

// statuslineNow sits inside every status-line fixture's windows.
var statuslineNow = time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)

// statuslineBuiltText is what statusline-built.json shows.
const statuslineBuiltText = "Opus 5.5 · ctx 71% · $4.12 · 5h 62%"

// statuslineCheckLine fails t unless out is exactly one line of at most 80
// cells with no control or format rune.
func statuslineCheckLine(t *testing.T, out string) {
	t.Helper()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Errorf("stdout %q is not exactly one line", out)
	}
	line := strings.TrimSuffix(out, "\n")
	cells := 0
	for _, r := range line {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			t.Errorf("stdout %q holds the control rune %U", out, r)
		}
		cells++
		if r >= 0x1100 {
			cells++
		}
	}
	if cells > 80 {
		t.Errorf("stdout %q is %d cells wide, want at most 80", out, cells)
	}
}

func TestStatuslinePrintsText(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "built-session-1")
	ingestFixClock(t, statuslineNow)

	stdout, stderr := ingestExec(t, bytes.NewReader(ingestFixture(t, "statusline-built.json")), "statusline")
	if stdout != statuslineBuiltText+"\n" {
		t.Errorf("stdout = %q, want %q", stdout, statuslineBuiltText+"\n")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	got := ingestSnap(t, db, "built-session-1")
	if got.sess.CostUSD != 4.12 || got.usage != 2 || got.cost != 1 || got.sess.CacheRecacheTokens != 42211 {
		t.Errorf("stored %+v, want the cost, the cache, two usage samples and one cost sample", got)
	}
	if log := readLog(t); log != nil {
		t.Errorf("log = %q, want none", log)
	}
}

// TestStatuslineCapturedPayloads: the real payloads of probe S1 print and
// store as expected, the start-up one included (no rate_limits yet).
func TestStatuslineCapturedPayloads(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"statusline-captured.json", "Haiku 5.5 · ctx 4% · $0.01 · 5h 40%\n"},
		{"statusline-captured-first-response.json", ""},
		{"statusline-captured-startup.json", "Haiku 5.5 · $0.00\n"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			testkit.HookHome(t)
			ingestFixClock(t, statuslineNow)
			stdout, stderr := ingestExec(t, bytes.NewReader(ingestFixture(t, tc.file)), "statusline")
			statuslineCheckLine(t, stdout)
			if tc.want != "" && stdout != tc.want {
				t.Errorf("stdout = %q, want %q", stdout, tc.want)
			}
			if !strings.HasPrefix(stdout, "Haiku 5.5") || stderr != "" {
				t.Errorf("stdout %q stderr %q", stdout, stderr)
			}
		})
	}
}

// statuslinePayload is statusline-built.json with the model's display
// name set to name.
func statuslinePayload(t *testing.T, name string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ingestFixture(t, "statusline-built.json"), &m); err != nil {
		t.Fatal(err)
	}
	m["model"] = map[string]any{"display_name": name}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStatuslineAlwaysPrintsOneLine(t *testing.T) {
	never := func() io.Reader {
		r, w := io.Pipe()
		t.Cleanup(func() { _ = w.Close() })
		return r
	}
	for _, tc := range []struct {
		name  string
		stdin func(t *testing.T) io.Reader
		setup func(t *testing.T)
		want  string // "" checks only the shape
	}{
		{"empty stdin", func(*testing.T) io.Reader { return strings.NewReader("") }, nil, "\n"},
		{"garbage", func(*testing.T) io.Reader { return strings.NewReader("\x1b[31mgarbage\n\n") }, nil, "\n"},
		{"one MiB", func(*testing.T) io.Reader { return strings.NewReader(strings.Repeat("{", 1<<20)) }, nil, "\n"},
		{"stdin that never closes", func(*testing.T) io.Reader { return never() }, nil, "\n"},
		{"stdin that panics", func(*testing.T) io.Reader { return ingestPanicReader{} }, nil, "\n"},
		{"unreachable store", func(t *testing.T) io.Reader {
			return bytes.NewReader(ingestFixture(t, "statusline-built.json"))
		}, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CURIA_DB", filepath.Join(file, "curia.db"))
		}, statuslineBuiltText + "\n"},
		{"escape sequences in the model name", func(t *testing.T) io.Reader {
			return strings.NewReader(statuslinePayload(t, "\x1b[31mEvil\x1b]0;title\x07\u202eX\u2028Y\r\nZ\x00"))
		}, nil, ""},
		{"a very long and wide model name", func(t *testing.T) io.Reader {
			return strings.NewReader(statuslinePayload(t, strings.Repeat("模型", 60)))
		}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testkit.HookHome(t)
			ingestFixClock(t, statuslineNow)
			if tc.setup != nil {
				tc.setup(t)
			}
			start := time.Now()
			stdout, stderr := ingestExec(t, tc.stdin(t), "statusline")
			if d := time.Since(start); d > time.Second {
				t.Errorf("took %v, want well under a second", d)
			}
			statuslineCheckLine(t, stdout)
			if tc.want != "" && stdout != tc.want {
				t.Errorf("stdout = %q, want %q", stdout, tc.want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
			if strings.Contains(stdout, "title") {
				t.Errorf("stdout %q holds the text of an OSC sequence", stdout)
			}
		})
	}
}

func TestStatuslineUnreachableStoreStillShowsText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, db string)
	}{
		{"store cannot be opened", func(t *testing.T, _ string) {
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CURIA_DB", filepath.Join(file, "curia.db"))
		}},
		{"store missing", func(t *testing.T, _ string) { t.Setenv("CURIA_DB", filepath.Join(t.TempDir(), "none", "curia.db")) }},
		{"config does not parse", ingestBadConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.HookHome(t)
			ingestFixClock(t, statuslineNow)
			tc.setup(t, db)
			stdout, stderr := ingestExec(t, bytes.NewReader(ingestFixture(t, "statusline-built.json")), "statusline")
			if stdout != statuslineBuiltText+"\n" || stderr != "" {
				t.Errorf("stdout %q stderr %q, want the text and nothing else", stdout, stderr)
			}
			if log := readLog(t); len(log) != 1 {
				t.Errorf("log = %q, want one line", log)
			}
		})
	}
}

// TestStatuslineStoreBusyStillPrints: while another process holds the
// write lock, the status line gives up on the ingest within its budget and
// still prints.
func TestStatuslineStoreBusyStillPrints(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "built-session-1")
	ingestFixClock(t, statuslineNow)
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()

	start := time.Now()
	stdout, stderr := ingestExec(t, bytes.NewReader(ingestFixture(t, "statusline-built.json")), "statusline")
	d := time.Since(start)
	if d < statuslineLimit/2 || d > statuslineLimit+150*time.Millisecond {
		t.Errorf("took %v, want about the %v budget", d, statuslineLimit)
	}
	if stdout != statuslineBuiltText+"\n" || stderr != "" {
		t.Errorf("stdout %q stderr %q, want the text and nothing else", stdout, stderr)
	}
	if got := ingestSnap(t, db, "built-session-1"); got.sess.CostKnown {
		t.Errorf("cost stored while the store was locked: %+v", got)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "deadline") {
		t.Errorf("log = %q, want one line saying the store did not answer in time", log)
	}
}

// statuslineBinary builds curia once (not -race) and returns its path. HOME
// is the developer's for the build only, so the build and module caches are
// found; GOPROXY=off keeps it off the network.
var statuslineBinary = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp(guardDir, "bin-") // removed with the guard's directory
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "curia")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "HOME="+guardOuterHome, "GOPROXY=off", "GOFLAGS=-mod=readonly")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, out)
	}
	return bin, nil
})

func statuslineBuilt(t *testing.T) string {
	t.Helper()
	bin, err := statuslineBinary()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

// statuslineEnv is an agent's status-line environment (probe S1) with
// HOME and CURIA_DB in home.
func statuslineEnv(home string) []string {
	return append(os.Environ(), "HOME="+home, "CURIA_DB="+filepath.Join(home, "curia.db"),
		"CLAUDECODE=1", "TMUX_PANE=%1", "CURIA_TASK_ID=T-1", "CLAUDE_CODE_SESSION_ID=built-session-1")
}

// statuslineLivePayload is a captured payload with its times moved to now
// and its cost set, so every part of it is inside the ingest bounds.
func statuslineLivePayload(t *testing.T, file, id string, now time.Time, cost float64) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ingestFixture(t, file), &m); err != nil {
		t.Fatal(err)
	}
	m["session_id"] = id
	if c, ok := m["cost"].(map[string]any); ok {
		c["total_cost_usd"] = cost
	}
	if rl, ok := m["rate_limits"].(map[string]any); ok {
		for w, ahead := range map[string]time.Duration{"five_hour": 2 * time.Hour, "seven_day": 72 * time.Hour} {
			if l, ok := rl[w].(map[string]any); ok {
				l["resets_at"] = now.Add(ahead).Unix()
			}
		}
	}
	if pc, ok := m["prompt_cache"].(map[string]any); ok {
		pc["expires_at"] = now.Add(30 * time.Minute).Unix()
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// statuslineWriteFile writes b to a new file in dir and returns its path.
func statuslineWriteFile(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStatuslineSurvivesClosedStdout: the built binary with its stdout a
// pipe nobody reads exits 0 (SIGPIPE ignored) and writes nothing to stderr.
func TestStatuslineSurvivesClosedStdout(t *testing.T) {
	bin := statuslineBuilt(t)
	home := t.TempDir()
	ingestSeed(t, filepath.Join(home, "curia.db"), "built-session-1")
	for _, args := range [][]string{{"statusline"}, {"ingest", "--statusline"}} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		cmd := exec.CommandContext(t.Context(), bin, args...)
		cmd.Env = statuslineEnv(home)
		cmd.Stdin = bytes.NewReader(statuslineLivePayload(t, "statusline-captured.json", "built-session-1", time.Now(), 1.25))
		cmd.Stdout = w
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err = cmd.Run()
		_ = w.Close()
		if err != nil {
			t.Errorf("%v: %v, want exit 0", args, err)
		}
		if stderr.Len() != 0 {
			t.Errorf("%v: stderr %q", args, stderr.String())
		}
	}
	if got := ingestSnap(t, filepath.Join(home, "curia.db"), "built-session-1"); got.sess.CostUSD != 1.25 {
		t.Errorf("cost %v, want 1.25 stored", got.sess.CostUSD)
	}
}

// TestStatuslineInstallerCommandsAccepted runs, under /bin/sh, exactly the
// two commands the installer writes or suggests (internal/install): the
// registered one prints the line, the chained one prints only the user's.
func TestStatuslineInstallerCommandsAccepted(t *testing.T) {
	built := statuslineBuilt(t)
	// A path that needs quoting, as the installer's shellQuote handles.
	dir := filepath.Join(t.TempDir(), "my tools 'q' $HOME")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "curia")
	if err := os.Symlink(built, bin); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	db := filepath.Join(home, "curia.db")
	ingestSeed(t, db, "built-session-1")
	theirs := "(cat >/dev/null; echo their-line)"
	chain := install.StatusLineChain(bin)
	if !strings.Contains(chain, "<your command>") {
		t.Fatalf("chain %q has no placeholder", chain)
	}
	for i, tc := range []struct{ command, want string }{
		{install.StatusLineCommand(bin), "Opus 5.5 · ctx 71% · $1.50 · 5h 62%\n"},
		{strings.Replace(chain, "<your command>", theirs, 1), "their-line\n"},
	} {
		payload, err := json.Marshal(map[string]any{
			"session_id":     "built-session-1",
			"model":          map[string]any{"display_name": "Opus 5.5"},
			"cost":           map[string]any{"total_cost_usd": 1.5 + float64(i)},
			"context_window": map[string]any{"used_percentage": 71},
			"rate_limits": map[string]any{
				"five_hour": map[string]any{"used_percentage": 62, "resets_at": time.Now().Add(time.Hour).Unix()},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", tc.command)
		cmd.Env = statuslineEnv(home)
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Errorf("%q: %v (stderr %q)", tc.command, err, stderr.String())
		}
		if stdout.String() != tc.want || stderr.Len() != 0 {
			t.Errorf("%q: stdout %q stderr %q, want %q and nothing else", tc.command, stdout.String(), stderr.String(), tc.want)
		}
		if got := ingestSnap(t, db, "built-session-1"); got.sess.CostUSD != 1.5+float64(i) {
			t.Errorf("%q: cost %v, want %v stored", tc.command, got.sess.CostUSD, 1.5+float64(i))
		}
	}
	// The command tree takes both forms as they are split.
	for _, args := range [][]string{{"statusline"}, {"ingest", "--statusline"}} {
		c, rest, err := rootCmd.Find(args)
		if err != nil || c.Name() != args[0] || !slices.Equal(rest, args[1:]) {
			t.Errorf("Find(%q) = %v, %q, %v", args, c.Name(), rest, err)
		}
	}
}

// TestStatuslineStartsNoProcess: the two commands' own files import no
// package that starts a process or opens a connection, and a run with an
// empty PATH asks tmux nothing.
func TestStatuslineStartsNoProcess(t *testing.T) {
	for _, file := range []string{"ingest.go", "statusline.go"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); p == "os/exec" || p == "net" || strings.HasPrefix(p, "net/") ||
				strings.HasSuffix(p, "/internal/tmux") || strings.HasSuffix(p, "/internal/linear") {
				t.Errorf("%s imports %s", file, p)
			}
		}
	}
	db := testkit.HookHome(t)
	ingestSeed(t, db, "built-session-1")
	ingestFixClock(t, statuslineNow)
	fake := useFakeTmux(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("TMUX_PANE", "%1")
	t.Setenv("CURIA_TASK_ID", "T-1")
	stdout, _ := ingestExec(t, bytes.NewReader(ingestFixture(t, "statusline-built.json")), "statusline")
	if stdout != statuslineBuiltText+"\n" {
		t.Errorf("stdout %q", stdout)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls %q, want none", fake.Calls)
	}
}

// TestStatuslineSigtermKeepsStoreSound: Claude Code SIGTERMs a run that a
// newer one supersedes (probe S1). Runs killed at random moments leave the
// store sound and the next run stores as usual.
func TestStatuslineSigtermKeepsStoreSound(t *testing.T) {
	bin := statuslineBuilt(t)
	home := t.TempDir()
	db := filepath.Join(home, "curia.db")
	ingestSeed(t, db, "built-session-1")
	in := t.TempDir()
	killed := 0
	for i := range 40 {
		payload := statuslineLivePayload(t, "statusline-captured.json", "built-session-1", time.Now(), float64(i))
		f, err := os.Open(statuslineWriteFile(t, in, fmt.Sprintf("p%d.json", i), payload))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), bin, "statusline")
		cmd.Env = statuslineEnv(home)
		cmd.Stdin = f
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i*373%15000) * time.Microsecond) // spread over the run's length
		_ = cmd.Process.Signal(syscall.SIGTERM)
		if err = cmd.Wait(); err != nil {
			killed++
		}
		_ = f.Close()
	}
	t.Logf("%d of 40 runs were killed by SIGTERM before they ended", killed)
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	var check string
	if err = raw.QueryRowContext(context.Background(), "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Errorf("integrity_check = %q, %v; want ok", check, err)
	}
	_ = raw.Close()

	cmd := exec.CommandContext(t.Context(), bin, "statusline")
	cmd.Env = statuslineEnv(home)
	cmd.Stdin = bytes.NewReader(statuslineLivePayload(t, "statusline-captured.json", "built-session-1", time.Now(), 99))
	if out, err := cmd.Output(); err != nil || !strings.HasPrefix(string(out), "Haiku 5.5") {
		t.Errorf("after the kills: stdout %q, err %v", out, err)
	}
	if got := ingestSnap(t, db, "built-session-1"); got.sess.CostUSD != 99 {
		t.Errorf("cost %v, want 99", got.sess.CostUSD)
	}
}

// TestStatuslineProcessLatency measures whole runs of the built binary as
// the e2e hook gates do (a sequential pass, files for stdin and stdout),
// over probe S1's captured payloads with their times moved to now, on a
// store with 50 sessions and 5000 samples: p95 under 50 ms, no retry.
func TestStatuslineProcessLatency(t *testing.T) {
	bin := statuslineBuilt(t)
	home := t.TempDir()
	db := filepath.Join(home, "curia.db")
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i)
	}
	ingestSeed(t, db, ids...)
	now := time.Now().UTC().Truncate(time.Second)
	ingestFillSamples(t, db, ids, now)
	files := []string{"statusline-captured.json", "statusline-captured-first-response.json", "statusline-captured-startup.json"}
	in := t.TempDir()
	out, err := os.Create(filepath.Clean(filepath.Join(in, "out")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	// Untimed warm-up: macOS checks a new executable on its first exec.
	if err = exec.CommandContext(t.Context(), bin, "--help").Run(); err != nil {
		t.Fatal(err)
	}
	const runs = 200
	latgate.Gate(t, "statusline process", 50*time.Millisecond, func() time.Duration {
		d := make([]time.Duration, 0, runs)
		for i := range runs {
			payload := statuslineLivePayload(t, files[i%len(files)], ids[i%len(ids)], time.Now(), 20+float64(i)/100)
			f, ferr := os.Open(statuslineWriteFile(t, in, fmt.Sprintf("p%d.json", i), payload))
			if ferr != nil {
				t.Fatal(ferr)
			}
			cmd := exec.CommandContext(t.Context(), bin, "statusline")
			cmd.Env = statuslineEnv(home)
			cmd.Stdin, cmd.Stdout = f, out
			start := time.Now()
			if err = cmd.Run(); err != nil {
				t.Fatal(err)
			}
			d = append(d, time.Since(start))
			_ = f.Close()
		}
		slices.Sort(d)
		t.Logf("curia statusline as a process over %d runs: p50 %v, p95 %v, max %v", runs, d[runs/2], d[runs*95/100], d[runs-1])
		return d[runs*95/100]
	})
	if got := ingestSnap(t, db, ids[(runs-1)%len(ids)]); !got.sess.CostKnown {
		t.Errorf("nothing stored: %+v", got)
	}
	if b, err := os.ReadFile(filepath.Clean(filepath.Join(home, ".local", "state", "curia", "curia.log"))); err == nil {
		t.Errorf("log = %q, want none", b)
	}
}

// TestStatuslineUnknownSessionPrintsStoresNothing: the status line may run
// before the session's first hook, and anything can run it with any id. It
// prints its line and exits 0, stores nothing and creates no session row;
// the problem is logged once however many runs repeat it. The first
// seconds of samples of a new session are lost: that is accepted.
func TestStatuslineUnknownSessionPrintsStoresNothing(t *testing.T) {
	db := testkit.HookHome(t)
	ingestSeed(t, db, "other")
	ingestFixClock(t, statuslineNow)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ingestLogAt(t, &now)
	prev := statuslineLimit
	statuslineLimit = 5 * time.Second // a loaded machine must not turn this into a timeout
	t.Cleanup(func() { statuslineLimit = prev })
	for range 5 {
		stdout, stderr := ingestExec(t, bytes.NewReader(ingestFixture(t, "statusline-built.json")), "statusline")
		if stdout != statuslineBuiltText+"\n" || stderr != "" {
			t.Fatalf("stdout %q stderr %q, want the text and nothing else", stdout, stderr)
		}
		now = now.Add(300 * time.Millisecond)
	}
	if got := ingestSnap(t, db, "built-session-1"); got.found || got.usage != 0 || got.cost != 0 {
		t.Errorf("unknown session stored %+v, want nothing", got)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "unknown session") {
		t.Errorf("log = %q, want one line about an unknown session", log)
	}
}

// TestStatuslineLogBoundedWhenStoreBroken: a store that cannot be opened is
// one log line per interval, not one per run.
func TestStatuslineLogBoundedWhenStoreBroken(t *testing.T) {
	db := testkit.HookHome(t)
	ingestFixClock(t, statuslineNow)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ingestLogAt(t, &now)
	ingestBadConfig(t, db)
	t.Setenv("CURIA_DB", filepath.Join(t.TempDir(), "none", "curia.db"))
	for range 20 {
		stdout, _ := ingestExec(t, bytes.NewReader(ingestFixture(t, "statusline-built.json")), "statusline")
		if stdout != statuslineBuiltText+"\n" {
			t.Fatalf("stdout %q", stdout)
		}
		now = now.Add(300 * time.Millisecond)
	}
	if log := readLog(t); len(log) != 1 {
		t.Errorf("log = %q, want one line", log)
	}
}

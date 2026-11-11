// Package e2e drives the built curia binary as Claude Code and tmux would,
// one process per call. It imports no internal package.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// rootToken stands for the repository root in the replay fixture (so
	// transcript paths are absolute, as Claude Code sends them) and in the
	// golden file.
	rootToken = "<root>"
	// timeToken replaces every non-null time in `curia ls --json`.
	timeToken = `"<time>"`
	// p95Budget is the p95 wall time of one hook process, start included.
	p95Budget = 50 * time.Millisecond
	// minTimedRuns is how many hook processes the latency pass times at least.
	minTimedRuns = 100
	// startBudget is how soon a started session must be in the store.
	startBudget = time.Second
)

// concurrentPanes run at the same time, one goroutine each; the lines of
// every other pane run afterwards, in file order.
var concurrentPanes = []string{"%1", "%2", "%3"}

// replayLine is one line of testdata/replay/five_sessions.jsonl.
type replayLine struct {
	Pane    string          `json:"pane"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// home runs the curia binary built for this test against its own temp home,
// database and tmux directory, with no reachable tmux server.
type home struct {
	bin, dir, logPath string
	env               []string
}

// hookRun is the wall time of one `curia hook` process.
type hookRun struct {
	event string
	took  time.Duration
}

// TestReplayFiveSessions replays five sessions in four panes through the
// built binary, S1 to S3 concurrently, then S4 and, in the same pane, S5, and
// checks the result. A second, sequential pass on a fresh store times the
// same events one process at a time.
func TestReplayFiveSessions(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	bin := build(t, root)
	lines := readReplay(t, root)

	h := newHome(t, bin)
	h.replay(t, lines)
	out := h.mustRun(t, "ls", "--json")
	want, err := os.ReadFile(filepath.Join("testdata", "ls.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := normalise(out, root); got != string(want) {
		t.Errorf("curia ls --json, normalised:\n%s\nwant (e2e/testdata/ls.golden.json):\n%s", got, want)
	}
	if strings.Contains(out, "44444444-") {
		t.Errorf("S4 was ended by S5 starting in its pane but is in curia ls --json:\n%s", out)
	}
	if got := h.mustRun(t, "status"); got != "W1" {
		t.Errorf("curia status = %q, want %q", got, "W1")
	}
	checkLog(t, h.logPath)

	checkLatency(t, newHome(t, bin).timeSequential(t, lines))
}

// build builds curia into a temp dir, execs it once untimed, and returns its
// path.
func build(t *testing.T, root string) string {
	t.Helper()
	statSources(t, root)
	bin := filepath.Join(t.TempDir(), "curia")
	// A normal build, not -race. -buildvcs=false: no git call, which under
	// the pre-commit hook would see lefthook's GIT_DIR.
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "./cmd/curia")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	// Untimed warm-up: macOS checks a new executable on its first exec
	// (about 300 ms here), once per binary, not per hook, so it is no part of
	// a hook's cost. --help reads no config and opens no store.
	if out, err := exec.Command(bin, "--help").CombinedOutput(); err != nil {
		t.Fatalf("curia --help: %v\n%s", err, out)
	}
	return bin
}

// newHome returns a home whose every run gets HOME and CURIA_DB in a new temp
// dir, TMUX unset and TMUX_TMPDIR an empty dir, so no tmux server is
// reachable and nothing real is touched.
func newHome(t *testing.T, bin string) *home {
	t.Helper()
	tmp := t.TempDir()
	dir, tmuxDir := filepath.Join(tmp, "home"), filepath.Join(tmp, "tmux")
	for _, d := range []string{dir, tmuxDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &home{
		bin:     bin,
		dir:     tmp,
		logPath: filepath.Join(dir, ".local", "state", "curia", "curia.log"),
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + dir,
			"CURIA_DB=" + filepath.Join(tmp, "c.db"),
			"TMUX_TMPDIR=" + tmuxDir,
		},
	}
}

// statSources stats every file the binary is built from. This package
// imports none of them, so without this `go test` would reuse a cached pass
// after they change; it keys its cache on the files a test stats.
func statSources(t *testing.T, root string) {
	t.Helper()
	for _, f := range []string{"go.mod", "go.sum"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			_, err = os.Stat(path)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// readReplay reads the replay script and puts root in place of rootToken.
func readReplay(t *testing.T, root string) []replayLine {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(filepath.Join(root, "testdata", "replay", "five_sessions.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	escaped := string(quoted[1 : len(quoted)-1])
	var lines []replayLine
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var l replayLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("replay line %q: %v", sc.Text(), err)
		}
		l.Payload = bytes.ReplaceAll(l.Payload, []byte(rootToken), []byte(escaped))
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// replay runs every line as `curia hook <event>`: the lines of each
// concurrent pane in order on their own goroutine, then the rest in order.
func (h *home) replay(t *testing.T, lines []replayLine) {
	t.Helper()
	var wg sync.WaitGroup
	for _, pane := range concurrentPanes {
		wg.Go(func() {
			for _, l := range lines {
				if l.Pane == pane {
					h.hook(t, l)
				}
			}
		})
	}
	wg.Wait()
	for _, l := range lines {
		if !slices.Contains(concurrentPanes, l.Pane) {
			h.hook(t, l)
		}
	}
}

// timeSequential runs the lines in file order, one process at a time, as
// many times over as it takes to time at least minTimedRuns runs, and
// returns the wall time of every run.
func (h *home) timeSequential(t *testing.T, lines []replayLine) []hookRun {
	t.Helper()
	var runs []hookRun
	for len(runs) < minTimedRuns {
		for _, l := range lines {
			runs = append(runs, hookRun{l.Event, h.hook(t, l)})
		}
	}
	return runs
}

// hook runs one replay line, checks it exited 0 and printed nothing, and
// returns its wall time. It is safe to call from several goroutines.
func (h *home) hook(t *testing.T, l replayLine) time.Duration {
	stdout, stderr, took, err := h.run(l.Payload, "TMUX_PANE="+l.Pane, "hook", l.Event)
	if err != nil || stdout != "" || stderr != "" {
		t.Errorf("curia hook %s in %s: err %v, stdout %q, stderr %q, want exit 0 and no output",
			l.Event, l.Pane, err, stdout, stderr)
	}
	return took
}

// mustRun runs curia with no stdin and returns its stdout; it fails the test
// unless the run exits 0 with nothing on stderr.
func (h *home) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, _, err := h.run(nil, "", args...)
	if err != nil || stderr != "" {
		t.Fatalf("curia %s: err %v, stderr %q, want exit 0 and no stderr", strings.Join(args, " "), err, stderr)
	}
	return stdout
}

// run runs curia with args, stdin and h.env plus extraEnv (if not empty).
func (h *home) run(stdin []byte, extraEnv string, args ...string) (stdout, stderr string, took time.Duration, err error) {
	cmd := exec.Command(h.bin, args...)
	cmd.Dir = h.dir
	cmd.Env = slices.Clone(h.env)
	if extraEnv != "" {
		cmd.Env = append(cmd.Env, extraEnv)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	began := time.Now()
	err = cmd.Run()
	return out.String(), errOut.String(), time.Since(began), err
}

// checkLatency logs p50, p95 and max per event and overall, and fails if the
// p95 over every run reaches p95Budget or a SessionStart takes startBudget.
func checkLatency(t *testing.T, runs []hookRun) {
	t.Helper()
	byEvent := map[string][]time.Duration{}
	var all []time.Duration
	for _, r := range runs {
		byEvent[r.event] = append(byEvent[r.event], r.took)
		all = append(all, r.took)
	}
	events := make([]string, 0, len(byEvent))
	for e := range byEvent {
		events = append(events, e)
	}
	slices.Sort(events)
	for _, e := range events {
		t.Logf("%-12s %s", e, stats(byEvent[e]))
	}
	t.Logf("%-12s %s", "all", stats(all))
	if p := percentile(all, 95); p >= p95Budget {
		t.Errorf("p95 wall time of curia hook = %v, want under %v", p, p95Budget)
	}
	if m := slices.Max(byEvent["SessionStart"]); m >= startBudget {
		t.Errorf("slowest SessionStart took %v, want the session stored within %v", m, startBudget)
	}
}

func stats(d []time.Duration) string {
	return fmt.Sprintf("n=%d p50=%v p95=%v max=%v", len(d), percentile(d, 50), percentile(d, 95), slices.Max(d))
}

// percentile is the nearest-rank p-th percentile of d.
func percentile(d []time.Duration, p float64) time.Duration {
	s := slices.Sorted(slices.Values(d))
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[max(rank, 1)-1]
}

// jsonTime matches a JSON string that may hold a time.
var jsonTime = regexp.MustCompile(`"\d{4}-\d\d-\d\dT[^"]*"`)

// normalise replaces what cannot be known in advance in `curia ls --json`:
// every string that is an RFC 3339 time becomes timeToken (null stays null),
// and root at the start of a string becomes rootToken.
func normalise(out, root string) string {
	out = jsonTime.ReplaceAllStringFunc(out, func(s string) string {
		if _, err := time.Parse(time.RFC3339Nano, s[1:len(s)-1]); err != nil {
			return s
		}
		return timeToken
	})
	quoted, _ := json.Marshal(root + "/")
	return strings.ReplaceAll(out, strings.TrimSuffix(string(quoted), `"`), `"`+rootToken+"/")
}

// logEntry matches the level, message and, when the first attribute is a
// pane or a cwd, that attribute of one log line.
var logEntry = regexp.MustCompile(`^time=\S+ level=(\S+) msg=("[^"]*"|\S+)(?: ((?:pane|cwd)=\S+))?`)

// expectedLog is every log entry the replay should leave, as level, message
// and pane or cwd, sorted. With no tmux server, each SessionStart in a pane
// logs that tmux could not say which window holds the pane, and ls logs that
// it did not reconcile. Each payload cwd does not exist, so git cannot change
// into it, which is not "not a git repository" and is logged.
var expectedLog = []string{
	`WARN "git failed" cwd=/nonexistent/replay/api`,
	`WARN "git failed" cwd=/nonexistent/replay/cli`,
	`WARN "git failed" cwd=/nonexistent/replay/docs`,
	`WARN "git failed" cwd=/nonexistent/replay/docs`,
	`WARN "git failed" cwd=/nonexistent/replay/web`,
	`WARN "tmux pane info" pane=%1`,
	`WARN "tmux pane info" pane=%2`,
	`WARN "tmux pane info" pane=%3`,
	`WARN "tmux pane info" pane=%4`,
	`WARN "tmux pane info" pane=%4`,
	`WARN "tmux unreachable, not reconciling"`,
}

// checkLog fails unless the log holds exactly the expected entries.
func checkLog(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for line := range strings.Lines(string(data)) {
		m := logEntry.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("log line not in slog text format: %q", line)
			continue
		}
		got = append(got, strings.TrimSpace(strings.Join(m[1:], " ")))
	}
	slices.Sort(got)
	if !slices.Equal(got, expectedLog) {
		t.Errorf("log entries:\n%s\nwant:\n%s\nlog:\n%s",
			strings.Join(got, "\n"), strings.Join(expectedLog, "\n"), data)
	}
}

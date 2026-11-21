package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/usage"
)

// costlensRealRun is the real costlensRun, kept before init replaces it.
var costlensRealRun = costlensRun

// No test may start the real analyzer or node: a test that has not set its own lookup finds neither node nor
// cost-lens, and a test that reaches a run it did not set up panics.
func init() {
	costlensLookPath = func(file string) (string, error) {
		return "", fmt.Errorf("%s: %w (costlensLookPath not set by this test)", file, exec.ErrNotFound)
	}
	costlensRun = func(context.Context, string, string, string) ([]byte, error) {
		panic("costlensRun: this test did not set its own")
	}
}

// costlensFake is a costlensRun that records each call and, while release is
// not nil, blocks until release is closed or its context ends.
type costlensFake struct {
	mu      sync.Mutex
	calls   [][]string // node, analyzer, project
	release chan struct{}
	out     []byte
	err     error
}

func (f *costlensFake) run(ctx context.Context, node, analyzer, project string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, []string{node, analyzer, project})
	rel, out, err := f.release, f.out, f.err
	f.mu.Unlock()
	if rel != nil {
		select {
		case <-rel:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, err
}

func (f *costlensFake) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *costlensFake) call(i int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls[i])
}

// costlensWaitCalls waits, at most 10 s, until fake has been called n times.
func costlensWaitCalls(t *testing.T, fake *costlensFake, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); fake.n() < n; {
		if time.Now().After(deadline) {
			t.Fatalf("costlensRun called %d times, want %d", fake.n(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// costlensUse makes this test's runs call fake and its lookups lookPath, with
// a fresh process cache; all three are put back afterwards.
func costlensUse(t *testing.T, lookPath func(string) (string, error), fake *costlensFake) {
	t.Helper()
	prevLook, prevRun := costlensLookPath, costlensRun
	costlensLookPath = lookPath
	costlensRun = fake.run
	costlensProcReset()
	t.Cleanup(func() {
		costlensProcReset()
		costlensLookPath, costlensRun = prevLook, prevRun
	})
}

// costlensProcReset forgets the process cache, waiting for its run in flight.
func costlensProcReset() {
	costlensProcMu.Lock()
	c := costlensProc
	costlensProc = nil
	costlensProcMu.Unlock()
	if c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.Wait(ctx)
	}
}

// costlensLookIn is a lookup that finds file in dir when anything is there
// under that name (exec.LookPath would also want an execute bit, which no
// test writes).
func costlensLookIn(dir string) func(string) (string, error) {
	return func(file string) (string, error) {
		p := filepath.Join(dir, file)
		if _, err := os.Lstat(p); err != nil {
			return "", fmt.Errorf("%s: %w", file, exec.ErrNotFound)
		}
		return p, nil
	}
}

// costlensFile writes content at path and returns path.
func costlensFile(t *testing.T, path, content string) string {
	t.Helper()
	testkit.WorktreeWrite(t, path, content)
	return path
}

// costlensSetup is a config naming an analyzer file and a bin directory
// holding a "node" link to /bin/sh.
func costlensSetup(t *testing.T) (cfg config.Config, bin string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(bin, "node")); err != nil {
		t.Fatal(err)
	}
	cfg = config.Default()
	cfg.LogPath = filepath.Join(dir, "curia.log")
	cfg.CostLens.Analyzer = costlensFile(t, filepath.Join(dir, "pkg", "analyzer", "cli.js"), "// analyzer\n")
	return cfg, bin
}

// costlensSummary is the built analyzer summary, whose branch curia/T-1-fix
// costs 5.03, with that branch renamed to branch.
func costlensSummary(t *testing.T, branch string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "usage", "testdata", "summary-built.json"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(b, []byte(`"curia/T-1-fix"`), []byte(strconv.Quote(branch)))
}

// costlensClock is a settable clock.
type costlensClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *costlensClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *costlensClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// costlensIs503 reports whether usd is 5.03, the built summary's branch cost, to within rounding.
func costlensIs503(usd float64) bool { return math.Abs(usd-5.03) < 1e-9 }

// costlensWait waits for c's run in flight, at most 10 s.
func costlensWait(t *testing.T, c *costlensCache) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Wait(ctx)
	if ctx.Err() != nil {
		t.Fatal("the analyzer run did not end")
	}
}

func TestCostlensAnalyzerFromConfig(t *testing.T) {
	cfg, bin := costlensSetup(t)
	t.Setenv("PATH", bin)
	costlensUse(t, exec.LookPath, &costlensFake{})
	node, analyzer, ok := costlensAnalyzer(cfg)
	if !ok || node != filepath.Join(bin, "node") || analyzer != cfg.CostLens.Analyzer {
		t.Errorf("costlensAnalyzer = %q %q %v, want %q %q true", node, analyzer, ok, filepath.Join(bin, "node"), cfg.CostLens.Analyzer)
	}

	// A configured analyzer that is not a regular file is no analyzer; the one on PATH is not looked for.
	for _, bad := range []string{filepath.Join(t.TempDir(), "gone.js"), t.TempDir()} {
		c := cfg
		c.CostLens.Analyzer = bad
		if _, _, ok := costlensAnalyzer(c); ok {
			t.Errorf("analyzer %q: ok true", bad)
		}
	}
}

// costlensPackage lays out a cost-lens package in a temp directory: target
// (relative to the package) written, cost-lens in bin linked to it, and
// analyzer/cli.js written when withAnalyzer is set.
func costlensPackage(t *testing.T, target string, withAnalyzer bool) (bin, pkg string) {
	t.Helper()
	_, bin = costlensSetup(t)
	pkg = filepath.Join(t.TempDir(), "node_modules", "cost-lens")
	costlensFile(t, filepath.Join(pkg, target), "// terminal\n")
	if withAnalyzer {
		costlensFile(t, filepath.Join(pkg, "analyzer", "cli.js"), "// analyzer\n")
	}
	if err := os.Symlink(filepath.Join(pkg, target), filepath.Join(bin, "cost-lens")); err != nil {
		t.Fatal(err)
	}
	return bin, pkg
}

func TestCostlensAnalyzerBesideTheLink(t *testing.T) {
	bin, pkg := costlensPackage(t, filepath.Join("terminal", "cli.js"), true)
	costlensUse(t, costlensLookIn(bin), &costlensFake{})
	realPkg, err := filepath.EvalSymlinks(pkg)
	if err != nil {
		t.Fatal(err)
	}
	node, analyzer, ok := costlensAnalyzer(config.Default())
	if want := filepath.Join(realPkg, "analyzer", "cli.js"); !ok || analyzer != want || node != filepath.Join(bin, "node") {
		t.Errorf("costlensAnalyzer = %q %q %v, want %q", node, analyzer, ok, want)
	}

	// Without analyzer/cli.js, with a directory there, or with a link to a file of another name: no analyzer.
	bin, _ = costlensPackage(t, filepath.Join("terminal", "cli.js"), false)
	costlensLookPath = costlensLookIn(bin)
	if _, a, ok := costlensAnalyzer(config.Default()); ok {
		t.Errorf("without analyzer/cli.js: %q", a)
	}
	bin, pkg = costlensPackage(t, filepath.Join("terminal", "cli.js"), false)
	if err = os.MkdirAll(filepath.Join(pkg, "analyzer", "cli.js"), 0o700); err != nil {
		t.Fatal(err)
	}
	costlensLookPath = costlensLookIn(bin)
	if _, a, ok := costlensAnalyzer(config.Default()); ok {
		t.Errorf("with a directory as analyzer/cli.js: %q", a)
	}
	bin, _ = costlensPackage(t, filepath.Join("terminal", "main.js"), true)
	costlensLookPath = costlensLookIn(bin)
	if _, a, ok := costlensAnalyzer(config.Default()); ok {
		t.Errorf("with a link to terminal/main.js: %q", a)
	}
	// No cost-lens on PATH at all.
	_, bin = costlensSetup(t)
	costlensLookPath = costlensLookIn(bin)
	if _, a, ok := costlensAnalyzer(config.Default()); ok {
		t.Errorf("without cost-lens: %q", a)
	}
}

func TestCostlensAnalyzerMissingNode(t *testing.T) {
	cfg, _ := costlensSetup(t)
	costlensUse(t, costlensLookIn(t.TempDir()), &costlensFake{})
	if n, a, ok := costlensAnalyzer(cfg); ok {
		t.Errorf("configured analyzer, no node: %q %q", n, a)
	}
	bin, _ := costlensPackage(t, filepath.Join("terminal", "cli.js"), true)
	if err := os.Remove(filepath.Join(bin, "node")); err != nil {
		t.Fatal(err)
	}
	costlensLookPath = costlensLookIn(bin)
	if n, a, ok := costlensAnalyzer(config.Default()); ok {
		t.Errorf("analyzer beside the link, no node: %q %q", n, a)
	}
}

func TestCostlensGetStartsOneRun(t *testing.T) {
	cfg, bin := costlensSetup(t)
	fake := &costlensFake{release: make(chan struct{}), out: costlensSummary(t, "curia/T-1-fix")}
	costlensUse(t, costlensLookIn(bin), fake)
	clk := &costlensClock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	c := newCostlensCache(cfg, cfg.LogPath, clk.now)
	wt := filepath.Join(t.TempDir(), ".curia-worktrees", "api-server-T-1")

	var wg sync.WaitGroup
	got := make([]bool, 10)
	for i := range got {
		wg.Go(func() { _, got[i] = c.Get(wt, "curia/T-1-fix") })
	}
	wg.Wait()
	if slices.Contains(got, true) {
		t.Errorf("Gets before the run ended = %v, want all false", got)
	}
	costlensWaitCalls(t, fake, 1)
	// One run at a time across the whole cache: another worktree waits for its turn.
	if _, ok := c.Get(filepath.Join(t.TempDir(), "other"), "curia/T-2-other"); ok {
		t.Error("Get of another worktree while a run is in flight = true")
	}
	if n := fake.n(); n != 1 {
		t.Errorf("runs started = %d, want 1", n)
	}
	if call := fake.call(0); !slices.Equal(call, []string{filepath.Join(bin, "node"), cfg.CostLens.Analyzer, wt}) {
		t.Errorf("run = %q", call)
	}

	close(fake.release)
	costlensWait(t, c)
	if usd, ok := c.Get(wt, "curia/T-1-fix"); !ok || !costlensIs503(usd) {
		t.Errorf("Get after the run = %v %v, want 5.03 true", usd, ok)
	}
}

// A run answers one branch of one worktree: another branch is its own run, and an empty worktree or branch asks
// nothing.
func TestCostlensGetPerBranch(t *testing.T) {
	cfg, bin := costlensSetup(t)
	fake := &costlensFake{out: costlensSummary(t, "curia/T-1-fix")}
	costlensUse(t, costlensLookIn(bin), fake)
	clk := &costlensClock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	c := newCostlensCache(cfg, cfg.LogPath, clk.now)
	wt := filepath.Join(t.TempDir(), ".curia-worktrees", "api-server-T-1")
	c.Get(wt, "curia/T-1-fix")
	costlensWait(t, c)
	// A branch the analyzer has no entry for is no answer, and asks again only for itself.
	if _, ok := c.Get(wt, "curia/T-9-absent"); ok {
		t.Error("Get of an absent branch = true")
	}
	costlensWait(t, c)
	if _, ok := c.Get(wt, "curia/T-9-absent"); ok {
		t.Error("absent branch after its run = true")
	}
	if n := fake.n(); n != 2 {
		t.Errorf("runs = %d, want 2 (one per worktree and branch)", n)
	}
	if usd, ok := c.Get(wt, "curia/T-1-fix"); !ok || !costlensIs503(usd) || fake.n() != 2 {
		t.Errorf("kept value = %v %v, runs %d", usd, ok, fake.n())
	}
	// An empty worktree or branch asks nothing.
	for _, q := range [][2]string{{"", "curia/T-1-fix"}, {wt, ""}} {
		if _, ok := c.Get(q[0], q[1]); ok {
			t.Errorf("Get(%q, %q) = true", q[0], q[1])
		}
	}
	costlensWait(t, c)
	if n := fake.n(); n != 2 {
		t.Errorf("runs after empty Gets = %d, want 2", n)
	}
}

// costlensLog is the log file's content.
func costlensLog(t *testing.T, cfg config.Config) string {
	t.Helper()
	b, err := os.ReadFile(cfg.LogPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func TestCostlensRunIsBounded(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// The stub's child holds stdout open: the whole process group is killed, and WaitDelay bounds the wait.
	sleeper := costlensFile(t, filepath.Join(dir, "sleep.js"), "sleep 30 &\necho $! > '"+pidFile+"'\nwait\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	out, err := costlensRealRun(ctx, "/bin/sh", sleeper, dir)
	if took := time.Since(start); err == nil || took > 3*time.Second {
		t.Errorf("sleeping analyzer: %q %v after %v, want an error within 3 s", out, err, took)
	}
	if b, rerr := os.ReadFile(filepath.Clean(pidFile)); rerr == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		for deadline := time.Now().Add(3 * time.Second); pid > 0 && syscall.Kill(pid, 0) == nil; {
			if time.Now().After(deadline) {
				t.Errorf("the analyzer's child %d still runs", pid)
				_ = syscall.Kill(pid, syscall.SIGKILL)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	big := costlensFile(t, filepath.Join(dir, "big.js"), "dd if=/dev/zero bs=1048576 count=5 2>/dev/null\n")
	out, err = costlensRealRun(context.Background(), "/bin/sh", big, dir)
	if err == nil || len(out) > usage.MaxSummary {
		t.Errorf("5 MiB of output: %d bytes, err %v; want at most %d and an error", len(out), err, usage.MaxSummary)
	}
	if _, ok := usage.BranchCost(out, "curia/T-1-fix"); ok {
		t.Error("BranchCost of a cut output = true")
	}
}

func TestCostlensArgvIsDirect(t *testing.T) {
	dir := t.TempDir()
	argvFile, stdinFile, cwdFile := filepath.Join(dir, "argv"), filepath.Join(dir, "stdin"), filepath.Join(dir, "cwd")
	stub := costlensFile(t, filepath.Join(dir, "analyzer.js"),
		"printf '%s\\0' \"$0\" \"$@\" > '"+argvFile+"'\ncat > '"+stdinFile+"'\npwd -P > '"+cwdFile+"'\n"+
			"echo '{\"currentProject\":{\"branches\":[]}}'\necho 'stderr is discarded' >&2\n")
	repo := filepath.Join(dir, "my repo; touch pwned")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := costlensRealRun(context.Background(), "/bin/sh", stub, repo)
	if err != nil || string(out) != "{\"currentProject\":{\"branches\":[]}}\n" {
		t.Fatalf("run = %q %v", out, err)
	}
	wantArgv := func(project string) {
		t.Helper()
		b, rerr := os.ReadFile(filepath.Clean(argvFile))
		if rerr != nil {
			t.Fatal(rerr)
		}
		got := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		if want := []string{stub, "--project", project}; !slices.Equal(got, want) {
			t.Errorf("argv = %q, want %q", got, want)
		}
		if slices.Contains(got, "--live-prices") {
			t.Error("--live-prices passed")
		}
	}
	wantArgv(repo)
	if b, _ := os.ReadFile(filepath.Clean(stdinFile)); len(b) != 0 {
		t.Errorf("stdin = %q, want /dev/null", b)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	if b, _ := os.ReadFile(filepath.Clean(cwdFile)); strings.TrimSpace(string(b)) != realDir {
		t.Errorf("cwd = %q, want the analyzer's folder %q", b, realDir)
	}
	if _, err = os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("the path went through a shell")
	}

	// A removed worktree is still the argument (probe C2), and the run still starts in the analyzer's folder.
	gone := filepath.Join(repo, ".curia-worktrees", "api-server-T-2")
	if _, err = costlensRealRun(context.Background(), "/bin/sh", stub, gone); err != nil {
		t.Fatal(err)
	}
	wantArgv(gone)
	if b, _ := os.ReadFile(filepath.Clean(cwdFile)); strings.TrimSpace(string(b)) != realDir {
		t.Errorf("cwd for a removed worktree = %q, want %q", b, realDir)
	}
}

func TestCostlensFailureIsRemembered(t *testing.T) {
	cfg, bin := costlensSetup(t)
	fake := &costlensFake{err: &exec.ExitError{}}
	costlensUse(t, costlensLookIn(bin), fake)
	clk := &costlensClock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	c := newCostlensCache(cfg, cfg.LogPath, clk.now)
	wt := filepath.Join(t.TempDir(), "wt")
	if _, ok := c.Get(wt, "curia/T-1-fix"); ok {
		t.Fatal("first Get = true")
	}
	costlensWait(t, c)
	clk.advance(costlensKeep - time.Second)
	for range 5 {
		if _, ok := c.Get(wt, "curia/T-1-fix"); ok {
			t.Error("Get after a failed run = true")
		}
	}
	costlensWait(t, c)
	if n := fake.n(); n != 1 {
		t.Errorf("runs within costlensKeep of a failure = %d, want 1", n)
	}
	log := costlensLog(t, cfg)
	if strings.Count(log, "cost-lens") != 1 || !strings.Contains(log, "level=WARN") || !strings.Contains(log, "kind=exit") {
		t.Errorf("log = %q, want one WARN line with kind=exit", log)
	}
	clk.advance(time.Second)
	c.Get(wt, "curia/T-1-fix")
	costlensWait(t, c)
	if n := fake.n(); n != 2 {
		t.Errorf("runs after costlensKeep = %d, want 2", n)
	}

	// Output that is not JSON is a failure too, logged once; a summary without the branch is no answer, not logged.
	fake.mu.Lock()
	fake.err, fake.out = nil, []byte("Error: cannot find module\n")
	fake.mu.Unlock()
	clk.advance(costlensKeep)
	c.Get(wt, "curia/T-1-fix")
	costlensWait(t, c)
	if log := costlensLog(t, cfg); !strings.Contains(log, "kind=unreadable") || strings.Contains(log, "cannot find module") {
		t.Errorf("log = %q, want kind=unreadable and none of the output", log)
	}
}

func TestCostlensMissingAnalyzerLoggedOnce(t *testing.T) {
	cfg, _ := costlensSetup(t)
	fake := &costlensFake{}
	costlensUse(t, costlensLookIn(t.TempDir()), fake)
	clk := &costlensClock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	c := newCostlensCache(cfg, cfg.LogPath, clk.now)
	for i := range 3 {
		if _, ok := c.Get(filepath.Join(t.TempDir(), strconv.Itoa(i)), "curia/T-1-fix"); ok {
			t.Error("Get without node = true")
		}
		clk.advance(costlensKeep)
	}
	costlensWait(t, c)
	if n := fake.n(); n != 0 {
		t.Errorf("runs without node = %d", n)
	}
	if log := costlensLog(t, cfg); strings.Count(log, "\n") != 1 || !strings.Contains(log, "level=INFO") {
		t.Errorf("log = %q, want one INFO line", log)
	}
}

func TestCostlensKeepExpires(t *testing.T) {
	cfg, bin := costlensSetup(t)
	fake := &costlensFake{out: costlensSummary(t, "curia/T-1-fix")}
	costlensUse(t, costlensLookIn(bin), fake)
	clk := &costlensClock{t: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)}
	c := newCostlensCache(cfg, cfg.LogPath, clk.now)
	wt := filepath.Join(t.TempDir(), "wt")
	c.Get(wt, "curia/T-1-fix")
	costlensWait(t, c)
	clk.advance(costlensKeep - time.Second)
	if usd, ok := c.Get(wt, "curia/T-1-fix"); !ok || !costlensIs503(usd) || fake.n() != 1 {
		t.Errorf("Get before costlensKeep = %v %v, runs %d", usd, ok, fake.n())
	}
	clk.advance(time.Second)
	if _, ok := c.Get(wt, "curia/T-1-fix"); ok {
		t.Error("Get at costlensKeep = true, want a new run first")
	}
	costlensWait(t, c)
	if n := fake.n(); n != 2 {
		t.Errorf("runs = %d, want 2", n)
	}
	if usd, ok := c.Get(wt, "curia/T-1-fix"); !ok || !costlensIs503(usd) {
		t.Errorf("Get after the new run = %v %v", usd, ok)
	}
}

// The analyzer is started without Curia's keys and tokens, and with the rest
// of the environment (a node program needs PATH and HOME).
func TestCostlensEnvDropsSecrets(t *testing.T) {
	t.Setenv("CURIA_LINEAR_API_KEY", "lin_secret")
	t.Setenv("CURIA_NTFY_TOKEN", "tk_secret")
	t.Setenv("CURIA_OTHER_SECRET", "x")
	t.Setenv("CURIA_KEEP_ME", "kept")
	dir := t.TempDir()
	stub := costlensFile(t, filepath.Join(dir, "analyzer.js"), "env\necho '{\"currentProject\":{\"branches\":[]}}'\n")
	out, err := costlensRealRun(context.Background(), "/bin/sh", stub, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"CURIA_LINEAR_API_KEY", "CURIA_NTFY_TOKEN", "CURIA_OTHER_SECRET", "lin_secret"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("analyzer environment holds %s", bad)
		}
	}
	if !strings.Contains(string(out), "CURIA_KEEP_ME=kept") || !strings.Contains(string(out), "PATH=") {
		t.Errorf("analyzer environment lost the rest:\n%s", out)
	}
}

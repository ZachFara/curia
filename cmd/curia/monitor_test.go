package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/usage"
)

var monitorNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// monitorFixture is a monitorEnv over a fresh store with live sessions s1
// (pane %1, pid 100) and s2 (pane %2, pid 200) on server /sock/a, and a
// process table where 100 has two descendants.
func monitorFixture(t *testing.T) (*monitorEnv, *store.Store) {
	t.Helper()
	db := hookHome(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, s := range []model.Session{
		{ID: "s1", Pane: "%1", Socket: "/sock/a", State: model.StateWorking, StartedAt: monitorNow, UpdatedAt: monitorNow},
		{ID: "s2", Pane: "%2", Socket: "/sock/a", State: model.StateIdle, StartedAt: monitorNow, UpdatedAt: monitorNow},
		{ID: "gone", Pane: "%3", Socket: "/sock/a", State: model.StateEnded, StartedAt: monitorNow, UpdatedAt: monitorNow},
	} {
		if err = st.UpsertSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Unix(1700000000, 0)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	env := &monitorEnv{
		st: st, cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return monitorNow },
		panes: func(_ context.Context, socket string) ([]tmux.Pane, error) {
			if socket != "/sock/a" {
				return nil, errors.New("wrong server asked: " + socket)
			}
			return []tmux.Pane{
				{ID: "%1", PID: 100, Socket: "/sock/a", ServerStart: start},
				{ID: "%2", PID: 200, Socket: "/sock/a", ServerStart: start},
				{ID: "%3", PID: 300, Socket: "/sock/a", ServerStart: start},
			}, nil
		},
		ps: func(context.Context) ([]usage.ProcRow, error) {
			return []usage.ProcRow{
				{PID: 100, PPID: 1, CPU: 1, RSSKB: 10 * 1024},
				{PID: 101, PPID: 100, CPU: 4, RSSKB: 20 * 1024},
				{PID: 102, PPID: 101, CPU: 5, RSSKB: 30 * 1024},
				{PID: 300, PPID: 1, CPU: 99, RSSKB: 1 << 20},
			}, nil
		},
	}
	return env, st
}

// A session's samples are its pane process and every descendant, from one
// snapshot; a pane whose process is missing from the snapshot, and an ended
// session, get none; each session is looked up on its own server.
func TestSampleResourcesSumsPaneTreePerSession(t *testing.T) {
	env, st := monitorFixture(t)
	env.tick(context.Background())
	got, err := st.LatestResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["s1"].CPU != 10 || got["s1"].RSSMB != 60 || !got["s1"].At.Equal(monitorNow) {
		t.Errorf("samples = %+v, want only s1: cpu 10, 60 MiB (s2's pane process exited; the ended session is skipped)", got)
	}
}

func TestSampleResourcesCallsPSOnceAndOnlyWithLiveSessions(t *testing.T) {
	env, _ := monitorFixture(t)
	calls := 0
	inner := env.ps
	env.ps = func(ctx context.Context) ([]usage.ProcRow, error) { calls++; return inner(ctx) }
	env.tick(context.Background())
	if calls != 1 {
		t.Errorf("ps ran %d times for two sessions, want one snapshot", calls)
	}
	env.cfg.Resources.Enabled = false
	env.tick(context.Background())
	if calls != 1 {
		t.Errorf("ps ran with resources.enabled = false")
	}
}

func TestSampleResourcesToleratesFailures(t *testing.T) {
	env, st := monitorFixture(t)
	env.ps = func(context.Context) ([]usage.ProcRow, error) { return nil, errors.New("ps: boom") }
	env.tick(context.Background()) // logged, not fatal
	env.ps = func(context.Context) ([]usage.ProcRow, error) { return nil, nil }
	env.panes = func(context.Context, string) ([]tmux.Pane, error) { return nil, errors.New("no server") }
	env.tick(context.Background())
	if got, _ := st.LatestResources(context.Background()); len(got) != 0 {
		t.Errorf("samples = %+v, want none when ps or tmux fail", got)
	}
}

func TestPruneResourcesRunsOncePerMinute(t *testing.T) {
	env, st := monitorFixture(t)
	old := monitorNow.Add(-store.ResourceKeep - time.Hour)
	if err := st.AddResourceSamples(context.Background(), []store.ResourceSample{{SessionID: "x", At: old}}); err != nil {
		t.Fatal(err)
	}
	if err := env.pruneResources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LatestResources(context.Background()); len(got) != 0 {
		t.Errorf("old sample survived the prune: %+v", got)
	}
	if err := st.AddResourceSamples(context.Background(), []store.ResourceSample{{SessionID: "x", At: old}}); err != nil {
		t.Fatal(err)
	}
	if err := env.pruneResources(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.LatestResources(context.Background()); len(got) != 1 {
		t.Errorf("prune ran twice inside a minute: %+v", got)
	}
}

func TestPSResourcesReadsTheRealProcessTable(t *testing.T) {
	rows, err := psResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cpu, kb, ok := usage.SumTree(rows, os.Getpid())
	if !ok || kb <= 0 || cpu < 0 {
		t.Errorf("this test process: cpu %v rss %v KiB found %v, want a resident process in the table", cpu, kb, ok)
	}
}

func TestMonitorLockAllowsOneHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.lock")
	if running, _ := monitorRunning(path); running {
		t.Fatal("running with no lock file")
	}
	l, held, err := takeMonitorLock(path)
	if err != nil || held {
		t.Fatalf("first take = %v, %v", held, err)
	}
	if _, held, _ = takeMonitorLock(path); !held {
		t.Error("a second take did not see the lock held")
	}
	if running, pid := monitorRunning(path); !running || pid != os.Getpid() {
		t.Errorf("monitorRunning = %v %d, want true and this pid", running, pid)
	}
	l.release()
	if running, _ := monitorRunning(path); running {
		t.Error("still running after the lock was released")
	}
	// A lock file with a pid that is not an integer above 1 names no process.
	if err = os.WriteFile(path, []byte("-9 junk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l2, _, _ := takeMonitorLock(path)
	defer l2.release()
}

// The in-process loop: it samples on start, a second start exits 0 saying
// one is running, and canceling the context ends it and frees the lock.
func TestMonitorLoopSamplesAndSecondStartSaysRunning(t *testing.T) {
	env, st := monitorFixture(t)
	cfg := env.cfg
	cfg.Resources.Interval = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	prev := monitorLoopEnv
	monitorLoopEnv = func(e *monitorEnv) { e.panes, e.ps, e.now = env.panes, env.ps, env.now }
	t.Cleanup(func() { monitorLoopEnv = prev })
	go func() { done <- runMonitorLoop(ctx, cfg, func(string, ...any) {}) }()
	waitFor(t, "a sample", func() bool { got, _ := st.LatestResources(context.Background()); return len(got) == 1 })

	var said string
	if err := runMonitorLoop(context.Background(), cfg, func(f string, _ ...any) { said = f }); err != nil || !strings.Contains(said, "already running") {
		t.Errorf("second start = %v, said %q, want exit 0 saying one is running", err, said)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if running, _ := monitorRunning(monitorLockPath(cfg)); running {
		t.Error("the lock is still held after the loop ended")
	}
	log, _ := os.ReadFile(cfg.LogPath)
	if !strings.Contains(string(log), "monitor: started") || !strings.Contains(string(log), "monitor: stopping") {
		t.Errorf("log lacks the start and stop lines:\n%s", log)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// monitorBinary builds this command once for the tests that run it as a
// real process.
func monitorBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "curia")
	// The guard moved HOME; the build keeps the developer's caches.
	goenv := exec.Command("go", "env", "GOCACHE", "GOMODCACHE")
	goenv.Env = append(os.Environ(), "HOME="+guardOuterHome)
	caches, err := goenv.Output()
	if err != nil {
		t.Fatal(err)
	}
	c := strings.Fields(string(caches))
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOCACHE="+c[0], "GOMODCACHE="+c[1])
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// TestMonitorRealProcess: start launches a detached monitor, a second start
// and the quiet start exit 0 and start no second one, status says running,
// SIGTERM ends it cleanly with the lock freed, and stop (through the user
// guard) does the same. monitor.autostart = false makes the quiet start do
// nothing.
func TestMonitorRealProcess(t *testing.T) {
	bin := monitorBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "CURIA_DB="+filepath.Join(home, "curia.db"), "TMUX=", "TMUX_PANE=")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("curia %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	lock := filepath.Join(home, "monitor.lock")
	t.Cleanup(func() {
		if _, pid := monitorRunning(lock); pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if out := run("monitor", "status"); !strings.Contains(out, "not running") {
		t.Errorf("status before start = %q", out)
	}
	if out := run("monitor", "start"); !strings.Contains(out, "started") {
		t.Errorf("start = %q", out)
	}
	waitFor(t, "the monitor to hold the lock", func() bool { r, _ := monitorRunning(lock); return r })
	_, pid := monitorRunning(lock)
	if out := run("monitor", "start"); !strings.Contains(out, "already running") {
		t.Errorf("second start = %q, want it to say one is running", out)
	}
	if out := run("monitor", "start", "--quiet"); out != "" {
		t.Errorf("quiet start with one running printed %q", out)
	}
	if out := run("monitor", "status"); !strings.Contains(out, "running (pid ") {
		t.Errorf("status = %q", out)
	}
	if _, pid2 := monitorRunning(lock); pid2 != pid {
		t.Errorf("pid changed from %d to %d: a second monitor took over", pid, pid2)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the monitor to stop on SIGTERM", func() bool { r, _ := monitorRunning(lock); return !r })
	logb, _ := os.ReadFile(filepath.Clean(filepath.Join(home, ".local/state/curia/curia.log")))
	if !strings.Contains(string(logb), "monitor: stopping") {
		t.Errorf("no clean-shutdown log line:\n%s", logb)
	}
}

// monitor.autostart = false makes the quiet start (the tmux line) do nothing.
func TestMonitorQuietStartRespectsAutostart(t *testing.T) {
	bin := monitorBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "CURIA_DB="+filepath.Join(home, "curia.db"), "TMUX=", "TMUX_PANE=")
	lock := filepath.Join(home, "monitor.lock")
	t.Cleanup(func() {
		if _, pid := monitorRunning(lock); pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if err := os.MkdirAll(filepath.Join(home, ".config", "curia"), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(home, ".config", "curia", "config.toml")
	if err := os.WriteFile(conf, []byte("[monitor]\nautostart = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "monitor", "start", "--quiet")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("quiet start = %v %q, want exit 0 and no output", err, out)
	}
	if running, _ := monitorRunning(lock); running {
		t.Fatal("quiet start ran the monitor with monitor.autostart = false")
	}
}

func TestMonitorStopThroughTheGuard(t *testing.T) {
	bin := monitorBinary(t)
	home := hookHome(t)
	cmd := exec.Command(bin, "monitor", "start")
	cmd.Env = append(os.Environ(), "HOME="+os.Getenv("HOME"), "CURIA_DB="+home, "TMUX=", "TMUX_PANE=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	lock := filepath.Join(filepath.Dir(home), "monitor.lock")
	waitFor(t, "the monitor", func() bool { r, _ := monitorRunning(lock); return r })
	_, pid := monitorRunning(lock)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	userTerminalAs(t, false)
	if out, err := runRoot(t, "monitor", "stop"); err == nil || !strings.Contains(err.Error(), "only the user may stop the monitor") {
		t.Errorf("stop with no terminal = %v %q, want the guard's refusal", err, out)
	}
	if r, _ := monitorRunning(lock); !r {
		t.Fatal("a refused stop stopped the monitor")
	}
	userTerminalAs(t, true)
	out, err := runRoot(t, "monitor", "stop")
	if err != nil || !strings.Contains(out, "stopped") {
		t.Errorf("stop = %v %q", err, out)
	}
	if r, _ := monitorRunning(lock); r {
		t.Error("monitor still running after stop")
	}
	if out, err = runRoot(t, "monitor", "stop"); err != nil || !strings.Contains(out, "not running") {
		t.Errorf("second stop = %v %q, want not running", err, out)
	}
}

// With no monitor, other commands behave as they do today: they neither
// start it, wait for it nor create its lock.
func TestOtherCommandsNeverTouchTheMonitor(t *testing.T) {
	db, _ := fleetHome(t, nil)
	for _, args := range [][]string{{"ls"}, {"ls", "--json"}, {"status"}} {
		if _, err := runRoot(t, args...); err != nil {
			t.Errorf("curia %v with no monitor: %v", args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(db), "monitor.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a command other than monitor made the lock file: %v", err)
	}
}

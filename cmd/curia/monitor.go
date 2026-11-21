package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/usage"
)

// `curia monitor` (spec section 4, Ruling 145): one long-running process,
// held by a lock file, that wakes every resources.interval and runs its
// jobs. Nothing else in Curia calls it or waits for it: with no monitor
// running every other command behaves as before, and what depends on it
// (the CPU and memory columns) shows "?" instead of a stale number. A later
// feature (Docker, schedules, auto-spawn) adds one entry to monitorJobs.

// monitorJob is one piece of work of a tick. A job that fails is logged and
// the loop goes on; each runs under its own deadline.
type monitorJob struct {
	name string
	run  func(e *monitorEnv, ctx context.Context) error
}

// monitorJobs are run in order on every tick.
var monitorJobs = []monitorJob{
	// The state changers come first so a slow sampler (a hung docker) delays none of them; end away
	// is first and fire schedules stays before auto-spawn, or a session spawned in a pass would get that pass's scheduled message.
	{"end away", (*monitorEnv).expireAway},
	{"fire schedules", (*monitorEnv).fireSchedules},
	{"auto-spawn", (*monitorEnv).autoSpawn},
	{"sample resources", (*monitorEnv).sampleResources},
	{"prune resources", (*monitorEnv).pruneResources},
	{"sample containers", (*monitorEnv).sampleContainers},
}

// monitorEnv is what a job may use.
type monitorEnv struct {
	st  *store.Store
	cfg config.Config
	log *slog.Logger
	now func() time.Time
	// panes lists the panes of the tmux server at socket ("" is the default
	// server); ps is one snapshot of the process table. Tests replace them.
	panes func(ctx context.Context, socket string) ([]tmux.Pane, error)
	ps    func(ctx context.Context) ([]usage.ProcRow, error)
	// tmuxFor is the tmux client of the server at socket (schedules type through it).
	tmuxFor func(socket string) tmux.Client

	lastPrune time.Time
	lastAuto  time.Time
}

const (
	// monitorJobTimeout bounds one job of one tick.
	monitorJobTimeout = 20 * time.Second
	// monitorPruneEvery is how often the prune job does anything.
	monitorPruneEvery = time.Minute
	// maxPSOutput bounds what is read from ps.
	maxPSOutput = 32 << 20
	// monitorStopWait is how long `monitor stop` waits for the lock to be released.
	monitorStopWait = 5 * time.Second
)

// sampleResources writes one sample per live tracked session: the CPU and
// resident memory summed over its pane's first process and every descendant,
// from ONE process-table snapshot taken after the panes are listed. Each
// session's pane is looked up on that session's own tmux server (its
// recorded socket; panes are matched only together with their server, as
// reconcile does). A session whose pane is gone, or whose pane process is
// not in the snapshot (it exited between the two reads), gets no sample.
//
// shortcut: pid reuse is not detected. The user guard's reader checks no
// start time or command either; the pid comes from tmux a moment before the
// snapshot, so a wrong match needs the pane's process to exit and the pid to
// be reissued inside that gap. Upgrade by adding lstart to the ps columns
// and the server pid (#{pid}) to the pane format and checking the parent.
func (e *monitorEnv) sampleResources(ctx context.Context) error {
	if !e.cfg.Resources.Enabled {
		return nil
	}
	sessions, err := e.st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return err
	}
	sessions = slices.DeleteFunc(sessions, func(s model.Session) bool {
		return s.Pane == "" || s.State == model.StateEnded || s.State == model.StateStale
	})
	if len(sessions) == 0 {
		return nil
	}
	targets := e.paneTargets(ctx, sessions)
	if len(targets) == 0 {
		return nil
	}
	rows, err := e.ps(ctx)
	if err != nil {
		return err
	}
	at := e.now()
	var out []store.ResourceSample
	for _, t := range targets {
		if cpu, kb, ok := usage.SumTree(rows, t.pid); ok {
			out = append(out, store.ResourceSample{SessionID: t.id, At: at, CPU: cpu, RSSMB: kb / 1024})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return e.st.AddResourceSamples(ctx, out)
}

// paneTarget is a live session and the pid of its pane's first process.
type paneTarget struct {
	id  string
	pid int
}

// paneTargets finds each session's pane process on the session's own tmux
// server, listing each server once; a server that cannot be listed, or a
// session not consistent with it (decisionsOnServer), is skipped.
func (e *monitorEnv) paneTargets(ctx context.Context, sessions []model.Session) []paneTarget {
	var targets []paneTarget
	live := map[string][]tmux.Pane{}
	listed := map[string]bool{}
	for _, s := range sessions {
		if !listed[s.Socket] {
			listed[s.Socket] = true
			p, err := e.panes(ctx, s.Socket)
			if err != nil {
				e.log.Info("monitor: panes not listed", "socket", sanitize.Line(s.Socket, 120), "err", sanitize.Line(err.Error(), 200))
				continue
			}
			live[s.Socket] = p
		}
		panes := live[s.Socket]
		if !decisionsOnServer(panes, s) {
			continue
		}
		for _, p := range panes {
			if p.ID == s.Pane && p.PID > 1 {
				targets = append(targets, paneTarget{s.ID, p.PID})
			}
		}
	}
	return targets
}

// pruneResources runs the age and cap prune (and the history tables' prune) at most once a minute, apart
// from the insert.
func (e *monitorEnv) pruneResources(ctx context.Context) error {
	if now := e.now(); now.Sub(e.lastPrune) >= monitorPruneEvery {
		e.lastPrune = now
		if _, err := e.st.PruneResources(ctx, now); err != nil {
			return err
		}
		return e.st.PruneHistory(ctx, now)
	}
	return nil
}

// tick runs every job once.
func (e *monitorEnv) tick(ctx context.Context) {
	for _, j := range monitorJobs {
		jctx, cancel := context.WithTimeout(ctx, monitorJobTimeout)
		if err := j.run(e, jctx); err != nil && ctx.Err() == nil {
			e.log.Warn("monitor: job failed", "job", j.name, "err", sanitize.Line(err.Error(), 300))
		}
		cancel()
	}
}

// boundedBuffer is a Writer that fails once more than max bytes are written.
type boundedBuffer struct {
	bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, errors.New("output too long")
	}
	return b.Buffer.Write(p)
}

// psResources is one snapshot of every process: `ps -A -o
// pid=,ppid=,%cpu=,rss=` by its fixed absolute path (psPath), no shell, its
// output bounded and under the caller's deadline.
func psResources(ctx context.Context) ([]usage.ProcRow, error) {
	ps, err := psPath()
	if err != nil {
		return nil, err
	}
	var out boundedBuffer
	out.max = maxPSOutput
	cmd := exec.CommandContext(ctx, ps, "-A", "-o", "pid=,ppid=,%cpu=,rss=")
	cmd.Stdout = &out
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("ps -A: %w", err)
	}
	return usage.ParsePS(out.String()), nil
}

// ---- the lock ----

// monitorLockPath is the lock file, in the directory of the database (the
// data directory; the spec's state directory is the log's, and the lock
// follows the store so a test's temporary store has its own).
func monitorLockPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.DBPath), "monitor.lock")
}

// monitorLock is a held flock on the lock file; closing it releases it.
type monitorLock struct{ f *os.File }

func (l monitorLock) release() { _ = l.f.Close() }

// takeMonitorLock takes the exclusive lock and writes this process's id into
// the file; held is true when another process has it.
func takeMonitorLock(path string) (lock monitorLock, held bool, err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return lock, false, err
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return lock, false, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return lock, errors.Is(err, syscall.EWOULDBLOCK), err
	}
	if err = f.Truncate(0); err == nil {
		exe, _ := os.Executable()
		_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"+exe+"\n"), 0)
	}
	if err != nil {
		_ = f.Close()
		return lock, false, err
	}
	return monitorLock{f}, false, nil
}

// monitorRunning reports whether a monitor holds the lock, and its pid.
func monitorRunning(path string) (running bool, pid int) {
	running, pid, _ = monitorHolder(path)
	return running, pid
}

// monitorHolder reports whether a monitor holds the lock, the pid the file
// names (an integer above 1; anything else is 0) and the binary path it
// recorded. It only reads: a shared, non-blocking lock attempt that is
// released at once.
func monitorHolder(path string) (running bool, pid int, exe string) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return false, 0, ""
	}
	defer func() { _ = f.Close() }()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err == nil || !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, 0, ""
	}
	b := make([]byte, 4096)
	n, _ := f.Read(b)
	first, rest, _ := strings.Cut(string(b[:n]), "\n")
	if p, perr := strconv.Atoi(strings.TrimSpace(first)); perr == nil && p > 1 {
		pid = p
	}
	exe, _, _ = strings.Cut(rest, "\n")
	return true, pid, exe
}

// monitorProcArgs is the full command line of process pid (`ps -p pid -o
// args=`); tests replace it.
var monitorProcArgs = func(pid int) (string, error) {
	ps, err := psPath()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out boundedBuffer
	out.max = 64 << 10
	cmd := exec.CommandContext(ctx, ps, "-p", strconv.Itoa(pid), "-o", "args=")
	cmd.Stdout = &out
	if err = cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// isMonitorProcess reports whether process pid is the monitor that recorded
// exe: its command line is exactly `<exe> monitor start --foreground`. A pid
// the lock file names may have been reused by an unrelated program, which
// must never be signaled.
func isMonitorProcess(pid int, exe string) bool {
	if exe == "" {
		return false
	}
	args, err := monitorProcArgs(pid)
	return err == nil && args == exe+" monitor start --foreground"
}

// stopMonitor stops the running monitor through the lock's pid, after
// checking that the pid is the monitor. It returns one line to say what
// happened and an error when a monitor runs and was not stopped.
func stopMonitor(cfg config.Config) (string, error) {
	path := monitorLockPath(cfg)
	running, pid, exe := monitorHolder(path)
	if !running {
		return "monitor: not running", nil
	}
	if pid == 0 || pid == os.Getpid() || !isMonitorProcess(pid, exe) {
		return "", fmt.Errorf("monitor: the lock is held by process %d, which is not the Curia monitor (or its command cannot be read); not signaled. "+
			"Look at it with: ps -p %d -o args=", pid, pid)
	}
	if err := monitorSignal(pid); err != nil {
		return "", fmt.Errorf("monitor: could not stop process %d: %w; stop it by hand: kill %d", pid, err, pid)
	}
	for deadline := time.Now().Add(monitorStopWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if still, _ := monitorRunning(path); !still {
			return "monitor: stopped", nil
		}
	}
	return "", fmt.Errorf("monitor: process %d was asked to stop and still holds the lock; stop it by hand: kill %d", pid, pid)
}

// ---- the commands ----

var monitorQuiet, monitorForeground bool

// monitorSpawn starts the detached monitor process; tests replace it.
var monitorSpawn = func() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "monitor", "start", "--foreground")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// monitorLoopEnv lets tests replace the loop's tmux and ps readers.
var monitorLoopEnv = func(*monitorEnv) {}

// monitorSignal asks the monitor process to stop; tests replace it.
var monitorSignal = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func init() {
	parent := &cobra.Command{
		Use:   "monitor",
		Short: "The background sampler: start, stop or ask about it",
		Long: "curia monitor is one long-running process (a lock file keeps it to one) that\n" +
			"every resources.interval samples each live agent's CPU and memory. Nothing\n" +
			"else depends on it: with it stopped, the dashboard shows ? instead of numbers.",
	}
	start := &cobra.Command{
		Use:   "start",
		Short: "Start the monitor in the background; one already running is left alone",
		Long: "Starts the monitor and returns. When one is already running it says so and exits 0.\n" +
			"--quiet (what the tmux snippet runs when a client attaches) prints nothing, and\n" +
			"does nothing when monitor.autostart is false. --foreground runs the loop here.",
		Args: cobra.NoArgs,
		RunE: runMonitorStart,
	}
	start.Flags().BoolVar(&monitorQuiet, "quiet", false, "print nothing; do nothing when monitor.autostart is false")
	start.Flags().BoolVar(&monitorForeground, "foreground", false, "run the loop in this process (what start launches)")
	stop := &cobra.Command{
		Use:         "stop",
		Short:       "Stop the monitor (the user only)",
		Args:        cobra.NoArgs,
		Annotations: userOnlyMark(),
		RunE:        runMonitorStop,
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Say whether the monitor is running",
		Args:  cobra.NoArgs,
		RunE:  runMonitorStatus,
	}
	parent.AddCommand(start, stop, status)
	rootCmd.AddCommand(parent)
}

func runMonitorStart(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if monitorQuiet && !cfg.Monitor.Autostart {
		return nil
	}
	say := func(format string, args ...any) {
		if !monitorQuiet {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
		}
	}
	if monitorForeground {
		return runMonitorLoop(cmd.Context(), cfg, say)
	}
	if running, pid := monitorRunning(monitorLockPath(cfg)); running {
		say("monitor: already running%s", pidNote(pid))
		return nil
	}
	if err = monitorSpawn(); err != nil {
		return fmt.Errorf("monitor: could not start: %w", err)
	}
	say("monitor: started")
	return nil
}

func pidNote(pid int) string {
	if pid == 0 {
		return ""
	}
	return " (pid " + strconv.Itoa(pid) + ")"
}

// runMonitorLoop holds the lock and runs the tick loop until ctx is done or
// SIGTERM or SIGINT arrives. A second instance finds the lock held, says so
// and returns nil.
// shortcut: cfg is read once here and passed to every job except auto-spawn (which rereads it), so a changed resources.*, schedule.* or docker limit needs a monitor restart; reload per tick if that bites.
func runMonitorLoop(ctx context.Context, cfg config.Config, say func(string, ...any)) error {
	lock, held, err := takeMonitorLock(monitorLockPath(cfg))
	if held {
		say("monitor: already running")
		return nil
	}
	if err != nil {
		return fmt.Errorf("monitor lock: %w", err)
	}
	defer lock.release()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	env := &monitorEnv{
		st: st, cfg: cfg, log: logger, now: clock,
		panes: func(ctx context.Context, socket string) ([]tmux.Pane, error) {
			return tmux.NewExec(socket).ListPanes(ctx)
		},
		ps:      psResources,
		tmuxFor: tmux.NewExec,
	}
	monitorLoopEnv(env)
	logger.Info("monitor: started", "pid", os.Getpid(), "interval", cfg.Resources.Interval.String())
	tick := time.NewTicker(cfg.Resources.Interval)
	defer tick.Stop()
	for {
		env.tick(ctx)
		select {
		case <-ctx.Done():
			logger.Info("monitor: stopping")
			return nil
		case <-tick.C:
		}
	}
}

func runMonitorStatus(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if running, pid := monitorRunning(monitorLockPath(cfg)); running {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "monitor: running%s, sampling every %v\n", pidNote(pid), cfg.Resources.Interval)
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "monitor: not running (curia monitor start)")
	return err
}

func runMonitorStop(cmd *cobra.Command, _ []string) error {
	if err := userOnly("monitor stop", "stop the monitor"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	line, err := stopMonitor(cfg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
	return err
}

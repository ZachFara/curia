package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/usage"
)

// The cost-lens analyzer (section 4.6) answers a review's branch cost. One
// run parses every Claude Code transcript and takes 8 to 15 seconds, so it
// runs in the background, one at a time per process, and its answer is kept.
// Its output also describes the developer's other projects and sessions:
// only the branch's sum (usage.BranchCost reads currentProject alone) is
// kept, and nothing it printed is logged or shown. Probe C2 (Ruling P5-R4):
// a worktree's sessions are counted only under the worktree's path, also
// once the worktree is removed, so the project asked about is always the
// task's worktree, never its repository.

// costlensLookPath finds node and cost-lens on PATH. Tests replace it.
var costlensLookPath = exec.LookPath

// costlensRun runs the analyzer and returns its stdout, cut at 4 MiB. Tests replace it.
var costlensRun = costlensExec

const (
	// costlensLimit bounds one run.
	costlensLimit = 90 * time.Second
	// costlensKeep is how long an answer, or a failure, is kept.
	costlensKeep = 5 * time.Minute
	// costlensWaitDelay bounds the wait for the output pipe once the run is killed.
	costlensWaitDelay = 2 * time.Second
)

// errCostlensTooLong is a run whose output was over usage.MaxSummary.
var errCostlensTooLong = errors.New("cost-lens output over 4 MiB")

// costlensAnalyzer finds node and the analyzer script (section 4.6); ok is false when either is missing.
func costlensAnalyzer(cfg config.Config) (node, analyzer string, ok bool) {
	node, err := costlensLookPath("node")
	if err != nil {
		return "", "", false
	}
	analyzer = cfg.CostLens.Analyzer
	if analyzer == "" {
		if analyzer, ok = costlensBesideLink(); !ok {
			return "", "", false
		}
	}
	if !costlensRegular(analyzer) {
		return "", "", false
	}
	return node, analyzer, true
}

// costlensBesideLink is the analyzer installed with the cost-lens command:
// cost-lens on PATH resolves to <package>/terminal/cli.js, and the analyzer
// is <package>/analyzer/cli.js.
func costlensBesideLink() (string, bool) {
	link, err := costlensLookPath("cost-lens")
	if err != nil {
		return "", false
	}
	target, err := filepath.EvalSymlinks(link)
	if err != nil || !strings.HasSuffix(filepath.ToSlash(target), "/terminal/cli.js") {
		return "", false
	}
	return filepath.Join(filepath.Dir(filepath.Dir(target)), "analyzer", "cli.js"), true
}

// costlensRegular reports whether path is a regular file (after links).
func costlensRegular(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// costlensExec is the real costlensRun: node <analyzer> --project <project>,
// no shell, stdin /dev/null, stderr discarded, the environment as it is. It
// runs in the analyzer's own folder, which is there (costlensAnalyzer checked
// the file), whereas the project may be a removed worktree (probe C2); and
// so no file of the repository's can steer it. It runs in its own process
// group, killed when ctx ends; stdout over usage.MaxSummary is cut there and
// is an error.
func costlensExec(ctx context.Context, node, analyzer, project string) ([]byte, error) {
	out := &costlensCapped{max: usage.MaxSummary}
	cmd := exec.CommandContext(ctx, node, analyzer, "--project", project)
	cmd.Dir, cmd.Stdout = filepath.Dir(analyzer), out // Stdin and Stderr nil: the null device
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = costlensWaitDelay
	err := cmd.Run()
	switch {
	case out.over:
		return out.buf, errCostlensTooLong
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, err
	}
	return out.buf, nil
}

// costlensCapped keeps at most max bytes; a write past them fails, which
// closes the pipe, so a run printing without end stops.
type costlensCapped struct {
	buf  []byte
	max  int
	over bool
}

func (w *costlensCapped) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); len(p) > room {
		w.buf = append(w.buf, p[:max(room, 0)]...)
		w.over = true
		return max(room, 0), errCostlensTooLong
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// costlensKey is what one run answers: a branch, in the project (worktree) it ran in.
type costlensKey struct{ project, branch string }

// costlensAnswer is a run's answer; ok is false for no answer (a failure, or
// a branch the analyzer has no entry for).
type costlensAnswer struct {
	at  time.Time
	usd float64
	ok  bool
}

// costlensCache answers branch-cost questions without ever blocking: the first Get for a repository starts one run.
type costlensCache struct {
	cfg     config.Config
	logPath string
	now     func() time.Time

	mu         sync.Mutex
	kept       map[costlensKey]costlensAnswer
	running    bool
	idle       chan struct{} // closed when the run in flight ends
	missLogged bool          // the missing analyzer has been logged
}

func newCostlensCache(cfg config.Config, logPath string, now func() time.Time) *costlensCache {
	return &costlensCache{cfg: cfg, logPath: logPath, now: now, kept: map[costlensKey]costlensAnswer{}}
}

// Get returns the branch's cost when a summary for repo younger than costlensKeep is held; otherwise it starts a run (unless one is running) and returns false.
//
// repo is the task's worktree (Ruling P5-R4). With no node or analyzer, no
// run starts: that is logged once and kept as no answer, like a failure.
func (c *costlensCache) Get(repo, branch string) (usd float64, ok bool) {
	if repo == "" || branch == "" {
		return 0, false
	}
	key := costlensKey{project: repo, branch: branch}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if a, held := c.kept[key]; held {
		if now.Sub(a.at) < costlensKeep {
			return a.usd, a.ok
		}
		delete(c.kept, key)
	}
	if c.running {
		return 0, false
	}
	node, analyzer, found := costlensAnalyzer(c.cfg)
	if !found {
		if !c.missLogged {
			c.missLogged = true
			logInfo(c.logPath, "cost-lens analyzer or node not found: reviews show no branch cost")
		}
		c.kept[key] = costlensAnswer{at: now}
		return 0, false
	}
	c.running, c.idle = true, make(chan struct{})
	go c.run(key, node, analyzer)
	return 0, false
}

// run asks the analyzer about key, on a context of its own bounded by
// costlensLimit, and keeps the answer.
func (c *costlensCache) run(key costlensKey, node, analyzer string) {
	ctx, cancel := context.WithTimeout(context.Background(), costlensLimit)
	defer cancel()
	a := costlensAnswer{}
	out, err := costlensRun(ctx, node, analyzer, key.project)
	switch {
	case err != nil:
		logWarn(c.logPath, "cost-lens analyzer run failed", "kind", costlensErrKind(err))
	default:
		if a.usd, a.ok = usage.BranchCost(out, key.branch); !a.ok && !json.Valid(out) {
			logWarn(c.logPath, "cost-lens analyzer run failed", "kind", "unreadable")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a.at = c.now()
	c.kept[key] = a
	c.running = false
	close(c.idle)
}

// costlensErrKind names the kind of a failed run, for the log: never its
// text, which may hold what the analyzer printed.
func costlensErrKind(err error) string {
	var exit *exec.ExitError
	switch {
	case errors.Is(err, errCostlensTooLong):
		return "too_long"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &exit):
		return "exit"
	case errors.Is(err, exec.ErrWaitDelay):
		return "wait_delay"
	}
	return "start"
}

// Wait blocks until no run is in flight or ctx ends; curia review --json uses it once, with costlensLimit.
func (c *costlensCache) Wait(ctx context.Context) {
	c.mu.Lock()
	running, idle := c.running, c.idle
	c.mu.Unlock()
	if !running {
		return
	}
	select {
	case <-idle:
	case <-ctx.Done():
	}
}

// costlensProc is the process's one cache (one run at a time per process),
// made on first use.
var (
	costlensProcMu sync.Mutex
	costlensProc   *costlensCache
)

// costlensFor is the process's cache, made from cfg on first use.
func costlensFor(cfg config.Config) *costlensCache {
	costlensProcMu.Lock()
	defer costlensProcMu.Unlock()
	if costlensProc == nil {
		costlensProc = newCostlensCache(cfg, cfg.LogPath, func() time.Time { return clock() })
	}
	return costlensProc
}

// costlensTaskCost is task t's branch cost as the process cache holds it
// now; it never waits. A task without a branch or a worktree asks nothing.
func costlensTaskCost(cfg config.Config, t model.Task) (float64, bool) {
	if t.Branch == "" || t.Worktree == "" {
		return 0, false
	}
	return costlensFor(cfg).Get(t.Worktree, t.Branch)
}

// costlensTaskCostWait is costlensTaskCost after waiting, at most
// costlensLimit or until ctx ends, for the run it starts (curia review --json).
func costlensTaskCostWait(ctx context.Context, cfg config.Config, t model.Task) (float64, bool) {
	if usd, ok := costlensTaskCost(cfg, t); ok || t.Branch == "" || t.Worktree == "" {
		return usd, ok
	}
	wctx, cancel := context.WithTimeout(ctx, costlensLimit)
	defer cancel()
	costlensFor(cfg).Wait(wctx)
	return costlensTaskCost(cfg, t)
}

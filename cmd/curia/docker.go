package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/usage"
)

// F19 part 2, Docker. A symlink named docker to this binary sits first on the
// PATH of sessions `curia launch` starts (and only there: nothing is written
// to the user's shell config); main runs dockerWrapperMain when invoked under
// that name. The wrapper labels and limits `run` and `create`, counts before
// them, and execs the bin docker (found on PATH without the wrapper), so
// stdio, signals and the exit code are docker's own. No shell is involved.

const (
	dockerWrapperDirName = "docker-wrapper"
	// dockerCallTimeout bounds the wrapper's one `docker ps`.
	dockerCallTimeout = 4 * time.Second
	maxDockerOutput   = 16 << 20
)

// dockerSampleTimeout bounds each docker call of the monitor; tests shorten it.
// dockerExec and dockerStat are variables so tests can look at them and the
// linter sees no taint from the PATH.
var (
	dockerExec = syscall.Exec
	dockerStat = os.Stat
)

var dockerSampleTimeout = 8 * time.Second

// dockerSafeID is what may go into a label value: ids Curia made.
var dockerSafeID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,80}$`)

// dockerRealPath finds the first executable `docker` on pathEnv that is not
// this binary (by file identity) and not in a wrapper directory, so a
// wrapper first on PATH twice, or an older one, cannot be chosen.
func dockerRealPath(pathEnv string) (string, error) {
	var selfInfo os.FileInfo
	if self, err := os.Executable(); err == nil {
		selfInfo, _ = dockerStat(self)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) || filepath.Base(dir) == dockerWrapperDirName {
			continue
		}
		p := filepath.Join(dir, "docker")
		fi, err := dockerStat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 || (selfInfo != nil && os.SameFile(fi, selfInfo)) {
			continue
		}
		return p, nil
	}
	return "", exec.ErrNotFound
}

// dockerOutput runs docker with args under a deadline, its output bounded;
// a hung docker is killed and its pipes closed.
func dockerOutput(ctx context.Context, bin string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out boundedBuffer
	out.max = maxDockerOutput
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w", args[0], err)
	}
	return out.String(), nil
}

// dockerFound is the real docker on this process's PATH, if any.
func dockerFound() (string, bool) {
	p, err := dockerRealPath(os.Getenv("PATH"))
	return p, err == nil
}

// dockerQuiet is a monitor call of docker: any failure is just "no answer".
func dockerQuiet(ctx context.Context, bin string, args ...string) (string, bool) {
	out, err := dockerOutput(ctx, bin, dockerSampleTimeout, args...)
	return out, err == nil
}

// dockerWrapperMain is the whole of `docker` under the wrapper. It never returns.
func dockerWrapperMain(args []string) {
	bin, err := dockerRealPath(os.Getenv("PATH"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "curia: docker wrapper: no docker found on PATH")
		os.Exit(127)
	}
	if call := usage.ParseDocker(args); call.Kind != "" {
		args = dockerPrepare(bin, call, args)
	}
	err = dockerExec(bin, append([]string{"docker"}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "curia: docker wrapper: exec %s: %v\n", sanitize.Line(bin, 200), err)
	os.Exit(126)
}

// dockerPrepare is the wrapper's work for run, create and compose up|run: the
// count check, then the label and limits for run and create. Anything that
// fails here (no config, no store, docker not answering) leaves the command
// as it was: the wrapper never blocks work it cannot judge.
func dockerPrepare(bin string, call usage.DockerCall, args []string) []string {
	cfg, err := loadConfig()
	if err != nil {
		return args
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*dockerCallTimeout)
	defer cancel()
	st, err := openStore(cfg)
	var sess model.Session
	var id string
	var refs []usage.SessionRef
	var vm *store.DockerVM
	if err == nil {
		defer func() { _ = st.Close() }()
		if cur, ok, verr := st.LatestDockerVM(ctx); verr == nil && ok && usage.ResourceFresh(cur.At, clock(), cfg.Resources.Interval) {
			vm = &cur
		}
		sess, id = mcpSession(ctx, st, slog.New(slog.DiscardHandler))
		refs = dockerSessionRefs(ctx, st)
	} else {
		id = os.Getenv("CURIA_SESSION_ID")
	}
	task := sess.TaskID
	if task == "" {
		task = os.Getenv("CURIA_TASK_ID")
	}
	// D-16 = A: compose up is refused at a cap like run and create, unless its
	// project already runs (it then only adds what is missing).
	if msg := dockerCapMessage(ctx, bin, cfg, refs, id, call, vm); msg != "" {
		dockerRefuse(cfg, id, msg)
	}
	if call.Kind != "run" {
		return args
	}
	var add []string
	for _, l := range [][2]string{{"curia.session", id}, {"curia.task", task}} {
		if dockerSafeID.MatchString(l[1]) {
			add = append(add, "--label", l[0]+"="+l[1])
		}
	}
	add = append(add, usage.DockerLimits(call, int64(cfg.Resources.ContainerMemoryMB), cfg.Resources.ContainerCPUs)...)
	return usage.DockerInsert(args, call.At, add)
}

// dockerCapMessage is the one count and memory check (the caller refuses on
// a message): "" while under the caps, with every cap off, when docker cannot
// be asked, or for a compose up of a project this session already runs. vm is
// the monitor's last current reading, nil when there is none (the memory
// check is then skipped).
func dockerCapMessage(ctx context.Context, bin string, cfg config.Config, refs []usage.SessionRef, session string, call usage.DockerCall, vm *store.DockerVM) string {
	r := cfg.Resources
	if r.MaxContainersPerSession == 0 && r.MaxContainersTotal == 0 && r.DockerMemoryRefusePct == 0 {
		return ""
	}
	out, err := dockerOutput(ctx, bin, dockerCallTimeout, "ps", "--format", usage.DockerPSFormat)
	if err != nil {
		return ""
	}
	if !dockerSafeID.MatchString(session) {
		session = ""
	}
	cs := usage.AttributeContainers(usage.ParseDockerPS(out), refs)
	if cwd, _ := os.Getwd(); call.Kind == "compose" && usage.ComposeRunning(cs, call, os.Getenv("COMPOSE_PROJECT_NAME"), cwd, session) {
		return ""
	}
	if msg := usage.DockerOverCap(cs, session, r.MaxContainersPerSession, r.MaxContainersTotal); msg != "" {
		return msg
	}
	if vm != nil {
		return usage.DockerMemoryOver(vm.UsedMB, vm.LimitMB, r.DockerMemoryRefusePct)
	}
	return ""
}

// dockerRefuse prints msg, records the refusal in Curia's log and exits 1.
func dockerRefuse(cfg config.Config, session, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	if logger, closeLog, err := openLog(cfg.LogPath); err == nil {
		logger.Warn("docker: refused", "session", sanitize.Line(session, 80), "reason", sanitize.Line(msg, 200))
		_ = closeLog()
	}
	os.Exit(1)
}

// dockerSessionRefs is the live sessions' attribution data; a failed read is none.
func dockerSessionRefs(ctx context.Context, st *store.Store) []usage.SessionRef {
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil
	}
	var refs []usage.SessionRef
	for _, s := range sessions {
		if s.State != model.StateEnded && s.State != model.StateStale {
			refs = append(refs, usage.SessionRef{ID: s.ID, TaskID: s.TaskID, Worktree: s.Worktree, Cwd: s.Cwd})
		}
	}
	return refs
}

// ---- the wrapper directory, for `curia launch` ----

// dockerLaunchEnv is env with the wrapper directory first on PATH. The
// directory holds one symlink, docker, to this binary. Any failure leaves
// env as it was: no wrapper, no harm.
func dockerLaunchEnv(cfg config.Config, env []string) []string {
	exe, err := os.Executable()
	if err != nil {
		return env
	}
	dir := filepath.Join(filepath.Dir(cfg.DBPath), dockerWrapperDirName)
	link := filepath.Join(dir, "docker")
	if cur, _ := os.Readlink(link); cur != exe {
		if os.MkdirAll(dir, 0o700) != nil {
			return env
		}
		_ = os.Remove(link)
		if os.Symlink(exe, link) != nil {
			return env
		}
	}
	path := os.Getenv("PATH")
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
			env = slices.Delete(slices.Clone(env), i, i+1)
			break
		}
	}
	return append(env, "PATH="+dir+string(filepath.ListSeparator)+path)
}

// ---- the monitor's job ----

// sampleContainers lists the running containers, attributes them and stores
// them with their CPU and memory from `docker stats --no-stream`, and the
// Docker VM's total memory from `docker info` with the containers' summed
// use. docker absent, the daemon down or a call over its deadline skips the
// tick without a word: nothing here may block or fail the loop.
func (e *monitorEnv) sampleContainers(ctx context.Context) error {
	if !e.cfg.Resources.Enabled {
		return nil
	}
	bin, found := dockerFound()
	if !found {
		return nil
	}
	psOut, ok := dockerQuiet(ctx, bin, "ps", "--format", usage.DockerPSFormat)
	if !ok {
		return nil
	}
	statsOut, ok := dockerQuiet(ctx, bin, "stats", "--no-stream", "--format", "{{json .}}")
	if !ok {
		return nil
	}
	refs := dockerSessionRefs(ctx, e.st)
	stats := usage.ParseDockerStats(statsOut)
	var rows []store.ContainerRow
	var usedMB int64
	for _, c := range usage.AttributeContainers(usage.ParseDockerPS(psOut), refs) {
		st := stats[c.ID]
		usedMB += st.MemMB
		rows = append(rows, store.ContainerRow{ID: c.ID, SessionID: c.SessionID, Name: c.Name, By: c.By, CPU: st.CPU, MemMB: st.MemMB})
	}
	at := e.now()
	if err := e.st.ReplaceContainers(ctx, rows, at); err != nil {
		return err
	}
	// The VM total is static and `docker info` has no current-use field: the
	// use is the containers' sum, which undercounts (probe P5).
	if info, ok := dockerQuiet(ctx, bin, "info", "--format", "{{.MemTotal}}"); ok {
		if b, perr := strconv.ParseInt(strings.TrimSpace(info), 10, 64); perr == nil && b > 0 {
			return e.st.AddDockerVM(ctx, at, usedMB, b>>20)
		}
	}
	return nil
}

// ---- the views ----

// containerView is what the monitor last saw, only while current: the
// containers and the VM reading, or nothing.
type containerView struct {
	Rows []store.ContainerRow
	VM   *store.DockerVM
}

func readContainerView(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) containerView {
	var v containerView
	if rows, seen, err := st.LatestContainers(ctx); err == nil && usage.ResourceFresh(seen, now, cfg.Resources.Interval) {
		v.Rows = rows
	}
	if vm, ok, err := st.LatestDockerVM(ctx); err == nil && ok && usage.ResourceFresh(vm.At, now, cfg.Resources.Interval) {
		v.VM = &vm
	}
	return v
}

// bySession is each session's container count and memory.
func (v containerView) bySession() (n map[string]int, memMB map[string]int64) {
	n, memMB = map[string]int{}, map[string]int64{}
	for _, r := range v.Rows {
		if r.SessionID != "" {
			n[r.SessionID]++
			memMB[r.SessionID] += r.MemMB
		}
	}
	return n, memMB
}

// dockerVMText is "VM 0.3/7.7 GB": the containers' summed use over the VM's
// total (the use undercounts: it leaves out the VM's own overhead).
func dockerVMText(vm store.DockerVM) string {
	return fmt.Sprintf("VM %.1f/%.1f GB", float64(vm.UsedMB)/1024, float64(vm.LimitMB)/1024)
}

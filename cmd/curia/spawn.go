package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
)

// spawnOptions are the choices of one spawn.
type spawnOptions struct {
	Model  string        // --model; "" uses the task's stored model, if any
	Team   bool          // --team
	Resume string        // a session id: curia resume
	Wait   time.Duration // how long to wait for the agent's first hook; 0: do not wait
	// Out receives the lines saying what the spawn did, as it goes (the
	// worktree, the setup script); nil discards them.
	Out io.Writer
}

// spawnResult is what a spawn started.
type spawnResult struct {
	Task    model.Task
	Pane    tmux.Pane
	Session model.Session // zero when the agent had not reported within Wait
}

var (
	// spawnSleep is the poll's sleep; tests replace it and clock.
	spawnSleep = time.Sleep
	// spawnSetupLimit bounds .curia/setup.sh; a launch claim is held for it
	// plus a minute.
	spawnSetupLimit = 10 * time.Minute
	// spawnBin is the curia binary the window runs; tests replace it.
	spawnBin = executablePath
)

const (
	spawnWait      = 20 * time.Second       // curia spawn's wait for the first hook
	spawnPoll      = 250 * time.Millisecond // its poll interval
	spawnLogCap    = 1 << 20                // the setup log's size limit
	spawnTailLines = 20                     // setup log lines shown on failure
	spawnLineMax   = 200                    // runes of a shown setup log line
)

// spawnSessionID is the form of a Claude Code session id (--resume).
var spawnSessionID = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// The flags of curia spawn.
var (
	spawnAgent = "claude"
	spawnModel string
	spawnTeam  bool
)

func init() {
	cmd := &cobra.Command{
		Use:   "spawn <id>",
		Short: "Start a task's agent in its own git worktree and tmux window",
		Long: "Creates (or reuses) the task's worktree under .curia-worktrees/ next to its\n" +
			"repository, on branch curia/<id>-<slug> from the task's base branch, runs the\n" +
			"worktree's .curia/setup.sh if it is executable (output in the setup log), opens\n" +
			"a tmux window named after the task in the repository's session and starts\n" +
			"claude there with the task's brief. Waits up to 20 s for the agent to report.",
		Args: cobra.ExactArgs(1),
		RunE: runSpawn,
	}
	cmd.Flags().StringVar(&spawnAgent, "agent", "claude", "the agent to start (only claude)")
	cmd.Flags().StringVar(&spawnModel, "model", "", "the model the agent uses")
	cmd.Flags().BoolVar(&spawnTeam, "team", false, "let the agent lead an agent team")
	rootCmd.AddCommand(cmd)
}

func runSpawn(cmd *cobra.Command, args []string) error {
	if spawnAgent != "claude" {
		return fmt.Errorf("--agent %q is not supported: only claude", sanitize.Line(spawnAgent, 40))
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	out := cmd.OutOrStdout()
	r, err := spawnTask(cmd.Context(), st, newTmuxClient(), cfg, args[0],
		spawnOptions{Model: spawnModel, Team: spawnTeam, Wait: spawnWait, Out: out})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, spawnLine(r))
	return err
}

// spawnTask is section 4.6, steps 1 to 4 and 6. curia spawn, curia resume
// and the dashboard's n key call it. Nothing it passes to tmux or git comes
// from the task's text: the window gets the task id, the worktree path and
// curia's own path, and the brief reaches the agent through curia launch.
// A failure after the launch claim releases the claim and says what stays.
func spawnTask(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, o spawnOptions) (spawnResult, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	t, err := spawnCheck(ctx, st, tm, cfg, id, &o)
	if err != nil {
		return spawnResult{}, err
	}
	claimed, err := spawnClaim(ctx, st, t)
	if err != nil {
		return spawnResult{}, err
	}
	r, err := spawnStart(ctx, st, tm, cfg, claimed, o)
	if err != nil {
		return r, spawnRelease(ctx, st, claimed, err)
	}
	return r, nil
}

// spawnCheckOptions refuses an id, model or session id that is not of the
// form Curia accepts, before anything else happens.
func spawnCheckOptions(id string, o spawnOptions) error {
	switch {
	case !task.ValidID(id):
		return fmt.Errorf("%q is not a task id (T-<n>)", sanitize.Line(id, 40))
	case o.Model != "" && !task.ValidModel(o.Model):
		return fmt.Errorf("model %q: must be 1 to 64 of A-Z, a-z, 0-9, ., _, [, ] and -", sanitize.Line(o.Model, 70))
	case o.Resume != "" && !spawnSessionID.MatchString(o.Resume):
		return fmt.Errorf("session id %q is not a Claude Code session id", sanitize.Line(o.Resume, 40))
	}
	return nil
}

// spawnCheck is step 1 before the claim: the options are valid, the task
// exists, is not dropped, is not paused (unless this is curia resume) and
// is not running. o.Model becomes the task's stored model when not given.
func spawnCheck(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, o *spawnOptions) (model.Task, error) {
	if err := spawnCheckOptions(id, *o); err != nil {
		return model.Task{}, err
	}
	t, found, err := st.GetTask(ctx, id)
	switch {
	case err != nil:
		return model.Task{}, err
	case !found:
		return model.Task{}, fmt.Errorf("no task %s", id)
	case t.Status == model.TaskDropped:
		return model.Task{}, fmt.Errorf("%s is dropped", id)
	case !t.PausedAt.IsZero() && o.Resume == "":
		return model.Task{}, fmt.Errorf("%s is paused: use curia resume", id)
	}
	if o.Model == "" {
		o.Model = t.AgentModel
	}
	if err = spawnCheckOptions(id, *o); err != nil {
		return model.Task{}, err
	}
	return t, spawnCheckIdle(ctx, st, tm, cfg, t)
}

// spawnCheckIdle refuses a task with a live linked session, or whose
// recorded pane is still open, after marking stale the sessions whose pane
// is gone. When tmux cannot list its panes (often: no server yet) no pane
// is open.
func spawnCheckIdle(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, t model.Task) error {
	if err := reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	panes, _ := tm.ListPanes(tctx)
	sessions, err := st.TaskSessions(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(sessions) > 0 {
		s := sessions[0]
		return fmt.Errorf("%s is already running in %s (session %s): end that session first",
			t.ID, spawnWhere(panes, s.Pane), spawnShortID(s.ID))
	}
	if t.Pane != "" && slices.ContainsFunc(panes, func(p tmux.Pane) bool { return p.ID == t.Pane }) {
		return fmt.Errorf("%s's window %s is still open: close it first", t.ID, spawnWhere(panes, t.Pane))
	}
	return nil
}

// spawnWhere names pane as session:window (pane), from tmux's list.
func spawnWhere(panes []tmux.Pane, pane string) string {
	i := slices.IndexFunc(panes, func(p tmux.Pane) bool { return p.ID == pane })
	if i < 0 {
		return "pane " + sanitize.Line(pane, 20)
	}
	return spawnPaneName(panes[i])
}

// spawnPaneName is session:window (pane), cleaned: tmux names are the user's.
func spawnPaneName(p tmux.Pane) string {
	return sanitize.Line(p.Session+":"+p.WindowName+" ("+p.ID+")", 120)
}

// spawnShortID is the first 8 runes of a session id, cleaned.
func spawnShortID(id string) string {
	r := []rune(sanitize.Line(id, 64))
	return string(r[:min(len(r), 8)])
}

// spawnClaim takes the launch claim (store.ClaimLaunch), held for the setup
// limit plus a minute.
func spawnClaim(ctx context.Context, st *store.Store, t model.Task) (model.Task, error) {
	hold := spawnSetupLimit + time.Minute
	claimed, err := st.ClaimLaunch(ctx, t.ID, clock(), hold)
	if !errors.Is(err, store.ErrTaskBusy) {
		return claimed, err
	}
	if cur, found, gerr := st.GetTask(ctx, t.ID); gerr == nil && found {
		t = cur
	}
	return model.Task{}, fmt.Errorf("another spawn of %s started at %s: wait for it, or retry after %s",
		t.ID, t.LaunchedAt.Local().Format(time.TimeOnly), t.LaunchedAt.Add(hold).Local().Format(time.TimeOnly))
}

// spawnRelease clears the launch claim made at claimed.LaunchedAt, unless
// another spawn has claimed the task since, and adds that to err.
func spawnRelease(ctx context.Context, st *store.Store, claimed model.Task, err error) error {
	_, uerr := st.UpdateTask(ctx, claimed.ID, func(t model.Task) (model.Task, error) {
		if t.LaunchedAt.Equal(claimed.LaunchedAt) {
			t.LaunchedAt, t.UpdatedAt = time.Time{}, clock()
		}
		return t, nil
	})
	if uerr != nil {
		return fmt.Errorf("%w (and the launch claim could not be released: %w)", err, uerr)
	}
	return err
}

// spawnStart is steps 2 to 4 and 6, after the claim.
func spawnStart(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, t model.Task, o spawnOptions) (spawnResult, error) {
	t, err := spawnWorktree(ctx, st, t, o.Out)
	if err != nil {
		return spawnResult{Task: t}, err
	}
	stays := " (the worktree " + sanitize.Line(t.Worktree, 300) + " stays; a retry reuses it)"
	if err = spawnSetup(ctx, cfg, t, o.Out); err != nil {
		return spawnResult{Task: t}, fmt.Errorf("%w%s", err, stays)
	}
	pane, err := spawnWindow(ctx, tm, t, o)
	if err != nil {
		return spawnResult{Task: t}, fmt.Errorf("%w%s", err, stays)
	}
	t, err = spawnLink(ctx, st, tm, t, pane, o)
	r := spawnResult{Task: t, Pane: pane}
	if err != nil {
		return r, fmt.Errorf("%w%s", err, stays)
	}
	r.Session, err = spawnAwait(ctx, st, tm, t, pane, o.Wait)
	if err != nil {
		return r, fmt.Errorf("%w%s", err, stays)
	}
	return r, nil
}

// spawnWorktreePath is task.WorktreePath, checked to be a direct child of
// .curia-worktrees next to repo.
func spawnWorktreePath(repo, id string) (string, error) {
	p := task.WorktreePath(repo, id)
	parent := filepath.Join(filepath.Dir(repo), ".curia-worktrees")
	if filepath.Dir(p) != parent || !strings.HasSuffix(filepath.Base(p), "-"+id) {
		return "", fmt.Errorf("worktree path %q is not inside %s", sanitize.Line(p, 300), sanitize.Line(parent, 300))
	}
	return p, nil
}

// spawnWorktree is step 2: it reuses the task's worktree when the path is
// already a worktree of the repository on the task's branch (a retry), and
// otherwise adds one, refusing to touch anything else at the path. The
// branch and path are stored on the task.
func spawnWorktree(ctx context.Context, st *store.Store, t model.Task, out io.Writer) (model.Task, error) {
	branch := t.Branch
	if branch == "" {
		branch = task.Branch(t.ID, t.Title)
	}
	if t.BaseBranch == "" {
		return t, fmt.Errorf("%s has no base branch: set one with --base", t.ID)
	}
	path, err := spawnWorktreePath(t.Repo, t.ID)
	if err != nil {
		return t, err
	}
	if err = errors.Join(worktreeCheckBranch("branch", branch), worktreeCheckBranch("base", t.BaseBranch),
		worktreeCheckRepo(ctx, t.Repo)); err != nil {
		return t, err
	}
	ours, err := worktreeIsOurs(ctx, t.Repo, path, branch)
	if err != nil {
		return t, err
	}
	shown := sanitize.Line(path, 300) + " on branch " + branch
	if ours {
		_, _ = fmt.Fprintf(out, "Worktree: reusing %s\n", shown)
	} else {
		if err = spawnAddWorktree(ctx, t, path, branch); err != nil {
			return t, err
		}
		_, _ = fmt.Fprintf(out, "Worktree: created %s (from %s)\n", shown, t.BaseBranch)
	}
	return st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		cur.Branch, cur.Worktree, cur.UpdatedAt = branch, path, clock()
		return cur, nil
	})
}

// spawnAddWorktree adds the worktree at path, which must not exist: a
// directory, file or link there is not Curia's, and is left alone.
func spawnAddWorktree(ctx context.Context, t model.Task, path, branch string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s exists and is not %s's worktree on %s: Curia leaves it alone; move it away to spawn %s",
			sanitize.Line(path, 300), t.ID, branch, t.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return worktreeAdd(ctx, t.Repo, path, branch, t.BaseBranch)
}

// spawnSetupScript finds the worktree's .curia/setup.sh and returns the
// path to run, or "" after saying why nothing runs: it is absent, resolves
// outside the worktree, or is not an executable regular file.
func spawnSetupScript(worktree string, out io.Writer) string {
	const name = ".curia/setup.sh"
	link := filepath.Join(worktree, ".curia", "setup.sh")
	skip := func(why string) string {
		_, _ = fmt.Fprintf(out, "Setup: %s %s; skipped\n", name, why)
		return ""
	}
	if _, err := os.Lstat(link); errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(out, "Setup: no %s\n", name)
		return ""
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return skip("cannot be resolved")
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil || !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return skip("resolves outside the worktree")
	}
	fi, err := os.Stat(resolved)
	switch {
	case err != nil || !fi.Mode().IsRegular():
		return skip("is not a regular file")
	case fi.Mode().Perm()&0o111 == 0:
		return skip("is not executable")
	}
	return resolved
}

// spawnSetupLogPath is the setup log of task id: setup/<id>.log next to the
// log file.
func spawnSetupLogPath(cfg config.Config, id string) string {
	return filepath.Join(filepath.Dir(cfg.LogPath), "setup", id+".log")
}

// spawnSetup is step 3: it runs the worktree's .curia/setup.sh, if it is
// to run, with no shell, the worktree as its directory, stdin from
// /dev/null, the task's variables, spawnSetupLimit and its output in the
// setup log (truncated first, capped at 1 MiB). A failure says the log's
// path and shows its last lines, cleaned.
func spawnSetup(ctx context.Context, cfg config.Config, t model.Task, out io.Writer) error {
	script := spawnSetupScript(t.Worktree, out)
	if script == "" {
		return nil
	}
	logPath := spawnSetupLogPath(cfg, t.ID)
	runErr, err := spawnRunSetup(ctx, t, script, logPath)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if runErr == nil {
		_, _ = fmt.Fprintf(out, "Setup: ran .curia/setup.sh (log: %s)\n", logPath)
		return nil
	}
	_, _ = fmt.Fprintf(out, "Setup: .curia/setup.sh failed: %s. Last lines of %s:\n", runErr, logPath)
	for _, l := range spawnTail(logPath) {
		_, _ = fmt.Fprintf(out, "  %s\n", l)
	}
	return fmt.Errorf("setup script failed: %w; its log is %s", runErr, logPath)
}

// spawnRunSetup runs script for t with its output in the log at logPath.
// It returns the script's failure as runErr, and a failure to run it at
// all (the log cannot be opened) as err.
func spawnRunSetup(ctx context.Context, t model.Task, script, logPath string) (runErr, err error) {
	if err = os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Clean(logPath), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithTimeout(ctx, spawnSetupLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, script)
	cmd.Dir = t.Worktree
	cmd.Env = append(spawnSetupEnv(), "CURIA_TASK_ID="+t.ID, "CURIA_REPO="+t.Repo, "CURIA_WORKTREE="+t.Worktree)
	w := &spawnCapWriter{w: f, left: spawnLogCap}
	cmd.Stdout, cmd.Stderr = w, w
	// Its own process group, so the limit kills what the script started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	runErr = cmd.Run()
	switch {
	case errors.Is(runErr, exec.ErrWaitDelay):
		runErr = nil // it succeeded; something it left running holds its output
	case runErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		runErr = fmt.Errorf("timed out after %s", spawnSetupLimit)
	}
	return runErr, nil
}

// spawnSetupEnv is this process's environment without git's repository
// variables, which would point the script's git at another repository.
func spawnSetupEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitRepoVars, name)
	})
}

// spawnCapWriter writes to w until left bytes are written and then drops
// the rest, reporting every write as complete so the script never sees an
// error.
type spawnCapWriter struct {
	w    io.Writer
	left int
}

func (c *spawnCapWriter) Write(p []byte) (int, error) {
	if c.left > 0 {
		n := min(len(p), c.left)
		if _, err := c.w.Write(p[:n]); err != nil {
			return 0, err
		}
		c.left -= n
	}
	return len(p), nil
}

// spawnTail returns the last spawnTailLines lines of the log at path, each
// cleaned to one line of at most spawnLineMax runes.
func spawnTail(path string) []string {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return []string{"(the log cannot be read: " + sanitize.Line(err.Error(), spawnLineMax) + ")"}
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	lines = lines[max(0, len(lines)-spawnTailLines):]
	for i, l := range lines {
		lines[i] = sanitize.Line(l, spawnLineMax)
	}
	return lines
}

// spawnCommand is the window's command: curia launch with the task id and
// the options, nothing taken from the task's text.
func spawnCommand(bin, id string, o spawnOptions) []string {
	c := []string{bin, "launch", id}
	if o.Model != "" {
		c = append(c, "--model", o.Model)
	}
	if o.Team {
		c = append(c, "--team")
	}
	if o.Resume != "" {
		c = append(c, "--resume", o.Resume)
	}
	return c
}

// spawnWindow is step 4. tmux falls back to $HOME for a start directory
// that does not exist, so the worktree is checked again first: it must
// still be the task's worktree (the setup script ran in it).
func spawnWindow(ctx context.Context, tm tmux.Client, t model.Task, o spawnOptions) (tmux.Pane, error) {
	ours, err := worktreeIsOurs(ctx, t.Repo, t.Worktree, t.Branch)
	if err != nil {
		return tmux.Pane{}, err
	}
	if !ours {
		return tmux.Pane{}, fmt.Errorf("%s is missing or no longer %s's worktree on %s; no window opened",
			sanitize.Line(t.Worktree, 300), t.ID, t.Branch)
	}
	bin, err := spawnBin()
	if err != nil {
		return tmux.Pane{}, fmt.Errorf("find the curia binary: %w", err)
	}
	pane, err := tm.NewWindow(ctx, tmux.WindowSpec{
		Session: task.SessionName(t.Repo),
		Name:    t.ID,
		Dir:     t.Worktree,
		Env:     []string{"CURIA_TASK_ID=" + t.ID},
		Command: spawnCommand(bin, t.ID, o),
	})
	if err != nil {
		return tmux.Pane{}, fmt.Errorf("open the window: %w", err)
	}
	return pane, nil
}

// spawnLink is step 6's write: the pane, team and model are stored and a
// backlog task becomes ready. If that fails, the window just opened is
// closed again, so nothing runs that the task does not record.
func spawnLink(ctx context.Context, st *store.Store, tm tmux.Client, t model.Task, pane tmux.Pane, o spawnOptions) (model.Task, error) {
	stored, err := st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		cur.Pane, cur.Team, cur.AgentModel, cur.UpdatedAt = pane.ID, o.Team, o.Model, clock()
		if cur.Status == model.TaskBacklog {
			cur.Status = model.TaskReady
		}
		return cur, nil
	})
	if err == nil {
		return stored, nil
	}
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subprocessTimeout)
	defer cancel()
	if kerr := tm.KillPane(kctx, pane.ID); kerr != nil {
		return t, fmt.Errorf("record the window: %w (and the window %s could not be closed: %w)", err, spawnPaneName(pane), kerr)
	}
	return t, fmt.Errorf("record the window: %w (the window was closed)", err)
}

// spawnAwait is step 6's wait: it polls for a live session of the task
// started at or after the launch claim, every spawnPoll for wait. When
// none appears and the pane is gone, the agent exited before its first
// hook, which is what Claude Code does when the user leaves its trust
// dialog with its default answer, and that is an error.
func spawnAwait(ctx context.Context, st *store.Store, tm tmux.Client, t model.Task, pane tmux.Pane, wait time.Duration) (model.Session, error) {
	if wait <= 0 {
		return model.Session{}, nil
	}
	deadline := clock().Add(wait)
	for ctx.Err() == nil {
		sessions, err := st.TaskSessions(ctx, t.ID)
		if err != nil {
			return model.Session{}, err
		}
		for _, s := range sessions {
			if !s.StartedAt.Before(t.LaunchedAt) {
				return s, nil
			}
		}
		if !clock().Before(deadline) {
			break
		}
		spawnSleep(spawnPoll)
	}
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), subprocessTimeout)
	defer cancel()
	panes, err := tm.ListPanes(tctx)
	if err != nil || slices.ContainsFunc(panes, func(p tmux.Pane) bool { return p.ID == pane.ID }) {
		return model.Session{}, nil
	}
	return model.Session{}, fmt.Errorf("the agent in %s exited before it reported, and its window closed. "+
		"If Claude Code does not trust %s yet, run claude there once and accept its trust dialog, then run curia spawn %s again",
		spawnPaneName(pane), sanitize.Line(t.Repo, 300), t.ID)
}

// spawnLine is the line of step 6.
func spawnLine(r spawnResult) string {
	where := spawnPaneName(r.Pane)
	if r.Session.ID != "" {
		return fmt.Sprintf("%s running in %s, session %s", r.Task.ID, where, spawnShortID(r.Session.ID))
	}
	return fmt.Sprintf("%s launched in %s; the agent has not reported yet (look at the window: it may be showing a startup dialog)",
		r.Task.ID, where)
}

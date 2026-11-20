package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/usage"
)

// spawnOptions are the choices of one spawn.
type spawnOptions struct {
	Agent string // --agent: claude, codex, or empty: the task's own, claude for a task never spawned
	Model string // --model; "" uses the task's stored model, if any, or spawnModel's default
	// ModelOnce marks Model as spawnModel's limit-forecast default: this
	// spawn's only, never stored on the task (Ruling P5-R7).
	ModelOnce bool
	Team      bool          // --team
	Resume    string        // a session id: curia resume
	Wait      time.Duration // how long to wait for the agent's first hook; 0: do not wait
	// ReadyOnly refuses a task whose stored status is not ready: the caller
	// is an agent (Ruling 113).
	ReadyOnly bool
	// AfterPause tells the agent that its branch ends in the pause's WIP
	// commit (curia resume).
	AfterPause bool
	// Out receives the lines saying what the spawn did, as it goes (the
	// worktree, the setup script); nil discards them.
	Out io.Writer
}

// spawnResult is what a spawn started.
type spawnResult struct {
	Task    model.Task
	Pane    tmux.Pane
	Session model.Session // zero when the agent had not reported within Wait
	// Note is spawnModel's note, "" when none, one line per sentence; spawnTask
	// also prints it to Out before anything else.
	Note string
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
	spawnWait = 20 * time.Second       // curia spawn's wait for the first hook
	spawnPoll = 250 * time.Millisecond // its poll interval
)

// spawnSessionID is the form of an agent session id (--resume): Claude
// Code's, and the UUID codex resume takes.
var spawnSessionID = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// The flags of curia spawn.
var (
	spawnAgent     string
	spawnFlagModel string
	spawnTeam      bool
)

func init() {
	cmd := &cobra.Command{
		Use:   "spawn <id>",
		Short: "Start a task's agent in its own git worktree and tmux window",
		Long: "Creates (or reuses) the task's worktree under .curia-worktrees/ next to its\n" +
			"repository, on branch curia/<id>-<slug> from the task's base branch, runs the\n" +
			"worktree's .curia/setup.sh if you approved its exact content (curia setup\n" +
			"approve; output in the setup log), opens\n" +
			"a tmux window named after the task in the repository's session and starts\n" +
			"the agent there with the task's brief (claude, or codex with --agent codex;\n" +
			"without --agent, the task's own). Under a limit forecast a Claude Code agent\n" +
			"with no model starts on usage.cheaper_model; --model always wins.\n" +
			"Waits up to 20 s for the agent to report.",
		Args: cobra.ExactArgs(1),
		RunE: runSpawn,
	}
	cmd.Flags().StringVar(&spawnAgent, "agent", "", "the agent to start: claude or codex (default: the task's own, claude for a new task)")
	cmd.Flags().StringVar(&spawnFlagModel, "model", "", "the model the agent uses")
	cmd.Flags().BoolVar(&spawnTeam, "team", false, "let the agent lead an agent team")
	rootCmd.AddCommand(cmd)
}

func runSpawn(cmd *cobra.Command, args []string) error {
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
	// An agent (anything but the user at a terminal) may spawn only what the
	// user marked ready (Ruling 113).
	r, err := spawnTask(cmd.Context(), st, newTmuxClient(), cfg, args[0],
		spawnOptions{Agent: spawnAgent, Model: spawnFlagModel, Team: spawnTeam, Wait: spawnWait, Out: out, ReadyOnly: !userAtTerminal()})
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
	t, note, err := spawnCheck(ctx, st, tm, cfg, id, &o)
	if err != nil {
		return spawnResult{}, err
	}
	claimed, err := spawnClaim(ctx, st, t)
	if err != nil {
		return spawnResult{}, err
	}
	if note != "" {
		_, _ = fmt.Fprintln(o.Out, note)
	}
	r, err := spawnStart(ctx, st, tm, cfg, claimed, o)
	r.Note = note
	if err != nil {
		return r, spawnRelease(ctx, st, claimed, err)
	}
	return r, nil
}

// spawnChosenOption names the first option that chooses how the agent runs.
func spawnChosenOption(o spawnOptions) string {
	switch {
	case o.Agent != "":
		return "--agent"
	case o.Model != "":
		return "--model"
	case o.Team:
		return "--team"
	case o.Resume != "":
		return "--resume"
	}
	return ""
}

// spawnCheckOptions refuses an id, agent, model or session id that is not
// of the form Curia accepts, before anything else happens. The agent is
// one of two fixed names; a model or session id that starts with a dash
// would reach the agent's arguments as an option, and is refused.
func spawnCheckOptions(id string, o spawnOptions) error {
	switch {
	case !task.ValidID(id):
		return fmt.Errorf("%q is not a task id (T-<n>)", sanitize.Line(id, 40))
	case o.Agent != "" && o.Agent != model.AgentClaude && o.Agent != model.AgentCodex:
		return fmt.Errorf("--agent %q is not supported: claude or codex", sanitize.Line(o.Agent, 40))
	case o.Model != "" && !spawnValidModel(o.Model):
		return fmt.Errorf("model %q: must be 1 to 64 of A-Z, a-z, 0-9, ., _, [, ] and -, not starting with -", sanitize.Line(o.Model, 70))
	case o.Resume != "" && (!spawnSessionID.MatchString(o.Resume) || strings.HasPrefix(o.Resume, "-")):
		return fmt.Errorf("session id %q is not a session id", sanitize.Line(o.Resume, 40))
	}
	return nil
}

// spawnValidModel is task.ValidModel, and not starting with a dash.
func spawnValidModel(m string) bool {
	return task.ValidModel(m) && !strings.HasPrefix(m, "-")
}

// spawnAgentOf is the agent a spawn of t with o starts: the task's stored
// agent on a resume or without --agent (claude when none is stored), else
// o.Agent.
func spawnAgentOf(t model.Task, o spawnOptions) string {
	if o.Resume == "" && o.Agent != "" {
		return o.Agent
	}
	return spawnStoredAgent(t)
}

// spawnStoredAgent is t's stored agent, claude when none is stored. The
// stored model (agent_model) is always this agent's: spawn writes the two
// together, and clears the model whenever it changes the agent
// (spawnStoreAgent, spawnLink).
func spawnStoredAgent(t model.Task) string {
	return cmp.Or(t.Agent, model.AgentClaude)
}

// spawnCheckAgent sets o.Agent to the agent this spawn starts and refuses
// an agent team for codex.
func spawnCheckAgent(t model.Task, o *spawnOptions) error {
	o.Agent = spawnAgentOf(t, *o)
	if o.Agent == model.AgentCodex && o.Team {
		return fmt.Errorf("agent teams are Claude Code's; spawn %s without --team or without --agent codex", t.ID)
	}
	if _, err := launchLookPath(o.Agent); o.Agent == model.AgentCodex && err != nil {
		return fmt.Errorf("codex is not installed (not on PATH): install the Codex CLI, or spawn %s with --agent claude", t.ID)
	}
	return nil
}

// spawnModel is section 4.4's cheaper-model rule: o.Model when set, else
// the task's stored model when it was stored for the agent this spawn
// starts, else spawnCheaperModel. A stored model of the other agent is not
// used, and the note says so (Ruling P5-R7). once reports that the model is
// the limit-forecast default, which is this spawn's only and not stored.
func spawnModel(ctx context.Context, st *store.Store, cfg config.Config, t model.Task, o spawnOptions, now time.Time) (modelName, note string, once bool) {
	agent := spawnAgentOf(t, o)
	var other string
	switch {
	case o.Model != "":
		return o.Model, "", false
	case t.AgentModel != "" && spawnStoredAgent(t) == agent:
		return t.AgentModel, "", false
	case t.AgentModel != "":
		other = fmt.Sprintf("%s's stored model %s was chosen for %s; %s starts without it (pass --model to choose)",
			t.ID, sanitize.Line(t.AgentModel, 70), spawnStoredAgent(t), agent)
	}
	modelName, note = spawnCheaperModel(ctx, st, cfg, t, o, agent, now)
	if other != "" {
		note = strings.TrimSuffix(other+"\n"+note, "\n")
	}
	return modelName, note, modelName != ""
}

// spawnCheaperModel is, for Claude Code on a new start (no resume) while a
// limit forecast warns, the configured cheaper model and the note saying
// so; otherwise no model. A failed usage read, or a cheaper model that is
// not a model name, gives no model and no note (logged): nothing automatic
// stops a spawn.
func spawnCheaperModel(ctx context.Context, st *store.Store, cfg config.Config, t model.Task, o spawnOptions, agent string, now time.Time) (modelName, note string) {
	if o.Resume != "" || agent != model.AgentClaude || cfg.Usage.CheaperModel == "" {
		return "", ""
	}
	cheaper := cfg.Usage.CheaperModel
	if !spawnValidModel(cheaper) {
		logWarn(cfg.LogPath, "spawn: usage.cheaper_model is not a model name; the limit forecast default is off", "task", t.ID)
		return "", ""
	}
	samples, err := st.UsageSince(ctx, now.Add(-usage.BurnWindow))
	if err != nil {
		logWarn(cfg.LogPath, "spawn: the limit forecast could not read usage", "task", t.ID, "err", err.Error())
		return "", ""
	}
	w, ok := usage.Warning(usage.Forecasts(samples, now), now)
	if !ok {
		return "", ""
	}
	return cheaper, fmt.Sprintf("limit forecast (%s): starting %s on %s; pass --model to choose", usage.BadgeText(w, now), t.ID, cheaper)
}

// spawnCheck is step 1 before the claim: the options are valid, the task
// exists, is not dropped, is not paused (unless this is curia resume) and
// is not running; for an agent (ReadyOnly) it is stored ready and, by its
// newest review, neither in review nor done (I7). o.Agent becomes the
// agent this spawn starts and o.Model spawnModel's model; the note is
// spawnModel's.
func spawnCheck(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, o *spawnOptions) (model.Task, string, error) {
	t, err := spawnCheckTask(ctx, st, id, o)
	if err != nil {
		return model.Task{}, "", err
	}
	var note string
	o.Model, note, o.ModelOnce = spawnModel(ctx, st, cfg, t, *o, clock())
	if err = spawnCheckOptions(id, *o); err != nil {
		return model.Task{}, "", err
	}
	return t, note, spawnCheckIdle(ctx, st, tm, cfg, t)
}

// spawnCheckTask is spawnCheck's checks of the options and the task.
func spawnCheckTask(ctx context.Context, st *store.Store, id string, o *spawnOptions) (model.Task, error) {
	if opt := spawnChosenOption(*o); o.ReadyOnly && opt != "" {
		return model.Task{}, fmt.Errorf("%s is the user's choice: %s's stored values apply to a spawn that is not the user's", opt, id)
	}
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
	case o.ReadyOnly && t.Status != model.TaskReady:
		return model.Task{}, fmt.Errorf("%s is %s: an agent may spawn only a task the user marked ready (curia task ready %s)",
			id, t.Status, id)
	}
	if o.ReadyOnly {
		if err = spawnCheckReviewed(ctx, st, t); err != nil {
			return model.Task{}, err
		}
	}
	return t, spawnCheckAgent(t, o)
}

// spawnCheckReviewed refuses a task whose newest review makes it review or
// done (task.Derive): its stored status stays ready for its whole life, so
// that alone would let an agent start finished work again.
func spawnCheckReviewed(ctx context.Context, st *store.Store, t model.Task) error {
	rs, err := st.ListReviews(ctx, store.ReviewFilter{TaskID: t.ID, Limit: 1})
	if err != nil || len(rs) == 0 {
		return err
	}
	if s := task.Derive(t, nil, 0, rs[0].Status); s == model.TaskReview || s == model.TaskDone {
		return fmt.Errorf("%s is %s (%s is %s): an agent may spawn only a task the user marked ready that is neither in review nor done",
			t.ID, s, rs[0].ID, reviewStatusWord(rs[0].Status))
	}
	return nil
}

// spawnCheckIdle refuses a task with a live linked session, or whose
// recorded pane is still open, after marking stale the sessions whose pane
// is gone. A pane is the task's only when tmux lists it on a server that
// started before the launch (lifecyclePaneStateOf): after a tmux restart
// the same id names someone else's pane. A listed pane that cannot be told
// apart (no launch time) still refuses. When tmux cannot list its panes
// (often: no server yet) no pane is open.
func spawnCheckIdle(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, t model.Task) error {
	if err := reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	panes, perr := tm.ListPanes(tctx)
	sessions, err := st.TaskSessions(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(sessions) > 0 {
		s := sessions[0]
		return fmt.Errorf("%s is already running in %s (session %s): end that session first",
			t.ID, spawnWhere(panes, s.Pane), spawnShortID(s.ID))
	}
	if spawnPaneOpen(panes, perr, t) {
		return fmt.Errorf("%s's window %s is still open: close it first", t.ID, spawnWhere(panes, t.Pane))
	}
	return nil
}

// spawnPaneOpen reports whether t's recorded pane is still its window: tmux
// lists it and it is not a recycled id (lifecyclePaneStateOf), or it is
// listed and cannot be told apart.
func spawnPaneOpen(panes []tmux.Pane, perr error, t model.Task) bool {
	switch lifecycleTaskPaneState(panes, perr, t) {
	case lifecyclePaneOurs:
		return true
	case lifecyclePaneUnknown:
		return t.Pane != "" && slices.ContainsFunc(panes, func(p tmux.Pane) bool { return p.ID == t.Pane })
	}
	return false
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
// limit plus a minute while no window is recorded, and only if the task's
// recorded pane is still t.Pane, the one spawnCheckIdle found closed.
func spawnClaim(ctx context.Context, st *store.Store, t model.Task) (model.Task, error) {
	hold := spawnSetupLimit + time.Minute
	claimed, err := st.ClaimLaunch(ctx, t.ID, t.Pane, clock(), hold)
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
	if err = setupStep(ctx, st, cfg, t, o.Out); err != nil {
		return spawnResult{Task: t}, fmt.Errorf("%w%s", err, stays)
	}
	if t, err = spawnStoreAgent(ctx, st, t, o.Agent); err != nil {
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
	} else if err = spawnNewWorktree(ctx, t, path, branch, shown, out); err != nil {
		return t, err
	}
	return st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		cur.Branch, cur.Worktree, cur.UpdatedAt = branch, path, clock()
		return cur, nil
	})
}

// spawnNewWorktree adds the worktree at path on branch and says which: on a
// new branch from the base, or again on the branch that exists (after a
// pause or a clean of the worktree), with its commits.
func spawnNewWorktree(ctx context.Context, t model.Task, path, branch, shown string, out io.Writer) error {
	existed, err := refExists(ctx, t.Repo, "refs/heads/"+branch)
	if err == nil {
		err = spawnAddWorktree(ctx, t, path, branch)
	}
	if err != nil {
		return err
	}
	if existed {
		_, _ = fmt.Fprintf(out, "Worktree: added %s again (the branch exists, with its commits)\n", shown)
	} else {
		_, _ = fmt.Fprintf(out, "Worktree: created %s (from %s)\n", shown, t.BaseBranch)
	}
	return nil
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

// spawnStoreAgent stores the agent on the task before the window opens:
// curia launch, which the window runs at once, reads it from the store to
// choose the binary, so the window's command carries no agent name. A
// changed agent clears the stored model, which was the other agent's, so
// the stored model stays the stored agent's even if the window then fails.
func spawnStoreAgent(ctx context.Context, st *store.Store, t model.Task, agent string) (model.Task, error) {
	if agent == "" || agent == spawnStoredAgent(t) {
		return t, nil
	}
	return st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		if spawnStoredAgent(cur) != agent {
			cur.AgentModel = ""
		}
		cur.Agent, cur.UpdatedAt = agent, clock()
		return cur, nil
	})
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
	if o.AfterPause {
		c = append(c, "--after-pause")
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

// spawnLink is step 6's write: the pane, agent, team and model are stored and a
// backlog task becomes ready. The model is stored together with the agent it
// is for; a limit-forecast default (o.ModelOnce) is not stored (Ruling
// P5-R7). If that fails, the window just opened is closed again, so nothing
// runs that the task does not record.
func spawnLink(ctx context.Context, st *store.Store, tm tmux.Client, t model.Task, pane tmux.Pane, o spawnOptions) (model.Task, error) {
	keep := o.Model
	if o.ModelOnce {
		keep = ""
	}
	stored, err := st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		cur.Pane, cur.PaneSocket, cur.Team, cur.AgentModel, cur.UpdatedAt = pane.ID, pane.Socket, o.Team, keep, clock()
		if o.Agent != "" {
			cur.Agent = o.Agent
		}
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

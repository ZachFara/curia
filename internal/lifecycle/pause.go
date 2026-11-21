package lifecycle

import (
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

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// pauseSleep is the sleep of the wait for killed panes to go; tests
// replace it.
var pauseSleep = time.Sleep

const (
	pauseWait = 3 * time.Second        // how long pause waits for the panes it killed to go
	pausePoll = 100 * time.Millisecond // its poll interval
)

// PauseOptions are the flags of one pause.
type PauseOptions struct{ Force, NoVerify bool }

// pausePlan is what a pause stops: the panes it closes, the agents it
// cannot stop (only with --force) and the session resume continues.
type pausePlan struct {
	kill    []lifecycleTarget
	skipped []string
	resume  string
}

// PauseTask is section 4.7's pause, in its order. Each step prints one line
// when done; a failure says what is left and how to finish.
func PauseTask(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, o PauseOptions, out io.Writer) error {
	t, path, err := pauseCheck(ctx, st, id)
	if err != nil {
		return err
	}
	plan, err := pauseMakePlan(ctx, st, tm, cfg, t, o.Force)
	if err != nil {
		return err
	}
	if err = pauseStop(ctx, st, tm, cfg, t, plan, out); err != nil {
		return err
	}
	commit, err := pauseCommit(ctx, t, path, o.NoVerify, out)
	if err != nil {
		return err
	}
	kept := pauseKeepWorktree(ctx, path, out)
	if !kept {
		left := "Left: the agent's panes closed, " + commit + ", the worktree " + sanitize.Line(path, 300) + " in place."
		if err = pauseRemove(ctx, t, path, left, out); err != nil {
			return err
		}
	}
	return pauseRecord(ctx, st, t, plan.resume, kept, out)
}

// pauseCheck is pause's first rule: the task exists, was spawned, is not
// paused and has its own worktree on disk. It returns the task and the
// worktree's path.
func pauseCheck(ctx context.Context, st *store.Store, id string) (model.Task, string, error) {
	t, err := lifecycleLoad(ctx, st, id)
	switch {
	case err != nil:
		return t, "", err
	case t.Branch == "":
		return t, "", fmt.Errorf("%s has not been spawned (it has no branch): nothing to pause", id)
	case !t.PausedAt.IsZero():
		return t, "", fmt.Errorf("%s is already paused: curia resume %s starts it again", id, id)
	}
	path, onDisk, err := Worktree(ctx, t)
	if err != nil {
		return t, "", err
	}
	if !onDisk {
		return t, "", fmt.Errorf("%s has no worktree at %s: nothing to pause", id, sanitize.Line(path, 300))
	}
	return t, path, nil
}

// pauseMakePlan reconciles, then decides what the pause stops: every live
// linked session must be idle or needs_decision (unless force), and each
// pane to close must be shown by tmux to be the one the agent runs in. A
// pane Curia cannot tell is the agent's is never closed; it refuses the
// pause unless force. The resume session is picked before anything is
// killed, while the sessions' update times are their own.
func pauseMakePlan(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, t model.Task, force bool) (pausePlan, error) {
	if err := Reconcile(ctx, st, tm, cfg.LogPath, Clock()); err != nil {
		return pausePlan{}, err
	}
	linked, err := lifecycleLinked(ctx, st, t.ID)
	if err != nil {
		return pausePlan{}, err
	}
	panes, perr := lifecycleListPanes(ctx, tm)
	if err = lifecycleLaunching(t, linked, panes, perr); err != nil {
		return pausePlan{}, err
	}
	live := lifecycleLive(linked)
	if !force {
		if err = PauseCheckStates(t, live); err != nil {
			return pausePlan{}, err
		}
	}
	plan := pausePlan{resume: pauseResumeSession(linked)}
	var unknown []string
	for _, tg := range pauseTargets(t, live) {
		switch lifecycleTargetState(panes, perr, tg) {
		case lifecyclePaneOurs:
			plan.kill = append(plan.kill, tg)
		case lifecyclePaneUnknown:
			unknown = append(unknown, pauseUnknown(tg, panes, perr))
		}
	}
	if len(unknown) > 0 && !force {
		return pausePlan{}, fmt.Errorf("%s not paused: Curia cannot stop %s. Nothing was done. "+
			"Close it yourself and run curia pause %s again, or pass --force to pause without stopping it",
			t.ID, strings.Join(unknown, "; "), t.ID)
	}
	plan.skipped = unknown
	return plan, nil
}

// PauseCheckStates refuses a pause while a live linked session is working
// or waiting (or in any state but idle and needs_decision).
func PauseCheckStates(t model.Task, live []model.Session) error {
	for _, s := range live {
		if s.State != model.StateIdle && s.State != model.StateNeedsDecision {
			return fmt.Errorf("%s not paused: its session %s is %s. Nothing was done. "+
				"Wait until it is idle, or run curia pause %s --force to stop it mid-turn",
				t.ID, SpawnShortID(s.ID), s.State, t.ID)
		}
	}
	return nil
}

// pauseResumeSession is the session curia resume continues: the most
// recently updated linked session that is not a teammate, when its id is a
// Claude Code session id, else none.
func pauseResumeSession(linked []model.Session) string {
	var best model.Session
	for _, s := range linked {
		if s.TeamRole != model.RoleTeammate && (best.ID == "" || s.UpdatedAt.After(best.UpdatedAt)) {
			best = s
		}
	}
	if !spawnSessionID.MatchString(best.ID) {
		return ""
	}
	return best.ID
}

// pauseTargets are the panes pause closes: each live linked session's, then
// the task's recorded window pane, each pane once.
func pauseTargets(t model.Task, live []model.Session) []lifecycleTarget {
	var out []lifecycleTarget
	add := func(tg lifecycleTarget) {
		if !slices.ContainsFunc(out, func(o lifecycleTarget) bool { return tg.pane != "" && o.pane == tg.pane }) {
			out = append(out, tg)
		}
	}
	for _, s := range live {
		add(lifecycleTarget{pane: s.Pane, socket: s.Socket, since: s.StartedAt, what: "session " + SpawnShortID(s.ID)})
	}
	if t.Pane != "" {
		add(lifecycleTarget{pane: t.Pane, socket: t.PaneSocket, window: true, since: t.LaunchedAt, what: t.ID + "'s window"})
	}
	return out
}

// pauseUnknown says why target's agent cannot be stopped.
func pauseUnknown(tg lifecycleTarget, panes []tmux.Pane, perr error) string {
	switch {
	case tg.pane == "":
		return tg.what + ", which runs outside tmux"
	case perr != nil:
		return tg.what + " in pane " + sanitize.Line(tg.pane, 20) + " (tmux cannot list its panes: " + sanitize.Line(perr.Error(), 200) + ")"
	case tg.window && tg.socket == "":
		return tg.what + " in pane " + sanitize.Line(tg.pane, 20) + " (its tmux server was not recorded, so it may still be open on another server): close it yourself"
	case lifecycleOtherServer(panes, tg.socket):
		return tg.what + " in pane " + sanitize.Line(tg.pane, 20) + " of another tmux server (" +
			sanitize.Line(filepath.Base(tg.socket), 60) + "): run curia pause from that server"
	}
	return tg.what + ": pane " + sanitize.Line(tg.pane, 20) + " is open but nothing shows it is still the agent's"
}

// pauseStop closes the planned panes, each only if tmux still shows it to
// be the agent's at that moment, waits up to pauseWait for them to go and
// reconciles. A session of the task live in an open pane after that stops
// the pause.
func pauseStop(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, t model.Task, plan pausePlan, out io.Writer) error {
	var closed []string
	for _, tg := range plan.kill {
		panes, err := lifecycleListPanes(ctx, tm)
		if err != nil {
			return pauseStopError(t, closed, fmt.Errorf("list tmux panes: %w", err))
		}
		i := slices.IndexFunc(panes, func(p tmux.Pane) bool { return p.ID == tg.pane })
		if lifecyclePaneStateOf(panes, nil, tg.pane, tg.socket, tg.since) != lifecyclePaneOurs {
			_, _ = fmt.Fprintf(out, "Pane %s of %s is already gone\n", sanitize.Line(tg.pane, 20), tg.what)
			continue
		}
		kctx, cancel := context.WithTimeout(ctx, SubprocessTimeout)
		err = tm.KillPane(kctx, panes[i].ID)
		cancel()
		if err != nil {
			return pauseStopError(t, closed, fmt.Errorf("close pane %s: %w", SpawnPaneName(panes[i]), err))
		}
		closed = append(closed, panes[i].ID)
		_, _ = fmt.Fprintf(out, "Closed pane %s of %s\n", SpawnPaneName(panes[i]), tg.what)
	}
	if len(plan.kill) == 0 {
		_, _ = fmt.Fprintf(out, "No agent pane of %s is open\n", t.ID)
	}
	for _, s := range plan.skipped {
		_, _ = fmt.Fprintf(out, "Not stopped (--force): %s\n", s)
	}
	if err := pauseAwaitGone(ctx, tm, closed); err != nil {
		return pauseStopError(t, closed, err)
	}
	if err := Reconcile(ctx, st, tm, cfg.LogPath, Clock()); err != nil {
		return pauseStopError(t, closed, err)
	}
	return pauseCheckStopped(ctx, st, tm, t, closed)
}

// pauseStopError is a failure while stopping the agent: the panes closed so
// far are named, and nothing else was done.
func pauseStopError(t model.Task, closed []string, err error) error {
	what := "no pane was closed"
	if len(closed) > 0 {
		what = "panes closed: " + strings.Join(closed, ", ")
	}
	return fmt.Errorf("%s not paused: %w. Left: %s; no commit, the worktree untouched. To finish: run curia pause %s again",
		t.ID, err, what, t.ID)
}

// pauseAwaitGone waits up to pauseWait for the panes to leave tmux's list.
func pauseAwaitGone(ctx context.Context, tm tmux.Client, closed []string) error {
	if len(closed) == 0 {
		return nil
	}
	for i := 0; ; i++ {
		panes, err := lifecycleListPanes(ctx, tm)
		if err != nil {
			return fmt.Errorf("list tmux panes: %w", err)
		}
		open := slices.DeleteFunc(slices.Clone(closed), func(id string) bool {
			return !slices.ContainsFunc(panes, func(p tmux.Pane) bool { return p.ID == id })
		})
		if len(open) == 0 {
			return nil
		}
		if i >= int(pauseWait/pausePoll) {
			return fmt.Errorf("panes %s still open after %s", strings.Join(open, ", "), pauseWait)
		}
		pauseSleep(pausePoll)
	}
}

// pauseCheckStopped refuses to go on when a live session of the task is
// still in an open pane that was not left alone on purpose (a new one, say).
func pauseCheckStopped(ctx context.Context, st *store.Store, tm tmux.Client, t model.Task, closed []string) error {
	live, err := st.TaskSessions(ctx, t.ID)
	if err != nil {
		return pauseStopError(t, closed, err)
	}
	panes, perr := lifecycleListPanes(ctx, tm)
	for _, s := range live {
		if lifecyclePaneStateOf(panes, perr, s.Pane, s.Socket, s.StartedAt) == lifecyclePaneOurs {
			return pauseStopError(t, closed, fmt.Errorf("session %s of %s is running in %s", SpawnShortID(s.ID), t.ID,
				spawnWhere(panes, s.Pane)))
		}
	}
	return nil
}

// pauseCommit is the WIP commit, when the worktree has changes: everything
// but ignored files, through the repository's hooks unless noVerify. The
// index is recorded first (git write-tree) and put back (git read-tree,
// which touches no file) when the commit fails, so a failure leaves the
// worktree as it was. It returns what it committed, for later messages.
func pauseCommit(ctx context.Context, t model.Task, path string, noVerify bool, out io.Writer) (string, error) {
	dirty, err := gitops.WorktreeDirty(ctx, path)
	if err != nil {
		return "", pauseCommitError(t, path, err, "the index untouched")
	}
	if !dirty {
		_, _ = fmt.Fprintf(out, "Nothing to commit in %s\n", sanitize.Line(path, 300))
		return "no commit", nil
	}
	tree, err := gitops.Output(ctx, gitops.GitLimit, path, "write-tree")
	if err == nil && !lifecycleObjectID.MatchString(tree) {
		err = fmt.Errorf("git write-tree printed %q", sanitize.Line(tree, 80))
	}
	if err != nil {
		return "", pauseCommitError(t, path, err, "the index untouched")
	}
	msg := "curia: WIP " + t.ID + " (paused)"
	if err = gitops.CommitAll(ctx, path, msg, noVerify); err != nil {
		index := "the index is as it was"
		if _, rerr := gitops.Output(ctx, gitops.GitLimit, path, "read-tree", tree); rerr != nil {
			index = fmt.Sprintf("the index could not be put back (%v): everything is staged; git -C %s read-tree %s restores it",
				rerr, sanitize.Line(path, 300), tree)
		}
		return "", pauseCommitError(t, path, err, index)
	}
	sha, err := gitops.Output(ctx, gitops.GitLimit, path, "rev-parse", "--short", "HEAD")
	if err != nil {
		sha = "(unknown)"
	}
	hooks := ""
	if noVerify {
		hooks = " (the repository's hooks skipped: --no-verify)"
	}
	_, _ = fmt.Fprintf(out, "Committed WIP %s %q on %s%s\n", sanitize.Line(sha, 64), msg, t.Branch, hooks)
	return "the WIP commit " + sanitize.Line(sha, 64) + " on " + t.Branch, nil
}

// pauseCommitError is a failed WIP commit: nothing after it ran.
func pauseCommitError(t model.Task, path string, err error, index string) error {
	return fmt.Errorf("%s not paused: the WIP commit failed: %w. Left: the agent's panes closed, the worktree %s with its files, %s. "+
		"To finish: fix what the repository's hook reports and run curia pause %s again, or skip its hooks with curia pause %s --no-verify",
		t.ID, err, sanitize.Line(path, 300), index, t.ID, t.ID)
}

// pauseKeepWorktree reports whether the pause keeps the worktree at path in
// place (Ruling 111): when it holds ignored files, which git worktree remove
// deletes without a word, or when Curia cannot tell. It says which.
func pauseKeepWorktree(ctx context.Context, path string, out io.Writer) bool {
	n, names, err := lifecycleIgnored(ctx, path)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(out, "Kept worktree %s: Curia could not look for ignored files in it (%s), so it does not remove it\n",
			sanitize.Line(path, 300), sanitize.Line(err.Error(), 200))
		return true
	case n > 0:
		_, _ = fmt.Fprintf(out, "Kept worktree %s: it holds %s (%s), which git worktree remove would delete without a word\n",
			sanitize.Line(path, 300), lifecycleCount(n, "ignored entry", "ignored entries"), strings.Join(names, ", "))
		return true
	}
	_, _ = fmt.Fprintln(out, "No ignored files in the worktree")
	return false
}

// pauseRemove removes the worktree with git worktree remove, never forced,
// after checking once more that it is the task's own.
func pauseRemove(ctx context.Context, t model.Task, path, left string, out io.Writer) error {
	ours, err := gitops.WorktreeIsOurs(ctx, t.Repo, path, t.Branch)
	if err == nil && !ours {
		err = fmt.Errorf("%s is no longer %s's worktree on %s; Curia leaves it alone", sanitize.Line(path, 300), t.ID, t.Branch)
	}
	if err == nil {
		err = gitops.RemoveWorktree(ctx, t.Repo, path, false)
	}
	if err != nil {
		return fmt.Errorf("%s not paused: %w (Curia never forces the removal). %s "+
			"To finish: look at git -C %s status, commit or move away what is there, then run curia pause %s again",
			t.ID, err, left, sanitize.Line(path, 300), t.ID)
	}
	_, _ = fmt.Fprintf(out, "Removed worktree %s (branch %s kept)\n", sanitize.Line(path, 300), t.Branch)
	return nil
}

// pauseRecord stores the pause: paused_at, no pane, the resume session,
// and no launch claim (nothing is launching; resume takes a new one). kept
// says the worktree stayed on disk, which resume then reuses.
func pauseRecord(ctx context.Context, st *store.Store, t model.Task, resume string, kept bool, out io.Writer) error {
	now := Clock()
	_, err := st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		cur.PausedAt, cur.Pane, cur.PaneSocket, cur.ResumeSession, cur.LaunchedAt, cur.UpdatedAt = now, "", "", resume, time.Time{}, now
		return cur, nil
	})
	if err != nil {
		where := "the worktree is removed"
		if kept {
			where = "the worktree is kept"
		}
		return fmt.Errorf("%s: %s and the branch %s kept, but recording the pause failed: %w. "+
			"To finish: curia spawn %s starts an agent on the branch again (a new conversation)", t.ID, where, t.Branch, err, t.ID)
	}
	how := "no session to resume: the agent will start from the brief"
	if resume != "" {
		how = "it continues session " + SpawnShortID(resume)
	}
	if kept {
		how += "; the worktree stays where it is and resume reuses it"
	}
	_, _ = fmt.Fprintf(out, "Paused %s: curia resume %s brings it back (%s)\n", t.ID, t.ID, how)
	return nil
}

// lifecycleObjectID is a git object id (SHA-1 or SHA-256).
var lifecycleObjectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// lifecyclePaneID is a tmux pane id, the only target pause closes.
var lifecyclePaneID = regexp.MustCompile(`^%[0-9]+$`)

// lifecycleIgnoredShown is how many ignored entries a message names.
const lifecycleIgnoredShown = 5

// lifecycleTarget is a pane to close: its id, the socket of the tmux server
// it was recorded on ("" when unknown), the time the agent in it is known to
// have been there since, and whose it is.
type lifecycleTarget struct {
	pane, socket string
	window       bool // the task's own window pane, whose server is unknown when socket is empty
	since        time.Time
	what         string
}

// lifecycleTargetState is lifecyclePaneStateOf for tg, except that a task's
// window pane with no recorded server (a task from before the column) is
// never ours: unknown is not "the same server".
func lifecycleTargetState(panes []tmux.Pane, perr error, tg lifecycleTarget) lifecyclePaneState {
	st := lifecyclePaneStateOf(panes, perr, tg.pane, tg.socket, tg.since)
	if st == lifecyclePaneOurs && tg.window && tg.socket == "" {
		return lifecyclePaneUnknown
	}
	return st
}

// lifecycleTaskPaneState places t's window pane (lifecycleTargetState).
func lifecycleTaskPaneState(panes []tmux.Pane, perr error, t model.Task) lifecyclePaneState {
	return lifecycleTargetState(panes, perr, lifecycleTarget{pane: t.Pane, socket: t.PaneSocket, window: true, since: t.LaunchedAt})
}

// lifecyclePaneState is what tmux shows of a pane an agent was in.
type lifecyclePaneState int

const (
	lifecyclePaneGone    lifecyclePaneState = iota // not listed, or the id now names another pane
	lifecyclePaneOurs                              // listed, on the server the agent started on
	lifecyclePaneUnknown                           // listed (or tmux cannot list), and nothing proves it is the agent's
)

// lifecyclePaneStateOf places pane, recorded on the tmux server at socket ("" when unknown), where an agent
// has been since since. tmux never reuses a pane id within one server, and a restarted server starts from %0
// again, so a pane listed by a server that started no later than since is the agent's; one listed by a server
// started after since is another pane. With no since, a listed pane is unknown; and a pane recorded on another
// server than the listed one (fleet.OtherSocket) is never "ours": the id names another pane here, while the
// agent's may still be open there, so it is unknown.
func lifecyclePaneStateOf(panes []tmux.Pane, perr error, pane, socket string, since time.Time) lifecyclePaneState {
	if pane == "" || perr != nil {
		return lifecyclePaneUnknown
	}
	i := slices.IndexFunc(panes, func(p tmux.Pane) bool { return p.ID == pane })
	switch {
	case lifecycleOtherServer(panes, socket):
		return lifecyclePaneUnknown
	case i < 0:
		return lifecyclePaneGone
	case since.IsZero() || !lifecyclePaneID.MatchString(panes[i].ID):
		return lifecyclePaneUnknown
	case panes[i].ServerStart.After(since):
		return lifecyclePaneGone
	}
	return lifecyclePaneOurs
}

// lifecycleOtherServer reports whether socket is known and is not the
// socket of the server that listed panes (fleet.OtherSocket).
func lifecycleOtherServer(panes []tmux.Pane, socket string) bool {
	return fleet.OtherSocket(fleet.Socket(panes), model.Session{Socket: socket})
}

// lifecycleListPanes lists tmux's panes within SubprocessTimeout.
func lifecycleListPanes(ctx context.Context, tm tmux.Client) ([]tmux.Pane, error) {
	tctx, cancel := context.WithTimeout(ctx, SubprocessTimeout)
	defer cancel()
	return tm.ListPanes(tctx)
}

// lifecycleLoad returns task id, or an error when there is none.
func lifecycleLoad(ctx context.Context, st *store.Store, id string) (model.Task, error) {
	t, found, err := st.GetTask(ctx, id)
	if err == nil && !found {
		err = fmt.Errorf("no task %s", id)
	}
	return t, err
}

// Worktree checks the task's branch, base and worktree before any
// git call that changes something: the branch and base are valid names and
// differ, the recorded worktree is where Curia puts it (a direct child of
// .curia-worktrees next to the repository), the repository is a main
// worktree's top level, and whatever is at the path is the task's own
// worktree on its branch (not moved, replaced or a symlink). It returns the
// path and whether the worktree is on disk.
func Worktree(ctx context.Context, t model.Task) (string, bool, error) {
	if err := errors.Join(gitops.CheckBranch("branch", t.Branch), gitops.CheckBranch("base", t.BaseBranch)); err != nil {
		return "", false, err
	}
	if t.Branch == t.BaseBranch {
		return "", false, fmt.Errorf("%s's branch %s is its base branch: Curia never touches it", t.ID, t.Branch)
	}
	path, err := SpawnWorktreePath(t.Repo, t.ID)
	if err != nil {
		return "", false, err
	}
	if t.Worktree != "" && t.Worktree != path {
		return "", false, fmt.Errorf("%s's recorded worktree %s is not where Curia puts it (%s): Curia leaves it alone",
			t.ID, sanitize.Line(t.Worktree, 300), sanitize.Line(path, 300))
	}
	if err = gitops.CheckRepo(ctx, t.Repo); err != nil {
		return "", false, err
	}
	if _, err = os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return path, false, nil
	} else if err != nil {
		return "", false, err
	}
	ours, err := gitops.WorktreeIsOurs(ctx, t.Repo, path, t.Branch)
	if err != nil {
		return "", false, err
	}
	if !ours {
		return "", false, fmt.Errorf("%s is not %s's worktree on %s (moved, replaced or a symlink): Curia leaves it alone",
			sanitize.Line(path, 300), t.ID, t.Branch)
	}
	return path, true, nil
}

// lifecycleLinked returns every session linked to task id, ended and stale
// ones included.
func lifecycleLinked(ctx context.Context, st *store.Store, id string) ([]model.Session, error) {
	all, err := st.ListSessions(ctx, store.ListFilter{IncludeEnded: true})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(s model.Session) bool { return s.TaskID != id }), nil
}

// lifecycleLive keeps the sessions that are neither ended nor stale.
func lifecycleLive(linked []model.Session) []model.Session {
	return slices.DeleteFunc(slices.Clone(linked), func(s model.Session) bool {
		return s.State == model.StateEnded || s.State == model.StateStale
	})
}

// lifecycleLaunching refuses while a spawn of the task may be under way:
// its launch claim is fresh, no linked session started since the claim and
// no window of it is open, so its setup script may be running in the
// worktree.
func lifecycleLaunching(t model.Task, linked []model.Session, panes []tmux.Pane, perr error) error {
	hold := SpawnSetupLimit + time.Minute
	if t.LaunchedAt.IsZero() || !t.LaunchedAt.After(Clock().Add(-hold)) {
		return nil
	}
	if slices.ContainsFunc(linked, func(s model.Session) bool { return !s.StartedAt.Before(t.LaunchedAt) }) {
		return nil
	}
	if lifecyclePaneStateOf(panes, perr, t.Pane, t.PaneSocket, t.LaunchedAt) == lifecyclePaneOurs {
		return nil
	}
	return fmt.Errorf("a spawn of %s started at %s may still be under way (no agent has reported and no window of it is open). "+
		"Nothing was done: wait for it, or retry after %s", t.ID,
		t.LaunchedAt.Local().Format(time.TimeOnly), t.LaunchedAt.Add(hold).Local().Format(time.TimeOnly))
}

// lifecycleIgnored counts the ignored entries in the worktree at path (an
// ignored directory counts once) and names the first few, cleaned.
func lifecycleIgnored(ctx context.Context, path string) (int, []string, error) {
	if err := gitops.CheckPath(path); err != nil {
		return 0, nil, err
	}
	out, err := gitops.Output(ctx, gitops.GitLimit, path, "status", "--porcelain", "--ignored=traditional", "--untracked-files=normal")
	if err != nil {
		return 0, nil, err
	}
	n := 0
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		name, ok := strings.CutPrefix(line, "!! ")
		if !ok {
			continue
		}
		n++
		if len(names) < lifecycleIgnoredShown {
			names = append(names, sanitize.Line(name, 100))
		}
	}
	if n > len(names) {
		names = append(names, fmt.Sprintf("and %d more", n-len(names)))
	}
	return n, names, nil
}

// lifecycleCount is "1 <one>" or "<n> <many>".
func lifecycleCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

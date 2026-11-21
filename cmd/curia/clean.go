package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
)

// cleanForce is curia clean's --force.
var cleanForce bool

// cleanLostShown is how many lost commits clean names.
const cleanLostShown = 50

func init() {
	cmd := &cobra.Command{
		Use:         "clean <id>",
		Short:       "Remove a finished task's worktree and branch (the user only)",
		Annotations: userOnlyMark(),
		Long: "Removes the task's worktree (git worktree remove, never forced: a worktree\n" +
			"with changes is left for you) and deletes its branch with git branch -d.\n" +
			"The branch must be merged into the task's base branch; a squash or rebase\n" +
			"merge looks unmerged to git, and then --force deletes it with -D after\n" +
			"naming every commit that is in neither the base nor any remote. A worktree\n" +
			"holding ignored files (dependencies, .env) is removed with them only under\n" +
			"--force, after they are named. The task is marked dropped, so the board\n" +
			"no longer offers it (curia task edit <id> --status ready brings it back).\n" +
			"Refuses while an agent of the task runs, and inside an agent session.",
		Args: cobra.ExactArgs(1),
		RunE: runClean,
	}
	cmd.Flags().BoolVar(&cleanForce, "force", false,
		"delete the branch even when git does not see it merged, and the worktree's ignored files (names what is lost)")
	rootCmd.AddCommand(cmd)
}

func runClean(cmd *cobra.Command, args []string) error {
	// No agent may delete a task's work (spec section 7).
	if err := userOnly("clean", "clean a task"); err != nil {
		return err
	}
	force := cleanForce
	return lifecycleRunCmd(cmd, args[0], func(ctx context.Context, st *store.Store, cfg config.Config, out io.Writer) error {
		return cleanTask(ctx, st, newTmuxClient(), cfg, args[0], force, out)
	})
}

// cleanTask is section 4.7's clean, in its order: the checks, the worktree
// (when on disk), the branch (when it exists), then the record. Nothing is
// removed before every check has passed.
func cleanTask(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, force bool, out io.Writer) error {
	t, path, onDisk, err := cleanCheck(ctx, st, tm, cfg, id)
	if err != nil {
		return err
	}
	exists, err := refExists(ctx, t.Repo, "refs/heads/"+t.Branch)
	if err != nil {
		return err
	}
	if onDisk {
		if err = cleanCheckIgnored(ctx, t, path, force); err != nil {
			return err
		}
	}
	if exists {
		if err = cleanCheckMerged(ctx, t, force, out); err != nil {
			return err
		}
	}
	if onDisk {
		if err = cleanRemoveWorktree(ctx, t, path, out); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(out, "No worktree on disk at %s\n", sanitize.Line(path, 300))
	}
	if exists {
		if err = cleanDeleteBranch(ctx, t, force, out); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(out, "Branch %s is already gone\n", t.Branch)
	}
	return cleanRecord(ctx, st, t, out)
}

// cleanCheck is clean's first rule: the task has a branch, nothing but its
// own worktree is at its path, no spawn is under way, no linked session is
// live and its recorded window is not open (or tmux cannot tell).
func cleanCheck(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string) (model.Task, string, bool, error) {
	t, err := lifecycleLoad(ctx, st, id)
	if err != nil {
		return t, "", false, err
	}
	if t.Branch == "" {
		return t, "", false, fmt.Errorf("%s has no branch: nothing to clean (never spawned, or cleaned already)", id)
	}
	path, onDisk, err := lifecycleWorktree(ctx, t)
	if err != nil {
		return t, "", false, err
	}
	if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
		return t, "", false, err
	}
	linked, err := lifecycleLinked(ctx, st, id)
	if err != nil {
		return t, "", false, err
	}
	panes, perr := lifecycleListPanes(ctx, tm)
	if err = lifecycleLaunching(t, linked, panes, perr); err != nil {
		return t, "", false, err
	}
	if live := lifecycleLive(linked); len(live) > 0 {
		return t, "", false, fmt.Errorf("%s has a live session %s in %s: end it first (or curia pause %s). Nothing was removed",
			id, spawnShortID(live[0].ID), spawnWhere(panes, live[0].Pane), id)
	}
	if t.Pane != "" && lifecycleTaskPaneState(panes, perr, t) != lifecyclePaneGone {
		return t, "", false, fmt.Errorf("%s's window %s is still open (or tmux cannot tell): close it first. Nothing was removed",
			id, spawnWhere(panes, t.Pane))
	}
	return t, path, onDisk, nil
}

// cleanCheckIgnored refuses, unless force, to remove a worktree that holds
// ignored files, which git worktree remove deletes without a word; it names
// them. Under force cleanRemoveWorktree names them again as it removes them.
func cleanCheckIgnored(ctx context.Context, t model.Task, path string, force bool) error {
	n, names, err := lifecycleIgnored(ctx, path)
	if err != nil || n == 0 || force {
		return err
	}
	return fmt.Errorf("%s not cleaned: its worktree %s holds %s (%s), which removing it deletes. Nothing was removed. "+
		"Move what you want to keep, then run curia clean %s again, or run curia clean %s --force to delete them with it "+
		"(--force also deletes the branch when git does not see it merged, naming the commits that would be lost)",
		t.ID, sanitize.Line(path, 300), lifecycleCount(n, "ignored entry", "ignored entries"), strings.Join(names, ", "), t.ID, t.ID)
}

// cleanCheckMerged refuses an unmerged branch unless force, naming the
// commits that deleting it would lose: those in neither the base nor any
// remote. With force it says it goes on and names them again.
func cleanCheckMerged(ctx context.Context, t model.Task, force bool, out io.Writer) error {
	merged, err := branchMerged(ctx, t.Repo, t.Branch, t.BaseBranch)
	if err != nil {
		return err
	}
	if merged {
		_, _ = fmt.Fprintf(out, "Branch %s is merged into %s\n", t.Branch, t.BaseBranch)
		return nil
	}
	n, lost, err := cleanLost(ctx, t)
	if err != nil {
		return err
	}
	if !force {
		loses := "Every commit on it is in " + t.BaseBranch + " or on a remote, so --force would lose none."
		if n > 0 {
			loses = fmt.Sprintf("With --force, %s in neither %s nor any remote would be lost: %s.",
				lifecycleCount(n, "commit", "commits"), t.BaseBranch, strings.Join(lost, "; "))
		}
		return fmt.Errorf("%s's branch %s is not merged into %s (git merge-base --is-ancestor). Nothing was removed. "+
			"A squash or rebase merge looks unmerged to git; if you merged it that way, run curia clean %s --force. %s",
			t.ID, t.Branch, t.BaseBranch, t.ID, loses)
	}
	_, _ = fmt.Fprintf(out, "Branch %s is not merged into %s; deleting it anyway (--force)\n", t.Branch, t.BaseBranch)
	if n > 0 {
		_, _ = fmt.Fprintf(out, "Commits lost (in neither %s nor any remote), %d:\n", t.BaseBranch, n)
		for _, l := range lost {
			_, _ = fmt.Fprintf(out, "  %s\n", l)
		}
	}
	return nil
}

// cleanLost counts the commits on the task's branch that are in neither the
// base nor any remote-tracking branch, and names up to cleanLostShown of
// them (short id and cleaned subject), newest first.
func cleanLost(ctx context.Context, t model.Task) (int, []string, error) {
	rng := []string{"refs/heads/" + t.Branch, "--not", t.BaseBranch, "--remotes", "--"}
	count, err := worktreeRun(ctx, worktreeGitLimit, t.Repo, append([]string{"rev-list", "--count"}, rng...)...)
	if err != nil {
		return 0, nil, err
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return 0, nil, fmt.Errorf("git rev-list --count printed %q", sanitize.Line(count, 40))
	}
	if n == 0 {
		return 0, nil, nil
	}
	log, err := worktreeRun(ctx, worktreeGitLimit, t.Repo,
		append([]string{"log", "--no-show-signature", "--format=%h %s", "--max-count=" + strconv.Itoa(cleanLostShown)}, rng...)...)
	if err != nil {
		return 0, nil, err
	}
	var lost []string
	for line := range strings.SplitSeq(log, "\n") {
		lost = append(lost, sanitize.Line(line, 120))
	}
	if n > len(lost) {
		lost = append(lost, fmt.Sprintf("and %d more (git log %s..%s)", n-len(lost), t.BaseBranch, t.Branch))
	}
	return n, lost, nil
}

// cleanRemoveWorktree removes the worktree with git worktree remove, never
// forced, after checking once more that it is the task's own. The ignored
// entries git deletes with it are named.
func cleanRemoveWorktree(ctx context.Context, t model.Task, path string, out io.Writer) error {
	n, names, err := lifecycleIgnored(ctx, path)
	if err == nil {
		var ours bool
		ours, err = worktreeIsOurs(ctx, t.Repo, path, t.Branch)
		if err == nil && !ours {
			err = fmt.Errorf("%s is no longer %s's worktree on %s; Curia leaves it alone", sanitize.Line(path, 300), t.ID, t.Branch)
		}
	}
	if err == nil {
		err = worktreeRemove(ctx, t.Repo, path, false)
	}
	if err != nil {
		return fmt.Errorf("%s not cleaned: %w (Curia never forces the removal). Nothing was removed: the worktree %s and the branch %s stay. "+
			"To finish: commit, stash or delete what git -C %s status shows, then run curia clean %s again",
			t.ID, err, sanitize.Line(path, 300), t.Branch, sanitize.Line(path, 300), t.ID)
	}
	with := ""
	if n > 0 {
		with = fmt.Sprintf(", with its %s (%s)", lifecycleCount(n, "ignored entry", "ignored entries"), strings.Join(names, ", "))
	}
	_, _ = fmt.Fprintf(out, "Removed worktree %s%s\n", sanitize.Line(path, 300), with)
	return nil
}

// cleanDeleteBranch deletes the branch: git branch -d, or -D under force.
func cleanDeleteBranch(ctx context.Context, t model.Task, force bool, out io.Writer) error {
	sha, err := worktreeRun(ctx, worktreeGitLimit, t.Repo, "rev-parse", "--short", "refs/heads/"+t.Branch)
	if err == nil {
		err = branchDelete(ctx, t.Repo, t.Branch, force)
	}
	if err != nil {
		return fmt.Errorf("%s not fully cleaned: %w. Left: the worktree gone, the branch %s kept. To finish: "+
			"if git says a worktree that is gone still uses the branch, run git -C %s worktree prune; "+
			"if it says the branch is not fully merged, use --force; then run curia clean %s again",
			t.ID, err, t.Branch, sanitize.Line(t.Repo, 300), t.ID)
	}
	_, _ = fmt.Fprintf(out, "Deleted branch %s (was %s)\n", t.Branch, sanitize.Line(sha, 64))
	return nil
}

// cleanRecord clears the task's branch, worktree, pane, pause and launch
// claim, sets cleaned_at and marks the task dropped (live check B, defect
// 7): its work is merged or deleted, and dropped is the one stored status
// the board neither shows nor spawns.
func cleanRecord(ctx context.Context, st *store.Store, t model.Task, out io.Writer) error {
	now := clock()
	rs, err := st.ListReviews(ctx, store.ReviewFilter{TaskID: t.ID, Limit: 1})
	if err != nil {
		return err
	}
	done := len(rs) > 0 && task.Derive(t, nil, 0, rs[0].Status) == model.TaskDone
	_, err = st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		cur.Branch, cur.Worktree, cur.Pane, cur.PaneSocket, cur.ResumeSession = "", "", "", "", ""
		cur.PausedAt, cur.LaunchedAt, cur.CleanedAt, cur.UpdatedAt = time.Time{}, time.Time{}, now, now
		cur.Status = model.TaskDropped // off the board: nothing spawns it again by accident
		return cur, nil
	})
	if err != nil {
		return fmt.Errorf("%s: the worktree and branch are gone, but recording it failed: %w. To finish: run curia clean %s again",
			t.ID, err, t.ID)
	}
	if done {
		_, _ = fmt.Fprintf(out, "Cleaned %s (done: its worktree and branch are removed, and it leaves the board; curia task edit %s --status ready brings it back)\n",
			t.ID, t.ID)
		return nil
	}
	_, _ = fmt.Fprintf(out, "Cleaned %s (marked dropped, so the board no longer offers it; curia task edit %s --status ready brings it back)\n",
		t.ID, t.ID)
	return nil
}

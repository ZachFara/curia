package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:         "resume <id>",
		Short:       "Bring a paused task's worktree and agent back (the user only)",
		Annotations: userOnlyMark(),
		Long: "Adds the paused task's worktree again on its branch (with its WIP commit),\n" +
			"runs .curia/setup.sh as curia spawn does, and opens the task's window with\n" +
			"claude --resume on the session the pause recorded (or the task's brief when\n" +
			"there is none), with the task's model and team setting. Waits up to 20 s\n" +
			"for the agent to report. Refuses to run inside an agent session.",
		Args: cobra.ExactArgs(1),
		RunE: runResume,
	})
}

func runResume(cmd *cobra.Command, args []string) error {
	// No agent may spawn or resume anything (spec section 7).
	if err := userOnly("resume", "resume a task"); err != nil {
		return err
	}
	return lifecycleRunCmd(cmd, args[0], func(ctx context.Context, st *store.Store, cfg config.Config, out io.Writer) error {
		return resumeTask(ctx, st, newTmuxClient(), cfg, args[0], out)
	})
}

// resumeTask is section 4.7's resume. The worktree, setup script, window and
// wait are spawnTask's, which adds the worktree on the existing branch.
// paused_at is cleared before spawnTask (it refuses a paused task without a
// session to resume) and put back when it fails, so a failed resume leaves
// the task paused and a retry reuses whatever worktree it made.
func resumeTask(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, out io.Writer) error {
	t, err := resumeCheck(ctx, st, id)
	if err != nil {
		return err
	}
	if t.ResumeSession != "" {
		_, _ = fmt.Fprintf(out, "Resume: session %s on branch %s\n", spawnShortID(t.ResumeSession), t.Branch)
	} else {
		_, _ = fmt.Fprintf(out, "Resume: no session recorded; the agent starts from the brief, on branch %s\n", t.Branch)
	}
	if _, err = st.UpdateTask(ctx, id, func(cur model.Task) (model.Task, error) {
		cur.PausedAt, cur.UpdatedAt = time.Time{}, clock()
		return cur, nil
	}); err != nil {
		return err
	}
	r, err := spawnTask(ctx, st, tm, cfg, id, spawnOptions{Team: t.Team, Resume: t.ResumeSession, Wait: spawnWait, Out: out})
	if err != nil {
		return resumeRepause(ctx, st, t, err)
	}
	_, err = fmt.Fprintln(out, spawnLine(r))
	return err
}

// resumeCheck is resume's first rule: the task is paused, its branch still
// exists, and nothing but its own worktree is at its worktree path.
func resumeCheck(ctx context.Context, st *store.Store, id string) (model.Task, error) {
	t, err := lifecycleLoad(ctx, st, id)
	switch {
	case err != nil:
		return t, err
	case t.PausedAt.IsZero():
		return t, fmt.Errorf("%s is not paused: nothing to resume", id)
	case t.Branch == "":
		return t, fmt.Errorf("%s has no branch: nothing to resume", id)
	}
	if _, _, err = lifecycleWorktree(ctx, t); err != nil {
		return t, err
	}
	ok, err := refExists(ctx, t.Repo, "refs/heads/"+t.Branch)
	if err != nil {
		return t, err
	}
	if !ok {
		return t, fmt.Errorf("%s's branch %s is gone, so there is nothing to resume: "+
			"curia clean %s, then curia spawn %s starts over from %s", id, t.Branch, id, id, t.BaseBranch)
	}
	return t, nil
}

// resumeRepause marks the task paused again after spawnTask failed with err.
func resumeRepause(ctx context.Context, st *store.Store, t model.Task, err error) error {
	_, uerr := st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		if cur.PausedAt.IsZero() {
			cur.PausedAt, cur.UpdatedAt = t.PausedAt, clock()
		}
		return cur, nil
	})
	if uerr != nil {
		return fmt.Errorf("%w (and %s could not be marked paused again: %w; it is not paused now: curia spawn %s starts it from the brief)",
			err, t.ID, uerr, t.ID)
	}
	return fmt.Errorf("%w. %s is paused again: curia resume %s retries", err, t.ID, t.ID)
}

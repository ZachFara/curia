package lifecycle

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// ResumeTask is section 4.7's resume. The worktree, setup script, window and
// wait are SpawnTask's, which adds the worktree on the existing branch.
// paused_at is cleared before SpawnTask (it refuses a paused task without a
// session to resume) and put back when it fails, so a failed resume leaves
// the task paused and a retry reuses whatever worktree it made.
func ResumeTask(ctx context.Context, st *store.Store, tm tmux.Client, cfg config.Config, id string, out io.Writer) error {
	t, err := resumeCheck(ctx, st, id)
	if err != nil {
		return err
	}
	if t.ResumeSession != "" {
		_, _ = fmt.Fprintf(out, "Resume: session %s on branch %s\n", SpawnShortID(t.ResumeSession), t.Branch)
	} else {
		_, _ = fmt.Fprintf(out, "Resume: no session recorded; the agent starts from the brief, on branch %s\n", t.Branch)
	}
	if _, err = st.UpdateTask(ctx, id, func(cur model.Task) (model.Task, error) {
		cur.PausedAt, cur.UpdatedAt = time.Time{}, Clock()
		return cur, nil
	}); err != nil {
		return err
	}
	o := SpawnOptions{Team: t.Team, Resume: t.ResumeSession, Wait: SpawnWait, Out: out, AfterPause: resumeAfterWIP(ctx, t)}
	r, err := SpawnTask(ctx, st, tm, cfg, id, o)
	if err != nil {
		return resumeRepause(ctx, st, t, err)
	}
	_, err = fmt.Fprintln(out, SpawnLine(r))
	return err
}

// resumeAfterWIP reports whether t's branch ends in the WIP commit its
// pause made, which the resumed agent is then told about.
func resumeAfterWIP(ctx context.Context, t model.Task) bool {
	subject, err := gitops.Output(ctx, gitops.GitLimit, t.Repo, "log", "-1", "--no-show-signature", "--format=%s", "refs/heads/"+t.Branch, "--")
	return err == nil && subject == "curia: WIP "+t.ID+" (paused)"
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
	if _, _, err = Worktree(ctx, t); err != nil {
		return t, err
	}
	ok, err := gitops.RefExists(ctx, t.Repo, "refs/heads/"+t.Branch)
	if err != nil {
		return t, err
	}
	if !ok {
		return t, fmt.Errorf("%s's branch %s is gone, so there is nothing to resume: "+
			"curia clean %s, then curia spawn %s starts over from %s", id, t.Branch, id, id, t.BaseBranch)
	}
	return t, nil
}

// resumeRepause marks the task paused again after SpawnTask failed with err.
func resumeRepause(ctx context.Context, st *store.Store, t model.Task, err error) error {
	_, uerr := st.UpdateTask(ctx, t.ID, func(cur model.Task) (model.Task, error) {
		if cur.PausedAt.IsZero() {
			cur.PausedAt, cur.UpdatedAt = t.PausedAt, Clock()
		}
		return cur, nil
	})
	if uerr != nil {
		return fmt.Errorf("%w (and %s could not be marked paused again: %w; it is not paused now: curia spawn %s starts it from the brief)",
			err, t.ID, uerr, t.ID)
	}
	return fmt.Errorf("%w. %s is paused again: curia resume %s retries", err, t.ID, t.ID)
}

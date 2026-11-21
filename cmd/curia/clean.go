package main

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/store"
)

// cleanForce is curia clean's --force.
var cleanForce bool

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
		return lifecycle.CleanTask(ctx, st, newTmuxClient(), cfg, args[0], force, out)
	})
}

package main

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/store"
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
		return lifecycle.ResumeTask(ctx, st, newTmuxClient(), cfg, args[0], out)
	})
}

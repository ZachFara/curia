package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// The flags of curia pause.
var (
	pauseForce    bool
	pauseNoVerify bool
)

func init() {
	cmd := &cobra.Command{
		Use:         "pause <id>",
		Short:       "Stop a task's agent, commit its work and remove its worktree (the user only)",
		Annotations: userOnlyMark(),
		Long: "Closes the tmux panes of the task's agent sessions and its window, commits\n" +
			"everything in its worktree (\"curia: WIP <id> (paused)\", through the\n" +
			"repository's hooks unless --no-verify), then removes the worktree with git\n" +
			"worktree remove, never forced. The branch stays, and curia resume <id>\n" +
			"brings the worktree and the agent's conversation back. A worktree holding\n" +
			"ignored files (dependencies, .env) is kept where it is, because git would\n" +
			"delete them without a word; the task is paused all the same and resume\n" +
			"reuses it. Refuses while an agent is working or waiting, unless --force.\n" +
			"Refuses to run inside an agent session.",
		Args: cobra.ExactArgs(1),
		RunE: runPause,
	}
	cmd.Flags().BoolVar(&pauseForce, "force", false, "pause even when an agent of the task is working or waiting")
	cmd.Flags().BoolVar(&pauseNoVerify, "no-verify", false, "skip the repository's commit hooks for the WIP commit")
	rootCmd.AddCommand(cmd)
}

func runPause(cmd *cobra.Command, args []string) error {
	// No agent may pause anything (spec section 7).
	if err := userOnly("pause", "pause a task"); err != nil {
		return err
	}
	o := lifecycle.PauseOptions{Force: pauseForce, NoVerify: pauseNoVerify}
	return lifecycleRunCmd(cmd, args[0], func(ctx context.Context, st *store.Store, cfg config.Config, out io.Writer) error {
		return lifecycle.PauseTask(ctx, st, newTmuxClient(), cfg, args[0], o, out)
	})
}

// lifecycleRunCmd checks id, opens the store and runs fn with the
// command's output: the wiring of pause, resume and clean.
func lifecycleRunCmd(cmd *cobra.Command, id string, fn func(context.Context, *store.Store, config.Config, io.Writer) error) error {
	if !task.ValidID(id) {
		return fmt.Errorf("%q is not a task id (T-<n>)", sanitize.Line(id, 40))
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
	return fn(cmd.Context(), st, cfg, cmd.OutOrStdout())
}

package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/lifecycle"
)

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
	r, err := lifecycle.SpawnTask(cmd.Context(), st, newTmuxClient(), cfg, args[0],
		lifecycle.SpawnOptions{Agent: spawnAgent, Model: spawnFlagModel, Team: spawnTeam, Wait: lifecycle.SpawnWait, Out: out, ReadyOnly: !userAtTerminal()})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, lifecycle.SpawnLine(r))
	return err
}

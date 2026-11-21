package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// launchExec replaces this process with the agent; tests replace it.
var launchExec = syscall.Exec

// The flags of curia launch.
var (
	launchModel      string
	launchTeam       bool
	launchResume     string
	launchAfterPause bool
)

// launchFailMax bounds the reason shown in the window.
const launchFailMax = 300

func init() {
	cmd := &cobra.Command{
		Use:    "launch <id>",
		Short:  "Start a task's agent, claude or codex (run by curia spawn inside the task's window)",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		// launch prints its own one line: nothing else is visible in the window.
		SilenceErrors: true,
		RunE:          runLaunch,
	}
	cmd.Flags().StringVar(&launchModel, "model", "", "the model the agent uses")
	cmd.Flags().BoolVar(&launchTeam, "team", false, "let the agent lead an agent team")
	cmd.Flags().StringVar(&launchResume, "resume", "", "resume this agent session instead of starting with the brief")
	cmd.Flags().BoolVar(&launchAfterPause, "after-pause", false, "tell the agent its work is in the pause's WIP commit")
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error { return launchFail(c, launchConfig(), err) })
	rootCmd.AddCommand(cmd)
}

// launchConfig is the configuration, or the defaults when it cannot be
// loaded, so a failure still reaches the default log file.
func launchConfig() config.Config {
	cfg, err := loadConfig()
	if err != nil {
		return config.Default()
	}
	return cfg
}

// runLaunch replaces this process with the task's agent (claude or codex).
// It returns only on failure, after launchFail.
func runLaunch(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err == nil {
		err = launchAgent(cmd.Context(), cfg, args)
	} else {
		cfg = config.Default()
	}
	if err == nil {
		return nil // only a test's launchExec returns without an error
	}
	return launchFail(cmd, cfg, err)
}

// launchFail logs err, shows it in the window as one cleaned line, waits
// for Enter so the window does not vanish with the reason, and returns err
// (exit 1).
func launchFail(cmd *cobra.Command, cfg config.Config, err error) error {
	logWarn(cfg.LogPath, "launch failed", "err", err.Error())
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "curia launch: %s (press Enter to close)\n", sanitize.Line(err.Error(), launchFailMax))
	_, _ = bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 4096)).ReadString('\n')
	return err
}

// launchCheckArgs checks the arguments and flags before the store is opened
// and returns the task id.
func launchCheckArgs(args []string) (string, error) {
	if len(args) != 1 {
		return "", fmt.Errorf("launch takes one task id, got %d arguments", len(args))
	}
	return args[0], lifecycle.SpawnCheckOptions(args[0], lifecycle.SpawnOptions{Model: launchModel, Resume: launchResume})
}

// launchAgent loads the task and execs its agent with lifecycle.LaunchArgv or
// lifecycle.LaunchCodexArgv in lifecycle.LaunchEnv. The binary is one of two fixed names chosen
// by the task's stored agent; the team switch is only ever Claude Code's.
func launchAgent(ctx context.Context, cfg config.Config, args []string) error {
	id, err := launchCheckArgs(args)
	if err != nil {
		return err
	}
	t, err := launchLoad(ctx, cfg, id)
	if err != nil {
		return err
	}
	var argv []string
	switch t.Agent {
	case "", model.AgentClaude:
		argv, err = lifecycle.LaunchArgv(t, launchModel, launchTeam, launchResume, launchAfterPause)
	case model.AgentCodex:
		argv, err = lifecycle.LaunchCodexArgv(t, launchModel, launchTeam, launchResume, launchAfterPause)
	default:
		err = fmt.Errorf("%s has agent %q, which curia launch does not start", id, sanitize.Line(t.Agent, 40))
	}
	if err != nil {
		return err
	}
	argv = lifecycle.LaunchWithRC(argv, cfg.RC.Launch, rcName(model.Session{TaskID: t.ID}, t.Title))
	path, err := lifecycle.LaunchLookPath(argv[0])
	if err != nil {
		return fmt.Errorf("%s not found: %w", argv[0], err)
	}
	if err = launchExec(path, argv, dockerLaunchEnv(cfg, lifecycle.LaunchEnv(os.Environ(), id, launchTeam && argv[0] == model.AgentClaude))); err != nil {
		return fmt.Errorf("start %s: %w", path, err)
	}
	return nil
}

// launchLoad reads task id from the store and closes the store, so nothing
// of it is open when this process becomes the agent.
func launchLoad(ctx context.Context, cfg config.Config, id string) (model.Task, error) {
	st, err := openStore(cfg)
	if err != nil {
		return model.Task{}, err
	}
	defer func() { _ = st.Close() }()
	t, found, err := st.GetTask(ctx, id)
	if err != nil {
		return model.Task{}, err
	}
	if !found {
		return model.Task{}, fmt.Errorf("no task %s", id)
	}
	return t, nil
}

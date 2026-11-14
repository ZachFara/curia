package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

var (
	// launchExec replaces this process with the agent; tests replace it.
	launchExec = syscall.Exec
	// launchLookPath finds the agent's binary; tests replace it.
	launchLookPath = exec.LookPath
)

// The flags of curia launch.
var (
	launchModel  string
	launchTeam   bool
	launchResume string
)

// launchTeamVar is the variable that turns Claude Code's agent teams on.
const launchTeamVar = "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"

// launchFailMax bounds the reason shown in the window.
const launchFailMax = 300

func init() {
	cmd := &cobra.Command{
		Use:    "launch <id>",
		Short:  "Start a task's agent (run by curia spawn inside the task's window)",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		// launch prints its own one line: nothing else is visible in the window.
		SilenceErrors: true,
		RunE:          runLaunch,
	}
	cmd.Flags().StringVar(&launchModel, "model", "", "the model the agent uses")
	cmd.Flags().BoolVar(&launchTeam, "team", false, "let the agent lead an agent team")
	cmd.Flags().StringVar(&launchResume, "resume", "", "resume this Claude Code session instead of starting with the brief")
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

// runLaunch replaces this process with claude for the task. It returns
// only on failure, after launchFail.
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
	return args[0], spawnCheckOptions(args[0], spawnOptions{Model: launchModel, Resume: launchResume})
}

// launchAgent loads the task and execs claude with launchArgv in launchEnv.
func launchAgent(ctx context.Context, cfg config.Config, args []string) error {
	id, err := launchCheckArgs(args)
	if err != nil {
		return err
	}
	t, err := launchLoad(ctx, cfg, id)
	if err != nil {
		return err
	}
	argv, err := launchArgv(t, launchModel, launchTeam, launchResume)
	if err != nil {
		return err
	}
	path, err := launchLookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found: %w", err)
	}
	if err = launchExec(path, argv, launchEnv(os.Environ(), id, launchTeam)); err != nil {
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

// launchArgv is claude's argument vector: the model, the team switch, then
// the session to resume or the brief as the one positional argument. The
// brief starts with fixed text, so claude never reads it as an option, and
// it is never seen by a shell or by tmux. No permission flag is ever added.
func launchArgv(t model.Task, modelName string, team bool, resume string) ([]string, error) {
	argv := []string{"claude"}
	if modelName != "" {
		argv = append(argv, "--model", modelName)
	}
	if team {
		argv = append(argv, "--teammate-mode", "tmux")
	}
	if resume != "" {
		return append(argv, "--resume", resume), nil
	}
	if !task.ValidBranch(t.Branch) || !task.ValidBranch(t.BaseBranch) {
		return nil, errors.New(t.ID + " has no branch and base branch yet: start it with curia spawn " + t.ID)
	}
	// Stored text is clean already; cleaning again is free (idempotent) and
	// keeps the rule that untrusted text is cleaned wherever it is sent.
	t.Title = sanitize.Line(t.Title, task.MaxTitle)
	t.Body = sanitize.Block(t.Body, task.MaxBody)
	return append(argv, task.Brief(t, team)), nil
}

// launchEnv is env with CURIA_TASK_ID set to id and, with team, the agent
// teams variable on; an inherited value of either is replaced, since exec
// does not drop duplicates.
func launchEnv(env []string, id string, team bool) []string {
	drop := []string{"CURIA_TASK_ID"}
	if team {
		drop = append(drop, launchTeamVar)
	}
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(drop, name)
	})
	env = append(env, "CURIA_TASK_ID="+id)
	if team {
		env = append(env, launchTeamVar+"=1")
	}
	return env
}

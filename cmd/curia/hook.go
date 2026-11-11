package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/hook"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

const (
	// maxPayloadBytes bounds how much of stdin one hook reads.
	maxPayloadBytes = 1 << 20
	// subprocessTimeout bounds each git or tmux call a hook makes.
	subprocessTimeout = time.Second
)

// hookExit ends the hook process; tests replace it. hookRun is hook.Run;
// tests replace it to reach the panic path.
var (
	hookExit = os.Exit
	hookRun  = hook.Run
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "hook <event>",
		Short: "Record one Claude Code hook event (run by Claude Code's hooks)",
		Long: "Reads the hook payload on stdin and $TMUX_PANE, and records the event.\n" +
			"Always exits 0 and prints nothing; errors go to the log file.",
		Args: cobra.ArbitraryArgs,
		// Every argument reaches runHook, so a bad one is logged, not printed.
		DisableFlagParsing: true,
		Run:                runHook,
	})
}

// runHook records the event and exits 0 on every path, panics included.
// Nothing is written to stdout or stderr, which belong to Claude Code.
func runHook(cmd *cobra.Command, args []string) {
	logger := slog.New(slog.DiscardHandler)
	closeLog := func() error { return nil }
	event := strings.Join(args, " ")
	defer func() {
		if r := recover(); r != nil {
			logger.Error("hook panicked", "event", event, "panic", fmt.Sprint(r))
		}
		_ = closeLog()
		hookExit(0)
	}()

	cfg, cfgErr := loadConfig()
	if cfgErr != nil {
		cfg = config.Default() // still log to the default log file
	}
	if l, c, err := openLog(cfg.LogPath); err == nil {
		logger, closeLog = l, c
	}
	err := cfgErr
	if err == nil {
		err = recordEvent(cmd.Context(), args, cmd.InOrStdin(), cfg, logger)
	}
	if err != nil {
		logger.Error("hook failed", "event", event, "err", err.Error())
	}
}

// recordEvent reads the payload and runs the hook against the store.
func recordEvent(ctx context.Context, args []string, stdin io.Reader, cfg config.Config, logger *slog.Logger) error {
	if len(args) != 1 {
		return fmt.Errorf("want one event name, got %d arguments", len(args))
	}
	payload, err := readPayload(stdin)
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	_, err = hookRun(ctx, model.EventKind(args[0]), payload, hookEnv(ctx, logger), st, cfg)
	return err
}

// readPayload reads stdin, up to maxPayloadBytes.
func readPayload(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxPayloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read payload: %w", err)
	}
	if len(b) > maxPayloadBytes {
		_, _ = io.Copy(io.Discard, r) // let Claude Code finish writing rather than hit a closed pipe
		return nil, fmt.Errorf("payload exceeds %d bytes", maxPayloadBytes)
	}
	return b, nil
}

// hookEnv is the real environment: $TMUX_PANE, the wall clock, git, tmux.
func hookEnv(ctx context.Context, logger *slog.Logger) hook.Env {
	client := tmux.NewExec("")
	return hook.Env{
		Pane: os.Getenv("TMUX_PANE"),
		Now:  time.Now,
		Git:  func(cwd string) (string, string) { return gitInfo(ctx, cwd) },
		PaneInfo: func(pane string) (string, string) {
			pctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
			defer cancel()
			p, err := client.PaneInfo(pctx, pane)
			if err != nil {
				logger.Warn("tmux pane info", "pane", pane, "err", err.Error())
				return "", ""
			}
			return p.Session, p.WindowID
		},
	}
}

// gitInfo returns the repository top level and current branch for cwd, or
// empty strings when cwd is empty or not in a repository. The branch is
// empty on a detached HEAD. The two git commands run at the same time.
func gitInfo(ctx context.Context, cwd string) (repo, branch string) {
	if cwd == "" { // git -C "" would use this process's directory
		return "", ""
	}
	ctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	waitRepo := startGit(ctx, cwd, "rev-parse", "--show-toplevel")
	waitBranch := startGit(ctx, cwd, "branch", "--show-current")
	repo, err := waitRepo()
	branch, _ = waitBranch()
	if err != nil {
		return "", ""
	}
	return repo, branch
}

// startGit starts git -C cwd args; the returned func waits for it and
// returns its trimmed stdout. Its stderr is discarded, never passed through.
func startGit(ctx context.Context, cwd string, args ...string) func() (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = gitEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return func() (string, error) { return "", err }
	}
	return func() (string, error) {
		err := cmd.Wait()
		return strings.TrimSpace(out.String()), err
	}
}

// gitRepoVars are the variables `git rev-parse --local-env-vars` lists: set
// in this process (for example because Claude Code was started from a git
// hook) they would point git at another repository than cwd's.
var gitRepoVars = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
	"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX",
	"GIT_SHALLOW_FILE", "GIT_COMMON_DIR",
}

// gitEnv is this process's environment without gitRepoVars, so git finds
// the repository from -C alone.
func gitEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitRepoVars, name)
	})
}

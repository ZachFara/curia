package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/mcp"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// Limits of `curia mcp`.
const (
	// Events older than mcpPruneAge are pruned at start, in at most
	// mcpPruneBatches batches of mcpPruneBatch.
	mcpPruneAge     = 7 * 24 * time.Hour
	mcpPruneBatch   = 1000
	mcpPruneBatches = 10
	// At most one blocking-ticket notification per mcpNotifyEvery, across
	// every process.
	mcpNotifyEvery = 30 * time.Second
	// mcpNotifyTimeout bounds the bell and the macOS notification, which
	// run inside a tool call: notify.Exec has no timeout of its own.
	mcpNotifyTimeout = 5 * time.Second
)

// Seams: mcpExit ends the process with a failure code, mcpRun runs
// osascript and mcpGOOS is the platform. Tests replace them.
var (
	mcpExit            = os.Exit
	mcpRun  notify.Run = notify.Exec
	mcpGOOS            = runtime.GOOS
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "mcp",
		Short: "Serve the decision tools over stdio (run by Claude Code)",
		Long: "A stdio MCP server, one per agent session, with four tools: search_decisions,\n" +
			"request_decision, check_decision and withdraw_decision. None of them answers a\n" +
			"ticket. Stdout carries protocol messages only; errors go to the log file.",
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			if code := mcpServe(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout()); code != 0 {
				mcpExit(code)
			}
		},
	})
}

// mcpServe runs the server on in and out until in ends, and returns the
// exit code. Nothing but protocol messages is written to out.
func mcpServe(ctx context.Context, in io.Reader, out io.Writer) int {
	cfg, cfgErr := loadConfig()
	if cfgErr != nil {
		cfg = config.Default() // still log to the default log file
	}
	logger := slog.New(slog.DiscardHandler)
	if l, closeLog, err := openLog(cfg.LogPath); err == nil {
		logger = l
		defer func() { _ = closeLog() }()
	}
	if cfgErr != nil {
		logger.Error("mcp: config", "err", cfgErr.Error())
		return 1
	}
	st, err := openStore(cfg)
	if err != nil {
		logger.Error("mcp: open store", "err", err.Error())
		return 1
	}
	defer func() { _ = st.Close() }()
	mcpPrune(ctx, st, clock(), logger)
	srv := &mcp.Server{
		Name: "curia", Version: version, Instructions: mcp.Instructions, Log: logger,
		Tools: mcp.Tools(mcp.Deps{
			Store: st, Cfg: cfg, Now: clock, Log: logger,
			Caller: mcpCaller(st, logger),
			Notify: mcpNotify(st, cfg, newTmuxClient(), logger),
		}),
	}
	if err := srv.Serve(ctx, in, out); err != nil {
		logger.Error("mcp: serve", "err", err.Error())
		return 1
	}
	return 0
}

// mcpPrune deletes events older than mcpPruneAge, a batch at a time, until
// a batch deletes fewer than a full batch or mcpPruneBatches have run.
func mcpPrune(ctx context.Context, st *store.Store, now time.Time, logger *slog.Logger) {
	for range mcpPruneBatches {
		n, err := st.PruneEvents(ctx, now.Add(-mcpPruneAge), mcpPruneBatch)
		if err != nil {
			logger.Error("mcp: prune events", "err", err.Error())
			return
		}
		if n < mcpPruneBatch {
			return
		}
	}
}

// mcpCaller resolves who is calling, on every tool call (decision 36):
// CURIA_SESSION_ID as is; else the live session bound to TMUX_PANE; else
// CLAUDE_CODE_SESSION_ID when its row is live; else unknown. Repo is the
// session's, else the git top level of CLAUDE_PROJECT_DIR (or of the working
// directory when it is unset). CLAUDE_CODE_SESSION_ID goes stale after
// /clear while this process keeps running, which is why the pane comes first.
func mcpCaller(st *store.Store, logger *slog.Logger) func(context.Context) mcp.Caller {
	return func(ctx context.Context) mcp.Caller {
		sess, id := mcpSession(ctx, st, logger)
		c := mcp.Caller{SessionID: id, Repo: sess.Repo, Pane: sess.Pane}
		if c.Repo == "" {
			c.Repo = mcpRepo(ctx, logger)
		}
		return c
	}
}

// mcpSession finds the caller's session id and its row (zero when there is
// none).
func mcpSession(ctx context.Context, st *store.Store, logger *slog.Logger) (model.Session, string) {
	if id := os.Getenv("CURIA_SESSION_ID"); id != "" {
		sess, _, err := st.GetSession(ctx, id)
		if err != nil {
			logger.Warn("mcp: caller lookup", "err", err.Error())
		}
		return sess, id
	}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		if sess, ok := mcpPaneSession(ctx, st, pane, logger); ok {
			return sess, sess.ID
		}
	}
	if id := os.Getenv("CLAUDE_CODE_SESSION_ID"); id != "" {
		sess, found, err := st.GetSession(ctx, id)
		if err != nil {
			logger.Warn("mcp: caller lookup", "err", err.Error())
		}
		if found && sess.State != model.StateEnded {
			return sess, sess.ID
		}
	}
	return model.Session{}, ""
}

// mcpPaneSession is the most recently updated session in pane that is not
// ended.
func mcpPaneSession(ctx context.Context, st *store.Store, pane string, logger *slog.Logger) (model.Session, bool) {
	live, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		logger.Warn("mcp: caller lookup", "err", err.Error())
		return model.Session{}, false
	}
	var best model.Session
	found := false
	for _, s := range live {
		if s.Pane == pane && (!found || s.UpdatedAt.After(best.UpdatedAt)) {
			best, found = s, true
		}
	}
	return best, found
}

// mcpRepo is the git top level of CLAUDE_PROJECT_DIR, or of the working
// directory when that is unset or empty, or "". The directory reaches git
// only as the argv value of -C, never a shell (decision 0002's reason for
// excluding G204). It is read with os.LookupEnv because gosec's G702 taint
// rule counts os.Getenv as a source and knows no sanitizer for argv; this
// is reported for a ruling (Task 12 report), not a fix of the rule.
func mcpRepo(ctx context.Context, logger *slog.Logger) string {
	dir, _ := os.LookupEnv("CLAUDE_PROJECT_DIR")
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		dir = wd
	}
	repo, _ := gitInfo(ctx, dir, logger)
	return repo
}

// mcpNotify alerts the user to a new blocking ticket: nothing when both
// notify flags are off; otherwise, when this process wins the shared rate
// limit slot, a bell in pane and a macOS notification, bounded by
// mcpNotifyTimeout. Errors are logged, never returned.
func mcpNotify(st *store.Store, cfg config.Config, tm tmux.Client, logger *slog.Logger) func(context.Context, model.Decision, string) {
	return func(ctx context.Context, d model.Decision, pane string) {
		if !cfg.NotifyBell && !cfg.NotifyMacOS {
			return
		}
		won, err := st.ClaimNotifySlot(ctx, clock(), mcpNotifyEvery)
		if err != nil {
			logger.Warn("mcp: claim notify slot", "err", err.Error())
			return
		}
		if !won {
			return
		}
		nctx, cancel := context.WithTimeout(ctx, mcpNotifyTimeout)
		defer cancel()
		if err := notify.Blocking(nctx, tm, mcpRun, mcpGOOS, cfg, d, pane); err != nil {
			logger.Warn("mcp: notify", "decision", d.ID, "err", err.Error())
		}
	}
}

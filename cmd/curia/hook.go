package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/attention"
	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/hook"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/state"
	"github.com/ZachFara/curia/internal/store"
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

// hookDeadline bounds a whole hook run; tests shorten it.
var hookDeadline = 5 * time.Second

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "hook <event>",
		Short: "Record one Claude Code hook event (run by Claude Code's hooks)",
		Long: "Reads the hook payload on stdin and $TMUX_PANE, and records the event.\n" +
			"Always exits 0 and prints nothing, except that a Stop which claims queued\n" +
			"decision answers prints one JSON line blocking the stop with them;\n" +
			"errors go to the log file.",
		Args: cobra.ArbitraryArgs,
		// Every argument reaches runHook, so a bad one is logged, not printed.
		DisableFlagParsing: true,
		Run:                runHook,
	})
}

// runHook records the event and exits 0 on every path, panics included.
// Nothing is written to stderr, and nothing to stdout except the one line
// hook.Run gives for a Stop that claimed queued answers: both belong to
// Claude Code. A watchdog exits after hookDeadline, logging it: stdin has
// no deadline of its own, and the transcript is read inside the store's
// write transaction.
func runHook(cmd *cobra.Command, args []string) {
	event := strings.Join(args, " ")
	end := &hookEnd{logger: slog.New(slog.DiscardHandler), closeLog: func() error { return nil }}
	watchdog := time.AfterFunc(hookDeadline, func() {
		end.finish(func(l *slog.Logger) {
			l.Error("hook timed out", "event", event, "after", hookDeadline.String())
		})
	})
	defer func() {
		watchdog.Stop()
		r := recover()
		end.finish(func(l *slog.Logger) {
			if r != nil {
				l.Error("hook panicked", "event", event, "panic", fmt.Sprint(r))
			}
		})
	}()

	cfg, cfgErr := loadConfig()
	if cfgErr != nil {
		cfg = config.Default() // still log to the default log file
	}
	logger := end.openLog(cfg.LogPath)
	err := cfgErr
	if err == nil {
		err = recordEvent(cmd.Context(), args, cmd.InOrStdin(), cfg, logger, func(b []byte) error {
			return end.emit(cmd.OutOrStdout(), b)
		})
	}
	if err != nil {
		logger.Error("hook failed", "event", event, "err", err.Error())
	}
}

// hookEnd ends a hook run exactly once, from the watchdog or from runHook
// returning, whichever comes first.
type hookEnd struct {
	mu       sync.Mutex
	ended    bool
	logger   *slog.Logger
	closeLog func() error
}

// openLog opens the log file at path for the rest of the run and returns
// its logger, or the discarding one when the file cannot be opened or the
// run has already ended.
func (h *hookEnd) openLog(path string) *slog.Logger {
	h.mu.Lock()
	defer h.mu.Unlock()
	l, c, err := openLog(path)
	switch {
	case err != nil:
	case h.ended:
		_ = c()
	default:
		h.logger, h.closeLog = l, c
	}
	return h.logger
}

// emit writes b to out unless the run has already ended, under the same
// lock as finish, so a watchdog that ends the run and a late result cannot
// both act.
func (h *hookEnd) emit(out io.Writer, b []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return nil
	}
	if _, err := out.Write(b); err != nil {
		return fmt.Errorf("write stop block: %w", err)
	}
	return nil
}

// finish runs last with the run's logger, closes the log and exits 0, the
// first time it is called; later calls do nothing.
func (h *hookEnd) finish(last func(*slog.Logger)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		return
	}
	h.ended = true
	last(h.logger)
	_ = h.closeLog()
	hookExit(0)
}

// recordEvent reads the payload and runs the hook against the store. What
// hook.Run gives for stdout goes to emit at once, before any tmux call, so
// a slow tmux cannot widen the gap between claiming an answer and handing
// it to Claude.
func recordEvent(ctx context.Context, args []string, stdin io.Reader, cfg config.Config, logger *slog.Logger,
	emit func([]byte) error,
) error {
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
	client := newTmuxClient()
	kind := model.EventKind(args[0])
	before := snapshotBefore(ctx, st, kind, payload, logger)
	res, err := hookRun(ctx, kind, payload, hookEnv(ctx, client, logger), st, cfg)
	if len(res.Stdout) > 0 {
		if eerr := emit(res.Stdout); eerr != nil {
			logger.Error("hook output", "event", string(kind), "err", eerr.Error())
		}
	}
	sess := res.Session
	if err != nil && sess.ID == "" {
		return err // nothing was written
	}
	sess = seenIfFocused(ctx, client, st, before, sess, logger)
	for _, w := range markerWindows(before, sess) {
		actx, cancel := context.WithTimeout(ctx, subprocessTimeout)
		if aerr := attention.Apply(actx, client, st, w); aerr != nil {
			logger.Error("hook attention marker", "window", w, "err", aerr.Error())
		}
		cancel()
	}
	return err
}

// seenIfFocused marks sess's pane seen, and returns sess seen, when the event
// is about to turn its window red while the user is looking at its pane
// (Ruling 38): there is nothing to draw their attention to. It asks tmux only
// then, so other events cost no tmux call. A failed check is logged and
// counts as not focused.
func seenIfFocused(ctx context.Context, client tmux.Client, st *store.Store, before beforeEvent, sess model.Session, logger *slog.Logger) model.Session {
	if sess.Pane == "" || sess.TmuxWindow == "" || attention.Marker(sess) == "" || attention.Marker(before.prev) != "" {
		return sess
	}
	fctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	focused, err := client.PaneFocused(fctx, sess.Pane)
	if err != nil {
		logger.Warn("tmux pane focus", "pane", sess.Pane, "err", err.Error())
		return sess
	}
	if !focused {
		return sess
	}
	now := time.Now()
	if err := st.MarkSeen(ctx, sess.Pane, now); err != nil {
		logger.Error("hook mark focused pane seen", "pane", sess.Pane, "err", err.Error())
		return sess
	}
	sess.SeenAt = now
	return sess
}

// beforeEvent is what the store held, before hook.Run, about the windows an
// event may change. known is false when it could not be read.
type beforeEvent struct {
	prev  model.Session   // the event's session; zero if it did not exist
	peers []model.Session // red live sessions that share the pane (when hook.Run ends them)
	known bool
}

// snapshotBefore reads the event's session by id, and, in a pane, on
// SessionStart (other than a compaction's for a stored session) or for a
// session with no row yet (the events where hook.Run ends the pane's other
// sessions), the pane's other red sessions. It is used
// only to decide whether to call tmux. A read that fails is logged and
// leaves known false.
func snapshotBefore(ctx context.Context, st *store.Store, kind model.EventKind, payload []byte, logger *slog.Logger) beforeEvent {
	pane := os.Getenv("TMUX_PANE")
	ev, err := hook.Parse(kind, payload, pane, time.Time{})
	if err != nil {
		return beforeEvent{} // hook.Run will report it
	}
	prev, found, err := st.GetSession(ctx, ev.SessionID)
	if err != nil {
		logger.Error("hook read session before event", "err", err.Error())
		return beforeEvent{}
	}
	b := beforeEvent{prev: prev, known: true}
	if (found && (kind != model.SessionStart || state.Compaction(ev))) || pane == "" {
		return b
	}
	live, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		logger.Error("hook list sessions before event", "err", err.Error())
		return beforeEvent{}
	}
	for _, p := range live {
		if p.Pane == pane && p.ID != ev.SessionID && p.State != model.StateStale && attention.Marker(p) != "" {
			b.peers = append(b.peers, p)
		}
	}
	return b
}

// markerWindows returns the windows whose marker the event may have changed:
// the windows of the red peers it ended, and, when the session's marker
// changed (an ended session has none, so ending a red one counts), or it
// moved away from a window, or the before read failed, the session's old and
// new windows.
func markerWindows(before beforeEvent, after model.Session) []string {
	var windows []string
	add := func(w string) {
		if w != "" && !slices.Contains(windows, w) {
			windows = append(windows, w)
		}
	}
	for _, p := range before.peers {
		add(p.TmuxWindow)
	}
	prev := before.prev
	changed := !before.known ||
		attention.Marker(prev) != attention.Marker(after) ||
		(prev.TmuxWindow != "" && prev.TmuxWindow != after.TmuxWindow)
	if changed {
		add(after.TmuxWindow)
		add(prev.TmuxWindow)
	}
	return windows
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
func hookEnv(ctx context.Context, client tmux.Client, logger *slog.Logger) hook.Env {
	return hook.Env{
		Pane: os.Getenv("TMUX_PANE"),
		Now:  time.Now,
		Git:  func(cwd string) (string, string) { return gitInfo(ctx, cwd, logger) },
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

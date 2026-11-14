package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ZachFara/curia/internal/attention"
	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// loadConfig loads ~/.config/curia/config.toml over the defaults (a missing
// file yields the defaults; CURIA_DB overrides the database path).
func loadConfig() (config.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config.Config{}, fmt.Errorf("find config: %w", err)
	}
	return config.Load(filepath.Join(home, ".config", "curia", "config.toml"))
}

// openStore opens the store at cfg.DBPath, creating it if needed.
func openStore(cfg config.Config) (*store.Store, error) {
	return store.Open(cfg.DBPath)
}

// openLog opens the log file at path for append, creating it (mode 0600) and
// its directory, and returns a logger on it and the func that closes it.
// The logger writes one line per entry: slog's text format quotes any value
// holding a line break or other unprintable character, so untrusted text in
// a value cannot start a forged entry.
func openLog(path string) (*slog.Logger, func() error, error) {
	p := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return slog.New(slog.NewTextHandler(f, nil)), f.Close, nil
}

// newTmuxClient builds the tmux client every command uses; tests replace it
// with a tmux.Fake.
var newTmuxClient = func() tmux.Client { return tmux.NewExec("") }

// clock is the wall clock of the fleet commands (ls, next); tests fix it.
var clock = time.Now

// reconcile marks sessions whose pane is gone as stale (fleet.Reconcile) and
// re-applies the attention marker of each window it marked a session stale
// in, so a red window does not stay red; a marker that fails is logged, not
// returned, unless the window is gone, which is the usual reason its session
// went stale. When tmux cannot list its panes (no server, or not installed) it
// logs that to the log file at logPath and skips reconciling.
func reconcile(ctx context.Context, st *store.Store, client tmux.Client, logPath string, now time.Time) error {
	tctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	panes, err := client.ListPanes(tctx)
	if err != nil {
		logWarn(logPath, "tmux unreachable, not reconciling", "err", err.Error())
		return nil
	}
	windows, err := fleet.Reconcile(ctx, st, panes, now)
	applyMarkers(ctx, client, st, logPath, "reconcile attention marker", windows)
	return err
}

// applyMarkers re-applies the attention marker of each window, logging a
// failure under msg unless the window is gone.
func applyMarkers(ctx context.Context, client tmux.Client, st *store.Store, logPath, msg string, windows []string) {
	for _, w := range windows {
		actx, cancel := context.WithTimeout(ctx, subprocessTimeout)
		if aerr := attention.Apply(actx, client, st, w); aerr != nil && !errors.Is(aerr, tmux.ErrNoWindow) {
			logWarn(logPath, msg, "window", w, "err", aerr.Error())
		}
		cancel()
	}
}

// listSettled lists the sessions f selects after settling those a denied
// prompt or an interrupt left working or waiting (fleet.Settle, Ruling 77),
// listing again when it settled any so they show idle. It returns the
// distinct windows of the settled sessions, whose markers may be out of
// date. A settle failure does not fail the list: the session stays as it
// was, and the failure is logged to logPath, or dropped when logPath is ""
// (status, which must stay silent).
func listSettled(ctx context.Context, st *store.Store, f store.ListFilter, logPath string, now time.Time) ([]model.Session, []string, error) {
	sessions, err := st.ListSessions(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	settled, serr := fleet.Settle(ctx, st, sessions, now)
	if serr != nil && logPath != "" {
		logWarn(logPath, "settle interrupted sessions", "err", serr.Error())
	}
	if len(settled) == 0 {
		return sessions, nil, nil
	}
	var windows []string
	for _, s := range sessions {
		if s.TmuxWindow != "" && slices.Contains(settled, s.ID) && !slices.Contains(windows, s.TmuxWindow) {
			windows = append(windows, s.TmuxWindow)
		}
	}
	sessions, err = st.ListSessions(ctx, f)
	if err != nil {
		return nil, nil, err
	}
	return sessions, windows, nil
}

// listSettledMarked is listSettled for the views that may call tmux (ls,
// next, the dashboard): it also re-applies the markers of the settled
// sessions' windows, as reconcile does for the windows it marks stale.
func listSettledMarked(ctx context.Context, st *store.Store, client tmux.Client, f store.ListFilter, logPath string, now time.Time) ([]model.Session, error) {
	sessions, windows, err := listSettled(ctx, st, f, logPath, now)
	applyMarkers(ctx, client, st, logPath, "settle attention marker", windows)
	return sessions, err
}

// logWarn appends one warning to the log file at logPath, if it opens.
func logWarn(logPath, msg string, args ...any) {
	if logger, closeLog, err := openLog(logPath); err == nil {
		logger.Warn(msg, args...)
		_ = closeLog()
	}
}

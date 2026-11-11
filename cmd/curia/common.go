package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
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

// reconcile marks sessions whose pane is gone as stale (fleet.Reconcile).
// When tmux cannot list its panes (no server, or not installed) it logs that
// to the log file at logPath and skips reconciling.
func reconcile(ctx context.Context, st *store.Store, client tmux.Client, logPath string, now time.Time) error {
	tctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	panes, err := client.ListPanes(tctx)
	if err != nil {
		if logger, closeLog, lerr := openLog(logPath); lerr == nil {
			logger.Warn("tmux unreachable, not reconciling", "err", err.Error())
			_ = closeLog()
		}
		return nil
	}
	return fleet.Reconcile(ctx, st, panes, now)
}

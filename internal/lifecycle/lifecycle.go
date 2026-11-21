// Package lifecycle starts, pauses, resumes and cleans a task's agent
// session and its worktree, and builds the command that launches the agent
// (spec section 4.6 and 4.7). It runs git through gitops and tmux through
// tmux.Client, and reads and writes the store. It decides nothing about who
// the user is: a caller that is an agent says so in SpawnOptions.ReadyOnly,
// and the user-only commands check their caller before calling in.
package lifecycle

import (
	"context"
	"time"

	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// Seams that cmd/curia sets before anything runs (cmd/curia/lifecycle.go):
// the clock, the reconcile of sessions with tmux's panes, the warning log and
// the bound on one git or tmux call are the commands' own and are shared
// with them, so the package takes them rather than owning a copy.
var (
	// Clock is the wall clock.
	Clock = time.Now
	// Reconcile marks sessions whose pane is gone as stale (cmd/curia's
	// reconcile), logging to logPath.
	Reconcile func(ctx context.Context, st *store.Store, client tmux.Client, logPath string, now time.Time) error
	// LogWarn appends a warning to the log at logPath.
	LogWarn func(logPath, msg string, args ...any)
	// SubprocessTimeout bounds each git or tmux call.
	SubprocessTimeout = time.Second
)

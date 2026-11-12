// Package notify alerts the user that a blocking decision is waiting: a
// terminal bell in the agent's pane and a macOS notification. The rate limit
// is the caller's (store.ClaimNotifySlot), because it spans processes.
package notify

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/tmux"
)

const (
	title     = "Curia: decision needed"
	bodyMax   = 140
	scriptA   = "on run argv"
	scriptB   = "display notification (item 2 of argv) with title (item 1 of argv)"
	scriptC   = "end run"
	osascript = "osascript"
)

// Run runs a program and waits for it.
type Run func(ctx context.Context, name string, args ...string) error

// Exec is the real Run.
func Exec(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// Blocking alerts for a blocking ticket: a bell in pane (when NotifyBell is
// set and pane is not empty) and, on darwin, a macOS notification (when
// NotifyMacOS is set). Both are tried; their errors are joined. Ticket text
// reaches osascript only as an argument the fixed script reads, never as part
// of the script.
func Blocking(ctx context.Context, tm tmux.Client, run Run, goos string, cfg config.Config, d model.Decision, pane string) error {
	if d.Mode != model.ModeBlocking {
		return nil
	}
	var errs []error
	if cfg.NotifyBell && pane != "" {
		errs = append(errs, tm.Bell(ctx, pane))
	}
	if cfg.NotifyMacOS && goos == "darwin" {
		body := sanitize.Line(d.ID+" "+filepath.Base(d.Repo)+": "+d.Question, bodyMax)
		errs = append(errs, run(ctx, osascript, "-e", scriptA, "-e", scriptB, "-e", scriptC, title, body))
	}
	return errors.Join(errs...)
}

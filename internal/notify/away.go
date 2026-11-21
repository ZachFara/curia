package notify

import (
	"context"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/sanitize"
)

// AwayEnded shows a macOS notification that an away period ended by itself
// (when NotifyMacOS is set, on darwin). The body reaches osascript only as an
// argument the fixed script reads. The caller owns the rate limit.
func AwayEnded(ctx context.Context, run Run, goos string, cfg config.Config, body string) error {
	if !cfg.NotifyMacOS || goos != "darwin" {
		return nil
	}
	return run(ctx, osascript, "-e", scriptA, "-e", scriptB, "-e", scriptC, "Curia: away ended", sanitize.Line(body, bodyMax))
}

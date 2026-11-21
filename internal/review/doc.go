// Package review decides whether a review may be approved and does the git and
// pull-request work of an approval, a request for changes and a drop (plan
// section 4.6). It never prints, parses flags or checks the user guard: the
// commands in cmd/curia do, before they call in. It imports the store, gitops,
// lifecycle, config, task, model, sanitize, deliver and tmux, never ui, hook,
// mcp or cmd.
package review

import (
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// OutcomeLine says what became of a message the package sent; cmd/curia sets it.
var OutcomeLine = func(out model.MessageOutcome) string { return string(out.Message.Status) }

// IntentHold is how long the intent an approval records before it runs
// git (store.ClaimReviewIntent) refuses another approval, a change request
// and a drop of the same review: longer than an approval can run (the
// view's six minutes at most), so it outlives only an approval whose process
// was killed.
const IntentHold = 10 * time.Minute

// reviewLineMax bounds a printed outcome line, in runes.
const reviewLineMax = 600

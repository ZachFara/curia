package deliver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// F21: `curia rc` types one fixed /rc line through the same guards as an
// answer or a message (reconcile, store claim, server check, a capture that
// shows the empty prompt, the recheck). A slash command carries no message
// id, so there is no row to claim: store.ClaimRC marks the session instead.

// Prompt readings of ReadPane, in the words of the recogniser's states.
const (
	PaneEmpty   = "empty"   // the empty input prompt, nothing else open
	PaneDraft   = "draft"   // text sits in the prompt
	PaneDialog  = "dialog"  // a dialog, menu or full-screen view is open
	PaneBusy    = "busy"    // a turn is running
	PaneUnknown = "unknown" // anything the recogniser does not know
)

// PaneRead is one capture of a pane: what the recogniser says about its
// prompt, and the visible text for the Remote Control indicator table.
type PaneRead struct {
	Prompt string
	Screen string
}

// ReadPane captures pane and reads its prompt. A synchronized window reads as unknown.
func ReadPane(ctx context.Context, tm tmux.Client, pane string) (PaneRead, error) {
	cctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	defer cancel()
	capt, err := tm.CapturePane(cctx, pane)
	if err != nil {
		return PaneRead{}, err
	}
	out := PaneRead{Prompt: PaneUnknown, Screen: capt.Screen}
	if capt.Synchronized {
		return out, nil
	}
	switch readCapture(capt) {
	case promptEmpty:
		out.Prompt = PaneEmpty
	case promptDraft:
		out.Prompt = PaneDraft
	case promptDialog:
		out.Prompt = PaneDialog
	case promptBusy, promptSubagents:
		out.Prompt = PaneBusy
	}
	return out, nil
}

var rcRefusals = map[store.TypingRefusal]string{
	store.TypingNoPane:      "pane gone",
	store.TypingEnded:       "the session has ended",
	store.TypingWorking:     "the agent is working",
	store.TypingWaiting:     "the agent is at a prompt of its own",
	store.TypingSubagents:   "the agent has subagents running",
	store.TypingSharedPane:  "the pane is shared by another session",
	store.TypingUnsubmitted: "an earlier typed line is not confirmed yet",
	store.TypingNotQueued:   "nothing to type",
}

// TypeRC types line, one fixed /rc line chosen by Curia, into session id's
// pane when every typing rule allows it. It returns typed, and when not the
// reason (nothing was typed). On typed true the session's rc_how is "typing":
// the caller reads the pane and settles it with Store.SetRC. A line that may
// sit in the prompt (a partial send) is settled here as unconfirmed.
func TypeRC(ctx context.Context, st *store.Store, tm tmux.Client, log *slog.Logger, id, line string, now time.Time) (typed bool, why string) {
	r := &delivery{st: st, tm: tm, log: log, now: now}
	it := item{id: "rc " + id, message: true}
	if why = r.reconcile(ctx, it); why != "" {
		return false, why
	}
	c, err := st.ClaimRC(ctx, id, now)
	switch {
	case err != nil:
		log.Warn("deliver: /rc claim failed", "session", id, "err", err.Error())
		return false, "could not check the session"
	case c.On:
		return false, "Remote Control is known to be on; a second /rc opens a panel or disconnects it"
	case c.Refusal != store.TypingClaimed:
		if c.Blocker == "/rc" {
			return false, "an earlier /rc is not confirmed yet: look at the pane, then run curia rc --check"
		}
		return false, rcRefusals[c.Refusal]
	}
	release := func() {
		sctx, cancel := settled(ctx, settleTimeout)
		defer cancel()
		if serr := st.SetRC(sctx, id, c.Session.RC, c.Session.RCHow, c.Session.RCCheckedAt); serr != nil {
			log.Warn("deliver: could not release the /rc claim", "session", id, "err", serr.Error())
		}
	}
	if why = r.serverRefusal(it.id, c.Session); why == "" {
		why = r.paneRefusal(ctx, it.id, c.Session)
	}
	if why == "" {
		why, _ = r.recheck(ctx, it, c.Session)
	}
	if why != "" {
		release()
		return false, why
	}
	switch err = tm.SendKeys(ctx, c.Session.Pane, line); {
	case err == nil:
		return true, ""
	case errors.Is(err, tmux.ErrPartialSend):
		sctx, cancel := settled(ctx, settleTimeout)
		defer cancel()
		_ = st.SetRC(sctx, id, "unknown", "unconfirmed", now)
		return false, "typing may not have finished; the line may sit in the prompt, so nothing more is typed"
	}
	log.Warn("deliver: /rc typing failed", "session", id, "err", err.Error())
	release()
	return false, "typing failed"
}

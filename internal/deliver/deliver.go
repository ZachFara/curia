// Package deliver stores an answer to a decision ticket and carries it back
// to the agent that asked (plan section 4.9): by typing one line into the
// agent's pane when that is safe, or by leaving it queued for the agent's
// Stop hook or its own check_decision.
package deliver

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/attention"
	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// tmuxTimeout bounds each tmux call deliver makes itself. SendKeys bounds
// each of its own steps (Ruling 81), so typing is not wrapped in it.
const tmuxTimeout = time.Second

// settleTimeout bounds each step that must still run once the answer is
// stored and typing is over (undoing a claim, re-reading the row, the
// attention markers), whatever is left of the caller's context. It is longer
// than the store's 2 second busy timeout, so a busy writer does not end it.
const settleTimeout = 3 * time.Second

// Notes of an outcome: why an answer is still queued, or needs no delivery.
// Every note for an answer still queued that the agent might never stop to
// collect tells the user what to do (askCheck).
const (
	noteNothing   = "nothing to deliver"
	noteNoSession = "no session to deliver to"
	// noteWorking and noteWaiting are followed by askCheck: an interrupted
	// or denied turn ends with no Stop (live check 2, defect 8).
	noteWorking = "agent is working; the Stop hook will deliver it"
	noteWaiting = "agent is at a prompt of its own; queued until it next stops"
)

// askCheck is the user's way to hand over answer id now: an idle agent never
// stops, so its Stop hook never runs.
func askCheck(id string) string {
	return "tell the agent to call check_decision " + id
}

// queuedNote is the note for an answer to d left queued for the Stop hook
// because of why.
func queuedNote(why string, d model.Decision) string {
	return why + "; queued for the Stop hook, or " + askCheck(d.ID)
}

// refusalNote says why store.ClaimTyping refused to claim the answer in c. An
// answer another path delivered first needs no note.
func refusalNote(c store.TypingClaim) string {
	d := c.Decision
	switch c.Refusal {
	case store.TypingNotQueued:
		if d.Delivery == model.DeliveryDelivered {
			return ""
		}
		return noteNothing
	case store.TypingWorking:
		return noteWorking + ", or " + askCheck(d.ID)
	case store.TypingWaiting:
		return noteWaiting + ", or " + askCheck(d.ID)
	case store.TypingEnded:
		// Defect 10: a nested session that exited leaves its pane to the
		// outer one, so "pane gone" would be wrong.
		return "the asking session has ended; it can read the answer with check_decision " + d.ID + " if it is resumed"
	case store.TypingSubagents:
		return "agent has subagents running; it gets the answer when they finish, or " + askCheck(d.ID)
	case store.TypingSharedPane:
		return queuedNote("pane is shared by another session", d) // Ruling 73
	case store.TypingUnsubmitted:
		// N4: only reading the blocking answer unblocks the session.
		return c.Unconfirmed + " was typed but not confirmed (it may sit unsubmitted in the agent's prompt); tell the agent to call check_decision " +
			c.Unconfirmed + "; " + d.ID + " is queued for the Stop hook"
	}
	return "pane gone; " + askCheck(d.ID) // stale, no pane or no row
}

// settled returns a context for a step that must run even when ctx is
// done: it keeps ctx's values but not its cancellation, and has its own
// timeout.
func settled(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// partialNote is the note for tmux.ErrPartialSend (Rulings 54 and 58): the
// claim stands, so no path delivers the answer again, and the user is told
// where to look and what to do. It names sess's tmux session and window
// when they are known.
func partialNote(d model.Decision, sess model.Session) string {
	var where []string
	if sess.TmuxSession != "" {
		where = append(where, "tmux session "+sess.TmuxSession)
	}
	if sess.TmuxWindow != "" {
		where = append(where, "window "+sess.TmuxWindow)
	}
	pane := "the agent's pane"
	if len(where) > 0 {
		pane += " (" + strings.Join(where, ", ") + ")"
	}
	return "typing may not have finished: check " + pane +
		"; press Enter if the line is there, otherwise tell it to call check_decision " + d.ID
}

// delivery is one run of Answer after the answer is stored.
type delivery struct {
	st  *store.Store
	tm  tmux.Client
	log *slog.Logger
	now time.Time
	// windows are the tmux windows whose attention marker may be out of
	// date: those Reconcile marked a session stale in.
	windows []string
}

// Answer answers ticket id with a at now and delivers the answer. A store
// error from the answer (store.ErrNotFound, store.ErrNotOpen,
// decision.ErrBadAnswer or another) is returned and nothing else happens;
// a second answer to the same ticket therefore delivers nothing. Once the
// answer is stored, nothing else is returned as an error: delivery problems
// are logged at WARN and told in the outcome's note.
//
// An answer is typed only into the pane of a session that, after
// reconciling with the live panes and inside store.ClaimTyping's
// transaction, is idle or needs_decision with no subagent running and no
// earlier typed answer unconfirmed, and then only when a capture of the pane
// taken just before typing shows Claude Code's empty input prompt (Ruling
// 85); the only text typed is decision.TypedMessage. A SendKeys error wrapping tmux.ErrPartialSend
// leaves the claim in place, since the line may be in the prompt; any other
// SendKeys error requeues the answer. Then the attention markers of the
// asking session's window and of every window Reconcile changed are
// re-applied. The requeue and the markers run even when ctx is done, each
// with its own short timeout, so a deadline met while typing never leaves
// an answer claimed that was not typed. The outcome holds the decision as
// stored.
func Answer(ctx context.Context, st *store.Store, tm tmux.Client, log *slog.Logger, id string, a model.Answer, now time.Time) (model.Outcome, error) {
	d, err := st.AnswerDecision(ctx, id, a, now)
	if err != nil {
		return model.Outcome{}, err
	}
	r := &delivery{st: st, tm: tm, log: log, now: now}
	out := r.deliver(ctx, d)
	r.mark(ctx, out.Decision)
	return out, nil
}

// deliver delivers stored answer d, if it needs delivering.
func (r *delivery) deliver(ctx context.Context, d model.Decision) model.Outcome {
	switch {
	case d.SessionID == "":
		return model.Outcome{Decision: d, Note: noteNoSession}
	case d.Delivery != model.DeliveryQueued:
		return model.Outcome{Decision: d, Note: noteNothing}
	}
	lctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	panes, err := r.tm.ListPanes(lctx)
	cancel()
	if err != nil {
		r.log.Warn("deliver: tmux unreachable, answer left queued", "id", d.ID, "err", err.Error())
		return model.Outcome{Decision: d, Note: queuedNote("tmux unreachable", d)}
	}
	r.windows, err = fleet.Reconcile(ctx, r.st, panes, r.now)
	if err != nil {
		r.log.Warn("deliver: reconcile failed, answer left queued", "id", d.ID, "err", err.Error())
		return model.Outcome{Decision: d, Note: queuedNote("could not check the session", d)}
	}
	c, err := r.st.ClaimTyping(ctx, d.ID, r.now)
	if err != nil {
		r.log.Warn("deliver: typing claim failed, answer left queued", "id", d.ID, "err", err.Error())
		return model.Outcome{Decision: d, Note: queuedNote("could not check the session", d)}
	}
	if c.Refusal != store.TypingClaimed {
		return model.Outcome{Decision: c.Decision, Note: refusalNote(c)}
	}
	return r.typeLine(ctx, c.Decision, c.Session)
}

// promptNotes say why a capture of the agent's pane refused typing
// (Ruling 85). Nothing was typed in any of these cases.
var promptNotes = map[promptState]string{
	promptUnknown:   "prompt not recognized (Claude Code's screen may have changed); nothing typed",
	promptDraft:     "prompt has a draft; nothing typed",
	promptDialog:    "a dialog or menu is open in the agent's pane; nothing typed",
	promptBusy:      "agent's pane shows a running turn; nothing typed",
	promptSubagents: "agent is waiting for background subagents; nothing typed",
	promptAgents:    "the agent's prompt lists background agents (or offers them); nothing typed",
	promptShell:     "the pane shows a shell: the session's Claude Code is not at its prompt; nothing typed",
}

// noteSynchronized is the refusal for a window with synchronize-panes on.
const noteSynchronized = "the agent's window has synchronize-panes on, so typing would reach every pane in it; nothing typed"

// paneRefusal captures the pane of sess just before typing and returns why
// it must not be typed into, or "" when it shows Claude Code's empty input
// prompt (Ruling 85).
func (r *delivery) paneRefusal(ctx context.Context, d model.Decision, sess model.Session) string {
	cctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	capt, err := r.tm.CapturePane(cctx, sess.Pane)
	cancel()
	if err != nil {
		r.log.Warn("deliver: could not read the agent's pane; not typing", "id", d.ID, "pane", sess.Pane, "err", err.Error())
		return "could not read the agent's pane; nothing typed"
	}
	if capt.Synchronized {
		// N5: the keys would reach every pane of the window, none of which
		// but this one was captured.
		r.log.Info("deliver: not typing into a synchronized window", "id", d.ID, "pane", sess.Pane)
		return noteSynchronized
	}
	state := readPrompt(capt.Screen)
	if state == promptEmpty {
		return ""
	}
	r.log.Info("deliver: not typing into the agent's pane", "id", d.ID, "pane", sess.Pane, "why", promptNotes[state])
	return promptNotes[state]
}

// typeLine types claimed answer d into the pane of sess, once a capture of
// the pane shows an empty input prompt.
func (r *delivery) typeLine(ctx context.Context, d model.Decision, sess model.Session) model.Outcome {
	if why := r.paneRefusal(ctx, d, sess); why != "" {
		return r.requeue(ctx, d, why, queuedNote(why, d))
	}
	// m3: a hook event may have landed while the pane was captured.
	switch why, err := r.st.StillTypable(ctx, sess.ID, sess.Pane); {
	case err != nil:
		r.log.Warn("deliver: recheck before typing failed; not typing", "id", d.ID, "err", err.Error())
		const what = "could not check the session again; nothing typed"
		return r.requeue(ctx, d, what, queuedNote(what, d))
	case why != store.TypingClaimed:
		r.log.Info("deliver: the session changed while its pane was read; not typing", "id", d.ID, "why", int(why))
		return r.requeue(ctx, d, "the session changed while its pane was read", refusalNote(store.TypingClaim{Decision: d, Refusal: why}))
	}
	err := r.tm.SendKeys(ctx, sess.Pane, decision.TypedMessage(d))
	switch {
	case err == nil:
		return model.Outcome{Decision: d}
	case errors.Is(err, tmux.ErrPartialSend):
		// The line may be in the prompt: never type it again, and never
		// requeue it for the Stop hook to deliver a second time.
		r.log.Warn("deliver: typing may not have finished; not typing again", "id", d.ID, "pane", sess.Pane, "err", err.Error())
		return model.Outcome{Decision: d, Note: partialNote(d, sess)}
	}
	r.log.Warn("deliver: typing failed", "id", d.ID, "pane", sess.Pane, "err", err.Error())
	return r.requeue(ctx, d, "typing failed", queuedNote("typing failed", d))
}

// requeue undoes the typing claim of d, after which nothing was typed, for
// the reason why, and notes it with note. It runs even if ctx ran out, or
// the answer would read as delivered and no path would deliver it. An
// answer the agent read with check_decision meanwhile stays delivered
// (re-review m1) and needs no note.
func (r *delivery) requeue(ctx context.Context, d model.Decision, why, note string) model.Outcome {
	qctx, cancel := settled(ctx, settleTimeout)
	defer cancel()
	if rerr := r.st.RequeueDelivery(qctx, d.ID); rerr != nil {
		r.log.Warn("deliver: requeue failed; the answer reads as delivered", "id", d.ID, "err", rerr.Error())
		return model.Outcome{Decision: d, Note: why + " and the answer could not be queued again; " + askCheck(d.ID)}
	}
	stored, found, err := r.st.GetDecision(qctx, d.ID)
	switch {
	case err != nil || !found:
		r.log.Warn("deliver: re-read after requeue", "id", d.ID, "found", found, "err", errText(err))
		// Report the row the requeue wrote.
		d.Delivery, d.DeliveredVia, d.DeliveredAt = model.DeliveryQueued, "", time.Time{}
		return model.Outcome{Decision: d, Note: note}
	case stored.Delivery != model.DeliveryQueued:
		r.log.Info("deliver: answer read with check_decision meanwhile; not queued again", "id", d.ID)
		return model.Outcome{Decision: stored}
	}
	r.log.Info("deliver: answer queued again", "id", d.ID)
	return model.Outcome{Decision: stored, Note: note}
}

// errText is err's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// mark re-applies the attention marker of the window of d's session and of
// every window Reconcile changed, even when ctx is done, so the asking
// window is not left red. Failures are logged at WARN, except for a window
// that no longer exists.
func (r *delivery) mark(ctx context.Context, d model.Decision) {
	windows := r.windows
	if d.SessionID != "" {
		gctx, cancel := settled(ctx, settleTimeout)
		sess, found, err := r.st.GetSession(gctx, d.SessionID)
		cancel()
		switch {
		case err != nil:
			r.log.Warn("deliver: read session for its marker", "session", d.SessionID, "err", err.Error())
		case found && sess.TmuxWindow != "" && !slices.Contains(windows, sess.TmuxWindow):
			windows = append([]string{sess.TmuxWindow}, windows...)
		}
	}
	for _, w := range windows {
		actx, cancel := settled(ctx, tmuxTimeout)
		if err := attention.Apply(actx, r.tm, r.st, w); err != nil && !errors.Is(err, tmux.ErrNoWindow) {
			r.log.Warn("deliver: attention marker", "window", w, "err", err.Error())
		}
		cancel()
	}
}

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

// tmuxTimeout bounds each tmux call.
const tmuxTimeout = time.Second

// settleTimeout bounds each step that must still run once the answer is
// stored and typing is over (undoing a claim, re-reading the row, the
// attention markers), whatever is left of the caller's context.
const settleTimeout = time.Second

// Notes of an outcome: why an answer is still queued, or needs no delivery.
const (
	noteNothing   = "nothing to deliver"
	noteNoSession = "no session to deliver to"
	noteNoTmux    = "tmux unreachable; queued for the Stop hook"
	noteWorking   = "agent is working; the Stop hook will deliver it"
	noteWaiting   = "agent is at a prompt of its own; queued until it next stops"
	notePaneGone  = "pane gone; the agent gets it from check_decision"
	noteFailed    = "typing failed; queued for the Stop hook"
	// noteShared is for a pane another live session is bound to (Ruling 73).
	noteShared = "pane is shared by another session; queued for the Stop hook"
	// noteNoCheck is for a store error before the claim: nothing was typed.
	noteNoCheck = "could not check the session; queued for the Stop hook"
	// noteNotRequeued is for a typing failure whose claim could not be undone.
	noteNotRequeued = "typing failed and the answer could not be queued again; " +
		"the agent can read it with check_decision"
)

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
// transaction, is idle or needs_decision; the only text typed is
// decision.TypedMessage. A SendKeys error wrapping tmux.ErrPartialSend
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
		return model.Outcome{Decision: d, Note: noteNoTmux}
	}
	r.windows, err = fleet.Reconcile(ctx, r.st, panes, r.now)
	if err != nil {
		r.log.Warn("deliver: reconcile failed, answer left queued", "id", d.ID, "err", err.Error())
		return model.Outcome{Decision: d, Note: noteNoCheck}
	}
	claimed, sess, ok, err := r.st.ClaimTyping(ctx, d.ID, r.now)
	if err != nil {
		r.log.Warn("deliver: typing claim failed, answer left queued", "id", d.ID, "err", err.Error())
		return model.Outcome{Decision: d, Note: noteNoCheck}
	}
	if !ok {
		return model.Outcome{Decision: claimed, Note: unclaimedNote(claimed, sess)}
	}
	return r.typeLine(ctx, claimed, sess)
}

// unclaimedNote says why answer d to session sess was not claimed for
// typing. An answer another path delivered first needs no note. A queued
// answer to a session idle or needs_decision in a pane was refused because
// another live session shares the pane (store.ClaimTyping).
func unclaimedNote(d model.Decision, sess model.Session) string {
	switch {
	case d.Delivery == model.DeliveryDelivered:
		return ""
	case sess.State == model.StateWorking:
		return noteWorking
	case sess.State == model.StateWaiting:
		return noteWaiting
	case sess.Pane != "" && (sess.State == model.StateIdle || sess.State == model.StateNeedsDecision):
		return noteShared
	}
	return notePaneGone // stale, ended, no pane or no row
}

// typeLine types claimed answer d into the pane of sess.
func (r *delivery) typeLine(ctx context.Context, d model.Decision, sess model.Session) model.Outcome {
	sctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	err := r.tm.SendKeys(sctx, sess.Pane, decision.TypedMessage(d))
	cancel()
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
	// Nothing was typed: undo the claim even if ctx ran out while typing,
	// or the answer would read as delivered and no path would deliver it.
	qctx, cancel := settled(ctx, settleTimeout)
	defer cancel()
	if rerr := r.st.RequeueDelivery(qctx, d.ID); rerr != nil {
		r.log.Warn("deliver: requeue failed; the answer reads as delivered", "id", d.ID, "err", rerr.Error())
		return model.Outcome{Decision: d, Note: noteNotRequeued}
	}
	r.log.Info("deliver: answer queued again", "id", d.ID)
	stored, found, err := r.st.GetDecision(qctx, d.ID)
	if err != nil || !found {
		r.log.Warn("deliver: re-read after requeue", "id", d.ID, "found", found, "err", errText(err))
		// The requeue went through: report the row it wrote.
		d.Delivery, d.DeliveredVia, d.DeliveredAt = model.DeliveryQueued, "", time.Time{}
		return model.Outcome{Decision: d, Note: noteFailed}
	}
	return model.Outcome{Decision: stored, Note: noteFailed}
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
// window is not left red. Failures are logged at WARN.
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
		if err := attention.Apply(actx, r.tm, r.st, w); err != nil {
			r.log.Warn("deliver: attention marker", "window", w, "err", err.Error())
		}
		cancel()
	}
}

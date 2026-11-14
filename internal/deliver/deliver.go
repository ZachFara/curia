// Package deliver stores an answer to a decision ticket and carries it back
// to the agent that asked, and carries a queued message to its target
// session (plan section 4.9): by typing one line into the agent's pane when
// that is safe, or by leaving it queued for the agent's Stop hook or its own
// check_decision or check_messages. Answers and messages share one typing
// path.
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

// Notes of an outcome: why an answer or a message is still queued, or needs
// no delivery. Every note for one still queued that the agent might never
// stop to collect tells the user what to do (item.ask).
const (
	noteNothing   = "nothing to deliver"
	noteNoSession = "no session to deliver to"
	// noteWorking and noteWaiting are followed by the ask: an interrupted
	// or denied turn ends with no Stop (live check 2, defect 8).
	noteWorking = "agent is working; the Stop hook will deliver it"
	noteWaiting = "agent is at a prompt of its own; queued until it next stops"
)

// askCheck is the user's way to hand over answer id now: an idle agent never
// stops, so its Stop hook never runs.
func askCheck(id string) string {
	return "tell the agent to call check_decision " + id
}

// askMessages is the user's way to hand over the session's messages now,
// and to confirm one typed into it.
const askMessages = "tell the agent to call check_messages"

// askFor is the way to hand over, or confirm, the typed line id: a message
// (M-<n>) or an answer (D-<n>).
func askFor(id string) string {
	if strings.HasPrefix(id, "M-") {
		return askMessages
	}
	return askCheck(id)
}

// item is an answer or a message on the one typing path both share: what
// its notes and logs call it, the line typed for it, and how its claim is
// undone.
type item struct {
	id      string // D-<n> or M-<n>
	message bool   // a message, not an answer
	text    string // the line to type, set once claimed
	// requeue undoes the typing claim and re-reads the row into the
	// caller's outcome. It reports false when the agent read the row
	// meanwhile (check_decision or check_messages), which leaves it
	// delivered.
	requeue func(ctx context.Context) (bool, error)
	// guard, when set, is the item's own last check before typing, run
	// after StillTypable: why not to type into sess, or "".
	guard func(ctx context.Context, sess model.Session) string
}

// noun is what the notes and logs call it.
func (it item) noun() string {
	if it.message {
		return "message"
	}
	return "answer"
}

// ask is the user's way to hand it over now.
func (it item) ask() string {
	if it.message {
		return askMessages
	}
	return askCheck(it.id)
}

// queuedNote is the note for it left queued for the Stop hook because of
// why.
func (it item) queuedNote(why string) string {
	return why + "; queued for the Stop hook, or " + it.ask()
}

// endedNote is the note for it when its session has ended.
func (it item) endedNote() string {
	if it.message {
		return "the target session has ended; if it is resumed, " + askMessages
	}
	return "the asking session has ended; it can read the answer with check_decision " + it.id + " if it is resumed"
}

// unsubmittedNote is the note for it when the typed line blocker (D-<n> or
// M-<n>) of its session is not confirmed. N4: only reading the blocking
// line unblocks the session; a message's note still ends with its own ask.
func (it item) unsubmittedNote(blocker string) string {
	head := blocker + " was typed but not confirmed (it may sit unsubmitted in the agent's prompt); "
	if !it.message {
		return head + askFor(blocker) + "; " + it.id + " is queued for the Stop hook"
	}
	if ask := askFor(blocker); ask != askMessages {
		head += ask + "; "
	}
	return head + it.id + " is queued for the Stop hook, or " + askMessages
}

// refusalNote says why the store's typing claim refused it (why), with
// blocker the typed line behind TypingUnsubmitted. One another path
// delivered first (delivered) needs no note.
func refusalNote(it item, why store.TypingRefusal, blocker string, delivered bool) string {
	switch why {
	case store.TypingNotQueued:
		if delivered {
			return ""
		}
		return noteNothing
	case store.TypingWorking:
		return noteWorking + ", or " + it.ask()
	case store.TypingWaiting:
		return noteWaiting + ", or " + it.ask()
	case store.TypingEnded:
		// Defect 10: a nested session that exited leaves its pane to the
		// outer one, so "pane gone" would be wrong.
		return it.endedNote()
	case store.TypingSubagents:
		return "agent has subagents running; it gets the " + it.noun() + " when they finish, or " + it.ask()
	case store.TypingSharedPane:
		return it.queuedNote("pane is shared by another session") // Ruling 73
	case store.TypingUnsubmitted:
		return it.unsubmittedNote(blocker)
	}
	return "pane gone; " + it.ask() // stale, no pane or no row
}

// settled returns a context for a step that must run even when ctx is
// done: it keeps ctx's values but not its cancellation, and has its own
// timeout.
func settled(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// partialNote is the note for tmux.ErrPartialSend (Rulings 54 and 58): the
// claim stands, so no path delivers it again, and the user is told where to
// look and what to do. It names sess's tmux session and window when they
// are known.
func partialNote(it item, sess model.Session) string {
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
	otherwise := "tell it to call check_decision " + it.id
	if it.message {
		otherwise = askMessages
	}
	return "typing may not have finished: check " + pane + "; press Enter if the line is there, otherwise " + otherwise
}

// delivery is one run of Answer or Message after the row is read.
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
// earlier typed line (answer or message) unconfirmed, and then only when a
// capture of the pane taken just before typing shows Claude Code's empty
// input prompt (Ruling 85); the only text typed is decision.TypedMessage.
// A SendKeys error wrapping tmux.ErrPartialSend leaves the claim in place,
// since the line may be in the prompt; any other SendKeys error requeues the
// answer. Then the attention markers of the asking session's window and of
// every window Reconcile changed are re-applied. The requeue and the markers
// run even when ctx is done, each with its own short timeout, so a deadline
// met while typing never leaves an answer claimed that was not typed. The
// outcome holds the decision as stored. Message shares every step after the
// claim.
func Answer(ctx context.Context, st *store.Store, tm tmux.Client, log *slog.Logger, id string, a model.Answer, now time.Time) (model.Outcome, error) {
	d, err := st.AnswerDecision(ctx, id, a, now)
	if err != nil {
		return model.Outcome{}, err
	}
	r := &delivery{st: st, tm: tm, log: log, now: now}
	out := r.answer(ctx, d)
	r.mark(ctx, out.Decision.SessionID)
	return out, nil
}

// answer delivers stored answer d, if it needs delivering.
func (r *delivery) answer(ctx context.Context, d model.Decision) model.Outcome {
	switch {
	case d.SessionID == "":
		return model.Outcome{Decision: d, Note: noteNoSession}
	case d.Delivery != model.DeliveryQueued:
		return model.Outcome{Decision: d, Note: noteNothing}
	}
	out := model.Outcome{Decision: d}
	it := item{id: d.ID, requeue: r.requeueAnswer(&out.Decision)}
	if why := r.reconcile(ctx, it); why != "" {
		out.Note = it.queuedNote(why)
		return out
	}
	c, err := r.st.ClaimTyping(ctx, d.ID, r.now)
	if err != nil {
		r.log.Warn("deliver: typing claim failed, answer left queued", "id", d.ID, "err", err.Error())
		out.Note = it.queuedNote("could not check the session")
		return out
	}
	out.Decision = c.Decision
	if c.Refusal != store.TypingClaimed {
		out.Note = refusalNote(it, c.Refusal, c.Unconfirmed, c.Decision.Delivery == model.DeliveryDelivered)
		return out
	}
	it.text = decision.TypedMessage(c.Decision)
	out.Note = r.typeLine(ctx, it, c.Session)
	return out
}

// requeueAnswer is an answer's item.requeue: it undoes the claim of *d and
// reads the row back into *d.
func (r *delivery) requeueAnswer(d *model.Decision) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		if err := r.st.RequeueDelivery(ctx, d.ID); err != nil {
			return false, err
		}
		stored, found, err := r.st.GetDecision(ctx, d.ID)
		if err != nil || !found {
			r.log.Warn("deliver: re-read after requeue", "id", d.ID, "found", found, "err", errText(err))
			// Report the row the requeue wrote.
			d.Delivery, d.DeliveredVia, d.DeliveredAt = model.DeliveryQueued, "", time.Time{}
			return true, nil
		}
		*d = stored
		return stored.Delivery == model.DeliveryQueued, nil
	}
}

// reconcile is the typing path's step 2: it lists the live panes (1 s) and
// reconciles the store with them, which marks stale the sessions whose pane
// is gone or recycled. It returns why it could not, or "".
func (r *delivery) reconcile(ctx context.Context, it item) string {
	lctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	panes, err := r.tm.ListPanes(lctx)
	cancel()
	if err != nil {
		r.log.Warn("deliver: tmux unreachable, "+it.noun()+" left queued", "id", it.id, "err", err.Error())
		return "tmux unreachable"
	}
	r.windows, err = fleet.Reconcile(ctx, r.st, panes, r.now)
	if err != nil {
		r.log.Warn("deliver: reconcile failed, "+it.noun()+" left queued", "id", it.id, "err", err.Error())
		return "could not check the session"
	}
	return ""
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
	promptManage: "background agents or teammates are listed under the agent's prompt " +
		"(nothing is typed to a team lead while its teammates are alive); nothing typed",
	promptShell: "the pane shows a shell: the session's Claude Code is not at its prompt; nothing typed",
}

// noteSynchronized is the refusal for a window with synchronize-panes on.
const noteSynchronized = "the agent's window has synchronize-panes on, so typing would reach every pane in it; nothing typed"

// paneRefusal captures the pane of sess just before typing the line id and
// returns why it must not be typed into, or "" when it shows Claude Code's
// empty input prompt (Ruling 85).
func (r *delivery) paneRefusal(ctx context.Context, id string, sess model.Session) string {
	cctx, cancel := context.WithTimeout(ctx, tmuxTimeout)
	capt, err := r.tm.CapturePane(cctx, sess.Pane)
	cancel()
	if err != nil {
		r.log.Warn("deliver: could not read the agent's pane; not typing", "id", id, "pane", sess.Pane, "err", err.Error())
		return "could not read the agent's pane; nothing typed"
	}
	if capt.Synchronized {
		// N5: the keys would reach every pane of the window, none of which
		// but this one was captured.
		r.log.Info("deliver: not typing into a synchronized window", "id", id, "pane", sess.Pane)
		return noteSynchronized
	}
	state := readPrompt(capt.Screen)
	if state == promptEmpty {
		return ""
	}
	r.log.Info("deliver: not typing into the agent's pane", "id", id, "pane", sess.Pane, "why", promptNotes[state])
	return promptNotes[state]
}

// typeLine types claimed item it into the pane of sess, once a capture of
// the pane shows an empty input prompt and the session, read again, may
// still be typed into (steps 4 to 6 of the typing path). It returns the
// outcome's note: "" when typed.
func (r *delivery) typeLine(ctx context.Context, it item, sess model.Session) string {
	if why := r.paneRefusal(ctx, it.id, sess); why != "" {
		return r.requeue(ctx, it, why, it.queuedNote(why))
	}
	if why, note := r.recheck(ctx, it, sess); why != "" {
		return r.requeue(ctx, it, why, note)
	}
	err := r.tm.SendKeys(ctx, sess.Pane, it.text)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, tmux.ErrPartialSend):
		// The line may be in the prompt: never type it again, and never
		// requeue it for the Stop hook to deliver a second time.
		r.log.Warn("deliver: typing may not have finished; not typing again", "id", it.id, "pane", sess.Pane, "err", err.Error())
		return partialNote(it, sess)
	}
	r.log.Warn("deliver: typing failed", "id", it.id, "pane", sess.Pane, "err", err.Error())
	return r.requeue(ctx, it, "typing failed", it.queuedNote("typing failed"))
}

// recheck is step 5: m3, a hook event may have landed while the pane was
// captured, so the session is read again (store.StillTypable), then the
// item's own guard runs. It returns why not to type and the note, or "".
func (r *delivery) recheck(ctx context.Context, it item, sess model.Session) (why, note string) {
	switch why, err := r.st.StillTypable(ctx, sess.ID, sess.Pane); {
	case err != nil:
		r.log.Warn("deliver: recheck before typing failed; not typing", "id", it.id, "err", err.Error())
		const what = "could not check the session again; nothing typed"
		return what, it.queuedNote(what)
	case why != store.TypingClaimed:
		r.log.Info("deliver: the session changed while its pane was read; not typing", "id", it.id, "why", int(why))
		return "the session changed while its pane was read", refusalNote(it, why, "", false)
	}
	if it.guard != nil {
		if why := it.guard(ctx, sess); why != "" {
			r.log.Info("deliver: not typing", "id", it.id, "pane", sess.Pane, "why", why)
			return why, it.queuedNote(why)
		}
	}
	return "", ""
}

// requeue undoes the typing claim of it, after which nothing was typed, for
// the reason why, and returns note. It runs even if ctx ran out, or the row
// would read as delivered and no path would deliver it. A row the agent
// read meanwhile (re-review m1) stays delivered and needs no note.
func (r *delivery) requeue(ctx context.Context, it item, why, note string) string {
	qctx, cancel := settled(ctx, settleTimeout)
	defer cancel()
	queued, err := it.requeue(qctx)
	switch {
	case err != nil:
		r.log.Warn("deliver: requeue failed; the "+it.noun()+" reads as delivered", "id", it.id, "err", err.Error())
		return why + " and the " + it.noun() + " could not be queued again; " + it.ask()
	case !queued:
		tool := "check_decision"
		if it.message {
			tool = "check_messages"
		}
		r.log.Info("deliver: "+it.noun()+" read with "+tool+" meanwhile; not queued again", "id", it.id)
		return ""
	}
	r.log.Info("deliver: "+it.noun()+" queued again", "id", it.id)
	return note
}

// errText is err's text, or "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// mark re-applies the attention marker of the window of session sessionID
// (the asking or target session) and of every window Reconcile changed,
// even when ctx is done, so that window is not left red. Failures are
// logged at WARN, except for a window that no longer exists.
func (r *delivery) mark(ctx context.Context, sessionID string) {
	windows := r.windows
	if sessionID != "" {
		gctx, cancel := settled(ctx, settleTimeout)
		sess, found, err := r.st.GetSession(gctx, sessionID)
		cancel()
		switch {
		case err != nil:
			r.log.Warn("deliver: read session for its marker", "session", sessionID, "err", err.Error())
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

package deliver

import (
	"context"
	"log/slog"
	"time"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// Notes for a message that is never typed whatever the session's state.
const (
	noteNotInstruction = "a rule or task is stored, never typed; nothing to deliver"
	// A message another agent session sent (curia send) is never typed
	// into a pane that session is or was bound to: the guards below.
	noteSenderSelf    = "the message comes from the target session itself; nothing typed"
	noteSenderPane    = "the sending agent session is in the target's pane; nothing typed"
	noteSenderUnknown = "the sending agent session is not known, so it may share the target's pane; nothing typed"
	noteSenderUnread  = "could not check the sending agent session; nothing typed"
)

// Message delivers queued instruction id by typing it into its target session's pane when every
// condition of the typing path holds, and otherwise leaves it queued and says why.
//
// It returns an error only when the message cannot be read (store.ErrNoMessage
// when there is none). A message that is not a queued instruction with a
// target session touches no tmux. Otherwise it takes Answer's path from step
// 2 with the message in place of an answer: ListPanes (1 s) and
// fleet.Reconcile; store.ClaimMessageTyping, whose refusal leaves the
// message queued with a note; CapturePane and readPrompt; store.StillTypable
// and, for a message an agent session sent, the sender guard; then SendKeys
// with message.TypedLine, the only text typed. Every refusal after the claim
// undoes it once (store.RequeueMessage); a SendKeys error wrapping
// tmux.ErrPartialSend keeps it, so the line is never typed or handed over
// again. Every note for a message left queued or unconfirmed ends with
// "tell the agent to call check_messages". The markers of the target's
// window and of every window Reconcile changed are re-applied. The outcome
// holds the message as stored.
func Message(ctx context.Context, st *store.Store, tm tmux.Client, log *slog.Logger, id string, now time.Time) (model.MessageOutcome, error) {
	m, found, err := st.GetMessage(ctx, id)
	if err != nil {
		return model.MessageOutcome{}, err
	}
	if !found {
		return model.MessageOutcome{}, store.ErrNoMessage
	}
	r := &delivery{st: st, tm: tm, log: log, now: now}
	out := r.message(ctx, m)
	r.mark(ctx, out.Message.TargetSession)
	return out, nil
}

// message delivers stored message m, if it is a queued instruction for a
// session.
func (r *delivery) message(ctx context.Context, m model.Message) model.MessageOutcome {
	out := model.MessageOutcome{Message: m}
	switch {
	case m.Status == model.MsgDelivered:
		return out
	case m.Status != model.MsgQueued:
		out.Note = noteNothing
		return out
	case m.Kind != model.KindInstruction:
		out.Note = noteNotInstruction
		return out
	case m.TargetSession == "":
		out.Note = noteNoSession
		return out
	}
	it := item{id: m.ID, message: true, requeue: r.requeueMessage(&out.Message), guard: r.senderGuard(m)}
	if why := r.reconcile(ctx, it); why != "" {
		out.Note = it.queuedNote(why)
		return out
	}
	c, err := r.st.ClaimMessageTyping(ctx, m.ID, r.now)
	if err != nil {
		r.log.Warn("deliver: typing claim failed, message left queued", "id", m.ID, "err", err.Error())
		out.Note = it.queuedNote("could not check the session")
		return out
	}
	out.Message = c.Message
	if c.Refusal != store.TypingClaimed {
		out.Note = refusalNote(it, c.Refusal, c.Unconfirmed, c.Message.Status == model.MsgDelivered)
		return out
	}
	it.text = message.TypedLine(c.Message)
	out.Note = r.typeLine(ctx, it, c.Session)
	return out
}

// requeueMessage is a message's item.requeue: it undoes the claim of *m and
// reads the row back into *m.
func (r *delivery) requeueMessage(m *model.Message) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		if err := r.st.RequeueMessage(ctx, m.ID); err != nil {
			return false, err
		}
		stored, found, err := r.st.GetMessage(ctx, m.ID)
		if err != nil || !found {
			r.log.Warn("deliver: re-read after requeue", "id", m.ID, "found", found, "err", errText(err))
			// Report the row the requeue wrote.
			m.Status, m.DeliveredVia, m.DeliveredAt = model.MsgQueued, "", time.Time{}
			return true, nil
		}
		*m = stored
		return stored.Status == model.MsgQueued, nil
	}
}

// senderGuard is the last check before typing message m, run after
// StillTypable, or nil for a message from the user. A message every other
// sender wrote is typed only when its sending session is known, is not the
// target, and is not bound, in any state, to the target's pane: an agent
// never types into its own pane, nor into one it shares (Ruling 73 covers
// a live sharer; this also covers one that ended or went stale there). The
// sender cannot choose or skip any other step.
func (r *delivery) senderGuard(m model.Message) func(context.Context, model.Session) string {
	if m.Sender == model.SenderUser {
		return nil
	}
	return func(ctx context.Context, target model.Session) string {
		switch m.SenderSession {
		case "":
			return noteSenderUnknown
		case target.ID:
			return noteSenderSelf
		}
		sender, found, err := r.st.GetSession(ctx, m.SenderSession)
		switch {
		case err != nil:
			r.log.Warn("deliver: read the sending session; not typing", "id", m.ID, "err", err.Error())
			return noteSenderUnread
		case !found:
			return noteSenderUnknown
		case sender.Pane != "" && sender.Pane == target.Pane:
			return noteSenderPane
		}
		return ""
	}
}

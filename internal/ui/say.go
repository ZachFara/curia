package ui

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/route"
	"github.com/ZachFara/curia/internal/sanitize"
)

// SayHold is how long an auto-sent message is held for undo before it is
// released (plan section 4.10, Mode C).
const SayHold = 5 * time.Second

// proposeTimeout bounds Propose: the router's own 5 s limit and the store
// reads around it.
const proposeTimeout = 10 * time.Second

// noticeSayTyped is shown when typed or pasted text that is no command
// reaches the proposal; it starts the quiet period (Ruling 88).
const noticeSayTyped = "typing ignored; enter sends, esc cancels"

// noticeTooLate is shown when an undo comes after the hold ran out.
const noticeTooLate = "too late to undo; sending"

// Proposal is how a message could be sent, for me to confirm or change.
type Proposal struct {
	Text       string
	Kind       model.MessageKind
	Candidates []route.Candidate // what Tab cycles through: the pick first, then the runner-up, then the rest
	Confidence float64           // 0 when no model was asked
	Routed     model.RoutedBy    // RoutedAddress, RoutedManual, or RoutedConfirmed for a model suggestion
	Auto       bool              // passed every auto-send guard: send held, with the undo window
	Note       string            // why the model was not used or auto-send did not apply; shown dim
}

// Choice is what I confirmed.
type Choice struct {
	Proposal Proposal
	Target   route.Candidate
	Kind     model.MessageKind
	Changed  bool // the target or the kind differs from the proposal's
}

// SaySource is where the say view proposes, sends, holds, releases, undoes
// and cancels a message. The command's implementation stores and delivers.
// An error from Send means nothing was stored, so the view offers the
// proposal again.
type SaySource interface {
	Propose(ctx context.Context, text string) (Proposal, error)           // may take up to 5 s
	Send(ctx context.Context, c Choice) (model.MessageOutcome, error)     // store queued, then deliver or apply
	Hold(ctx context.Context, c Choice) (model.Message, error)            // store held
	Release(ctx context.Context, id string) (model.MessageOutcome, error) // held -> queued, then deliver or apply
	Undo(ctx context.Context, id string) error
	Cancel(ctx context.Context, text string) error // record a canceled message
}

// sayState is what the say view is doing.
type sayState int

const (
	sayInput     sayState = iota // typing the message
	sayRouting                   // Propose in flight
	sayProposal                  // waiting for Enter, Tab, k or Esc
	saySending                   // Send in flight
	sayHolding                   // Hold in flight
	sayHeld                      // counting down the undo window
	sayReleasing                 // Release in flight
	sayUndoing                   // Undo in flight
	sayCanceling                 // Cancel in flight
	sayDone                      // the outcome or an error is shown
)

// sayKinds is the order k steps through.
var sayKinds = []model.MessageKind{model.KindInstruction, model.KindRule, model.KindTask}

// SayModel is the say view's Bubble Tea model.
type SayModel struct {
	src  SaySource
	now  func() time.Time
	tick func(time.Duration) tea.Cmd // schedules the next countdown tick

	state  sayState
	buf    []rune    // the input line
	burst  time.Time // when the last burst of several typed runes came
	text   string    // the message, once submitted
	p      Proposal
	target int // index of the shown target in p.Candidates
	kind   model.MessageKind

	shownSince time.Time // when the proposal, as it is now, came on screen
	lastEnter  time.Time // when the last Enter came, acted on or not
	quietUntil time.Time // keys but Esc before this are ignored (answeredQuiet)

	held      model.Message // the message stored held
	heldAt    time.Time     // when the hold was stored: the undo window starts
	undoAsked bool          // an undo came while Hold was in flight
	closing   bool          // quit once the call in flight returns

	width     int
	notice    string // the last notice, cleared by the next key
	noticeErr bool
	result    string // the outcome or error line; Result
	resultErr bool
	sent      bool // the message was sent (or applied)
}

type sayProposedMsg struct {
	p   Proposal
	err error
}

type saySentMsg struct {
	out model.MessageOutcome
	err error
}

type sayHeldMsg struct {
	msg model.Message
	err error
}

type sayUndoneMsg struct{ err error }

type sayCanceledMsg struct{ err error }

type sayTickMsg struct{}

// NewSay returns the say view. text is the message, or "" to start with the
// input line.
func NewSay(src SaySource, now func() time.Time, text string) SayModel {
	m := SayModel{src: src, now: now, tick: sayTick}
	if text != "" {
		// The Enter that ran the command counts as the last Enter, so its
		// repeats never confirm the proposal that appears at once.
		m.text, m.state, m.lastEnter = text, sayRouting, now()
	}
	return m
}

func sayTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return sayTickMsg{} })
}

// Result is the outcome line once the view has quit, and whether the
// message was sent. It is "" and false after a cancel.
func (m SayModel) Result() (line string, ok bool) {
	return m.result, m.sent
}

// Init proposes the message given to NewSay.
func (m SayModel) Init() tea.Cmd {
	if m.state == sayRouting {
		return m.proposeCmd()
	}
	return nil
}

// Update handles one message.
func (m SayModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		return m.updateKey(msg)
	case sayProposedMsg:
		return m.applyProposal(msg)
	case saySentMsg:
		return m.applySent(msg)
	case sayHeldMsg:
		return m.applyHeld(msg)
	case sayUndoneMsg:
		return m.applyUndone(msg)
	case sayCanceledMsg:
		if msg.err != nil {
			m.result, m.resultErr = "cancel not recorded: "+msg.err.Error(), true
		}
		m.state = sayDone
		return m, tea.Quit
	case sayTickMsg:
		return m.onTick()
	}
	return m, nil
}

// call runs f off the event loop with a timeout of d.
func call(d time.Duration, f func(context.Context) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return f(ctx)
	}
}

func (m SayModel) proposeCmd() tea.Cmd {
	src, text := m.src, m.text
	return call(proposeTimeout, func(ctx context.Context) tea.Msg {
		p, err := src.Propose(ctx, text)
		return sayProposedMsg{p: p, err: err}
	})
}

// updateKey handles one key. Every Enter is recorded, whatever the state,
// so a held Enter's repeats are known as repeats when a proposal appears.
func (m SayModel) updateKey(msg tea.KeyMsg) (SayModel, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m.interrupt()
	}
	repeat := msg.Type == tea.KeyEnter && m.enterRepeated()
	m.notice, m.noticeErr = "", false
	switch m.state {
	case sayInput:
		return m.inputKey(msg)
	case sayProposal:
		return m.proposalKey(msg, repeat)
	case sayHeld:
		return m.heldKey(msg)
	case sayDone:
		return m.doneKey(msg)
	case sayRouting:
		if msg.Type == tea.KeyEsc {
			return m.cancel()
		}
	}
	return m.busyKey(msg), nil
}

// enterRepeated records an Enter and reports whether it came within the
// quiet period of the Enter before it, as a held Enter's repeats do (see
// quietPeriod).
func (m *SayModel) enterRepeated() bool {
	now := m.now()
	repeated := !m.lastEnter.IsZero() && now.Sub(m.lastEnter) < quietPeriod
	m.lastEnter = now
	return repeated
}

// inputKey edits the input line: typed and pasted text is appended as
// text, Enter proposes it, Esc quits without a call. An Enter within the
// dwell of a burst of several runes is a line break of text pasted without
// bracketed paste: it is kept as a space and the burst goes on.
func (m SayModel) inputKey(msg tea.KeyMsg) (SayModel, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.state = sayDone
		return m, tea.Quit
	case tea.KeyEnter:
		if now := m.now(); now.Sub(m.burst) < dwell {
			m.burst = now
			m = m.appendText([]rune{' '})
			m.notice = "pasted line break kept as a space; enter routes"
			return m, nil
		}
		text := strings.TrimSpace(string(m.buf))
		if text == "" {
			return m, nil
		}
		m.text, m.state = text, sayRouting
		return m, m.proposeCmd()
	case tea.KeyBackspace:
		m.buf = m.buf[:max(len(m.buf)-1, 0)]
	case tea.KeyRunes, tea.KeySpace:
		if len(msg.Runes) > 1 && !msg.Paste {
			m.burst = m.now()
		}
		if !msg.Alt {
			return m.appendText(msg.Runes), nil
		}
	}
	return m, nil
}

// appendText adds typed or pasted runes to the input as text, cleaned by
// sanitize.Line (escape sequences and control characters removed, line
// breaks and tabs as spaces), keeping a space at either end, and cut at
// the room left under message.MaxText.
func (m SayModel) appendText(rs []rune) SayModel {
	room := message.MaxText - len(m.buf)
	if len(rs) == 0 {
		return m
	}
	if len(rs) > room {
		m.notice = "message limit reached"
	}
	if room <= 0 {
		return m
	}
	body := sanitize.Line(string(rs), room)
	n := utf8.RuneCountInString(body)
	if n < room && unicode.IsSpace(rs[0]) {
		body, n = " "+body, n+1
	}
	if n < room && body != "" && body != " " && unicode.IsSpace(rs[len(rs)-1]) {
		body += " "
	}
	m.buf = append(slices.Clip(m.buf), []rune(body)...)
	return m
}

// proposalKey handles a key on the proposal: Enter sends, Tab and
// Shift+Tab change the target, k the kind, Esc cancels. Pasted and typed
// text is never a command and starts the quiet period.
func (m SayModel) proposalKey(msg tea.KeyMsg, repeat bool) (SayModel, tea.Cmd) {
	k := msg.String()
	switch {
	case msg.Paste:
		return m.typed(), nil
	case k == "esc":
		return m.cancel()
	case repeat, m.now().Before(m.quietUntil):
		return m.tooSoon(), nil
	}
	switch k {
	case "enter":
		return m.confirm()
	case "tab":
		return m.cycle(1), nil
	case "shift+tab":
		return m.cycle(-1), nil
	case "k":
		return m.nextKind(), nil
	}
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		return m.typed(), nil
	}
	return m, nil
}

// typed ignores text that is no command and starts the quiet period, so
// the rest of what is typed cannot act (Ruling 88).
func (m SayModel) typed() SayModel {
	m = m.quiet()
	m.notice = noticeSayTyped
	return m
}

// quiet starts the quiet period of answeredQuiet: after typed or pasted
// text and after the outcome of a send, every key but Esc is ignored for
// it, and each ignored key restarts it (Rulings 88 and 93).
func (m SayModel) quiet() SayModel {
	m.quietUntil = m.now().Add(answeredQuiet)
	return m
}

// tooSoon ignores a key that came too soon: it restarts the dwell and a
// running quiet period, so a held, repeating key never gets through
// (Ruling 67).
func (m SayModel) tooSoon() SayModel {
	now := m.now()
	if now.Before(m.quietUntil) {
		m = m.quiet()
	}
	m.shownSince, m.notice = now, noticeTooSoon
	return m
}

// cycle shows the target d places on, and restarts the dwell.
func (m SayModel) cycle(d int) SayModel {
	if n := len(m.p.Candidates); n > 0 {
		m.target = ((m.target+d)%n + n) % n
	}
	m.shownSince = m.now()
	return m
}

// nextKind shows the next kind route.Settle accepts for the target, and
// restarts the dwell.
func (m SayModel) nextKind() SayModel {
	m.shownSince = m.now()
	c, ok := m.current()
	if !ok {
		return m
	}
	i := slices.Index(sayKinds, m.kind)
	for step := 1; step < len(sayKinds); step++ {
		k := sayKinds[(i+step+len(sayKinds))%len(sayKinds)]
		if _, err := route.Settle(k, c, m.p.Candidates); err == nil {
			m.kind = k
			return m
		}
	}
	m.notice = candLabel(c) + " takes no other kind"
	return m
}

// current is the shown target, and false when there is none.
func (m SayModel) current() (route.Candidate, bool) {
	if m.target >= len(m.p.Candidates) {
		return route.Candidate{}, false
	}
	return m.p.Candidates[m.target], true
}

// choice is the shown target and kind.
func (m SayModel) choice() Choice {
	c, _ := m.current()
	return Choice{
		Proposal: m.p, Target: c, Kind: m.kind,
		Changed: m.target != 0 || m.kind != m.p.Kind,
	}
}

// confirm sends the shown target and kind, once they have been on screen,
// unchanged, for the dwell (Ruling 66). The outcome starts the quiet
// period that absorbs what is typed or held on after a send (Ruling 93).
func (m SayModel) confirm() (SayModel, tea.Cmd) {
	if m.now().Sub(m.shownSince) < dwell {
		return m.tooSoon(), nil
	}
	if _, ok := m.current(); !ok {
		m.notice = "no target to send to; esc cancels"
		return m, nil
	}
	m.state = saySending
	src, c := m.src, m.choice()
	return m, call(answerTimeout, func(ctx context.Context) tea.Msg {
		out, err := src.Send(ctx, c)
		return saySentMsg{out: out, err: err}
	})
}

// cancel records the message as canceled; the view quits when it is.
func (m SayModel) cancel() (SayModel, tea.Cmd) {
	m.state = sayCanceling
	src, text := m.src, m.text
	return m, call(answerTimeout, func(ctx context.Context) tea.Msg {
		return sayCanceledMsg{err: src.Cancel(ctx, text)}
	})
}

// busyKey handles a key while a call is in flight: nothing acts but u or
// Esc while the hold is being stored (an undo once it is). Text typed or
// pasted starts the quiet period, so it cannot act on the proposal when it
// appears.
func (m SayModel) busyKey(msg tea.KeyMsg) SayModel {
	switch {
	case m.state == sayHolding && isUndoKey(msg):
		m.undoAsked = true
	case m.now().Before(m.quietUntil):
		m = m.quiet()
	case msg.Paste || msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace:
		m = m.typed()
	}
	return m
}

// isUndoKey reports whether msg asks to undo a hold: Esc, or a typed (not
// pasted) u.
func isUndoKey(msg tea.KeyMsg) bool {
	if msg.Paste {
		return false
	}
	return msg.Type == tea.KeyEsc || (msg.Type == tea.KeyRunes && !msg.Alt && slices.Contains(msg.Runes, 'u'))
}

// heldKey handles a key during the undo window: u or Esc undoes, every
// other key is ignored.
func (m SayModel) heldKey(msg tea.KeyMsg) (SayModel, tea.Cmd) {
	if isUndoKey(msg) {
		return m.undo()
	}
	return m, nil
}

// expired reports whether the undo window has run out.
func (m SayModel) expired() bool {
	return !m.now().Before(m.heldAt.Add(SayHold))
}

// undo undoes the held message, unless the window has run out: then
// nothing can cancel it and it is released.
func (m SayModel) undo() (SayModel, tea.Cmd) {
	if m.expired() {
		var cmd tea.Cmd
		m, cmd = m.release()
		m.notice = noticeTooLate
		return m, cmd
	}
	m.state = sayUndoing
	src, id := m.src, m.held.ID
	return m, call(answerTimeout, func(ctx context.Context) tea.Msg {
		return sayUndoneMsg{err: src.Undo(ctx, id)}
	})
}

// release sends the held message.
func (m SayModel) release() (SayModel, tea.Cmd) {
	m.state = sayReleasing
	src, id := m.src, m.held.ID
	return m, call(answerTimeout, func(ctx context.Context) tea.Msg {
		out, err := src.Release(ctx, id)
		return saySentMsg{out: out, err: err}
	})
}

// onTick releases the held message once the window has run out, and
// otherwise schedules the next tick for the next whole second left.
func (m SayModel) onTick() (SayModel, tea.Cmd) {
	if m.state != sayHeld {
		return m, nil
	}
	if m.expired() {
		return m.release()
	}
	left := m.heldAt.Add(SayHold).Sub(m.now())
	next := left % time.Second
	if next == 0 {
		next = time.Second
	}
	return m, m.tick(next)
}

// doneKey handles a key once the outcome is shown: Esc closes; q closes
// when it comes the quiet period after the last key, so nothing typed or
// held on after a send closes the view (Ruling 93); every other key
// restarts that period.
func (m SayModel) doneKey(msg tea.KeyMsg) (SayModel, tea.Cmd) {
	if msg.Type == tea.KeyEsc {
		return m, tea.Quit
	}
	quiet := m.now().Before(m.quietUntil)
	if !quiet && !msg.Paste && msg.String() == "q" {
		return m, tea.Quit
	}
	m = m.quiet()
	if quiet && msg.String() == "q" {
		m.notice = noticeTooSoon
	}
	return m, nil
}

// interrupt is Ctrl+C: it cancels a message not yet sent, undoes a held one
// and quits; with a call in flight it quits once the call returns.
func (m SayModel) interrupt() (SayModel, tea.Cmd) {
	switch m.state {
	case sayInput, sayDone:
		return m, tea.Quit
	case sayRouting, sayProposal:
		return m.cancel()
	case sayHolding:
		m.undoAsked = true
	case sayHeld:
		m.closing = true
		return m.undo()
	}
	m.closing = true
	return m, nil
}

// applyProposal shows the proposal, or holds it at once when it passed
// every auto-send guard. A proposal that comes after a cancel is dropped.
func (m SayModel) applyProposal(msg sayProposedMsg) (SayModel, tea.Cmd) {
	if m.state != sayRouting {
		return m, nil
	}
	if msg.err != nil {
		return m.finish("routing failed: "+msg.err.Error(), true), nil
	}
	m.p, m.target, m.kind = msg.p, 0, msg.p.Kind
	m.shownSince, m.state = m.now(), sayProposal
	if !m.p.Auto {
		return m, nil
	}
	if _, ok := m.current(); !ok {
		return m.finish("no target to send to", true), nil
	}
	m.state = sayHolding
	src, c := m.src, m.choice()
	return m, call(answerTimeout, func(ctx context.Context) tea.Msg {
		held, err := src.Hold(ctx, c)
		return sayHeldMsg{msg: held, err: err}
	})
}

// applyHeld starts the undo window, or undoes at once when asked while the
// hold was being stored.
func (m SayModel) applyHeld(msg sayHeldMsg) (SayModel, tea.Cmd) {
	if msg.err != nil {
		m = m.finish("hold failed: "+msg.err.Error(), true)
		if m.closing {
			return m, tea.Quit
		}
		return m, nil
	}
	m.held, m.heldAt, m.state = msg.msg, m.now(), sayHeld
	if m.undoAsked {
		return m.undo()
	}
	return m, m.tick(time.Second)
}

// applySent shows a send's or a release's outcome. A refused send offers
// the proposal again after the quiet period; a failed release is final.
func (m SayModel) applySent(msg saySentMsg) (SayModel, tea.Cmd) {
	switch {
	case msg.err != nil && m.state == saySending:
		m.closing = false
		m.state, m.shownSince = sayProposal, m.now()
		m = m.quiet()
		m.notice, m.noticeErr = "send failed: "+msg.err.Error(), true
		return m, nil
	case msg.err != nil:
		m = m.finish("release failed: "+msg.err.Error(), true)
	default:
		m = m.finish(m.outcomeLine(msg.out), false)
		m.sent = true
	}
	if m.closing {
		return m, tea.Quit
	}
	return m, nil
}

// applyUndone shows an undo's result.
func (m SayModel) applyUndone(msg sayUndoneMsg) (SayModel, tea.Cmd) {
	if msg.err != nil {
		m = m.finish("undo failed: "+msg.err.Error(), true)
	} else {
		m = m.finish(sanitize.Line(m.held.ID, maxIDCells)+" undone", false)
	}
	if m.closing {
		return m, tea.Quit
	}
	return m, nil
}

// finish shows line as the view's last word and starts the quiet period.
func (m SayModel) finish(line string, isErr bool) SayModel {
	m.state = sayDone
	m.result, m.resultErr = sanitize.Line(line, maxNote), isErr
	return m.quiet()
}

// outcomeLine is "M-7 instruction → <label> · delivered", with "applied"
// and the result for a rule or task, and the outcome's note in place of
// either when there is one.
func (m SayModel) outcomeLine(out model.MessageOutcome) string {
	what := "delivered"
	if out.Message.DeliveredVia == model.ViaApplied {
		what = strings.TrimSpace("applied " + sanitize.Line(out.Message.ResultRef, maxIDCells))
	}
	if out.Note != "" {
		what = sanitize.Line(out.Note, maxNote)
	}
	c, _ := m.current()
	head := strings.TrimSpace(sanitize.Line(out.Message.ID, maxIDCells) + " " + kindText(m.kind))
	return head + " → " + candLabel(c) + " · " + what
}

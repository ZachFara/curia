package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// answerTimeout bounds one answer: the store write and, when the session is
// idle, typing it into the pane.
const answerTimeout = 5 * time.Second

// noticeChanged is shown when an answer key is ignored because the ticket
// on screen changed.
const noticeChanged = "queue changed; press again"

// noticeTooSoon is shown when a key is ignored because it came too soon:
// before the ticket on screen or the open input was there for the dwell,
// during the quiet period, or as a repeat of Enter (Ruling 88, defect 11:
// nothing changed, so it must not say the queue did).
const noticeTooSoon = "too soon; pause, then press again"

// noticeTyped is shown when typed text that is no command is ignored; it
// starts the quiet period, so the rest of the text cannot act (Ruling 88).
const noticeTyped = "typing ignored; press w to write an answer, r to give a reason"

// quietPeriod is how long answer keys are ignored after one's own answer
// (Ruling 63) and after an answer key refused for a change the user did not
// cause (Ruling 68), so a key pressed twice or held never answers the next
// ticket unread. Each ignored key restarts it. A terminal sends no key-up,
// so a held key is told from presses by time alone: the period is longer
// than the usual delay before a held key starts repeating (375 to 660 ms).
// Accepted residue: a keyboard set to a repeat delay above 750 ms can still
// get a repeat through.
const quietPeriod = 750 * time.Millisecond

// dwell is how long a ticket must have been on screen before an answer key
// acts on it, and a confirmation open before "y" gives it (Ruling 66). The
// terminal is written on a frame ticker, not after every message, so keys
// that arrive together are handled before anything new is drawn.
const dwell = 250 * time.Millisecond

// DecideSource is where the decide view reads open tickets, already in queue
// order, and writes an answer. The command's implementation expires, lists,
// sorts and delivers.
type DecideSource interface {
	Tickets(ctx context.Context) ([]model.Ticket, error)
	Answer(ctx context.Context, id string, a model.Answer) (model.Outcome, error)
}

// DecideOptions configure the decide view.
type DecideOptions struct {
	SessionID string // not empty: only this session's tickets
	Embedded  bool   // inside the dashboard: closing sends DecideClosedMsg instead of quitting
	Via       model.AnsweredVia
}

// DecideClosedMsg is sent when an embedded decide view closes.
type DecideClosedMsg struct{}

// inputKind is what the text being typed is for.
type inputKind int

const (
	inputNone   inputKind = iota
	inputText             // a free-text answer
	inputReason           // reasoning to stage for the next answer
	inputWhy              // reasoning for an answer that waits in pending
)

// pendingAnswer is an answer waiting for its reasoning, its text or a
// confirmation. ticket is the ticket as it was on screen when the answer
// began; the answer is sent only while that same ticket is still selected.
type pendingAnswer struct {
	ticket model.Ticket
	a      model.Answer
}

// DecideModel is the decide view's Bubble Tea model.
type DecideModel struct {
	src  DecideSource
	now  func() time.Time
	opts DecideOptions

	tickets  []model.Ticket // the last good read, filtered by session, in source order
	cursor   int            // index of the selected ticket
	selected string         // id of the selected ticket, kept across refreshes
	offset   int            // index of the first queue line on screen
	loaded   bool           // a read has succeeded
	changed  bool           // a refresh changed the ticket on screen: the next answer key is ignored

	quietUntil   time.Time // answer keys before this are ignored (after one's own answer)
	shownSince   time.Time // when the selected ticket, as it is now, came on screen
	promptSince  time.Time // when the confirmation opened
	inputSince   time.Time // when the input opened, or last refused a too early Enter
	inputByEnter bool      // an Enter opened the reasoning input
	lastEnter    time.Time // when the last Enter came, acted on or not

	issued, applied int // sequence numbers of the last refresh started and applied

	width, height int // 0 until the first WindowSizeMsg: no limit

	reasoning string      // staged for the next answer
	scope     model.Scope // staged for the next answer; "" keeps the ticket's
	draft     string      // the free text of a refused answer, offered again by "w"

	input      inputKind
	buf        []rune
	pending    pendingAnswer
	confirming bool // pending waits for "y"
	sending    bool // an answer is in flight
	closing    bool // close once the answer in flight returns, unless it has a note
	quitting   bool // closing was asked by Ctrl+C: quit the program, even embedded

	notice    string // the last outcome or message, cleared by the next key
	noticeErr bool   // notice is an error
	err       error  // the last refresh's error, cleared by a good one
}

// decideRefreshMsg is the result of one read of the source. Only the
// refreshes of the one-second chain (tick) schedule the next.
type decideRefreshMsg struct {
	tickets []model.Ticket
	err     error
	seq     int
	tick    bool
}

// decideTickMsg starts the next refresh of the chain.
type decideTickMsg struct{}

// decideAnsweredMsg is the result of one answer.
type decideAnsweredMsg struct {
	id     string
	ticket model.Ticket // as it was on screen when the answer was sent
	a      model.Answer
	out    model.Outcome
	err    error
}

// NewDecide returns the decide view over src; now is the clock for ages and
// the stakes line.
func NewDecide(src DecideSource, now func() time.Time, opts DecideOptions) DecideModel {
	return DecideModel{src: src, now: now, opts: opts}
}

// Init starts the first refresh and with it the one-second chain.
func (m DecideModel) Init() tea.Cmd {
	return m.refreshCmd(m.issued, true)
}

// refresh starts a read of the source.
func (m DecideModel) refresh(tick bool) (DecideModel, tea.Cmd) {
	m.issued++
	return m, m.refreshCmd(m.issued, tick)
}

func (m DecideModel) refreshCmd(seq int, tick bool) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		tickets, err := src.Tickets(ctx)
		return decideRefreshMsg{tickets: tickets, err: err, seq: seq, tick: tick}
	}
}

// Update handles one message.
func (m DecideModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case decideRefreshMsg:
		m = m.applyRefresh(msg)
		if msg.tick {
			return m, tea.Tick(refreshEvery, func(time.Time) tea.Msg { return decideTickMsg{} })
		}
		return m, nil
	case decideTickMsg:
		return m.refresh(true)
	case decideAnsweredMsg:
		return m.applyAnswer(msg)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m.moveTo(m.cursor), nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			return m.updateRunes(msg)
		}
		return m.updateKey(msg, false)
	}
	return m, nil
}

// applyRefresh takes a good read that is not older than the one shown, and
// keeps the selection on the same ticket. When the ticket on screen is gone
// or changed, the next answer key is ignored and the staged reasoning and
// scope are dropped.
func (m DecideModel) applyRefresh(msg decideRefreshMsg) DecideModel {
	if msg.seq < m.applied {
		return m
	}
	m.applied, m.err = msg.seq, msg.err
	if msg.err != nil {
		return m
	}
	before, had := m.current()
	m.tickets = nil // a fresh slice: earlier copies of the model keep theirs
	for _, t := range msg.tickets {
		if m.opts.SessionID == "" || t.SessionID == m.opts.SessionID {
			m.tickets = append(m.tickets, t)
		}
	}
	m = m.reselect()
	if m.loaded && m.isNewOnScreen(before, had) {
		m.changed = true
		m.reasoning, m.scope, m.draft = "", "", ""
	}
	m.loaded = true
	return m.markShown(before, had)
}

// isNewOnScreen reports whether the selected ticket differs from before
// (had says whether there was one).
func (m DecideModel) isNewOnScreen(before model.Ticket, had bool) bool {
	after, has := m.current()
	return had != has || (has && !sameTicket(before, after))
}

// markShown restarts the dwell when the selected ticket differs from
// before, whatever changed it.
func (m DecideModel) markShown(before model.Ticket, had bool) DecideModel {
	if m.isNewOnScreen(before, had) {
		m.shownSince = m.now()
	}
	return m
}

// reselect puts the cursor back on the selected ticket or, when it is
// gone, on the ticket nearest its old place.
func (m DecideModel) reselect() DecideModel {
	if i := slices.IndexFunc(m.tickets, func(t model.Ticket) bool { return t.ID == m.selected }); i >= 0 {
		m.cursor = i
	}
	return m.moveTo(m.cursor)
}

// ticketFace is what the view shows of a ticket and an answer depends on.
type ticketFace struct {
	ID, SessionID, Repo               string
	Question, Context, Recommendation string
	Rationale, Assumption             string
	DiffersFrom, DiffersWhy           string
	Reversibility                     model.Reversibility
	Mode                              model.Mode
	Scope                             model.Scope
}

func face(t model.Ticket) ticketFace {
	return ticketFace{
		ID: t.ID, SessionID: t.SessionID, Repo: t.Repo,
		Question: t.Question, Context: t.Context, Recommendation: t.Recommendation,
		Rationale: t.Rationale, Assumption: t.Assumption,
		DiffersFrom: t.DiffersFrom, DiffersWhy: t.DiffersWhy,
		Reversibility: t.Reversibility, Mode: t.Mode, Scope: t.Scope,
	}
}

// sameTicket reports whether a and b are the same ticket with the same text.
func sameTicket(a, b model.Ticket) bool {
	return face(a) == face(b) && slices.Equal(a.Options, b.Options)
}

// moveTo selects ticket i, clamped to the queue, and scrolls it into view.
func (m DecideModel) moveTo(i int) DecideModel {
	m.cursor = max(min(i, len(m.tickets)-1), 0)
	m.selected = ""
	if len(m.tickets) > 0 {
		m.selected = m.tickets[m.cursor].ID
	}
	if n := m.queueRows(); n > 0 {
		m.offset = min(m.offset, m.cursor)
		m.offset = max(m.offset, m.cursor-n+1)
		m.offset = max(min(m.offset, len(m.tickets)-n), 0)
	}
	return m
}

// queueRows is how many queue lines are shown: about a quarter of the
// window, at least one; 0 means no limit (the height is not known yet).
func (m DecideModel) queueRows() int {
	if m.height <= 0 {
		return 0
	}
	return max((m.height-2)/4, 1)
}

// current returns the selected ticket, and false when the queue is empty.
func (m DecideModel) current() (model.Ticket, bool) {
	if len(m.tickets) == 0 {
		return model.Ticket{}, false
	}
	return m.tickets[m.cursor], true
}

// updateRunes handles keys typed faster than the program reads them, which
// arrive as one message with several runes, one rune at a time. A rune that
// opens or closes a prompt drops the rest: the user has not seen the prompt
// yet.
func (m DecideModel) updateRunes(msg tea.KeyMsg) (DecideModel, tea.Cmd) {
	var cmds []tea.Cmd
	for _, r := range msg.Runes {
		was := m.prompting()
		var cmd tea.Cmd
		m, cmd = m.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}, true)
		cmds = append(cmds, cmd)
		if m.prompting() != was {
			break
		}
	}
	return m, tea.Batch(cmds...)
}

// prompting reports whether a confirmation or an input is open.
func (m DecideModel) prompting() bool {
	return m.confirming || m.input != inputNone
}

// updateKey handles one key; split is set for a rune taken from a message
// of several.
func (m DecideModel) updateKey(msg tea.KeyMsg, split bool) (DecideModel, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		if m.sending { // M1: quitting now could cut a delivery short
			m.closing, m.quitting = true, true
			return m, nil
		}
		return m, tea.Quit
	}
	m.notice, m.noticeErr = "", false
	if msg.Type == tea.KeyEnter && m.enterRepeated() {
		return m.tooSoon(), nil
	}
	switch {
	case m.confirming:
		return m.confirmKey(msg, split)
	case m.input != inputNone:
		return m.inputKey(msg)
	case msg.Paste: // pasted text is never a command
		return m, nil
	}
	return m.commandKey(msg)
}

// enterRepeated records an Enter and reports whether it came within the
// quiet period of the Enter before it, as a held or doubled Enter does: no
// key-up reaches a terminal program, so a gap is the only sign it was let
// go, and a held key's first repeat comes after the keyboard's repeat
// delay, up to 660 ms (Ruling 88, defect 5; see quietPeriod).
func (m *DecideModel) enterRepeated() bool {
	now := m.now()
	repeated := !m.lastEnter.IsZero() && now.Sub(m.lastEnter) < quietPeriod
	m.lastEnter = now
	return repeated
}

// tooSoon ignores a key that came too soon: it restarts the dwell of the
// ticket and of an open input, so a repeating key never gets through, and
// it extends a running quiet period.
func (m DecideModel) tooSoon() DecideModel {
	now := m.now()
	if now.Before(m.quietUntil) {
		m.quietUntil = now.Add(quietPeriod)
	}
	m.inputSince = now
	return m.refuse(noticeTooSoon)
}

func (m DecideModel) commandKey(msg tea.KeyMsg) (DecideModel, tea.Cmd) {
	switch k := msg.String(); k {
	case "q", "esc":
		if m.sending { // quitting now could cut a delivery short
			m.closing = true
			return m, nil
		}
		return m, m.close()
	case "j", "down":
		return m.move(m.cursor + 1), nil
	case "k", "up":
		return m.move(m.cursor - 1), nil
	case "1", "2", "3", "4":
		return m.answerOption(int(k[0] - '1'))
	case "enter":
		return m.answerRecommended()
	case "w":
		return m.openText(), nil
	case "r", "s":
		// Both stage part of the next answer, so they wait like an answer
		// key (defect 4: typed text changed a ticket's scope).
		var ok bool
		if m, ok = m.guard(); !ok {
			return m, nil
		}
		if k == "r" {
			return m.openReason(), nil
		}
		m.scope = nextScope(m.scope)
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			return m.typedText(), nil
		}
	}
	return m, nil
}

// typedText ignores a key that is no command, as when the user types a
// reason into the queue out of habit, and starts the quiet period, so the
// rest of what is typed cannot answer or stage anything (Ruling 88, defect
// 4). Each key restarts it, as an ignored answer key does.
func (m DecideModel) typedText() DecideModel {
	m.quietUntil = m.now().Add(quietPeriod)
	return m.refuse(noticeTyped)
}

// close quits, or when embedded tells the dashboard the view closed.
func (m DecideModel) close() tea.Cmd {
	if m.opts.Embedded {
		return func() tea.Msg { return DecideClosedMsg{} }
	}
	return tea.Quit
}

// move selects ticket i. Moving is the user choosing what is on screen, so
// it clears a queue change; leaving a ticket drops what was staged for it.
func (m DecideModel) move(i int) DecideModel {
	before, had := m.current()
	if m = m.moveTo(i); m.selected == before.ID {
		return m // at the edge of the queue: nothing moved, nothing cleared
	}
	m.changed, m.quietUntil = false, time.Time{}
	m.reasoning, m.scope, m.draft = "", "", ""
	return m.markShown(before, had)
}

// scopeCycle is the order "s" steps through; "" is the ticket's own scope.
var scopeCycle = []model.Scope{"", model.ScopeTask, model.ScopeRepo, model.ScopeGlobal}

func nextScope(s model.Scope) model.Scope {
	return scopeCycle[(slices.Index(scopeCycle, s)+1)%len(scopeCycle)]
}

// answerOption answers with option i, when the ticket has it and it is
// drawn.
func (m DecideModel) answerOption(i int) (DecideModel, tea.Cmd) {
	t, ok := m.current()
	if !ok || i >= len(t.Options) {
		return m, nil
	}
	if i >= m.optionsShown() {
		return m.refuse(fmt.Sprintf("option %d not shown: window too short", i+1)), nil
	}
	return m.startAnswer(t, model.Answer{Key: t.Options[i].Key})
}

// answerRecommended answers with the recommended option, when it is drawn.
func (m DecideModel) answerRecommended() (DecideModel, tea.Cmd) {
	t, ok := m.current()
	if !ok {
		return m, nil
	}
	i := slices.IndexFunc(t.Options, func(o model.Option) bool { return o.Key == t.Recommendation })
	if i < 0 {
		return m, nil
	}
	if i >= m.optionsShown() {
		return m.refuse("enter=" + sanitize.Line(t.Recommendation, decision.MaxKey) + " not shown: window too short"), nil
	}
	return m.startAnswer(t, model.Answer{Key: t.Recommendation})
}

// guard reports whether an answer may start now. It refuses while an answer
// is in flight; during the quiet period, restarting it; once after a refresh
// changed the ticket on screen, using up the flag and starting the quiet
// period (refuseChanged); and before the ticket on screen has been there
// for the dwell. Every refusal restarts the dwell (refuse).
func (m DecideModel) guard() (DecideModel, bool) {
	now := m.now()
	switch {
	case m.sending:
		return m.refuse("still sending the last answer"), false
	case now.Before(m.quietUntil):
		m.quietUntil = now.Add(quietPeriod)
		return m.refuse(noticeTooSoon), false
	case m.changed:
		return m.refuseChanged(), false
	case now.Sub(m.shownSince) < dwell:
		return m.refuse(noticeTooSoon), false
	}
	return m, true
}

// refuse ignores an answer key with notice and restarts the dwell, so a
// held, repeating key never gets through: it must be let go for the dwell
// first (Ruling 67).
func (m DecideModel) refuse(notice string) DecideModel {
	m.notice, m.shownSince = notice, m.now()
	return m
}

// refuseChanged ignores an answer key that ran into a change the user did
// not cause. It uses up the change flag and starts the quiet period, so the
// repeats of a held key are ignored however late the first one comes
// (Ruling 68; see quietPeriod for what is left).
func (m DecideModel) refuseChanged() DecideModel {
	m.changed, m.quietUntil = false, m.now().Add(quietPeriod)
	return m.refuse(noticeChanged)
}

// startAnswer begins answering t with a's key. A blocking ticket with no
// staged reasoning first asks for reasoning.
func (m DecideModel) startAnswer(t model.Ticket, a model.Answer) (DecideModel, tea.Cmd) {
	m, ok := m.guard()
	if !ok {
		return m, nil
	}
	a.Reasoning, a.Scope, a.Via = m.reasoning, m.scope, m.opts.Via
	p := pendingAnswer{ticket: t, a: a}
	if t.Mode == model.ModeBlocking && a.Reasoning == "" {
		return m.askWhy(p), nil
	}
	return m.proceed(p)
}

// askWhy opens the reasoning input for p, which waits in pending.
func (m DecideModel) askWhy(p pendingAnswer) DecideModel {
	m.pending, m.input, m.buf, m.inputSince = p, inputWhy, nil, m.now()
	m.inputByEnter = m.lastEnter.Equal(m.inputSince)
	return m
}

// openText opens the free-text answer input for the selected ticket.
func (m DecideModel) openText() DecideModel {
	t, ok := m.current()
	if !ok {
		return m
	}
	m, ok = m.guard()
	if !ok {
		return m
	}
	a := model.Answer{Reasoning: m.reasoning, Scope: m.scope, Via: m.opts.Via}
	m.pending, m.input, m.buf = pendingAnswer{ticket: t, a: a}, inputText, []rune(m.draft)
	m.inputSince, m.inputByEnter = m.now(), false
	return m
}

// openReason opens the reasoning input for the selected ticket, holding
// what is staged already.
func (m DecideModel) openReason() DecideModel {
	if t, ok := m.current(); ok {
		m.pending, m.input, m.buf = pendingAnswer{ticket: t}, inputReason, []rune(m.reasoning)
		m.inputSince, m.inputByEnter = m.now(), false
	}
	return m
}

// inputKey edits the text being typed: typed and pasted text is appended up
// to the input's cap, Backspace deletes, Enter submits, Esc cancels.
func (m DecideModel) inputKey(msg tea.KeyMsg) (DecideModel, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m.cancelInput(), nil
	case tea.KeyEnter:
		return m.submitInput()
	case tea.KeyBackspace:
		m.buf = m.buf[:max(len(m.buf)-1, 0)]
	case tea.KeyRunes, tea.KeySpace:
		room := max(m.inputCap()-len(m.buf), 0)
		m.buf = append(slices.Clip(m.buf), msg.Runes[:min(len(msg.Runes), room)]...)
	}
	return m, nil
}

// inputCap is the most runes the open input takes.
func (m DecideModel) inputCap() int {
	if m.input == inputText {
		return decision.MaxAnswerText
	}
	return decision.MaxReasoning
}

func (m DecideModel) cancelInput() DecideModel {
	if m.input != inputReason {
		if m.pending.a.Text != "" {
			m.draft = m.pending.a.Text // "w" offers it again
		}
		m.pending, m.notice = pendingAnswer{}, "answer canceled"
	}
	m.input, m.buf = inputNone, nil
	return m
}

// submitInput takes what was typed, on an Enter that came the dwell after
// the input opened, or the quiet period after it when an Enter opened it (a
// held Enter's first repeat must not submit it, Ruling 88). A free-text
// answer to a blocking ticket with no staged reasoning then asks for the
// reasoning, as an option answer does (Ruling 88, defect 4: a reason typed
// after it was read as commands).
func (m DecideModel) submitInput() (DecideModel, tea.Cmd) {
	wait := dwell
	if m.inputByEnter {
		wait = quietPeriod
	}
	if m.now().Sub(m.inputSince) < wait {
		return m.tooSoon(), nil
	}
	text := strings.TrimSpace(string(m.buf))
	p := m.pending
	switch m.input {
	case inputReason:
		if t, ok := m.current(); !ok || !sameTicket(t, p.ticket) {
			m = m.refuseChanged() // typed for a ticket no longer on screen
			break
		}
		m.reasoning = text
	case inputWhy:
		p.a.Reasoning = text
	case inputText:
		if text == "" {
			return m, nil // nothing to send yet
		}
		p.a.Text = text
	}
	kind := m.input
	m.input, m.buf, m.pending = inputNone, nil, pendingAnswer{}
	switch {
	case kind == inputReason:
		return m, nil
	case kind == inputText && p.ticket.Mode == model.ModeBlocking && p.a.Reasoning == "":
		return m.askWhy(p), nil
	}
	return m.proceed(p)
}

// proceed asks to confirm an answer to an irreversible ticket and sends any
// other at once.
func (m DecideModel) proceed(p pendingAnswer) (DecideModel, tea.Cmd) {
	if p.ticket.Reversibility == model.ReversIrreversible {
		m.pending, m.confirming, m.promptSince = p, true, m.now()
		return m, nil
	}
	return m.send(p)
}

// confirmKey sends the pending answer on a "y" that arrived as a message of
// its own, the dwell after the prompt opened; any other key, a paste, a "y"
// within a burst of keys and a "y" too soon cancel it.
func (m DecideModel) confirmKey(msg tea.KeyMsg, split bool) (DecideModel, tea.Cmd) {
	p := m.pending
	m.pending, m.confirming = pendingAnswer{}, false
	early := m.now().Sub(m.promptSince) < dwell
	if split || early || msg.Paste || msg.String() != "y" {
		m.notice = "answer canceled"
		return m, nil
	}
	return m.send(p)
}

// send starts p's answer, but only while p's ticket is still the one on
// screen; otherwise the answer is dropped with the queue-change notice.
func (m DecideModel) send(p pendingAnswer) (DecideModel, tea.Cmd) {
	if t, ok := m.current(); !ok || !sameTicket(t, p.ticket) {
		return m.refuseChanged(), nil
	}
	m.sending = true
	src, tk, a := m.src, p.ticket, p.a
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), answerTimeout)
		defer cancel()
		out, err := src.Answer(ctx, tk.ID, a)
		return decideAnsweredMsg{id: tk.ID, ticket: tk, a: a, out: out, err: err}
	}
}

// applyAnswer handles an answer's result. A refused answer is shown, a
// close asked for meanwhile is dropped, and what was typed is kept while
// the same ticket is on screen. An accepted one closes when asked, or
// leaves the queue at once, starts the quiet period (whatever a read did
// to the selection meanwhile), and re-reads the queue; reads started
// before it returned are out of date and dropped.
func (m DecideModel) applyAnswer(msg decideAnsweredMsg) (DecideModel, tea.Cmd) {
	m.sending = false
	if msg.err != nil {
		m.closing, m.quitting = false, false
		m.notice, m.noticeErr = "answer failed: "+msg.err.Error(), true
		if t, ok := m.current(); ok && sameTicket(t, msg.ticket) {
			m.reasoning, m.scope, m.draft = msg.a.Reasoning, msg.a.Scope, msg.a.Text
		}
		return m.refresh(false)
	}
	// M2: an outcome with a note (not delivered, or not for certain) is
	// shown before the view closes.
	if m.closing && msg.out.Note == "" {
		if m.quitting {
			return m, tea.Quit
		}
		return m, m.close()
	}
	m.closing, m.quitting = false, false
	m.notice, m.noticeErr = outcomeText(msg), undelivered(msg.out)
	m.reasoning, m.scope, m.draft = "", "", ""
	before, had := m.current()
	m.tickets = slices.DeleteFunc(slices.Clone(m.tickets), func(t model.Ticket) bool { return t.ID == msg.id })
	m = m.reselect().markShown(before, had)
	m.quietUntil = m.now().Add(quietPeriod)
	m, cmd := m.refresh(false)
	m.applied = m.issued
	return m, cmd
}

package ui

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

const (
	// reviewDetailEvery is how often the shown review's detail is read again.
	reviewDetailEvery = 5 * time.Second
	// reviewDetailTimeout bounds one Detail: it runs git diff --stat.
	reviewDetailTimeout = 30 * time.Second
	// reviewActionTimeout bounds one approve, change request or drop: a
	// merge may take 2 minutes, a push 2 more and gh pr create one.
	reviewActionTimeout = 6 * time.Minute
)

const (
	// noticeReviewChanged cancels a prompt or the note input whose review
	// changed or left the queue, and refuses the next key after it.
	noticeReviewChanged = "the review changed; read it again"
	// noticeReviewReading refuses an action key before the shown review's
	// detail has been read.
	noticeReviewReading = "still reading the review; press again once it is shown"
	// noticeReviewTyped is shown when typed or pasted text that is no
	// command is ignored; it starts the quiet period (Ruling 88).
	noticeReviewTyped = "typing ignored; press c to write a change request"
)

// ReviewRow is one waiting review in the queue.
type ReviewRow struct {
	Review model.Review
	Task   model.Task
}

// ReviewDetail is everything the view shows about one review. Every string in it is untrusted.
type ReviewDetail struct {
	Review      model.Review
	Task        model.Task
	DiffStat    string           // "" when there is none to show
	DiffNote    string           // a warning or the reason there is no diff; "" when none
	Head        string           // the full commit id of the branch the diff was built from; "" when none
	Assumptions []model.Decision // open assume tickets of the task
	Blocking    []model.Decision // open blocking tickets of the task
	Decided     []model.Decision
	Sessions    int
	Teammates   int
	Duration    time.Duration
	CostUSD     float64
	CostKnown   bool
	Approve     model.ApproveHow // what the a key will do
}

// ReviewSource is where the view reads and acts. The command's implementation runs git.
//
// Approve is given the Head of the detail that was on screen when the
// prompt opened, so it approves only what was shown: it refuses, leaving
// the review pending, when the branch is no longer at that commit.
// Approve, RequestChanges and Drop return the outcome line on success,
// which means the review is no longer pending. An error means the review
// is still pending (a refusal, or a failure that changed nothing the
// review records); the view shows it and keeps the review. The context of
// DiffCmd bounds building the command only and is canceled when the tool
// exits: the command must not be bound to it with exec.CommandContext
// unless it may live as long as the tool.
type ReviewSource interface {
	Queue(ctx context.Context) ([]ReviewRow, error)                              // pending reviews, oldest first; no git
	Detail(ctx context.Context, reviewID string) (ReviewDetail, error)           // may take seconds
	Approve(ctx context.Context, reviewID, head string) (line string, err error) // may take minutes
	RequestChanges(ctx context.Context, reviewID, note string) (line string, err error)
	Drop(ctx context.Context, reviewID string) (line string, err error)
	DiffCmd(ctx context.Context, reviewID string) (*exec.Cmd, error) // the configured diff tool
}

// ReviewOptions configure the review view.
type ReviewOptions struct {
	TaskID   string // not empty: only this task's pending review
	Embedded bool   // inside the dashboard: closing sends ReviewClosedMsg instead of quitting
}

// ReviewClosedMsg is sent when an embedded review view closes.
type ReviewClosedMsg struct{}

// reviewMode is what the keys go to.
type reviewMode int

const (
	reviewBrowse         reviewMode = iota // reading, moving, scrolling
	reviewConfirmApprove                   // the approve prompt waits for y
	reviewConfirmDrop                      // the drop prompt waits for y
	reviewNote                             // typing a change request
)

// ReviewModel is the review view's Bubble Tea model.
type ReviewModel struct {
	src  ReviewSource
	now  func() time.Time
	opts ReviewOptions
	tick func(time.Duration, tea.Msg) tea.Cmd      // schedules a refresh
	run  func(*exec.Cmd, tea.ExecCallback) tea.Cmd // runs the diff tool on the terminal

	rows            []ReviewRow // the last good queue read, filtered by task
	cursor          int
	selected        string // id of the shown review, kept across refreshes
	loaded          bool   // a queue read has succeeded
	err             error  // the last queue read's error
	issued, applied int    // sequence numbers of the last queue read started and applied

	detail    ReviewDetail // the last good detail read
	hasDetail bool
	detailErr error     // the last detail read's error, for the shown review
	asking    bool      // a Detail is in flight (one at a time)
	askedID   string    // what it was asked for
	askedAt   time.Time // the shown row's UpdatedAt when it was asked

	shownSince time.Time // when what is on screen (face) came, or a refused key restarted the dwell
	changed    bool      // a prompt or the note was canceled by a change: refuse the next key
	quietUntil time.Time // keys before this are ignored, and each restarts it
	quietFor   time.Duration
	toolQuiet  bool // the running quiet period follows the diff tool: it holds q and Esc too

	mode        reviewMode
	pendingID   string // the review the prompt or the note input opened on
	pendingFace string // and what was on screen then
	pending     ReviewDetail
	promptSince time.Time     // when the prompt opened, or a y too early restarted its wait
	promptWait  time.Duration // dwell, then quietPeriod after a y too early
	inputSince  time.Time     // when the note input opened, or an Enter too early restarted it
	lastEnter   time.Time     // when the last Enter came, acted on or not
	burst       time.Time     // when the last burst of several typed runes came into the note
	buf         []rune        // the note being typed
	draftID     string        // the review draft was typed for
	draft       []rune        // a note not sent: canceled by a change, or refused

	busy       bool // an action is in flight
	closeAsked bool // a close key came while it was
	diffing    bool // the diff tool is being prepared or runs

	notice    string // the last notice, cleared by the next key
	noticeErr bool
	result    string // the last action's line or error, kept until the next action
	resultErr bool
	results   []string // every action's line or error, in order (Result)

	width, height int // 0 until the first WindowSizeMsg
	offset        int // first body line on screen
	body          []bodyLine
	alerts        int    // the body's first lines that are open tickets
	bodyFace      string // what the body was built for
	bodyWidth     int
}

type reviewQueueMsg struct {
	rows []ReviewRow
	err  error
	seq  int
	tick bool // the read of the one-second chain: schedule the next
}

type reviewTickMsg struct{}

type reviewDetailTickMsg struct{}

type reviewDetailMsg struct {
	id     string
	detail ReviewDetail
	err    error
}

type reviewActedMsg struct {
	verb, id, line string
	err            error
}

type reviewDiffMsg struct {
	cmd    *exec.Cmd
	err    error
	cancel context.CancelFunc
}

type reviewDiffDoneMsg struct{ err error }

// NewReview returns the review view over src; now is the clock for ages,
// the dwell and the quiet periods.
func NewReview(src ReviewSource, now func() time.Time, opts ReviewOptions) ReviewModel {
	return ReviewModel{src: src, now: now, opts: opts, tick: reviewTick, run: tea.ExecProcess}
}

func reviewTick(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// Init starts the first queue read and with it the one-second chain, and
// the five-second detail chain.
func (m ReviewModel) Init() tea.Cmd {
	return tea.Batch(m.queueCmd(m.issued, true), m.tick(reviewDetailEvery, reviewDetailTickMsg{}))
}

func (m ReviewModel) queueCmd(seq int, tick bool) tea.Cmd {
	src := m.src
	return call(refreshTimeout, func(ctx context.Context) tea.Msg {
		rows, err := src.Queue(ctx)
		return reviewQueueMsg{rows: rows, err: err, seq: seq, tick: tick}
	})
}

// refresh starts a queue read.
func (m ReviewModel) refresh(tick bool) (ReviewModel, tea.Cmd) {
	m.issued++
	return m, m.queueCmd(m.issued, tick)
}

// Update handles one message.
func (m ReviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := m.update(msg)
	return m.relayout(), cmd
}

func (m ReviewModel) update(msg tea.Msg) (ReviewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case reviewQueueMsg:
		next, cmd := m.applyQueue(msg)
		if msg.tick {
			cmd = tea.Batch(cmd, next.tick(refreshEvery, reviewTickMsg{}))
		}
		return next, cmd
	case reviewTickMsg:
		return m.refresh(true)
	case reviewDetailMsg:
		return m.applyDetail(msg)
	case reviewDetailTickMsg:
		next, cmd := m.askDetail(true)
		return next, tea.Batch(cmd, next.tick(reviewDetailEvery, reviewDetailTickMsg{}))
	case reviewActedMsg:
		return m.applyActed(msg)
	case reviewDiffMsg:
		return m.applyDiff(msg)
	case reviewDiffDoneMsg:
		return m.applyDiffDone(msg), nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste && m.mode != reviewNote {
			return m.updateRunes(msg)
		}
		return m.updateKey(msg, false)
	}
	return m, nil
}

// current is the shown row, and false when the queue is empty.
func (m ReviewModel) current() (ReviewRow, bool) {
	if len(m.rows) == 0 {
		return ReviewRow{}, false
	}
	return m.rows[m.cursor], true
}

// selectedID is the shown review's id, or "".
func (m ReviewModel) selectedID() string {
	r, _ := m.current()
	return r.Review.ID
}

// shownDetail is the detail of the shown review when it has been read for
// the review as the queue shows it now (or later).
func (m ReviewModel) shownDetail() (ReviewDetail, bool) {
	r, ok := m.current()
	if !ok || !m.hasDetail || m.detail.Review.ID != r.Review.ID || m.detail.Review.UpdatedAt.Before(r.Review.UpdatedAt) {
		return ReviewDetail{}, false
	}
	return m.detail, true
}

// face is what is on screen about the shown review: its detail, or its id
// and UpdatedAt while the detail is not read. A different face restarts the
// dwell and cancels a prompt opened on the old one.
func (m ReviewModel) face() string {
	r, ok := m.current()
	if !ok {
		return ""
	}
	if d, ok := m.shownDetail(); ok {
		return "d" + detailFace(d)
	}
	return "r" + field(r.Review.ID) + field(strconv.FormatInt(r.Review.UpdatedAt.UnixNano(), 10))
}

// field is s with its length in front, so joined fields cannot run into
// each other.
func field(s string) string {
	return strconv.Itoa(len(s)) + ":" + s
}

// detailFace is every field of d the view draws.
func detailFace(d ReviewDetail) string {
	r, t := d.Review, d.Task
	parts := []string{
		r.ID, r.TaskID, strconv.FormatInt(r.CreatedAt.UnixNano(), 10), strconv.FormatInt(r.UpdatedAt.UnixNano(), 10),
		r.Summary, string(r.Tests), r.TestsDetail, string(r.Status),
		t.ID, t.Title, t.Repo, t.BaseBranch, t.Branch,
		d.DiffStat, d.DiffNote, d.Head, string(d.Approve),
		fmt.Sprintf("%d %d %d %g %t", d.Sessions, d.Teammates, d.Duration, d.CostUSD, d.CostKnown),
	}
	for _, list := range [][]model.Decision{d.Assumptions, d.Blocking, d.Decided} {
		parts = append(parts, strconv.Itoa(len(list)))
		for _, x := range list {
			parts = append(parts, x.ID, x.Question, x.Assumption, string(x.Status), x.AnswerKey, x.Answer)
			for _, o := range x.Options {
				parts = append(parts, o.Key, o.Label)
			}
		}
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(field(p))
	}
	return b.String()
}

// applyQueue takes a good read that is not older than the one shown and
// keeps the same review on screen.
func (m ReviewModel) applyQueue(msg reviewQueueMsg) (ReviewModel, tea.Cmd) {
	if msg.seq < m.applied {
		return m, nil
	}
	m.applied, m.err = msg.seq, msg.err
	if msg.err != nil {
		return m, nil
	}
	before := m.face()
	m.rows = nil // a fresh slice: earlier copies of the model keep theirs
	for _, r := range msg.rows {
		if m.opts.TaskID == "" || r.Review.TaskID == m.opts.TaskID {
			m.rows = append(m.rows, r)
		}
	}
	m.loaded = true
	m = m.reselect().observe(before)
	return m.askDetail(false)
}

// reselect puts the cursor back on the shown review or, when it is gone,
// on the review nearest its old place.
func (m ReviewModel) reselect() ReviewModel {
	if i := slices.IndexFunc(m.rows, func(r ReviewRow) bool { return r.Review.ID == m.selected }); i >= 0 {
		m.cursor = i
	}
	return m.moveTo(m.cursor)
}

// moveTo shows review i, clamped to the queue; another review starts at
// the top.
func (m ReviewModel) moveTo(i int) ReviewModel {
	m.cursor = max(min(i, len(m.rows)-1), 0)
	if id := m.selectedID(); id != m.selected {
		m.selected, m.offset, m.detailErr = id, 0, nil
	}
	return m
}

// observe restarts the dwell when what is on screen differs from before,
// and cancels a prompt or the note input whose review is no longer on
// screen as it was when it opened.
func (m ReviewModel) observe(before string) ReviewModel {
	after := m.face()
	if after != before {
		m.shownSince = m.now()
	}
	if m.mode != reviewBrowse && (m.selectedID() != m.pendingID || after != m.pendingFace) {
		m = m.cancelChanged()
	}
	return m
}

// cancelChanged closes the prompt or the note input because its review
// changed. A note typed so far is kept for the same review; the quiet
// period starts, so the rest of what is being typed cannot act; and the
// next key is refused with the notice.
func (m ReviewModel) cancelChanged() ReviewModel {
	if m.mode == reviewNote {
		m.draftID, m.draft = m.pendingID, slices.Clone(m.buf)
		m = m.quiet(answeredQuiet)
	}
	m.mode, m.buf, m.pending = reviewBrowse, nil, ReviewDetail{}
	m.changed = true
	m.notice, m.noticeErr = noticeReviewChanged, false
	return m
}

// askDetail reads the shown review's detail, when it has not been read for
// the review as the queue shows it (or always, when force is set). One read
// runs at a time; applyDetail asks again when the selection moved meanwhile.
func (m ReviewModel) askDetail(force bool) (ReviewModel, tea.Cmd) {
	r, ok := m.current()
	if !ok || m.asking {
		return m, nil
	}
	if _, read := m.shownDetail(); read && !force {
		return m, nil
	}
	m.asking, m.askedID, m.askedAt = true, r.Review.ID, r.Review.UpdatedAt
	src, id := m.src, r.Review.ID
	return m, call(reviewDetailTimeout, func(ctx context.Context) tea.Msg {
		d, err := src.Detail(ctx, id)
		return reviewDetailMsg{id: id, detail: d, err: err}
	})
}

// applyDetail takes a detail read for the shown review; an error keeps the
// last good one. A read for a review no longer shown is dropped.
func (m ReviewModel) applyDetail(msg reviewDetailMsg) (ReviewModel, tea.Cmd) {
	m.asking = false
	before := m.face()
	if msg.id == m.selectedID() {
		m.detailErr = msg.err
		if msg.err == nil {
			m.detail, m.hasDetail = msg.detail, true
		}
	}
	m = m.observe(before)
	if r, ok := m.current(); ok && (r.Review.ID != m.askedID || r.Review.UpdatedAt.After(m.askedAt)) {
		return m.askDetail(false)
	}
	return m, nil
}

// updateRunes handles keys typed faster than the program reads them, which
// arrive as one message with several runes, one rune at a time. A rune that
// opens or closes a prompt, moves or acts drops the rest: the user has not
// seen what it did.
func (m ReviewModel) updateRunes(msg tea.KeyMsg) (ReviewModel, tea.Cmd) {
	for _, r := range msg.Runes {
		was := m.mode
		var cmd tea.Cmd
		m, cmd = m.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}, true)
		if cmd != nil || m.mode != was || m.busy || m.diffing {
			return m, cmd
		}
	}
	return m, nil
}

// updateKey handles one key; split is set for a rune taken from a message
// of several. Every Enter is recorded, whatever the state, so a held
// Enter's repeats are known as repeats.
func (m ReviewModel) updateKey(msg tea.KeyMsg, split bool) (ReviewModel, tea.Cmd) {
	repeated := msg.Type == tea.KeyEnter && m.enterRepeated()
	if m.busy || m.diffing {
		return m.busyKey(msg), nil
	}
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	m.notice, m.noticeErr = "", false
	switch m.mode {
	case reviewConfirmApprove, reviewConfirmDrop:
		return m.confirmKey(msg, split)
	case reviewNote:
		return m.noteKey(msg, repeated)
	}
	if msg.Paste { // pasted text is never a command
		return m.typedText(), nil
	}
	return m.commandKey(msg)
}

// enterRepeated records an Enter and reports whether it came within the
// quiet period of the Enter before it, as a held or doubled Enter does (see
// quietPeriod).
func (m *ReviewModel) enterRepeated() bool {
	now := m.now()
	repeated := !m.lastEnter.IsZero() && now.Sub(m.lastEnter) < quietPeriod
	m.lastEnter = now
	return repeated
}

// busyKey ignores a key while an action is in flight or the diff tool is
// being prepared: nothing acts, q, Esc and Ctrl+C included, since closing
// could cut a merge short. A close key is told why.
func (m ReviewModel) busyKey(msg tea.KeyMsg) ReviewModel {
	if m.busy && (msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyEsc || (!msg.Paste && msg.String() == "q")) {
		m.closeAsked = true
	}
	return m
}

// commandKey handles a key outside a prompt and the note input. q and Esc
// close, even in the quiet after an action (Ruling 93), except right after
// the diff tool; every other key is refused once after a change canceled a
// prompt, and during a quiet period.
func (m ReviewModel) commandKey(msg tea.KeyMsg) (ReviewModel, tea.Cmd) {
	k := msg.String()
	quiet := m.now().Before(m.quietUntil)
	switch {
	case (k == "q" || k == "esc") && quiet && m.toolQuiet: // the key that closed the diff tool, pressed twice
		return m.tooSoon(), nil
	case k == "q" || k == "esc":
		return m, m.close()
	case m.changed:
		return m.refuseChanged(), nil
	case quiet:
		return m.tooSoon(), nil
	}
	if scrolled, ok := m.scrollKey(k); ok {
		return scrolled, nil
	}
	if next, cmd, ok := m.actionKey(k); ok {
		return next, cmd
	}
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		return m.typedText(), nil
	}
	return m, nil
}

// actionKey moves to another review or starts an action for ] [ n p a x c
// and o.
func (m ReviewModel) actionKey(k string) (ReviewModel, tea.Cmd, bool) {
	var cmd tea.Cmd
	switch k {
	case "]", "n":
		m, cmd = m.move(m.cursor + 1)
	case "[", "p":
		m, cmd = m.move(m.cursor - 1)
	case "a":
		m = m.askApprove()
	case "x":
		m = m.askDrop()
	case "c":
		m = m.openNote()
	case "o":
		m, cmd = m.openDiff()
	default:
		return m, nil, false
	}
	return m, cmd, true
}

// scrollKey scrolls the body for j, k, the arrows and the page keys.
func (m ReviewModel) scrollKey(k string) (ReviewModel, bool) {
	page := max(m.bodyRoom()-1, 1)
	switch k {
	case "j", "down":
		m.offset++
	case "k", "up":
		m.offset--
	case "pgdown":
		m.offset += page
	case "pgup":
		m.offset -= page
	default:
		return m, false
	}
	return m.clampOffset(), true
}

// close quits, or when embedded tells the dashboard the view closed.
func (m ReviewModel) close() tea.Cmd {
	if m.opts.Embedded {
		return func() tea.Msg { return ReviewClosedMsg{} }
	}
	return tea.Quit
}

// quiet starts a quiet period of d.
func (m ReviewModel) quiet(d time.Duration) ReviewModel {
	m.quietUntil, m.quietFor, m.toolQuiet = m.now().Add(d), d, false
	return m
}

// typedText ignores text that is no command and starts the quiet period,
// so the rest of what is typed or pasted cannot act (Ruling 88).
func (m ReviewModel) typedText() ReviewModel {
	m = m.quiet(answeredQuiet)
	return m.refuse(noticeReviewTyped)
}

// refuse ignores a key with notice and restarts the dwell, so a held,
// repeating key never gets through: it must be let go first (Ruling 67).
func (m ReviewModel) refuse(notice string) ReviewModel {
	m.notice, m.shownSince = notice, m.now()
	return m
}

// tooSoon ignores a key that came too soon. It restarts the dwell and a
// running quiet period.
func (m ReviewModel) tooSoon() ReviewModel {
	if now := m.now(); now.Before(m.quietUntil) {
		m.quietUntil = now.Add(m.quietFor)
	}
	return m.refuse(noticeTooSoon)
}

// refuseChanged ignores the first key after a change canceled a prompt or
// the note input, and starts the quiet period (or keeps a longer one), so
// the repeats of a held key are ignored however late the first one comes
// (Ruling 68).
func (m ReviewModel) refuseChanged() ReviewModel {
	m.changed = false
	if until := m.now().Add(quietPeriod); until.After(m.quietUntil) {
		m.quietUntil, m.quietFor = until, quietPeriod
	}
	return m.refuse(noticeReviewChanged)
}

// move shows review i and reads its detail.
func (m ReviewModel) move(i int) (ReviewModel, tea.Cmd) {
	before, was := m.face(), m.selected
	if m = m.moveTo(i); m.selected == was {
		return m, nil // at the edge of the queue
	}
	return m.observe(before).askDetail(false)
}

// guard reports whether an action key may act on the shown review now: its
// detail has been read, and it has been on screen, as it is, for the dwell.
// A refusal restarts the dwell.
func (m ReviewModel) guard() (ReviewModel, bool) {
	if _, ok := m.current(); !ok {
		return m, false
	}
	if _, ok := m.shownDetail(); !ok {
		return m.refuse(noticeReviewReading), false
	}
	if m.now().Sub(m.shownSince) < dwell {
		return m.tooSoon(), false
	}
	return m, true
}

func (m ReviewModel) askApprove() ReviewModel {
	m, ok := m.guard()
	if !ok {
		return m
	}
	d, _ := m.shownDetail()
	if d.Approve != model.ApproveMerge && d.Approve != model.ApprovePR {
		m.notice, m.noticeErr = "cannot approve: the approve method "+strconv.Quote(sanitize.Line(string(d.Approve), maxWordCells))+" is unknown", true
		return m
	}
	return m.openPrompt(reviewConfirmApprove, d)
}

func (m ReviewModel) askDrop() ReviewModel {
	m, ok := m.guard()
	if !ok {
		return m
	}
	d, _ := m.shownDetail()
	return m.openPrompt(reviewConfirmDrop, d)
}

// openPrompt asks to confirm mode's action on d, as it is on screen now.
func (m ReviewModel) openPrompt(mode reviewMode, d ReviewDetail) ReviewModel {
	m.mode, m.pending, m.pendingID, m.pendingFace = mode, d, d.Review.ID, m.face()
	m.promptSince, m.promptWait = m.now(), dwell
	return m
}

// confirmKey acts on a "y" that came as a key of its own, the wait after
// the prompt opened, while the review is still as it was shown. A "y" too
// soon is ignored and restarts the wait, now as long as the quiet period,
// so the repeats of a held key never confirm (a held key's first repeat
// comes 375 to 660 ms after the press). Any other key, a "y" in a burst
// and a paste cancel; a paste also starts the quiet period.
func (m ReviewModel) confirmKey(msg tea.KeyMsg, split bool) (ReviewModel, tea.Cmd) {
	if msg.Paste || split || msg.String() != "y" {
		m.mode, m.pending, m.shownSince = reviewBrowse, ReviewDetail{}, m.now()
		if msg.Paste {
			m = m.typedText()
		}
		return m, nil
	}
	if now := m.now(); now.Sub(m.promptSince) < m.promptWait {
		m.promptSince, m.promptWait = now, quietPeriod
		m.notice = noticeTooSoon
		return m, nil
	}
	if m.selectedID() != m.pendingID || m.face() != m.pendingFace {
		return m.cancelChanged(), nil
	}
	mode, id, head := m.mode, m.pendingID, m.pending.Head
	m.mode, m.pending = reviewBrowse, ReviewDetail{}
	src := m.src
	if mode == reviewConfirmDrop {
		return m.act(func(ctx context.Context) reviewActedMsg {
			line, err := src.Drop(ctx, id)
			return reviewActedMsg{verb: "drop", id: id, line: line, err: err}
		})
	}
	return m.act(func(ctx context.Context) reviewActedMsg {
		line, err := src.Approve(ctx, id, head)
		return reviewActedMsg{verb: "approve", id: id, line: line, err: err}
	})
}

// openNote opens the change request input on the shown review, holding a
// note not sent for it before.
func (m ReviewModel) openNote() ReviewModel {
	m, ok := m.guard()
	if !ok {
		return m
	}
	d, _ := m.shownDetail()
	m.mode, m.pending, m.pendingID, m.pendingFace = reviewNote, d, d.Review.ID, m.face()
	m.buf = nil
	if m.draftID == d.Review.ID {
		m.buf = slices.Clone(m.draft)
	}
	m.inputSince, m.burst = m.now(), time.Time{}
	return m
}

// noteKey edits the note: typed and pasted text is text, never a command;
// Backspace deletes; Enter sends; Esc cancels and keeps nothing.
func (m ReviewModel) noteKey(msg tea.KeyMsg, repeated bool) (ReviewModel, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode, m.buf, m.pending, m.shownSince = reviewBrowse, nil, ReviewDetail{}, m.now()
		m.draftID, m.draft = "", nil
	case tea.KeyEnter:
		return m.submitNote(repeated)
	case tea.KeyBackspace:
		m.buf = m.buf[:max(len(m.buf)-1, 0)]
	case tea.KeyRunes, tea.KeySpace:
		if len(msg.Runes) > 1 && !msg.Paste {
			m.burst = m.now()
		}
		if !msg.Alt || msg.Paste {
			m = m.appendNote(msg.Runes)
		}
	}
	return m, nil
}

// appendNote adds runes to the note as text, cleaned by sanitize.Line
// (escape sequences and control characters removed, line breaks and tabs
// as spaces, runs of spaces as one), keeping a space between words at
// either end, and cut without an ellipsis at the room left under
// model.MaxReviewNote.
func (m ReviewModel) appendNote(rs []rune) ReviewModel {
	if len(rs) == 0 {
		return m
	}
	text := sanitize.Line(string(rs), 0) // the whole paste is cleaned in one linear pass, then cut
	var body []rune
	if unicode.IsSpace(rs[0]) && len(m.buf) > 0 && m.buf[len(m.buf)-1] != ' ' {
		body = append(body, ' ')
	}
	body = append(body, []rune(text)...)
	if text != "" && unicode.IsSpace(rs[len(rs)-1]) {
		body = append(body, ' ')
	}
	if room := max(model.MaxReviewNote-len(m.buf), 0); len(body) > room {
		body = body[:room]
		m.notice = fmt.Sprintf("the note is limited to %d characters", model.MaxReviewNote)
	}
	m.buf = append(slices.Clip(m.buf), body...)
	return m
}

// submitNote sends the note on an Enter that is not a line break of text
// pasted without bracketed paste, not a repeat, and not within the dwell of
// the input opening; a blank note sends nothing.
func (m ReviewModel) submitNote(repeated bool) (ReviewModel, tea.Cmd) {
	now := m.now()
	if now.Sub(m.burst) < dwell {
		m.burst = now
		m = m.appendNote([]rune{' '})
		m.notice = "pasted line break kept as a space; enter sends"
		return m, nil
	}
	if repeated || now.Sub(m.inputSince) < dwell {
		m.inputSince, m.notice = now, noticeTooSoon
		return m, nil
	}
	note := strings.TrimSpace(string(m.buf))
	if note == "" {
		m.notice = "the note is empty: type the changes to ask for, or esc to cancel"
		return m, nil
	}
	if m.selectedID() != m.pendingID || m.face() != m.pendingFace {
		return m.cancelChanged(), nil
	}
	id := m.pendingID
	m.mode, m.buf, m.pending = reviewBrowse, nil, ReviewDetail{}
	m.draftID, m.draft = id, []rune(note) // offered again if the request fails
	src := m.src
	return m.act(func(ctx context.Context) reviewActedMsg {
		line, err := src.RequestChanges(ctx, id, note)
		return reviewActedMsg{verb: "request changes", id: id, line: line, err: err}
	})
}

// act runs f off the event loop. Until it returns, the status line reads
// "working…" and every key is ignored.
func (m ReviewModel) act(f func(context.Context) reviewActedMsg) (ReviewModel, tea.Cmd) {
	m.busy, m.closeAsked = true, false
	m.result, m.resultErr = "", false
	return m, call(reviewActionTimeout, func(ctx context.Context) tea.Msg { return f(ctx) })
}

// applyActed shows an action's line or error and starts the quiet period
// after it (Ruling 93). A review acted on leaves the queue at once; the
// queue is read again, and reads started before the action returned are
// out of date and dropped. After an error the shown review's detail is
// read again, so a refusal because the branch moved shows the new state.
func (m ReviewModel) applyActed(msg reviewActedMsg) (ReviewModel, tea.Cmd) {
	m.busy, m.closeAsked = false, false
	m = m.quiet(answeredQuiet)
	var cmd tea.Cmd
	if msg.err != nil {
		m.result, m.resultErr = msg.verb+" failed: "+msg.err.Error(), true
		m, cmd = m.askDetail(true)
	} else {
		m.result = msg.line
		if m.result == "" {
			m.result = msg.id + ": " + msg.verb + " done"
		}
		if m.draftID == msg.id {
			m.draftID, m.draft = "", nil
		}
		before := m.face()
		m.rows = slices.DeleteFunc(slices.Clone(m.rows), func(r ReviewRow) bool { return r.Review.ID == msg.id })
		m, cmd = m.reselect().observe(before).askDetail(false)
	}
	m.results = append(slices.Clip(m.results), m.result)
	m, read := m.refresh(false)
	m.applied = m.issued
	return m, tea.Batch(cmd, read)
}

// Result is the line or error of every action taken in the view, in order,
// as the view showed them, so that the command can print them once the
// view has quit. It is empty when nothing was acted on.
func (m ReviewModel) Result() []string {
	return slices.Clone(m.results)
}

// openDiff builds the configured diff tool's command for the shown review
// off the event loop; applyDiff runs it on the terminal.
func (m ReviewModel) openDiff() (ReviewModel, tea.Cmd) {
	m, ok := m.guard()
	if !ok {
		return m, nil
	}
	m.diffing = true
	src, id := m.src, m.selectedID()
	ctx, cancel := context.WithCancel(context.Background())
	return m, func() tea.Msg {
		c, err := src.DiffCmd(ctx, id)
		return reviewDiffMsg{cmd: c, err: err, cancel: cancel}
	}
}

func (m ReviewModel) applyDiff(msg reviewDiffMsg) (ReviewModel, tea.Cmd) {
	if msg.err != nil || msg.cmd == nil {
		msg.cancel()
		m.diffing = false
		text := "no diff tool"
		if msg.err != nil {
			text = msg.err.Error()
		}
		m.notice, m.noticeErr = "diff failed: "+text, true
		return m, nil
	}
	cancel := msg.cancel
	return m, m.run(msg.cmd, func(err error) tea.Msg {
		cancel()
		return reviewDiffDoneMsg{err: err}
	})
}

// applyDiffDone shows the tool's error. A key that closed the tool and came
// twice must not act here, so the quiet period starts, holding q and Esc
// too, and the dwell restarts.
func (m ReviewModel) applyDiffDone(msg reviewDiffDoneMsg) ReviewModel {
	m.diffing = false
	if msg.err != nil {
		m.notice, m.noticeErr = "diff tool failed: "+msg.err.Error(), true
	}
	m = m.quiet(quietPeriod)
	m.toolQuiet, m.shownSince = true, m.now()
	return m
}

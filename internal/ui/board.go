package ui

import (
	"context"
	"fmt"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
)

// boardActionTimeout bounds one action of the board: a status change or a
// spawn, which returns once the agent's window is started.
const boardActionTimeout = 5 * time.Second

// boardStatuses are the statuses of the board's four columns, in order.
// Dropped tasks are not shown; review and done are not produced in Plan 3
// and are not shown either.
var boardStatuses = [...]model.TaskStatus{
	model.TaskBacklog, model.TaskReady, model.TaskRunning, model.TaskNeedsDecision,
}

// boardColumns is the number of columns.
const boardColumns = len(boardStatuses)

// noticeBoardTyped is shown when typed or pasted text that is no command
// is ignored; it starts the quiet period, so the rest of it cannot act.
const noticeBoardTyped = "typing ignored; the board takes single keys"

// BoardSource is where the board reads its cards, already derived and ordered, and acts on a task.
type BoardSource interface {
	Cards(ctx context.Context) ([]task.Card, error)
	SetStatus(ctx context.Context, id string, s model.TaskStatus) error // backlog or ready
	Spawn(ctx context.Context, id string) (line string, err error)      // returns at once; the line is shown
}

// BoardClosedMsg is sent when an embedded board closes.
type BoardClosedMsg struct{}

// BoardModel is the task board's Bubble Tea model.
type BoardModel struct {
	src      BoardSource
	tm       tmux.Client
	now      func() time.Time
	embedded bool

	cols     [boardColumns][]task.Card // the last good read, by column, in source order
	col, row int                       // the selected card's place
	selected string                    // id of the selected card, kept across refreshes
	loaded   bool                      // a read has succeeded

	shownSince time.Time     // when the selected card, as it is now, came on screen
	quietUntil time.Time     // keys but q and Esc before this are ignored
	quietFor   time.Duration // the length of the running quiet period, which an ignored key restarts

	confirming  bool          // pending waits for "y"
	pending     task.Card     // the card as it was on screen when n was pressed
	promptSince time.Time     // when the prompt opened, or last ignored a "y"
	promptWait  time.Duration // how long after promptSince a "y" counts

	busy     bool // an action is in flight
	closing  bool // close once the action in flight returns, unless it failed
	quitting bool // closing was asked by Ctrl+C: quit the program, even embedded

	issued, applied int // sequence numbers of the last refresh started and applied

	width, height int // 0 until the first WindowSizeMsg

	notice    string // the last outcome or message, cleared by the next key
	noticeErr bool   // notice is an error
	err       error  // the last refresh's error, cleared by a good one
}

// boardRefreshMsg is the result of one read of the source. Only the
// refreshes of the one-second chain (tick) schedule the next.
type boardRefreshMsg struct {
	cards []task.Card
	err   error
	seq   int
	tick  bool
}

// boardTickMsg starts the next refresh of the chain.
type boardTickMsg struct{}

// boardActedMsg is the result of one action.
type boardActedMsg struct {
	verb string // "set" or "spawn", for the error notice
	line string
	err  error
}

// NewBoard returns the board. embedded: closing sends BoardClosedMsg instead of quitting.
func NewBoard(src BoardSource, tm tmux.Client, now func() time.Time, embedded bool) BoardModel {
	return BoardModel{src: src, tm: tm, now: now, embedded: embedded}
}

// Init starts the first refresh and with it the one-second chain.
func (m BoardModel) Init() tea.Cmd {
	return m.refreshCmd(m.issued, true)
}

// refresh starts a read of the source.
func (m BoardModel) refresh(tick bool) (BoardModel, tea.Cmd) {
	m.issued++
	return m, m.refreshCmd(m.issued, tick)
}

func (m BoardModel) refreshCmd(seq int, tick bool) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		cards, err := src.Cards(ctx)
		return boardRefreshMsg{cards: cards, err: err, seq: seq, tick: tick}
	}
}

// Update handles one message.
func (m BoardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case boardRefreshMsg:
		m = m.applyRefresh(msg)
		if msg.tick {
			return m, tea.Tick(refreshEvery, func(time.Time) tea.Msg { return boardTickMsg{} })
		}
		return m, nil
	case boardTickMsg:
		return m.refresh(true)
	case boardActedMsg:
		return m.applyActed(msg)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			return m.updateRunes(msg)
		}
		return m.updateKey(msg)
	}
	return m, nil
}

// applyRefresh takes a good read that is not older than the one shown and
// keeps the selection on the same task. A prompt whose card is gone or no
// longer as it was shown is closed: its "y" would apply to something the
// user did not see.
func (m BoardModel) applyRefresh(msg boardRefreshMsg) BoardModel {
	if msg.seq < m.applied {
		return m
	}
	m.applied, m.err = msg.seq, msg.err
	if msg.err != nil {
		return m
	}
	before, had := m.current()
	m.cols = [boardColumns][]task.Card{} // fresh slices: earlier copies of the model keep theirs
	for _, c := range msg.cards {
		if i := slices.Index(boardStatuses[:], c.Status); i >= 0 {
			m.cols[i] = append(m.cols[i], c)
		}
	}
	m.loaded = true
	m = m.reselect().markShown(before, had)
	if m.confirming {
		if c, ok := m.find(m.pending.Task.ID); !ok || !sameCard(c, m.pending) {
			m.confirming, m.pending = false, task.Card{}
			m.notice, m.noticeErr = cardID(c)+" changed; spawn canceled", false
		}
	}
	return m
}

// find returns the card with id.
func (m BoardModel) find(id string) (task.Card, bool) {
	for _, col := range m.cols {
		if i := slices.IndexFunc(col, func(c task.Card) bool { return c.Task.ID == id }); i >= 0 {
			return col[i], true
		}
	}
	return task.Card{Task: model.Task{ID: id}}, false
}

// reselect puts the selection back on the selected task or, when it is
// gone, on the card nearest its old place.
func (m BoardModel) reselect() BoardModel {
	for ci, col := range m.cols {
		if i := slices.IndexFunc(col, func(c task.Card) bool { return c.Task.ID == m.selected }); i >= 0 {
			m.col, m.row = ci, i
			return m.place(ci, i)
		}
	}
	return m.place(m.nearestColumn(m.col), m.row)
}

// nearestColumn is the column with cards nearest col, col itself first and
// the left one first at equal distance; col when every column is empty.
func (m BoardModel) nearestColumn(col int) int {
	for d := range boardColumns {
		for _, c := range []int{col - d, col + d} {
			if c >= 0 && c < boardColumns && len(m.cols[c]) > 0 {
				return c
			}
		}
	}
	return col
}

// place selects row of column col, clamped to the column.
func (m BoardModel) place(col, row int) BoardModel {
	m.col = col
	m.row = max(min(row, len(m.cols[col])-1), 0)
	m.selected = ""
	if c, ok := m.current(); ok {
		m.selected = c.Task.ID
	}
	return m
}

// current returns the selected card, and false when there is none.
func (m BoardModel) current() (task.Card, bool) {
	if m.row >= len(m.cols[m.col]) {
		return task.Card{}, false
	}
	return m.cols[m.col][m.row], true
}

// cardFace is what the board shows of a card and an action depends on.
type cardFace struct {
	ID, Title, Repo, Pane string
	Status                model.TaskStatus
}

// sameCard reports whether a and b are the same task as shown.
func sameCard(a, b task.Card) bool {
	face := func(c task.Card) cardFace {
		return cardFace{ID: c.Task.ID, Title: c.Task.Title, Repo: c.Task.Repo, Pane: c.Pane, Status: c.Status}
	}
	return face(a) == face(b)
}

// markShown restarts the dwell when the selected card differs from before,
// whatever changed it.
func (m BoardModel) markShown(before task.Card, had bool) BoardModel {
	after, has := m.current()
	if had != has || (has && !sameCard(before, after)) {
		m.shownSince = m.now()
	}
	return m
}

// updateRunes handles keys typed faster than the program reads them, which
// arrive as one message with several runes, one rune at a time. A rune
// that opens or closes a prompt, acts or closes the board drops the rest:
// the user has not seen what it did. A move restarts the dwell, so a key
// after it in the burst cannot act.
func (m BoardModel) updateRunes(msg tea.KeyMsg) (BoardModel, tea.Cmd) {
	var cmds []tea.Cmd
	for _, r := range msg.Runes {
		was := m.confirming
		var cmd tea.Cmd
		m, cmd = m.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt})
		cmds = append(cmds, cmd)
		if cmd != nil || m.confirming != was {
			break
		}
	}
	return m, tea.Batch(cmds...)
}

// updateKey handles one key.
func (m BoardModel) updateKey(msg tea.KeyMsg) (BoardModel, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		if m.busy { // quitting now could cut an action short
			m.closing, m.quitting = true, true
			return m, nil
		}
		return m, tea.Quit
	}
	m.notice, m.noticeErr = "", false
	switch {
	case m.confirming:
		return m.confirmKey(msg)
	case msg.Paste: // pasted text is never a command
		return m.typedText(), nil
	}
	return m.commandKey(msg)
}

func (m BoardModel) commandKey(msg tea.KeyMsg) (BoardModel, tea.Cmd) {
	k := msg.String()
	if k != "q" && k != "esc" && m.now().Before(m.quietUntil) {
		return m.tooSoon(), nil
	}
	switch k {
	case "q", "esc", "t":
		if m.busy {
			m.closing = true
			return m, nil
		}
		return m, m.close()
	case "h", "left":
		return m.moveColumn(-1), nil
	case "l", "right":
		return m.moveColumn(1), nil
	case "j", "down":
		return m.moveRow(1), nil
	case "k", "up":
		return m.moveRow(-1), nil
	case "enter":
		return m.jump()
	case "r":
		return m.toggleReady()
	case "n":
		return m.askSpawn(), nil
	}
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		return m.typedText(), nil
	}
	return m, nil
}

// typedText ignores text that is no command, as when the user types into
// the board out of habit, and starts the quiet period, so the rest of what
// is typed cannot move, act or answer a prompt.
func (m BoardModel) typedText() BoardModel {
	m = m.quiet(answeredQuiet)
	m.notice = noticeBoardTyped
	return m
}

// quiet starts a quiet period of d.
func (m BoardModel) quiet(d time.Duration) BoardModel {
	m.quietUntil, m.quietFor = m.now().Add(d), d
	return m
}

// tooSoon ignores a key that came too soon. It restarts the dwell of the
// selected card and a running quiet period, so a held, repeating key never
// gets through: it must be let go first (Ruling 67).
func (m BoardModel) tooSoon() BoardModel {
	now := m.now()
	if now.Before(m.quietUntil) {
		m.quietUntil = now.Add(m.quietFor)
	}
	m.shownSince, m.notice = now, noticeTooSoon
	return m
}

// close quits, or when embedded tells the dashboard the board closed.
func (m BoardModel) close() tea.Cmd {
	if m.embedded {
		return func() tea.Msg { return BoardClosedMsg{} }
	}
	return tea.Quit
}

// moveColumn selects the next column with cards in direction dir, keeping
// the row where the column has it.
func (m BoardModel) moveColumn(dir int) BoardModel {
	for c := m.col + dir; c >= 0 && c < boardColumns; c += dir {
		if len(m.cols[c]) > 0 {
			return m.moveTo(c, m.row)
		}
	}
	return m
}

// moveRow moves within the column. In the narrow list, which shows the
// columns one under another, it goes on into the next or previous column
// with cards.
func (m BoardModel) moveRow(dir int) BoardModel {
	row := m.row + dir
	if row >= 0 && row < len(m.cols[m.col]) {
		return m.moveTo(m.col, row)
	}
	if m.wide() {
		return m
	}
	for c := m.col + dir; c >= 0 && c < boardColumns; c += dir {
		if n := len(m.cols[c]); n > 0 {
			if dir < 0 {
				return m.moveTo(c, n-1)
			}
			return m.moveTo(c, 0)
		}
	}
	return m
}

// moveTo selects row of column col; a different card restarts the dwell.
func (m BoardModel) moveTo(col, row int) BoardModel {
	before, had := m.current()
	return m.place(col, row).markShown(before, had)
}

// guard reports whether an action may start on the selected card now. It
// refuses while an action is in flight, and before the card has been on
// screen for the dwell, restarting it, so a key typed in the same burst as
// a move, or held, never acts on a card the user has not seen (Ruling 66).
func (m BoardModel) guard() (BoardModel, bool) {
	switch {
	case m.busy:
		m.notice = "still working on the last action"
		return m, false
	case m.now().Sub(m.shownSince) < dwell:
		return m.tooSoon(), false
	}
	return m, true
}

// jump switches the tmux client to the lead pane of the selected task and
// closes the board.
func (m BoardModel) jump() (BoardModel, tea.Cmd) {
	c, ok := m.current()
	if !ok {
		return m, nil
	}
	if m, ok = m.guard(); !ok {
		return m, nil
	}
	if c.Pane == "" {
		m.notice = cardID(c) + " has no live agent"
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	if err := m.tm.SwitchTo(ctx, c.Pane); err != nil {
		m.notice, m.noticeErr = "jump failed: "+err.Error(), true
		return m, nil
	}
	return m, m.close()
}

// toggleReady moves the selected task between backlog and ready.
func (m BoardModel) toggleReady() (BoardModel, tea.Cmd) {
	c, ok := m.current()
	if !ok {
		return m, nil
	}
	to := model.TaskReady
	switch c.Status {
	case model.TaskBacklog:
	case model.TaskReady:
		to = model.TaskBacklog
	default:
		m.notice = cardID(c) + " " + statusPhrase(c.Status) + "; r moves a task between backlog and ready"
		return m, nil
	}
	if m, ok = m.guard(); !ok {
		return m, nil
	}
	src, id := m.src, c.Task.ID
	return m.act(func(ctx context.Context) boardActedMsg {
		return boardActedMsg{verb: "set", err: src.SetStatus(ctx, id, to)}
	})
}

// askSpawn asks to confirm spawning the selected task, as it is on screen
// now.
func (m BoardModel) askSpawn() BoardModel {
	c, ok := m.current()
	if !ok {
		return m
	}
	if c.Status != model.TaskBacklog && c.Status != model.TaskReady {
		m.notice = cardID(c) + " " + statusPhrase(c.Status) + "; only a backlog or ready task can be spawned"
		return m
	}
	if m, ok = m.guard(); !ok {
		return m
	}
	m.confirming, m.pending = true, c
	m.promptSince, m.promptWait = m.now(), dwell
	return m
}

// confirmKey spawns the pending task on a "y" that came as a key of its
// own, the wait after the prompt opened, while the task is still as it was
// shown. A "y" too soon is ignored and restarts the wait, now as long as
// the quiet period, so the repeats of a held key never confirm (a held
// key's first repeat comes 375 to 660 ms after the press). Any other key
// and a paste cancel.
func (m BoardModel) confirmKey(msg tea.KeyMsg) (BoardModel, tea.Cmd) {
	if msg.Paste || msg.String() != "y" {
		m.confirming, m.pending = false, task.Card{}
		if msg.Paste {
			m = m.typedText()
		}
		m.notice = "spawn canceled"
		return m, nil
	}
	if now := m.now(); now.Sub(m.promptSince) < m.promptWait {
		m.promptSince, m.promptWait = now, quietPeriod
		return m, nil
	}
	p := m.pending
	m.confirming, m.pending = false, task.Card{}
	if c, ok := m.current(); !ok || !sameCard(c, p) {
		m.notice = cardID(p) + " changed; spawn canceled"
		return m, nil
	}
	src, id := m.src, p.Task.ID
	return m.act(func(ctx context.Context) boardActedMsg {
		line, err := src.Spawn(ctx, id)
		return boardActedMsg{verb: "spawn", line: line, err: err}
	})
}

// act runs f off the event loop. The quiet period that starts now absorbs
// the repeats of the key that acted.
func (m BoardModel) act(f func(context.Context) boardActedMsg) (BoardModel, tea.Cmd) {
	m.busy = true
	m = m.quiet(quietPeriod)
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), boardActionTimeout)
		defer cancel()
		return f(ctx)
	}
}

// applyActed shows an action's result and re-reads the board; reads
// started before it returned are out of date and dropped. A close asked
// for meanwhile happens now, unless the action failed.
func (m BoardModel) applyActed(msg boardActedMsg) (BoardModel, tea.Cmd) {
	m.busy = false
	if msg.err == nil && m.closing {
		if m.quitting {
			return m, tea.Quit
		}
		return m, m.close()
	}
	m.closing, m.quitting = false, false
	switch {
	case msg.err != nil:
		m.notice, m.noticeErr = msg.verb+" failed: "+msg.err.Error(), true
	case msg.line != "":
		m.notice = msg.line
	}
	m, cmd := m.refresh(false)
	m.applied = m.issued
	return m, cmd
}

// cardID is c's id as drawn.
func cardID(c task.Card) string {
	return sanitize.Line(c.Task.ID, maxIDCells)
}

// statusPhrase says what a task shown as s is, after its id.
func statusPhrase(s model.TaskStatus) string {
	switch s {
	case model.TaskRunning:
		return "is running"
	case model.TaskNeedsDecision:
		return "needs a decision"
	default:
		return fmt.Sprintf("is %s", s)
	}
}

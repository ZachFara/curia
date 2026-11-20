// Package ui is the dashboard: a Bubble Tea model that lists every session,
// an agent team as one group row, shown as a tmux popup (curia ui) or in a
// narrow always-on pane (curia sidebar), and jumps to or kills the selected
// session's pane.
package ui

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

const (
	// refreshEvery is how often the model re-reads its Source.
	refreshEvery = time.Second
	// refreshTimeout bounds one read of the Source.
	refreshTimeout = 2 * time.Second
	// tmuxTimeout bounds one tmux call: the window-name lookup, a jump or a kill.
	tmuxTimeout = time.Second
)

// Source is where the dashboard reads its sessions, already in display
// order, and the open decision tickets of each. The command's
// implementation reconciles, lists and ranks, and sweeps expiry before it
// counts.
type Source interface {
	Sessions(ctx context.Context) ([]model.Session, error)
	// OpenDecisions counts the open tickets by session id and mode; only
	// the popup reads it.
	OpenDecisions(ctx context.Context) (map[string]model.OpenCount, error)
	// Tasks returns the tasks by id, for the TASK column; only the popup
	// reads it.
	Tasks(ctx context.Context) (map[string]model.Task, error)
	// Reviews returns the id of the pending review of each task, by task
	// id, for the r key; only the popup reads it.
	Reviews(ctx context.Context) (map[string]string, error)
	// Budget returns what the dashboard shows about usage, for the status
	// line, the b key and the pause suggestion; only the popup reads it.
	Budget(ctx context.Context) (Budget, error)
	// Pause pauses task taskID as curia pause does, without --force, and
	// returns the pause's last line. The popup calls it only after P and a
	// confirming y, for the task the prompt named (spec section 7).
	Pause(ctx context.Context, taskID string) (line string, err error)
}

// Budget is what the dashboard shows about usage. Every string in it is already rendered by Curia.
type Budget struct {
	Status string             // the forecast warning or the API projection; "" when there is none
	Burn   map[string]float64 // USD per hour by session id; nil when unknown

	// The pause suggestion (section 4.4), only while a limit forecast is in
	// force; PauseTask is "" when there is none.
	PauseTask  string // "T-7"
	PauseLabel string // "T-7 (P3) api-server"
	PauseTitle string // the task's title
	PauseLimit string // when the limit lands: "limit ~48m" or "at limit"
}

// pauseSuggestion is what a pause prompt names: the suggestion as the
// notice showed it when P was pressed. The zero value is no prompt.
type pauseSuggestion struct{ task, label, title string }

// suggestion is b's pause suggestion as a prompt names it; zero when none.
func (b Budget) suggestion() pauseSuggestion {
	if b.PauseTask == "" {
		return pauseSuggestion{}
	}
	return pauseSuggestion{task: b.PauseTask, label: b.PauseLabel, title: b.PauseTitle}
}

// Mode is how the dashboard is shown.
type Mode int

const (
	// ModePopup is the full table in a tmux popup; a jump closes it.
	ModePopup Mode = iota
	// ModeSidebar is the narrow table in an always-on pane; a jump keeps it open.
	ModeSidebar
)

// Level is how full a session's context window is.
type Level int

// Context levels.
const (
	LevelUnknown Level = iota
	LevelOK
	LevelWarn
	LevelCrit
)

// ContextLevel grades the whole percentage of limit (the context window's
// size) that used fills, rounded down as the dashboard shows it: LevelOK
// under warnPct, LevelWarn from warnPct to critPct inclusive, LevelCrit
// above critPct, and LevelUnknown when limit is not positive.
func ContextLevel(used, limit, warnPct, critPct int) Level {
	if limit <= 0 {
		return LevelUnknown
	}
	switch pct := contextPct(used, limit); {
	case pct > 100: // the window is wrong or unknown; no color
		return LevelUnknown
	case pct < warnPct:
		return LevelOK
	case pct <= critPct:
		return LevelWarn
	default:
		return LevelCrit
	}
}

// contextPct is the whole percentage of limit that used fills, rounded
// down; limit must be positive.
func contextPct(used, limit int) int {
	return used * 100 / limit
}

// Model is the dashboard's Bubble Tea model.
type Model struct {
	src  Source
	tm   tmux.Client
	cfg  config.Config
	now  func() time.Time
	mode Mode

	sessions []model.Session            // the last good read, in Source order
	counts   map[string]model.OpenCount // the last good OpenDecisions read; replaced, never mutated
	tasks    map[string]model.Task      // the last good Tasks read; replaced, never mutated
	reviews  map[string]string          // the last good Reviews read; replaced, never mutated
	budget   Budget                     // the last good Budget read; replaced, never mutated
	names    map[string]string          // window id to window name; replaced, never mutated
	live     []tmux.Pane                // the panes of the last window-name read: the server jumps and kills reach
	expanded map[string]bool            // team ids shown expanded; replaced, never mutated
	rows     []row                      // the units that pass the filter, as rows
	cursor   int                        // index of the selected row
	selected rowKey                     // the selected row, kept across refreshes
	selTeam  string                     // the selected row's team, to find its group row once collapsed
	offset   int                        // index of the first row on screen

	width, height int // 0 until the first WindowSizeMsg: no limit

	filter    string
	filtering bool   // typing into the filter
	burnSort  bool   // units ordered by their burn (b), not by rank
	killPane  string // pane awaiting kill confirmation, if any
	notice    string // message for the last key, cleared by the next one
	err       error  // the last refresh's error, cleared by a good one

	chain int // the refresh chain that is live; messages of earlier chains are dropped

	open     func(sessionID string) tea.Model // builds the decide view; nil: d cannot answer
	noAnswer string                           // why d cannot answer; "": inside an agent session
	noKill   string                           // why x cannot kill; "": it can
	board    func() tea.Model                 // builds the task board; nil: t and n say why
	review   func(taskID string) tea.Model    // builds the review view; nil: r says why
	noReview string                           // why r cannot open a review; "": inside an agent session
	child    tea.Model                        // the decide view, the board or the review view while it is shown
	childGen int                              // which child is (or was last) shown

	rowFace  string        // the selected row and its task's pending review, as last drawn
	rowSince time.Time     // when rowFace came, or an r too soon restarted the wait
	rowWait  time.Duration // how long r waits from rowSince: dwell, then quietPeriod after an r too soon

	pauseOn    bool            // WithPause: P can pause the suggested task
	noPause    string          // why P cannot pause; "": noticeNoPause
	pauseAsk   pauseSuggestion // what the open pause prompt names; zero: no prompt
	pauseSince time.Time       // when the prompt opened, or a y too soon restarted its wait
	pauseWait  time.Duration   // how long y waits from pauseSince: dwell, then quietPeriod after a y too soon
	keyAt      time.Time       // when the last key came, acted on or not
	prevKeyAt  time.Time       // when the key before it came
	confirmAt  time.Time       // when a y was taken, waiting out burstGap before it pauses; zero when none
	confirmSeq int             // the pauseConfirmMsg the taken y waits for
	pausing    string          // the task a pause in flight is for; "" when none
}

// rowKind is what a row of the table stands for.
type rowKind int

const (
	rowSolo  rowKind = iota // a session in no team, or a lead without teammates
	rowGroup                // a collapsed team: its lead's cells, its worst member's state
	rowLead                 // an expanded team's lead
	rowMate                 // a teammate, under its expanded lead
)

// row is one row of the table.
type row struct {
	kind    rowKind
	s       model.Session   // the session whose cells the row shows: the lead's for a group row
	team    string          // the team id; "" for a solo row
	worst   model.Session   // a group row's worst member
	members []model.Session // a group row's lead and teammates
}

// rowKey names a row across refreshes: a group row and its expanded lead
// row show the same session.
type rowKey struct {
	id    string
	group bool
}

func (r row) key() rowKey { return rowKey{id: r.s.ID, group: r.kind == rowGroup} }

// unit is a group row's team as a fleet.Unit: its lead, its teammates and
// its worst member.
func (r row) unit() fleet.Unit {
	if len(r.members) == 0 {
		return fleet.Unit{Lead: r.s, Worst: r.s}
	}
	return fleet.Unit{Lead: r.members[0], Teammates: r.members[1:], Worst: r.worst}
}

// target is the session Enter jumps to: a group row's worst member, else
// the row's own session.
func (r row) target() model.Session {
	if r.kind == rowGroup {
		return r.worst
	}
	return r.s
}

// refreshMsg is the result of one refresh of chain.
type refreshMsg struct {
	sessions   []model.Session
	counts     map[string]model.OpenCount
	tasks      map[string]model.Task
	reviews    map[string]string
	budget     Budget
	names      map[string]string
	live       []tmux.Pane
	err        error
	countsErr  error
	tasksErr   error
	reviewsErr error
	budgetErr  error
	chain      int
}

// tickMsg starts the next refresh of chain.
type tickMsg struct{ chain int }

// pauseConfirmMsg pauses the task of the y taken as seq once burstGap has
// passed with no other key.
type pauseConfirmMsg struct{ seq int }

// pausedMsg is the result of one pause.
type pausedMsg struct {
	line string
	err  error
}

// pauseTimeout bounds one call of Source.Pause; the store's runs a pause to
// its own limits (git's, the repository's hooks).
const pauseTimeout = 6 * time.Minute

// Notices of the P key.
const (
	noticeNoPause         = "pausing is not available here"
	noticeNothingToPause  = "nothing to pause: no limit forecast"
	noticePauseCanceled   = "pause canceled"
	noticePausing         = "a pause is running; wait for its result"
	noticeSuggestionMoved = "the suggestion changed"
)

// childMsg is a message of the decide view shown as child gen. Every
// message a child's commands send comes back wrapped in one, so a closed
// child's late messages (its one-second tick, a read in flight) are told
// apart and dropped instead of reaching the next child.
type childMsg struct {
	gen int
	msg tea.Msg
}

// noticeNoAnswer is the d key's notice when the dashboard has no decide
// factory: curia ui sets none inside an agent session (spec section 7).
const noticeNoAnswer = "decisions cannot be answered inside an agent session (CLAUDECODE is set)"

// Notices of the team and board keys.
const (
	noticeNoBoard   = "the task board opens in the popup (curia ui)"
	noticePickTask  = "pick a task, then n"
	noticeKillGroup = "expand the team to kill one pane"
)

// Notices of the r key. noticeNoReview is said when the dashboard has no
// review factory and no reason for it: curia ui sets none inside an agent
// session (spec section 7).
const (
	noticeNoReview        = "reviews cannot be opened inside an agent session (CLAUDECODE is set)"
	noticeNoReviewWaiting = "no review is waiting for this session"
	noticeReviewSidebar   = "open the popup to review"
)

// New returns the dashboard over src. tm resolves window names and performs
// jumps and kills; cfg gives the context thresholds; now is the clock for
// the age column.
func New(src Source, tm tmux.Client, cfg config.Config, now func() time.Time, mode Mode) Model {
	return Model{src: src, tm: tm, cfg: cfg, now: now, mode: mode}
}

// WithDecide returns m with the d key enabled in the popup: on a row with
// open tickets, d shows the model open builds for that session's id until
// it sends DecideClosedMsg. Without it, d says why it cannot answer.
func (m Model) WithDecide(open func(sessionID string) tea.Model) Model {
	m.open = open
	return m
}

// WithoutDecide returns m with the d key disabled, saying why when pressed.
func (m Model) WithoutDecide(why string) Model {
	m.open, m.noAnswer = nil, why
	return m
}

// WithoutKill returns m with x (kill a pane) disabled, saying why when
// pressed.
func (m Model) WithoutKill(why string) Model {
	m.noKill = why
	return m
}

// WithBoard returns m with the t key enabled: t shows the model open builds
// until it sends BoardClosedMsg, and n shows it with the notice "pick a
// task, then n". Without it, both say the board is in the popup.
func (m Model) WithBoard(open func() tea.Model) Model {
	m.board = open
	return m
}

// WithReview returns m with the r key enabled in the popup: on a row whose
// task has a pending review, r shows the model open builds for that task's
// id until it sends ReviewClosedMsg.
func (m Model) WithReview(open func(taskID string) tea.Model) Model {
	m.review = open
	return m
}

// WithoutReview returns m with the r key disabled, saying why when pressed.
func (m Model) WithoutReview(why string) Model {
	m.review, m.noReview = nil, why
	return m
}

// WithPause returns m with the P key enabled in the popup: while the
// Budget suggests a task to pause, a notice line offers it, P asks to
// confirm and a y pauses it through Source.Pause. Without it, P says why
// not.
func (m Model) WithPause() Model {
	m.pauseOn, m.noPause = true, ""
	return m
}

// WithoutPause returns m with the P key disabled, saying why when pressed.
func (m Model) WithoutPause(why string) Model {
	m.pauseOn, m.noPause = false, why
	return m
}

// Init starts the first refresh.
func (m Model) Init() tea.Cmd {
	return m.refresh()
}

// refresh reads the Source and the window names off the event loop; the
// popup also reads the open decision counts, the tasks, the pending
// reviews and the budget.
func (m Model) refresh() tea.Cmd {
	src, tm, chain, popup := m.src, m.tm, m.chain, m.mode == ModePopup
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		msg := refreshMsg{chain: chain}
		msg.sessions, msg.err = src.Sessions(ctx)
		if popup {
			msg.counts, msg.countsErr = src.OpenDecisions(ctx)
			msg.tasks, msg.tasksErr = src.Tasks(ctx)
			msg.reviews, msg.reviewsErr = src.Reviews(ctx)
			msg.budget, msg.budgetErr = src.Budget(ctx)
		}
		msg.names, msg.live = windowNames(tm)
		return msg
	}
}

// windowNames maps each window id on the server to its name, and returns
// the panes it listed; nil when tmux cannot list its panes, so every window
// shows its stored id.
func windowNames(tm tmux.Client) (map[string]string, []tmux.Pane) {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	panes, err := tm.ListPanes(ctx)
	if err != nil {
		return nil, nil
	}
	names := make(map[string]string, len(panes))
	for _, p := range panes {
		names[p.WindowID] = p.WindowName
	}
	return names, panes
}

// Update handles one message. While the decide view or the board is shown,
// every message goes to it (updateChild).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if pm, ok := msg.(pausedMsg); ok { // a pause's result, whatever is shown
		return m.applyPaused(pm), nil
	}
	if _, ok := msg.(childMsg); ok || m.child != nil {
		return m.updateChild(msg)
	}
	switch msg := msg.(type) {
	case refreshMsg:
		if msg.chain != m.chain {
			return m, nil
		}
		chain := m.chain
		return m.applyRefresh(msg), tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{chain: chain} })
	case tickMsg:
		if msg.chain != m.chain {
			return m, nil
		}
		return m, m.refresh()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m.reselect(), nil
	case tea.KeyMsg:
		return m.keyMsg(msg)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case pauseConfirmMsg:
		return m.applyPauseConfirm(msg)
	}
	return m, nil
}

// keyMsg handles a key message: the time it came is recorded for the burst
// rule (inBurst), a key while a pause's "y" waits out burstGap is
// keyAfterPauseConfirm's, and several runes of one read are taken one by
// one.
func (m Model) keyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.prevKeyAt, m.keyAt = m.keyAt, m.now()
	if !m.confirmAt.IsZero() {
		return m.keyAfterPauseConfirm()
	}
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
		return m.updateRunes(msg)
	}
	return m.updateKey(msg)
}

// applyRefresh keeps the last good sessions, counts, tasks, reviews and
// budget when a read fails, and shows the first error.
func (m Model) applyRefresh(msg refreshMsg) Model {
	m.names, m.live = msg.names, msg.live
	m.err = cmp.Or(msg.err, msg.countsErr, msg.tasksErr, msg.reviewsErr, msg.budgetErr)
	if msg.err == nil {
		m.sessions = msg.sessions
	}
	if msg.countsErr == nil {
		m.counts = msg.counts
	}
	if msg.tasksErr == nil {
		m.tasks = msg.tasks
	}
	if msg.reviewsErr == nil {
		m.reviews = msg.reviews
	}
	if msg.budgetErr == nil {
		m.budget = msg.budget
		if m.pauseAsk != (pauseSuggestion{}) && m.budget.suggestion() != m.pauseAsk {
			m = m.cancelPause(m.suggestionMoved())
		}
	}
	return m.reselect()
}

// openDecide shows the decide view for the selected session's tickets. It
// does nothing in the sidebar, which reads no counts, and says why when
// the session has no open ticket or the dashboard has no factory. The view
// gets the window size, and its commands are wrapped as child gen.
func (m Model) openDecide() (Model, tea.Cmd) {
	r, ok := m.current()
	if m.mode != ModePopup || !ok {
		return m, nil
	}
	if c := m.counts[r.s.ID]; c.Blocking+c.Assume == 0 {
		m.notice = "no open decisions"
		return m, nil
	}
	if m.open == nil {
		m.notice = noticeNoAnswer
		if m.noAnswer != "" {
			m.notice = m.noAnswer
		}
		return m, nil
	}
	return m.showChild(m.open(r.s.ID))
}

// openReview shows the review view for the selected row's task: on a
// group row the lead's. It says why not in the sidebar, which reads no
// reviews, without a factory, and when the last good Reviews read has no
// pending review for the task. It opens only once the row has been on
// screen, as it is, for the dwell. An r too soon restarts the wait, now as
// long as the quiet period, so the repeats of a held r that starts too
// soon never open the view (a held key's first repeat comes 375 to 660 ms
// after the press; Rulings 67, 88). From then on every key goes to the
// view, whose own guards decide what acts.
func (m Model) openReview() (Model, tea.Cmd) {
	r, ok := m.current()
	switch {
	case m.mode != ModePopup:
		m.notice = noticeReviewSidebar
		return m, nil
	case !ok:
		return m, nil
	case m.review == nil:
		m.notice = cmp.Or(m.noReview, noticeNoReview)
		return m, nil
	}
	if _, pending := m.reviews[r.s.TaskID]; r.s.TaskID == "" || !pending {
		m.notice = noticeNoReviewWaiting
		return m, nil
	}
	if now := m.now(); now.Sub(m.rowSince) < max(m.rowWait, dwell) {
		m.rowSince, m.rowWait, m.notice = now, quietPeriod, noticeTooSoon
		return m, nil
	}
	return m.showChild(m.review(r.s.TaskID))
}

// openBoard shows the task board, with notice on its status line when it
// is not empty, or says why there is none.
func (m Model) openBoard(notice string) (Model, tea.Cmd) {
	if m.board == nil {
		m.notice = noticeNoBoard
		return m, nil
	}
	child := m.board()
	if b, ok := child.(BoardModel); ok && notice != "" {
		child = b.WithNotice(notice)
	}
	return m.showChild(child)
}

// showChild shows child, the decide view, the board or the review view,
// until it closes.
// It gets the window size, and its commands are wrapped as child gen.
func (m Model) showChild(child tea.Model) (Model, tea.Cmd) {
	m.childGen++
	cmds := []tea.Cmd{child.Init()}
	if m.width > 0 || m.height > 0 {
		var cmd tea.Cmd
		child, cmd = child.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
		cmds = append(cmds, cmd)
	}
	m.child = child
	return m, wrapChild(m.childGen, tea.Batch(cmds...))
}

// updateChild handles a message while a child (the decide view, the board
// or the review view) is shown, and a child's wrapped message at any time.
// A wrapped message of a child that is no longer shown is dropped, which
// also ends that child's tick chain. DecideClosedMsg, BoardClosedMsg (also
// sent after the board's jump) or ReviewClosedMsg closes the child and
// starts a new refresh chain, so a tick of the chain that ran before d, t
// or r (which the child got and ignored) never doubles it; the table is
// back on screen, so r's dwell starts again. Any other message goes to the
// child, a window size also to the dashboard, which keeps its layout for
// when the child closes.
func (m Model) updateChild(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cm, ok := msg.(childMsg); ok {
		if m.child == nil || cm.gen != m.childGen {
			return m, nil
		}
		msg = cm.msg
	}
	switch msg := msg.(type) {
	case DecideClosedMsg, BoardClosedMsg, ReviewClosedMsg:
		m.child, m.notice, m.rowSince, m.rowWait = nil, "", m.now(), dwell
		m.chain++
		return m, m.refresh()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m = m.reselect()
	}
	var cmd tea.Cmd
	m.child, cmd = m.child.Update(msg)
	return m, wrapChild(m.childGen, cmd)
}

// teaPath is Bubble Tea's import path. A message of a type it defines is
// one its runtime acts on and never hands to the model: tea.Quit (ctrl+c),
// the exec of tea.ExecProcess (the review view's diff tool), the screen
// and mouse modes.
var teaPath = reflect.TypeFor[tea.QuitMsg]().PkgPath()

// wrapChild wraps the messages cmd sends as child gen's. A batch is
// wrapped command by command, and a runtime message of Bubble Tea's own
// (see teaPath) passes through, so the program still quits and runs the
// diff tool as the child asks. A sequence would pass through unwrapped;
// no child uses one.
func wrapChild(gen int, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		switch msg := cmd().(type) {
		case nil:
			return nil
		case tea.BatchMsg:
			out := make(tea.BatchMsg, len(msg))
			for i, c := range msg {
				out[i] = wrapChild(gen, c)
			}
			return out
		default:
			if reflect.TypeOf(msg).PkgPath() == teaPath {
				return msg
			}
			return childMsg{gen: gen, msg: msg}
		}
	}
}

// reselect recomputes the rows and puts the cursor back on the selected
// row; when it is gone, on the row of the same session (a team expanded or
// collapsed), on its team's row (a teammate's team collapsed), or on the
// row nearest its old place.
func (m Model) reselect() Model {
	m.rows = m.buildRows() // a fresh slice: earlier copies of the model keep theirs
	if i := m.findSelected(); i >= 0 {
		m.cursor = i
	}
	return m.moveTo(m.cursor)
}

// findSelected is the index of the selected row as reselect looks for it,
// or -1.
func (m Model) findSelected() int {
	if i := slices.IndexFunc(m.rows, func(r row) bool { return r.key() == m.selected }); i >= 0 {
		return i
	}
	if i := slices.IndexFunc(m.rows, func(r row) bool { return r.s.ID == m.selected.id }); i >= 0 {
		return i
	}
	if m.selTeam == "" {
		return -1
	}
	return slices.IndexFunc(m.rows, func(r row) bool { return r.team == m.selTeam })
}

// buildRows lays out the units of the sessions that pass the filter, in
// the Source's order or, with the burn sort on, by burn: a solo session as
// one row, a team as its group row or, when expanded, its lead's row and
// its teammates' rows. A team is shown when any member passes the filter,
// and is shown expanded while a filter is set.
func (m Model) buildRows() []row {
	units := sourceUnits(m.sessions)
	if m.burnSort {
		units = burnOrder(units, m.budget.Burn)
	}
	var rows []row
	for _, u := range units {
		members := append([]model.Session{u.Lead}, u.Teammates...)
		if !slices.ContainsFunc(members, func(s model.Session) bool { return matches(s, m.filter) }) {
			continue
		}
		team := u.Lead.TeamID
		switch {
		case len(u.Teammates) == 0:
			rows = append(rows, row{kind: rowSolo, s: u.Lead})
		case m.filter != "" || m.expanded[team]:
			rows = append(rows, row{kind: rowLead, s: u.Lead, team: team})
			for _, s := range u.Teammates {
				rows = append(rows, row{kind: rowMate, s: s, team: team})
			}
		default:
			rows = append(rows, row{kind: rowGroup, s: u.Lead, team: team, worst: u.Worst, members: members})
		}
	}
	return rows
}

// sourceUnits groups the sessions into fleet.Units, kept in the Source's
// order: a unit stands where its first member is. For the ranked sessions
// the store source gives, that is its worst member, as fleet.Units orders.
func sourceUnits(sessions []model.Session) []fleet.Unit {
	at := make(map[string]int, len(sessions))
	for i, s := range sessions {
		if _, ok := at[s.ID]; !ok {
			at[s.ID] = i
		}
	}
	first := func(u fleet.Unit) int {
		i := at[u.Lead.ID]
		for _, s := range u.Teammates {
			i = min(i, at[s.ID])
		}
		return i
	}
	units := fleet.Units(sessions)
	slices.SortStableFunc(units, func(a, b fleet.Unit) int { return cmp.Compare(first(a), first(b)) })
	return units
}

// burnOrder orders units by their members' summed burn, highest first.
// A unit none of whose members has a burn follows those that have one, and
// such units, like ties, keep their order. A burn that is not a finite,
// positive number counts as zero.
func burnOrder(units []fleet.Unit, burn map[string]float64) []fleet.Unit {
	type burning struct {
		u   fleet.Unit
		has bool
		usd float64
	}
	bs := make([]burning, len(units))
	for i, u := range units {
		bs[i].u = u
		for _, s := range append([]model.Session{u.Lead}, u.Teammates...) {
			v, ok := burn[s.ID]
			bs[i].has = bs[i].has || ok
			if ok && v > 0 && !math.IsInf(v, 1) {
				bs[i].usd += v
			}
		}
	}
	slices.SortStableFunc(bs, func(a, b burning) int {
		if a.has != b.has {
			if a.has {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.usd, a.usd)
	})
	out := make([]fleet.Unit, len(bs))
	for i, b := range bs {
		out[i] = b.u
	}
	return out
}

// matches reports whether the repo cell as shown ("repo@branch", the repo
// as its basename), the branch, the last update or the team member name
// contains filter, ignoring case.
func matches(s model.Session, filter string) bool {
	f := strings.ToLower(filter)
	for _, field := range []string{repoText(s), s.Branch, s.LastUpdate, s.TeamMember} {
		if strings.Contains(strings.ToLower(field), f) {
			return true
		}
	}
	return false
}

// moveTo selects row i, clamped to the rows, and scrolls it into view.
// When the selected row, or whether its task's review is waiting, differs
// from before, r's dwell starts again.
func (m Model) moveTo(i int) Model {
	m.cursor = max(min(i, len(m.rows)-1), 0)
	m.selected, m.selTeam = rowKey{}, ""
	face := ""
	if len(m.rows) > 0 {
		r := m.rows[m.cursor]
		m.selected, m.selTeam = r.key(), r.team
		face = field(r.s.ID) + strconv.FormatBool(r.kind == rowGroup) + field(r.s.TaskID) + field(m.reviews[r.s.TaskID])
	}
	if face != m.rowFace {
		m.rowFace, m.rowSince, m.rowWait = face, m.now(), dwell
	}
	if n := m.visibleRows(); n > 0 {
		m.offset = min(m.offset, m.cursor)
		m.offset = max(m.offset, m.cursor-n+1)
		m.offset = max(min(m.offset, len(m.rows)-n), 0)
	}
	return m
}

// visibleRows is how many rows fit between the header and the status line,
// and the pause notice line when it is shown; 0 means no limit (the height
// is not known yet).
func (m Model) visibleRows() int {
	if m.height <= 0 {
		return 0
	}
	if m.pauseNotice() != "" {
		return max(m.height-3, 1)
	}
	return max(m.height-2, 1)
}

// current returns the selected row, and false when there are no rows.
func (m Model) current() (row, bool) {
	if len(m.rows) == 0 {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

// shownSessions is how many sessions the rows stand for.
func (m Model) shownSessions() int {
	n := 0
	for _, r := range m.rows {
		n += max(len(r.members), 1)
	}
	return n
}

// toggleTeam expands the team of the selected group row, or collapses that
// of the selected lead row; on any other row it does nothing.
func (m Model) toggleTeam() Model {
	r, ok := m.current()
	if !ok || (r.kind != rowGroup && r.kind != rowLead) {
		return m
	}
	exp := maps.Clone(m.expanded)
	if exp == nil {
		exp = map[string]bool{}
	}
	if r.kind == rowGroup {
		exp[r.team] = true
	} else {
		delete(exp, r.team)
	}
	m.expanded = exp
	return m.reselect()
}

// updateRunes handles keys typed faster than the program reads them, which
// arrive as one message with several runes, one rune at a time; a "/"
// among them sends the rest into the filter. A "d", "t", "n" or "r" that
// opens a view, and a "P" that opens the pause prompt, drop the rest: the
// user has not seen the view or the prompt yet. Into an open pause prompt
// such a read cancels it: a y in a burst never confirms.
func (m Model) updateRunes(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.pauseAsk != (pauseSuggestion{}) && m.pausing == "" {
		m.notice = ""
		return m.cancelPause(noticePauseCanceled), nil
	}
	var cmds []tea.Cmd
	for _, r := range msg.Runes {
		var cmd tea.Cmd
		m, cmd = m.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt})
		cmds = append(cmds, cmd)
		if m.child != nil || m.pauseAsk != (pauseSuggestion{}) {
			break
		}
	}
	return m, tea.Batch(cmds...)
}

func (m Model) updateKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	m.notice = ""
	switch {
	case msg.Type == tea.KeyCtrlC:
		return m, tea.Quit
	case m.pausing != "":
		m.notice = noticePausing
		return m, nil
	case m.killPane != "":
		return m.confirmKill(msg), nil
	case m.pauseAsk != (pauseSuggestion{}):
		return m.pauseKey(msg)
	case m.filtering:
		return m.filterKey(msg), nil
	case msg.Paste: // pasted text is never a command
		return m, nil
	}
	return m.commandKey(msg)
}

// commandKey handles a key on the table.
func (m Model) commandKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "j", "down":
		return m.moveTo(m.cursor + 1), nil
	case "k", "up":
		return m.moveTo(m.cursor - 1), nil
	case "enter":
		return m.jump()
	case " ":
		return m.toggleTeam(), nil
	case "/":
		m.filtering = true
	case "x":
		return m.askKill(), nil
	case "d":
		return m.openDecide()
	case "t":
		return m.openBoard("")
	case "n":
		return m.openBoard(noticePickTask)
	case "r":
		return m.openReview()
	case "b":
		return m.toggleBurn(), nil
	case "P":
		return m.askPause(), nil
	}
	return m, nil
}

// toggleBurn turns the burn sort on or off in the popup; the selection
// stays on its row. The sidebar has no burn sort.
func (m Model) toggleBurn() Model {
	if m.mode != ModePopup {
		return m
	}
	m.burnSort = !m.burnSort
	return m.reselect()
}

// filterKey edits the filter: typed text is appended, Backspace deletes,
// Enter keeps the filter, Esc clears it; the arrows still move.
func (m Model) filterKey(msg tea.KeyMsg) Model {
	switch msg.Type {
	case tea.KeyEnter:
		m.filtering = false
		return m
	case tea.KeyEsc:
		m.filtering, m.filter = false, ""
	case tea.KeyBackspace:
		r := []rune(m.filter)
		m.filter = string(r[:max(len(r)-1, 0)])
	case tea.KeyRunes, tea.KeySpace:
		m.filter += string(msg.Runes)
	case tea.KeyDown:
		return m.moveTo(m.cursor + 1)
	case tea.KeyUp:
		return m.moveTo(m.cursor - 1)
	default:
		return m
	}
	return m.reselect()
}

// jump switches the tmux client to the selected row's pane (a group row's
// worst member's); in popup mode a successful jump quits.
func (m Model) jump() (Model, tea.Cmd) {
	r, ok := m.current()
	if !ok {
		return m, nil
	}
	s := r.target()
	if m.notice = unreachable(s, m.live); m.notice != "" {
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	if err := m.tm.SwitchTo(ctx, s.Pane); err != nil {
		m.notice = fmt.Sprintf("jump failed: %v", err)
		return m, nil
	}
	if m.mode == ModePopup {
		return m, tea.Quit
	}
	return m, nil
}

// askKill asks to confirm killing the selected session's pane. The pane is
// fixed now, so a refresh that reorders the rows cannot change the target.
// A group row stands for several panes, so it is refused.
func (m Model) askKill() Model {
	r, ok := m.current()
	switch {
	case m.noKill != "":
		m.notice = m.noKill
		return m
	case !ok:
		return m
	case r.kind == rowGroup:
		m.notice = noticeKillGroup
		return m
	}
	if m.notice = unreachable(r.s, m.live); m.notice == "" {
		m.killPane = r.s.Pane
	}
	return m
}

// unreachable says why s's pane cannot be jumped to or killed, or is empty
// when it can. A stale session's pane is gone; after a tmux server restart
// its id may name an unrelated pane, so it is never used. A session
// recorded on another tmux server than the one whose panes are live
// (fleet.OtherServer) has a pane id that names another pane here.
func unreachable(s model.Session, live []tmux.Pane) string {
	switch {
	case s.Pane == "":
		return "not in tmux"
	case s.State == model.StateStale:
		return "pane gone"
	case fleet.OtherServer(live, s):
		return noticeOtherServer
	default:
		return ""
	}
}

// noticeOtherServer refuses a jump to, or a kill of, the pane of a session
// recorded on another tmux server.
const noticeOtherServer = "on another tmux server: open the dashboard there"

// confirmKill kills the pane on "y"; any other key cancels. The row
// disappears or changes on a later refresh; the store is not edited here.
func (m Model) confirmKill(msg tea.KeyMsg) Model {
	pane := m.killPane
	m.killPane = ""
	if msg.String() != "y" {
		m.notice = "kill canceled"
		return m
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	if err := m.tm.KillPane(ctx, pane); err != nil {
		m.notice = fmt.Sprintf("kill failed: %v", err)
		return m
	}
	m.notice = "killed pane " + pane
	return m
}

// updateMouse selects and jumps to the row under a left click. A click
// while a kill waits for confirmation cancels it.
func (m Model) updateMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
		return m, nil
	}
	m.notice = ""
	switch {
	case m.pausing != "":
		m.notice = noticePausing
		return m, nil
	case m.pauseAsk != (pauseSuggestion{}):
		return m.cancelPause(noticePauseCanceled), nil
	case m.killPane != "":
		m.killPane, m.notice = "", "kill canceled"
		return m, nil
	}
	i := m.offset + msg.Y - 1 // line 0 is the header
	if msg.Y < 1 || i >= len(m.rows) || (m.visibleRows() > 0 && msg.Y > m.visibleRows()) {
		return m, nil
	}
	return m.moveTo(i).jump()
}

// askPause opens the prompt to pause the suggested task, as the notice line
// names it now. It says why not in the sidebar, without WithPause (the
// guard refused: read-only) and when nothing is suggested.
func (m Model) askPause() Model {
	switch {
	case m.mode != ModePopup || !m.pauseOn:
		m.notice = cmp.Or(m.noPause, noticeNoPause)
	case m.budget.PauseTask == "":
		m.notice = noticeNothingToPause
	default:
		m.pauseAsk, m.pauseSince, m.pauseWait = m.budget.suggestion(), m.now(), dwell
	}
	return m
}

// pauseKey takes a "y" that came as a key of its own, the wait after the
// prompt opened; it pauses once burstGap has passed with no other key
// (applyPauseConfirm). A "y" too soon, or within burstGap after another key
// (the burst rule, inBurst), is ignored and restarts the wait, now as long
// as the quiet period, so the repeats of a held key and a "y" pasted
// without bracketing never confirm. Any other key and a paste cancel.
func (m Model) pauseKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if msg.Paste || msg.String() != "y" {
		return m.cancelPause(noticePauseCanceled), nil
	}
	if now := m.now(); now.Sub(m.pauseSince) < m.pauseWait || inBurst(now, m.prevKeyAt) {
		m.pauseSince, m.pauseWait, m.notice = now, quietPeriod, noticeTooSoon
		return m, nil
	}
	m.confirmAt, m.confirmSeq = m.now(), m.confirmSeq+1
	seq := m.confirmSeq
	return m, tea.Tick(burstGap, func(time.Time) tea.Msg { return pauseConfirmMsg{seq: seq} })
}

// keyAfterPauseConfirm handles a key that came while a taken "y" waits out
// burstGap. Within burstGap of the "y" it arrived with it (the burst rule):
// the "y" is refused and the prompt's wait restarts, the prompt still open.
// Later (the wait's message is late) the pause starts and the key is
// ignored.
func (m Model) keyAfterPauseConfirm() (Model, tea.Cmd) {
	if inBurst(m.keyAt, m.confirmAt) {
		m.confirmAt = time.Time{}
		m.pauseSince, m.pauseWait, m.notice = m.keyAt, quietPeriod, noticeTooSoon
		return m, nil
	}
	return m.startPause()
}

// applyPauseConfirm starts the pause of the "y" msg waited for, when it
// still waits.
func (m Model) applyPauseConfirm(msg pauseConfirmMsg) (Model, tea.Cmd) {
	if msg.seq != m.confirmSeq || m.confirmAt.IsZero() {
		return m, nil
	}
	return m.startPause()
}

// startPause closes the prompt and pauses the task it named, off the update
// loop. A refresh that changed the suggestion has closed the prompt already
// (applyRefresh), and the store's Pause checks it again.
func (m Model) startPause() (Model, tea.Cmd) {
	ask := m.pauseAsk
	m.confirmAt, m.pauseAsk = time.Time{}, pauseSuggestion{}
	m.pausing = ask.task
	src := m.src
	return m, call(pauseTimeout, func(ctx context.Context) tea.Msg {
		line, err := src.Pause(ctx, ask.task)
		return pausedMsg{line: line, err: err}
	})
}

// applyPaused shows a pause's line, or its error, as the notice; nothing
// else in the view changes.
func (m Model) applyPaused(msg pausedMsg) Model {
	m.pausing, m.notice = "", msg.line
	if msg.err != nil {
		m.notice = msg.err.Error()
	}
	return m
}

// cancelPause closes the pause prompt, and a "y" waiting with it, with
// notice.
func (m Model) cancelPause(notice string) Model {
	m.pauseAsk, m.confirmAt, m.notice = pauseSuggestion{}, time.Time{}, notice
	return m
}

// suggestionMoved is the notice of a prompt canceled because the
// suggestion changed: what is suggested now, if anything.
func (m Model) suggestionMoved() string {
	if m.budget.PauseTask == "" {
		return noticeSuggestionMoved + ": nothing to pause now"
	}
	return noticeSuggestionMoved + ": now " + m.budget.PauseLabel
}

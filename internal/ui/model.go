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
	"slices"
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
	names    map[string]string          // window id to window name; replaced, never mutated
	expanded map[string]bool            // team ids shown expanded; replaced, never mutated
	rows     []row                      // the units that pass the filter, as rows
	cursor   int                        // index of the selected row
	selected rowKey                     // the selected row, kept across refreshes
	selTeam  string                     // the selected row's team, to find its group row once collapsed
	offset   int                        // index of the first row on screen

	width, height int // 0 until the first WindowSizeMsg: no limit

	filter    string
	filtering bool   // typing into the filter
	killPane  string // pane awaiting kill confirmation, if any
	notice    string // message for the last key, cleared by the next one
	err       error  // the last refresh's error, cleared by a good one

	chain int // the refresh chain that is live; messages of earlier chains are dropped

	open     func(sessionID string) tea.Model // builds the decide view; nil: d cannot answer
	noAnswer string                           // why d cannot answer; "": inside an agent session
	board    func() tea.Model                 // builds the task board; nil: t and n say why
	child    tea.Model                        // the decide view or the board while it is shown
	childGen int                              // which child is (or was last) shown
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
	sessions  []model.Session
	counts    map[string]model.OpenCount
	tasks     map[string]model.Task
	names     map[string]string
	err       error
	countsErr error
	tasksErr  error
	chain     int
}

// tickMsg starts the next refresh of chain.
type tickMsg struct{ chain int }

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

// WithBoard returns m with the t key enabled: t shows the model open builds
// until it sends BoardClosedMsg, and n shows it with the notice "pick a
// task, then n". Without it, both say the board is in the popup.
func (m Model) WithBoard(open func() tea.Model) Model {
	m.board = open
	return m
}

// Init starts the first refresh.
func (m Model) Init() tea.Cmd {
	return m.refresh()
}

// refresh reads the Source and the window names off the event loop; the
// popup also reads the open decision counts and the tasks.
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
		}
		msg.names = windowNames(tm)
		return msg
	}
}

// windowNames maps each window id on the server to its name; nil when tmux
// cannot list its panes, so every window shows its stored id.
func windowNames(tm tmux.Client) map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
	defer cancel()
	panes, err := tm.ListPanes(ctx)
	if err != nil {
		return nil
	}
	names := make(map[string]string, len(panes))
	for _, p := range panes {
		names[p.WindowID] = p.WindowName
	}
	return names
}

// Update handles one message. While the decide view or the board is shown,
// every message goes to it (updateChild).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			return m.updateRunes(msg)
		}
		return m.updateKey(msg)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	}
	return m, nil
}

// applyRefresh keeps the last good sessions, counts and tasks when a read
// fails, and shows the first error.
func (m Model) applyRefresh(msg refreshMsg) Model {
	m.names, m.err = msg.names, cmp.Or(msg.err, msg.countsErr, msg.tasksErr)
	if msg.err == nil {
		m.sessions = msg.sessions
	}
	if msg.countsErr == nil {
		m.counts = msg.counts
	}
	if msg.tasksErr == nil {
		m.tasks = msg.tasks
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

// showChild shows child, the decide view or the board, until it closes.
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

// updateChild handles a message while the decide view or the board is
// shown, and a child's wrapped message at any time. A wrapped message of a
// child that is no longer shown is dropped, which also ends that child's
// tick chain. DecideClosedMsg or BoardClosedMsg (also sent after the
// board's jump) closes the child and starts a new refresh chain, so a tick
// of the chain that ran before d or t (which the child got and ignored)
// never doubles it. Any other message goes to the child, a window size
// also to the dashboard, which keeps its layout for when the child closes.
func (m Model) updateChild(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cm, ok := msg.(childMsg); ok {
		if m.child == nil || cm.gen != m.childGen {
			return m, nil
		}
		msg = cm.msg
	}
	switch msg := msg.(type) {
	case DecideClosedMsg, BoardClosedMsg:
		m.child, m.notice = nil, ""
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

// wrapChild wraps the messages cmd sends as child gen's. A batch is
// wrapped command by command and tea.Quit (ctrl+c) passes through, so the
// program still runs and quits as the child asks; the decide view and the
// board use no other runtime command.
func wrapChild(gen int, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		switch msg := cmd().(type) {
		case nil:
			return nil
		case tea.QuitMsg:
			return msg
		case tea.BatchMsg:
			out := make(tea.BatchMsg, len(msg))
			for i, c := range msg {
				out[i] = wrapChild(gen, c)
			}
			return out
		default:
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

// buildRows lays out the units of the sessions that pass the filter: a
// solo session as one row, a team as its group row or, when expanded, its
// lead's row and its teammates' rows. A team is shown when any member
// passes the filter, and is shown expanded while a filter is set.
func (m Model) buildRows() []row {
	var rows []row
	for _, u := range sourceUnits(m.sessions) {
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
func (m Model) moveTo(i int) Model {
	m.cursor = max(min(i, len(m.rows)-1), 0)
	m.selected, m.selTeam = rowKey{}, ""
	if len(m.rows) > 0 {
		r := m.rows[m.cursor]
		m.selected, m.selTeam = r.key(), r.team
	}
	if n := m.visibleRows(); n > 0 {
		m.offset = min(m.offset, m.cursor)
		m.offset = max(m.offset, m.cursor-n+1)
		m.offset = max(min(m.offset, len(m.rows)-n), 0)
	}
	return m
}

// visibleRows is how many rows fit between the header and the status line;
// 0 means no limit (the height is not known yet).
func (m Model) visibleRows() int {
	if m.height <= 0 {
		return 0
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
// among them sends the rest into the filter. A "d", "t" or "n" that opens
// the decide view or the board drops the rest: the user has not seen the
// view yet.
func (m Model) updateRunes(msg tea.KeyMsg) (Model, tea.Cmd) {
	var cmds []tea.Cmd
	for _, r := range msg.Runes {
		var cmd tea.Cmd
		m, cmd = m.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt})
		cmds = append(cmds, cmd)
		if m.child != nil {
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
	case m.killPane != "":
		return m.confirmKill(msg), nil
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
	}
	return m, nil
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
	if m.notice = unreachable(s); m.notice != "" {
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
	case !ok:
		return m
	case r.kind == rowGroup:
		m.notice = noticeKillGroup
		return m
	}
	if m.notice = unreachable(r.s); m.notice == "" {
		m.killPane = r.s.Pane
	}
	return m
}

// unreachable says why s's pane cannot be jumped to or killed, or is empty
// when it can. A stale session's pane is gone; after a tmux server restart
// its id may name an unrelated pane, so it is never used.
func unreachable(s model.Session) string {
	switch {
	case s.Pane == "":
		return "not in tmux"
	case s.State == model.StateStale:
		return "pane gone"
	default:
		return ""
	}
}

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
	if m.killPane != "" {
		m.killPane, m.notice = "", "kill canceled"
		return m, nil
	}
	i := m.offset + msg.Y - 1 // line 0 is the header
	if msg.Y < 1 || i >= len(m.rows) || (m.visibleRows() > 0 && msg.Y > m.visibleRows()) {
		return m, nil
	}
	return m.moveTo(i).jump()
}

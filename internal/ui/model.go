// Package ui is the dashboard: a Bubble Tea model that lists every session,
// shown as a tmux popup (curia ui) or in a narrow always-on pane (curia
// sidebar), and jumps to or kills the selected session's pane.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
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
// order. The command's implementation reconciles, lists and ranks.
type Source interface {
	Sessions(ctx context.Context) ([]model.Session, error)
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

	sessions []model.Session   // the last good read, in Source order
	names    map[string]string // window id to window name; replaced, never mutated
	rows     []model.Session   // sessions that pass the filter
	cursor   int               // index of the selected row
	selected string            // id of the selected session, kept across refreshes
	offset   int               // index of the first row on screen

	width, height int // 0 until the first WindowSizeMsg: no limit

	filter    string
	filtering bool   // typing into the filter
	killPane  string // pane awaiting kill confirmation, if any
	notice    string // message for the last key, cleared by the next one
	err       error  // the last refresh's error, cleared by a good one
}

// refreshMsg is the result of one refresh.
type refreshMsg struct {
	sessions []model.Session
	names    map[string]string
	err      error
}

// tickMsg starts the next refresh.
type tickMsg struct{}

// New returns the dashboard over src. tm resolves window names and performs
// jumps and kills; cfg gives the context thresholds; now is the clock for
// the age column.
func New(src Source, tm tmux.Client, cfg config.Config, now func() time.Time, mode Mode) Model {
	return Model{src: src, tm: tm, cfg: cfg, now: now, mode: mode}
}

// Init starts the first refresh.
func (m Model) Init() tea.Cmd {
	return m.refresh()
}

// refresh reads the Source and the window names off the event loop.
func (m Model) refresh() tea.Cmd {
	src, tm := m.src, m.tm
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		sessions, err := src.Sessions(ctx)
		return refreshMsg{sessions: sessions, names: windowNames(tm), err: err}
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

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case refreshMsg:
		return m.applyRefresh(msg), tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
	case tickMsg:
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

// applyRefresh keeps the last good sessions when the Source fails.
func (m Model) applyRefresh(msg refreshMsg) Model {
	m.names, m.err = msg.names, msg.err
	if msg.err == nil {
		m.sessions = msg.sessions
	}
	return m.reselect()
}

// reselect recomputes the filtered rows and puts the cursor back on the
// selected session, or, when it is gone, on the row nearest its old place.
func (m Model) reselect() Model {
	m.rows = nil // a fresh slice: earlier copies of the model keep theirs
	for _, s := range m.sessions {
		if matches(s, m.filter) {
			m.rows = append(m.rows, s)
		}
	}
	for i, s := range m.rows {
		if s.ID == m.selected {
			m.cursor = i
		}
	}
	return m.moveTo(m.cursor)
}

// matches reports whether the repo cell as shown ("repo@branch", the repo
// as its basename), the branch or the last update contains filter, ignoring
// case.
func matches(s model.Session, filter string) bool {
	f := strings.ToLower(filter)
	for _, field := range []string{repoText(s), s.Branch, s.LastUpdate} {
		if strings.Contains(strings.ToLower(field), f) {
			return true
		}
	}
	return false
}

// moveTo selects row i, clamped to the rows, and scrolls it into view.
func (m Model) moveTo(i int) Model {
	m.cursor = max(min(i, len(m.rows)-1), 0)
	m.selected = ""
	if len(m.rows) > 0 {
		m.selected = m.rows[m.cursor].ID
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

// current returns the selected session, and false when there are no rows.
func (m Model) current() (model.Session, bool) {
	if len(m.rows) == 0 {
		return model.Session{}, false
	}
	return m.rows[m.cursor], true
}

// updateRunes handles keys typed faster than the program reads them, which
// arrive as one message with several runes, one rune at a time; a "/"
// among them sends the rest into the filter.
func (m Model) updateRunes(msg tea.KeyMsg) (Model, tea.Cmd) {
	var cmds []tea.Cmd
	for _, r := range msg.Runes {
		var cmd tea.Cmd
		m, cmd = m.updateKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt})
		cmds = append(cmds, cmd)
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
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "j", "down":
		return m.moveTo(m.cursor + 1), nil
	case "k", "up":
		return m.moveTo(m.cursor - 1), nil
	case "enter":
		return m.jump()
	case "/":
		m.filtering = true
	case "x":
		return m.askKill(), nil
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

// jump switches the tmux client to the selected session's pane; in popup
// mode a successful jump quits.
func (m Model) jump() (Model, tea.Cmd) {
	s, ok := m.current()
	if !ok {
		return m, nil
	}
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
func (m Model) askKill() Model {
	s, ok := m.current()
	if !ok {
		return m
	}
	if m.notice = unreachable(s); m.notice == "" {
		m.killPane = s.Pane
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

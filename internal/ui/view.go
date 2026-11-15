package ui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
)

// column is one column of the table.
type column int

const (
	colState column = iota
	colWindow
	colRepo
	colTask
	colModel
	colContext
	colSubagents
	colDecisions
	colUpdate
	colAge
)

// colSpec says how a column is laid out. A flex column shrinks, widest
// first, down to minFlex cells when the table is too wide; a right column
// is right-aligned.
type colSpec struct {
	title       string
	flex, right bool
}

var specs = map[column]colSpec{
	colState:     {title: ""},
	colWindow:    {title: "SESSION:WINDOW", flex: true},
	colRepo:      {title: "REPO@BRANCH", flex: true},
	colTask:      {title: "TASK", flex: true},
	colModel:     {title: "MODEL", flex: true},
	colContext:   {title: "CTX", right: true},
	colSubagents: {title: "SUB", right: true},
	colDecisions: {title: "DEC", right: true},
	colUpdate:    {title: "LAST UPDATE", flex: true},
	colAge:       {title: "AGE", right: true},
}

// The popup's columns by width (Plan 1 live-check defect 13): all of them
// from fullWidth, without MODEL and SUB from midWidth, also without LAST
// UPDATE from narrowWidth, and below it only state, window, TASK (the id
// alone), DEC and AGE. The sidebar always has its four.
var (
	popupColumns = []column{
		colState, colWindow, colRepo, colTask, colModel, colContext, colSubagents, colDecisions, colUpdate, colAge,
	}
	midColumns     = []column{colState, colWindow, colRepo, colTask, colContext, colDecisions, colUpdate, colAge}
	narrowColumns  = []column{colState, colWindow, colRepo, colTask, colContext, colDecisions, colAge}
	tinyColumns    = []column{colState, colWindow, colTask, colDecisions, colAge}
	sidebarColumns = []column{colState, colWindow, colContext, colAge}
)

const (
	fullWidth   = 120
	midWidth    = 90
	narrowWidth = 60
)

const (
	// minFlex is the narrowest a flex column shrinks to.
	minFlex = 3
	// ellipsis ends a truncated cell.
	ellipsis = "…"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	staleStyle  = lipgloss.NewStyle().Faint(true)
	helpStyle   = lipgloss.NewStyle().Faint(true)
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

// levelStyle colors a context percentage: green, yellow, red, or plain when
// the level is unknown.
func levelStyle(l Level) lipgloss.Style {
	s := lipgloss.NewStyle()
	switch l {
	case LevelOK:
		return s.Foreground(lipgloss.Color("2"))
	case LevelWarn:
		return s.Foreground(lipgloss.Color("3"))
	case LevelCrit:
		return s.Foreground(lipgloss.Color("1"))
	default:
		return s
	}
}

// decStyle colors a session's open ticket count: red when any is blocking.
func decStyle(c model.OpenCount) lipgloss.Style {
	if c.Blocking > 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	}
	return lipgloss.NewStyle()
}

// stateIcon is the one-cell icon of a state.
func stateIcon(s model.State) string {
	switch s {
	case model.StateNeedsDecision:
		return "◆"
	case model.StateWaiting:
		return "●"
	case model.StateWorking:
		return "◐"
	case model.StateIdle:
		return "○"
	case model.StateStale:
		return "×"
	case model.StateEnded:
		return "-"
	default:
		return "?"
	}
}

// View renders the header, the rows on screen and the status line, every
// line at most the window width; while the decide view is shown, that view.
func (m Model) View() string {
	if m.child != nil {
		return m.child.View()
	}
	cols, gap := m.columns()
	first, last := m.offset, len(m.rows)
	if n := m.visibleRows(); n > 0 {
		last = min(last, first+n)
	}
	cells := make([][]string, 0, last-first)
	for _, r := range m.rows[first:last] {
		cells = append(cells, m.cells(r, cols))
	}
	widths := fitWidths(cols, m.titles(cols), cells, m.width, gap)

	lines := []string{m.renderHeader(cols, widths, gap)}
	if len(m.rows) == 0 {
		lines = append(lines, m.emptyText())
	}
	for i, c := range cells {
		lines = append(lines, m.renderRow(m.rows[first+i], c, cols, widths, gap, first+i == m.cursor))
	}
	for m.height > 0 && len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, m.renderStatus())
	if m.width > 0 {
		clip := lipgloss.NewStyle().MaxWidth(m.width)
		for i, l := range lines {
			lines[i] = clip.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// columns are the columns at the window's width and the gap between them;
// an unknown width (0) gets every column.
func (m Model) columns() ([]column, int) {
	if m.mode == ModeSidebar {
		return sidebarColumns, 1
	}
	switch w := m.width; {
	case w <= 0 || w >= fullWidth:
		return popupColumns, 2
	case w >= midWidth:
		return midColumns, 2
	case w >= narrowWidth:
		return narrowColumns, 2
	default:
		return tinyColumns, 2
	}
}

// titles are the header cells of cols.
func (m Model) titles(cols []column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = specs[c].title
		if c == colWindow && m.mode == ModeSidebar {
			out[i] = "WINDOW"
		}
	}
	return out
}

// cells are the texts of r's columns. Session and task text is untrusted,
// so every piece of it goes through Cell; the DEC cell is a number of our
// own, empty when zero, and the TASK cell is empty without a task.
func (m Model) cells(r row, cols []column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c {
		case colState:
			out[i] = stateIcon(r.target().State)
		case colWindow:
			out[i] = m.windowCell(r)
		case colTask:
			out[i] = m.taskText(r.s)
		case colDecisions:
			out[i] = decText(m.rowCount(r))
		default:
			out[i] = Cell(m.cellText(r.s, c))
		}
	}
	return out
}

// windowCell is the window cell of r: a group row's is "▸ <lead's window>
// +<teammates>", an expanded lead's "▾ <window>", and a teammate's its
// member name (else its window) indented two cells.
func (m Model) windowCell(r row) string {
	w := Cell(m.windowText(r.s))
	switch r.kind {
	case rowGroup:
		return "▸ " + w + " +" + strconv.Itoa(len(r.members)-1)
	case rowLead:
		return "▾ " + w
	case rowMate:
		if r.s.TeamMember != "" {
			w = Cell(r.s.TeamMember)
		}
		return "  " + w
	default:
		return w
	}
}

// taskText is the TASK cell of s: its task's id and title, the id alone
// below narrowWidth or when the task is not known, and empty without one.
func (m Model) taskText(s model.Session) string {
	if s.TaskID == "" {
		return ""
	}
	id := Cell(s.TaskID)
	t, ok := m.tasks[s.TaskID]
	if !ok || t.Title == "" || (m.width > 0 && m.width < narrowWidth) {
		return id
	}
	return id + " " + Cell(t.Title)
}

// rowCount is the open tickets and undelivered answers of r's session, or
// of all a group row's members together.
func (m Model) rowCount(r row) model.OpenCount {
	if r.kind != rowGroup {
		return m.counts[r.s.ID]
	}
	var sum model.OpenCount
	for _, s := range r.members {
		c := m.counts[s.ID]
		sum.Blocking += c.Blocking
		sum.Assume += c.Assume
		sum.Undelivered += c.Undelivered
	}
	return sum
}

// decText is the number of open tickets of both modes in c, then "Q<n>"
// for its answers not yet delivered (Ruling 89), empty when there are
// neither.
func decText(c model.OpenCount) string {
	var parts []string
	if n := c.Blocking + c.Assume; n > 0 {
		parts = append(parts, strconv.Itoa(n))
	}
	if c.Undelivered > 0 {
		parts = append(parts, "Q"+strconv.Itoa(c.Undelivered))
	}
	return strings.Join(parts, " ")
}

func (m Model) cellText(s model.Session, c column) string {
	switch c {
	case colRepo:
		return repoText(s)
	case colModel:
		return s.Model
	case colContext:
		if s.ContextMax <= 0 {
			return ""
		}
		return fmt.Sprintf("%d%%", contextPct(s.ContextUsed, s.ContextMax))
	case colSubagents:
		return strconv.Itoa(s.SubagentsActive)
	case colUpdate:
		return s.LastUpdate
	default:
		return Age(s.UpdatedAt, m.now())
	}
}

// repoText is "repo@branch" with the basename of the stored repo path
// (Ruling 22), the repo alone without a branch, and empty without a repo.
func repoText(s model.Session) string {
	repo := RepoName(s.Repo)
	if repo == "" {
		return ""
	}
	if s.Branch == "" {
		return repo
	}
	return repo + "@" + s.Branch
}

// RepoName is how views show a stored repository path: its basename
// (Ruling 22), or empty for no repository. curia ls uses it too.
func RepoName(repo string) string {
	if repo == "" {
		return ""
	}
	return filepath.Base(repo)
}

// windowText is "session:window" with the window's current name, or its
// stored id when tmux does not know it; empty outside tmux.
func (m Model) windowText(s model.Session) string {
	if s.TmuxSession == "" && s.TmuxWindow == "" {
		return ""
	}
	window := s.TmuxWindow
	if name := m.names[s.TmuxWindow]; name != "" {
		window = name
	}
	return s.TmuxSession + ":" + window
}

// fitWidths gives each column the width of its widest cell, then, when
// width is positive and the row would be wider, takes cells from the
// widest flex column until it fits or every flex column is at minFlex.
func fitWidths(cols []column, titles []string, rows [][]string, width, gap int) []int {
	w := make([]int, len(titles))
	total := gap * (len(titles) - 1)
	for i, t := range titles {
		w[i] = lipgloss.Width(t)
		for _, r := range rows {
			w[i] = max(w[i], lipgloss.Width(r[i]))
		}
		total += w[i]
	}
	for excess := total - width; width > 0 && excess > 0; excess-- {
		widest := -1
		for i, c := range cols {
			if specs[c].flex && w[i] > minFlex && (widest < 0 || w[i] > w[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		w[widest]--
	}
	return w
}

func (m Model) renderHeader(cols []column, widths []int, gap int) string {
	titles := m.titles(cols)
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fit(titles[i], widths[i], specs[c].right)
	}
	return headerStyle.Render(strings.Join(parts, strings.Repeat(" ", gap)))
}

// renderRow lays out one row: dim when its state is stale, reversed when
// selected, the context cell colored by its level.
func (m Model) renderRow(r row, cells []string, cols []column, widths []int, gap int, selected bool) string {
	s := r.s
	base := lipgloss.NewStyle()
	if r.target().State == model.StateStale {
		base = staleStyle
	}
	if selected {
		base = base.Reverse(true)
	}
	level := ContextLevel(s.ContextUsed, s.ContextMax, m.cfg.ContextWarnPct, m.cfg.ContextCritPct)
	parts := make([]string, len(cols))
	for i, c := range cols {
		st := base
		switch c {
		case colContext:
			st = levelStyle(level).Inherit(base)
		case colDecisions:
			st = decStyle(m.rowCount(r)).Inherit(base)
		}
		parts[i] = st.Render(fit(cells[i], widths[i], specs[c].right))
	}
	return strings.Join(parts, base.Render(strings.Repeat(" ", gap)))
}

func (m Model) emptyText() string {
	if m.filter != "" && len(m.sessions) > 0 {
		return "no sessions match /" + clean(m.filter)
	}
	return "no sessions"
}

// renderStatus is the bottom line: a pending kill, the filter being typed,
// the last key's notice, the last refresh error, or help.
func (m Model) renderStatus() string {
	style, text := helpStyle, m.helpText()
	switch {
	case m.killPane != "":
		style, text = lipgloss.NewStyle(), "kill pane "+m.killPane+"? y kills, any other key cancels"
	case m.filtering:
		style, text = lipgloss.NewStyle(), "/"+m.filter
	case m.notice != "":
		style, text = lipgloss.NewStyle(), m.notice
	case m.err != nil:
		style, text = errorStyle, "refresh failed: "+m.err.Error()
	}
	text = clean(text)
	if m.width > 0 {
		text = truncate(text, m.width)
	}
	return style.Render(text)
}

func (m Model) helpText() string {
	text := fmt.Sprintf("%d sessions", m.shownSessions())
	if m.filter != "" {
		text = fmt.Sprintf("%d of %d sessions match /%s", m.shownSessions(), len(m.sessions), m.filter)
	}
	if m.mode == ModePopup {
		text += "  enter jump  d decisions  t tasks  / filter  x kill  q quit"
	}
	return text
}

// fit truncates s to w cells and pads it to exactly w, on the left when
// right is set.
func fit(s string, w int, right bool) string {
	s = truncate(s, w)
	pad := strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
	if right {
		return pad + s
	}
	return s + pad
}

// truncate cuts s to at most w cells, ending it with an ellipsis when cut.
func truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := lipgloss.Width(string(r))
		if used+rw > w-1 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return b.String() + ellipsis
}

// Cell makes untrusted text safe to print as one table cell: clean(s), or
// "-" for an empty cell. curia ls uses it too.
func Cell(s string) string {
	if s == "" {
		return "-"
	}
	return clean(s)
}

// clean makes untrusted text safe to draw: every character that is not
// printable (control characters such as ESC, C1 codes, tabs, line breaks,
// and invisible format characters such as bidi overrides) becomes '?', so
// no escape sequence reaches the terminal and a row stays one line.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) { // includes the ASCII space
			return r
		}
		return '?'
	}, s)
}

// Age is how long before now t was, in its largest whole unit, such as
// "5m"; empty for the zero time.
func Age(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(now.Sub(t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

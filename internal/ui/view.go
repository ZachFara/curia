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
	colModel
	colContext
	colSubagents
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
	colModel:     {title: "MODEL", flex: true},
	colContext:   {title: "CTX", right: true},
	colSubagents: {title: "SUB", right: true},
	colUpdate:    {title: "LAST UPDATE", flex: true},
	colAge:       {title: "AGE", right: true},
}

var (
	popupColumns   = []column{colState, colWindow, colRepo, colModel, colContext, colSubagents, colUpdate, colAge}
	sidebarColumns = []column{colState, colWindow, colContext, colAge}
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
// line at most the window width.
func (m Model) View() string {
	cols, gap := popupColumns, 2
	if m.mode == ModeSidebar {
		cols, gap = sidebarColumns, 1
	}
	first, last := m.offset, len(m.rows)
	if n := m.visibleRows(); n > 0 {
		last = min(last, first+n)
	}
	cells := make([][]string, 0, last-first)
	for _, s := range m.rows[first:last] {
		cells = append(cells, m.cells(s, cols))
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

// cells are the texts of s's columns. Session text is untrusted, so every
// cell goes through Cell.
func (m Model) cells(s model.Session, cols []column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = Cell(m.cellText(s, c))
	}
	return out
}

func (m Model) cellText(s model.Session, c column) string {
	switch c {
	case colState:
		return stateIcon(s.State)
	case colWindow:
		return m.windowText(s)
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
	if s.Repo == "" {
		return ""
	}
	repo := filepath.Base(s.Repo)
	if s.Branch == "" {
		return repo
	}
	return repo + "@" + s.Branch
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

// renderRow lays out one row: dim when stale, reversed when selected, the
// context cell colored by its level.
func (m Model) renderRow(s model.Session, cells []string, cols []column, widths []int, gap int, selected bool) string {
	base := lipgloss.NewStyle()
	if s.State == model.StateStale {
		base = staleStyle
	}
	if selected {
		base = base.Reverse(true)
	}
	level := ContextLevel(s.ContextUsed, s.ContextMax, m.cfg.ContextWarnPct, m.cfg.ContextCritPct)
	parts := make([]string, len(cols))
	for i, c := range cols {
		st := base
		if c == colContext {
			st = levelStyle(level).Inherit(base)
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
	text := fmt.Sprintf("%d sessions", len(m.rows))
	if m.filter != "" {
		text = fmt.Sprintf("%d of %d sessions match /%s", len(m.rows), len(m.sessions), m.filter)
	}
	if m.mode == ModePopup {
		text += "  enter jump  / filter  x kill  q quit"
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

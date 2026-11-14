package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

const (
	// boardWideMin is the narrowest window that shows four columns; below
	// it the board is one list under status headings.
	boardWideMin = 100
	// boardGap is the space between columns.
	boardGap = 2
	// boardEmpty is shown when no task is on the board.
	boardEmpty = "no tasks; add one with curia task add"
	// boardHelp is the status line when there is nothing else to say.
	boardHelp = "h/l column  j/k move  enter jump  r ready/backlog  n spawn  t back"
)

// boardHeadings are the columns' headings, in boardStatuses order.
var boardHeadings = [boardColumns]string{"BACKLOG", "READY", "RUNNING", "NEEDS DECISION"}

var selectedStyle = lipgloss.NewStyle().Reverse(true)

// View renders the board and the status line, every line at most the
// window width and the whole at most its height. Every card string is
// drawn through sanitize.Line.
func (m BoardModel) View() string {
	w := m.textWidth()
	var lines []string
	switch {
	case !m.loaded:
	case m.empty():
		lines = []string{boardEmpty}
	case m.wide():
		lines = m.columnsView(w)
	default:
		lines = m.listView(w)
	}
	for m.height > 0 && len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, m.statusLine(w))
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

// textWidth is the window width, or defaultWidth before it is known.
func (m BoardModel) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return defaultWidth
}

// wide reports whether the board is drawn as four columns.
func (m BoardModel) wide() bool {
	return m.textWidth() >= boardWideMin
}

// empty reports whether no card is on the board.
func (m BoardModel) empty() bool {
	for _, col := range m.cols {
		if len(col) > 0 {
			return false
		}
	}
	return true
}

// bodyRows is how many lines the board may use above the status line; 0
// means no limit (the height is not known yet).
func (m BoardModel) bodyRows() int {
	if m.height <= 0 {
		return 0
	}
	return max(m.height-1, 1)
}

// heading is column c's heading with its card count.
func (m BoardModel) heading(c int) string {
	return fmt.Sprintf("%s (%d)", boardHeadings[c], len(m.cols[c]))
}

// columnsView is the heading line and the cards, four columns side by side.
func (m BoardModel) columnsView(w int) []string {
	cw := max((w-boardGap*(boardColumns-1))/boardColumns, 1)
	gap := strings.Repeat(" ", boardGap)
	room := 0
	if n := m.bodyRows(); n > 0 {
		room = max(n-1, 1) // below the heading line
	}
	var cells [boardColumns][]string
	head := make([]string, boardColumns)
	rows := 0
	for c := range boardColumns {
		head[c] = headerStyle.Render(fit(m.heading(c), cw, false))
		cells[c] = m.columnCells(c, cw, room)
		rows = max(rows, len(cells[c]))
	}
	lines := []string{strings.Join(head, gap)}
	blank := strings.Repeat(" ", cw)
	for i := range rows {
		parts := make([]string, boardColumns)
		for c := range boardColumns {
			parts[c] = blank
			if i < len(cells[c]) {
				parts[c] = cells[c][i]
			}
		}
		lines = append(lines, strings.Join(parts, gap))
	}
	return lines
}

// columnCells are column c's cards on screen, each cw cells wide: at most
// room (0: all), scrolled to keep the selected card on screen.
func (m BoardModel) columnCells(c, cw, room int) []string {
	cards := m.cols[c]
	first, last := 0, len(cards)
	if room > 0 && last > room {
		if c == m.col {
			first = max(m.row-room+1, 0)
		}
		last = first + room
	}
	out := make([]string, 0, last-first)
	for i, card := range cards[first:last] {
		sel := c == m.col && first+i == m.row
		out = append(out, markCard(fit(cardMark(sel)+cardID(card)+" "+cardTitle(card), cw, false), sel))
	}
	return out
}

// listView is one list: each column's heading, then its cards with their
// repository, scrolled to keep the selected card on screen.
func (m BoardModel) listView(w int) []string {
	idW, repoW := 0, 0
	for _, col := range m.cols {
		for _, c := range col {
			idW = max(idW, lipgloss.Width(cardID(c)))
			repoW = max(repoW, lipgloss.Width(cardRepo(c)))
		}
	}
	repoW = min(repoW, max(w/5, minFlex))
	var lines []string
	sel := 0
	for ci, col := range m.cols {
		lines = append(lines, headerStyle.Render(truncate(m.heading(ci), w)))
		for i, c := range col {
			selected := ci == m.col && i == m.row
			if selected {
				sel = len(lines)
			}
			text := cardMark(selected) + fit(cardID(c), idW, false) + "  " + fit(cardRepo(c), repoW, false) + "  " + cardTitle(c)
			lines = append(lines, markCard(truncate(text, w), selected))
		}
	}
	if room := m.bodyRows(); room > 0 && len(lines) > room {
		first := max(sel-room+1, 0)
		lines = lines[first : first+room]
	}
	return lines
}

// cardMark is the selection mark before a card.
func cardMark(selected bool) string {
	if selected {
		return "> "
	}
	return "  "
}

// markCard draws a selected card in reverse video.
func markCard(s string, selected bool) string {
	if selected {
		return selectedStyle.Render(s)
	}
	return s
}

// cardTitle is c's title as drawn.
func cardTitle(c task.Card) string {
	return sanitize.Line(c.Task.Title, task.MaxTitle)
}

// cardRepo is the basename of c's repository as drawn, or "-".
func cardRepo(c task.Card) string {
	if r := sanitize.Line(RepoName(c.Task.Repo), maxRepoCells); r != "" {
		return r
	}
	return "-"
}

// statusLine is the spawn prompt, the last notice, the last read's error
// or the keys, cut to the width.
func (m BoardModel) statusLine(w int) string {
	style, text := helpStyle, boardHelp
	switch {
	case m.confirming:
		style, text = lipgloss.NewStyle(), "spawn "+cardID(m.pending)+"? y starts it, any other key cancels"
	case m.notice != "":
		style, text = lipgloss.NewStyle(), sanitize.Line(m.notice, maxNote)
		if m.noticeErr {
			style = errorStyle
		}
	case m.err != nil:
		style, text = errorStyle, "read failed: "+sanitize.Line(m.err.Error(), maxNote)
	}
	return style.Render(truncate(text, w))
}

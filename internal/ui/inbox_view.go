package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/sanitize"
)

const (
	inboxEmpty = "nothing needs you"
	inboxHelp  = "j/k move  enter open  a accept  / filter  tab sessions  t board  q quit"
	// inboxLabelWidth is the session or task column; the summary takes the rest.
	inboxLabelWidth = 14
	// inboxTextLimit bounds each untrusted string before it is laid out.
	inboxTextLimit = 300
)

var inboxHeadings = map[InboxKind]string{
	InboxDecision: "DECISIONS", InboxPrompt: "PERMISSION PROMPTS", InboxReview: "REVIEWS", InboxAssume: "ASSUMPTIONS TO CONFIRM",
}

// cell fits s, cleaned, into exactly w cells (wide characters count as two).
func inboxCell(s string, w int) string {
	return lipgloss.NewStyle().Inline(true).Width(w).MaxWidth(w).Render(sanitize.Line(s, inboxTextLimit))
}

// View renders the groups and a status line, every line at most the window
// width and the whole at most its height. Every item string is drawn through
// sanitize.Line.
func (m InboxModel) View() string {
	w := m.width
	if w <= 0 {
		w = 80
	}
	shown := m.shown()
	var lines []string
	switch {
	case !m.loaded:
	case len(shown) == 0 && m.filter != "":
		lines = []string{"no item matches the filter"}
	case len(shown) == 0:
		lines = []string{inboxEmpty}
	}
	body, cursorLine := m.itemLines(shown, w)
	cursorLine += len(lines)
	lines = append(lines, body...)
	if n := m.height - 1; n > 0 && len(lines) > n {
		first := max(min(cursorLine-n+1, len(lines)-n), 0)
		lines = lines[first : first+n]
	}
	for m.height > 0 && len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, m.statusLine())
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

func (m InboxModel) statusLine() string {
	switch {
	case m.filtering:
		return "/" + sanitize.Line(m.filter, inboxTextLimit) + "_"
	case m.notice != "":
		return m.notice
	case m.err != nil:
		return "read failed: " + sanitize.Line(m.err.Error(), inboxTextLimit)
	case m.filter != "":
		return "filter: " + sanitize.Line(m.filter, inboxTextLimit) + "  (/ then esc clears)"
	}
	return inboxHelp
}

// itemLines lays out the items under their group headings, and returns the
// line of the selected one.
func (m InboxModel) itemLines(shown []InboxItem, w int) (lines []string, cursorLine int) {
	counts := map[InboxKind]int{}
	for _, it := range shown {
		counts[it.Kind]++
	}
	for i, it := range shown {
		if i == 0 || shown[i-1].Kind != it.Kind {
			lines = append(lines, fmt.Sprintf("%s (%d)", inboxHeadings[it.Kind], counts[it.Kind]))
		}
		line := fmt.Sprintf("  %4s  %s  %s", Age(it.At, m.now()), inboxCell(it.Label, inboxLabelWidth),
			sanitize.Line(it.Summary, inboxTextLimit))
		line = lipgloss.NewStyle().Inline(true).MaxWidth(w).Render(line)
		if i == m.cursor {
			cursorLine = len(lines)
			line = selectedStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return lines, cursorLine
}

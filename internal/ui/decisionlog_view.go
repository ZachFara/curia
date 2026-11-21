package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/sanitize"
)

const (
	decisionLogHelp     = "j/k move  Enter in full  / search  s scope  w since  r refresh  q back"
	decisionLogFullHelp = "j/k scroll  Esc back"
	decisionLogTextMax  = 2000 // runes of a question or answer in the full view
)

// View draws the log or one decision, every line at most the window width
// (wide characters counted) and the whole at most its height. Every stored
// string is cleaned and bounded first.
func (m DecisionLogModel) View() string {
	w := m.width
	if w <= 0 {
		w = defaultWidth
	}
	var lines []string
	status := decisionLogHelp
	switch {
	case m.full && m.sel < len(m.rows):
		lines, status = m.fullLines(w), decisionLogFullHelp
	default:
		lines = m.listLines()
	}
	if m.typing {
		status = "search: " + sanitize.Line(m.input, 120) + "_"
	}
	for m.height > 0 && len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	if m.height > 0 && len(lines) > m.height-1 {
		lines = lines[:m.height-1]
	}
	lines = append(lines, status)
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

func (m DecisionLogModel) listLines() []string {
	head := "DECISIONS"
	if m.text != "" {
		head += " \"" + sanitize.Line(m.text, 40) + "\""
	}
	if s := decisionLogScopes[m.scope]; s != "" {
		head += " scope " + s
	}
	if d := decisionLogWindows[m.window]; d > 0 {
		head += fmt.Sprintf(" last %dd", int(d.Hours()/24))
	}
	lines := []string{head, DecisionLogSummaryLine(m.sum)}
	switch {
	case m.err != nil:
		lines = append(lines, "read failed: "+sanitize.Line(m.err.Error(), 120))
	case len(m.rows) == 0:
		lines = append(lines, "no decisions match")
	}
	first := 0
	if room := m.height - 4; m.height > 0 && room > 0 && m.sel >= room {
		first = m.sel - room + 1
	}
	for i := first; i < len(m.rows); i++ {
		r, mark := m.rows[i], " "
		if i == m.sel {
			mark = ">"
		}
		st := string(r.Status)
		if r.Superseded {
			st = "superseded"
		}
		lines = append(lines, fmt.Sprintf("%s%-7s %-10s %-6s %s r%d a%d %s", mark, sanitize.Line(r.ID, 7), sanitize.Line(st, 10),
			sanitize.Line(r.Scope, 6), r.CreatedAt.Local().Format("01-02"), r.Reuse, r.AskedAgain, sanitize.Line(r.Question, 200)))
	}
	return lines
}

// fullLines is the selected decision whole, wrapped to w, from line m.at.
func (m DecisionLogModel) fullLines(w int) []string {
	r := m.rows[m.sel]
	var all []string
	add := func(s string) { all = append(all, wrap(s, w)...) }
	add("DECISION " + sanitize.Line(r.ID, 30))
	add("status  " + sanitize.Line(DecisionLogStatus(r), 60))
	add("scope   " + sanitize.Line(r.Scope, 20) + "  " + sanitize.Line(r.Repo, 120))
	add("asked   " + r.CreatedAt.Local().Format("2006-01-02 15:04"))
	if !r.AnsweredAt.IsZero() {
		add("decided " + r.AnsweredAt.Local().Format("2006-01-02 15:04"))
	}
	add("reused  " + DecisionLogReuse(r, m.now()) + "; asked again " + fmt.Sprint(r.AskedAgain))
	add("")
	block := func(s string) { all = append(all, indent(wrap(s, w-len(gutter)))...) } // indented: a line cannot pass for a heading
	add("Question")
	block(sanitize.Text(r.Question, decisionLogTextMax))
	add("")
	add("Answer")
	block(sanitize.Text(r.Answer, decisionLogTextMax))
	return all[min(m.at, max(len(all)-1, 0)):]
}

package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/sanitize"
)

const timelineHelp = "j/k move  n older  w window  / filter  r refresh  q back"

// View draws the header, the events and the status line, every line at most
// the window width (wide characters counted) and the whole at most its height.
// Every event string is cleaned and bounded first.
func (m TimelineModel) View() string {
	w := m.width
	if w <= 0 {
		w = defaultWidth
	}
	head := fmt.Sprintf("TIMELINE last %s", timelineWindows[m.window])
	if m.task != "" || m.repo != "" {
		head += " " + sanitize.Line(m.task+m.repo, 60)
	}
	lines := []string{head}
	switch {
	case m.err != nil:
		lines = append(lines, "read failed: "+sanitize.Line(m.err.Error(), 120))
	case len(m.evs) == 0:
		lines = append(lines, "nothing happened in this window")
	}
	for i := m.top; i < len(m.evs) && (m.height <= 0 || len(lines) < m.height-1); i++ {
		e := m.evs[i]
		who := e.TaskID
		if who == "" {
			who = e.SessionID[:min(8, len(e.SessionID))]
		}
		lines = append(lines, fmt.Sprintf("%s %-8s %-15s %s", e.At.Local().Format("01-02 15:04"),
			sanitize.Line(who, 8), sanitize.Line(e.Kind, 15), sanitize.Line(e.Line, 200)))
	}
	status := timelineHelp
	if m.typing {
		status = "task id or repo path: " + sanitize.Line(m.input, 120) + "_"
	}
	for m.height > 0 && len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, status)
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

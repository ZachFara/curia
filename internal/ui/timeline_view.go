package ui

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/sanitize"
)

const (
	timelineHelp     = "j/k move  Enter in full  o open  n older  w window  / filter  q back"
	timelineFullHelp = "j/k scroll  o open  Esc back"
)

func (m TimelineModel) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return defaultWidth
}

// View draws the header, the events and the status line, every line at most
// the window width (wide characters counted) and the whole at most its height.
// Every event string is cleaned and bounded first.
func (m TimelineModel) View() string {
	w := m.width
	if w <= 0 {
		w = defaultWidth
	}
	if m.full {
		return m.fullView(w)
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
		mark := " "
		if i == m.top {
			mark = ">"
		}
		lines = append(lines, fmt.Sprintf("%s%s %-8s %-15s %s", mark, e.At.Local().Format("01-02 15:04"),
			sanitize.Line(who, 8), sanitize.Line(e.Kind, 15), sanitize.Line(e.Line, 200)))
	}
	status := cmp.Or(m.notice, timelineHelp)
	if m.typing {
		status = "task id or repo path: " + sanitize.Line(m.input, 120) + "_"
	}
	return m.frame(lines, status, w)
}

// frame pads lines to the window, adds the status line and clips each to w columns.
func (m TimelineModel) frame(lines []string, status string, w int) string {
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

// fullView shows one event whole: time, kind, session, task, then its text.
func (m TimelineModel) fullView(w int) string {
	e := m.selected()
	lines := []string{
		"TIMELINE EVENT " + sanitize.Line(e.Kind, 30),
		"time    " + e.At.Local().Format("2006-01-02 15:04:05"),
		"session " + sanitize.Line(e.SessionID, 80),
		"task    " + sanitize.Line(e.TaskID, 80),
		"",
	}
	if m.body == nil {
		lines = append(lines, "reading...")
	}
	for i := m.bodyAt; i < len(m.body) && (m.height <= 0 || len(lines) < m.height-1); i++ {
		lines = append(lines, m.body[i])
	}
	return m.frame(lines, cmp.Or(m.notice, timelineFullHelp), w)
}

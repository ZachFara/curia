package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const settingsHelp = "j/k move  e edit  r refresh  q back"

// settingsGroup is the section of a key: the part before its first dot,
// or "general" for a top-level key.
func settingsGroup(key string) string {
	if g, _, ok := strings.Cut(key, "."); ok {
		return g
	}
	return "general"
}

// View draws the keys grouped by section, every line at most the window
// width (wide characters counted) and the whole at most its height. Every
// string is cleaned and bounded first.
func (m SettingsModel) View() string {
	w := m.width
	if w <= 0 {
		w = defaultWidth
	}
	var lines []string
	cursorLine, group := 0, ""
	for i, r := range m.rows {
		if g := settingsGroup(r.Key); g != group {
			group = g
			lines = append(lines, "["+group+"]")
		}
		mark := "  "
		if i == m.cursor {
			mark, cursorLine = "> ", len(lines)
		}
		lines = append(lines, fmt.Sprintf("%s%s = %s  (%s)", mark, cleanLine(r.Key, 40), cleanLine(r.Value, 60), cleanLine(r.Source, 10)))
		if r.Refused != "" {
			lines = append(lines, "    repository value refused: "+cleanLine(r.Refused, 160))
		}
	}
	head := "SETTINGS"
	if m.readOnly != "" {
		head += " (read-only: " + cleanLine(m.readOnly, 80) + ")"
	}
	body := m.height - 2 // the head and the status line
	if body > 0 && len(lines) > body {
		top := min(max(cursorLine-body/2, 0), len(lines)-body)
		lines = lines[top : top+body]
	}
	out := []string{head}
	if m.err != nil {
		out = append(out, "read failed: "+cleanLine(m.err.Error(), 120))
	}
	out = append(out, lines...)
	for m.height > 0 && len(out) < m.height-1 {
		out = append(out, "")
	}
	out = append(out, m.status())
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range out {
		out[i] = clip.Render(l)
	}
	return strings.Join(out, "\n")
}

func (m SettingsModel) status() string {
	r, _ := m.row()
	switch {
	case m.typing:
		return cleanLine(r.Key, 40) + " = " + cleanLine(m.input, 120) + "_"
	case m.asking:
		return "y sets " + cleanLine(r.Key, 30) + " = " + cutValue(m.input) + ", any key cancels. " + cleanLine(m.notice, 80)
	case m.notice != "":
		return cleanLine(m.notice, 200)
	}
	return settingsHelp
}

// cutValue is the input as the confirmation shows it: cut at 30 runes with an
// ellipsis and the full length, since the whole input is what gets set.
func cutValue(in string) string {
	v := cleanLine(in, 30)
	if n := len([]rune(strings.TrimSpace(clean(in)))); n > 30 {
		return v + "… (" + strconv.Itoa(n) + " chars)"
	}
	return v
}

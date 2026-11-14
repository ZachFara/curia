package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/route"
	"github.com/ZachFara/curia/internal/sanitize"
)

const (
	// sayLabelRunes bounds a drawn candidate label (route's own bound).
	sayLabelRunes = 120
	// sayNoteLines bounds the lines a proposal's note is wrapped over.
	sayNoteLines = 3
	// sayPrompt starts the input line and the message line.
	sayPrompt = "say: "
	// sayArrow starts the proposal and hold lines.
	sayArrow = "→ "
	// minLabelCells is the narrowest the label is cut to before the kind
	// moves to a line of its own.
	minLabelCells = 8
)

// View renders, top to bottom: the input line or the message, the
// proposal or the hold with its countdown, the proposal's note, and the
// notice, the outcome or the keys. Every line is at most the window width.
func (m SayModel) View() string {
	w := m.textWidth()
	var lines []string
	if m.state == sayInput {
		lines = append(lines, m.inputLine(w))
	} else {
		lines = append(lines, helpStyle.Render(sayPrompt+truncate(sanitize.Line(m.text, message.MaxText), w-len(sayPrompt))))
	}
	lines = append(lines, m.middle(w)...)
	lines = append(lines, m.bottom(w)...)
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

// textWidth is the window width, or defaultWidth before it is known.
func (m SayModel) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return defaultWidth
}

// inputLine is the prompt and the end of the text being typed, with a
// cursor.
func (m SayModel) inputLine(w int) string {
	return sayPrompt + tail(clean(string(m.buf)), w-len(sayPrompt)-1) + "_"
}

// middle is what the view is doing with the message.
func (m SayModel) middle(w int) []string {
	switch m.state {
	case sayRouting:
		return []string{"routing…"}
	case sayProposal:
		return append(m.proposalLines(w), m.noteLines(w)...)
	case saySending:
		return append(m.proposalLines(w), "sending…")
	case sayHolding:
		return m.holdLines(w, "holding…")
	case sayHeld:
		return append(m.holdLines(w, m.countdown()), m.noteLines(w)...)
	case sayReleasing:
		return m.holdLines(w, "sending…")
	case sayUndoing:
		return m.holdLines(w, "undoing…")
	case sayCanceling:
		return []string{"canceling…"}
	case sayDone:
		style := lipgloss.NewStyle()
		if m.resultErr {
			style = errorStyle
		}
		return styled(style, capLines(wrap(m.result, w), maxNoticeLines, w))
	}
	return nil
}

// bottom is the notice, or the keys of the state.
func (m SayModel) bottom(w int) []string {
	if m.notice != "" {
		style := lipgloss.NewStyle()
		if m.noticeErr {
			style = errorStyle
		}
		return styled(style, capLines(wrap(sanitize.Line(m.notice, maxNote), w), maxNoticeLines, w))
	}
	var help string
	switch m.state {
	case sayInput:
		help = "enter routes  esc quits"
	case sayRouting:
		help = "esc cancels"
	case sayProposal:
		help = m.proposalHelp(w)
	case sayHeld:
		help = "u or esc undoes"
	case sayDone:
		help = "q or esc closes"
	default:
		return nil
	}
	return []string{helpStyle.Render(truncate(help, w))}
}

// proposalHelp names the keys of the proposal and which target of how many
// is shown; shorter when the window is narrow.
func (m SayModel) proposalHelp(w int) string {
	pos := fmt.Sprintf("%d/%d", m.target+1, len(m.p.Candidates))
	if long := "enter send  tab target " + pos + "  k kind  esc cancel"; lipgloss.Width(long) <= w {
		return long
	}
	return "enter send  tab " + pos + "  k kind  esc"
}

// proposalLines is "→ <label> · <kind> (0.93)": the number only for the
// model's suggestion as it was made. The label is cut so the kind and the
// number stay on screen; in a very narrow window they get a line of their
// own.
func (m SayModel) proposalLines(w int) []string {
	c, _ := m.current()
	suffix := " · " + kindText(m.kind)
	if m.p.Routed == model.RoutedConfirmed && !m.choice().Changed {
		suffix += fmt.Sprintf(" (%.2f)", m.p.Confidence)
	}
	return arrowLines(candLabel(c), suffix, w)
}

// holdLines is "→ <label> (<what>)".
func (m SayModel) holdLines(w int, what string) []string {
	c, _ := m.current()
	return arrowLines(candLabel(c), " ("+what+")", w)
}

// arrowLines is the arrow, the label cut to fit and suffix on one line, or
// on two when fewer than minLabelCells would be left for the label.
func arrowLines(label, suffix string, w int) []string {
	room := w - lipgloss.Width(sayArrow) - lipgloss.Width(suffix)
	if room >= minLabelCells {
		return []string{sayArrow + truncate(label, room) + suffix}
	}
	return []string{
		sayArrow + truncate(label, w-lipgloss.Width(sayArrow)),
		"  " + truncate(strings.TrimSpace(suffix), w-2),
	}
}

// countdown is "u to undo, 3s": the whole seconds left of the undo window,
// by the clock, rounded up.
func (m SayModel) countdown() string {
	left := m.heldAt.Add(SayHold).Sub(m.now())
	if left <= 0 {
		return "sending…"
	}
	secs := (left + time.Second - 1) / time.Second
	return fmt.Sprintf("u to undo, %ds", secs)
}

// noteLines is the proposal's note, dim.
func (m SayModel) noteLines(w int) []string {
	note := sanitize.Line(m.p.Note, maxNote)
	return styled(helpStyle, capLines(wrap(note, w), sayNoteLines, w))
}

// candLabel is c's label as drawn, or its key when it has none.
func candLabel(c route.Candidate) string {
	if l := sanitize.Line(c.Label, sayLabelRunes); l != "" {
		return l
	}
	if k := sanitize.Line(c.Key, maxIDCells); k != "" {
		return k
	}
	return "-"
}

// kindText is a message kind as drawn.
func kindText(k model.MessageKind) string {
	return sanitize.Line(string(k), maxWordCells)
}

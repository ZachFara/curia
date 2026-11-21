package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/usage"
)

func (m DetailModel) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return defaultWidth
}

// room is how many body lines fit under the head line and above the keys line.
func (m DetailModel) room() int {
	if m.height <= 0 {
		return len(m.body())
	}
	return max(m.height-2, 1)
}

// View draws the head line, the body from the scroll offset and the keys
// line: every line at most the window width, the whole exactly its height.
func (m DetailModel) View() string {
	w := m.textWidth()
	body := m.body()
	room := m.room()
	start := min(m.offset, max(len(body)-room, 0))
	out := []bodyLine{{text: m.head(), kind: lineHead}}
	out = append(out, body[start:min(start+room, len(body))]...)
	for m.height > 0 && len(out) < m.height-1 {
		out = append(out, bodyLine{})
	}
	out = append(out, bodyLine{text: "enter pane · j/k scroll · esc back · read-only", kind: lineFaint})
	clip := lipgloss.NewStyle().MaxWidth(w)
	drawn := make([]string, len(out))
	for i, l := range out {
		drawn[i] = clip.Render(styleOf(l.kind).Render(l.text))
	}
	return strings.Join(drawn, "\n")
}

func (m DetailModel) head() string {
	s := m.data.Session
	if !m.loaded {
		return truncate("reading "+sanitize.Line(m.id, maxIDCells)+"…", m.textWidth())
	}
	state := string(s.State)
	if s.State == model.StateEnded && s.EndReason != "" {
		state += " (" + sanitize.Line(s.EndReason, 20) + ")"
	}
	parts := []string{sanitize.Line(s.ID, maxIDCells), state, sanitize.Line(s.Repo, 40), sanitize.Line(s.Branch, 60), sanitize.Line(s.TaskID, maxIDCells), detailMode(s)}
	return truncate(strings.Join(nonEmpty(parts), " · "), m.textWidth())
}

// detailMode is the session's own permission mode (F20) when it is not the
// plain one and has not lapsed; the configured default is not shown here.
func detailMode(s model.Session) string {
	if s.Mode == "" || s.Mode == model.ModeNormal || (!s.ModeUntil.IsZero() && !time.Now().Before(s.ModeUntil)) {
		return ""
	}
	return "mode " + sanitize.Line(s.Mode, 10)
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// body is every panel, as lines of at most the width.
func (m DetailModel) body() []bodyLine {
	w := m.textWidth()
	var out []bodyLine
	section := func(title string) { out = append(out, bodyLine{text: title, kind: lineHead}) }
	one := func(k lineKind, s string) { out = append(out, kindLines(k, sanitize.Line(s, 400), w, 1)...) }
	if m.err != nil {
		one(lineAlert, "read failed: "+m.err.Error())
	}
	if !m.loaded {
		return out
	}
	d := m.data
	section("Transcript")
	for _, l := range d.Tail {
		one(linePlain, l)
	}
	if len(d.Tail) == 0 {
		one(lineFaint, "no transcript lines")
	}
	if d.TailNote != "" {
		one(lineWarn, d.TailNote)
	}
	out = append(out, m.rowLines(w)...)
	if m.diff.Has {
		section("Diff")
		out = append(out, m.diffLines(w)...)
	}
	section("Cost")
	one(linePlain, m.costText())
	if d.Resources != "" {
		section("Resources")
		one(linePlain, d.Resources)
	}
	if d.ActivityOK {
		section("Activity")
		for _, l := range d.Activity {
			one(linePlain, l)
		}
	}
	return out
}

func ticketKind(t model.Decision) lineKind {
	if t.Status == model.StatusOpen && t.Mode == model.ModeBlocking {
		return lineAlert
	}
	return linePlain
}

// messageState is a message's status, with "unconfirmed" for one typed into
// a pane that has not confirmed it.
func messageState(msg model.Message) string {
	if msg.Status == model.MsgDelivered && msg.DeliveredVia == model.ViaSendKeys && msg.ConfirmedAt.IsZero() {
		return "delivered, unconfirmed"
	}
	return string(msg.Status)
}

// MessageState is messageState for the command-line form.
func MessageState(msg model.Message) string { return messageState(msg) }

// RenderDetail is the view's head and body as plain lines (`curia show`): one renderer for both forms.
func RenderDetail(d DetailData, diff DetailDiff, now func() time.Time) string {
	m := DetailModel{id: d.Session.ID, now: now, data: d, loaded: true, diff: diff}
	lines := []string{m.head()}
	for _, l := range m.body() {
		lines = append(lines, l.text)
	}
	return strings.Join(lines, "\n")
}

func (m DetailModel) costText() string {
	d := m.data
	parts := []string{"session " + cmpCost(usage.CostCell(d.Session.CostUSD, d.Session.CostKnown))}
	if d.TaskCostKnown {
		parts = append(parts, "task branch "+usage.CostCell(d.TaskCostUSD, true))
	}
	if c := usage.BudgetCell(d.Budget); c != "" {
		if d.Budget.Level() != model.BudgetUnknown {
			c = "budget " + c
		}
		parts = append(parts, c)
	}
	if c := usage.CacheCell(d.Session, m.now()); c != "" {
		parts = append(parts, "cache "+c)
	}
	return strings.Join(parts, " · ")
}

func cmpCost(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// diffLines is the diff stat, one line each (its columns kept), cut at maxDetailDiffLines, and the note.
func (m DetailModel) diffLines(w int) []bodyLine {
	var out []bodyLine
	lines := strings.Split(sanitize.Block(m.diff.Stat, maxDiffBytes), "\n")
	for i, l := range lines {
		if i == maxDetailDiffLines {
			out = append(out, kindLines(lineFaint, fmt.Sprintf("… %d more lines", len(lines)-i), w, 1)...)
			break
		}
		out = append(out, bodyLine{text: truncate(l, w)})
	}
	if m.diff.Note != "" {
		out = append(out, kindLines(lineWarn, sanitize.Line(m.diff.Note, 400), w, 1)...)
	}
	return out
}

// rowLines are the Tickets and Messages panels.
func (m DetailModel) rowLines(w int) []bodyLine {
	d := m.data
	var out []bodyLine
	section := func(title string) { out = append(out, bodyLine{text: title, kind: lineHead}) }
	one := func(k lineKind, s string) { out = append(out, kindLines(k, sanitize.Line(s, 400), w, 1)...) }
	section("Tickets")
	for _, t := range d.Tickets {
		line := fmt.Sprintf("%s %s %s", t.ID, t.Status, t.Question)
		if t.Answer != "" {
			line += " → " + t.Answer
		}
		one(ticketKind(t), line)
	}
	if len(d.Tickets) == 0 {
		one(lineFaint, "none")
	}
	section("Messages")
	for _, msg := range d.Messages {
		one(linePlain, fmt.Sprintf("%s %s %s", msg.ID, messageState(msg), msg.Text))
	}
	if len(d.Messages) == 0 {
		one(lineFaint, "none")
	}
	return out
}

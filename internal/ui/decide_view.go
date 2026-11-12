package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

const (
	// defaultWidth wraps text before the first WindowSizeMsg.
	defaultWidth = 80
	// maxIDCells bounds a drawn ticket id ("D-" and nine digits when valid).
	maxIDCells = 16
	// maxRepoCells bounds a drawn repository name.
	maxRepoCells = 64
	// maxNote bounds a drawn outcome note or error.
	maxNote = 300
	// optionIndent indents an option's wrapped lines and trade-offs.
	optionIndent = "   "
)

var (
	questionStyle = lipgloss.NewStyle().Bold(true)
	stakesStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	recStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
)

// detailLimits cap the long parts of the selected ticket, in lines; 0 is
// no cap.
type detailLimits struct{ context, tradeoffs, rationale int }

var (
	// fullDetail is the layout of section 3 of the view.
	fullDetail = detailLimits{context: 6}
	// compactDetail is used when the full one does not fit the window, so
	// the options stay on screen.
	compactDetail = detailLimits{context: 2, tradeoffs: 1, rationale: 2}
)

// View renders the header, the queue, the selected ticket and the status
// line, every line at most the window width and the whole at most its
// height.
func (m DecideModel) View() string {
	w := m.textWidth()
	queue := m.renderQueue(w)
	lines := append([]string{m.renderHeader()}, queue...)
	lines = append(lines, "")
	lines = append(lines, m.renderDetail(w, m.detailRoom(len(queue)))...)
	for m.height > 0 && len(lines) < m.height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, m.renderStatus(w))
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

// textWidth is the window width, or defaultWidth before it is known.
func (m DecideModel) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return defaultWidth
}

// detailRoom is how many lines the selected ticket may use; 0 is no limit.
func (m DecideModel) detailRoom(queueLines int) int {
	if m.height <= 0 {
		return 0
	}
	return max(m.height-queueLines-3, 1) // header, separator, status line
}

func (m DecideModel) renderHeader() string {
	blocking := 0
	for _, t := range m.tickets {
		if t.Mode == model.ModeBlocking {
			blocking++
		}
	}
	return headerStyle.Render(fmt.Sprintf("%d open · %d blocking", len(m.tickets), blocking))
}

// renderQueue is one line per ticket on screen: selection mark, id, mode,
// age, repo and the question cut to fit.
func (m DecideModel) renderQueue(w int) []string {
	if len(m.tickets) == 0 {
		return []string{"no open decisions"}
	}
	first, last := m.offset, len(m.tickets)
	if n := m.queueRows(); n > 0 {
		last = min(last, first+n)
	}
	idW, repoW := 0, 0
	for _, t := range m.tickets[first:last] {
		idW = max(idW, lipgloss.Width(ticketID(t)))
		repoW = max(repoW, lipgloss.Width(ticketRepo(t)))
	}
	repoW = min(repoW, max(w/5, minFlex))
	out := make([]string, 0, last-first)
	for i, t := range m.tickets[first:last] {
		out = append(out, m.queueLine(t, idW, repoW, w, first+i == m.cursor))
	}
	return out
}

func (m DecideModel) queueLine(t model.Ticket, idW, repoW, w int, selected bool) string {
	mark, style := " ", lipgloss.NewStyle()
	if selected {
		mark, style = ">", style.Reverse(true)
	}
	mode := "B"
	if t.Mode == model.ModeAssume {
		mode = "a"
	}
	prefix := strings.Join([]string{
		mark, fit(ticketID(t), idW, false), mode, fit(Age(t.CreatedAt, m.now()), 3, true), fit(ticketRepo(t), repoW, false), "",
	}, " ")
	question := truncate(sanitize.Line(t.Question, decision.MaxQuestion), w-lipgloss.Width(prefix))
	return style.Render(prefix + question)
}

// ticketID is t's id as drawn.
func ticketID(t model.Ticket) string {
	return sanitize.Line(t.ID, maxIDCells)
}

// ticketRepo is the basename of t's repository as drawn, or "-".
func ticketRepo(t model.Ticket) string {
	if r := sanitize.Line(RepoName(t.Repo), maxRepoCells); r != "" {
		return r
	}
	return "-"
}

// renderDetail draws the selected ticket in room lines (0: no limit): in
// full when it fits, else compact, else cut with a last line of "…".
func (m DecideModel) renderDetail(w, room int) []string {
	t, ok := m.current()
	if !ok {
		return nil
	}
	lines := m.detailLines(t, w, fullDetail)
	if room <= 0 || len(lines) <= room {
		return lines
	}
	if lines = m.detailLines(t, w, compactDetail); len(lines) > room {
		lines = append(lines[:room-1:room-1], ellipsis)
	}
	return lines
}

func (m DecideModel) detailLines(t model.Ticket, w int, lim detailLimits) []string {
	out := styled(questionStyle, wrap(sanitize.Text(t.Question, decision.MaxQuestion), w))
	if s := decision.Stakes(t, m.now()); s != "" {
		out = append(out, styled(stakesStyle, wrap(s, w))...)
	}
	meta := strings.Join([]string{
		sanitize.Line(string(t.Reversibility), maxWordCells),
		sanitize.Line(string(t.Mode), maxWordCells),
		"scope " + sanitize.Line(string(t.Scope), maxWordCells),
	}, " · ")
	out = append(out, styled(helpStyle, wrap(meta, w))...)
	out = append(out, capLines(wrap(sanitize.Text(t.Context, decision.MaxContext), w), lim.context, w)...)
	out = append(out, optionLines(t, w, lim.tradeoffs)...)
	return append(out, m.tailLines(t, w, lim.rationale)...)
}

// maxWordCells bounds a drawn one-word field such as the mode.
const maxWordCells = 16

// optionLines are the options as "1  A  Postgres table", trade-offs
// indented beneath, the recommended one highlighted and marked.
func optionLines(t model.Ticket, w, tradeoffs int) []string {
	var out []string
	inner := max(w-len(optionIndent), 1)
	for i, o := range t.Options {
		prefix := fmt.Sprintf("%d  %s  ", i+1, sanitize.Line(o.Key, decision.MaxKey))
		label := sanitize.Line(o.Label, decision.MaxLabel)
		rec := o.Key == t.Recommendation
		if rec {
			label += " (recommended)"
		}
		// The label hangs under itself, so the prefix keeps its spacing.
		lines := wrap(label, w-lipgloss.Width(prefix))
		for j := range lines {
			lead := prefix
			if j > 0 {
				lead = strings.Repeat(" ", lipgloss.Width(prefix))
			}
			lines[j] = lead + lines[j]
		}
		if rec {
			lines = styled(recStyle, lines)
		}
		out = append(out, lines...)
		tr := capLines(wrap(sanitize.Text(o.Tradeoffs, decision.MaxTradeoffs), inner), tradeoffs, inner)
		for _, l := range tr {
			out = append(out, optionIndent+l)
		}
	}
	return out
}

// tailLines are the rationale, the assumption, the precedent it differs
// from, and the staged reasoning and scope.
func (m DecideModel) tailLines(t model.Ticket, w, rationale int) []string {
	out := capLines(wrap(sanitize.Text(t.Rationale, decision.MaxRationale), w), rationale, w)
	if t.Mode == model.ModeAssume {
		out = append(out, wrap("assumes: "+sanitize.Text(t.Assumption, decision.MaxAssumption), w)...)
	}
	if t.DiffersFrom != "" {
		why := sanitize.Text(t.DiffersWhy, decision.MaxDiffersWhy)
		out = append(out, wrap("differs from "+sanitize.Line(t.DiffersFrom, maxIDCells)+": "+why, w)...)
	}
	if m.reasoning != "" {
		out = append(out, wrap("staged reasoning: "+sanitize.Text(m.reasoning, decision.MaxReasoning), w)...)
	}
	if m.scope != "" {
		out = append(out, "staged scope: "+string(m.scope))
	}
	return out
}

// renderStatus is the bottom line: the pending confirmation, the text being
// typed, the last outcome or notice, the last refresh error, or the keys.
func (m DecideModel) renderStatus(w int) string {
	style, text := helpStyle, m.helpText()
	switch {
	case m.closing:
		style, text = lipgloss.NewStyle(), "closing once the answer is sent"
	case m.confirming:
		style, text = lipgloss.NewStyle(), m.confirmText()
	case m.input != inputNone:
		return m.inputLine(w)
	case m.notice != "":
		style, text = lipgloss.NewStyle(), sanitize.Line(m.notice, maxNote)
		if m.noticeErr {
			style = errorStyle
		}
	case m.err != nil:
		style, text = errorStyle, "refresh failed: "+sanitize.Line(m.err.Error(), maxNote)
	}
	return style.Render(truncate(text, w))
}

func (m DecideModel) helpText() string {
	q := "q quit"
	if m.opts.Embedded {
		q = "q close"
	}
	return "1-4 answer  enter recommended  w write  r reason  s scope  " + q
}

// confirmText asks to confirm the pending answer to an irreversible ticket.
func (m DecideModel) confirmText() string {
	return fmt.Sprintf("answer %s with %s? y sends, any other key cancels", ticketID(m.pending.ticket), answerText(m.pending.a))
}

// answerText is how an answer is named on the status line: its option key,
// or its free text quoted and cut.
func answerText(a model.Answer) string {
	if a.Key != "" {
		return sanitize.Line(a.Key, decision.MaxKey)
	}
	return `"` + sanitize.Line(a.Text, 24) + `"`
}

// outcomeText is the status line after an answer: "D-9 answered: B ·
// delivered", or the outcome's note in place of "delivered".
func outcomeText(msg decideAnsweredMsg) string {
	note := "delivered"
	if msg.out.Note != "" {
		note = msg.out.Note
	}
	return sanitize.Line(msg.id, maxIDCells) + " answered: " + answerText(msg.a) + " · " + sanitize.Line(note, maxNote)
}

// inputLine is the prompt and the end of the text being typed, with a cursor.
func (m DecideModel) inputLine(w int) string {
	var prompt string
	switch m.input {
	case inputText:
		prompt = "answer (enter sends): "
	case inputReason:
		prompt = "reasoning (enter stages): "
	default:
		prompt = "reasoning for " + answerText(m.pending.a) + " (enter sends): "
	}
	prompt = truncate(prompt, w-1)
	return prompt + tail(clean(string(m.buf)), w-lipgloss.Width(prompt)-1) + "_"
}

// tail is the end of s that fits in w cells.
func tail(s string, w int) string {
	r := []rune(s)
	i, used := len(r), 0
	for i > 0 {
		rw := lipgloss.Width(string(r[i-1]))
		if used+rw > w {
			break
		}
		used += rw
		i--
	}
	return string(r[i:])
}

// styled renders each line in style.
func styled(style lipgloss.Style, lines []string) []string {
	for i, l := range lines {
		lines[i] = style.Render(l)
	}
	return lines
}

// capLines keeps at most n lines (no cap when n is 0), the last one ending
// in "…" when lines were dropped.
func capLines(lines []string, n, w int) []string {
	if n <= 0 || len(lines) <= n {
		return lines
	}
	lines = lines[:n]
	lines[n-1] = truncate(lines[n-1]+ellipsis, w)
	return lines
}

// wrap breaks s, already cleaned, into lines of at most w cells at spaces,
// breaking a word wider than w; each line break of s starts a new line. An
// empty s has no lines.
func wrap(s string, w int) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		out = append(out, wrapLine(para, max(w, 1))...)
	}
	return out
}

func wrapLine(s string, w int) []string {
	var lines []string
	line, used := "", 0
	for _, word := range strings.Fields(s) {
		for lipgloss.Width(word) > w {
			if used > 0 {
				lines, line, used = append(lines, line), "", 0
			}
			var head string
			head, word = splitWidth(word, w)
			lines = append(lines, head)
		}
		ww := lipgloss.Width(word)
		switch {
		case ww == 0:
		case used == 0:
			line, used = word, ww
		case used+1+ww <= w:
			line, used = line+" "+word, used+1+ww
		default:
			lines, line, used = append(lines, line), word, ww
		}
	}
	if used > 0 || len(lines) == 0 {
		lines = append(lines, line)
	}
	return lines
}

// splitWidth cuts s after at most w cells, and at least one rune.
func splitWidth(s string, w int) (head, rest string) {
	used := 0
	for i, r := range s {
		rw := lipgloss.Width(string(r))
		if i > 0 && used+rw > w {
			return s[:i], s[i:]
		}
		used += rw
	}
	return s, ""
}

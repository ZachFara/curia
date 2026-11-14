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
	// gutter indents every line of the selected ticket but the first line
	// of each option, so no ticket text sits in the columns of an option
	// row ("2 *B") and passes for one.
	gutter = "  "
)

var (
	questionStyle = lipgloss.NewStyle().Bold(true)
	stakesStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	recStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
)

// detailPlan says how much of the selected ticket is drawn. Line caps are
// 0 for no cap and -1 for none of it.
type detailPlan struct {
	question, context, tradeoffs, rationale int
	oneLine                                 bool // options and the lines around them one line each
	bare                                    bool // the question and the options only
}

// detailPlans are tried in order until one fits the window. Context and
// rationale go first, then trade-offs; the question shrinks to two lines,
// then one; the first line of every option, with its mark, goes last.
var detailPlans = []detailPlan{
	{context: 6},
	{context: 2, tradeoffs: 1, rationale: 2},
	{question: 2, context: -1, tradeoffs: 1, rationale: -1, oneLine: true},
	{question: 2, context: -1, tradeoffs: -1, rationale: -1, oneLine: true},
	{question: 1, context: -1, tradeoffs: -1, rationale: -1, oneLine: true, bare: true},
}

// View renders the header, the queue, the selected ticket and the status
// line, every line at most the window width and the whole at most its
// height.
func (m DecideModel) View() string {
	w := m.textWidth()
	queue := m.renderQueue(w)
	status := m.renderStatus(w)
	detail, _ := m.detail(w, m.detailRoom(len(queue)))
	lines := append([]string{m.renderHeader()}, queue...)
	lines = append(lines, "")
	lines = append(lines, detail...)
	for m.height > 0 && len(lines) < m.height-len(status) {
		lines = append(lines, "")
	}
	lines = append(lines, status...)
	clip := lipgloss.NewStyle().MaxWidth(w)
	for i, l := range lines {
		lines[i] = clip.Render(l)
	}
	return strings.Join(lines, "\n")
}

// optionsShown is how many of the selected ticket's options View draws, so
// that a key never answers with an option that is not on screen.
func (m DecideModel) optionsShown() int {
	first, last := m.queueSpan()
	_, n := m.detail(m.textWidth(), m.detailRoom(max(last-first, 1)))
	return n
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
	// The header, the separator and the status lines.
	return max(m.height-queueLines-2-len(m.renderStatus(m.textWidth())), 1)
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

// queueSpan is the range of tickets on screen in the queue.
func (m DecideModel) queueSpan() (first, last int) {
	first, last = m.offset, len(m.tickets)
	if n := m.queueRows(); n > 0 {
		last = min(last, first+n)
	}
	return first, last
}

// renderQueue is one line per ticket on screen: selection mark, id, mode,
// age, repo and the question cut to fit.
func (m DecideModel) renderQueue(w int) []string {
	if len(m.tickets) == 0 {
		return []string{"no open decisions"}
	}
	first, last := m.queueSpan()
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

// detail draws the selected ticket in room lines (0: no limit) with the
// first plan that fits, and says how many of its options it drew. When not
// even the last plan fits, it keeps the question's line and as many option
// lines as there is room for.
func (m DecideModel) detail(w, room int) ([]string, int) {
	t, ok := m.current()
	if !ok {
		return nil, 0
	}
	var lines []string
	for _, p := range detailPlans {
		lines = m.detailLines(t, w, p)
		if room <= 0 || len(lines) <= room {
			return lines, len(t.Options)
		}
	}
	return lines[:room], room - 1 // the bare plan: one question line, one line per option
}

func (m DecideModel) detailLines(t model.Ticket, w int, p detailPlan) []string {
	pw := max(w-len(gutter), 1) // prose width, inside the gutter
	question := wrap(sanitize.Text(t.Question, decision.MaxQuestion), pw)
	if len(question) == 0 {
		question = []string{""}
	}
	out := styled(questionStyle, indent(capLines(question, p.question, pw)))
	if !p.bare {
		out = append(out, styled(stakesStyle, indent(para(decision.Stakes(t, m.now()), pw, p.oneLine)))...)
		meta := strings.Join(append([]string{
			sanitize.Line(string(t.Reversibility), maxWordCells),
			sanitize.Line(string(t.Mode), maxWordCells),
			"scope " + sanitize.Line(string(t.Scope), maxWordCells),
		}, asker(t)...), " · ")
		out = append(out, styled(helpStyle, indent(para(meta, pw, p.oneLine)))...)
		if p.context >= 0 {
			out = append(out, indent(capLines(wrap(sanitize.Text(t.Context, decision.MaxContext), pw), p.context, pw))...)
		}
	}
	out = append(out, optionLines(t, w, p)...)
	if !p.bare {
		out = append(out, indent(m.tailLines(t, pw, p))...)
	}
	return out
}

// asker says who asks for t (M6): the tmux session and window and the
// branch of its session, and the session's state when it is not live, so
// two agents on one repository can be told apart.
func asker(t model.Ticket) []string {
	if t.SessionID == "" {
		return []string{"no session"}
	}
	var out []string
	where := sanitize.Line(t.TmuxSession, maxRepoCells) + ":" + sanitize.Line(t.TmuxWindow, maxIDCells)
	if where = strings.Trim(where, ":"); where != "" {
		out = append(out, "from "+where)
	}
	if b := sanitize.Line(t.Branch, maxRepoCells); b != "" {
		out = append(out, "on "+b)
	}
	switch t.SessionState {
	case model.StateEnded, model.StateStale:
		out = append(out, "session "+string(t.SessionState))
	case "":
		out = append(out, "session unknown")
	}
	return out
}

// indent puts each line inside the gutter.
func indent(lines []string) []string {
	for i, l := range lines {
		lines[i] = gutter + l
	}
	return lines
}

// maxWordCells bounds a drawn one-word field such as the mode.
const maxWordCells = 16

// optionLines are the options as "1  A  Postgres table" with trade-offs
// indented beneath. The recommended one is "2 *B  Redis queue": the mark
// sits in its own column between the number and the key, where no label
// can reach, and the line is highlighted.
func optionLines(t model.Ticket, w int, p detailPlan) []string {
	var out []string
	inner := max(w-len(optionIndent), 1)
	for i, o := range t.Options {
		rec := o.Key == t.Recommendation
		mark := " "
		if rec {
			mark = "*"
		}
		prefix := fmt.Sprintf("%d %s%s  ", i+1, mark, sanitize.Line(o.Key, decision.MaxKey))
		label := sanitize.Line(o.Label, decision.MaxLabel)
		lines := []string{truncate(prefix+label, w)}
		if !p.oneLine {
			lines = hang(prefix, label, w)
		}
		if rec {
			lines = styled(recStyle, lines)
		}
		out = append(out, lines...)
		if p.tradeoffs >= 0 {
			tr := capLines(wrap(sanitize.Text(o.Tradeoffs, decision.MaxTradeoffs), inner), p.tradeoffs, inner)
			for _, l := range tr {
				out = append(out, optionIndent+l)
			}
		}
	}
	return out
}

// hang wraps text in w cells after prefix, its later lines indented to
// line up under its first.
func hang(prefix, text string, w int) []string {
	pw := lipgloss.Width(prefix)
	lines := wrap(text, w-pw)
	if len(lines) == 0 {
		return []string{prefix}
	}
	for j := range lines {
		lead := prefix
		if j > 0 {
			lead = strings.Repeat(" ", pw)
		}
		lines[j] = lead + lines[j]
	}
	return lines
}

// tailLines are the rationale, the assumption, the precedent it differs
// from, and the staged reasoning and scope.
func (m DecideModel) tailLines(t model.Ticket, w int, p detailPlan) []string {
	var out []string
	if p.rationale >= 0 {
		out = capLines(wrap(sanitize.Text(t.Rationale, decision.MaxRationale), w), p.rationale, w)
	}
	if t.Mode == model.ModeAssume {
		out = append(out, para("assumes: "+sanitize.Text(t.Assumption, decision.MaxAssumption), w, p.oneLine)...)
	}
	if t.DiffersFrom != "" {
		why := sanitize.Text(t.DiffersWhy, decision.MaxDiffersWhy)
		out = append(out, para("differs from "+sanitize.Line(t.DiffersFrom, maxIDCells)+": "+why, w, p.oneLine)...)
	}
	if m.reasoning != "" {
		out = append(out, para("staged reasoning: "+sanitize.Text(m.reasoning, decision.MaxReasoning), w, p.oneLine)...)
	}
	if m.scope != "" {
		out = append(out, "staged scope: "+string(m.scope))
	}
	return out
}

// para is s, already cleaned, wrapped to w, or cut to one line when
// oneLine is set; empty s has no lines.
func para(s string, w int, oneLine bool) []string {
	if s == "" || !oneLine {
		return wrap(s, w)
	}
	return []string{truncate(strings.ReplaceAll(s, "\n", " "), w)}
}

// maxNoticeLines bounds the lines a notice is wrapped over.
const maxNoticeLines = 4

// renderStatus is the bottom of the view: the pending confirmation, the
// text being typed, the last outcome or notice, the last refresh error, or
// the keys. A notice is wrapped over as many lines as it needs, up to
// maxNoticeLines (M2: an outcome note tells the user what to do); the rest
// is one line.
func (m DecideModel) renderStatus(w int) []string {
	style, text := helpStyle, m.helpText(w)
	switch {
	case m.closing:
		style, text = lipgloss.NewStyle(), "closing once the answer is sent"
	case m.confirming:
		style, text = lipgloss.NewStyle(), m.confirmText(w)
	case m.input != inputNone:
		return []string{m.inputLine(w)}
	case m.notice != "":
		style = lipgloss.NewStyle()
		if m.noticeErr {
			style = errorStyle
		}
		return styled(style, capLines(wrap(sanitize.Line(m.notice, maxNote), w), maxNoticeLines, w))
	case m.err != nil:
		style, text = errorStyle, "refresh failed: "+sanitize.Line(m.err.Error(), maxNote)
	}
	return []string{style.Render(truncate(text, w))}
}

// helpText is the keys line. It names the key Enter sends, and is shorter
// when the window is too narrow for the long one.
func (m DecideModel) helpText(w int) string {
	q := "quit"
	if m.opts.Embedded {
		q = "close"
	}
	t, ok := m.current()
	if !ok {
		return "q " + q
	}
	rec := sanitize.Line(t.Recommendation, decision.MaxKey)
	long := "1-4 answer  enter=" + rec + " recommended  w write  r reason  s scope  q " + q
	if lipgloss.Width(long) <= w {
		return long
	}
	return "1-4 enter=" + rec + " w:text r:why s:scope q:" + q
}

// confirmText asks to confirm the pending answer to an irreversible ticket;
// when the question does not fit, what "y" does comes first.
func (m DecideModel) confirmText(w int) string {
	id, what := ticketID(m.pending.ticket), answerText(m.pending.a)
	if s := fmt.Sprintf("answer %s with %s? y sends, any other key cancels", id, what); lipgloss.Width(s) <= w {
		return s
	}
	return fmt.Sprintf("y sends: %s %s; other keys cancel", id, what)
}

// answerText is how an answer is named on the status line: its option key,
// or its free text quoted and cut.
func answerText(a model.Answer) string {
	if a.Key != "" {
		return sanitize.Line(a.Key, decision.MaxKey)
	}
	return `"` + sanitize.Line(a.Text, 24) + `"`
}

// outcomeText is the status after an answer: "D-9 answered: B ·
// delivered", with the stored status word (confirmed, overridden) and the
// outcome's note in place of "delivered" (M2).
func outcomeText(msg decideAnsweredMsg) string {
	note := "delivered"
	if msg.out.Note != "" {
		note = msg.out.Note
	}
	word := sanitize.Line(string(msg.out.Decision.Status), maxWordCells)
	if word == "" {
		word = string(model.StatusAnswered)
	}
	return sanitize.Line(msg.id, maxIDCells) + " " + word + ": " + answerText(msg.a) + " · " + sanitize.Line(note, maxNote)
}

// undelivered reports whether out's note says its answer was not delivered,
// or not for certain: it is drawn as an error. A note on an answer with
// nothing to deliver is not.
func undelivered(out model.Outcome) bool {
	return out.Note != "" && out.Decision.Delivery != model.DeliveryNone && out.Decision.Delivery != ""
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

package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

const (
	// maxBranchCells bounds a drawn branch name (task.ValidBranch allows 200).
	maxBranchCells = 200
	// maxDiffBytes and maxDiffLines bound the drawn diff stat (plan section
	// 4.5: 64 KiB and 200 lines, and the command's "… N more lines").
	maxDiffBytes = 64 << 10
	maxDiffLines = 201
	// maxTicketQuestion and maxTicketAnswer bound a ticket's text on its
	// line (section 4.5).
	maxTicketQuestion  = 100
	maxDecidedQuestion = 80
	maxDecidedAnswer   = 60
	// maxHelpLines bounds the keys line when it wraps.
	maxHelpLines = 3
	// maxResultLines bounds an action's outcome line when it wraps: at 40
	// columns its 300 runes need 8, and its end says what to do next.
	maxResultLines = 8
	// minHeadCells is the narrowest the head line is drawn beside the
	// position; narrower, the position gets a line of its own.
	minHeadCells = 12
)

// lineKind is how a line is drawn.
type lineKind int

const (
	linePlain lineKind = iota
	lineHead           // bold: the head line and section headings
	lineAlert          // red: open tickets, failed tests, errors
	lineWarn           // yellow: the diff's warning
	lineFaint          // dim: keys and hints
)

// bodyLine is one drawn line: text already cleaned and at most the width,
// and how it is drawn.
type bodyLine struct {
	text string
	kind lineKind
}

func styleOf(k lineKind) lipgloss.Style {
	switch k {
	case lineHead:
		return headerStyle
	case lineAlert:
		return errorStyle
	case lineWarn:
		return stakesStyle
	case lineFaint:
		return helpStyle
	}
	return lipgloss.NewStyle()
}

// kindLines wraps s, already cleaned, to w cells as lines of kind k, keeping at
// most n of them (0: all).
func kindLines(k lineKind, s string, w, n int) []bodyLine {
	var out []bodyLine
	for _, l := range capLines(wrap(s, w), n, w) {
		out = append(out, bodyLine{text: l, kind: k})
	}
	return out
}

// View draws the head line pinned at the top, the body from the scroll
// offset, and the outcome and status lines pinned at the bottom: every
// line at most the window width and the whole exactly its height.
func (m ReviewModel) View() string {
	w := m.textWidth()
	top, bottom := m.top(w), m.bottom(w)
	room := len(m.body)
	if m.height > 0 {
		room = max(m.height-len(top)-len(bottom), 0)
	}
	start := min(m.offset, max(len(m.body)-room, 0))
	out := append([]bodyLine(nil), top...)
	out = append(out, m.body[start:min(start+room, len(m.body))]...)
	for m.height > 0 && len(out) < m.height-len(bottom) {
		out = append(out, bodyLine{})
	}
	out = append(out, bottom...)
	if m.height > 0 && len(out) > m.height {
		out = out[len(out)-m.height:] // the prompt and its keys matter most
	}
	clip := lipgloss.NewStyle().MaxWidth(w)
	drawn := make([]string, len(out))
	for i, l := range out {
		drawn[i] = clip.Render(styleOf(l.kind).Render(l.text))
	}
	return strings.Join(drawn, "\n")
}

// textWidth is the window width, or defaultWidth before it is known.
func (m ReviewModel) textWidth() int {
	if m.width > 0 {
		return m.width
	}
	return defaultWidth
}

// relayout rebuilds the body when what is shown or the width changed, and
// keeps the scroll offset inside it.
func (m ReviewModel) relayout() ReviewModel {
	w := m.textWidth()
	if f := m.face(); f != m.bodyFace || w != m.bodyWidth {
		m.body, m.alerts = m.buildBody(w)
		m.bodyFace, m.bodyWidth = f, w
	}
	return m.clampOffset()
}

// bodyRoom is how many body lines are on screen.
func (m ReviewModel) bodyRoom() int {
	if m.height <= 0 {
		return len(m.body)
	}
	w := m.textWidth()
	return max(m.height-len(m.top(w))-len(m.bottom(w)), 1)
}

func (m ReviewModel) clampOffset() ReviewModel {
	m.offset = max(min(m.offset, len(m.body)-m.bodyRoom()), 0)
	return m
}

// top is the head line with the position at its right, and, when the open
// tickets are scrolled out of sight, a red line saying they are there.
func (m ReviewModel) top(w int) []bodyLine {
	if !m.loaded {
		return []bodyLine{{text: "reading reviews…", kind: lineFaint}}
	}
	r, ok := m.current()
	if !ok {
		return kindLines(linePlain, m.emptyText(), w, 0)
	}
	out := headLines(m.headText(r), fmt.Sprintf("review %d of %d", m.cursor+1, len(m.rows)), w)
	if d, ok := m.shownDetail(); ok && m.alerts > 0 && m.offset > 0 {
		out = append(out, kindLines(lineAlert, ticketCounts(d)+" above (k scrolls up)", w, 0)...)
	}
	return out
}

// headLines wraps head beside pos, which ends the first line.
func headLines(head, pos string, w int) []bodyLine {
	hw := w - lipgloss.Width(pos) - 1
	if hw < minHeadCells {
		return append([]bodyLine{{text: fit(pos, w, true), kind: lineHead}}, kindLines(lineHead, head, w, 0)...)
	}
	out := kindLines(lineHead, head, hw, 0)
	out[0].text = fit(out[0].text, hw, false) + " " + pos
	return out
}

func (m ReviewModel) emptyText() string {
	if m.opts.TaskID != "" {
		return sanitize.Line(m.opts.TaskID, maxIDCells) + " has no review waiting"
	}
	return "no review is waiting"
}

// headText is "R-3 · T-42 Fix the auth refresh · api-server ·
// curia/T-42-fix-the-auth-refresh → main · waiting 12m", from the detail
// once it is read.
func (m ReviewModel) headText(r ReviewRow) string {
	rv, t := r.Review, r.Task
	if d, ok := m.shownDetail(); ok {
		rv, t = d.Review, d.Task
	}
	taskID := t.ID
	if taskID == "" {
		taskID = rv.TaskID
	}
	parts := []string{
		sanitize.Line(rv.ID, maxIDCells),
		strings.TrimSpace(sanitize.Line(taskID, maxIDCells) + " " + sanitize.Line(t.Title, task.MaxTitle)),
	}
	if repo := sanitize.Line(RepoName(t.Repo), maxRepoCells); repo != "" {
		parts = append(parts, repo)
	}
	parts = append(parts, branchText(t.Branch, "(no branch)")+" → "+branchText(t.BaseBranch, "(no base)"))
	if age := Age(rv.CreatedAt, m.now()); age != "" {
		parts = append(parts, "waiting "+age)
	}
	return strings.Join(parts, " · ")
}

// branchText is branch b as drawn, or none when it is empty.
func branchText(b, none string) string {
	if s := sanitize.Line(b, maxBranchCells); s != "" {
		return s
	}
	return none
}

// ticketCounts is "2 unconfirmed assumptions, 1 open blocking ticket".
func ticketCounts(d ReviewDetail) string {
	var parts []string
	if n := len(d.Assumptions); n > 0 {
		parts = append(parts, plural(n, "unconfirmed assumption"))
	}
	if n := len(d.Blocking); n > 0 {
		parts = append(parts, plural(n, "open blocking ticket"))
	}
	return strings.Join(parts, ", ")
}

// plural is "1 thing" or "n things".
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// buildBody is section 4.5 from its second item: the open tickets in red,
// the summary, the tests, the diff, the decisions and the sessions. alerts
// is how many of the first lines are open tickets.
func (m ReviewModel) buildBody(w int) (body []bodyLine, alerts int) {
	if _, ok := m.current(); !ok {
		return nil, 0
	}
	d, ok := m.shownDetail()
	if !ok {
		return []bodyLine{{text: "reading the review…", kind: lineFaint}}, 0
	}
	for _, a := range d.Assumptions {
		body = append(body, kindLines(lineAlert, assumptionText(a), w, 0)...)
	}
	for _, b := range d.Blocking {
		text := sanitize.Line(b.ID, maxIDCells) + " open blocking ticket: " + sanitize.Line(b.Question, maxTicketQuestion)
		body = append(body, kindLines(lineAlert, text, w, 0)...)
	}
	alerts = len(body)
	if alerts > 0 {
		body = append(body, bodyLine{})
	}
	body = append(body, summaryLines(d.Review, w)...)
	body = append(body, diffLines(d, w)...)
	body = append(body, decidedLines(d.Decided, w)...)
	return append(body, kindLines(linePlain, sessionsText(d), w, 0)...), alerts
}

func assumptionText(a model.Decision) string {
	return sanitize.Line(a.ID, maxIDCells) + " unconfirmed assumption: " + sanitize.Line(a.Question, maxTicketQuestion) +
		" → proceeding with: " + sanitize.Line(a.Assumption, maxTicketQuestion)
}

// summaryLines are the agent's summary and its test report.
func summaryLines(r model.Review, w int) []bodyLine {
	out := []bodyLine{{text: "Summary (the agent's):", kind: lineHead}}
	summary := sanitize.Text(r.Summary, task.MaxSummary)
	if summary == "" {
		summary = "(none)"
	}
	out = append(out, kindLines(linePlain, summary, w, 0)...)
	out = append(out, bodyLine{})
	word, kind := testsWord(r.Tests)
	out = append(out, kindLines(kind, "Tests (the agent's report): "+word, w, 0)...)
	out = append(out, kindLines(linePlain, sanitize.Line(r.TestsDetail, task.MaxTestsDetail), w, 0)...)
	return append(out, bodyLine{})
}

// testsWord is how the agent's test status reads, red when they failed.
func testsWord(s model.TestsStatus) (string, lineKind) {
	switch s {
	case model.TestsFailed:
		return "failed", lineAlert
	case model.TestsNotRun:
		return "not run", linePlain
	case "":
		return "not reported", linePlain
	}
	return sanitize.Line(string(s), maxWordCells), linePlain
}

// diffLines are the diff stat, each line cut to the width (its columns
// line up), and the warning under it.
func diffLines(d ReviewDetail, w int) []bodyLine {
	head := "Diff against " + branchText(d.Task.BaseBranch, "(no base)")
	if len(d.Head) >= 7 && strings.Trim(d.Head[:7], "0123456789abcdef") == "" { // a commit id, nothing else
		head += ", branch at " + d.Head[:7]
	}
	out := kindLines(lineHead, head+":", w, 0)
	stat := sanitize.Block(d.DiffStat, maxDiffBytes)
	if stat != "" {
		ls := strings.Split(stat, "\n")
		if n := len(ls) - maxDiffLines; n > 0 {
			ls = append(ls[:maxDiffLines:maxDiffLines], fmt.Sprintf("… %d more lines", n))
		}
		for _, l := range ls {
			out = append(out, bodyLine{text: truncate(strings.ReplaceAll(l, "\t", "    "), w)})
		}
	}
	note := sanitize.Line(d.DiffNote, maxNote)
	if note != "" {
		out = append(out, kindLines(lineWarn, note, w, 0)...)
	}
	if stat == "" && note == "" {
		out = append(out, bodyLine{text: "nothing to show", kind: lineFaint})
	}
	return append(out, bodyLine{})
}

// decidedLines are "D-4 answered: <question> → <answer>", one per decided
// ticket of the task.
func decidedLines(ds []model.Decision, w int) []bodyLine {
	if len(ds) == 0 {
		return []bodyLine{{text: "Decisions on this task: none", kind: lineHead}, {}}
	}
	out := []bodyLine{{text: "Decisions on this task:", kind: lineHead}}
	for _, d := range ds {
		word := sanitize.Line(string(d.Status), maxWordCells)
		if word == "" {
			word = string(model.StatusAnswered)
		}
		text := sanitize.Line(d.ID, maxIDCells) + " " + word + ": " + sanitize.Line(d.Question, maxDecidedQuestion) +
			" → " + sanitize.Line(answerOf(d), maxDecidedAnswer)
		out = append(out, kindLines(linePlain, text, w, 0)...)
	}
	return append(out, bodyLine{})
}

// answerOf is a decided ticket's answer: its free text, or its option key
// and label.
func answerOf(d model.Decision) string {
	if d.Answer != "" {
		return d.Answer
	}
	for _, o := range d.Options {
		if o.Key == d.AnswerKey {
			return o.Key + " " + o.Label
		}
	}
	return d.AnswerKey
}

// sessionsText is "Sessions: 3 (lead and 2 teammates) · duration 1h12m ·
// cost $4.12", or "cost not recorded".
func sessionsText(d ReviewDetail) string {
	s := fmt.Sprintf("Sessions: %d", d.Sessions)
	if d.Teammates > 0 {
		s += " (lead and " + plural(d.Teammates, "teammate") + ")"
	}
	s += " · duration " + durationText(d.Duration)
	if d.CostKnown {
		return s + fmt.Sprintf(" · cost $%.2f", d.CostUSD)
	}
	return s + " · cost not recorded"
}

// durationText is "1h12m", "12m" or "45s", and "unknown" when not positive.
func durationText(d time.Duration) string {
	switch {
	case d <= 0:
		return "unknown"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// bottom is the last action's outcome, then the status: the action in
// flight, the prompt, the note input, a notice, a read error or the keys.
func (m ReviewModel) bottom(w int) []bodyLine {
	var out []bodyLine
	if m.result != "" {
		kind := linePlain
		if m.resultErr {
			kind = lineAlert
		}
		out = kindLines(kind, sanitize.Line(m.result, maxNote), w, maxResultLines)
	}
	return append(out, m.status(w)...)
}

func (m ReviewModel) status(w int) []bodyLine {
	switch {
	case m.busy:
		s := "working…"
		if m.closeAsked {
			s += "; the view cannot close until it returns"
		}
		return kindLines(linePlain, s, w, 0)
	case m.diffing:
		return kindLines(linePlain, "opening the diff…", w, 0)
	case m.mode == reviewConfirmApprove || m.mode == reviewConfirmDrop:
		return append(m.noticeLines(w), m.promptLines(w)...)
	case m.mode == reviewNote:
		return m.noteLines(w)
	case m.notice != "":
		return m.noticeLines(w)
	case m.err != nil:
		return kindLines(lineAlert, "refresh failed: "+sanitize.Line(m.err.Error(), maxNote), w, 2)
	case m.detailErr != nil:
		text := "reading " + sanitize.Line(m.selectedID(), maxIDCells) + " failed: " + sanitize.Line(m.detailErr.Error(), maxNote)
		return kindLines(lineAlert, text, w, 2)
	}
	return m.helpLines(w)
}

// noticeLines is the notice, red when it is an error, or nothing.
func (m ReviewModel) noticeLines(w int) []bodyLine {
	kind := linePlain
	if m.noticeErr {
		kind = lineAlert
	}
	return kindLines(kind, sanitize.Line(m.notice, maxNote), w, maxNoticeLines)
}

// promptLines ask to confirm the pending action; an approval that expires
// unconfirmed assumptions says so in red above it.
func (m ReviewModel) promptLines(w int) []bodyLine {
	d := m.pending
	branch, base := branchText(d.Task.Branch, "(no branch)"), branchText(d.Task.BaseBranch, "(no base)")
	if m.mode == reviewConfirmDrop {
		taskID := d.Task.ID
		if taskID == "" {
			taskID = d.Review.TaskID
		}
		return kindLines(linePlain, "drop "+sanitize.Line(taskID, maxIDCells)+"? its agent is not stopped. y confirms, any other key cancels", w, 0)
	}
	var out []bodyLine
	if n := len(d.Assumptions); n > 0 {
		ids := make([]string, n)
		for i, a := range d.Assumptions {
			ids[i] = sanitize.Line(a.ID, maxIDCells)
		}
		out = kindLines(lineAlert, fmt.Sprintf("%s (%s) will expire.", plural(n, "unconfirmed assumption"), strings.Join(ids, ", ")), w, 0)
	}
	text := "merge " + branch + " into " + base + "? y confirms, any other key cancels"
	if d.Approve == model.ApprovePR {
		text = "push " + branch + " and open a pull request? y confirms, any other key cancels"
	}
	return append(out, kindLines(linePlain, text, w, 0)...)
}

// noteLines are the note input: what it is for and how to send it, a
// notice, and the end of the note with a cursor.
func (m ReviewModel) noteLines(w int) []bodyLine {
	head := fmt.Sprintf("request changes on %s · enter sends · esc cancels · %d/%d",
		sanitize.Line(m.pendingID, maxIDCells), len(m.buf), model.MaxReviewNote)
	out := kindLines(lineFaint, head, w, 0)
	if m.notice != "" {
		out = append(out, m.noticeLines(w)...)
	}
	return append(out, bodyLine{text: "> " + tail(clean(string(m.buf)), max(w-3, 0)) + "_"})
}

// helpLines are the keys and what a will do (section 4.5, item 8).
func (m ReviewModel) helpLines(w int) []bodyLine {
	q := "q quit"
	if m.opts.Embedded {
		q = "q close"
	}
	if _, ok := m.current(); !ok {
		return []bodyLine{{text: q, kind: lineFaint}}
	}
	a := "a approve"
	if d, ok := m.shownDetail(); ok {
		switch d.Approve {
		case model.ApproveMerge:
			a = "a merge into " + branchText(d.Task.BaseBranch, "(no base)")
		case model.ApprovePR:
			a = "a push and open a pull request"
		}
	}
	return kindLines(lineFaint, a+" · c request changes · x drop · o diff · ]/[ next/prev · j/k scroll · "+q, w, maxHelpLines)
}

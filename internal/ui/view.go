package ui

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/usage"
)

// column is one column of the table.
type column int

const (
	colState column = iota
	colWindow
	colRepo
	colTask
	colModel
	colContext
	colSubagents
	colDecisions
	colCost
	colCache
	colUpdate
	colAge
)

// colSpec says how a column is laid out. A flex column shrinks, widest
// first, down to minFlex cells when the table is too wide; a right column
// is right-aligned.
type colSpec struct {
	title       string
	flex, right bool
}

var specs = map[column]colSpec{
	colState:     {title: ""},
	colWindow:    {title: "SESSION:WINDOW", flex: true},
	colRepo:      {title: "REPO@BRANCH", flex: true},
	colTask:      {title: "TASK", flex: true},
	colModel:     {title: "MODEL", flex: true},
	colContext:   {title: "CTX", right: true},
	colSubagents: {title: "SUB", right: true},
	colDecisions: {title: "DEC", right: true},
	colCost:      {title: "COST", right: true},
	colCache:     {title: "CACHE"},
	colUpdate:    {title: "LAST UPDATE", flex: true},
	colAge:       {title: "AGE", right: true},
}

// The popup's columns by width (Plan 1 live-check defect 13): all of them
// from fullWidth, without MODEL and SUB from costWidth, also without COST
// and CACHE from midWidth (Plan 3's columns), also without LAST UPDATE from
// narrowWidth, and below it only state, window, TASK (the id alone), DEC
// and AGE. The sidebar always has its four.
var (
	popupColumns = []column{
		colState, colWindow, colRepo, colTask, colModel, colContext, colSubagents, colDecisions, colCost, colCache, colUpdate, colAge,
	}
	costColumns = []column{
		colState, colWindow, colRepo, colTask, colContext, colDecisions, colCost, colCache, colUpdate, colAge,
	}
	midColumns     = []column{colState, colWindow, colRepo, colTask, colContext, colDecisions, colUpdate, colAge}
	narrowColumns  = []column{colState, colWindow, colRepo, colTask, colContext, colDecisions, colAge}
	tinyColumns    = []column{colState, colWindow, colTask, colDecisions, colAge}
	sidebarColumns = []column{colState, colWindow, colContext, colAge}
)

const (
	fullWidth   = 120
	costWidth   = 100
	midWidth    = 90
	narrowWidth = 60
)

const (
	// minFlex is the narrowest a flex column shrinks to.
	minFlex = 3
	// ellipsis ends a truncated cell.
	ellipsis = "…"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	staleStyle  = lipgloss.NewStyle().Faint(true)
	helpStyle   = lipgloss.NewStyle().Faint(true)
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	reviewStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	pauseStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// cacheWarnLeft is how little warm cache time the CACHE cell shows yellow.
const cacheWarnLeft = 5 * time.Minute

// burnNote ends the header while the burn sort is on.
const burnNote = "sorted by burn (b)"

// reviewReady reports whether s is review-ready: idle, with its task's
// review waiting.
func reviewReady(s model.Session) bool {
	return s.ReviewPending && s.State == model.StateIdle
}

// rowIcon is the state cell of s: a review-ready session's "◆", else its
// state's icon.
func rowIcon(s model.Session) string {
	if reviewReady(s) {
		return "◆"
	}
	return stateIcon(s.State)
}

// stateStyle colors the state cell of s: blue when it is review-ready.
func stateStyle(s model.Session) lipgloss.Style {
	if reviewReady(s) {
		return reviewStyle
	}
	return lipgloss.NewStyle()
}

// levelStyle colors a context percentage: green, yellow, red, or plain when
// the level is unknown.
func levelStyle(l Level) lipgloss.Style {
	s := lipgloss.NewStyle()
	switch l {
	case LevelOK:
		return s.Foreground(lipgloss.Color("2"))
	case LevelWarn:
		return s.Foreground(lipgloss.Color("3"))
	case LevelCrit:
		return s.Foreground(lipgloss.Color("1"))
	default:
		return s
	}
}

// decStyle colors a session's open ticket count: red when any is blocking.
func decStyle(c model.OpenCount) lipgloss.Style {
	if c.Blocking > 0 {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	}
	return lipgloss.NewStyle()
}

// cacheStyle colors a CACHE cell by the expiry it shows at now: yellow
// while warm with less than cacheWarnLeft left, dim once cold, plain when
// warm for longer or unknown (a zero expiry).
func cacheStyle(expires, now time.Time) lipgloss.Style {
	switch left := expires.Sub(now); {
	case expires.IsZero():
		return lipgloss.NewStyle()
	case left <= 0:
		return lipgloss.NewStyle().Faint(true)
	case left < cacheWarnLeft:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	default:
		return lipgloss.NewStyle()
	}
}

// stateIcon is the one-cell icon of a state.
func stateIcon(s model.State) string {
	switch s {
	case model.StateNeedsDecision:
		return "◆"
	case model.StateWaiting:
		return "●"
	case model.StateWorking:
		return "◐"
	case model.StateIdle:
		return "○"
	case model.StateStale:
		return "×"
	case model.StateEnded:
		return "-"
	default:
		return "?"
	}
}

// View renders the header, the rows on screen and the status line, every
// line at most the window width; while the decide view is shown, that view.
func (m Model) View() string {
	if m.child != nil {
		return m.child.View()
	}
	lay := m.layout(m.now())
	lines := []string{m.renderHeader(lay)}
	if len(m.rows) == 0 {
		lines = append(lines, m.emptyText())
	}
	for i, c := range lay.cells {
		lines = append(lines, m.renderRow(m.rows[lay.first+i], c, lay, lay.first+i == m.cursor))
	}
	notice := m.pauseNotice()
	bottom := 1
	if notice != "" {
		bottom = 2
	}
	for m.height > 0 && len(lines) < m.height-bottom {
		lines = append(lines, "")
	}
	if notice != "" {
		lines = append(lines, pauseStyle.Render(m.fitLine(notice)))
	}
	lines = append(lines, m.renderStatus())
	if m.width > 0 {
		clip := lipgloss.NewStyle().MaxWidth(m.width)
		for i, l := range lines {
			lines[i] = clip.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// tableLayout is how View lays out the table at now.
type tableLayout struct {
	now    time.Time
	cols   []column
	widths []int      // each column's width, in cells
	gap    int        // the cells between two columns
	first  int        // the index of the first row on screen
	cells  [][]string // the cells of the rows on screen
	note   string     // what the header shows after its titles: burnNote or ""
}

// layout lays out the rows on screen at now. With the burn sort on, the
// columns leave room for burnNote at the header's end, so the note is seen
// at any width the columns can shrink to.
func (m Model) layout(now time.Time) tableLayout {
	cols, gap := m.columns()
	first, last := m.offset, len(m.rows)
	if n := m.visibleRows(); n > 0 {
		last = min(last, first+n)
	}
	cells := make([][]string, 0, last-first)
	for _, r := range m.rows[first:last] {
		cells = append(cells, m.cells(r, cols, now))
	}
	width, note := m.width, ""
	if m.burnSort {
		note = burnNote
		if width > 0 {
			width = max(width-gap-lipgloss.Width(note), 1)
		}
	}
	widths := fitWidths(cols, m.titles(cols), cells, width, gap)
	return tableLayout{now: now, cols: cols, widths: widths, gap: gap, first: first, cells: cells, note: note}
}

// columns are the columns at the window's width and the gap between them;
// an unknown width (0) gets every column.
func (m Model) columns() ([]column, int) {
	if m.mode == ModeSidebar {
		return sidebarColumns, 1
	}
	switch w := m.width; {
	case w <= 0 || w >= fullWidth:
		return popupColumns, 2
	case w >= costWidth:
		return costColumns, 2
	case w >= midWidth:
		return midColumns, 2
	case w >= narrowWidth:
		return narrowColumns, 2
	default:
		return tinyColumns, 2
	}
}

// titles are the header cells of cols.
func (m Model) titles(cols []column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = specs[c].title
		if c == colWindow && m.mode == ModeSidebar {
			out[i] = "WINDOW"
		}
	}
	return out
}

// cells are the texts of r's columns at now. Session and task text is
// untrusted, so every piece of it goes through Cell; the DEC, COST and
// CACHE cells are Curia's own text, empty when there is nothing to show,
// and the TASK cell is empty without a task.
func (m Model) cells(r row, cols []column, now time.Time) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c {
		case colState:
			out[i] = rowIcon(r.target())
		case colWindow:
			out[i] = m.windowCell(r)
		case colTask:
			out[i] = m.taskText(r.s)
		case colDecisions:
			out[i] = decText(m.rowCount(r))
		case colCost:
			out[i] = costText(r)
		case colCache:
			out[i], _ = cacheText(r, now)
		default:
			out[i] = Cell(m.cellText(r.s, c))
		}
	}
	return out
}

// windowCell is the window cell of r: a group row's is "▸ <lead's window>
// +<teammates>", an expanded lead's "▾ <window>", and a teammate's its
// member name (else its window) indented two cells.
func (m Model) windowCell(r row) string {
	w := Cell(m.windowText(r.s))
	switch r.kind {
	case rowGroup:
		return "▸ " + w + " +" + strconv.Itoa(len(r.members)-1)
	case rowLead:
		return "▾ " + w
	case rowMate:
		if r.s.TeamMember != "" {
			w = Cell(r.s.TeamMember)
		}
		return "  " + w
	default:
		return w
	}
}

// taskText is the TASK cell of s: its task's id and title, the id alone
// below narrowWidth or when the task is not known, and empty without one.
func (m Model) taskText(s model.Session) string {
	if s.TaskID == "" {
		return ""
	}
	id := Cell(s.TaskID)
	t, ok := m.tasks[s.TaskID]
	if !ok || t.Title == "" || (m.width > 0 && m.width < narrowWidth) {
		return id
	}
	return id + " " + Cell(t.Title)
}

// rowCount is the open tickets and undelivered answers of r's session, or
// of all a group row's members together.
func (m Model) rowCount(r row) model.OpenCount {
	if r.kind != rowGroup {
		return m.counts[r.s.ID]
	}
	var sum model.OpenCount
	for _, s := range r.members {
		c := m.counts[s.ID]
		sum.Blocking += c.Blocking
		sum.Assume += c.Assume
		sum.Undelivered += c.Undelivered
	}
	return sum
}

// costText is the COST cell of r: a group row's members' known costs
// summed (fleet.Unit.Cost), else the session's own; blank when unknown.
func costText(r row) string {
	if r.kind == rowGroup {
		return usage.CostCell(r.unit().Cost())
	}
	return usage.CostCell(r.s.CostUSD, r.s.CostKnown)
}

// cacheText is the CACHE cell of r at now and the expiry its style is
// judged by. A session's own row shows usage.CacheCell. A group row shows
// the cache fleet.Unit.CacheOf picks: the soonest warm expiry among its
// idle, waiting and needs_decision members, else "cold" when a member's
// cache is cold (in any state), else blank.
func cacheText(r row, now time.Time) (string, time.Time) {
	if r.kind != rowGroup {
		return usage.CacheCell(r.s, now), r.s.CacheExpiresAt
	}
	s, ok := r.unit().CacheOf(now)
	switch {
	case !ok:
		return "", time.Time{}
	case s.CacheExpiresAt.After(now):
		return usage.CacheCell(s, now), s.CacheExpiresAt
	default:
		return "cold", s.CacheExpiresAt
	}
}

// decText is the number of open tickets of both modes in c, then "Q<n>"
// for its answers not yet delivered (Ruling 89), empty when there are
// neither.
func decText(c model.OpenCount) string {
	var parts []string
	if n := c.Blocking + c.Assume; n > 0 {
		parts = append(parts, strconv.Itoa(n))
	}
	if c.Undelivered > 0 {
		parts = append(parts, "Q"+strconv.Itoa(c.Undelivered))
	}
	return strings.Join(parts, " ")
}

func (m Model) cellText(s model.Session, c column) string {
	switch c {
	case colRepo:
		return repoText(s)
	case colModel:
		return s.Model
	case colContext:
		if s.ContextMax <= 0 {
			return ""
		}
		return fmt.Sprintf("%d%%", contextPct(s.ContextUsed, s.ContextMax))
	case colSubagents:
		return strconv.Itoa(s.SubagentsActive)
	case colUpdate:
		return s.LastUpdate
	default:
		return Age(s.UpdatedAt, m.now())
	}
}

// repoText is "repo@branch" with the basename of the stored repo path
// (Ruling 22), the repo alone without a branch, and empty without a repo.
func repoText(s model.Session) string {
	repo := RepoName(s.Repo)
	if repo == "" {
		return ""
	}
	if s.Branch == "" {
		return repo
	}
	return repo + "@" + s.Branch
}

// RepoName is how views show a stored repository path: its basename
// (Ruling 22), or empty for no repository. curia ls uses it too.
func RepoName(repo string) string {
	if repo == "" {
		return ""
	}
	return filepath.Base(repo)
}

// windowText is "session:window" with the window's current name, or its
// stored id when tmux does not know it; empty outside tmux.
func (m Model) windowText(s model.Session) string {
	if s.TmuxSession == "" && s.TmuxWindow == "" {
		return ""
	}
	window := s.TmuxWindow
	if name := m.names[s.TmuxWindow]; name != "" {
		window = name
	}
	return s.TmuxSession + ":" + window
}

// fitWidths gives each column the width of its widest cell, then, when
// width is positive and the row would be wider, takes cells from the
// widest flex column until it fits or every flex column is at minFlex.
func fitWidths(cols []column, titles []string, rows [][]string, width, gap int) []int {
	w := make([]int, len(titles))
	total := gap * (len(titles) - 1)
	for i, t := range titles {
		w[i] = lipgloss.Width(t)
		for _, r := range rows {
			w[i] = max(w[i], lipgloss.Width(r[i]))
		}
		total += w[i]
	}
	for excess := total - width; width > 0 && excess > 0; excess-- {
		widest := -1
		for i, c := range cols {
			if specs[c].flex && w[i] > minFlex && (widest < 0 || w[i] > w[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		w[widest]--
	}
	return w
}

// renderHeader is the titles, then the layout's note, if any.
func (m Model) renderHeader(lay tableLayout) string {
	titles := m.titles(lay.cols)
	parts := make([]string, len(lay.cols))
	for i, c := range lay.cols {
		parts[i] = fit(titles[i], lay.widths[i], specs[c].right)
	}
	if lay.note != "" {
		parts = append(parts, lay.note)
	}
	return headerStyle.Render(strings.Join(parts, strings.Repeat(" ", lay.gap)))
}

// renderRow lays out one row: dim when its state is stale, reversed when
// selected, the state cell blue when review-ready, the context cell
// colored by its level, a shown cache cell by its expiry.
func (m Model) renderRow(r row, cells []string, lay tableLayout, selected bool) string {
	cols, widths, gap := lay.cols, lay.widths, lay.gap
	s := r.s
	base := lipgloss.NewStyle()
	if r.target().State == model.StateStale {
		base = staleStyle
	}
	if selected {
		base = base.Reverse(true)
	}
	level := ContextLevel(s.ContextUsed, s.ContextMax, m.cfg.ContextWarnPct, m.cfg.ContextCritPct)
	parts := make([]string, len(cols))
	for i, c := range cols {
		st := base
		switch c {
		case colState:
			st = stateStyle(r.target()).Inherit(base)
		case colContext:
			st = levelStyle(level).Inherit(base)
		case colDecisions:
			st = decStyle(m.rowCount(r)).Inherit(base)
		case colCache:
			if cells[i] != "" {
				_, expires := cacheText(r, lay.now)
				st = cacheStyle(expires, lay.now).Inherit(base)
			}
		}
		parts[i] = st.Render(fit(cells[i], widths[i], specs[c].right))
	}
	return strings.Join(parts, base.Render(strings.Repeat(" ", gap)))
}

func (m Model) emptyText() string {
	if m.filter != "" && len(m.sessions) > 0 {
		return "no sessions match /" + clean(m.filter)
	}
	return "no sessions"
}

// renderStatus is the bottom line: a pending kill, the filter being typed,
// the last key's notice, the last refresh error, or help, after the
// budget's status when there is one.
func (m Model) renderStatus() string {
	style, text := helpStyle, m.helpText()
	switch {
	case m.killPane != "":
		style, text = lipgloss.NewStyle(), "kill pane "+m.killPane+"? y kills, any other key cancels"
	case m.pauseAsk != (pauseSuggestion{}):
		style, text = lipgloss.NewStyle(), m.pausePrompt()
	case m.filtering:
		style, text = lipgloss.NewStyle(), "/"+m.filter
	case m.notice != "":
		style, text = lipgloss.NewStyle(), m.notice
	case m.err != nil:
		style, text = errorStyle, "refresh failed: "+m.err.Error()
	case m.budget.Status != "":
		return m.budgetStatus(text)
	}
	return style.Render(m.fitLine(text))
}

// fitLine is untrusted text cleaned and cut to the window's width.
func (m Model) fitLine(text string) string {
	text = clean(text)
	if m.width > 0 {
		text = truncate(text, m.width)
	}
	return text
}

// pauseNotice is the notice line of the pause suggestion (section 4.5),
// shown in the popup above the status line while the Budget suggests a
// task and P can pause it; "" otherwise. Its parts come from the Source and
// are cleaned when drawn.
func (m Model) pauseNotice() string {
	if m.mode != ModePopup || !m.pauseOn || m.budget.PauseTask == "" {
		return ""
	}
	return cmp.Or(m.budget.PauseLimit, "limit") + " before reset: P pauses " + m.budget.PauseLabel
}

// pausePrompt asks to confirm the pause the prompt names. The title is cut
// first, so the question and y/N fit whenever the window allows.
func (m Model) pausePrompt() string {
	head := "pause " + clean(m.pauseAsk.task) + ` "`
	tail := `"? it commits work in progress and closes its pane  y/N`
	title := clean(m.pauseAsk.title)
	if m.width > 0 {
		title = truncate(title, max(m.width-lipgloss.Width(head)-lipgloss.Width(tail), 0))
	}
	return head + title + tail
}

// budgetStatus is the budget's status, then help: the status first, so a
// narrow window cuts the help. The status comes from the Source, so it is
// cleaned as untrusted text is.
func (m Model) budgetStatus(help string) string {
	status, rest := clean(m.budget.Status), "  "+clean(help)
	if m.width > 0 {
		status = truncate(status, m.width)
		rest = truncate(rest, m.width-lipgloss.Width(status))
	}
	return lipgloss.NewStyle().Render(status) + helpStyle.Render(rest)
}

func (m Model) helpText() string {
	text := fmt.Sprintf("%d sessions", m.shownSessions())
	if m.filter != "" {
		text = fmt.Sprintf("%d of %d sessions match /%s", m.shownSessions(), len(m.sessions), m.filter)
	}
	if m.mode == ModePopup {
		text += "  enter jump  d decisions  r review  t tasks  b burn  / filter  x kill  q quit"
	}
	return text
}

// fit truncates s to w cells and pads it to exactly w, on the left when
// right is set.
func fit(s string, w int, right bool) string {
	s = truncate(s, w)
	pad := strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
	if right {
		return pad + s
	}
	return s + pad
}

// truncate cuts s to at most w cells, ending it with an ellipsis when cut.
// The kept part is measured whole, as lipgloss.Width measures the cell: a
// character's width can depend on the one after it (a variation selector
// turns a one-cell character into a two-cell emoji), so summing the runes'
// own widths would draw a cell wider than its column.
func truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	kept := ""
	for _, r := range s {
		next := kept + string(r)
		if lipgloss.Width(next) > w-1 {
			break
		}
		kept = next
	}
	return kept + ellipsis
}

// Cell makes untrusted text safe to print as one table cell: clean(s), or
// "-" for an empty cell. curia ls uses it too.
func Cell(s string) string {
	if s == "" {
		return "-"
	}
	return clean(s)
}

// clean makes untrusted text safe to draw: every character that is not
// printable (control characters such as ESC, C1 codes, tabs, line breaks,
// and invisible format characters such as bidi overrides) becomes '?', so
// no escape sequence reaches the terminal and a row stays one line.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) { // includes the ASCII space
			return r
		}
		return '?'
	}, s)
}

// Age is how long before now t was, in its largest whole unit, such as
// "5m"; empty for the zero time.
func Age(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(now.Sub(t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

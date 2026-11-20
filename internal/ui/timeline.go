package ui

import (
	"cmp"
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// timelinePage is how many events one read returns.
const timelinePage = 100

// timelineWindows are the spans `w` cycles through; the first is the default.
var timelineWindows = []time.Duration{24 * time.Hour, 6 * time.Hour, 7 * 24 * time.Hour}

// TimelineFilter selects what the timeline view reads; Since is the start of its window.
type TimelineFilter struct {
	Since        time.Time
	Repo, TaskID string
}

// TimelineSource reads one page of events newest first: the events of f after the cursor event in the order (a zero cursor: from now), at most limit. The cursor is the last event of the page before, so events that share a time are neither skipped nor repeated.
type TimelineSource func(ctx context.Context, f TimelineFilter, after model.TimelineEvent, limit int) ([]model.TimelineEvent, error)

// TimelineTextFunc reads the whole stored text of one event (the list holds only its cut line); the view cleans and bounds it.
type TimelineTextFunc func(ctx context.Context, e model.TimelineEvent) (string, error)

// TimelineOpenMsg asks the dashboard to open the existing view for an event: the agent detail for a session entry, the decide view for a ticket, the review view for a review.
type TimelineOpenMsg struct{ Event model.TimelineEvent }

// timelineTextLimit bounds the full text shown, in runes.
const timelineTextLimit = 4000

// timelineTarget is which existing view an event opens: "detail", "decide", "review" or "".
func timelineTarget(e model.TimelineEvent) string {
	switch {
	case e.SessionID != "" && (e.Kind == "session_start" || e.Kind == "session_end" || e.Kind == "turn_end"):
		return "detail"
	case e.SessionID != "" && (e.Kind == "ticket_raised" || e.Kind == "answered" || e.Kind == "confirmed" || e.Kind == "overridden"):
		return "decide"
	case e.TaskID != "" && (e.Kind == "review_opened" || e.Kind == "review_closed"):
		return "review"
	}
	return ""
}

// openFromTimeline opens the view for an event through the dashboard's own
// factories (the ones d, r and i use), or tells the timeline why not
// (read-only, inside an agent session, not available).
func (m Model) openFromTimeline(e model.TimelineEvent) (Model, tea.Cmd) {
	switch timelineTarget(e) {
	case "detail":
		if m.detail != nil {
			return m.showChild(m.detail(e.SessionID))
		}
		return m.tellTimeline("the agent detail opens in the popup (curia ui)")
	case "decide":
		if m.open != nil {
			return m.showChild(m.open(e.SessionID))
		}
		return m.tellTimeline(cmp.Or(m.noAnswer, noticeNoAnswer))
	case "review":
		if m.review != nil {
			return m.showChild(m.review(e.TaskID))
		}
		return m.tellTimeline(cmp.Or(m.noReview, noticeNoReview))
	}
	return m.tellTimeline("this entry has no view to open")
}

func (m Model) tellTimeline(text string) (Model, tea.Cmd) {
	var cmd tea.Cmd
	m.child, cmd = m.child.Update(timelineNoticeMsg{text})
	return m, wrapChild(m.childGen, cmd)
}

type timelineNoticeMsg struct{ text string }

// TimelineClosedMsg is sent when the timeline view closes.
type TimelineClosedMsg struct{}

// TimelineModel is the read-only timeline view (spec F25).
type TimelineModel struct {
	src    TimelineSource
	text   TimelineTextFunc // nil: the full view shows the line
	now    func() time.Time
	window int // index into timelineWindows
	repo   string
	task   string

	evs    []model.TimelineEvent // newest first, pages appended
	more   bool                  // the last page was full
	top    int                   // index of the first event on screen; Enter and o act on it
	full   bool                  // the full view of evs[top] is shown
	body   []string              // its wrapped text, once read
	bodyAt int                   // first body line on screen
	notice string
	gen    int  // the read in flight or last applied; older results are dropped
	typing bool // typing a filter
	input  string
	err    error
	width  int
	height int
}

// NewTimeline returns the timeline over src.
func NewTimeline(src TimelineSource, now func() time.Time) TimelineModel {
	return TimelineModel{src: src, now: now}
}

// WithText returns m whose Enter reads an event's whole text through f.
func (m TimelineModel) WithText(f TimelineTextFunc) TimelineModel {
	m.text = f
	return m
}

type timelineTextMsg struct {
	ref, kind string
	text      string
	err       error
}

type timelineLoadedMsg struct {
	gen    int
	evs    []model.TimelineEvent
	err    error
	append bool
}

// Init reads the first page.
func (m TimelineModel) Init() tea.Cmd { return m.load(false) }

func (m TimelineModel) load(more bool) tea.Cmd {
	f := TimelineFilter{Since: m.now().Add(-timelineWindows[m.window]), Repo: m.repo, TaskID: m.task}
	var after model.TimelineEvent
	if more && len(m.evs) > 0 {
		after = m.evs[len(m.evs)-1]
	}
	src, gen := m.src, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		evs, err := src(ctx, f, after, timelinePage)
		return timelineLoadedMsg{gen: gen, evs: evs, err: err, append: more}
	}
}

// reload starts a fresh read from the top, dropping any read in flight.
func (m TimelineModel) reload() (TimelineModel, tea.Cmd) {
	m.gen++
	m.evs, m.top, m.more = nil, 0, false
	return m, m.load(false)
}

// Update handles one message.
func (m TimelineModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case timelineLoadedMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.err = msg.err
		if msg.err == nil {
			m.evs = append(m.evs, msg.evs...)
			m.more = len(msg.evs) == timelinePage
		}
	case timelineNoticeMsg:
		m.notice = msg.text
	case timelineTextMsg:
		if m.full && m.selected().Ref == msg.ref && m.selected().Kind == msg.kind {
			m.body = m.bodyLines(msg.text, msg.err)
		}
	case tea.KeyMsg:
		m.notice = ""
		if m.full {
			return m.fullKey(msg)
		}
		if m.typing {
			return m.typed(msg)
		}
		return m.key(msg)
	}
	return m, nil
}

func (m TimelineModel) close() (tea.Model, tea.Cmd) {
	return m, func() tea.Msg { return TimelineClosedMsg{} }
}

func (m TimelineModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc":
		return m.close()
	case "j", "down":
		m.top = min(m.top+1, max(len(m.evs)-1, 0))
	case "k", "up":
		m.top = max(m.top-1, 0)
	case "enter":
		return m.showFull()
	case "o":
		return m.open()
	case "n": // older page
		if m.more {
			m.more = false // one read at a time
			return m, m.load(true)
		}
	case "w":
		m.window = (m.window + 1) % len(timelineWindows)
		return m.reload()
	case "r":
		return m.reload()
	case "/":
		m.typing, m.input = true, ""
	}
	return m, nil
}

// typed handles a key while a filter is typed: a task id (T-42) or a
// repository path; empty clears both.
func (m TimelineModel) typed(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.typing = false
	case tea.KeyEnter:
		m.typing, m.repo, m.task = false, "", ""
		if strings.HasPrefix(m.input, "T-") {
			m.task = m.input
		} else {
			m.repo = m.input
		}
		return m.reload()
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeyRunes, tea.KeySpace:
		if len(m.input) < 200 {
			m.input += string(msg.Runes)
		}
	}
	return m, nil
}

func (m TimelineModel) selected() model.TimelineEvent {
	if m.top < len(m.evs) {
		return m.evs[m.top]
	}
	return model.TimelineEvent{}
}

// open asks the dashboard for the view of the selected event.
func (m TimelineModel) open() (tea.Model, tea.Cmd) {
	e := m.selected()
	if timelineTarget(e) == "" {
		m.notice = "this entry has no view to open"
		return m, nil
	}
	return m, func() tea.Msg { return TimelineOpenMsg{Event: e} }
}

// showFull shows the selected event whole, reading its text off the event loop.
func (m TimelineModel) showFull() (tea.Model, tea.Cmd) {
	e := m.selected()
	if e.Kind == "" {
		return m, nil
	}
	m.full, m.bodyAt, m.body = true, 0, nil
	if m.text == nil {
		m.body = m.bodyLines(e.Line, nil)
		return m, nil
	}
	src := m.text
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		text, err := src(ctx, e)
		return timelineTextMsg{ref: e.Ref, kind: e.Kind, text: text, err: err}
	}
}

// bodyLines cleans, bounds and wraps text to the width.
func (m TimelineModel) bodyLines(text string, err error) []string {
	if err != nil {
		return []string{"read failed: " + sanitize.Line(err.Error(), 120)}
	}
	return wrap(sanitize.Text(text, timelineTextLimit), m.textWidth())
}

func (m TimelineModel) fullKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc", "enter":
		m.full = false
	case "o":
		return m.open()
	case "j", "down":
		m.bodyAt = min(m.bodyAt+1, max(len(m.body)-1, 0))
	case "k", "up":
		m.bodyAt = max(m.bodyAt-1, 0)
	}
	return m, nil
}

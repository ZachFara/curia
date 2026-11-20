package ui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
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

// TimelineSource reads one page of events newest first: the events of f strictly before `before` (zero: from now), at most limit.
type TimelineSource func(ctx context.Context, f TimelineFilter, before time.Time, limit int) ([]model.TimelineEvent, error)

// TimelineClosedMsg is sent when the timeline view closes.
type TimelineClosedMsg struct{}

// TimelineModel is the read-only timeline view (spec F25).
type TimelineModel struct {
	src    TimelineSource
	now    func() time.Time
	window int // index into timelineWindows
	repo   string
	task   string

	evs    []model.TimelineEvent // newest first, pages appended
	more   bool                  // the last page was full
	top    int                   // index of the first event on screen
	gen    int                   // the read in flight or last applied; older results are dropped
	typing bool                  // typing a filter
	input  string
	err    error
	width  int
	height int
}

// NewTimeline returns the timeline over src.
func NewTimeline(src TimelineSource, now func() time.Time) TimelineModel {
	return TimelineModel{src: src, now: now}
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
	var before time.Time
	if more && len(m.evs) > 0 {
		before = m.evs[len(m.evs)-1].At
	}
	src, gen := m.src, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		evs, err := src(ctx, f, before, timelinePage)
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
	case tea.KeyMsg:
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

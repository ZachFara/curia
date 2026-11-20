package ui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
)

// The agent detail view (spec F24): a read-only drill-in for one session.
// It offers no action of its own: Enter hands over to the dashboard's jump
// (DetailJumpMsg), Esc goes back.

const (
	// detailEvery is how often the shown session is read again.
	detailEvery = 2 * time.Second
	// detailTimeout bounds one Detail read; detailDiffTimeout one Diff (git).
	detailTimeout     = 10 * time.Second
	detailDiffTimeout = 30 * time.Second
	// maxDetailDiffLines bounds the drawn diff stat.
	maxDetailDiffLines = 40
)

// DetailData is what the view shows about one session. Every string in it is untrusted.
type DetailData struct {
	Session  model.Session
	Tail     []string // the transcript's last lines, already cleaned
	TailNote string   // why the tail is empty, or a warning; "" when none
	Tickets  []model.Decision
	Messages []model.Message
	// TaskCostUSD is the task branch's cost, when TaskCostKnown.
	TaskCostUSD   float64
	TaskCostKnown bool
	// Activity is F27's time by class and last events; the section is drawn
	// only when ActivityOK (a source with no F27 data leaves it false).
	Activity   []string
	ActivityOK bool
}

// DetailDiff is the diff panel: drawn only when Has (a session with no worktree has none).
type DetailDiff struct {
	Has        bool
	Stat, Note string
}

// DetailSource is where the view reads. Detail is quick (rows and a bounded
// transcript tail); Diff runs git and may take seconds, so it is asked apart.
type DetailSource interface {
	Detail(ctx context.Context, sessionID string) (DetailData, error)
	Diff(ctx context.Context, sessionID string) (DetailDiff, error)
}

// DetailClosedMsg is sent when the detail view closes (Esc, q).
type DetailClosedMsg struct{}

// DetailJumpMsg is sent on Enter: the dashboard closes the view and jumps to the pane as it does for its own Enter.
type DetailJumpMsg struct{}

type (
	detailMsg struct {
		data DetailData
		err  error
	}
	detailDiffMsg struct {
		diff DetailDiff
		err  error
	}
	detailTickMsg struct{}
)

// DetailModel is the detail view's Bubble Tea model.
type DetailModel struct {
	src  DetailSource
	id   string
	now  func() time.Time
	tick func(time.Duration, tea.Msg) tea.Cmd // schedules a refresh; tests replace it

	data    DetailData
	loaded  bool
	err     error // the last Detail read's error
	diff    DetailDiff
	diffErr error
	asking  bool // a Detail is in flight (one at a time)
	offset  int
	width   int
	height  int
}

// NewDetail returns the detail view of session id over src.
func NewDetail(src DetailSource, id string, now func() time.Time) DetailModel {
	return DetailModel{src: src, id: id, now: now, tick: reviewTick}
}

// Init reads the session and the diff and starts the refresh chain.
func (m DetailModel) Init() tea.Cmd {
	src, id := m.src, m.id
	return tea.Batch(m.readCmd(),
		call(detailDiffTimeout, func(ctx context.Context) tea.Msg {
			d, err := src.Diff(ctx, id)
			return detailDiffMsg{d, err}
		}),
		m.tick(detailEvery, detailTickMsg{}))
}

func (m DetailModel) readCmd() tea.Cmd {
	src, id := m.src, m.id
	return call(detailTimeout, func(ctx context.Context) tea.Msg {
		d, err := src.Detail(ctx, id)
		return detailMsg{d, err}
	})
}

// Update handles one message.
func (m DetailModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case detailMsg:
		m.asking, m.err = false, msg.err
		if msg.err == nil {
			m.data, m.loaded = msg.data, true
		}
	case detailDiffMsg:
		m.diff, m.diffErr = msg.diff, msg.err
	case detailTickMsg:
		next := m.tick(detailEvery, detailTickMsg{})
		if m.asking {
			return m, next
		}
		m.asking = true
		return m, tea.Batch(m.readCmd(), next)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m.clamp(), nil
}

func (m DetailModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q":
		return m, func() tea.Msg { return DetailClosedMsg{} }
	case "enter":
		return m, func() tea.Msg { return DetailJumpMsg{} }
	case "j", "down":
		m.offset++
	case "k", "up":
		m.offset--
	case "pgdown", " ":
		m.offset += max(m.room()-1, 1)
	case "pgup":
		m.offset -= max(m.room()-1, 1)
	}
	return m.clamp(), nil
}

func (m DetailModel) clamp() DetailModel {
	m.offset = max(min(m.offset, len(m.body())-m.room()), 0)
	return m
}

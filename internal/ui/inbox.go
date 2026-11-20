package ui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

// InboxKind is what an inbox item is, in the order the groups are shown.
type InboxKind int

// Inbox kinds (spec F23).
const (
	InboxDecision InboxKind = iota // an open blocking ticket
	InboxPrompt                    // a session waiting on a permission prompt
	InboxReview                    // a review pending
	InboxAssume                    // an open assume ticket to confirm
)

// InboxItem is one thing that needs the owner: a query result over existing
// rows, never stored. Key names it across refreshes.
type InboxItem struct {
	Kind      InboxKind
	Key       string // ticket id, session id or review id
	SessionID string // the session whose tickets d opens ("" for a ticket of no session)
	TaskID    string // the task whose review opens
	Session   model.Session
	At        time.Time // since when it waits, for the age
	Label     string    // the session or task, untrusted
	Summary   string    // untrusted
}

// InboxSource is where the inbox reads its items, already in display order:
// the groups in InboxKind order, oldest first within one (blocking decisions
// as decision.Triage orders them).
type InboxSource interface {
	Items(ctx context.Context) ([]InboxItem, error)
}

// InboxClosedMsg is sent when Tab or t leaves the inbox; Board asks the
// dashboard to show the board.
type InboxClosedMsg struct{ Board bool }

// InboxOpenMsg asks the dashboard to open the decide or review view for an
// item, which the inbox has just re-read and found still waiting.
type InboxOpenMsg struct{ Item InboxItem }

// InboxNoticeMsg shows a notice on the inbox's status line.
type InboxNoticeMsg struct{ Text string }

// Notices of the inbox.
const (
	noticeInboxGone    = "that item is gone (answered or closed elsewhere); the list is refreshed"
	noticeInboxNotA    = "a answers a decision or confirms an assumption; Enter opens this item"
	noticeInboxLimited = "jump failed: "
)

type inboxReadMsg struct {
	items []InboxItem
	err   error
	seq   int
	tick  bool
}
type (
	inboxTickMsg struct{}
	inboxActMsg  struct {
		item  InboxItem
		items []InboxItem
		err   error
		found bool
	}
)

// InboxModel is the inbox view's Bubble Tea model.
type InboxModel struct {
	src InboxSource
	tm  tmux.Client
	now func() time.Time

	items    []InboxItem // the last good read, filtered by nothing
	loaded   bool
	selected string // key of the selected item, kept across refreshes
	cursor   int    // index into the filtered items

	filter    string
	filtering bool

	since time.Time     // when the selected item, as it is now, came on screen
	wait  time.Duration // how long Enter and a wait from since: dwell, then quietPeriod after a too-soon key

	issued, applied int
	width, height   int
	notice          string
	err             error
	busy            bool // a re-read for an action is in flight
}

// NewInbox returns the inbox over src; tm is used only to jump to a prompt's pane.
func NewInbox(src InboxSource, tm tmux.Client, now func() time.Time) InboxModel {
	return InboxModel{src: src, tm: tm, now: now, since: now(), wait: dwell}
}

// Init starts the first read and with it the one-second chain.
func (m InboxModel) Init() tea.Cmd { return m.read(m.issued, true) }

func (m InboxModel) read(seq int, tick bool) tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		items, err := src.Items(ctx)
		return inboxReadMsg{items: items, err: err, seq: seq, tick: tick}
	}
}

// shown is the items that pass the filter.
func (m InboxModel) shown() []InboxItem {
	if m.filter == "" {
		return m.items
	}
	f := strings.ToLower(m.filter)
	return slices.DeleteFunc(slices.Clone(m.items), func(it InboxItem) bool {
		return !strings.Contains(strings.ToLower(it.Label+" "+it.Summary), f)
	})
}

func (m InboxModel) current() (InboxItem, bool) {
	s := m.shown()
	if m.cursor < 0 || m.cursor >= len(s) {
		return InboxItem{}, false
	}
	return s[m.cursor], true
}

// place selects item i, clamped; the dwell starts again when the selected item changed.
func (m InboxModel) place(i int) InboxModel {
	s := m.shown()
	m.cursor = max(min(i, len(s)-1), 0)
	key := ""
	if len(s) > 0 {
		key = s[m.cursor].Key
	}
	if key != m.selected {
		m.selected, m.since, m.wait = key, m.now(), dwell
	}
	return m
}

// reselect keeps the selection on the same item across a refresh, else on the nearest place.
func (m InboxModel) reselect() InboxModel {
	if i := slices.IndexFunc(m.shown(), func(it InboxItem) bool { return it.Key == m.selected }); i >= 0 {
		m.cursor = i
	}
	return m.place(m.cursor)
}

// Update handles one message.
func (m InboxModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case inboxReadMsg:
		if msg.seq >= m.applied {
			m.applied, m.err = msg.seq, msg.err
			if msg.err == nil {
				m.items, m.loaded = msg.items, true
				m = m.reselect()
			}
		}
		if msg.tick {
			return m, tea.Tick(refreshEvery, func(time.Time) tea.Msg { return inboxTickMsg{} })
		}
		return m, nil
	case inboxTickMsg:
		m.issued++
		return m, m.read(m.issued, true)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case InboxNoticeMsg:
		m.notice = msg.Text
		return m, nil
	case inboxActMsg:
		return m.acted(msg)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m InboxModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.notice = ""
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	if msg.Paste || (msg.Type == tea.KeyRunes && len(msg.Runes) > 1) { // typed text in a burst is never a command
		return m, nil
	}
	if m.filtering {
		return m.filterKey(msg), nil
	}
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "j", "down":
		return m.place(m.cursor + 1), nil
	case "k", "up":
		return m.place(m.cursor - 1), nil
	case "tab":
		return m, func() tea.Msg { return InboxClosedMsg{} }
	case "t":
		return m, func() tea.Msg { return InboxClosedMsg{Board: true} }
	case "/":
		m.filtering = true
		return m, nil
	case "enter":
		return m.act(false)
	case "a":
		return m.act(true)
	}
	return m, nil
}

func (m InboxModel) filterKey(msg tea.KeyMsg) InboxModel {
	switch msg.Type {
	case tea.KeyEnter:
		m.filtering = false
		return m
	case tea.KeyEsc:
		m.filtering, m.filter = false, ""
	case tea.KeyBackspace:
		r := []rune(m.filter)
		m.filter = string(r[:max(len(r)-1, 0)])
	case tea.KeyRunes, tea.KeySpace:
		m.filter += string(msg.Runes)
	case tea.KeyDown:
		return m.place(m.cursor + 1)
	case tea.KeyUp:
		return m.place(m.cursor - 1)
	default:
		return m
	}
	return m.reselect()
}

// act re-reads the items and acts on the selected one only if it is still
// there (answered from the phone meanwhile). Like r on the dashboard it waits
// for the item to have been on screen for the dwell, so a held key never
// opens a view unread. a (accept) is for decisions and assumptions; the view
// it opens has the F7 key and its confirmation.
func (m InboxModel) act(accept bool) (tea.Model, tea.Cmd) {
	it, ok := m.current()
	switch {
	case !ok || m.busy:
		return m, nil
	case accept && it.Kind != InboxDecision && it.Kind != InboxAssume:
		m.notice = noticeInboxNotA
		return m, nil
	}
	if now := m.now(); now.Sub(m.since) < max(m.wait, dwell) {
		m.since, m.wait, m.notice = now, quietPeriod, noticeTooSoon
		return m, nil
	}
	m.busy = true
	src := m.src
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		items, err := src.Items(ctx)
		i := slices.IndexFunc(items, func(x InboxItem) bool { return x.Key == it.Key && x.Kind == it.Kind })
		if err != nil || i < 0 {
			return inboxActMsg{items: items, err: err}
		}
		return inboxActMsg{item: items[i], items: items, found: true}
	}
}

func (m InboxModel) acted(msg inboxActMsg) (tea.Model, tea.Cmd) {
	m.busy = false
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.items = msg.items
	m = m.reselect()
	if !msg.found {
		m.notice = noticeInboxGone
		return m, nil
	}
	if msg.item.Kind != InboxPrompt {
		return m, func() tea.Msg { return InboxOpenMsg{Item: msg.item} }
	}
	// A prompt is only jumped to: Curia does not answer it.
	s, tm := msg.item.Session, m.tm
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxTimeout)
		defer cancel()
		panes, _ := tm.ListPanes(ctx)
		if why := unreachable(s, panes); why != "" {
			return InboxNoticeMsg{Text: noticeInboxLimited + why}
		}
		if err := tm.SwitchTo(ctx, s.Pane); err != nil {
			return InboxNoticeMsg{Text: noticeInboxLimited + err.Error()}
		}
		return tea.Quit()
	}
}

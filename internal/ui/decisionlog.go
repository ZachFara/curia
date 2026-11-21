package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
)

// The dashboard's decision log view (spec F25) and the wording it shares
// with `curia decisions log`: one data path (store.DecisionLog), two
// renderers. The view only reads.

// DecisionLogFilter selects what the view reads.
type DecisionLogFilter struct {
	Text, Scope string
	Since       time.Time // zero: all time
}

// DecisionLogSource reads the log: the newest decisions of f and the summary.
type DecisionLogSource func(ctx context.Context, f DecisionLogFilter) ([]model.DecisionLogRow, model.DecisionLogSummary, error)

// DecisionLogClosedMsg is sent when the decision log view closes.
type DecisionLogClosedMsg struct{}

// DecisionLogStatus is a row's status, with superseded marked.
func DecisionLogStatus(r model.DecisionLogRow) string {
	if r.Superseded {
		return string(r.Status) + " (superseded)"
	}
	return string(r.Status)
}

// DecisionLogReuse is a row's reuse count with the age of the last reuse.
func DecisionLogReuse(r model.DecisionLogRow, now time.Time) string {
	if r.Reuse == 0 {
		return "0"
	}
	return fmt.Sprintf("%d (last %s ago)", r.Reuse, Age(r.LastReuse, now))
}

// DecisionLogSummaryLine is the share of answered decisions reused at least once.
func DecisionLogSummaryLine(s model.DecisionLogSummary) string {
	if s.Answered == 0 {
		return "no answered decisions"
	}
	return strings.TrimSpace(fmt.Sprintf("%d of %d answered decisions (%d%%) were reused at least once",
		s.Reused, s.Answered, s.Reused*100/s.Answered))
}

// decisionLogScopes are what `s` cycles through; "" is every scope.
var decisionLogScopes = []string{"", "task", "repo", "global"}

// decisionLogWindows are what `w` cycles through; 0 is all time.
var decisionLogWindows = []time.Duration{0, 7 * 24 * time.Hour, 30 * 24 * time.Hour}

// DecisionLogModel is the read-only decision log view.
type DecisionLogModel struct {
	src    DecisionLogSource
	now    func() time.Time
	text   string // the search words
	scope  int    // index into decisionLogScopes
	window int    // index into decisionLogWindows

	rows   []model.DecisionLogRow
	sum    model.DecisionLogSummary
	sel    int
	full   bool // the selected decision is shown whole
	at     int  // first line of the full view on screen
	gen    int
	typing bool
	input  string
	err    error
	width  int
	height int
}

// NewDecisionLog returns the decision log view over src.
func NewDecisionLog(src DecisionLogSource, now func() time.Time) DecisionLogModel {
	return DecisionLogModel{src: src, now: now}
}

type decisionLogLoadedMsg struct {
	gen  int
	rows []model.DecisionLogRow
	sum  model.DecisionLogSummary
	err  error
}

// Init reads the log.
func (m DecisionLogModel) Init() tea.Cmd { return m.load() }

func (m DecisionLogModel) load() tea.Cmd {
	f := DecisionLogFilter{Text: m.text, Scope: decisionLogScopes[m.scope]}
	if d := decisionLogWindows[m.window]; d > 0 {
		f.Since = m.now().Add(-d)
	}
	src, gen := m.src, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		rows, sum, err := src(ctx, f)
		return decisionLogLoadedMsg{gen: gen, rows: rows, sum: sum, err: err}
	}
}

// reload reads again, dropping any read in flight.
func (m DecisionLogModel) reload() (DecisionLogModel, tea.Cmd) {
	m.gen++
	m.sel, m.full = 0, false
	return m, m.load()
}

// Update handles one message.
func (m DecisionLogModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case decisionLogLoadedMsg:
		if msg.gen == m.gen {
			m.rows, m.sum, m.err = msg.rows, msg.sum, msg.err
			m.sel = min(m.sel, max(len(m.rows)-1, 0))
		}
	case tea.KeyMsg:
		if m.typing {
			return m.typed(msg)
		}
		if m.full {
			return m.fullKey(msg)
		}
		return m.key(msg)
	}
	return m, nil
}

func (m DecisionLogModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc":
		return m, func() tea.Msg { return DecisionLogClosedMsg{} }
	case "j", "down":
		m.sel = min(m.sel+1, max(len(m.rows)-1, 0))
	case "k", "up":
		m.sel = max(m.sel-1, 0)
	case "enter":
		m.full, m.at = len(m.rows) > 0, 0
	case "s":
		m.scope = (m.scope + 1) % len(decisionLogScopes)
		return m.reload()
	case "w":
		m.window = (m.window + 1) % len(decisionLogWindows)
		return m.reload()
	case "r":
		return m.reload()
	case "/":
		m.typing, m.input = true, ""
	}
	return m, nil
}

func (m DecisionLogModel) fullKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc", "enter":
		m.full = false
	case "j", "down":
		m.at++
	case "k", "up":
		m.at = max(m.at-1, 0)
	}
	return m, nil
}

// typed handles a key while the search words are typed; empty clears them.
func (m DecisionLogModel) typed(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.typing = false
	case tea.KeyEnter:
		m.typing, m.text = false, m.input
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

// WithDecisionLog returns m with the L key enabled in the popup: L shows the
// model open builds until it sends DecisionLogClosedMsg.
func (m Model) WithDecisionLog(open func() tea.Model) Model {
	return m.withView("decisionlog", func(string) tea.Model { return open() })
}

// openDecisionLog shows the decision log view, or says it opens in the popup.
func (m Model) openDecisionLog() (Model, tea.Cmd) {
	open := m.views["decisionlog"]
	if open == nil {
		m.notice = "the decision log opens in the popup (curia ui)"
		return m, nil
	}
	return m.showChild(open(""))
}

func (DecisionLogClosedMsg) closedChild() {}

func init() {
	registerCommand(command{keys: []string{"L"}, help: "L log", order: 160, run: Model.openDecisionLog})
}

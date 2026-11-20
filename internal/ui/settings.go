package ui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// settingsTimeout bounds one read or write of the settings.
const settingsTimeout = 5 * time.Second

// SettingRow is one config key as `curia config list` shows it.
type SettingRow struct {
	Key, Value string
	Source     string // default, user, repo or flag
	Refused    string // a repository value that was ignored, and why; "" when none
	Editable   bool   // a scalar the dashboard may change
}

// SettingsSource is the settings view's data and its one write: the same
// rows `config list` prints, and the function `config set` uses.
type SettingsSource interface {
	Rows(ctx context.Context) ([]SettingRow, error)
	Set(ctx context.Context, key, value string) (string, error) // the line to show
}

// SettingsClosedMsg is sent when the settings view closes.
type SettingsClosedMsg struct{}

// SettingsModel is the settings view (spec F28): every key with its value
// and layer; with no readOnly reason, `e` changes a scalar after a
// confirmation.
type SettingsModel struct {
	src      SettingsSource
	now      func() time.Time
	readOnly string // why editing is unavailable; "": it is available

	rows   []SettingRow
	cursor int
	notice string
	err    error

	typing   bool // typing the new value
	input    string
	asking   bool // the value is typed; waiting for y
	askSince time.Time
	askWait  time.Duration
	keyAt    time.Time
	prevKey  time.Time
	confirm  time.Time // when a y was taken, waiting out burstGap
	seq      int
	busy     bool

	width, height int
}

// NewSettings returns the settings view over src; readOnly is the reason
// editing is unavailable (the user guard refused), or "".
func NewSettings(src SettingsSource, now func() time.Time, readOnly string) SettingsModel {
	return SettingsModel{src: src, now: now, readOnly: readOnly}
}

type settingsLoadedMsg struct {
	rows []SettingRow
	err  error
}

type settingsSetMsg struct {
	line string
	err  error
}

type settingsConfirmMsg struct{ seq int }

// Init reads the rows.
func (m SettingsModel) Init() tea.Cmd { return m.load() }

func (m SettingsModel) load() tea.Cmd {
	src := m.src
	return call(settingsTimeout, func(ctx context.Context) tea.Msg {
		rows, err := src.Rows(ctx)
		return settingsLoadedMsg{rows: rows, err: err}
	})
}

func (m SettingsModel) row() (SettingRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return SettingRow{}, false
	}
	return m.rows[m.cursor], true
}

// Update handles one message.
func (m SettingsModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case settingsLoadedMsg:
		m.err = msg.err
		if msg.err == nil {
			first := m.rows == nil
			m.rows = msg.rows
			m.cursor = min(m.cursor, max(len(m.rows)-1, 0))
			if first { // start on something that can be changed
				for i, r := range m.rows {
					if r.Editable {
						m.cursor = i
						break
					}
				}
			}
		}
	case settingsSetMsg:
		m.busy, m.notice = false, msg.line
		if msg.err != nil {
			m.notice = "not changed: " + cleanLine(msg.err.Error(), 160)
		}
		return m, m.load()
	case settingsConfirmMsg:
		if msg.seq == m.seq && !m.confirm.IsZero() {
			return m.startSet()
		}
	case tea.KeyMsg:
		m.prevKey, m.keyAt = m.keyAt, m.now()
		return m.key(msg)
	}
	return m, nil
}

func (m SettingsModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case !m.confirm.IsZero():
		return m.keyAfterConfirm()
	case m.asking:
		return m.askKey(msg)
	case m.typing:
		return m.typed(msg), nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q", "esc":
		return m, func() tea.Msg { return SettingsClosedMsg{} }
	case "j", "down":
		m.cursor = min(m.cursor+1, max(len(m.rows)-1, 0))
	case "k", "up":
		m.cursor = max(m.cursor-1, 0)
	case "r":
		return m, m.load()
	case "e":
		return m.edit(), nil
	}
	return m, nil
}

// edit opens the value prompt for the selected key, or says why not.
func (m SettingsModel) edit() SettingsModel {
	r, ok := m.row()
	switch {
	case !ok:
	case m.readOnly != "":
		m.notice = m.readOnly
	case m.busy:
		m.notice = "still saving"
	case !r.Editable:
		m.notice = cleanLine(r.Key, 60) + " is not changed from here: use curia config set or edit"
	default:
		m.typing, m.input, m.notice = true, "", ""
	}
	return m
}

// typed handles a key while the new value is typed.
func (m SettingsModel) typed(msg tea.KeyMsg) SettingsModel {
	switch msg.Type {
	case tea.KeyEsc:
		m.typing = false
	case tea.KeyEnter:
		if m.input == "" {
			m.typing = false
			break
		}
		m.typing, m.asking, m.askSince, m.askWait = false, true, m.now(), dwell
	case tea.KeyBackspace:
		if r := []rune(m.input); len(r) > 0 {
			m.input = string(r[:len(r)-1])
		}
	case tea.KeyRunes, tea.KeySpace:
		if len(m.input) < 200 {
			m.input += string(msg.Runes)
		}
	}
	return m
}

// askKey takes the y of the confirmation: the wait since the prompt opened
// and the burst rule (a y within burstGap of another key) refuse it, and any
// other key or a paste cancels. A good y waits out burstGap (keyAfterConfirm).
func (m SettingsModel) askKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Paste || msg.String() != "y" {
		m.asking, m.notice = false, "canceled"
		return m, nil
	}
	if now := m.now(); now.Sub(m.askSince) < m.askWait || inBurst(now, m.prevKey) {
		m.askSince, m.askWait, m.notice = now, quietPeriod, noticeTooSoon
		return m, nil
	}
	m.confirm, m.seq = m.now(), m.seq+1
	seq := m.seq
	return m, tea.Tick(burstGap, func(time.Time) tea.Msg { return settingsConfirmMsg{seq: seq} })
}

// keyAfterConfirm: a key within burstGap of the y arrived with it, so the y
// is refused; a later one means the wait's message is late, and the change goes.
func (m SettingsModel) keyAfterConfirm() (tea.Model, tea.Cmd) {
	if inBurst(m.keyAt, m.confirm) {
		m.confirm = time.Time{}
		m.askSince, m.askWait, m.notice = m.keyAt, quietPeriod, noticeTooSoon
		return m, nil
	}
	return m.startSet()
}

func (m SettingsModel) startSet() (tea.Model, tea.Cmd) {
	r, ok := m.row()
	if !ok {
		return m, nil
	}
	m.confirm, m.asking, m.busy = time.Time{}, false, true
	src, key, value := m.src, r.Key, m.input
	return m, call(settingsTimeout, func(ctx context.Context) tea.Msg {
		line, err := src.Set(ctx, key, value)
		return settingsSetMsg{line: line, err: err}
	})
}

// cleanLine is a bounded single line of untrusted text.
func cleanLine(s string, n int) string {
	s = strings.TrimSpace(clean(s))
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

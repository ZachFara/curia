package ui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

type fakeInbox struct {
	mu    sync.Mutex
	items []InboxItem
	err   error
}

func (f *fakeInbox) Items(context.Context) ([]InboxItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]InboxItem(nil), f.items...), f.err
}

func (f *fakeInbox) set(items ...InboxItem) {
	f.mu.Lock()
	f.items = items
	f.mu.Unlock()
}

func inboxItems() []InboxItem {
	return []InboxItem{
		{Kind: InboxDecision, Key: "D-1", SessionID: "S1", Label: "T-1", Summary: "Which store?", At: testNow.Add(-time.Hour)},
		{
			Kind: InboxPrompt, Key: "S2", SessionID: "S2", Label: "S2", Summary: "Allow Bash?", At: testNow.Add(-time.Minute),
			Session: model.Session{ID: "S2", Pane: "%3", State: model.StateWaiting},
		},
		{Kind: InboxReview, Key: "R-1", TaskID: "T-2", Label: "T-2", Summary: "Done.", At: testNow.Add(-2 * time.Hour)},
		{Kind: InboxAssume, Key: "D-2", SessionID: "S1", Label: "T-1", Summary: "Redis", At: testNow.Add(-3 * time.Hour)},
	}
}

// settle reads the inbox's first refresh.
func settledInbox(t *testing.T, src InboxSource, clk *time.Time) tea.Model {
	t.Helper()
	var m tea.Model = NewInbox(src, &tmux.Fake{Panes: []tmux.Pane{{ID: "%3", Session: "work", WindowID: "@1"}}}, func() time.Time { return *clk })
	m, _ = m.Update(m.Init()())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	return m
}

func TestInboxShowsGroupsInOrderAndSaysWhenEmpty(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(inboxItems()...)
	v := settledInbox(t, src, &clk).View()
	last := -1
	for _, h := range []string{"DECISIONS (1)", "PERMISSION PROMPTS (1)", "REVIEWS (1)", "ASSUMPTIONS TO CONFIRM (1)"} {
		i := strings.Index(v, h)
		if i <= last {
			t.Fatalf("heading %q out of order or missing:\n%s", h, v)
		}
		last = i
	}
	src.set()
	if v := settledInbox(t, src, &clk).View(); !strings.Contains(v, inboxEmpty) {
		t.Errorf("empty inbox view:\n%s", v)
	}
}

func TestInboxCleansAndFitsAgentText(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(InboxItem{
		Kind: InboxPrompt, Key: "S1", Label: "\x1b[31mred\x1b[0m 日本語日本語日本語日本語", At: testNow,
		Summary: "\x1b]0;title\x07ask 😀😀😀 " + strings.Repeat("日本", 200) + "\x1b[2J",
	})
	v := settledInbox(t, src, &clk).View()
	if strings.ContainsRune(v, 0x1b) || strings.Contains(v, "title") {
		t.Errorf("escape sequence reached the screen: %q", v)
	}
	for _, l := range strings.Split(v, "\n") {
		if w := lipgloss.Width(l); w > 80 {
			t.Errorf("line is %d cells wide: %q", w, l)
		}
	}
}

func TestInboxEnterRereadsAndSaysWhenGone(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(inboxItems()...)
	m := settledInbox(t, src, &clk)
	clk = clk.Add(time.Second)   // past the dwell
	src.set(inboxItems()[1:]...) // D-1 answered from the phone
	m, cmd := press(m, "enter")
	if cmd == nil {
		t.Fatal("enter did nothing")
	}
	m, cmd = m.Update(cmd())
	if cmd != nil {
		t.Errorf("a gone item opened something: %T", cmd())
	}
	v := m.View()
	if !strings.Contains(v, noticeInboxGone) || strings.Contains(v, "Which store?") {
		t.Errorf("view after a gone item:\n%s", v)
	}
}

func TestInboxEnterInsideDwellDoesNothing(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(inboxItems()...)
	m := settledInbox(t, src, &clk)
	if _, cmd := press(m, "enter"); cmd != nil {
		t.Error("enter inside the dwell acted")
	}
}

// The dashboard opens on the inbox, Enter on a decision opens the decide
// view, Tab shows the session list and Tab again the inbox.
func TestDashboardOpensOnInboxAndRoutesKeys(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(inboxItems()...)
	tm := &tmux.Fake{Panes: []tmux.Pane{{ID: "%3", Session: "work", WindowID: "@1"}}}
	opened := ""
	now := func() time.Time { return clk }
	fs := &fakeSource{sessions: threeSessions()}
	d := New(fs, tm, config.Default(), now, ModePopup).
		WithInbox(func() tea.Model { return NewInbox(src, tm, now) }).
		WithDecide(func(id string) tea.Model { opened = id; return stubChild{view: "DECIDE " + id} })

	var m tea.Model = d
	m, _ = m.Update(m.Init()())                  // openInboxMsg: shows the inbox
	m, _ = m.Update(dashboard(m).child.Init()()) // the inbox's own first read
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	if !strings.Contains(m.View(), "DECISIONS") {
		t.Fatalf("not on the inbox:\n%s", m.View())
	}
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = drain(m, cmd)
	if v := m.View(); strings.Contains(v, "DECISIONS") || !strings.Contains(v, "SESSION:WINDOW") {
		t.Fatalf("tab did not show the session list:\n%s", v)
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = drain(m, cmd)
	if !strings.Contains(m.View(), "DECISIONS") {
		t.Fatalf("tab did not return to the inbox:\n%s", m.View())
	}
	clk = clk.Add(time.Second)
	m, cmd = press(m, "enter")
	m = drain(m, cmd)
	if opened != "S1" || !strings.Contains(m.View(), "DECIDE S1") {
		t.Fatalf("enter on a decision opened %q:\n%s", opened, m.View())
	}
	// Closing the decide view returns to the inbox.
	m, cmd = m.Update(childMsg{gen: dashboard(m).childGen, msg: DecideClosedMsg{}})
	m = drain(m, cmd)
	if !strings.Contains(m.View(), "DECISIONS") {
		t.Errorf("closing the decide view did not return to the inbox:\n%s", m.View())
	}
}

// Read-only: without a decide factory Enter says why and opens nothing; a
// prompt's Enter jumps and quits.
func TestDashboardInboxReadOnlyAndPromptJump(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(inboxItems()...)
	tm := &tmux.Fake{Panes: []tmux.Pane{{ID: "%3", Session: "work", WindowID: "@1"}}}
	now := func() time.Time { return clk }
	d := New(&fakeSource{}, tm, config.Default(), now, ModePopup).
		WithInbox(func() tea.Model { return NewInbox(src, tm, now) }).
		WithoutDecide("read-only here")
	var m tea.Model = d
	m, _ = m.Update(m.Init()())
	m, _ = m.Update(dashboard(m).child.Init()())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	clk = clk.Add(time.Second)
	m, cmd := press(m, "enter")
	m = drain(m, cmd)
	if !strings.Contains(m.View(), "read-only here") {
		t.Errorf("read-only enter said nothing:\n%s", m.View())
	}
	m, _ = press(m, "j")
	clk = clk.Add(time.Second)
	_, cmd = press(m, "enter")
	for i := 0; i < 4 && cmd != nil; i++ { // re-read, then the jump
		msg := cmd()
		if _, ok := msg.(childMsg); ok {
			m, cmd = m.Update(msg)
			continue
		}
		if _, ok := msg.(tea.QuitMsg); ok {
			break
		}
		cmd = nil
	}
	if len(calls(tm, "SwitchTo")) != 1 {
		t.Errorf("calls = %q, want one SwitchTo", tm.Calls)
	}
}

func TestInboxReadErrorKeepsLastGoodList(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(inboxItems()...)
	m := settledInbox(t, src, &clk)
	src.err = errors.New("db locked")
	m, _ = m.Update(inboxReadMsg{err: src.err, seq: 5})
	if v := m.View(); !strings.Contains(v, "Which store?") || !strings.Contains(v, "read failed") {
		t.Errorf("view after a failed read:\n%s", v)
	}
}

// stubChild is a view that shows a fixed line.
type stubChild struct{ view string }

func (stubChild) Init() tea.Cmd                         { return nil }
func (s stubChild) Update(tea.Msg) (tea.Model, tea.Cmd) { return s, nil }
func (s stubChild) View() string                        { return s.view }

func isRead(msg tea.Msg) bool {
	if c, ok := msg.(childMsg); ok {
		msg = c.msg
	}
	switch msg.(type) {
	case refreshMsg, inboxReadMsg:
		return true
	}
	return false
}

// drain feeds cmd's messages (and those of what they start, once) back to m.
func drain(m tea.Model, cmd tea.Cmd) tea.Model {
	for range 6 {
		if cmd == nil {
			return m
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); ok {
			return m
		}
		var next tea.Cmd
		m, next = m.Update(msg)
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				m = drain(m, c)
			}
			return m
		}
		cmd = next
		if isRead(msg) { // its next command is a one-second tick: not waited for
			return m
		}
	}
	return m
}

// dashboard is m as the dashboard Model; a test that gets another type fails by panic.
func dashboard(m tea.Model) Model {
	d, ok := m.(Model)
	if !ok {
		panic("not the dashboard model")
	}
	return d
}

// Detail opens from the session list reached through Tab, and closing it
// returns to that list, not the inbox.
func TestDetailFromTheListReachedByTabClosesToTheList(t *testing.T) {
	clk := testNow
	src := &fakeInbox{}
	src.set(inboxItems()...)
	tm := &tmux.Fake{}
	now := func() time.Time { return clk }
	var m tea.Model = New(&fakeSource{sessions: threeSessions()}, tm, config.Default(), now, ModePopup).
		WithInbox(func() tea.Model { return NewInbox(src, tm, now) }).
		WithDetail(func(id string) tea.Model { return stubChild{view: "DETAIL " + id} })
	m, _ = m.Update(m.Init()())
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = drain(m, cmd)
	m, cmd = press(m, "i")
	m = drain(m, cmd)
	if !strings.Contains(m.View(), "DETAIL a") {
		t.Fatalf("i did not open the detail view:\n%s", m.View())
	}
	m, cmd = m.Update(childMsg{gen: dashboard(m).childGen, msg: DetailClosedMsg{}})
	m = drain(m, cmd)
	if v := m.View(); strings.Contains(v, "DECISIONS") || !strings.Contains(v, "SESSION:WINDOW") {
		t.Errorf("closing the detail view did not return to the session list:\n%s", v)
	}
}

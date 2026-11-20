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

	"github.com/ZachFara/curia/internal/model"
)

type fakeDetail struct {
	mu    sync.Mutex
	data  DetailData
	diff  DetailDiff
	err   error
	reads int
	diffs int
}

func (f *fakeDetail) Detail(context.Context, string) (DetailData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.data, f.err
}

func (f *fakeDetail) Diff(context.Context, string) (DetailDiff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffs++
	return f.diff, nil
}

// detailFeed runs cmd, expanding batches, and feeds each message but the
// closing ones back to m; it returns the model and the closing messages.
func detailFeed(m tea.Model, cmd tea.Cmd) tea.Model {
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			for _, b := range msg {
				run(b)
			}
		case DetailClosedMsg, DetailJumpMsg:
		default:
			var next tea.Cmd
			m, next = m.Update(msg)
			run(next)
		}
	}
	run(cmd)
	return m
}

func newDetailTest(f *fakeDetail, w int) tea.Model {
	m := NewDetail(f, "s1", func() time.Time { return testNow })
	m.tick = func(time.Duration, tea.Msg) tea.Cmd { return nil }
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: w, Height: 40})
	tm = detailFeed(tm, tm.Init())
	return tm
}

func TestDetailPanelsMatchTheData(t *testing.T) {
	f := &fakeDetail{
		data: DetailData{
			Session:     model.Session{ID: "s1", State: model.StateIdle, Repo: "api", CostUSD: 1.5, CostKnown: true},
			Tail:        []string{"hello", "tool: Bash"},
			Tickets:     []model.Decision{{ID: "D-1", Status: model.StatusAnswered, Question: "q?", Answer: "yes"}},
			Messages:    []model.Message{{ID: "M-1", Status: model.MsgDelivered, DeliveredVia: model.ViaSendKeys, Text: "do x"}},
			TaskCostUSD: 3, TaskCostKnown: true,
		},
		diff: DetailDiff{Has: true, Stat: " a.go | 2 +-"},
	}
	v := newDetailTest(f, 80).View()
	for _, want := range []string{
		"Transcript", "hello", "tool: Bash", "D-1 answered q? → yes", "M-1 delivered, unconfirmed do x",
		"Diff", "a.go | 2 +-", "session $1.50", "task branch $3.00", "enter pane",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	if strings.Contains(v, "Activity") {
		t.Error("Activity shown without a source")
	}
}

func TestDetailNoDiffPanelWithoutWorktreeAndActivityOnlyWithData(t *testing.T) {
	f := &fakeDetail{data: DetailData{Session: model.Session{ID: "s1", State: model.StateEnded, EndReason: "other"}, ActivityOK: true, Activity: []string{"working 5m"}}}
	v := newDetailTest(f, 80).View()
	if strings.Contains(v, "Diff") {
		t.Error("diff panel for a session without a worktree")
	}
	if !strings.Contains(v, "ended (other)") || !strings.Contains(v, "working 5m") {
		t.Errorf("ended session or activity missing:\n%s", v)
	}
	if !strings.Contains(v, "no transcript lines") {
		t.Errorf("no empty-tail text:\n%s", v)
	}
}

func TestDetailControlSequencesNeverReachTheScreen(t *testing.T) {
	f := &fakeDetail{data: DetailData{
		Session: model.Session{ID: "s1"}, Tail: []string{"red \x1b[31mtext\x1b]0;title\x07 end"},
		Tickets: []model.Decision{{ID: "D-1", Question: "q \x1b[2Jx"}},
	}}
	v := newDetailTest(f, 80).View()
	if strings.Contains(v, "\x1b[31m") || strings.Contains(v, "\x1b]") || strings.Contains(v, "\x1b[2J") || strings.Contains(v, "\x07") {
		t.Errorf("escape sequence on screen: %q", v)
	}
}

func TestDetailLayoutAt80ColumnsWithWideText(t *testing.T) {
	wide := strings.Repeat("漢字🙂", 60)
	f := &fakeDetail{
		data: DetailData{
			Session: model.Session{ID: "s1", Repo: wide, Branch: wide}, Tail: []string{wide, strings.Repeat("x", 5000)},
			Tickets: []model.Decision{{ID: "D-1", Question: wide}}, Messages: []model.Message{{ID: "M-1", Text: wide}},
		},
		diff: DetailDiff{Has: true, Stat: " " + wide + " | 1 +", Note: wide},
	}
	for _, w := range []int{80, 40} {
		v := newDetailTest(f, w).View()
		lines := strings.Split(v, "\n")
		if len(lines) != 40 {
			t.Errorf("width %d: %d lines, want 40", w, len(lines))
		}
		for _, l := range lines {
			if lipgloss.Width(l) > w {
				t.Errorf("width %d: line of %d cells: %q", w, lipgloss.Width(l), l)
			}
		}
	}
}

func TestDetailRefreshShowsAChangingTranscriptAndAllowsOneReadInFlight(t *testing.T) {
	f := &fakeDetail{data: DetailData{Session: model.Session{ID: "s1"}, Tail: []string{"first"}}}
	m := newDetailTest(f, 80)
	m, cmd := m.Update(detailTickMsg{})
	f.mu.Lock()
	f.data.Tail = []string{"second"}
	f.mu.Unlock()
	if _, cmd2 := m.Update(detailTickMsg{}); cmd2 != nil { // a read is in flight: no second one
		t.Fatal("a second read started while one was in flight")
	}
	m = detailFeed(m, cmd)
	if v := m.View(); !strings.Contains(v, "second") || strings.Contains(v, "first") {
		t.Errorf("tail not refreshed:\n%s", v)
	}
	f.mu.Lock()
	f.err = errors.New("boom")
	f.mu.Unlock()
	m, cmd = m.Update(detailTickMsg{})
	m = detailFeed(m, cmd)
	if v := m.View(); !strings.Contains(v, "read failed: boom") || !strings.Contains(v, "second") {
		t.Errorf("a failed read must keep the last good data and say so:\n%s", v)
	}
}

func TestDetailKeysCloseJumpAndScroll(t *testing.T) {
	tail := make([]string, 100)
	for i := range tail {
		tail[i] = "line"
	}
	f := &fakeDetail{data: DetailData{Session: model.Session{ID: "s1"}, Tail: tail}}
	m := newDetailTest(f, 80)
	before := m.View()
	m, _ = m.Update(key("j"))
	if m.View() == before {
		t.Error("j did not scroll")
	}
	for k, want := range map[string]tea.Msg{"esc": DetailClosedMsg{}, "q": DetailClosedMsg{}, "enter": DetailJumpMsg{}} {
		_, cmd := m.Update(key(k))
		if cmd == nil || cmd() != want {
			t.Errorf("%s: want %T", k, want)
		}
	}
	if _, cmd := m.Update(key("x")); cmd != nil {
		t.Error("an unknown key did something")
	}
}

// The dashboard opens the view on i for the selected session and, on Enter in it, jumps to the pane.
func TestDashboardOpensDetailAndEnterJumps(t *testing.T) {
	m0, fake, _ := newTest(t, ModePopup, threeSessions()...)
	f := &fakeDetail{data: DetailData{Session: model.Session{ID: "a"}}}
	var asked string
	m := detailAsModel(t, m0).WithDetail(func(id string) tea.Model {
		asked = id
		d := NewDetail(f, id, func() time.Time { return testNow })
		d.tick = func(time.Duration, tea.Msg) tea.Cmd { return nil }
		return d
	})
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	tm, cmd := tm.Update(key("i"))
	if asked != "a" || detailAsModel(t, tm).child == nil {
		t.Fatalf("i did not open the view for the selected session: %q", asked)
	}
	_ = cmd
	// Esc inside closes it and the table is back.
	tm, _ = tm.Update(childMsg{gen: detailAsModel(t, tm).childGen, msg: DetailClosedMsg{}})
	if detailAsModel(t, tm).child != nil {
		t.Fatal("DetailClosedMsg did not close the view")
	}
	tm, _ = tm.Update(key("i"))
	tm, cmd = tm.Update(childMsg{gen: detailAsModel(t, tm).childGen, msg: DetailJumpMsg{}})
	if detailAsModel(t, tm).child != nil || len(calls(fake, "SwitchTo")) != 1 {
		t.Fatalf("Enter in the detail did not jump: child %v, calls %v", detailAsModel(t, tm).child, fake.Calls)
	}
	if cmd == nil {
		t.Fatal("no command after the jump")
	}
}

func detailAsModel(t *testing.T, m tea.Model) Model {
	t.Helper()
	d, ok := m.(Model)
	if !ok {
		t.Fatalf("%T is not the dashboard", m)
	}
	return d
}

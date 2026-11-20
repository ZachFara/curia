package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
)

var timelineT0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// timelineFake serves n events, one minute apart, newest first, and records its calls.
type timelineFake struct {
	n     int
	calls []TimelineFilter
	befor []time.Time
}

func (f *timelineFake) src(_ context.Context, fl TimelineFilter, before time.Time, limit int) ([]model.TimelineEvent, error) {
	f.calls, f.befor = append(f.calls, fl), append(f.befor, before)
	var out []model.TimelineEvent
	for i := range f.n {
		at := timelineT0.Add(-time.Duration(i) * time.Minute)
		if !before.IsZero() && !at.Before(before) {
			continue
		}
		out = append(out, model.TimelineEvent{At: at, SessionID: "abcdefghij", Kind: "turn_end", Line: fmt.Sprintf("line %d 日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語\x1b[31mred", i)})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// pump runs cmd and feeds its message back, so no test waits on a timer.
func timelinePump(m tea.Model, cmd tea.Cmd) tea.Model {
	for cmd != nil {
		msg := cmd()
		m, cmd = m.Update(msg)
	}
	return m
}

func timelineOpen(t *testing.T, f *timelineFake) tea.Model {
	t.Helper()
	m := NewTimeline(f.src, func() time.Time { return timelineT0 })
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	return timelinePump(tm, m.Init())
}

func TestTimelineViewFitsEightyColumnsAndIsClean(t *testing.T) {
	m := timelineOpen(t, &timelineFake{n: 5})
	out := m.View()
	if strings.Contains(out, "\x1b") {
		t.Errorf("escape sequence in view: %q", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 12 {
		t.Errorf("%d lines, want at most 12", len(lines))
	}
	for _, l := range lines {
		if lipgloss.Width(l) > 80 {
			t.Errorf("line is %d columns: %q", lipgloss.Width(l), l)
		}
	}
	if !strings.Contains(out, "turn_end") || !strings.Contains(out, "abcdefgh") {
		t.Errorf("view:\n%s", out)
	}
}

func TestTimelinePagesOlderAndFilters(t *testing.T) {
	f := &timelineFake{n: 150}
	m := timelineOpen(t, f)
	if got := timelineLen(m); got != timelinePage {
		t.Fatalf("first page = %d events", got)
	}
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	m = timelinePump(m, cmd)
	if got := timelineLen(m); got != 150 {
		t.Errorf("after n: %d events, want 150", got)
	}
	if want := timelineT0.Add(-99 * time.Minute); !f.befor[1].Equal(want) {
		t.Errorf("older page asked before %v, want %v", f.befor[1], want)
	}
	// / T-7 enter reloads from the top with the task filter.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	for _, k := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("T-7")}, {Type: tea.KeyEnter}} {
		var c tea.Cmd
		m, c = m.Update(k)
		m = timelinePump(m, c)
	}
	if last := f.calls[len(f.calls)-1]; last.TaskID != "T-7" || !f.befor[len(f.befor)-1].IsZero() {
		t.Errorf("last read = %+v before %v", last, f.befor[len(f.befor)-1])
	}
	// w widens the window.
	m, c := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("w")})
	timelinePump(m, c)
	if got := f.calls[len(f.calls)-1].Since; !got.Equal(timelineT0.Add(-6 * time.Hour)) {
		t.Errorf("window since = %v", got)
	}
}

func TestTimelineClosesWithQ(t *testing.T) {
	m := timelineOpen(t, &timelineFake{n: 1})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if _, ok := cmd().(TimelineClosedMsg); !ok {
		t.Error("q did not close the view")
	}
}

func TestTKeyOpensTimelineAndClosesBack(t *testing.T) {
	m, _, _ := newTest(t, ModePopup, threeSessions()...)
	f := &timelineFake{n: 3}
	m = timelineWith(t, m, func() tea.Model { return NewTimeline(f.src, func() time.Time { return timelineT0 }) })
	m, first := press(m, "T")
	if !strings.HasPrefix(m.View(), "TIMELINE") {
		t.Fatalf("T did not show the timeline:\n%s", m.View())
	}
	m = feed(m, first)
	if !strings.Contains(m.View(), "turn_end") {
		t.Errorf("timeline shows no events:\n%s", m.View())
	}
	m, closeCmd := press(m, "q")
	m, _ = m.Update(closeCmd())
	if !strings.Contains(m.View(), "claude-opus-5-5") {
		t.Errorf("view after close is not the table:\n%s", m.View())
	}
}

func timelineLen(m tea.Model) int {
	tm, ok := m.(TimelineModel)
	if !ok {
		return -1
	}
	return len(tm.evs)
}

func timelineWith(t *testing.T, m tea.Model, open func() tea.Model) tea.Model {
	t.Helper()
	dm, ok := m.(Model)
	if !ok {
		t.Fatalf("model is a %T, want Model", m)
	}
	return dm.WithTimeline(open)
}

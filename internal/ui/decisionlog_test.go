package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
)

type decisionLogFake struct{ calls []DecisionLogFilter }

func (f *decisionLogFake) src(_ context.Context, fl DecisionLogFilter) ([]model.DecisionLogRow, model.DecisionLogSummary, error) {
	f.calls = append(f.calls, fl)
	rows := []model.DecisionLogRow{
		{
			ID: "D-2", Status: model.StatusAnswered, Scope: "repo", Repo: "/r/a", CreatedAt: timelineT0.Add(-time.Hour), AnsweredAt: timelineT0.Add(-30 * time.Minute),
			Reuse: 2, LastReuse: timelineT0.Add(-10 * time.Minute), AskedAgain: 1,
			Question: "Which store 日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語?\x1b[31m", Answer: strings.Repeat("Use Redis. ", 100),
		},
		{ID: "D-1", Status: model.StatusAnswered, Scope: "task", Superseded: true, CreatedAt: timelineT0.Add(-2 * time.Hour), Question: "Old question", Answer: "A"},
	}
	return rows, model.DecisionLogSummary{Answered: 2, Reused: 1}, nil
}

func decisionLogOpen(f *decisionLogFake) tea.Model {
	m := NewDecisionLog(f.src, func() time.Time { return timelineT0 })
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 80, Height: 14})
	return timelinePump(tm, m.Init())
}

func TestDecisionLogListFitsAndMarksSuperseded(t *testing.T) {
	out := decisionLogOpen(&decisionLogFake{}).View()
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "1 of 2 answered decisions (50%)") || !strings.Contains(out, "superseded") || !strings.Contains(out, "r2 a1") {
		t.Errorf("list:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 14 {
		t.Errorf("%d lines", len(lines))
	}
	for _, l := range lines {
		if lipgloss.Width(l) > 80 {
			t.Errorf("line is %d columns: %q", lipgloss.Width(l), l)
		}
	}
}

func TestDecisionLogEnterShowsInFullAndFilters(t *testing.T) {
	f := &decisionLogFake{}
	m := decisionLogOpen(f)
	m, _ = timelineKey(m, "j")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if out := m.View(); !strings.Contains(out, "DECISION D-1") || !strings.Contains(out, "superseded") || !strings.Contains(out, "Old question") {
		t.Errorf("full view of the second row:\n%s", out)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, l := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(l) > 80 {
			t.Errorf("full line is %d columns: %q", lipgloss.Width(l), l)
		}
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = timelineKey(m, "/")
	for _, k := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("redis")}, {Type: tea.KeyEnter}} {
		var c tea.Cmd
		m, c = m.Update(k)
		m = timelinePump(m, c)
	}
	m, c := timelineKey(m, "s")
	timelinePump(m, c)
	if last := f.calls[len(f.calls)-1]; last.Text != "redis" || last.Scope != "task" {
		t.Errorf("last read = %+v", last)
	}
}

func TestLKeyOpensDecisionLogAndQClosesIt(t *testing.T) {
	nm, _, _ := newTest(t, ModePopup, threeSessions()...)
	f := &decisionLogFake{}
	m := tea.Model(dashboard(nm).WithDecisionLog(func() tea.Model { return NewDecisionLog(f.src, func() time.Time { return timelineT0 }) }))
	m, first := press(m, "L")
	m = feed(m, first)
	if !strings.HasPrefix(m.View(), "DECISIONS") {
		t.Fatalf("L did not open the log:\n%s", m.View())
	}
	m, closeCmd := press(m, "q")
	m, _ = m.Update(closeCmd())
	if !strings.Contains(m.View(), "claude-opus-5-5") {
		t.Errorf("view after close is not the table:\n%s", m.View())
	}
}

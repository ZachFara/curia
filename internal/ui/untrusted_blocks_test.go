package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
)

// D-6: a continuation line of untrusted multi-line text is indented, so a
// line of agent text cannot read as a section heading or a row.
const fakeRow = "FAKE-ROW T-99 running"

func noLineStarts(t *testing.T, view, what string) {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if strings.HasPrefix(l, fakeRow) {
			t.Errorf("%s: a line of untrusted text starts at column 0: %q\n%s", what, l, view)
		}
	}
	if !strings.Contains(view, fakeRow) {
		t.Errorf("%s: the text is missing:\n%s", what, view)
	}
}

func TestDetailDiffStatLinesAreIndented(t *testing.T) {
	f := &fakeDetail{
		data: DetailData{Session: model.Session{ID: "s1", State: model.StateIdle}},
		diff: DetailDiff{Has: true, Stat: " a.go | 2 +-\n" + fakeRow},
	}
	noLineStarts(t, newDetailTest(f, 80).View(), "detail diff stat")
}

func TestTimelineFullBodyLinesAreIndented(t *testing.T) {
	f := &timelineFake{n: 3}
	m := NewTimeline(f.src, func() time.Time { return timelineT0 }).WithText(func(context.Context, model.TimelineEvent) (string, error) {
		return "first line\n" + fakeRow, nil
	})
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	tm = timelinePump(tm, m.Init())
	tm, _ = timelineKey(tm, "j")
	tm, cmd := tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	tm = timelinePump(tm, cmd)
	noLineStarts(t, tm.View(), "timeline full body")
}

func TestDecisionLogFullLinesAreIndented(t *testing.T) {
	f := &decisionLogFake{}
	m := NewDecisionLog(func(ctx context.Context, fl DecisionLogFilter) ([]model.DecisionLogRow, model.DecisionLogSummary, error) {
		rows, sum, err := f.src(ctx, fl)
		rows[0].Question, rows[0].Answer = "q\n"+fakeRow, "a\n"+fakeRow
		return rows, sum, err
	}, func() time.Time { return timelineT0 })
	var tm tea.Model = m
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	tm = timelinePump(tm, m.Init())
	tm, _ = tm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	noLineStarts(t, tm.View(), "decision log")
}

// D-10: the settings confirmation shows that the value is cut, and how long
// it is whole.
func TestSettingsConfirmShowsACutValueAsCut(t *testing.T) {
	f := &settingsFake{rows: settingsFakeRows()}
	d := settingsStart(t, f, "")
	long := strings.Repeat("a", 45)
	d.key("e", 2*time.Second)
	d.key(long, 2*time.Second)
	d.key("enter", 2*time.Second)
	v := d.m.View()
	if !strings.Contains(v, strings.Repeat("a", 30)+"…") || !strings.Contains(v, "45 chars") {
		t.Errorf("the confirmation does not show the cut and the full length:\n%s", v)
	}
}

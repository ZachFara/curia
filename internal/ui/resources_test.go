package ui

import (
	"slices"
	"strings"
	"testing"
)

// The CPU/RAM column (F19): the text the source rendered, "?" for a session
// with no current sample once the source reports resources at all, nothing
// when the source has no resource data, and nothing on a narrow window.
func TestResourcesColumn(t *testing.T) {
	m, _ := newCostTest(t, ModePopup, 200, Budget{Res: map[string]string{"s1": "13% 340M"}}, costSessions()...)
	for key, want := range map[string]string{"work:@1": "13% 340M", "work:@2": "?"} {
		if got := costCell(t, m, key, "CPU/RAM"); got != want {
			t.Errorf("%s CPU/RAM = %q, want %q", key, got, want)
		}
	}
	for _, c := range []struct {
		width int
		res   map[string]string
	}{{200, nil}, {119, map[string]string{}}} {
		m, _ = newCostTest(t, ModePopup, c.width, Budget{Res: c.res}, costSessions()...)
		titles, _, _ := costTable(t, m)
		if slices.Contains(titles, "CPU/RAM") {
			t.Errorf("width %d, res %v: the table has a CPU/RAM column: %q", c.width, c.res, titles)
		}
	}
}

// D-5: the cell is drawn through the cleaning function like every other cell.
func TestResourcesCellIsCleaned(t *testing.T) {
	m, _ := newCostTest(t, ModePopup, 200, Budget{Res: map[string]string{"s1": "13%\x1b[31m\u202e 340M"}}, costSessions()...)
	if v := m.View(); strings.ContainsAny(v, "\x1b\u202e") {
		t.Errorf("a control or bidi rune of the CPU/RAM cell reached the view:\n%q", v)
	}
}

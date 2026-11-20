package ui_test

import (
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

// A card with a budget shows the share spent and the warning marks (F16), in
// the columns and in the list, at 80 columns and with wide text; a card
// whose spend is unknown shows ?% and none of the marks.
func TestBoardShowsBudget(t *testing.T) {
	src := &fakeBoardSource{}
	cs := fourCards()
	for i, b := range []model.Budget{
		{Limit: 5, Spent: 0, Known: true},
		{Limit: 5, Spent: 4, Known: true},
		{Limit: 5, Spent: 5, Known: true},
		{Limit: 5},
	} {
		cs[i].Budget = b
	}
	cs[0].Task.Title = "日本語のタスク 😀"
	src.set(cs...)
	for _, c := range []struct {
		width int
		want  []string
	}{
		{80, []string{"curia  0% 日本", "curia  80%! task", "curia  100%!! task", "curia  ?% task"}},
		{120, []string{"T-1 0% 日本", "T-2 80%! task", "T-3 100%!! task", "T-4 ?% task"}},
	} {
		width := c.width
		view := newBoard(t, src, &tmux.Fake{}, width, false).View()
		checkWidth(t, view, width)
		for _, want := range c.want {
			if !strings.Contains(view, want) {
				t.Errorf("width %d: board lacks %q:\n%s", width, want, view)
			}
		}
	}
}

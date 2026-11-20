package ui_test

import (
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

// A task an agent filed shows that, by whom and why; the text is untrusted.
func TestBoardShowsAgentFiledTasks(t *testing.T) {
	c := card(1, model.TaskBacklog, "")
	c.Task.FiledBy, c.Task.FiledReason = "sess-9\x1b[31m", "flaky test\x1b]0;x\x07 found"
	for _, width := range []int{120, 80, 40} {
		src := &fakeBoardSource{}
		src.set(c)
		view := newBoard(t, src, &tmux.Fake{}, width, false).View()
		if strings.ContainsAny(view, "\x07") || strings.Contains(view, "\x1b[31m") || strings.Contains(view, "\x1b]0") {
			t.Errorf("width %d: view holds an escape sequence: %q", width, view)
		}
		if !strings.Contains(view, "[agent sess-9") {
			t.Errorf("width %d: no agent marker:\n%s", width, view)
		}
		checkWidth(t, view, width)
	}
	src := &fakeBoardSource{}
	src.set(c)
	if view := newBoard(t, src, &tmux.Fake{}, 60, false).View(); !strings.Contains(view, "flaky test") {
		t.Errorf("the list view lacks the reason:\n%s", view)
	}
}

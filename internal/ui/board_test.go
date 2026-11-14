package ui_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// boardNow is the fixed clock of the board tests.
var boardNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// boardClock is the clock of every board newBoard returns.
var boardClock testClock

// fakeBoardSource serves cards and records actions. SetStatus changes the
// card's status, as the store would.
type fakeBoardSource struct {
	mu       sync.Mutex
	cards    []task.Card
	err      error // returned by Cards when set
	setErr   error // returned by SetStatus when set
	spawnErr error
	line     string // what Spawn says
	calls    []string
}

func (f *fakeBoardSource) Cards(context.Context) ([]task.Card, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.cards), nil
}

func (f *fakeBoardSource) SetStatus(_ context.Context, id string, s model.TaskStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "SetStatus "+id+" "+string(s))
	if f.setErr != nil {
		return f.setErr
	}
	for i, c := range f.cards {
		if c.Task.ID == id {
			f.cards[i].Status, f.cards[i].Task.Status = s, s
		}
	}
	return nil
}

func (f *fakeBoardSource) Spawn(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Spawn "+id)
	return f.line, f.spawnErr
}

func (f *fakeBoardSource) set(cs ...task.Card) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = slices.Clone(cs)
}

func (f *fakeBoardSource) failReads(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeBoardSource) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func wantBoardCalls(t *testing.T, src *fakeBoardSource, want ...string) {
	t.Helper()
	if got := src.recorded(); !slices.Equal(got, want) {
		t.Errorf("source calls = %q, want %q", got, want)
	}
}

// card returns task T-n of repo /src/curia shown as status, with its lead
// in pane when pane is not empty.
func card(n int, status model.TaskStatus, pane string) task.Card {
	id := "T-" + strconv.Itoa(n)
	stored := status
	if status == model.TaskRunning || status == model.TaskNeedsDecision {
		stored = model.TaskReady
	}
	c := task.Card{
		Task:   model.Task{N: int64(n), ID: id, Title: "task " + id, Repo: "/src/curia", Status: stored},
		Status: status,
		Pane:   pane,
	}
	if pane != "" {
		c.Sessions = []model.Session{{ID: "S" + id, Pane: pane, TaskID: id, State: model.StateWorking}}
	}
	return c
}

// fourCards is one card in each column and one dropped.
func fourCards() []task.Card {
	return []task.Card{
		card(1, model.TaskBacklog, ""),
		card(2, model.TaskReady, ""),
		card(3, model.TaskRunning, "%3"),
		card(4, model.TaskNeedsDecision, "%4"),
		card(5, model.TaskDropped, ""),
	}
}

// newBoard returns a board over src after its first refresh and a window
// size of width x 30, with the clock moved on by keyGap.
func newBoard(t *testing.T, src *fakeBoardSource, tm tmux.Client, width int, embedded bool) tea.Model {
	t.Helper()
	boardClock.set(boardNow)
	m := reload(t, ui.NewBoard(src, tm, boardClock.now, embedded))
	m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
	boardClock.advance(keyGap)
	return m
}

func bkey(k string) tea.KeyMsg {
	switch k {
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return dkey(k)
}

// bpress sends keys through send, keyGap apart.
func bpress(t *testing.T, m tea.Model, keys ...string) tea.Model {
	t.Helper()
	for _, k := range keys {
		boardClock.advance(keyGap)
		m = send(t, m, bkey(k))
	}
	return m
}

// boardAt sends keys through send at the clock as it is.
func boardAt(t *testing.T, m tea.Model, keys ...string) tea.Model {
	t.Helper()
	for _, k := range keys {
		m = send(t, m, bkey(k))
	}
	return m
}

// selectedID is the id of the card drawn as selected ("> T-n").
func selectedID(t *testing.T, m tea.Model) string {
	t.Helper()
	view := m.View()
	i := strings.Index(view, "> T-")
	if i < 0 {
		t.Fatalf("no card selected:\n%s", view)
	}
	rest := view[i+2:]
	end := strings.IndexAny(rest, " \n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func wantSelected(t *testing.T, m tea.Model, id string) {
	t.Helper()
	if got := selectedID(t, m); got != id {
		t.Errorf("selected %s, want %s:\n%s", got, id, m.View())
	}
}

func checkWidth(t *testing.T, view string, width int) {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w > width {
			t.Errorf("line is %d cells, want at most %d: %q", w, width, l)
		}
	}
}

// cellIndex is the cell column at which sub starts in line, or -1.
func cellIndex(line, sub string) int {
	i := strings.Index(line, sub)
	if i < 0 {
		return -1
	}
	return lipgloss.Width(line[:i])
}

func TestBoardColumnsWide(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(fourCards()...)
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	view := m.View()
	checkWidth(t, view, 120)
	lines := strings.Split(view, "\n")
	headings := []string{"BACKLOG (1)", "READY (1)", "RUNNING (1)", "NEEDS DECISION (1)"}
	at := make([]int, len(headings))
	for i, h := range headings {
		if at[i] = cellIndex(lines[0], h); at[i] < 0 {
			t.Fatalf("heading line %q lacks %q", lines[0], h)
		}
	}
	if !slices.IsSorted(at) {
		t.Errorf("headings at cells %v, want them left to right", at)
	}
	for i, id := range []string{"T-1", "T-2", "T-3", "T-4"} {
		if got := cellIndex(lines[1], id+" task "+id); got != at[i]+2 {
			t.Errorf("%s drawn at cell %d, want %d under %q:\n%s", id, got, at[i]+2, headings[i], view)
		}
	}
	if strings.Contains(view, "T-5") {
		t.Errorf("dropped T-5 shown:\n%s", view)
	}
	wantSelected(t, m, "T-1")
}

func TestBoardListNarrow(t *testing.T) {
	src := &fakeBoardSource{}
	cs := fourCards()
	cs = slices.Insert(cs, 1, card(6, model.TaskBacklog, ""))
	src.set(cs...)
	for _, width := range []int{60, 99} {
		m := newBoard(t, src, &tmux.Fake{}, width, false)
		view := m.View()
		checkWidth(t, view, width)
		lines := strings.Split(view, "\n")
		want := []string{"BACKLOG (2)", "T-1", "T-6", "READY (1)", "T-2", "RUNNING (1)", "T-3", "NEEDS DECISION (1)", "T-4"}
		for i, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("width %d: line %d = %q, want it to hold %q:\n%s", width, i, lines[i], w, view)
			}
		}
		if strings.Contains(view, "T-5") {
			t.Errorf("width %d: dropped T-5 shown:\n%s", width, view)
		}
	}
	// At 100 columns the board is wide: all four headings on one line.
	m := newBoard(t, src, &tmux.Fake{}, 100, false)
	view := m.View()
	checkWidth(t, view, 100)
	if first := strings.Split(view, "\n")[0]; !strings.Contains(first, "BACKLOG") || !strings.Contains(first, "NEEDS DECISION") {
		t.Errorf("at 100 columns the first line is %q, want the four headings", first)
	}
}

func TestBoardMoveAcrossAndWithinColumns(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""), card(2, model.TaskBacklog, ""),
		card(3, model.TaskRunning, "%3"), card(4, model.TaskNeedsDecision, "%4"), card(7, model.TaskNeedsDecision, ""))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	steps := []struct{ key, want string }{
		{"j", "T-2"},
		{"down", "T-2"}, // the end of the column
		{"k", "T-1"},
		{"up", "T-1"},
		{"j", "T-2"},
		{"l", "T-3"}, // the empty ready column is skipped; the row is clamped
		{"right", "T-4"},
		{"j", "T-7"},
		{"l", "T-7"}, // no column to the right
		{"h", "T-3"},
		{"left", "T-1"},
		{"h", "T-1"},
	}
	for _, s := range steps {
		m = bpress(t, m, s.key)
		wantSelected(t, m, s.want)
	}
	wantBoardCalls(t, src)
}

func TestBoardListMovesAcrossHeadings(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""), card(2, model.TaskBacklog, ""), card(3, model.TaskRunning, "%3"))
	m := newBoard(t, src, &tmux.Fake{}, 60, false)
	for _, s := range []struct{ key, want string }{
		{"j", "T-2"}, {"j", "T-3"}, {"j", "T-3"}, {"k", "T-2"}, {"l", "T-3"}, {"h", "T-1"},
	} {
		m = bpress(t, m, s.key)
		wantSelected(t, m, s.want)
	}
}

func TestBoardSelectionFollowsTask(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""), card(2, model.TaskReady, ""), card(3, model.TaskReady, ""))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = bpress(t, m, "l", "j")
	wantSelected(t, m, "T-3")
	src.set(card(1, model.TaskBacklog, ""), card(2, model.TaskReady, ""), card(3, model.TaskRunning, "%3"))
	m = reload(t, m)
	wantSelected(t, m, "T-3")
	lines := strings.Split(m.View(), "\n")
	if got, want := cellIndex(lines[1], "> T-3"), cellIndex(lines[0], "RUNNING"); got != want {
		t.Errorf("T-3 drawn at cell %d, want %d under RUNNING:\n%s", got, want, m.View())
	}
	// The selected task gone and its column empty: the selection moves to
	// the nearest column with cards, the left one first.
	src.set(card(1, model.TaskBacklog, ""), card(2, model.TaskReady, ""))
	m = reload(t, m)
	wantSelected(t, m, "T-2")
}

func TestBoardReadyToggle(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""), card(3, model.TaskRunning, "%3"))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = bpress(t, m, "r")
	wantBoardCalls(t, src, "SetStatus T-1 ready")
	wantSelected(t, m, "T-1")
	lines := strings.Split(m.View(), "\n")
	if got, want := cellIndex(lines[1], "> T-1"), cellIndex(lines[0], "READY"); got != want {
		t.Errorf("after r, T-1 at cell %d, want %d under READY:\n%s", got, want, m.View())
	}
	m = bpress(t, m, "r")
	wantBoardCalls(t, src, "SetStatus T-1 ready", "SetStatus T-1 backlog")
	m = bpress(t, m, "l", "r")
	wantBoardCalls(t, src, "SetStatus T-1 ready", "SetStatus T-1 backlog")
	if s := lastLine(m); !strings.Contains(s, "T-3 is running") {
		t.Errorf("status line = %q, want why r did nothing", s)
	}
}

func TestBoardHeldReadyToggleActsOnce(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = boardAt(t, m, "r")
	boardClock.advance(500 * time.Millisecond) // a held key's first repeat
	for range 20 {
		m = boardAt(t, m, "r")
		boardClock.advance(30 * time.Millisecond)
	}
	wantBoardCalls(t, src, "SetStatus T-1 ready")
}

func TestBoardSpawnNeedsConfirmation(t *testing.T) {
	src := &fakeBoardSource{line: "spawned T-1 in curia:2"}
	src.set(card(1, model.TaskBacklog, ""), card(2, model.TaskReady, ""))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = bpress(t, m, "n")
	if s := lastLine(m); s != "spawn T-1? y starts it, any other key cancels" {
		t.Errorf("prompt = %q", s)
	}
	boardClock.advance(300 * time.Millisecond)
	m = boardAt(t, m, "y")
	wantBoardCalls(t, src, "Spawn T-1")
	if s := lastLine(m); s != "spawned T-1 in curia:2" {
		t.Errorf("status line = %q, want the spawn's line", s)
	}
	m = bpress(t, m, "l", "n", "x")
	if s := lastLine(m); s != "spawn canceled" {
		t.Errorf("status line = %q, want the cancel", s)
	}
	m = bpress(t, m, "y")
	wantBoardCalls(t, src, "Spawn T-1")
	src.spawnErr = errors.New("no tmux server")
	m = bpress(t, m, "n", "y")
	wantBoardCalls(t, src, "Spawn T-1", "Spawn T-2")
	if s := lastLine(m); !strings.Contains(s, "spawn failed: no tmux server") {
		t.Errorf("status line = %q, want the error", s)
	}
}

func TestBoardSpawnConfirmHonoursDwell(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""))
	// n and y in one read: the y is dropped, unseen prompts answer nothing.
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = boardAt(t, m, "ny")
	wantBoardCalls(t, src)
	if s := lastLine(m); !strings.HasPrefix(s, "spawn T-1?") {
		t.Errorf("status line = %q, want the prompt open", s)
	}
	// n and y as two reads at once.
	m = newBoard(t, src, &tmux.Fake{}, 120, false)
	m = boardAt(t, m, "n")
	m = boardAt(t, m, "y")
	wantBoardCalls(t, src)
	_ = m
	// n then y as separate messages handled before a frame is drawn: the y
	// is ignored and restarts the wait, so a y soon after is ignored too.
	m = newBoard(t, src, &tmux.Fake{}, 120, false)
	m = boardAt(t, m, "n", "y")
	boardClock.advance(300 * time.Millisecond)
	m = boardAt(t, m, "y")
	wantBoardCalls(t, src)
	if s := lastLine(m); !strings.HasPrefix(s, "spawn T-1?") {
		t.Errorf("status line = %q, want the prompt still open", s)
	}
	boardClock.advance(time.Second)
	m = boardAt(t, m, "y")
	wantBoardCalls(t, src, "Spawn T-1")
	_ = m
}

func TestBoardSpawnHeldYNeverConfirms(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = boardAt(t, m, "n")
	boardClock.advance(10 * time.Millisecond)
	m = boardAt(t, m, "y")
	boardClock.advance(600 * time.Millisecond) // the first repeat, after the keyboard's delay
	for range 40 {
		m = boardAt(t, m, "y")
		boardClock.advance(30 * time.Millisecond)
	}
	wantBoardCalls(t, src)
	_ = m
}

func TestBoardSpawnOnlyFromBacklogOrReady(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(3, model.TaskRunning, "%3"), card(4, model.TaskNeedsDecision, "%4"))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = bpress(t, m, "n")
	if s := lastLine(m); s != "T-3 is running; only a backlog or ready task can be spawned" {
		t.Errorf("status line = %q", s)
	}
	m = bpress(t, m, "y", "l", "n")
	if s := lastLine(m); s != "T-4 needs a decision; only a backlog or ready task can be spawned" {
		t.Errorf("status line = %q", s)
	}
	bpress(t, m, "y")
	wantBoardCalls(t, src)
}

func TestBoardSpawnConfirmAppliesToShownTask(t *testing.T) {
	cases := []struct {
		name  string
		after []task.Card
		want  []string
	}{
		{"gone", []task.Card{card(2, model.TaskBacklog, "")}, nil},
		{"renamed", []task.Card{func() task.Card { c := card(1, model.TaskBacklog, ""); c.Task.Title = "other"; return c }()}, nil},
		{"now running", []task.Card{card(1, model.TaskRunning, "%1")}, nil},
		{"another task first", []task.Card{card(0, model.TaskBacklog, ""), card(1, model.TaskBacklog, "")}, []string{"Spawn T-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeBoardSource{}
			src.set(card(1, model.TaskBacklog, ""))
			m := newBoard(t, src, &tmux.Fake{}, 120, false)
			m = bpress(t, m, "n")
			src.set(c.after...)
			m = reload(t, m)
			bpress(t, m, "y")
			wantBoardCalls(t, src, c.want...)
		})
	}
	// Changed and changed back while the prompt was open: the prompt closed
	// when it changed, so the y answers nothing.
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = bpress(t, m, "n")
	renamed := card(1, model.TaskBacklog, "")
	renamed.Task.Title = "other"
	src.set(renamed)
	m = reload(t, m)
	if s := lastLine(m); s != "T-1 changed; spawn canceled" {
		t.Errorf("status line = %q, want the prompt closed", s)
	}
	src.set(card(1, model.TaskBacklog, ""))
	m = reload(t, m)
	bpress(t, m, "y")
	wantBoardCalls(t, src)
}

func TestBoardEnterJumpsToLeadPane(t *testing.T) {
	src := &fakeBoardSource{}
	c := card(3, model.TaskRunning, "%5")
	c.Sessions = append(c.Sessions, model.Session{ID: "S9", Pane: "%9", TaskID: "T-3", State: model.StateWorking})
	src.set(card(1, model.TaskBacklog, ""), c)
	tm := &tmux.Fake{}
	m := newBoard(t, src, tm, 120, false)
	m = bpress(t, m, "enter")
	if len(tm.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", tm.Calls)
	}
	if s := lastLine(m); s != "T-1 has no live agent" {
		t.Errorf("status line = %q", s)
	}
	m = bpress(t, m, "l")
	boardClock.advance(keyGap)
	_, cmd := m.Update(bkey("enter"))
	if want := []string{"SwitchTo %5"}; !slices.Equal(tm.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", tm.Calls, want)
	}
	if !isQuitCmd(cmd) {
		t.Error("a jump did not quit")
	}
}

func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestBoardPasteIsNotACommand(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""), card(3, model.TaskRunning, "%3"))
	tm := &tmux.Fake{}
	m := newBoard(t, src, tm, 120, false)
	for _, p := range []string{"n", "y", "ny", "r", "l", "t", "q"} {
		boardClock.advance(keyGap)
		m = send(t, m, paste(p))
	}
	boardClock.advance(keyGap)
	m = send(t, m, paste("y"))
	wantSelected(t, m, "T-1")
	// A paste answers no prompt.
	m = bpress(t, m, "n")
	boardClock.advance(keyGap)
	m = send(t, m, paste("y"))
	m = bpress(t, m, "y")
	wantBoardCalls(t, src)
	if len(tm.Calls) != 0 {
		t.Errorf("tmux calls = %q", tm.Calls)
	}
	_ = m
}

func TestBoardTypedTextIsNotACommand(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""))
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	// "company" typed at a normal pace, as if into an input: c starts the
	// quiet period, and every key after it restarts it.
	for _, r := range "company" {
		m = boardAt(t, m, string(r))
		boardClock.advance(150 * time.Millisecond)
	}
	boardClock.advance(400 * time.Millisecond)
	m = boardAt(t, m, "y")
	wantBoardCalls(t, src)
	_ = m
}

func TestBoardBurstMoveThenActIgnored(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""), card(2, model.TaskBacklog, ""), card(3, model.TaskRunning, "%3"))
	tm := &tmux.Fake{}
	m := newBoard(t, src, tm, 120, false)
	m = boardAt(t, m, "jr")
	m = boardAt(t, m, "l")
	m = boardAt(t, m, "enter")
	wantBoardCalls(t, src)
	if len(tm.Calls) != 0 {
		t.Errorf("tmux calls = %q", tm.Calls)
	}
	_ = m
}

func TestBoardEmbeddedCloseSendsMessage(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""))
	for _, k := range []string{"t", "q", "esc"} {
		m := newBoard(t, src, &tmux.Fake{}, 120, true)
		boardClock.advance(keyGap)
		_, cmd := m.Update(bkey(k))
		if cmd == nil {
			t.Fatalf("%s: no command", k)
		}
		if _, ok := cmd().(ui.BoardClosedMsg); !ok {
			t.Errorf("%s: command did not send BoardClosedMsg", k)
		}
		m = newBoard(t, src, &tmux.Fake{}, 120, false)
		boardClock.advance(keyGap)
		if _, cmd = m.Update(bkey(k)); !isQuitCmd(cmd) {
			t.Errorf("%s: standalone board did not quit", k)
		}
	}
	// A jump closes an embedded board too.
	src.set(card(3, model.TaskRunning, "%3"))
	m := newBoard(t, src, &tmux.Fake{}, 120, true)
	boardClock.advance(keyGap)
	_, cmd := m.Update(bkey("enter"))
	if cmd == nil {
		t.Fatal("enter: no command")
	}
	if _, ok := cmd().(ui.BoardClosedMsg); !ok {
		t.Error("enter: command did not send BoardClosedMsg")
	}
}

func TestBoardTextCannotDrawEscapes(t *testing.T) {
	const evil = "\x1b[2J\x1b]0;x\x07\u202e"
	src := &fakeBoardSource{line: "spawned" + evil + "\nok"}
	bad := card(1, model.TaskBacklog, "")
	bad.Task.Title = "A" + evil + "\nB"
	bad.Task.Repo = "/src/re" + evil + "po"
	wide := card(2, model.TaskReady, "")
	wide.Task.Title = strings.Repeat("漢字", 30) + "\u202e" + strings.Repeat("🙂", 10)
	src.set(bad, wide, card(3, model.TaskRunning, "%3"))
	for _, width := range []int{40, 80, 200} {
		m := newBoard(t, src, &tmux.Fake{}, width, false)
		for _, keys := range [][]string{nil, {"n", "y"}, {"l"}} {
			m = bpress(t, m, keys...)
			view := m.View()
			for _, b := range []string{"\x1b", "\x07", "\u202e"} {
				if strings.Contains(view, b) {
					t.Errorf("width %d %q: view holds %q:\n%q", width, keys, b, view)
				}
			}
			checkWidth(t, view, width)
			if !slices.ContainsFunc(strings.Split(view, "\n"), func(l string) bool {
				return strings.Contains(l, "T-1") && strings.Contains(l, "A B")
			}) {
				t.Errorf("width %d %q: no line holds T-1 and its title on one line:\n%s", width, keys, view)
			}
		}
	}
}

func TestBoardSourceErrorKeepsLastCards(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(fourCards()...)
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	m = bpress(t, m, "l")
	src.failReads(errors.New("database is locked\x1b[2J"))
	m = reload(t, m)
	view := m.View()
	for _, id := range []string{"T-1", "T-2", "T-3", "T-4"} {
		if !strings.Contains(view, id) {
			t.Errorf("view lost %s after a failed read:\n%s", id, view)
		}
	}
	wantSelected(t, m, "T-2")
	if s := lastLine(m); !strings.Contains(s, "database is locked") || strings.Contains(s, "\x1b") {
		t.Errorf("status line = %q, want the cleaned error", s)
	}
	src.failReads(nil)
	m = reload(t, m)
	if s := lastLine(m); strings.Contains(s, "locked") {
		t.Errorf("status line = %q after a good read", s)
	}
}

func TestBoardEmpty(t *testing.T) {
	for _, cs := range [][]task.Card{nil, {card(5, model.TaskDropped, "")}} {
		src := &fakeBoardSource{}
		src.set(cs...)
		for _, width := range []int{60, 120} {
			m := newBoard(t, src, &tmux.Fake{}, width, false)
			if view := m.View(); !strings.Contains(view, "no tasks; add one with curia task add") {
				t.Errorf("width %d, %d cards: view = %q", width, len(cs), view)
			}
			m = bpress(t, m, "j", "l", "r", "n", "y", "enter")
			_ = m
			wantBoardCalls(t, src)
		}
	}
}

func TestBoardFitsHeight(t *testing.T) {
	src := &fakeBoardSource{}
	var cs []task.Card
	for i := 1; i <= 40; i++ {
		cs = append(cs, card(i, model.TaskBacklog, ""))
	}
	src.set(cs...)
	for _, width := range []int{60, 120} {
		m := newBoard(t, src, &tmux.Fake{}, width, false)
		m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 10})
		for range 30 {
			m = bpress(t, m, "j")
		}
		view := m.View()
		if n := len(strings.Split(view, "\n")); n > 10 {
			t.Errorf("width %d: %d lines, want at most 10", width, n)
		}
		wantSelected(t, m, "T-31")
		if !strings.Contains(view, fmt.Sprintf("> T-%d", 31)) {
			t.Errorf("width %d: selected card not on screen:\n%s", width, view)
		}
	}
}

func TestBoardCloseWaitsForActionInFlight(t *testing.T) {
	src := &fakeBoardSource{}
	src.set(card(1, model.TaskBacklog, ""))
	for _, k := range []string{"q", "ctrl+c"} {
		m := newBoard(t, src, &tmux.Fake{}, 120, true)
		boardClock.advance(keyGap)
		m, act := m.Update(bkey("r"))
		if act == nil {
			t.Fatalf("%s: r started nothing", k)
		}
		m, cmd := m.Update(bkey(k))
		if cmd != nil {
			t.Fatalf("%s: closed while the action was in flight", k)
		}
		_, cmd = m.Update(act())
		if cmd == nil {
			t.Fatalf("%s: no close after the action returned", k)
		}
		msg := cmd()
		_, closed := msg.(ui.BoardClosedMsg)
		_, quit := msg.(tea.QuitMsg)
		if (k == "q" && !closed) || (k == "ctrl+c" && !quit) {
			t.Errorf("%s: after the action, got %T", k, msg)
		}
	}
	// A failed action keeps the board open to show why.
	src.setErr = errors.New("database is locked")
	m := newBoard(t, src, &tmux.Fake{}, 120, false)
	boardClock.advance(keyGap)
	m, act := m.Update(bkey("r"))
	m, _ = m.Update(bkey("q"))
	m, cmd := m.Update(act())
	if isQuitCmd(cmd) {
		t.Error("closed after a failed action")
	}
	if s := lastLine(m); s != "set failed: database is locked" {
		t.Errorf("status line = %q", s)
	}
}

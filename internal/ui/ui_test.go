package ui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os/exec"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
)

// testNow is the fixed clock of the dashboard tests.
var testNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// fakeSource returns sessions, or err when it is set, and counts, or
// countsErr when it is set.
type fakeSource struct {
	sessions  []model.Session
	err       error
	counts    map[string]model.OpenCount
	countsErr error
	countRead int // calls of OpenDecisions
	tasks     map[string]model.Task
	tasksErr  error
	taskRead  int // calls of Tasks

	reviews    map[string]string
	reviewsErr error
	reviewRead int // calls of Reviews

	budget     Budget // the zero value unless a test sets one
	budgetErr  error
	budgetRead int // calls of Budget

	pause  func(id string) (string, error) // nil: a call of Pause fails the test
	paused []string                        // the ids Pause was called with
}

// Pause records the call and answers with f.pause. A test that sets no
// pause allows none: the call panics, which fails the test.
func (f *fakeSource) Pause(_ context.Context, id string) (string, error) {
	if f.pause == nil {
		panic("Pause(" + id + ") called on a source whose test allows no pause")
	}
	f.paused = append(f.paused, id)
	return f.pause(id)
}

func (f *fakeSource) Budget(context.Context) (Budget, error) {
	f.budgetRead++
	if f.budgetErr != nil {
		return Budget{}, f.budgetErr
	}
	b := f.budget
	b.Burn = maps.Clone(f.budget.Burn)
	return b, nil
}

func (f *fakeSource) Reviews(context.Context) (map[string]string, error) {
	f.reviewRead++
	if f.reviewsErr != nil {
		return nil, f.reviewsErr
	}
	return maps.Clone(f.reviews), nil
}

func (f *fakeSource) Tasks(context.Context) (map[string]model.Task, error) {
	f.taskRead++
	if f.tasksErr != nil {
		return nil, f.tasksErr
	}
	return maps.Clone(f.tasks), nil
}

func (f *fakeSource) Sessions(context.Context) ([]model.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.sessions), nil
}

func (f *fakeSource) OpenDecisions(context.Context) (map[string]model.OpenCount, error) {
	f.countRead++
	if f.countsErr != nil {
		return nil, f.countsErr
	}
	return maps.Clone(f.counts), nil
}

// threeSessions are in Source order: not the order fleet.Rank would give,
// so a test can tell the model keeps the Source's order.
func threeSessions() []model.Session {
	return []model.Session{
		{
			ID: "a", Pane: "%3", TmuxSession: "work", TmuxWindow: "@1", Repo: "curia", Branch: "main",
			State: model.StateWorking, Model: "claude-opus-5-5", ContextUsed: 120000, ContextMax: 200000,
			SubagentsActive: 2, LastUpdate: "Editing store.go", UpdatedAt: testNow.Add(-5 * time.Minute),
		},
		{
			ID: "b", Pane: "%5", TmuxSession: "work", TmuxWindow: "@2", Repo: "api", Branch: "fix-login",
			State: model.StateNeedsDecision, Model: "claude-sonnet-4-5", ContextUsed: 10, ContextMax: 100,
			LastUpdate: "Allow Bash: make test?", UpdatedAt: testNow.Add(-2 * time.Hour),
		},
		{
			ID: "c", Pane: "", Repo: "notes", State: model.StateIdle, Model: "claude-haiku-4-5",
			LastUpdate: "Done.", UpdatedAt: testNow.Add(-30 * time.Second),
		},
	}
}

// newTest returns a model over sessions with a fake tmux whose server has
// windows @1 "editor" and @2 "server", after its first refresh.
func newTest(t *testing.T, mode Mode, sessions ...model.Session) (tea.Model, *tmux.Fake, *fakeSource) {
	t.Helper()
	fake := &tmux.Fake{Panes: []tmux.Pane{
		{ID: "%3", Session: "work", WindowID: "@1", WindowName: "editor"},
		{ID: "%5", Session: "work", WindowID: "@2", WindowName: "server"},
	}}
	src := &fakeSource{sessions: sessions}
	m := New(src, fake, config.Default(), func() time.Time { return testNow }, mode)
	return refresh(t, m), fake, src
}

// refresh runs the model's refresh command and feeds its message back.
func refresh(t *testing.T, m tea.Model) tea.Model {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
	m, next := m.Update(cmd())
	if next == nil {
		t.Fatal("refresh did not schedule the next one")
	}
	return m
}

func key(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

// press sends each key in turn and returns the model and the last command.
func press(m tea.Model, keys ...string) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		m, cmd = m.Update(key(k))
	}
	return m, cmd
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// calls returns the fake's calls of method.
func calls(f *tmux.Fake, method string) []string {
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, method+" ") || c == method {
			out = append(out, c)
		}
	}
	return out
}

// lineWith returns the index of the first line of view containing s, or -1.
func lineWith(view, s string) int {
	for i, l := range strings.Split(view, "\n") {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

// statusLine is the last line of the view.
func statusLine(m tea.Model) string {
	lines := strings.Split(m.View(), "\n")
	return lines[len(lines)-1]
}

func TestContextLevelThresholds(t *testing.T) {
	tests := []struct {
		used, max int
		want      Level
	}{
		{59, 100, LevelOK},
		{60, 100, LevelWarn},
		{85, 100, LevelWarn},
		{86, 100, LevelCrit},
		{50, 0, LevelUnknown},
		{0, 200000, LevelOK},
		{171999, 200000, LevelWarn}, // shown as 85%, so Warn
		{172000, 200000, LevelCrit}, // 86%
		{119999, 200000, LevelOK},   // shown as 59%
		{120000, 200000, LevelWarn},
	}
	for _, tc := range tests {
		if got := ContextLevel(tc.used, tc.max, 60, 85); got != tc.want {
			t.Errorf("ContextLevel(%d, %d, 60, 85) = %v, want %v", tc.used, tc.max, got, tc.want)
		}
	}
}

func TestContextLevelColors(t *testing.T) {
	want := map[Level]lipgloss.TerminalColor{
		LevelOK: lipgloss.Color("2"), LevelWarn: lipgloss.Color("3"), LevelCrit: lipgloss.Color("1"),
		LevelUnknown: lipgloss.NoColor{},
	}
	for level, color := range want {
		if got := levelStyle(level).GetForeground(); got != color {
			t.Errorf("levelStyle(%v) foreground = %v, want %v", level, got, color)
		}
	}
	if !staleStyle.GetFaint() {
		t.Error("stale rows are not dim")
	}
}

func TestViewShowsRankedRows(t *testing.T) {
	m, _, _ := newTest(t, ModePopup, threeSessions()...)
	view := m.View()
	rows := [][]string{
		{stateIcon(model.StateWorking), "work:editor", "curia@main", "claude-opus-5-5", "60%", "2", "Editing store.go", "5m"},
		{stateIcon(model.StateNeedsDecision), "work:server", "api@fix-login", "claude-sonnet-4-5", "10%", "0", "Allow Bash: make test?", "2h"},
		{stateIcon(model.StateIdle), "-", "notes", "claude-haiku-4-5", "-", "0", "Done.", "30s"},
	}
	prev := lineWith(view, "SESSION:WINDOW")
	if prev != 0 {
		t.Fatalf("header not on the first line:\n%s", view)
	}
	if len(popupColumns) != 12 {
		t.Errorf("popup has %d columns, want 12", len(popupColumns))
	}
	if h := strings.Fields(strings.Split(view, "\n")[0]); !slices.Equal(h, []string{
		"SESSION:WINDOW", "REPO@BRANCH", "TASK", "MODEL", "CTX", "SUB", "DEC", "COST", "CACHE", "LAST", "UPDATE", "AGE",
	}) {
		t.Errorf("header = %q, want DEC between SUB and COST, CACHE, LAST UPDATE", h)
	}
	for _, cells := range rows {
		i := lineWith(view, cells[3]) // the model names are distinct
		if i <= prev {
			t.Fatalf("row %q at line %d, want after line %d:\n%s", cells[3], i, prev, view)
		}
		line := strings.Split(view, "\n")[i]
		for _, c := range cells {
			if !strings.Contains(line, c) {
				t.Errorf("row %q lacks column %q", line, c)
			}
		}
		prev = i
	}
}

func TestWindowNameFallsBackToID(t *testing.T) {
	s := threeSessions()[0]
	s.TmuxWindow = "@9" // not on the server
	m, _, _ := newTest(t, ModePopup, s)
	if !strings.Contains(m.View(), "work:@9") {
		t.Errorf("view lacks work:@9:\n%s", m.View())
	}

	down := &downTmux{}
	m = refresh(t, New(&fakeSource{sessions: threeSessions()[:1]}, down, config.Default(), func() time.Time { return testNow }, ModePopup))
	if !strings.Contains(m.View(), "work:@1") {
		t.Errorf("with tmux down, view lacks work:@1:\n%s", m.View())
	}
}

// downTmux is a tmux client whose server cannot be reached.
type downTmux struct{ tmux.Fake }

func (*downTmux) ListPanes(context.Context) ([]tmux.Pane, error) {
	return nil, errors.New("no server running")
}

func (*downTmux) SwitchTo(context.Context, string) error { return errors.New("no server running") }

func TestEnterSwitchesToSelectedPane(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	_, cmd := press(m, "down", "enter")
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %5"}) {
		t.Errorf("SwitchTo calls = %q, want [SwitchTo %%5]", got)
	}
	if !isQuit(cmd) {
		t.Error("popup did not quit after jumping")
	}
}

func TestJKMoveSelection(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	_, _ = press(m, "j", "j", "j", "k", "k", "k", "j", "enter") // clamps at both ends
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %5"}) {
		t.Errorf("SwitchTo calls = %q, want [SwitchTo %%5]", got)
	}
}

func TestSidebarEnterDoesNotQuit(t *testing.T) {
	m, fake, _ := newTest(t, ModeSidebar, threeSessions()...)
	_, cmd := press(m, "j", "enter")
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %5"}) {
		t.Errorf("SwitchTo calls = %q, want [SwitchTo %%5]", got)
	}
	if isQuit(cmd) {
		t.Error("sidebar quit after jumping")
	}
}

func TestSwitchFailureShownAndPopupStays(t *testing.T) {
	m := refresh(t, New(&fakeSource{sessions: threeSessions()}, &downTmux{}, config.Default(), func() time.Time { return testNow }, ModePopup))
	m, cmd := press(m, "enter")
	if isQuit(cmd) {
		t.Error("popup quit although the jump failed")
	}
	if s := statusLine(m); !strings.Contains(s, "no server running") {
		t.Errorf("status line = %q, want the switch error", s)
	}
}

func TestKillRequiresConfirmation(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	m, _ = press(m, "down", "x")
	if got := calls(fake, "KillPane"); len(got) != 0 {
		t.Fatalf("x alone killed: %q", got)
	}
	if s := statusLine(m); !strings.Contains(s, "%5") || !strings.Contains(s, "y") {
		t.Errorf("status line = %q, want a confirmation prompt naming %%5", s)
	}
	m, _ = press(m, "j") // any other key cancels, and does nothing else
	if got := calls(fake, "KillPane"); len(got) != 0 {
		t.Fatalf("x then j killed: %q", got)
	}
	_, cmd := press(m, "x", "y")
	if got := calls(fake, "KillPane"); !slices.Equal(got, []string{"KillPane %5"}) {
		t.Errorf("KillPane calls = %q, want [KillPane %%5]", got)
	}
	if isQuit(cmd) {
		t.Error("popup quit after killing")
	}
}

func TestKillTargetsPaneChosenAtPrompt(t *testing.T) {
	m, fake, src := newTest(t, ModePopup, threeSessions()...)
	m, _ = press(m, "down", "x")
	// A refresh reorders the rows while the prompt is open.
	src.sessions = slices.Concat(threeSessions()[1:2], threeSessions()[:1], threeSessions()[2:])
	m = refresh(t, m)
	_, _ = press(m, "y")
	if got := calls(fake, "KillPane"); !slices.Equal(got, []string{"KillPane %5"}) {
		t.Errorf("KillPane calls = %q, want [KillPane %%5]", got)
	}
}

func TestKillWithoutPaneRefused(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	m, _ = press(m, "down", "down", "x")
	if s := statusLine(m); !strings.Contains(s, "not in tmux") {
		t.Errorf("status line = %q, want not in tmux", s)
	}
	_, _ = press(m, "y")
	if got := calls(fake, "KillPane"); len(got) != 0 {
		t.Errorf("killed a session with no pane: %q", got)
	}
}

func TestFilterNarrowsRows(t *testing.T) {
	sessions := []model.Session{
		{ID: "1", Repo: "auth-svc", Branch: "main", Model: "m-repo", UpdatedAt: testNow},
		{ID: "2", Repo: "web", Branch: "fix-auth", Model: "m-branch", UpdatedAt: testNow},
		{ID: "3", Repo: "api", Branch: "main", LastUpdate: "Refactored AUTH middleware", Model: "m-update", UpdatedAt: testNow},
		{ID: "4", Repo: "docs", Branch: "main", LastUpdate: "Wrote README", Model: "m-other", UpdatedAt: testNow},
	}
	m, _, _ := newTest(t, ModePopup, sessions...)
	m, _ = press(m, "/", "auth")
	view := m.View()
	for _, want := range []string{"m-repo", "m-branch", "m-update"} {
		if !strings.Contains(view, want) {
			t.Errorf("filtered view lacks %s:\n%s", want, view)
		}
	}
	if strings.Contains(view, "m-other") {
		t.Errorf("filtered view still shows m-other:\n%s", view)
	}
	if s := statusLine(m); !strings.Contains(s, "/auth") {
		t.Errorf("status line = %q, want the filter being typed", s)
	}

	// Enter keeps the filter and leaves filter mode; Esc then quits.
	m, _ = press(m, "enter")
	if strings.Contains(m.View(), "m-other") {
		t.Error("Enter dropped the filter")
	}
	// Esc while typing clears the filter instead of quitting.
	m, cmd := press(m, "/", "backspace", "esc")
	if isQuit(cmd) || !strings.Contains(m.View(), "m-other") {
		t.Errorf("Esc while filtering: quit %v, view:\n%s", isQuit(cmd), m.View())
	}
}

func TestFilterKeysAreText(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	m, cmd := press(m, "/", "q", "x", "j", "k")
	if isQuit(cmd) || len(calls(fake, "KillPane")) != 0 {
		t.Fatal("keys typed into the filter acted as commands")
	}
	if s := statusLine(m); !strings.Contains(s, "/qxjk") {
		t.Errorf("status line = %q, want /qxjk", s)
	}
}

func TestRowWithoutPaneNotJumpable(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	m, cmd := press(m, "down", "down", "enter")
	if got := calls(fake, "SwitchTo"); len(got) != 0 {
		t.Errorf("SwitchTo calls = %q, want none", got)
	}
	if isQuit(cmd) {
		t.Error("popup quit without jumping")
	}
	if s := statusLine(m); !strings.Contains(s, "not in tmux") {
		t.Errorf("status line = %q, want not in tmux", s)
	}
}

func TestNarrowWidthTruncates(t *testing.T) {
	sessions := threeSessions()
	sessions[0].LastUpdate = strings.Repeat("a very long last update ", 10)
	sessions[1].Repo = "a-repository-with-a-really-long-name"
	for _, tc := range []struct {
		mode  Mode
		width int
	}{{ModePopup, 60}, {ModePopup, 20}, {ModeSidebar, 30}, {ModeSidebar, 8}} {
		m, _, _ := newTest(t, tc.mode, sessions...)
		m, _ = m.Update(tea.WindowSizeMsg{Width: tc.width, Height: 20})
		for _, l := range strings.Split(m.View(), "\n") {
			if w := lipgloss.Width(l); w > tc.width {
				t.Errorf("mode %v width %d: line is %d cells: %q", tc.mode, tc.width, w, l)
			}
		}
	}
}

func TestSidebarShowsNarrowColumns(t *testing.T) {
	m, _, _ := newTest(t, ModeSidebar, threeSessions()...)
	view := m.View()
	line := strings.Split(view, "\n")[lineWith(view, "work:editor")]
	for _, want := range []string{stateIcon(model.StateWorking), "work:editor", "60%", "5m"} {
		if !strings.Contains(line, want) {
			t.Errorf("sidebar row %q lacks %q", line, want)
		}
	}
	for _, absent := range []string{"claude-opus-5-5", "curia@main", "Editing store.go"} {
		if strings.Contains(view, absent) {
			t.Errorf("sidebar shows %q:\n%s", absent, view)
		}
	}
}

func TestQuitKeys(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		for _, mode := range []Mode{ModePopup, ModeSidebar} {
			m, _, _ := newTest(t, mode, threeSessions()...)
			if _, cmd := press(m, k); !isQuit(cmd) {
				t.Errorf("mode %v: %q did not quit", mode, k)
			}
		}
	}
	m, _, _ := newTest(t, ModePopup, threeSessions()...)
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); !isQuit(cmd) {
		t.Error("ctrl+c did not quit")
	}
}

func TestSourceErrorKeepsRows(t *testing.T) {
	m, _, src := newTest(t, ModePopup, threeSessions()...)
	src.err = errors.New("database is locked")
	m = refresh(t, m) // refresh fails if it schedules no next refresh
	if !strings.Contains(m.View(), "claude-opus-5-5") {
		t.Errorf("rows gone after a failed refresh:\n%s", m.View())
	}
	if s := statusLine(m); !strings.Contains(s, "database is locked") {
		t.Errorf("status line = %q, want the Source error", s)
	}
	src.err = nil
	m = refresh(t, m)
	if s := statusLine(m); strings.Contains(s, "database is locked") {
		t.Errorf("status line = %q after a good refresh, want the error gone", s)
	}
}

func TestTickRefreshes(t *testing.T) {
	m, _, src := newTest(t, ModePopup, threeSessions()...)
	src.sessions = src.sessions[:1]
	m, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("tick started no refresh")
	}
	m, _ = m.Update(cmd())
	if strings.Contains(m.View(), "claude-haiku-4-5") {
		t.Errorf("tick did not re-read the Source:\n%s", m.View())
	}
}

func TestSelectionFollowsSessionAcrossRefresh(t *testing.T) {
	m, fake, src := newTest(t, ModePopup, threeSessions()...)
	m, _ = press(m, "down") // session b, pane %5
	src.sessions = slices.Concat(threeSessions()[1:2], threeSessions()[:1], threeSessions()[2:])
	m = refresh(t, m)
	_, _ = press(m, "enter")
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %5"}) {
		t.Errorf("SwitchTo calls = %q, want [SwitchTo %%5]", got)
	}
}

func TestClickJumpsToRow(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	y := lineWith(m.View(), "claude-sonnet-4-5")
	_, cmd := m.Update(tea.MouseMsg{X: 3, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %5"}) {
		t.Errorf("SwitchTo calls = %q, want [SwitchTo %%5]", got)
	}
	if !isQuit(cmd) {
		t.Error("popup did not quit after a click jump")
	}
	// A click on the header or below the rows does nothing.
	for _, y := range []int{0, 10} {
		_, _ = m.Update(tea.MouseMsg{X: 3, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	}
	if got := calls(fake, "SwitchTo"); len(got) != 1 {
		t.Errorf("SwitchTo calls = %q, want one", got)
	}
}

func TestSelectionScrollsIntoView(t *testing.T) {
	var sessions []model.Session
	for i := range 10 {
		sessions = append(sessions, model.Session{
			ID: string(rune('a' + i)), Pane: "%" + string(rune('a'+i)), Model: "model-" + string(rune('a'+i)),
			UpdatedAt: testNow,
		})
	}
	m, fake, _ := newTest(t, ModePopup, sessions...)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 5}) // header, 3 rows, status
	m, _ = press(m, "j", "j", "j", "j", "j")
	view := m.View()
	if n := len(strings.Split(view, "\n")); n != 5 {
		t.Errorf("view has %d lines, want 5:\n%s", n, view)
	}
	if !strings.Contains(view, "model-f") || strings.Contains(view, "model-a") {
		t.Errorf("selected row model-f not scrolled into view:\n%s", view)
	}
	// A click maps through the scroll offset.
	y := lineWith(view, "model-e")
	_, _ = m.Update(tea.MouseMsg{Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %e"}) {
		t.Errorf("SwitchTo calls = %q, want [SwitchTo %%e]", got)
	}
}

func TestViewStripsControlCharacters(t *testing.T) {
	s := threeSessions()[0]
	s.LastUpdate = "evil\x1b]52;c;cGF3bmVk\x07 \x1b[2J\nnext\u202e"
	s.Repo = "re\x1bpo"
	s.Model = "mo\tdel"
	fake := &tmux.Fake{Panes: []tmux.Pane{{ID: "%3", Session: "work", WindowID: "@1", WindowName: "win\x1b[31m"}}}
	m := refresh(t, New(&fakeSource{sessions: []model.Session{s}}, fake, config.Default(), func() time.Time { return testNow }, ModePopup))
	view := m.View()
	if !strings.Contains(view, "evil?]52;c;cGF3bmVk? ?[2J?next?") {
		t.Errorf("last update not shown with control characters replaced:\n%q", view)
	}
	m, _ = press(m, "/", "\x1b[1m") // pasted into the filter
	for _, v := range []string{view, m.View()} {
		for _, bad := range []string{"\x1b", "\x07", "\t", "\n\n", "\u202e"} {
			if strings.Contains(v, bad) {
				t.Errorf("view contains %q:\n%q", bad, v)
			}
		}
	}
}

func TestEmptyView(t *testing.T) {
	m, _, _ := newTest(t, ModePopup)
	if !strings.Contains(m.View(), "no sessions") {
		t.Errorf("empty view:\n%s", m.View())
	}
	m, fake, _ := newTest(t, ModePopup)
	_, cmd := press(m, "enter", "x", "y")
	if len(fake.Calls) != 1 || isQuit(cmd) { // only the refresh's ListPanes
		t.Errorf("keys on an empty list: calls %q, quit %v", fake.Calls, isQuit(cmd))
	}
}

// Keys typed faster than the program reads arrive as one message with
// several runes; each still counts.
func TestFastTypedKeysEachCount(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	m, _ = m.Update(key("jj"))
	m, _ = m.Update(key("kx"))
	_, _ = m.Update(key("y"))
	if got := calls(fake, "KillPane"); !slices.Equal(got, []string{"KillPane %5"}) {
		t.Errorf("KillPane calls = %q, want [KillPane %%5]", got)
	}
	m, _, _ = newTest(t, ModePopup, threeSessions()...)
	m, _ = m.Update(key("/au"))
	if s := statusLine(m); !strings.Contains(s, "/au") {
		t.Errorf("status line = %q, want /au", s)
	}
	if _, cmd := m.Update(key("jq")); isQuit(cmd) {
		t.Error("q typed into the filter quit")
	}
}

func TestPasteIsNotACommand(t *testing.T) {
	m, fake, _ := newTest(t, ModePopup, threeSessions()...)
	paste := func(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true} }
	m, _ = m.Update(paste("jxy"))
	if _, cmd := m.Update(paste("q")); isQuit(cmd) || len(calls(fake, "KillPane")) != 0 {
		t.Error("pasted text acted as commands")
	}
	m, _ = press(m, "/")
	m, _ = m.Update(paste("au th"))
	if s := statusLine(m); !strings.Contains(s, "/au th") {
		t.Errorf("status line = %q, want the pasted filter /au th", s)
	}
}

// A stale session's pane is gone; after a tmux server restart its id may
// name an unrelated pane, so it is never jumped to or killed.
func TestStaleRowNotJumpableOrKillable(t *testing.T) {
	stale := threeSessions()[1] // pane %5, which the fake server has
	stale.State = model.StateStale
	m, fake, _ := newTest(t, ModePopup, stale)
	m, cmd := press(m, "enter")
	if isQuit(cmd) || !strings.Contains(statusLine(m), "pane gone") {
		t.Errorf("Enter on a stale row: quit %v, status %q, want pane gone", isQuit(cmd), statusLine(m))
	}
	y := lineWith(m.View(), "claude-sonnet-4-5")
	m, cmd = m.Update(tea.MouseMsg{X: 3, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if isQuit(cmd) || !strings.Contains(statusLine(m), "pane gone") {
		t.Errorf("click on a stale row: quit %v, status %q, want pane gone", isQuit(cmd), statusLine(m))
	}
	m, _ = press(m, "x")
	if s := statusLine(m); !strings.Contains(s, "pane gone") {
		t.Errorf("x on a stale row: status %q, want pane gone", s)
	}
	_, _ = press(m, "y")
	if got := slices.Concat(calls(fake, "SwitchTo"), calls(fake, "KillPane")); len(got) != 0 {
		t.Errorf("tmux calls on a stale row = %q, want none", got)
	}
}

// Ruling 22: the repo column shows the basename of the stored repo path.
func TestRepoColumnShowsBasename(t *testing.T) {
	s := threeSessions()[:2]
	s[0].Repo = "/Users/me/src/curia"
	s[1].Repo, s[1].Branch, s[1].Model = "", "main", "no-repo-model"
	m, _, _ := newTest(t, ModePopup, s...)
	view := m.View()
	if !strings.Contains(view, "curia@main") || strings.Contains(view, "/Users/me") {
		t.Errorf("repo column is not curia@main:\n%s", view)
	}
	line := strings.Split(view, "\n")[lineWith(view, "no-repo-model")]
	if strings.Contains(line, "@main") || strings.Contains(line, ".") {
		t.Errorf("row without a repo = %q, want an empty repo cell", line)
	}
}

// The filter matches what the repo cell shows (the basename), not the
// hidden directories of the stored path.
func TestFilterMatchesDisplayedRepo(t *testing.T) {
	sessions := []model.Session{
		{ID: "1", Repo: "/home/zach/src/web", Branch: "main", Model: "m-web", UpdatedAt: testNow},
		{ID: "2", Repo: "/home/zach/src/api", Branch: "dev", Model: "m-api", UpdatedAt: testNow},
	}
	m, _, _ := newTest(t, ModePopup, sessions...)
	if f, _ := press(m, "/", "zach"); strings.Contains(f.View(), "m-web") || strings.Contains(f.View(), "m-api") {
		t.Errorf("filter on a hidden path directory matched:\n%s", f.View())
	}
	m, _ = press(m, "/", "web@m")
	if view := m.View(); !strings.Contains(view, "m-web") || strings.Contains(view, "m-api") {
		t.Errorf("filter web@m, want only m-web:\n%s", view)
	}
}

func TestShownPercentMatchesColor(t *testing.T) {
	s := threeSessions()[0]
	s.ContextUsed, s.ContextMax = 171999, 200000 // 85.9995%
	m, _, _ := newTest(t, ModePopup, s)
	if !strings.Contains(m.View(), "85%") {
		t.Errorf("view lacks 85%%:\n%s", m.View())
	}
	if l := ContextLevel(s.ContextUsed, s.ContextMax, 60, 85); l != LevelWarn {
		t.Errorf("level of a row shown as 85%% = %v, want Warn", l)
	}
}

// slowTmux is a tmux client whose ListPanes waits until its context ends.
type slowTmux struct {
	tmux.Fake
	deadline time.Duration // time left on ListPanes' context when called
}

func (s *slowTmux) ListPanes(ctx context.Context) ([]tmux.Pane, error) {
	if d, ok := ctx.Deadline(); ok {
		s.deadline = time.Until(d)
	}
	return nil, ctx.Err()
}

// slowSource uses up most of the refresh's time.
type slowSource struct{ fakeSource }

func (s *slowSource) Sessions(ctx context.Context) ([]model.Session, error) {
	d, _ := ctx.Deadline()
	time.Sleep(time.Until(d) - 50*time.Millisecond)
	return s.fakeSource.Sessions(ctx)
}

// The window-name lookup has its own timeout: a slow Source does not
// leave it an expired or nearly expired context.
func TestWindowNamesHaveOwnTimeout(t *testing.T) {
	tm := &slowTmux{}
	src := &slowSource{fakeSource{sessions: threeSessions()[:1]}}
	m := New(src, tm, config.Default(), func() time.Time { return testNow }, ModePopup)
	_ = refresh(t, m)
	if tm.deadline < 500*time.Millisecond {
		t.Errorf("ListPanes got %v of its timeout, want its own (about %v)", tm.deadline, tmuxTimeout)
	}
}

// decSessions are sessions S1, S2 and S3, in that order, each in a pane.
func decSessions() []model.Session {
	var out []model.Session
	for i, id := range []string{"S1", "S2", "S3"} {
		out = append(out, model.Session{
			ID: id, Pane: "%" + strconv.Itoa(i+1), State: model.StateIdle, Model: "model-" + id,
			UpdatedAt: testNow,
		})
	}
	return out
}

// decCounts: S1 has one blocking and one assume ticket open, S2 two assume
// tickets, S3 none.
func decCounts() map[string]model.OpenCount {
	return map[string]model.OpenCount{"S1": {Blocking: 1, Assume: 1}, "S2": {Assume: 2}}
}

// newDecTest returns a model over decSessions with counts after its first
// refresh, with open set as its decide factory when it is not nil.
func newDecTest(t *testing.T, mode Mode, counts map[string]model.OpenCount, open func(string) tea.Model) (tea.Model, *fakeSource) {
	t.Helper()
	src := &fakeSource{sessions: decSessions(), counts: counts}
	m := New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, mode)
	if open != nil {
		m = m.WithDecide(open)
	}
	return refresh(t, m), src
}

// decCell is the DEC cell of the row of session id: the text under the
// header's DEC title, which is as wide as the column.
func decCell(t *testing.T, view, id string) string {
	t.Helper()
	lines := strings.Split(view, "\n")
	byteCol := strings.Index(lines[0], "DEC")
	i := lineWith(view, "model-"+id)
	if byteCol < 0 || i < 0 {
		t.Fatalf("no DEC column or no row %s:\n%s", id, view)
	}
	// Every character drawn here (the icons, an ellipsis) is one cell.
	col := utf8.RuneCountInString(lines[0][:byteCol])
	row := []rune(lines[i])
	return strings.TrimSpace(string(row[col : col+len("DEC")]))
}

func TestDecColumnShowsOpenCount(t *testing.T) {
	m, _ := newDecTest(t, ModePopup, decCounts(), nil)
	view := m.View()
	for id, want := range map[string]string{"S1": "2", "S2": "2", "S3": ""} {
		if got := decCell(t, view, id); got != want {
			t.Errorf("%s's DEC cell = %q, want %q:\n%s", id, got, want, view)
		}
	}
	if got := decStyle(decCounts()["S1"]).GetForeground(); got != lipgloss.Color("1") {
		t.Errorf("S1's DEC color = %v, want red", got)
	}
	if got := decStyle(decCounts()["S2"]).GetForeground(); got != (lipgloss.NoColor{}) {
		t.Errorf("S2's DEC color = %v, want plain", got)
	}
}

func TestSidebarHasNoDecColumn(t *testing.T) {
	m, src := newDecTest(t, ModeSidebar, decCounts(), nil)
	if strings.Contains(m.View(), "DEC") {
		t.Errorf("sidebar shows DEC:\n%s", m.View())
	}
	if src.countRead != 0 {
		t.Errorf("sidebar read OpenDecisions %d times, want never", src.countRead)
	}
	if strings.Contains(statusLine(m), "d decisions") {
		t.Errorf("sidebar help = %q, want no d key", statusLine(m))
	}
}

func TestPopupHelpNamesDKey(t *testing.T) {
	m, src := newDecTest(t, ModePopup, decCounts(), nil)
	if s := statusLine(m); !strings.Contains(s, "d decisions") {
		t.Errorf("popup help = %q, want d decisions", s)
	}
	if src.countRead != 1 {
		t.Errorf("popup refresh read OpenDecisions %d times, want 1", src.countRead)
	}
}

// fakeChild stands in for the decide view (or, with board set, the task
// board; with closeWith set, the review view): it records every message it
// gets, starts with a childPing and closes on "q" with DecideClosedMsg
// (BoardClosedMsg for a board, closeWith when set).
type fakeChild struct {
	id        string
	got       *[]tea.Msg
	board     bool
	closeWith tea.Msg
}

// childPing is the message a fakeChild's Init sends.
type childPing struct{ id string }

func (c fakeChild) Init() tea.Cmd { return func() tea.Msg { return childPing{c.id} } }

func (c fakeChild) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	*c.got = append(*c.got, msg)
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "q" {
		if c.closeWith != nil {
			return c, func() tea.Msg { return c.closeWith }
		}
		if c.board {
			return c, func() tea.Msg { return BoardClosedMsg{} }
		}
		return c, func() tea.Msg { return DecideClosedMsg{} }
	}
	return c, nil
}

func (c fakeChild) View() string {
	if c.closeWith != nil {
		return "review view of " + c.id
	}
	if c.board {
		return "board view " + c.id
	}
	return "decide view of " + c.id
}

// childFactory returns a decide factory of fakeChilds, the ids it was
// called with and the messages the children got.
func childFactory() (func(string) tea.Model, *[]string, *[]tea.Msg) {
	var opened []string
	var got []tea.Msg
	return func(id string) tea.Model {
		opened = append(opened, id)
		return fakeChild{id: id, got: &got}
	}, &opened, &got
}

// feed runs cmd and feeds the messages it gives (each of a batch) to m,
// without running the commands they return.
func feed(m tea.Model, cmd tea.Cmd) tea.Model {
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			m = feed(m, c)
		}
	case nil:
	default:
		m, _ = m.Update(msg)
	}
	return m
}

func TestDKeyOpensDecideForRow(t *testing.T) {
	open, opened, got := childFactory()
	m, _ := newDecTest(t, ModePopup, decCounts(), open)
	m, cmd := press(m, "d")
	if !slices.Equal(*opened, []string{"S1"}) {
		t.Fatalf("factory called with %q, want [S1]", *opened)
	}
	if v := m.View(); v != "decide view of S1" {
		t.Errorf("view = %q, want the child's", v)
	}
	m = feed(m, cmd) // the child's Init
	m, _ = press(m, "x", "j")
	want := []tea.Msg{childPing{"S1"}, key("x"), key("j")}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("child got %#v, want %#v", *got, want)
	}
	if v := m.View(); v != "decide view of S1" {
		t.Errorf("after keys, view = %q, want the child's", v)
	}
}

// The child gets the dashboard's window size when it opens, and every
// resize after.
func TestDecideGetsWindowSize(t *testing.T) {
	open, _, got := childFactory()
	m, _ := newDecTest(t, ModePopup, decCounts(), open)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	m, _ = press(m, "d")
	m, _ = m.Update(tea.WindowSizeMsg{Width: 50, Height: 10})
	want := []tea.Msg{tea.WindowSizeMsg{Width: 70, Height: 20}, tea.WindowSizeMsg{Width: 50, Height: 10}}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("child got %#v, want %#v", *got, want)
	}
	m, cmd := press(m, "q")
	m = feed(m, cmd)
	for _, l := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(l); w > 50 {
			t.Errorf("dashboard after close is %d cells wide, want the new width 50: %q", w, l)
		}
	}
}

func TestDecideClosedReturnsToDashboard(t *testing.T) {
	open, _, _ := childFactory()
	m, src := newDecTest(t, ModePopup, decCounts(), open)
	m, _ = press(m, "d")
	_, cmd := press(m, "q")
	if cmd == nil {
		t.Fatal("q in the child sent nothing")
	}
	reads := src.countRead
	m, refreshCmd := m.Update(cmd()) // the child's DecideClosedMsg
	if !strings.Contains(m.View(), "model-S1") {
		t.Errorf("view after close is not the dashboard:\n%s", m.View())
	}
	if refreshCmd == nil {
		t.Fatal("close started no refresh")
	}
	if _, ok := refreshCmd().(refreshMsg); !ok || src.countRead != reads+1 {
		t.Errorf("close's command is not a refresh (OpenDecisions reads %d, want %d)", src.countRead, reads+1)
	}
	// A bare DecideClosedMsg closes too.
	m, _ = press(m, "d")
	if m, _ = m.Update(DecideClosedMsg{}); !strings.Contains(m.View(), "model-S1") {
		t.Errorf("view after a bare close is not the dashboard:\n%s", m.View())
	}
}

func TestDKeyWithoutOpenDecisions(t *testing.T) {
	open, opened, _ := childFactory()
	m, _ := newDecTest(t, ModePopup, decCounts(), open)
	m, _ = press(m, "j", "j", "d") // S3
	if s := statusLine(m); s != "no open decisions" {
		t.Errorf("status line = %q, want no open decisions", s)
	}
	if len(*opened) != 0 {
		t.Errorf("factory called with %q, want never", *opened)
	}
}

// Without a factory (curia ui inside an agent session) the dashboard still
// works, and d says why it cannot answer.
func TestDKeyWithoutFactoryExplains(t *testing.T) {
	m, _ := newDecTest(t, ModePopup, decCounts(), nil)
	m, _ = press(m, "d")
	if s := statusLine(m); !strings.Contains(s, "agent session") {
		t.Errorf("status line = %q, want the reason d cannot answer", s)
	}
	if !strings.Contains(m.View(), "model-S1") {
		t.Errorf("view is not the dashboard:\n%s", m.View())
	}
}

func TestDKeyDisabledInSidebar(t *testing.T) {
	m, _ := newDecTest(t, ModeSidebar, decCounts(), nil)
	before := m.View()
	m, cmd := press(m, "d")
	if cmd != nil || m.View() != before {
		t.Errorf("d in the sidebar: command %v, view:\n%s\nwant nothing to change from:\n%s", cmd != nil, m.View(), before)
	}
}

func TestNarrowWidthStillFitsWithDec(t *testing.T) {
	sessions := decSessions()
	sessions[0].LastUpdate = strings.Repeat("a very long last update ", 10)
	sessions[1].Repo = "a-repository-with-a-really-long-name"
	sessions[0].Repo = "repo-S1" // the MODEL column is not shown at 60
	src := &fakeSource{sessions: sessions, counts: map[string]model.OpenCount{"S1": {Blocking: 12, Assume: 3}}}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, ModePopup))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	view := m.View()
	for _, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w > 60 {
			t.Errorf("line is %d cells, want at most 60: %q", w, l)
		}
	}
	if i := lineWith(view, "repo-S1"); i < 0 || !strings.Contains(strings.Split(view, "\n")[i], "  15  ") {
		t.Errorf("S1's DEC cell at width 60 is not 15:\n%s", view)
	}
}

func TestOpenDecisionsErrorKeepsLastCounts(t *testing.T) {
	m, src := newDecTest(t, ModePopup, decCounts(), nil)
	src.countsErr = errors.New("database is locked")
	src.counts = nil
	m = refresh(t, m)
	if got := decCell(t, m.View(), "S1"); got != "2" {
		t.Errorf("S1's DEC cell after a failed read = %q, want the last good 2", got)
	}
	if s := statusLine(m); !strings.Contains(s, "refresh failed: database is locked") {
		t.Errorf("status line = %q, want the error", s)
	}
	src.countsErr = nil
	m = refresh(t, m)
	if got := decCell(t, m.View(), "S1"); got != "" {
		t.Errorf("S1's DEC cell after a good empty read = %q, want empty", got)
	}
	if s := statusLine(m); strings.Contains(s, "database is locked") {
		t.Errorf("status line = %q after a good refresh, want the error gone", s)
	}
}

// Messages of a decide view that has closed never reach the next one, and
// a closed view's tick chain ends.
func TestClosedDecideMessagesDropped(t *testing.T) {
	open, opened, got := childFactory()
	m, _ := newDecTest(t, ModePopup, decCounts(), open)
	m, first := press(m, "d")
	m, closeCmd := press(m, "q")
	m = feed(m, closeCmd)
	m, second := press(m, "j", "d") // S2
	if !slices.Equal(*opened, []string{"S1", "S2"}) {
		t.Fatalf("factory called with %q, want [S1 S2]", *opened)
	}
	*got = nil
	m = feed(m, first) // the first child's Init arrives late
	if len(*got) != 0 {
		t.Errorf("second child got the first child's message: %#v", *got)
	}
	m = feed(m, second)
	if want := []tea.Msg{childPing{"S2"}}; !reflect.DeepEqual(*got, want) {
		t.Errorf("second child got %#v, want %#v", *got, want)
	}
	m, closeCmd = press(m, "q")
	m = feed(m, closeCmd)
	if _, cmd := m.Update(first()); cmd != nil {
		t.Error("a closed child's message started a command")
	}
}

// The dashboard's own refresh chain stops while the decide view is shown
// and restarts once when it closes: a tick from before is dropped.
func TestDashboardTickChainNotDoubled(t *testing.T) {
	open, _, got := childFactory()
	m, _ := newDecTest(t, ModePopup, decCounts(), open)
	m, _ = press(m, "d")
	m, cmd := m.Update(tickMsg{}) // the tick scheduled before d
	if cmd != nil {
		t.Error("a dashboard tick while the decide view is shown started a command")
	}
	if len(*got) != 1 || (*got)[0] != (tickMsg{}) {
		t.Errorf("child got %#v, want the tick (every message goes to it)", *got)
	}
	m, closeCmd := press(m, "q")
	m, restart := m.Update(closeCmd())
	if restart == nil {
		t.Fatal("close started no refresh")
	}
	if _, cmd := m.Update(tickMsg{}); cmd != nil {
		t.Error("a tick of the old chain started a refresh after close")
	}
	if _, next := m.Update(restart()); next == nil {
		t.Fatal("the new chain's refresh scheduled no tick")
	}
}

// embedSource is a DecideSource with one ticket of S1 that records answers.
type embedSource struct {
	tickets []model.Ticket
	answers []model.Answer
}

func (s *embedSource) Tickets(context.Context) ([]model.Ticket, error) {
	return slices.Clone(s.tickets), nil
}

func (s *embedSource) Answer(_ context.Context, _ string, a model.Answer) (model.Outcome, error) {
	s.answers = append(s.answers, a)
	d := s.tickets[0].Decision
	s.tickets = nil
	d.Status, d.AnswerKey = model.StatusOverridden, a.Key
	return model.Outcome{Decision: d}, nil
}

// The real decide view, embedded: its refresh, tick and answer messages
// reach it, its dwell still refuses a key pressed too soon, and its tick
// chain ends when it closes.
func TestEmbeddedDecideKeepsItsGates(t *testing.T) {
	clk := testNow
	now := func() time.Time { return clk }
	dsrc := &embedSource{tickets: []model.Ticket{{Decision: model.Decision{
		ID: "D-1", SessionID: "S1", Question: "Which queue?", Mode: model.ModeAssume,
		Reversibility: model.ReversCheap, Recommendation: "A", Assumption: "Redis",
		Options: []model.Option{{Key: "A", Label: "Postgres"}, {Key: "B", Label: "Redis"}},
		Status:  model.StatusOpen,
	}}}}
	open := func(id string) tea.Model {
		return NewDecide(dsrc, now, DecideOptions{SessionID: id, Embedded: true, Via: model.AnsweredPopup})
	}
	src := &fakeSource{sessions: decSessions(), counts: decCounts()}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), now, ModePopup).WithDecide(open))
	m, initCmd := press(m, "d")
	msg := initCmd()
	m, tick := m.Update(msg) // the decide view's first read
	if !strings.Contains(m.View(), "Which queue?") {
		t.Fatalf("decide view did not get its refresh:\n%s", m.View())
	}
	if tick == nil {
		t.Fatal("the decide view's refresh scheduled no tick")
	}

	m, cmd := press(m, "2") // at once: inside the dwell
	if cmd != nil || len(dsrc.answers) != 0 {
		t.Fatalf("an answer key inside the dwell answered: %v", dsrc.answers)
	}
	clk = clk.Add(time.Second)
	m, cmd = press(m, "2")
	if cmd == nil {
		t.Fatalf("an answer key after the dwell sent nothing:\n%s", m.View())
	}
	m, _ = m.Update(cmd()) // the answer and its result
	if len(dsrc.answers) != 1 || dsrc.answers[0].Key != "B" || dsrc.answers[0].Via != model.AnsweredPopup {
		t.Errorf("answers = %+v, want one B via popup", dsrc.answers)
	}
	if !strings.Contains(m.View(), "D-1 overridden") { // the stored status word (M2)
		t.Errorf("decide view did not get its answer result:\n%s", m.View())
	}

	// The tick message (what the timer sends) reaches the view: it re-reads.
	dm, ok := m.(Model)
	if !ok {
		t.Fatalf("Update returned a %T, want Model", m)
	}
	gen := dm.childGen
	m, next := m.Update(childMsg{gen: gen, msg: decideTickMsg{}})
	if next == nil {
		t.Error("the decide view's tick started no refresh")
	}
	m, closeCmd := press(m, "q")
	m, _ = m.Update(closeCmd())
	if !strings.Contains(m.View(), "model-S1") {
		t.Fatalf("view after close is not the dashboard:\n%s", m.View())
	}
	if _, cmd := m.Update(msg); cmd != nil {
		t.Error("a closed decide view's message started a command")
	}
	if _, cmd := m.Update(childMsg{gen: 1, msg: decideTickMsg{}}); cmd != nil {
		t.Error("a closed decide view's tick started a refresh: its chain did not end")
	}
}

// Keys typed in one burst after a d that opens the decide view are dropped:
// the user has not seen the view, and the dashboard must not act on them.
func TestDKeyInBurstDropsTheRest(t *testing.T) {
	open, opened, got := childFactory()
	m, _ := newDecTest(t, ModePopup, decCounts(), open)
	m, cmd := m.Update(key("dq"))
	if !slices.Equal(*opened, []string{"S1"}) {
		t.Fatalf("factory called with %q, want [S1]", *opened)
	}
	if isQuit(cmd) || m.View() != "decide view of S1" {
		t.Errorf("q after d in a burst: quit %v, view %q", isQuit(cmd), m.View())
	}
	if len(*got) != 0 {
		t.Errorf("child got %#v from the burst, want nothing", *got)
	}
}

// Ruling 89, defect 8: the DEC cell counts the answers a session has not
// got yet apart from its open tickets.
func TestDefect8DecCellShowsUndelivered(t *testing.T) {
	src := &fakeSource{sessions: decSessions(), counts: map[string]model.OpenCount{
		"S1": {Blocking: 1, Undelivered: 1}, "S2": {Undelivered: 2},
	}}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, ModePopup))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	view := m.View()
	lines := strings.Split(view, "\n")
	for id, want := range map[string]string{"S1": "  1 Q1  ", "S2": "  Q2  "} {
		if i := lineWith(view, "model-"+id); i < 0 || !strings.Contains(lines[i], want) {
			t.Errorf("%s's row lacks the DEC cell %q:\n%s", id, strings.TrimSpace(want), view)
		}
	}
}

// taskTasks are the tasks the dashboard tests' source knows.
func taskTasks() map[string]model.Task {
	return map[string]model.Task{"T-42": {ID: "T-42", Title: "auth refactor"}}
}

// rowOf is the line of view containing sub.
func rowOf(t *testing.T, view, sub string) string {
	t.Helper()
	i := lineWith(view, sub)
	if i < 0 {
		t.Fatalf("no row %q:\n%s", sub, view)
	}
	return strings.Split(view, "\n")[i]
}

// titlesMatch reports whether header fields got are want, each title
// possibly cut short with an ellipsis to fit.
func titlesMatch(got, want []string) bool {
	return slices.EqualFunc(got, want, func(g, w string) bool {
		return g == w || (strings.HasSuffix(g, ellipsis) && strings.HasPrefix(w, strings.TrimSuffix(g, ellipsis)))
	})
}

func TestTaskColumnShowsIdAndTitle(t *testing.T) {
	s := threeSessions()
	s[0].TaskID = "T-42" // known
	s[1].TaskID = "T-7"  // unknown
	src := &fakeSource{sessions: s, tasks: taskTasks()}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, ModePopup))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	view := m.View()
	if r := rowOf(t, view, "claude-opus-5-5"); !strings.Contains(r, "  T-42 auth refactor  ") {
		t.Errorf("known task row = %q, want T-42 auth refactor", r)
	}
	if r := rowOf(t, view, "claude-sonnet-4-5"); !regexp.MustCompile(`  T-7 +claude-sonnet`).MatchString(r) {
		t.Errorf("unknown task row = %q, want T-7 alone", r)
	}
	if r := rowOf(t, view, "claude-haiku-4-5"); !regexp.MustCompile(`  notes +claude-haiku`).MatchString(r) {
		t.Errorf("row without a task = %q, want an empty TASK cell", r)
	}
	// Below 60 columns the cell is the id alone.
	m, _ = m.Update(tea.WindowSizeMsg{Width: 50, Height: 20})
	view = m.View()
	if r := rowOf(t, view, "work:@1"); !strings.Contains(r, "T-42") || strings.Contains(view, "auth refactor") {
		t.Errorf("at 50 columns the TASK cell is not the id alone:\n%s", view)
	}
}

// widthSessions are two sessions with a known task, in source order.
func widthSessions() []model.Session {
	s := threeSessions()[:2]
	s[0].TaskID = "T-42"
	return s
}

func TestColumnsDropAsWidthShrinks(t *testing.T) {
	for _, tc := range []struct {
		width int
		want  []string
	}{
		{130, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "MODEL", "CTX", "SUB", "DEC", "COST", "CACHE", "LAST", "UPDATE", "AGE"}},
		{120, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "MODEL", "CTX", "SUB", "DEC", "COST", "CACHE", "LAST", "UPDATE", "AGE"}},
		{119, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "CTX", "DEC", "COST", "CACHE", "LAST", "UPDATE", "AGE"}},
		{100, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "CTX", "DEC", "COST", "CACHE", "LAST", "UPDATE", "AGE"}},
		{99, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "CTX", "DEC", "LAST", "UPDATE", "AGE"}},
		{90, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "CTX", "DEC", "LAST", "UPDATE", "AGE"}},
		{70, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "CTX", "DEC", "AGE"}},
		{60, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "CTX", "DEC", "AGE"}},
		{50, []string{"SESSION:WINDOW", "TASK", "DEC", "AGE"}},
	} {
		src := &fakeSource{sessions: widthSessions(), tasks: taskTasks()}
		fake := &tmux.Fake{Panes: []tmux.Pane{
			{ID: "%3", Session: "work", WindowID: "@1", WindowName: "editor"},
			{ID: "%5", Session: "work", WindowID: "@2", WindowName: "server"},
		}}
		m := refresh(t, New(src, fake, config.Default(), func() time.Time { return testNow }, ModePopup))
		m, _ = m.Update(tea.WindowSizeMsg{Width: tc.width, Height: 20})
		view := m.View()
		lines := strings.Split(view, "\n")
		if h := strings.Fields(lines[0]); !titlesMatch(h, tc.want) {
			t.Errorf("width %d: header = %q, want %q", tc.width, h, tc.want)
		}
		for _, l := range lines {
			if w := lipgloss.Width(l); w > tc.width {
				t.Errorf("width %d: line is %d cells: %q", tc.width, w, l)
			}
		}
		if lines[1] == lines[2] {
			t.Errorf("width %d: the two rows are drawn the same: %q", tc.width, lines[1])
		}
	}
}

// teamSessions are a solo session (solo, working) and a team: lead L
// (working, task T-42) with teammates alpha (needs a decision), beta
// (working) and gamma (idle, unseen), ranked as the store source ranks.
func teamSessions() []model.Session {
	mate := func(id, member, pane string, state model.State, ago time.Duration) model.Session {
		return model.Session{
			ID: id, Pane: pane, TmuxSession: "work", TmuxWindow: "@2", Repo: "api", Branch: "main",
			State: state, Model: "model-" + id, UpdatedAt: testNow.Add(-ago), LastUpdate: "update " + id,
			TaskID: "T-42", TeamID: "team-1", TeamRole: model.RoleTeammate, TeamMember: member,
		}
	}
	lead := mate("L", "team-lead", "%10", model.StateWorking, 3*time.Minute)
	lead.TeamRole = model.RoleLead
	return fleet.Rank([]model.Session{
		{
			ID: "solo", Pane: "%3", TmuxSession: "work", TmuxWindow: "@1", Repo: "curia", Branch: "main",
			State: model.StateWorking, Model: "model-solo", UpdatedAt: testNow.Add(-time.Minute),
		},
		lead,
		mate("A", "alpha", "%11", model.StateNeedsDecision, 4*time.Minute),
		mate("B", "beta", "%12", model.StateWorking, 2*time.Minute),
		mate("C", "gamma", "%13", model.StateIdle, 30*time.Second),
	})
}

// teamCounts: A has a blocking ticket, L an assume ticket, B an answer
// not yet delivered.
func teamCounts() map[string]model.OpenCount {
	return map[string]model.OpenCount{"A": {Blocking: 1}, "L": {Assume: 1}, "B": {Undelivered: 1}}
}

// newTeamTest returns the popup over teamSessions after its first refresh,
// 200 columns wide.
func newTeamTest(t *testing.T, mode Mode) (tea.Model, *tmux.Fake, *fakeSource) {
	t.Helper()
	fake := &tmux.Fake{Panes: []tmux.Pane{
		{ID: "%3", Session: "work", WindowID: "@1", WindowName: "editor"},
		{ID: "%10", Session: "work", WindowID: "@2", WindowName: "server"},
	}}
	src := &fakeSource{sessions: teamSessions(), counts: teamCounts(), tasks: taskTasks()}
	m := refresh(t, New(src, fake, config.Default(), func() time.Time { return testNow }, mode))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 20})
	return m, fake, src
}

// bodyLines are the view's lines between the header and the status line
// that are not blank.
func bodyLines(m tea.Model) []string {
	lines := strings.Split(m.View(), "\n")
	var out []string
	for _, l := range lines[1 : len(lines)-1] {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestTeamShowsAsOneCollapsedRow(t *testing.T) {
	m, _, _ := newTeamTest(t, ModePopup)
	rows := bodyLines(m)
	if len(rows) != 2 {
		t.Fatalf("rows = %q, want the team's group row and the solo row", rows)
	}
	group := rows[0]
	for _, want := range []string{stateIcon(model.StateNeedsDecision), "▸ work:server +3", "T-42 auth refactor", "model-L", "update L"} {
		if !strings.Contains(group, want) {
			t.Errorf("group row %q lacks %q", group, want)
		}
	}
	if !strings.Contains(group, "  2 Q1  ") {
		t.Errorf("group row %q, want the members' DEC sum 2 Q1", group)
	}
	for _, absent := range []string{"alpha", "beta", "gamma", "model-A"} {
		if strings.Contains(m.View(), absent) {
			t.Errorf("collapsed team shows %q:\n%s", absent, m.View())
		}
	}
	if s := statusLine(m); !strings.HasPrefix(s, "5 sessions") {
		t.Errorf("status line = %q, want 5 sessions counted", s)
	}
}

func TestSpaceExpandsAndCollapsesTeam(t *testing.T) {
	m, _, src := newTeamTest(t, ModePopup)
	m, _ = press(m, " ")
	rows := bodyLines(m)
	want := []string{"▾ work:server", "  alpha", "  gamma", "  beta", "work:editor"}
	if len(rows) != len(want) {
		t.Fatalf("expanded rows = %q, want %q", rows, want)
	}
	for i, w := range want {
		if !strings.Contains(rows[i], w) {
			t.Errorf("row %d = %q, want %q", i, rows[i], w)
		}
	}
	if !strings.Contains(rows[1], "model-A") || !strings.Contains(rows[1], stateIcon(model.StateNeedsDecision)) {
		t.Errorf("teammate row %q does not show alpha's own cells", rows[1])
	}
	// Kept across a refresh, even when the members change places.
	src.sessions = slices.Concat(src.sessions[1:], src.sessions[:1])
	m = refresh(t, m)
	if rows := bodyLines(m); len(rows) != 5 {
		t.Errorf("after a refresh rows = %q, want the team still expanded", rows)
	}
	// Space on the lead row collapses; the selection stays on the team.
	m, _ = press(m, " ")
	if rows := bodyLines(m); len(rows) != 2 || !strings.Contains(rows[0], "▸ work:server +3") {
		t.Errorf("after a second space rows = %q, want one group row", rows)
	}
	// Space on a teammate row does nothing; on a solo row neither.
	m, _ = press(m, " ", "j", "j")
	before := m.View()
	m, _ = press(m, "k", " ")
	if rows := bodyLines(m); len(rows) != 5 {
		t.Errorf("space on a teammate row changed the rows: %q\nbefore:\n%s", rows, before)
	}
	m, _ = press(m, "j", "j", "j", " ")
	if rows := bodyLines(m); len(rows) != 5 {
		t.Errorf("space on the solo row changed the rows: %q", rows)
	}
}

func TestTeamRanksByWorstMember(t *testing.T) {
	waiting := model.Session{ID: "W", Pane: "%20", State: model.StateWaiting, Model: "model-W", UpdatedAt: testNow}
	sessions := teamSessions()
	src := &fakeSource{sessions: fleet.Rank(append(sessions, waiting))}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, ModePopup))
	if rows := bodyLines(m); len(rows) != 3 || !strings.Contains(rows[0], "▸") || !strings.Contains(rows[1], "model-W") {
		t.Errorf("with alpha needing a decision, rows = %q, want the team before the waiting session", rows)
	}
	// alpha is now working: the team's worst is working, after waiting.
	for i := range sessions {
		if sessions[i].ID == "A" {
			sessions[i].State = model.StateWorking
		}
	}
	src.sessions = fleet.Rank(append(sessions, waiting))
	m = refresh(t, m)
	rows := bodyLines(m)
	if len(rows) != 3 || !strings.Contains(rows[0], "model-W") || !strings.Contains(rows[1], "▸") {
		t.Errorf("with no member needing a decision, rows = %q, want the waiting session first", rows)
	}
	if !strings.Contains(rows[1], stateIcon(model.StateIdle)) {
		t.Errorf("group row %q, want gamma's idle icon (its worst member)", rows[1])
	}
}

func TestEnterOnGroupJumpsToWorstMember(t *testing.T) {
	m, fake, _ := newTeamTest(t, ModePopup)
	_, cmd := press(m, "enter")
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %11"}) {
		t.Errorf("SwitchTo calls = %q, want alpha's pane %%11", got)
	}
	if !isQuit(cmd) {
		t.Error("popup did not quit after the jump")
	}
	// On the expanded lead row Enter jumps to the lead.
	m, fake, _ = newTeamTest(t, ModePopup)
	_, _ = press(m, " ", "enter")
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %10"}) {
		t.Errorf("SwitchTo calls = %q, want the lead's pane %%10", got)
	}
}

func TestKillRefusedOnGroupRow(t *testing.T) {
	m, fake, _ := newTeamTest(t, ModePopup)
	m, _ = press(m, "x")
	if s := statusLine(m); s != "expand the team to kill one pane" {
		t.Errorf("status line = %q, want the refusal", s)
	}
	_, _ = press(m, "y")
	if got := calls(fake, "KillPane"); len(got) != 0 {
		t.Errorf("killed from a group row: %q", got)
	}
	// Expanded, x on a teammate asks for that pane.
	m, fake, _ = newTeamTest(t, ModePopup)
	_, _ = press(m, " ", "j", "x", "y")
	if got := calls(fake, "KillPane"); !slices.Equal(got, []string{"KillPane %11"}) {
		t.Errorf("KillPane calls = %q, want alpha's pane", got)
	}
}

func TestFilterExpandsMatchingTeam(t *testing.T) {
	m, _, _ := newTeamTest(t, ModePopup)
	m, _ = press(m, "/", "update C")
	rows := bodyLines(m)
	if len(rows) != 4 || !strings.Contains(rows[0], "▾ work:server") {
		t.Fatalf("filtered rows = %q, want the team expanded and the solo row gone", rows)
	}
	m, _, _ = newTeamTest(t, ModePopup)
	m, _ = press(m, "/", "gamm")
	if rows := bodyLines(m); len(rows) != 4 {
		t.Errorf("a member name match: rows = %q, want the team expanded", rows)
	}
	m, _ = press(m, "esc") // clears the filter: collapsed again
	if rows := bodyLines(m); len(rows) != 2 {
		t.Errorf("after the filter is cleared rows = %q, want the team collapsed", rows)
	}
}

func TestSoloRowsUnchanged(t *testing.T) {
	m, _, _ := newTest(t, ModePopup, threeSessions()...)
	rows := bodyLines(m)
	if len(rows) != 3 {
		t.Fatalf("rows = %q, want one per session", rows)
	}
	for i, id := range []string{"claude-opus-5-5", "claude-sonnet-4-5", "claude-haiku-4-5"} {
		if !strings.Contains(rows[i], id) || strings.ContainsAny(rows[i], "▸▾") {
			t.Errorf("row %d = %q, want %s in Source order and no team mark", i, rows[i], id)
		}
	}
	// A team member whose lead is not among the sessions is solo too.
	s := threeSessions()
	s[0].TeamID, s[0].TeamRole, s[0].TeamMember = "team-9", model.RoleTeammate, "orphan"
	m, _, _ = newTest(t, ModePopup, s...)
	if rows := bodyLines(m); len(rows) != 3 || strings.ContainsAny(m.View(), "▸▾") {
		t.Errorf("a teammate without its lead: rows = %q", rows)
	}
}

// boardFactory returns a board factory of fakeChilds, how often it was
// called and the messages the children got.
func boardFactory() (func() tea.Model, *int, *[]tea.Msg) {
	n := 0
	var got []tea.Msg
	return func() tea.Model {
		n++
		return fakeChild{id: strconv.Itoa(n), got: &got, board: true}
	}, &n, &got
}

// withBoard returns m, a Model, WithBoard(open).
func withBoard(t *testing.T, m tea.Model, open func() tea.Model) tea.Model {
	t.Helper()
	dm, ok := m.(Model)
	if !ok {
		t.Fatalf("model is a %T, want Model", m)
	}
	return dm.WithBoard(open)
}

func TestTKeyOpensBoardAndClosesBack(t *testing.T) {
	open, opened, got := boardFactory()
	m, _, src := newTest(t, ModePopup, threeSessions()...)
	m = withBoard(t, m, open)
	if s := statusLine(m); !strings.Contains(s, "t tasks") {
		t.Errorf("help = %q, want t tasks", s)
	}
	m, first := press(m, "t")
	if *opened != 1 || m.View() != "board view 1" {
		t.Fatalf("t: factory calls %d, view %q", *opened, m.View())
	}
	m = feed(m, first) // the board's Init
	m, _ = press(m, "x", "j", "enter")
	want := []tea.Msg{childPing{"1"}, key("x"), key("j"), key("enter")}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("board got %#v, want %#v", *got, want)
	}
	reads := src.countRead
	m, closeCmd := press(m, "q")
	m, restart := m.Update(closeCmd())
	if !strings.Contains(m.View(), "claude-opus-5-5") {
		t.Fatalf("view after BoardClosedMsg is not the table:\n%s", m.View())
	}
	if restart == nil {
		t.Fatal("close started no refresh")
	}
	if _, ok := restart().(refreshMsg); !ok || src.countRead != reads+1 {
		t.Error("close's command is not a refresh")
	}
	// A late message of the closed board is dropped, even with a new one open.
	m, _ = press(m, "t")
	*got = nil
	if _, cmd := m.Update(first()); cmd != nil || len(*got) != 0 {
		t.Errorf("the closed board's message reached the new one: %#v", *got)
	}
	// A t in a burst opens the board and drops the rest.
	m, _, _ = newTest(t, ModePopup, threeSessions()...)
	m = withBoard(t, m, open)
	m, cmd := m.Update(key("tq"))
	if isQuit(cmd) || m.View() != "board view 3" {
		t.Errorf("q after t in a burst: quit %v, view %q", isQuit(cmd), m.View())
	}
}

// dashBoardSource is a BoardSource with one backlog task; Spawn records
// its calls.
type dashBoardSource struct{ spawns []string }

func (*dashBoardSource) Cards(context.Context) ([]task.Card, error) {
	return []task.Card{{Task: model.Task{ID: "T-1", Title: "first", Status: model.TaskBacklog}, Status: model.TaskBacklog}}, nil
}

func (*dashBoardSource) SetStatus(context.Context, string, model.TaskStatus) error { return nil }

func (s *dashBoardSource) Spawn(_ context.Context, id string) (string, error) {
	s.spawns = append(s.spawns, id)
	return id + " launched", nil
}

// newBoardDash returns the popup over threeSessions with a real board
// over bsrc, on clock clk.
func newBoardDash(t *testing.T, bsrc *dashBoardSource, clk *time.Time) tea.Model {
	t.Helper()
	now := func() time.Time { return *clk }
	m := New(&fakeSource{sessions: threeSessions()}, &tmux.Fake{}, config.Default(), now, ModePopup)
	m = m.WithBoard(func() tea.Model { return NewBoard(bsrc, &tmux.Fake{}, now, true) })
	return refresh(t, m)
}

func TestNKeyOpensBoardWithNotice(t *testing.T) {
	clk := testNow
	m := newBoardDash(t, &dashBoardSource{}, &clk)
	m, cmd := press(m, "n")
	m = feed(m, cmd) // the board's first read
	if !strings.Contains(m.View(), "first") {
		t.Fatalf("n did not open the board:\n%s", m.View())
	}
	if s := statusLine(m); s != "pick a task, then n" {
		t.Errorf("status line = %q, want the notice", s)
	}
	clk = clk.Add(time.Second)
	m, _ = press(m, "j")
	if s := statusLine(m); s == "pick a task, then n" {
		t.Error("the notice outlived the next key")
	}
}

// The board's spawn runs off the update loop; while it is in flight the
// board says so and refuses another spawn.
func TestBoardSpawnInFlightRefusesSecond(t *testing.T) {
	clk := testNow
	bsrc := &dashBoardSource{}
	m := newBoardDash(t, bsrc, &clk)
	m, cmd := press(m, "t")
	m = feed(m, cmd)
	clk = clk.Add(time.Second)
	m, _ = press(m, "n")
	clk = clk.Add(time.Second)
	m, spawn := press(m, "y")
	if spawn == nil {
		t.Fatal("y started no spawn")
	}
	if len(bsrc.spawns) != 0 {
		t.Fatal("the spawn ran on the update loop")
	}
	if s := statusLine(m); !strings.Contains(s, "spawning T-1") {
		t.Errorf("status line while spawning = %q, want progress", s)
	}
	clk = clk.Add(time.Second)
	m, again := press(m, "n")
	clk = clk.Add(time.Second)
	m, yes := press(m, "y")
	if again != nil || yes != nil {
		t.Error("a second n, y while spawning started something")
	}
	if s := statusLine(m); s == "" {
		t.Error("no status line")
	}
	m = feed(m, spawn)
	if !slices.Equal(bsrc.spawns, []string{"T-1"}) {
		t.Errorf("spawns = %q, want one", bsrc.spawns)
	}
	if s := statusLine(m); s != "T-1 launched" {
		t.Errorf("status line after the spawn = %q, want its line", s)
	}
}

func TestTKeyWithoutBoard(t *testing.T) {
	for _, mode := range []Mode{ModeSidebar, ModePopup} {
		m, _, _ := newTest(t, mode, threeSessions()...)
		for _, k := range []string{"t", "n"} {
			m2, cmd := press(m, k)
			if cmd != nil || strings.Contains(m2.View(), "BACKLOG") {
				t.Errorf("mode %v: %s without a board opened something", mode, k)
			}
			if s := statusLine(m2); s != noticeNoBoard {
				t.Errorf("mode %v: %s: status line = %q, want %q", mode, k, s, noticeNoBoard)
			}
		}
	}
	m, _, _ := newTest(t, ModeSidebar, threeSessions()...)
	if s := statusLine(m); strings.Contains(s, "t tasks") {
		t.Errorf("sidebar help = %q, want no t key", s)
	}
}

func TestTasksErrorKeepsLastTasks(t *testing.T) {
	s := threeSessions()
	s[0].TaskID = "T-42"
	src := &fakeSource{sessions: s, tasks: taskTasks()}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, ModePopup))
	src.tasks, src.tasksErr = nil, errors.New("database is locked")
	m = refresh(t, m)
	if !strings.Contains(m.View(), "T-42 auth refactor") {
		t.Errorf("task title gone after a failed read:\n%s", m.View())
	}
	if s := statusLine(m); !strings.Contains(s, "refresh failed: database is locked") {
		t.Errorf("status line = %q, want the error", s)
	}
	src.tasksErr = nil
	m = refresh(t, m)
	if strings.Contains(m.View(), "auth refactor") || strings.Contains(statusLine(m), "locked") {
		t.Errorf("after a good empty read:\n%s", m.View())
	}
}

func TestSidebarHasNoTaskColumnAndCollapsedTeams(t *testing.T) {
	m, _, src := newTeamTest(t, ModeSidebar)
	view := m.View()
	for _, absent := range []string{"TASK", "T-42", "auth refactor", "alpha", "beta", "gamma"} {
		if strings.Contains(view, absent) {
			t.Errorf("sidebar shows %q:\n%s", absent, view)
		}
	}
	if rows := bodyLines(m); len(rows) != 2 || !strings.Contains(rows[0], "▸ work:server +3") {
		t.Errorf("sidebar rows = %q, want the team collapsed", rows)
	}
	if h := strings.Fields(strings.Split(view, "\n")[0]); !slices.Equal(h, []string{"WINDOW", "CTX", "AGE"}) {
		t.Errorf("sidebar header = %q", h)
	}
	if src.taskRead != 0 {
		t.Errorf("sidebar read Tasks %d times, want never", src.taskRead)
	}
}

// hostile is text that tries to draw: escape sequences, bidi controls, a
// line break, wide characters, and length.
const hostile = "al\x1b[31mpha\x1b]52;c;eA==\x07\u202e\u2067evil\n数据库迁移" + "-very-long-name-that-goes-on"

func TestTeamTextCannotDrawEscapes(t *testing.T) {
	sessions := teamSessions()
	for i := range sessions {
		sessions[i].TeamMember = hostile
	}
	fake := &tmux.Fake{Panes: []tmux.Pane{{ID: "%10", Session: "work", WindowID: "@2", WindowName: "win\x1b[2J\u202e"}}}
	src := &fakeSource{sessions: sessions, tasks: map[string]model.Task{"T-42": {ID: "T-42", Title: hostile}}}
	m := refresh(t, New(src, fake, config.Default(), func() time.Time { return testNow }, ModePopup))
	m, _ = press(m, " ")
	view := m.View()
	if !strings.Contains(view, "al?[31mpha") {
		t.Errorf("member name not drawn cleaned:\n%q", view)
	}
	wantClean(t, view)
}

// wantClean fails when view holds a control or format character.
func wantClean(t *testing.T, view string) {
	t.Helper()
	for _, bad := range []string{"\x1b", "\x07", "\u202e", "\u2067", "\t"} {
		if strings.Contains(view, bad) {
			t.Errorf("view contains %q:\n%q", bad, view)
		}
	}
}

// Plan 1 live-check defect 13: the table fits every width, with a team
// collapsed and expanded, a task column, and long, hostile and wide
// names; one column draws without a panic.
func TestDashboardFitsEveryWidth(t *testing.T) {
	sessions := teamSessions()
	for i := range sessions {
		sessions[i].TeamMember = hostile
		sessions[i].LastUpdate = hostile
		sessions[i].Repo = "/src/数据库-" + hostile
	}
	tasks := map[string]model.Task{"T-42": {ID: "T-42", Title: hostile}}
	for _, width := range []int{1, 40, 60, 80, 120, 200} {
		for _, expand := range []bool{false, true} {
			src := &fakeSource{sessions: sessions, counts: teamCounts(), tasks: tasks}
			m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, ModePopup))
			m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
			if expand {
				m, _ = press(m, " ")
			}
			view := m.View()
			for _, l := range strings.Split(view, "\n") {
				if w := lipgloss.Width(l); w > width {
					t.Errorf("width %d expanded %v: line is %d cells: %q", width, expand, w, l)
				}
			}
			wantClean(t, view)
			if n := len(strings.Split(view, "\n")); n != 12 {
				t.Errorf("width %d expanded %v: %d lines, want the height 12", width, expand, n)
			}
		}
	}
}

// revSessions: S1 is review-ready (idle, and its task T-41 has review R-1
// waiting), S2 works on T-42, which has no review, and S3 has no task.
func revSessions() []model.Session {
	s := decSessions()
	s[0].TaskID, s[0].ReviewPending = "T-41", true
	s[1].TaskID, s[1].State = "T-42", model.StateWorking
	return s
}

// revReviews is the Reviews read of revSessions: T-41's review R-1.
func revReviews() map[string]string { return map[string]string{"T-41": "R-1"} }

// newRevTest returns the dashboard over sessions and reviews in mode on the
// clock clk, set up by setup when it is not nil, after its first refresh.
func newRevTest(t *testing.T, mode Mode, clk *sayClock, sessions []model.Session, reviews map[string]string,
	setup func(Model) Model,
) (tea.Model, *fakeSource) {
	t.Helper()
	src := &fakeSource{sessions: sessions, reviews: reviews}
	m := New(src, &tmux.Fake{}, config.Default(), clk.now, mode)
	if setup != nil {
		m = setup(m)
	}
	return refresh(t, m), src
}

// reviewFactory returns a review factory of fakeChilds that close with
// ReviewClosedMsg, the task ids it was called with and the messages the
// children got.
func reviewFactory() (func(string) tea.Model, *[]string, *[]tea.Msg) {
	var opened []string
	var got []tea.Msg
	return func(id string) tea.Model {
		opened = append(opened, id)
		return fakeChild{id: id, got: &got, closeWith: ReviewClosedMsg{}}
	}, &opened, &got
}

// withReview is a newRevTest setup that sets open as the review factory.
func withReview(open func(string) tea.Model) func(Model) Model {
	return func(m Model) Model { return m.WithReview(open) }
}

func TestRKeyOpensReviewForRowsTask(t *testing.T) {
	clk := &sayClock{t: testNow}
	open, opened, got := reviewFactory()
	m, _ := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), withReview(open))
	clk.advance(time.Second)
	m, cmd := press(m, "r")
	if !slices.Equal(*opened, []string{"T-41"}) {
		t.Fatalf("factory called with %q, want [T-41]", *opened)
	}
	if v := m.View(); v != "review view of T-41" {
		t.Errorf("view = %q, want the child's", v)
	}
	m = feed(m, cmd) // the child's Init
	m, _ = press(m, "a", "y", "r")
	want := []tea.Msg{childPing{"T-41"}, key("a"), key("y"), key("r")}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("child got %#v, want %#v (every key, the dashboard reads none)", *got, want)
	}
	if len(*opened) != 1 || m.View() != "review view of T-41" {
		t.Errorf("an r inside the review view opened another (%q) or changed the view: %q", *opened, m.View())
	}
}

// On a team's group row the lead's task counts, not a teammate's.
func TestRKeyOnGroupRowUsesLeadTask(t *testing.T) {
	sessions := teamSessions() // the lead L and its teammates on T-42
	for i := range sessions {
		if sessions[i].TeamRole == model.RoleTeammate {
			sessions[i].TaskID = "T-43"
		}
	}
	for _, c := range []struct {
		reviews map[string]string
		opened  []string
		notice  string
	}{
		{reviews: map[string]string{"T-42": "R-1"}, opened: []string{"T-42"}},
		{reviews: map[string]string{"T-43": "R-2"}, notice: noticeNoReviewWaiting},
	} {
		clk := &sayClock{t: testNow}
		open, opened, _ := reviewFactory()
		m, _ := newRevTest(t, ModePopup, clk, sessions, c.reviews, withReview(open))
		if rows := bodyLines(m); !strings.Contains(rows[0], "▸ ") {
			t.Fatalf("first row %q is not the team's group row", rows[0])
		}
		clk.advance(time.Second)
		m, _ = press(m, "r")
		if !slices.Equal(*opened, c.opened) {
			t.Errorf("reviews %v: factory called with %q, want %q", c.reviews, *opened, c.opened)
		}
		if c.notice != "" && statusLine(m) != c.notice {
			t.Errorf("reviews %v: status line = %q, want %q", c.reviews, statusLine(m), c.notice)
		}
	}
}

func TestRKeyWithoutPendingReview(t *testing.T) {
	clk := &sayClock{t: testNow}
	open, opened, _ := reviewFactory()
	m, _ := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), withReview(open))
	for _, moves := range [][]string{{"j"}, {"j", "j"}} { // S2: a task without a review; S3: no task
		m2, _ := press(m, moves...)
		clk.advance(time.Second)
		m2, cmd := press(m2, "r")
		if s := statusLine(m2); s != "no review is waiting for this session" {
			t.Errorf("after %q: status line = %q, want the notice", moves, s)
		}
		if cmd != nil || !strings.Contains(m2.View(), "model-S1") {
			t.Errorf("after %q: r started a command or left the table:\n%s", moves, m2.View())
		}
	}
	m, _ = newRevTest(t, ModePopup, clk, nil, revReviews(), withReview(open))
	clk.advance(time.Second)
	if _, cmd := press(m, "r"); cmd != nil {
		t.Error("r with no rows started a command")
	}
	if len(*opened) != 0 {
		t.Errorf("factory called with %q, want never", *opened)
	}
}

// Without a factory r says why: the reason given, or, with none given (curia
// ui inside an agent session), that reviews are not opened there.
func TestRKeyWithoutFactory(t *testing.T) {
	const why = "reviews cannot be opened here: the log file cannot be opened"
	for _, c := range []struct {
		setup func(Model) Model
		want  string
	}{
		{setup: func(m Model) Model { return m.WithoutReview(why) }, want: why},
		{setup: nil, want: "reviews cannot be opened inside an agent session (CLAUDECODE is set)"},
		{setup: func(m Model) Model {
			return m.WithReview(func(string) tea.Model { panic("opened") }).WithoutReview(why)
		}, want: why},
	} {
		clk := &sayClock{t: testNow}
		m, _ := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), c.setup)
		clk.advance(time.Second)
		m, cmd := press(m, "r")
		if s := statusLine(m); s != c.want {
			t.Errorf("status line = %q, want %q", s, c.want)
		}
		if cmd != nil || !strings.Contains(m.View(), "model-S1") {
			t.Errorf("r without a factory started a command or left the table:\n%s", m.View())
		}
	}
}

func TestRKeyDisabledInSidebar(t *testing.T) {
	clk := &sayClock{t: testNow}
	open, opened, _ := reviewFactory()
	m, src := newRevTest(t, ModeSidebar, clk, revSessions(), revReviews(), withReview(open))
	clk.advance(time.Second)
	m, cmd := press(m, "r")
	if s := statusLine(m); s != "open the popup to review" {
		t.Errorf("status line = %q, want the sidebar notice", s)
	}
	if cmd != nil || len(*opened) != 0 {
		t.Errorf("r in the sidebar opened %q", *opened)
	}
	if src.reviewRead != 0 {
		t.Errorf("sidebar read Reviews %d times, want never", src.reviewRead)
	}
}

func TestReviewClosedReturnsToDashboard(t *testing.T) {
	clk := &sayClock{t: testNow}
	open, opened, got := reviewFactory()
	m, src := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), withReview(open))
	clk.advance(time.Second)
	m, first := press(m, "r")
	m, closeCmd := press(m, "q")
	if closeCmd == nil {
		t.Fatal("q in the child sent nothing")
	}
	reads := src.reviewRead
	m, restart := m.Update(closeCmd()) // the child's ReviewClosedMsg
	if !strings.Contains(m.View(), "model-S1") {
		t.Fatalf("view after close is not the dashboard:\n%s", m.View())
	}
	if restart == nil {
		t.Fatal("close started no refresh")
	}
	if _, ok := restart().(refreshMsg); !ok || src.reviewRead != reads+1 {
		t.Errorf("close's command is not a refresh (Reviews reads %d, want %d)", src.reviewRead, reads+1)
	}
	// The table is back on screen: r waits the dwell again.
	m, _ = press(m, "r")
	if s := statusLine(m); s != noticeTooSoon || len(*opened) != 1 {
		t.Errorf("r at once after the close: status %q, opened %q; want too soon", s, *opened)
	}
	clk.advance(time.Second)
	m, _ = press(m, "r")
	*got = nil
	if _, cmd := m.Update(first()); cmd != nil || len(*got) != 0 {
		t.Errorf("the closed child's late message reached the new one: %#v", *got)
	}
	// A bare ReviewClosedMsg closes too.
	if m, _ = m.Update(ReviewClosedMsg{}); !strings.Contains(m.View(), "model-S1") {
		t.Errorf("view after a bare close is not the dashboard:\n%s", m.View())
	}
}

func TestPastedRIsNotACommand(t *testing.T) {
	clk := &sayClock{t: testNow}
	open, opened, _ := reviewFactory()
	m, _ := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), withReview(open))
	clk.advance(time.Second)
	for _, p := range []string{"r", "ra", "ry", "jr"} {
		m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(p), Paste: true})
		if cmd != nil || len(*opened) != 0 || !strings.Contains(m2.View(), "model-S1") {
			t.Errorf("pasted %q: command %v, opened %q, view:\n%s", p, cmd != nil, *opened, m2.View())
		}
	}
}

func TestReviewReadyRowIcon(t *testing.T) {
	s := revSessions()
	working := s[0] // T-41's review is waiting, but the session works on
	working.ID, working.Model, working.Pane, working.State = "S4", "model-S4", "%4", model.StateWorking
	asking := s[2]
	asking.ID, asking.Model, asking.Pane, asking.State = "S5", "model-S5", "%5", model.StateNeedsDecision
	clk := &sayClock{t: testNow}
	m, _ := newRevTest(t, ModePopup, clk, append(s, working, asking), revReviews(), nil)
	view := m.View()
	for name, icon := range map[string]string{"model-S1": "◆", "model-S4": "◐", "model-S2": "◐", "model-S3": "○", "model-S5": "◆"} {
		if row := rowOf(t, view, name); !strings.HasPrefix(row, icon+" ") {
			t.Errorf("row %q, want the state icon %s", row, icon)
		}
	}
	if got := stateStyle(s[0]).GetForeground(); got != lipgloss.Color("4") {
		t.Errorf("review-ready icon color = %v, want blue (4)", got)
	}
	for _, other := range []model.Session{working, asking, s[1], s[2]} {
		if got := stateStyle(other).GetForeground(); got != (lipgloss.NoColor{}) {
			t.Errorf("%s's icon color = %v, want plain", other.ID, got)
		}
	}
}

func TestReviewsErrorKeepsLastMap(t *testing.T) {
	clk := &sayClock{t: testNow}
	open, opened, _ := reviewFactory()
	m, src := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), withReview(open))
	src.reviews, src.reviewsErr = nil, errors.New("database is locked")
	m = refresh(t, m)
	if s := statusLine(m); !strings.Contains(s, "refresh failed: database is locked") {
		t.Errorf("status line = %q, want the error", s)
	}
	clk.advance(time.Second)
	if m2, _ := press(m, "r"); !slices.Equal(*opened, []string{"T-41"}) {
		t.Errorf("after a failed read r opened %q, want T-41 from the last good map:\n%s", *opened, m2.View())
	}
	src.reviewsErr = nil
	m = refresh(t, m)
	clk.advance(time.Second)
	m, _ = press(m, "r")
	if s := statusLine(m); s != noticeNoReviewWaiting || len(*opened) != 1 {
		t.Errorf("after a good empty read: status %q, opened %q", s, *opened)
	}
}

func TestHelpLineNamesReview(t *testing.T) {
	clk := &sayClock{t: testNow}
	m, src := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), nil)
	if s := statusLine(m); !strings.Contains(s, "  r review  ") {
		t.Errorf("popup help = %q, want r review", s)
	}
	if src.reviewRead != 1 {
		t.Errorf("popup refresh read Reviews %d times, want 1", src.reviewRead)
	}
	m, _ = newRevTest(t, ModeSidebar, clk, revSessions(), revReviews(), nil)
	if s := statusLine(m); strings.Contains(s, "review") {
		t.Errorf("sidebar help = %q, want no r key", s)
	}
}

// r waits the dashboard's dwell: the selected row (and whether its task's
// review is waiting) must have been on screen for it. A refused r restarts
// it, so a held r that starts too soon never opens the view.
func TestRKeyWaitsForDwell(t *testing.T) {
	clk := &sayClock{t: testNow}
	open, opened, _ := reviewFactory()
	m, src := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), withReview(open))
	m, _ = press(m, "r") // at once: the table has just been drawn
	if s := statusLine(m); s != noticeTooSoon || len(*opened) != 0 {
		t.Fatalf("r at once: status %q, opened %q; want too soon", s, *opened)
	}
	// Held from there: the first repeat 375 to 660 ms after, then one every
	// 30 ms, never opens it.
	for _, first := range []time.Duration{375 * time.Millisecond, 500 * time.Millisecond, 660 * time.Millisecond} {
		clk.advance(time.Second)
		m, _ = press(m, "j", "k") // the row anew: the dwell starts
		m, _ = press(m, "r")
		clk.advance(first)
		for held := time.Duration(0); held < 3*time.Second; held += 30 * time.Millisecond {
			m, _ = press(m, "r")
			clk.advance(30 * time.Millisecond)
		}
		if len(*opened) != 0 {
			t.Fatalf("an r held from the dwell, first repeat at %v, opened %q", first, *opened)
		}
	}
	clk.advance(time.Second) // let go, pause
	if m, _ = press(m, "r"); !slices.Equal(*opened, []string{"T-41"}) {
		t.Fatalf("r after a pause opened %q, want T-41:\n%s", *opened, m.View())
	}

	// Moving onto the row restarts the dwell.
	*opened = nil
	src.reviews = map[string]string{"T-41": "R-1", "T-42": "R-2"}
	m, _ = newRevTest(t, ModePopup, clk, revSessions(), src.reviews, withReview(open))
	clk.advance(time.Second)
	m, _ = press(m, "j")
	clk.advance(100 * time.Millisecond)
	if m, _ = press(m, "r"); len(*opened) != 0 || statusLine(m) != noticeTooSoon {
		t.Errorf("r 100 ms after j opened %q", *opened)
	}
	// A burst that moves and opens: the r is too soon, and nothing after it acts.
	clk.advance(time.Second)
	if _, _ = m.Update(key("kr")); len(*opened) != 0 {
		t.Errorf("the burst kr opened %q", *opened)
	}

	// A review appearing on the selected row restarts the dwell too.
	sessions := revSessions()
	sessions[0].ReviewPending = false
	m, src = newRevTest(t, ModePopup, clk, sessions, nil, withReview(open))
	clk.advance(time.Second)
	src.reviews = revReviews()
	m = refresh(t, m)
	clk.advance(100 * time.Millisecond)
	if m, _ = press(m, "r"); len(*opened) != 0 || statusLine(m) != noticeTooSoon {
		t.Errorf("r 100 ms after the review appeared opened %q (%q)", *opened, statusLine(m))
	}
	clk.advance(time.Second)
	if _, _ = press(m, "r"); !slices.Equal(*opened, []string{"T-41"}) {
		t.Errorf("r after the dwell opened %q, want T-41", *opened)
	}
}

// dashPump runs commands as the program would, feeding each message to the
// dashboard and running what it starts. A refresh's result is fed without
// running the one-second tick it schedules; a message of Bubble Tea's own
// runtime (the diff tool's exec) is recorded instead of fed, as the program
// acts on it and never gives it to the model.
type dashPump struct {
	t       *testing.T
	m       tea.Model
	quit    bool
	runtime []tea.Msg
}

func (p *dashPump) run(cmd tea.Cmd) {
	p.t.Helper()
	teaPath := reflect.TypeFor[tea.QuitMsg]().PkgPath()
	pending := []tea.Cmd{cmd}
	for steps := 0; len(pending) > 0; steps++ {
		if steps > 200 {
			p.t.Fatal("commands did not settle")
		}
		c := pending[0]
		pending = pending[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil:
		case tea.BatchMsg:
			pending = append(pending, msg...)
		case tea.QuitMsg:
			p.quit = true
		case refreshMsg:
			p.m, _ = p.m.Update(msg)
		default:
			if reflect.TypeOf(msg).PkgPath() == teaPath {
				p.runtime = append(p.runtime, msg)
				continue
			}
			var next tea.Cmd
			p.m, next = p.m.Update(msg)
			pending = append(pending, next)
		}
	}
}

// key sends k and runs what it starts.
func (p *dashPump) key(k tea.KeyMsg) {
	p.t.Helper()
	p.run(p.step(k))
}

// step sends k and returns what it starts without running it.
func (p *dashPump) step(k tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd
	p.m, cmd = p.m.Update(k)
	return cmd
}

// stepConfirm sends y and returns the action its confirmation starts, once
// burstGap has passed with no other key, without running it.
func (p *dashPump) stepConfirm() tea.Cmd {
	p.t.Helper()
	wait := p.step(key("y"))
	if wait == nil {
		return nil
	}
	var cmd tea.Cmd
	p.m, cmd = p.m.Update(wait())
	return cmd
}

// child is the review view the dashboard shows, or fails.
func (p *dashPump) child() ReviewModel {
	p.t.Helper()
	dm, ok := p.m.(Model)
	if !ok {
		p.t.Fatalf("model is a %T, want Model", p.m)
	}
	rm, ok := dm.child.(ReviewModel)
	if !ok {
		p.t.Fatalf("the dashboard shows %T, want the review view:\n%s", dm.child, p.m.View())
	}
	return rm
}

// realReviewFactory returns a factory of real, embedded review views over
// src on now, with their refresh ticks off, and the task ids it was called
// with.
func realReviewFactory(src ReviewSource, now func() time.Time) (func(string) tea.Model, *[]string) {
	var opened []string
	return func(id string) tea.Model {
		opened = append(opened, id)
		rm := NewReview(src, now, ReviewOptions{TaskID: id, Embedded: true})
		rm.tick = func(_ time.Duration, msg tea.Msg) tea.Cmd {
			if c, ok := msg.(reviewConfirmMsg); ok { // burstGap passes with no other key
				return func() tea.Msg { return c }
			}
			return nil
		}
		return rm
	}, &opened
}

// newRealReviewDash returns the popup over revSessions with real review
// views over rsrc, 120 x 40, on clk, past the dwell.
func newRealReviewDash(t *testing.T, clk *sayClock, rsrc *fakeReviews) (*dashPump, *[]string) {
	t.Helper()
	open, opened := realReviewFactory(rsrc, clk.now)
	m, _ := newRevTest(t, ModePopup, clk, revSessions(), revReviews(), withReview(open))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	clk.advance(time.Second)
	return &dashPump{t: t, m: m}, opened
}

// An r in a burst with a following a and y opens the review and nothing
// more; an a and a y right behind it, as keys of their own, reach the view,
// whose own guards refuse them.
func TestRKeyBurstDoesNotApprove(t *testing.T) {
	for _, burst := range []string{"ray", "ra", "ry"} {
		clk := &sayClock{t: testNow}
		rsrc := &fakeReviews{}
		rsrc.set(rvDetail(1))
		p, opened := newRealReviewDash(t, clk, rsrc)
		p.key(key(burst))
		if !slices.Equal(*opened, []string{"T-41"}) || !strings.Contains(p.m.View(), "R-1") {
			t.Fatalf("burst %q: opened %q, view:\n%s", burst, *opened, p.m.View())
		}
		for _, k := range []string{"a", "y", "a", "y"} {
			clk.advance(10 * time.Millisecond)
			p.key(key(k))
		}
		if p.child().mode != reviewBrowse {
			t.Errorf("burst %q: a prompt is open:\n%s", burst, p.m.View())
		}
		wantActions(t, rsrc)
	}
	// r, then a and y as fast as one types, each a key of its own.
	clk := &sayClock{t: testNow}
	rsrc := &fakeReviews{}
	rsrc.set(rvDetail(1))
	p, _ := newRealReviewDash(t, clk, rsrc)
	for _, k := range []string{"r", "a", "y"} {
		p.key(key(k))
		clk.advance(reviewTyping)
	}
	wantActions(t, rsrc)
}

// A held r opens the review once; its repeats go to the view, which takes
// them as typing and acts on nothing. Once let go, the view works.
func TestRKeyHeldOpensOnce(t *testing.T) {
	clk := &sayClock{t: testNow}
	rsrc := &fakeReviews{}
	rsrc.set(rvDetail(1))
	p, opened := newRealReviewDash(t, clk, rsrc)
	p.key(key("r"))
	clk.advance(500 * time.Millisecond)
	for held := time.Duration(0); held < 3*time.Second; held += reviewRepeat {
		p.key(key("r"))
		clk.advance(reviewRepeat)
	}
	if len(*opened) != 1 {
		t.Errorf("a held r opened %d views, want 1", len(*opened))
	}
	wantActions(t, rsrc)
	_ = p.child()
	clk.advance(reviewGap)
	p.key(key("a"))
	clk.advance(time.Second)
	p.key(key("y"))
	wantActions(t, rsrc, "Approve R-1")
}

// The dashboard's last read says T-41's review is waiting, but it closed
// before r: the view opened for the task says so, and nothing can act.
func TestRKeyOnReviewClosedSinceRefresh(t *testing.T) {
	clk := &sayClock{t: testNow}
	rsrc := &fakeReviews{} // R-1 was closed meanwhile
	p, opened := newRealReviewDash(t, clk, rsrc)
	p.key(key("r"))
	if !slices.Equal(*opened, []string{"T-41"}) || !strings.Contains(p.m.View(), "T-41 has no review waiting") {
		t.Fatalf("opened %q, view:\n%s", *opened, p.m.View())
	}
	for _, k := range []string{"a", "y", "x", "y", "c"} {
		clk.advance(reviewGap)
		p.key(key(k))
	}
	wantActions(t, rsrc)
	if p.child().mode != reviewBrowse {
		t.Errorf("a prompt opened on no review:\n%s", p.m.View())
	}
	clk.advance(reviewGap)
	p.key(key("q"))
	if !strings.Contains(p.m.View(), "model-S1") {
		t.Errorf("q did not return to the dashboard:\n%s", p.m.View())
	}
}

// An approval runs off the update loop and shows progress; while it runs a
// second action is refused and nothing closes the view or the dashboard,
// which says why.
func TestEmbeddedReviewApproveInFlight(t *testing.T) {
	clk := &sayClock{t: testNow}
	rsrc := &fakeReviews{line: "R-1 approved: merged"}
	rsrc.set(rvDetail(1))
	p, _ := newRealReviewDash(t, clk, rsrc)
	p.key(key("r"))
	clk.advance(reviewGap)
	p.key(key("a"))
	clk.advance(time.Second)
	approve := p.stepConfirm()
	if approve == nil {
		t.Fatal("y started nothing")
	}
	wantActions(t, rsrc) // not on the update loop
	if s := statusLine(p.m); !strings.Contains(s, "working…") {
		t.Errorf("status line while approving = %q, want progress", s)
	}
	for _, k := range []tea.KeyMsg{key("q"), key("esc"), {Type: tea.KeyCtrlC}, key("a"), key("y"), key("x"), key("y")} {
		clk.advance(time.Second)
		p.key(k)
	}
	if p.quit {
		t.Fatal("a key quit the dashboard while the approval ran")
	}
	_ = p.child()
	if s := statusLine(p.m); !strings.Contains(s, "cannot close until it returns") {
		t.Errorf("status line = %q, want why it cannot close", s)
	}
	p.run(approve)
	wantActions(t, rsrc, "Approve R-1")
	if !strings.Contains(p.m.View(), "R-1 approved: merged") {
		t.Errorf("the approval's line is not shown:\n%s", p.m.View())
	}
	p.key(key("q")) // q closes even in the quiet after an action (Ruling 93)
	if p.quit || !strings.Contains(p.m.View(), "model-S1") {
		t.Errorf("q after the approval: quit %v, view:\n%s", p.quit, p.m.View())
	}
}

// The view's diff tool runs through tea.ExecProcess: its exec message must
// reach the program unwrapped, and the tool's end reach the view.
func TestEmbeddedReviewRunsDiffTool(t *testing.T) {
	clk := &sayClock{t: testNow}
	rsrc := &fakeReviews{diff: exec.Command("true")}
	rsrc.set(rvDetail(1))
	p, _ := newRealReviewDash(t, clk, rsrc)
	p.key(key("r"))
	clk.advance(reviewGap)
	p.key(key("o"))
	if len(p.runtime) != 1 {
		t.Fatalf("runtime messages = %#v, want the diff tool's exec", p.runtime)
	}
	if _, wrapped := p.runtime[0].(childMsg); wrapped {
		t.Fatal("the exec message was wrapped")
	}
	p.run(func() tea.Msg { return reviewDiffDoneMsg{} }) // what the program sends when the tool exits
	if p.child().diffing {
		t.Fatal("the view still waits for the tool")
	}
	clk.advance(reviewGap)
	p.key(key("q"))
	if !strings.Contains(p.m.View(), "model-S1") {
		t.Errorf("q after the tool did not return to the dashboard:\n%s", p.m.View())
	}
}

// Review-ready rows and r's notices draw no stored escape or control text
// and fit every width.
func TestReviewReadyRowsFitEveryWidth(t *testing.T) {
	sessions := revSessions()
	for i := range sessions {
		sessions[i].Repo, sessions[i].Branch = "/src/"+hostile, hostile
		sessions[i].LastUpdate, sessions[i].TaskID = hostile, "T-4\x1b[2J"+hostile
		sessions[i].ReviewPending, sessions[i].State = true, model.StateIdle
	}
	tasks := map[string]model.Task{sessions[0].TaskID: {ID: sessions[0].TaskID, Title: hostile}}
	reviews := map[string]string{sessions[0].TaskID: "R-1"}
	for _, width := range []int{40, 60, 80, 120, 200} {
		for _, k := range []string{"", "r", "j"} {
			clk := &sayClock{t: testNow}
			src := &fakeSource{sessions: sessions, tasks: tasks, reviews: reviews}
			m := refresh(t, New(src, &tmux.Fake{}, config.Default(), clk.now, ModePopup))
			m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 10})
			clk.advance(time.Second)
			if k != "" {
				m, _ = press(m, k, "r")
			}
			view := m.View()
			for _, l := range strings.Split(view, "\n") {
				if w := lipgloss.Width(l); w > width {
					t.Errorf("width %d after %q: line is %d cells: %q", width, k, w, l)
				}
			}
			wantClean(t, view)
			if strings.Count(view, "◆") != len(sessions) {
				t.Errorf("width %d: want every row review-ready:\n%s", width, view)
			}
		}
	}
}

// TestKillAndJumpRefuseOtherServer: the dashboard neither kills nor jumps to
// this server's pane that has the id of a session recorded on another tmux
// server; a session recorded on this server, or on none, is reachable.
func TestKillAndJumpRefuseOtherServer(t *testing.T) {
	ss := threeSessions()
	ss[0].Socket = "/tmp/tmux-501/default"
	ss[1].Socket = "/tmp/tmux-501/other" // b, in %5, which this server also has
	m, fake, _ := newTest(t, ModePopup, ss...)
	for i := range fake.Panes {
		fake.Panes[i].Socket = "/tmp/tmux-501/default"
	}
	m = refresh(t, m)
	m = selectID(t, m, "b")
	m, _ = press(m, "x", "y")
	m, cmd := press(m, "enter")
	if got := append(calls(fake, "KillPane"), calls(fake, "SwitchTo")...); len(got) != 0 {
		t.Errorf("tmux calls %q for a session on another server", got)
	}
	if cmd != nil && isQuit(cmd) {
		t.Error("the popup quit as after a jump")
	}
	if v := m.View(); !strings.Contains(v, "on another tmux server") {
		t.Errorf("the view does not say why:\n%s", v)
	}
	m = selectID(t, m, "a")
	_, _ = press(m, "x", "y")
	if got := calls(fake, "KillPane"); !slices.Equal(got, []string{"KillPane %3"}) {
		t.Errorf("this server's session: kill calls %q", got)
	}
}

// selectID moves the selection down to the row of session id.
func selectID(t *testing.T, m tea.Model, id string) tea.Model {
	t.Helper()
	for range 10 {
		m, _ = press(m, "up")
	}
	for range 10 {
		mm, ok := m.(Model)
		if !ok {
			t.Fatalf("model is %T", m)
		}
		if r, ok := mm.current(); ok && r.s.ID == id {
			return m
		}
		m, _ = press(m, "down")
	}
	t.Fatalf("no row for %s", id)
	return m
}

// costSessions: s1 idle with a cost and a cache 12m30s from expiry, s2
// working with a cost and a warm cache, s3 idle with neither; each in its
// own window (work:@1 to work:@3), in this Source order.
func costSessions() []model.Session {
	s := func(n int, state model.State) model.Session {
		id := "s" + strconv.Itoa(n)
		return model.Session{
			ID: id, Pane: "%" + strconv.Itoa(n), TmuxSession: "work", TmuxWindow: "@" + strconv.Itoa(n),
			State: state, Model: "model-" + id, UpdatedAt: testNow,
		}
	}
	s1, s2, s3 := s(1, model.StateIdle), s(2, model.StateWorking), s(3, model.StateIdle)
	s1.CostUSD, s1.CostKnown, s1.CacheExpiresAt = 4.12, true, testNow.Add(12*time.Minute+30*time.Second)
	s2.CostUSD, s2.CostKnown, s2.CacheExpiresAt = 0.07, true, testNow.Add(12*time.Minute)
	return []model.Session{s1, s2, s3}
}

// newCostTest returns the dashboard in mode over sessions and budget after
// its first refresh, width columns wide (0: no WindowSizeMsg).
func newCostTest(t *testing.T, mode Mode, width int, budget Budget, sessions ...model.Session) (tea.Model, *fakeSource) {
	t.Helper()
	src := &fakeSource{sessions: sessions, budget: budget}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, mode))
	if width > 0 {
		m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 20})
	}
	return m, src
}

// costSplit cuts a drawn line into cells of the given widths, separated by
// gap spaces, measuring as the table does (lipgloss.Width), and returns
// what follows the last cell. A cell drawn wider or narrower than its
// column, which shifts every column after it, is an error. A line clipped
// at the window's edge ends early.
func costSplit(line string, widths []int, gap int) ([]string, string, error) {
	runes := []rune(line)
	i := 0
	var cells []string
	for c, w := range widths {
		for k := 0; c > 0 && k < gap; k++ {
			if i == len(runes) {
				return cells, "", nil
			}
			if runes[i] != ' ' {
				return nil, "", fmt.Errorf("column %d starts at %q, not after the gap", c, string(runes[i:]))
			}
			i++
		}
		cur := ""
		for i < len(runes) && lipgloss.Width(cur+string(runes[i])) <= w {
			cur += string(runes[i])
			i++
		}
		if i < len(runes) && lipgloss.Width(cur) != w {
			return nil, "", fmt.Errorf("column %d is %d cells, drawn %q (%d cells)", c, w, cur, lipgloss.Width(cur))
		}
		cells = append(cells, strings.TrimSpace(cur))
	}
	return cells, string(runes[i:]), nil
}

// costTable reads m's table back from its view: the header's titles, each
// row's cells, and what the header shows after its last title. It fails
// the test when a line's columns do not line up with the header's.
func costTable(t *testing.T, m tea.Model) (titles []string, rows [][]string, note string) {
	t.Helper()
	mm, ok := m.(Model)
	if !ok {
		t.Fatalf("model is %T", m)
	}
	lay := mm.layout(mm.now())
	lines := strings.Split(m.View(), "\n")
	titles, rest, err := costSplit(lines[0], lay.widths, lay.gap)
	if err != nil {
		t.Fatalf("width %d: header %q: %v", mm.width, lines[0], err)
	}
	for i := range lay.cells {
		cells, after, err := costSplit(lines[1+i], lay.widths, lay.gap)
		if err != nil || after != "" {
			t.Fatalf("width %d: row %q: %v, then %q\nheader %q", mm.width, lines[1+i], err, after, lines[0])
		}
		rows = append(rows, cells)
	}
	return titles, rows, strings.TrimSpace(rest)
}

// costCell is the cell under title of the row that has a cell equal to
// key, such as its window "work:@1".
func costCell(t *testing.T, m tea.Model, key, title string) string {
	t.Helper()
	titles, rows, _ := costTable(t, m)
	col := slices.Index(titles, title)
	if col < 0 {
		t.Fatalf("no %s column in %q", title, titles)
	}
	for _, r := range rows {
		if slices.Contains(r, key) {
			return r[col]
		}
	}
	t.Fatalf("no row %q in %q", key, rows)
	return ""
}

func TestCostAndCacheColumns(t *testing.T) {
	for _, width := range []int{120, 100, 0} {
		m, _ := newCostTest(t, ModePopup, width, Budget{}, costSessions()...)
		titles, _, _ := costTable(t, m)
		if dec := slices.Index(titles, "DEC"); dec < 0 || !slices.Equal(titles[dec+1:dec+3], []string{"COST", "CACHE"}) {
			t.Errorf("width %d: header %q, want COST and CACHE right after DEC", width, titles)
		}
		for _, c := range []struct{ window, cost, cache string }{
			{"work:@1", "$4.12", "12m"}, // idle: the cache's time left
			{"work:@2", "$0.07", ""},    // working: no cache cell
			{"work:@3", "", ""},         // nothing known: blank, not $0.00
		} {
			if got := costCell(t, m, c.window, "COST"); got != c.cost {
				t.Errorf("width %d: %s COST = %q, want %q", width, c.window, got, c.cost)
			}
			if got := costCell(t, m, c.window, "CACHE"); got != c.cache {
				t.Errorf("width %d: %s CACHE = %q, want %q", width, c.window, got, c.cache)
			}
		}
	}
	// COST is right-aligned: a narrower cost ends where a wider one does.
	m, _ := newCostTest(t, ModePopup, 120, Budget{}, costSessions()...)
	view := m.View()
	r1, r2 := rowOf(t, view, "work:@1"), rowOf(t, view, "work:@2")
	if e1, e2 := lipgloss.Width(r1[:strings.Index(r1, "$4.12")+5]), lipgloss.Width(r2[:strings.Index(r2, "$0.07")+5]); e1 != e2 {
		t.Errorf("COST cells end at %d and %d, want right-aligned:\n%s", e1, e2, view)
	}
}

// Below 100 columns the table is exactly Plan 3's: the cost and cache
// fields change nothing.
func TestCostAndCacheHiddenBelow100(t *testing.T) {
	plain := costSessions()
	for i := range plain {
		plain[i].CostUSD, plain[i].CostKnown, plain[i].CacheExpiresAt = 0, false, time.Time{}
	}
	for _, width := range []int{99, 90, 80, 60, 50, 20} {
		m, _ := newCostTest(t, ModePopup, width, Budget{}, costSessions()...)
		was, _ := newCostTest(t, ModePopup, width, Budget{}, plain...)
		if m.View() != was.View() {
			t.Errorf("width %d: the view changed:\n%s\nwant:\n%s", width, m.View(), was.View())
		}
		if v := m.View(); strings.Contains(v, "COST") || strings.Contains(v, "CACHE") || strings.Contains(v, "$4.12") {
			t.Errorf("width %d shows the cost columns:\n%s", width, v)
		}
	}
	m, _ := newCostTest(t, ModePopup, 99, Budget{}, costSessions()...)
	if h := strings.Fields(strings.Split(m.View(), "\n")[0]); !titlesMatch(h, []string{"SESSION:WINDOW", "REPO@BRANCH", "TASK", "CTX", "DEC", "LAST", "UPDATE", "AGE"}) {
		t.Errorf("width 99: header = %q, want Plan 3's", h)
	}
}

func TestCacheCellStyles(t *testing.T) {
	if got := cacheStyle(testNow.Add(4*time.Minute+59*time.Second), testNow).GetForeground(); got != lipgloss.Color("3") {
		t.Errorf("4m left: color %v, want yellow", got)
	}
	for _, cold := range []time.Time{testNow.Add(-time.Minute), testNow} {
		if st := cacheStyle(cold, testNow); !st.GetFaint() || st.GetForeground() != (lipgloss.NoColor{}) {
			t.Errorf("cold at %v: faint %v color %v, want dim", cold, st.GetFaint(), st.GetForeground())
		}
	}
	for _, plain := range []time.Time{testNow.Add(5 * time.Minute), testNow.Add(12 * time.Minute), {}} {
		if st := cacheStyle(plain, testNow); st.GetFaint() || st.GetForeground() != (lipgloss.NoColor{}) {
			t.Errorf("expiry %v: faint %v color %v, want plain", plain, st.GetFaint(), st.GetForeground())
		}
	}
}

// costTeam is a team in window @2: lead L (working, $1.00, a warm cache
// that does not count while it works), alpha (idle, $2.50, 9m left) and
// beta (waiting, no cost, 4m left); and a solo session in window @1.
func costTeam() []model.Session {
	mate := func(id, member string, state model.State) model.Session {
		return model.Session{
			ID: id, Pane: "%1" + id, TmuxSession: "work", TmuxWindow: "@2", State: state, Model: "model-" + id,
			UpdatedAt: testNow, TeamID: "team-1", TeamRole: model.RoleTeammate, TeamMember: member,
		}
	}
	lead, a, b := mate("L", "team-lead", model.StateWorking), mate("A", "alpha", model.StateIdle), mate("B", "beta", model.StateWaiting)
	lead.TeamRole = model.RoleLead
	lead.CostUSD, lead.CostKnown, lead.CacheExpiresAt = 1, true, testNow.Add(2*time.Minute)
	a.CostUSD, a.CostKnown, a.CacheExpiresAt = 2.5, true, testNow.Add(9*time.Minute)
	b.CacheExpiresAt = testNow.Add(4 * time.Minute)
	solo := model.Session{ID: "solo", Pane: "%1", TmuxSession: "work", TmuxWindow: "@1", State: model.StateWorking, Model: "model-solo", UpdatedAt: testNow}
	return fleet.Rank([]model.Session{solo, lead, a, b})
}

func TestGroupRowSumsCostAndShowsSoonestCache(t *testing.T) {
	const group = "▸ work:@2 +2"
	m, _ := newCostTest(t, ModePopup, 140, Budget{}, costTeam()...)
	if got := costCell(t, m, group, "COST"); got != "$3.50" {
		t.Errorf("group COST = %q, want the members' known costs summed", got)
	}
	if got := costCell(t, m, group, "CACHE"); got != "4m" {
		t.Errorf("group CACHE = %q, want beta's 4m (the soonest among members not working)", got)
	}
	// Expanded, each member shows its own cells.
	m, _ = press(m, " ")
	for _, c := range []struct{ key, cost, cache string }{{"▾ work:@2", "$1.00", ""}, {"alpha", "$2.50", "9m"}, {"beta", "", "4m"}} {
		if got := costCell(t, m, c.key, "COST"); got != c.cost {
			t.Errorf("%s COST = %q, want %q", c.key, got, c.cost)
		}
		if got := costCell(t, m, c.key, "CACHE"); got != c.cache {
			t.Errorf("%s CACHE = %q, want %q", c.key, got, c.cache)
		}
	}
	// No member idle, waiting or needs_decision with a warm cache, and one
	// cold (in any state): "cold"; with no cache state known at all: blank.
	team := costTeam()
	for i := range team {
		switch team[i].ID {
		case "A":
			team[i].State = model.StateWorking
		case "B":
			team[i].State, team[i].CacheExpiresAt = model.StateWorking, testNow.Add(-time.Minute)
		}
	}
	m, _ = newCostTest(t, ModePopup, 140, Budget{}, team...)
	if got := costCell(t, m, group, "CACHE"); got != "cold" {
		t.Errorf("group CACHE = %q, want cold", got)
	}
	for i := range team {
		team[i].CacheExpiresAt, team[i].CostKnown, team[i].CostUSD = time.Time{}, false, 0
	}
	m, _ = newCostTest(t, ModePopup, 140, Budget{}, team...)
	if c, k := costCell(t, m, group, "COST"), costCell(t, m, group, "CACHE"); c != "" || k != "" {
		t.Errorf("group with nothing known: COST %q CACHE %q, want both blank", c, k)
	}
}

// costOrder is the window cells of m's rows, top to bottom.
func costOrder(t *testing.T, m tea.Model) []string {
	t.Helper()
	titles, rows, _ := costTable(t, m)
	col := slices.Index(titles, "SESSION:WINDOW")
	if col < 0 {
		t.Fatalf("no window column in %q", titles)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r[col]
	}
	return out
}

// costBurn is s1 0.5 and s2 3.0 dollars an hour; s3 has no burn.
func costBurn() Budget { return Budget{Burn: map[string]float64{"s1": 0.5, "s2": 3.0}} }

func TestBKeyTogglesBurnSort(t *testing.T) {
	m, _ := newCostTest(t, ModePopup, 120, costBurn(), costSessions()...)
	rank := []string{"work:@1", "work:@2", "work:@3"}
	if got := costOrder(t, m); !slices.Equal(got, rank) {
		t.Fatalf("rows = %q, want the rank order %q", got, rank)
	}
	if s := statusLine(m); !strings.Contains(s, "  b burn  ") {
		t.Errorf("help = %q, want b burn", s)
	}
	m, _ = press(m, "b")
	if got, want := costOrder(t, m), []string{"work:@2", "work:@1", "work:@3"}; !slices.Equal(got, want) {
		t.Errorf("after b rows = %q, want %q (highest burn first, no burn last)", got, want)
	}
	if _, _, note := costTable(t, m); note != "sorted by burn (b)" {
		t.Errorf("after b the header ends %q, want sorted by burn (b)", note)
	}
	m, _ = press(m, "b")
	if got := costOrder(t, m); !slices.Equal(got, rank) {
		t.Errorf("after b b rows = %q, want the rank order", got)
	}
	if _, _, note := costTable(t, m); note != "" {
		t.Errorf("after b b the header ends %q, want no note", note)
	}
	// Units without a burn keep their rank order after those with one, and
	// ties keep it too.
	sessions := slices.Concat(costSessions(), []model.Session{
		{ID: "s4", Pane: "%4", TmuxSession: "work", TmuxWindow: "@4", State: model.StateIdle, UpdatedAt: testNow},
	})
	m, _ = newCostTest(t, ModePopup, 120, Budget{Burn: map[string]float64{"s2": 1, "s4": 1}}, sessions...)
	m, _ = press(m, "b")
	if got, want := costOrder(t, m), []string{"work:@2", "work:@4", "work:@1", "work:@3"}; !slices.Equal(got, want) {
		t.Errorf("ties and no burn: rows = %q, want %q", got, want)
	}
}

func TestBurnSortKeepsSelection(t *testing.T) {
	fake := &tmux.Fake{Panes: []tmux.Pane{{ID: "%1", Session: "work", WindowID: "@1"}, {ID: "%2", Session: "work", WindowID: "@2"}}}
	src := &fakeSource{sessions: costSessions(), budget: costBurn()}
	m := refresh(t, New(src, fake, config.Default(), func() time.Time { return testNow }, ModePopup))
	m = selectID(t, m, "s1") // the first row; second once sorted by burn
	m, _ = press(m, "b")
	if mm, ok := m.(Model); !ok || mm.cursor != 1 {
		t.Errorf("after b the cursor is on row %d, want s1's row 1", mm.cursor)
	}
	m, _ = press(m, "b")
	_, _ = press(m, "enter")
	if got := calls(fake, "SwitchTo"); !slices.Equal(got, []string{"SwitchTo %1"}) {
		t.Errorf("SwitchTo calls = %q, want s1's pane after two re-sorts", got)
	}
}

func TestBurnSortGroupsByUnit(t *testing.T) {
	// The team's members burn 1.0, 1.0 and 1.5 (3.5 together), the solo
	// session 2.0, the waiting session W none. In rank order the team is
	// first (alpha needs a decision), then W, then solo.
	waiting := model.Session{ID: "W", Pane: "%20", TmuxSession: "work", TmuxWindow: "@3", State: model.StateWaiting, UpdatedAt: testNow}
	sessions := fleet.Rank(append(teamSessions(), waiting))
	burn := Budget{Burn: map[string]float64{"A": 1, "B": 1, "C": 1.5, "solo": 2}}
	m, _ := newCostTest(t, ModePopup, 200, burn, sessions...)
	if got, want := costOrder(t, m), []string{"▸ work:@2 +3", "work:@3", "work:@1"}; !slices.Equal(got, want) {
		t.Fatalf("rank order = %q, want %q", got, want)
	}
	m, _ = press(m, "b")
	if got, want := costOrder(t, m), []string{"▸ work:@2 +3", "work:@1", "work:@3"}; !slices.Equal(got, want) {
		t.Errorf("by burn = %q, want %q: the team's summed 3.5 before the solo 2.0", got, want)
	}
	// Expanded, the team's rows stay together where the unit stands.
	m, _ = press(m, " ")
	if got, want := costOrder(t, m), []string{"▾ work:@2", "alpha", "gamma", "beta", "work:@1", "work:@3"}; !slices.Equal(got, want) {
		t.Errorf("expanded by burn = %q, want %q", got, want)
	}
	// A burn that is not a finite, non-negative number counts as zero.
	burn.Burn["solo"], burn.Burn["W"] = math.NaN(), 0.5
	m, _ = newCostTest(t, ModePopup, 200, burn, sessions...)
	m, _ = press(m, "b")
	if got, want := costOrder(t, m), []string{"▸ work:@2 +3", "work:@3", "work:@1"}; !slices.Equal(got, want) {
		t.Errorf("NaN burn: rows = %q, want %q", got, want)
	}
}

func TestBIsTextInTheFilter(t *testing.T) {
	m, _ := newCostTest(t, ModePopup, 120, costBurn(), costSessions()...)
	m, _ = press(m, "/", "b")
	if s := statusLine(m); s != "/b" {
		t.Errorf("status line = %q, want the filter /b", s)
	}
	m, _ = press(m, "enter") // keeps the filter, leaves the input
	if _, _, note := costTable(t, m); note != "" {
		t.Errorf("b typed into the filter sorted the table: header ends %q", note)
	}
	if s := statusLine(m); !strings.Contains(s, "match /b") {
		t.Errorf("status line = %q, want the filter kept", s)
	}
}

func TestPasteDoesNotToggleBurn(t *testing.T) {
	m, _ := newCostTest(t, ModePopup, 120, costBurn(), costSessions()...)
	rank := costOrder(t, m)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b"), Paste: true})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("bbb"), Paste: true})
	if _, _, note := costTable(t, m); note != "" || !slices.Equal(costOrder(t, m), rank) {
		t.Errorf("a pasted b sorted the table: header ends %q, rows %q", note, costOrder(t, m))
	}
	// Typed fast, b is still a key.
	m, _ = m.Update(key("jb"))
	if _, _, note := costTable(t, m); note != "sorted by burn (b)" {
		t.Errorf("a typed b in a burst: header ends %q, want the sort", note)
	}
}

func TestBudgetStatusLine(t *testing.T) {
	const warning = "5h 62% → limit ~48m (resets 1h50m)"
	m, src := newCostTest(t, ModePopup, 120, Budget{}, costSessions()...)
	before := statusLine(m)
	src.budget.Status = warning
	m = refresh(t, m)
	if s := statusLine(m); !strings.HasPrefix(s, warning) || !strings.Contains(s, "enter jump") {
		t.Errorf("status line = %q, want the warning, then the help", s)
	}
	if src.budgetRead != 2 {
		t.Errorf("Budget read %d times, want once per refresh", src.budgetRead)
	}
	// Narrow, the warning comes first and the help is cut.
	m, _ = m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	if s := statusLine(m); !strings.HasPrefix(s, warning) || lipgloss.Width(s) > 40 {
		t.Errorf("at 40 columns status line = %q", s)
	}
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	// A notice, a filter or an error still takes the line.
	if m2, _ := press(m, "/", "x"); statusLine(m2) != "/x" {
		t.Errorf("filtering: status line = %q", statusLine(m2))
	}
	src.budget.Status = ""
	m = refresh(t, m)
	if s := statusLine(m); s != before {
		t.Errorf("status line = %q with no status, want the one of before %q", s, before)
	}
}

func TestBudgetErrorKeepsLastValue(t *testing.T) {
	const warning = "5h 62% → limit ~48m (resets 1h50m)"
	b := costBurn()
	b.Status = warning
	m, src := newCostTest(t, ModePopup, 120, b, costSessions()...)
	m, _ = press(m, "b")
	byBurn := costOrder(t, m)
	src.budget, src.budgetErr = Budget{}, errors.New("database is locked")
	m = refresh(t, m)
	if s := statusLine(m); !strings.Contains(s, "refresh failed: database is locked") {
		t.Errorf("status line = %q, want the error", s)
	}
	if got := costOrder(t, m); !slices.Equal(got, byBurn) {
		t.Errorf("after a failed read rows = %q, want the last burns' order %q", got, byBurn)
	}
	src.budget, src.budgetErr = b, nil
	m = refresh(t, m)
	if s := statusLine(m); !strings.HasPrefix(s, warning) {
		t.Errorf("after a good read status line = %q, want the warning", s)
	}
	src.budget = Budget{}
	m = refresh(t, m)
	if got := costOrder(t, m); !slices.Equal(got, []string{"work:@1", "work:@2", "work:@3"}) || strings.Contains(statusLine(m), warning) {
		t.Errorf("after a good empty read: rows %q, status %q", got, statusLine(m))
	}
}

func TestSidebarHasNoCostColumns(t *testing.T) {
	b := costBurn()
	b.Status = "5h 62% → limit ~48m (resets 1h50m)"
	for _, width := range []int{0, 30, 200} {
		m, src := newCostTest(t, ModeSidebar, width, b, costSessions()...)
		view := m.View()
		for _, absent := range []string{"COST", "CACHE", "$4.12", "12m", "limit", "b burn"} {
			if strings.Contains(view, absent) {
				t.Errorf("width %d: sidebar shows %q:\n%s", width, absent, view)
			}
		}
		m, _ = press(m, "b")
		if v := m.View(); v != view {
			t.Errorf("width %d: b changed the sidebar:\n%s\nwas:\n%s", width, v, view)
		}
		m, _ = m.Update(tickMsg{})
		_ = m
		if src.budgetRead != 0 {
			t.Errorf("width %d: sidebar read Budget %d times, want never", width, src.budgetRead)
		}
	}
}

func TestCostTextCannotDrawEscapes(t *testing.T) {
	s := costSessions()
	s[0].CostUSD = math.NaN() // a faulty source: never drawn as NaN
	b := Budget{Status: "\x1b[2J5h\x1b]52;c;eA==\x07 \u202ewarn\n", Burn: map[string]float64{"s1": math.Inf(1)}}
	m, _ := newCostTest(t, ModePopup, 120, b, s...)
	m, _ = press(m, "b")
	view := m.View()
	wantClean(t, view)
	if s := statusLine(m); !strings.HasPrefix(s, "?[2J5h?]52;c;eA==? ?warn?") {
		t.Errorf("status line = %q, want the status drawn cleaned", s)
	}
	if strings.Contains(view, "NaN") || strings.Contains(view, "Inf") {
		t.Errorf("view draws a non-number:\n%s", view)
	}
}

// costWide are teamSessions with a cost and a cache, and wide and
// combining text (CJK, emoji with a variation selector or a skin tone, a
// flag, an accent) in every text cell.
func costWide() ([]model.Session, *tmux.Fake, map[string]model.Task) {
	sessions := teamSessions()
	for i := range sessions {
		sessions[i].TeamMember = "数据库迁移✈️" + sessions[i].ID
		sessions[i].LastUpdate = "テスト✈️中🇺🇸 e\u0301 数据 " + sessions[i].ID
		sessions[i].Repo = "/src/リポジトリ👍🏽"
		sessions[i].Branch = "ブランチ☺️"
		sessions[i].CostUSD, sessions[i].CostKnown = 12.34, true
		sessions[i].CacheExpiresAt, sessions[i].CacheRecacheTokens = testNow.Add(-time.Minute), 84_600
	}
	fake := &tmux.Fake{Panes: []tmux.Pane{
		{ID: "%3", Session: "work", WindowID: "@1", WindowName: "编辑器☺️"},
		{ID: "%10", Session: "work", WindowID: "@2", WindowName: "サーバー✈️"},
	}}
	return sessions, fake, map[string]model.Task{"T-42": {ID: "T-42", Title: "認証✈️の修正"}}
}

// No column shifts at any width, collapsed or expanded, sorted or not,
// with wide and combining text in the cells.
func TestColumnsHoldWithWideNames(t *testing.T) {
	sessions, fake, tasks := costWide()
	burn := Budget{Burn: map[string]float64{"solo": 1}, Status: "5h 62% → limit ~48m (resets 1h50m)"}
	for _, mode := range []Mode{ModePopup, ModeSidebar} {
		for _, width := range []int{1, 8, 30, 40, 60, 80, 99, 100, 119, 120, 200} {
			for _, keys := range [][]string{nil, {" "}, {"b"}, {"b", " "}} {
				src := &fakeSource{sessions: sessions, counts: teamCounts(), tasks: tasks, budget: burn}
				m := refresh(t, New(src, fake, config.Default(), func() time.Time { return testNow }, mode))
				m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
				m, _ = press(m, keys...)
				titles, rows, _ := costTable(t, m)
				for _, l := range strings.Split(m.View(), "\n") {
					if w := lipgloss.Width(l); w > width {
						t.Errorf("mode %v width %d keys %q: line is %d cells: %q", mode, width, keys, w, l)
					}
				}
				if col := slices.Index(titles, "COST"); col >= 0 && len(rows) > 0 && len(rows[0]) > col && rows[0][col] != "$49.36" && rows[0][col] != "$12.34" {
					t.Errorf("mode %v width %d keys %q: first row's COST %q", mode, width, keys, rows[0][col])
				}
			}
		}
	}
}

// A cut cell is exactly as wide as its column, whatever its text: a
// variation selector widens the character before it.
func TestFitKeepsGraphemeWidths(t *testing.T) {
	for _, s := range []string{"✈️xabc", "☺️☺️☺️", "数据库迁移", "🇺🇸🇺🇸x", "e\u0301e\u0301e\u0301e\u0301", "👍🏽👍🏽"} {
		for w := 0; w <= 8; w++ {
			for _, right := range []bool{false, true} {
				if got := lipgloss.Width(fit(s, w, right)); got != w {
					t.Errorf("fit(%q, %d) = %q, %d cells", s, w, fit(s, w, right), got)
				}
			}
			if got := lipgloss.Width(truncate(s, w)); got > w {
				t.Errorf("truncate(%q, %d) = %q, %d cells", s, w, truncate(s, w), got)
			}
		}
	}
}

// pauseBudget is a Budget under a limit forecast that suggests pausing T-7.
func pauseBudget() Budget {
	return Budget{
		Status: "5h 62% → limit ~48m (resets 1h50m)", Burn: map[string]float64{"s1": 0.5, "s2": 3},
		PauseTask: "T-7", PauseLabel: "T-7 (P3) api-server", PauseTitle: "Fix the auth refresh", PauseLimit: "limit ~48m",
	}
}

// The notice line and the prompt of section 4.5 for pauseBudget.
const (
	pauseNoticeText = "limit ~48m before reset: P pauses T-7 (P3) api-server"
	pausePromptText = `pause T-7 "Fix the auth refresh"? it commits work in progress and closes its pane  y/N`
	pauseLineText   = "Paused T-7: curia resume T-7 brings it back"
)

// newPauseTest returns the popup over costSessions and budget, with pausing
// enabled unless setup says otherwise (nil: WithPause), 120 x 20, on clk,
// after its first refresh, and its source, whose Pause answers pauseLineText.
func newPauseTest(t *testing.T, clk *sayClock, mode Mode, budget Budget, setup func(Model) Model) (*dashPump, *fakeSource) {
	t.Helper()
	src := &fakeSource{sessions: costSessions(), budget: budget}
	src.pause = func(string) (string, error) { return pauseLineText, nil }
	m := New(src, &tmux.Fake{}, config.Default(), clk.now, mode)
	if setup == nil {
		setup = Model.WithPause
	}
	m = setup(m)
	tm := refresh(t, m)
	tm, _ = tm.Update(tea.WindowSizeMsg{Width: 120, Height: 20})
	return &dashPump{t: t, m: tm}, src
}

// pauseOpen presses P and lets the dwell pass.
func pauseOpen(p *dashPump, clk *sayClock) {
	p.t.Helper()
	p.key(key("P"))
	if s := statusLine(p.m); s != pausePromptText {
		p.t.Fatalf("after P status line = %q, want the prompt %q", s, pausePromptText)
	}
	clk.advance(300 * time.Millisecond)
}

func TestPauseNoticeShown(t *testing.T) {
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	lines := strings.Split(p.m.View(), "\n")
	if len(lines) != 20 || lines[len(lines)-2] != pauseNoticeText {
		t.Errorf("the line above the status line = %q, want %q:\n%s", lines[len(lines)-2], pauseNoticeText, p.m.View())
	}
	if s := lines[len(lines)-1]; !strings.HasPrefix(s, "5h 62% → limit ~48m (resets 1h50m)") {
		t.Errorf("status line = %q, want the forecast warning still", s)
	}
	// The table keeps its alignment above the notice.
	if _, rows, _ := costTable(t, p.m); len(rows) != 3 {
		t.Errorf("rows = %q, want the three sessions", rows)
	}
	// Without a suggestion there is no such line, and the table gets it back.
	p2, _ := newPauseTest(t, clk, ModePopup, Budget{Status: "5h 62% → limit ~48m (resets 1h50m)"}, nil)
	if v := p2.m.View(); strings.Contains(v, "P pauses") || len(strings.Split(v, "\n")) != 20 {
		t.Errorf("without a suggestion the view shows a pause notice or the wrong height:\n%s", v)
	}
	// A refresh that removes the suggestion removes the line.
	src.budget = Budget{}
	p.m = refresh(t, p.m)
	if v := p.m.View(); strings.Contains(v, "P pauses") {
		t.Errorf("after the suggestion went the notice stays:\n%s", v)
	}
}

func TestPauseNeedsKeyThenConfirm(t *testing.T) {
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	pauseOpen(p, clk)
	if len(src.paused) != 0 {
		t.Fatalf("P alone paused %q", src.paused)
	}
	p.key(key("y"))
	if !slices.Equal(src.paused, []string{"T-7"}) {
		t.Fatalf("Pause calls = %q, want [T-7]", src.paused)
	}
	if s := statusLine(p.m); s != pauseLineText {
		t.Errorf("status line = %q, want the pause's line %q", s, pauseLineText)
	}
	// The prompt is closed: another y does nothing.
	clk.advance(time.Second)
	p.key(key("y"))
	if len(src.paused) != 1 {
		t.Errorf("a second y paused again: %q", src.paused)
	}
}

func TestPauseAnyOtherKeyCancels(t *testing.T) {
	for _, k := range []string{"n", "esc", "enter", "P", "x", "j"} {
		clk := &sayClock{t: testNow}
		p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
		pauseOpen(p, clk)
		cmd := p.step(key(k))
		if isQuit(cmd) {
			t.Errorf("%s in the prompt quit the dashboard", k)
		}
		p.run(cmd)
		if s := statusLine(p.m); s != "pause canceled" {
			t.Errorf("%s in the prompt: status line = %q, want pause canceled", k, s)
		}
		clk.advance(time.Second)
		p.key(key("y"))
		if len(src.paused) != 0 {
			t.Errorf("%s then y paused %q", k, src.paused)
		}
	}
	// A click cancels too, and jumps nowhere.
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	pauseOpen(p, clk)
	var cmd tea.Cmd
	p.m, cmd = p.m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, Y: 1})
	if isQuit(cmd) || statusLine(p.m) != "pause canceled" {
		t.Errorf("a click in the prompt: quit %v, status %q", isQuit(cmd), statusLine(p.m))
	}
	clk.advance(time.Second)
	p.key(key("y"))
	if len(src.paused) != 0 {
		t.Errorf("a click then y paused %q", src.paused)
	}
}

func TestPauseConfirmHonorsDwell(t *testing.T) {
	// P and y in one read: the prompt opens and the y is dropped.
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	p.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Py")})
	if s := statusLine(p.m); s != pausePromptText || len(src.paused) != 0 {
		t.Errorf("Py in one read: status %q, calls %q, want the prompt and no call", s, src.paused)
	}
	// The rest of a read after the P is dropped: the user has not seen the
	// prompt yet, so a j behind it does not cancel it either.
	p2, _ := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	p2.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Pj")})
	if s := statusLine(p2.m); s != pausePromptText {
		t.Errorf("Pj in one read: status %q, want the prompt", s)
	}
	// A y in a read of several runes into the open prompt never confirms.
	clk.advance(time.Second)
	p.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yy")})
	if len(src.paused) != 0 || statusLine(p.m) != "pause canceled" {
		t.Errorf("yy in one read: calls %q, status %q", src.paused, statusLine(p.m))
	}
	// y 100 ms after P: too soon; the wait restarts as the quiet period, so
	// a held y's repeats never confirm.
	p.key(key("P"))
	for range 3 {
		clk.advance(100 * time.Millisecond)
		p.key(key("y"))
		if s := statusLine(p.m); s != pausePromptText || len(src.paused) != 0 {
			t.Fatalf("y 100 ms after the last key: status %q, calls %q, want the prompt still open", s, src.paused)
		}
	}
	clk.advance(500 * time.Millisecond) // within the quiet period of the last y
	if p.key(key("y")); len(src.paused) != 0 {
		t.Fatalf("y 500 ms after a y too soon paused %q", src.paused)
	}
	clk.advance(quietPeriod)
	if p.key(key("y")); !slices.Equal(src.paused, []string{"T-7"}) {
		t.Errorf("y after the quiet period: calls %q, want [T-7]", src.paused)
	}
}

func TestPausePasteDoesNotConfirm(t *testing.T) {
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	p.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Py"), Paste: true})
	if s := statusLine(p.m); strings.Contains(s, "pause T-7") || len(src.paused) != 0 {
		t.Errorf("a pasted Py: status %q, calls %q, want nothing", s, src.paused)
	}
	pauseOpen(p, clk)
	p.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y"), Paste: true})
	if s := statusLine(p.m); s != "pause canceled" || len(src.paused) != 0 {
		t.Errorf("a pasted y: status %q, calls %q", s, src.paused)
	}
	clk.advance(time.Second)
	p.key(key("y"))
	if len(src.paused) != 0 {
		t.Errorf("a y after the canceled prompt paused %q", src.paused)
	}
}

// The burst rule (inBurst): a y followed within burstGap by another key, as
// an unbracketed paste of "y" and a line break arrives, is refused and the
// prompt stays open, its wait restarted; the confirmation it waited for
// does nothing.
func TestPauseConfirmInBurstRefused(t *testing.T) {
	for _, next := range []string{"enter", "y", "a"} {
		clk := &sayClock{t: testNow}
		p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
		pauseOpen(p, clk)
		wait := p.step(key("y"))
		if wait == nil {
			t.Fatal("y after the dwell started no wait")
		}
		clk.advance(10 * time.Millisecond)
		p.key(key(next))
		p.run(wait)
		if len(src.paused) != 0 || statusLine(p.m) != pausePromptText {
			t.Errorf("y then %s 10 ms later: calls %q, status %q", next, src.paused, statusLine(p.m))
		}
		clk.advance(10 * time.Millisecond)
		if p.key(key("y")); len(src.paused) != 0 {
			t.Errorf("y then %s then y: calls %q", next, src.paused)
		}
		clk.advance(time.Second)
		if p.key(key("y")); !slices.Equal(src.paused, []string{"T-7"}) {
			t.Errorf("after %s: a y on its own, later, calls %q, want [T-7]", next, src.paused)
		}
	}
	// A key that comes after burstGap, while the wait's message is late,
	// does not stop the pause, which runs once.
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	pauseOpen(p, clk)
	wait := p.step(key("y"))
	clk.advance(time.Second)
	p.key(key("j"))
	p.run(wait)
	if !slices.Equal(src.paused, []string{"T-7"}) {
		t.Errorf("a late key: calls %q, want [T-7] once", src.paused)
	}
}

func TestPausePromptCanceledWhenSuggestionChanges(t *testing.T) {
	t9 := pauseBudget()
	t9.PauseTask, t9.PauseLabel, t9.PauseTitle = "T-9", "T-9 (P3) web", "Speed up the build"
	none := pauseBudget()
	none.PauseTask, none.PauseLabel, none.PauseTitle, none.PauseLimit = "", "", "", ""
	retitled := pauseBudget()
	retitled.PauseTitle = "Drop the auth tables"
	for _, c := range []struct {
		name string
		next Budget
		want string
	}{
		{"another task", t9, "the suggestion changed: now T-9 (P3) web"},
		{"none", none, "the suggestion changed: nothing to pause now"},
		{"retitled", retitled, "the suggestion changed: now T-7 (P3) api-server"},
	} {
		clk := &sayClock{t: testNow}
		p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
		pauseOpen(p, clk)
		src.budget = c.next
		p.m = refresh(t, p.m)
		if s := statusLine(p.m); s != c.want {
			t.Errorf("%s: status line = %q, want %q", c.name, s, c.want)
		}
		clk.advance(time.Second)
		p.key(key("y"))
		if len(src.paused) != 0 {
			t.Errorf("%s: y after the change paused %q", c.name, src.paused)
		}
	}
	// A y taken before the change waits out burstGap: the change still
	// cancels it.
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	pauseOpen(p, clk)
	wait := p.step(key("y"))
	src.budget = t9
	p.m = refresh(t, p.m)
	p.run(wait)
	if len(src.paused) != 0 || !strings.HasPrefix(statusLine(p.m), "the suggestion changed") {
		t.Errorf("a change while y waits: calls %q, status %q", src.paused, statusLine(p.m))
	}
	// The same suggestion with a later limit (a minute passed) keeps the
	// prompt open.
	clk = &sayClock{t: testNow}
	p, src = newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	pauseOpen(p, clk)
	src.budget.PauseLimit, src.budget.Status = "limit ~47m", "5h 63% → limit ~47m (resets 1h49m)"
	p.m = refresh(t, p.m)
	if s := statusLine(p.m); s != pausePromptText {
		t.Errorf("a new limit for the same task: status %q, want the prompt", s)
	}
	if p.key(key("y")); !slices.Equal(src.paused, []string{"T-7"}) {
		t.Errorf("y: calls %q, want [T-7]", src.paused)
	}
}

func TestPauseWithoutSuggestion(t *testing.T) {
	clk := &sayClock{t: testNow}
	p, _ := newPauseTest(t, clk, ModePopup, Budget{Status: "next 2h ≈ $9.00 at the current rate"}, nil)
	p.key(key("P"))
	if s := statusLine(p.m); s != "nothing to pause: no limit forecast" {
		t.Errorf("status line = %q", s)
	}
	clk.advance(time.Second)
	p.key(key("y")) // no prompt: nothing to confirm (the source allows no pause)
}

func TestPauseDisabledInSidebarAndWithoutWithPause(t *testing.T) {
	for _, c := range []struct {
		name  string
		mode  Mode
		setup func(Model) Model
		want  string
	}{
		{"popup without WithPause", ModePopup, func(m Model) Model { return m }, "pausing is not available here"},
		{
			"popup read-only", ModePopup, func(m Model) Model {
				return m.WithoutPause("curia ui cannot run inside an agent session (CLAUDECODE is set)")
			},
			"curia ui cannot run inside an agent session (CLAUDECODE is set)",
		},
		{"WithoutPause after WithPause", ModePopup, func(m Model) Model { return m.WithPause().WithoutPause("no terminal") }, "no terminal"},
		{"sidebar", ModeSidebar, Model.WithPause, "pausing is not available here"},
	} {
		clk := &sayClock{t: testNow}
		p, src := newPauseTest(t, clk, c.mode, pauseBudget(), c.setup)
		src.pause = nil // any call fails the test
		if v := p.m.View(); strings.Contains(v, "P pauses") {
			t.Errorf("%s: the view offers P:\n%s", c.name, v)
		}
		p.key(key("P"))
		if s := statusLine(p.m); s != c.want {
			t.Errorf("%s: status line = %q, want %q", c.name, s, c.want)
		}
		clk.advance(time.Second)
		p.key(key("y"))
	}
}

func TestPauseErrorShown(t *testing.T) {
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	fail := true
	src.pause = func(string) (string, error) {
		if fail {
			return "", errors.New("T-7 not paused: the WIP commit failed: \x1b[31mhook said no\x1b[0m\nsecond line")
		}
		return pauseLineText, nil
	}
	before := strings.Split(p.m.View(), "\n")
	pauseOpen(p, clk)
	p.key(key("y"))
	view := p.m.View()
	wantClean(t, view)
	if s := statusLine(p.m); s != "T-7 not paused: the WIP commit failed: ?[31mhook said no?[0m?second line" {
		t.Errorf("status line = %q, want the error cleaned", s)
	}
	after := strings.Split(view, "\n")
	if !slices.Equal(before[:len(before)-1], after[:len(after)-1]) {
		t.Errorf("a failed pause changed the view:\n%s\nwas:\n%s", view, strings.Join(before, "\n"))
	}
	fail = false
	clk.advance(time.Second)
	pauseOpen(p, clk)
	p.key(key("y"))
	if !slices.Equal(src.paused, []string{"T-7", "T-7"}) || statusLine(p.m) != pauseLineText {
		t.Errorf("a second P: calls %q, status %q", src.paused, statusLine(p.m))
	}
}

func TestPauseNeverCalledWithoutKeys(t *testing.T) {
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	for range 100 {
		clk.advance(time.Second)
		p.m = refresh(t, p.m)
	}
	if len(src.paused) != 0 || src.budgetRead < 100 {
		t.Errorf("after 100 refreshes: calls %q, budget reads %d", src.paused, src.budgetRead)
	}
}

// While a pause runs, every key but Ctrl+C is refused, so its result is
// seen and a second pause cannot start; the result arrives even then.
func TestPauseInFlightRefusesKeys(t *testing.T) {
	clk := &sayClock{t: testNow}
	p, src := newPauseTest(t, clk, ModePopup, pauseBudget(), nil)
	pauseOpen(p, clk)
	wait := p.step(key("y"))
	var call tea.Cmd
	p.m, call = p.m.Update(wait())
	if call == nil {
		t.Fatal("the confirmation started no pause")
	}
	for _, k := range []string{"P", "y", "q", "esc", "t", "enter"} {
		clk.advance(time.Second)
		cmd := p.step(key(k))
		if isQuit(cmd) || statusLine(p.m) != "a pause is running; wait for its result" {
			t.Errorf("%s while pausing: quit %v, status %q", k, isQuit(cmd), statusLine(p.m))
		}
	}
	if !isQuit(p.step(tea.KeyMsg{Type: tea.KeyCtrlC})) {
		t.Error("ctrl+c while pausing does not quit")
	}
	p.run(call)
	if !slices.Equal(src.paused, []string{"T-7"}) || statusLine(p.m) != pauseLineText {
		t.Errorf("after the pause: calls %q, status %q", src.paused, statusLine(p.m))
	}
}

// The notice line and the prompt fit every width, with wide and hostile
// text, and the table above them keeps its alignment and the height.
func TestPauseNoticeFitsEveryWidth(t *testing.T) {
	sessions, fake, tasks := costWide()
	b := pauseBudget()
	b.PauseLabel, b.PauseTitle = "T-7 (P3) リポジトリ👍🏽-"+hostile, "認証✈️の修正 "+hostile
	for _, width := range []int{1, 8, 30, 40, 60, 80, 99, 100, 120, 200} {
		for _, prompt := range []bool{false, true} {
			clk := &sayClock{t: testNow}
			src := &fakeSource{sessions: sessions, counts: teamCounts(), tasks: tasks, budget: b}
			m := refresh(t, New(src, fake, config.Default(), clk.now, ModePopup).WithPause())
			m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 12})
			if prompt {
				m, _ = press(m, "P")
			}
			view := m.View()
			lines := strings.Split(view, "\n")
			for _, l := range lines {
				if w := lipgloss.Width(l); w > width {
					t.Errorf("width %d prompt %v: line is %d cells: %q", width, prompt, w, l)
				}
			}
			wantClean(t, view)
			if len(lines) != 12 {
				t.Errorf("width %d prompt %v: %d lines, want the height 12", width, prompt, len(lines))
			}
			costTable(t, m)
			if s := lines[len(lines)-1]; width >= 80 && prompt && !strings.HasSuffix(s, "y/N") {
				t.Errorf("width %d: the prompt %q does not end in y/N", width, s)
			}
		}
	}
}

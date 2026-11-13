package ui

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
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
	if len(popupColumns) != 9 {
		t.Errorf("popup has %d columns, want 9", len(popupColumns))
	}
	if h := strings.Fields(strings.Split(view, "\n")[0]); !slices.Equal(h, []string{
		"SESSION:WINDOW", "REPO@BRANCH", "MODEL", "CTX", "SUB", "DEC", "LAST", "UPDATE", "AGE",
	}) {
		t.Errorf("header = %q, want DEC between SUB and LAST UPDATE", h)
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
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 5}) // header, 3 rows, status
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

// fakeChild stands in for the decide view: it records every message it
// gets, starts with a childPing and closes on "q".
type fakeChild struct {
	id  string
	got *[]tea.Msg
}

// childPing is the message a fakeChild's Init sends.
type childPing struct{ id string }

func (c fakeChild) Init() tea.Cmd { return func() tea.Msg { return childPing{c.id} } }

func (c fakeChild) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	*c.got = append(*c.got, msg)
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "q" {
		return c, func() tea.Msg { return DecideClosedMsg{} }
	}
	return c, nil
}

func (c fakeChild) View() string { return "decide view of " + c.id }

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
	src := &fakeSource{sessions: sessions, counts: map[string]model.OpenCount{"S1": {Blocking: 12, Assume: 3}}}
	m := refresh(t, New(src, &tmux.Fake{}, config.Default(), func() time.Time { return testNow }, ModePopup))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	view := m.View()
	for _, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w > 60 {
			t.Errorf("line is %d cells, want at most 60: %q", w, l)
		}
	}
	if got := decCell(t, view, "S1"); got != "15" {
		t.Errorf("S1's DEC cell at width 60 = %q, want 15:\n%s", got, view)
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

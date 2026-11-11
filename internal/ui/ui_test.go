package ui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

// testNow is the fixed clock of the dashboard tests.
var testNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// fakeSource returns sessions, or err when it is set.
type fakeSource struct {
	sessions []model.Session
	err      error
}

func (f *fakeSource) Sessions(context.Context) ([]model.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.sessions), nil
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

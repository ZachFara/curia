package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/hook"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// hookResult is what one in-process run of `curia hook` produced.
type hookResult struct {
	exitCode, exits int
	output          string
	log             []string
}

// hookHome points HOME (and so the default log and config paths) and
// CURIA_DB at a temp dir, outside tmux, and returns the database path.
func hookHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMUX_PANE", "")
	db := filepath.Join(home, "curia.db")
	t.Setenv("CURIA_DB", db)
	return db
}

// runHookCmd runs `curia <args>` in-process with stdin and a fake exit func.
// Its output is what was written to stdout, then what was written to stderr.
func runHookCmd(t *testing.T, stdin io.Reader, args ...string) hookResult {
	t.Helper()
	s := hookRunStreams(t, stdin, args...)
	return hookResult{exitCode: s.exitCode, exits: s.exits, output: s.stdout + s.stderr, log: s.log}
}

func readLog(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(config.Default().LogPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// assertQuietExitZero checks the hook exited 0 exactly once and printed nothing.
func assertQuietExitZero(t *testing.T, res hookResult) {
	t.Helper()
	if res.exitCode != 0 || res.exits != 1 {
		t.Errorf("exit code %d after %d exit calls, want one exit(0)", res.exitCode, res.exits)
	}
	if res.output != "" {
		t.Errorf("hook wrote to stdout/stderr: %q", res.output)
	}
}

func TestMalformedPayloadExitsZero(t *testing.T) {
	hookHome(t)
	res := runHookCmd(t, strings.NewReader("{"), "hook", "PostToolUse")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 {
		t.Fatalf("log has %d lines, want 1: %q", len(res.log), res.log)
	}
	if !strings.Contains(res.log[0], "PostToolUse") {
		t.Errorf("log line does not name the event: %q", res.log[0])
	}
}

// TestHookWatchdogExitsZero: stdin has no deadline, so a payload that never
// ends must not keep the hook (and Claude Code waiting on it) forever.
func TestHookWatchdogExitsZero(t *testing.T) {
	hookHome(t)
	prevDeadline, prevExit := hookDeadline, hookExit
	hookDeadline = 100 * time.Millisecond
	exits := make(chan int, 2)
	hookExit = func(code int) { exits <- code }
	stdin, writer := io.Pipe()
	rootCmd.SetIn(stdin)
	rootCmd.SetArgs([]string{"hook", "Stop"})
	t.Cleanup(func() {
		hookDeadline, hookExit = prevDeadline, prevExit
		rootCmd.SetIn(nil)
		rootCmd.SetArgs(nil)
	})
	done := make(chan error, 1)
	go func() { done <- rootCmd.Execute() }()

	select {
	case code := <-exits:
		if code != 0 {
			t.Errorf("exit code %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook still running 5s after its 100ms deadline")
	}
	_ = writer.Close() // a real hook is gone by now; let this one return
	if err := <-done; err != nil {
		t.Errorf("Execute returned %v", err)
	}
	select {
	case <-exits:
		t.Error("hook exited a second time")
	default:
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "timed out") || !strings.Contains(log[0], "Stop") {
		t.Errorf("log = %q, want one line saying the Stop hook timed out", log)
	}
}

func TestHookExitsZeroWhenStoreLocked(t *testing.T) {
	db := hookHome(t)
	st, err := store.Open(db) // create the file in WAL mode first
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	conn, err := raw.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "ROLLBACK") }()

	start := time.Now()
	res := runHookCmd(t, strings.NewReader(`{"session_id":"A","cwd":"/repo"}`), "hook", "PostToolUse")
	elapsed := time.Since(start)

	assertQuietExitZero(t, res)
	if elapsed > 3*time.Second {
		t.Errorf("hook took %v with the store locked, want under 3s", elapsed)
	}
	if len(res.log) != 1 || !strings.Contains(res.log[0], "locked") {
		t.Errorf("log = %q, want one line reporting the lock", res.log)
	}
}

func TestHookRecordsSession(t *testing.T) {
	db := hookHome(t)
	body := `{"session_id":"A","transcript_path":"/nonexistent/t.jsonl","cwd":"` + t.TempDir() +
		`","hook_event_name":"SessionStart","source":"startup"}`
	res := runHookCmd(t, strings.NewReader(body), "hook", "SessionStart")
	assertQuietExitZero(t, res)
	if res.log != nil {
		t.Errorf("log after a good event: %q", res.log)
	}

	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sess, found, err := st.GetSession(context.Background(), "A")
	if err != nil || !found {
		t.Fatalf("GetSession = found %v, err %v", found, err)
	}
	if sess.State != model.StateIdle || sess.Pane != "" {
		t.Errorf("session = %+v, want idle with no pane", sess)
	}
}

func TestHookBadInvocationsExitZero(t *testing.T) {
	tests := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"unknown event", `{"session_id":"A"}`, []string{"hook", "PreCompact"}},
		{"no event", `{"session_id":"A"}`, []string{"hook"}},
		{"two events", `{"session_id":"A"}`, []string{"hook", "Stop", "Stop"}},
		{"flag-like event", `{"session_id":"A"}`, []string{"hook", "--bogus"}},
		{
			"oversized payload", `{"session_id":"A","pad":"` + strings.Repeat("x", maxPayloadBytes) + `"}`,
			[]string{"hook", "Stop"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := hookHome(t)
			res := runHookCmd(t, strings.NewReader(tc.stdin), tc.args...)
			assertQuietExitZero(t, res)
			if len(res.log) != 1 {
				t.Errorf("log = %q, want one line", res.log)
			}
			if _, err := os.Stat(db); err == nil {
				st, err := store.Open(db)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = st.Close() }()
				if all, _ := st.ListSessions(context.Background(), store.ListFilter{IncludeEnded: true}); len(all) != 0 {
					t.Errorf("sessions recorded: %+v", all)
				}
			}
		})
	}
}

func TestHookPanicExitsZero(t *testing.T) {
	hookHome(t)
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		panic("boom\nforged line")
	}
	t.Cleanup(func() { hookRun = prev })

	res := runHookCmd(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Stop")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 || !strings.Contains(res.log[0], "boom") {
		t.Errorf("log = %q, want one line with the panic", res.log)
	}
}

func TestHookLogCannotBeForged(t *testing.T) {
	hookHome(t)
	res := runHookCmd(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Bogus\ntime=2026 level=INFO msg=forged")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 {
		t.Errorf("log = %q, want the untrusted text kept on one line", res.log)
	}
}

// markerCalls returns the SetWindowOption calls the fake recorded.
func markerCalls(f *tmux.Fake) []string {
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "SetWindowOption") {
			out = append(out, c)
		}
	}
	return out
}

// markerHook is a hook run in pane %1 of window @7 against a fake tmux.
type markerHook struct {
	t    *testing.T
	db   string
	fake *tmux.Fake
}

func newMarkerHook(t *testing.T) *markerHook {
	t.Helper()
	db := hookHome(t)
	t.Setenv("TMUX_PANE", "%1")
	fake := useFakeTmux(t)
	fake.Panes = []tmux.Pane{{ID: "%1", Session: "s", WindowID: "@7"}}
	return &markerHook{t: t, db: db, fake: fake}
}

// event runs one hook event for session id and returns the marker calls it made.
func (m *markerHook) event(kind, id string) []string {
	m.t.Helper()
	m.fake.Calls = nil
	body := `{"session_id":"` + id + `","cwd":"` + m.t.TempDir() + `","message":"needs you"}`
	assertQuietExitZero(m.t, runHookCmd(m.t, strings.NewReader(body), "hook", kind))
	return markerCalls(m.fake)
}

func (m *markerHook) seen() {
	m.t.Helper()
	dbPath := m.db
	st, err := store.Open(dbPath)
	if err != nil {
		m.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.MarkSeen(context.Background(), "%1", time.Now()); err != nil {
		m.t.Fatal(err)
	}
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("marker calls = %q, want %q", got, want)
	}
}

// Ruling 37: a session at its empty prompt is listed, but not counted in
// the badge and not offered by next; its first prompt makes it working.
func TestHookFreshSessionIsIdleUntilFirstPrompt(t *testing.T) {
	m := newMarkerHook(t)
	wantCalls(t, m.event("SessionStart", "A"))
	if s := getSession(t, m.db, "A"); s.State != model.StateIdle {
		t.Errorf("A after SessionStart = %s, want idle", s.State)
	}
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status = %q, %v, want an empty badge", out, err)
	}
	if out, err := runRoot(t, "ls"); err != nil || !strings.Contains(out, "idle") {
		t.Errorf("ls = %q, %v, want A listed as idle", out, err)
	}
	m.fake.Calls = nil
	if _, err := runRoot(t, "next"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.fake.Calls, "DisplayMessage nothing needs you") {
		t.Errorf("next made calls %q, want nothing needs you", m.fake.Calls)
	}
	wantCalls(t, m.event("UserPromptSubmit", "A"))
	if s := getSession(t, m.db, "A"); s.State != model.StateWorking {
		t.Errorf("A after UserPromptSubmit = %s, want working", s.State)
	}
}

func TestHookToolEventsOnWorkingSessionMakeNoCall(t *testing.T) {
	m := newMarkerHook(t)
	wantCalls(t, m.event("SessionStart", "A"))
	wantCalls(t, m.event("PreToolUse", "A"))
	wantCalls(t, m.event("PostToolUse", "A"))
}

func TestHookNotificationOnSeenSessionSetsRed(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.seen()
	wantCalls(t, m.event("Notification", "A"), "SetWindowOption @7 @curia_marker red")
}

// Ruling 38: an event that would turn the window the user is looking at red
// marks its pane seen instead.
func TestHookFocusedPaneIsMarkedSeenNotRed(t *testing.T) {
	m := newMarkerHook(t)
	m.fake.Focused = true
	m.event("SessionStart", "A")
	wantCalls(t, m.event("Notification", "A"))
	if !slices.Contains(m.fake.Calls, "PaneFocused %1") {
		t.Errorf("calls = %q, want a PaneFocused %%1 check", m.fake.Calls)
	}
	s := getSession(t, m.db, "A")
	if s.State != model.StateWaiting || s.SeenAt.Before(s.UpdatedAt) {
		t.Errorf("A = %s seen %v updated %v, want waiting and seen", s.State, s.SeenAt, s.UpdatedAt)
	}
	// A later event that would turn it red is checked again.
	wantCalls(t, m.event("Notification", "A"))
	if !slices.Contains(m.fake.Calls, "PaneFocused %1") {
		t.Errorf("calls = %q, want a second check", m.fake.Calls)
	}
}

func TestHookUnfocusedPaneTurnsRed(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	wantCalls(t, m.event("Notification", "A"), "SetWindowOption @7 @curia_marker red")
	if !slices.Contains(m.fake.Calls, "PaneFocused %1") {
		t.Errorf("calls = %q, want a PaneFocused %%1 check", m.fake.Calls)
	}
	// Already red: the marker does not change, so tmux is not asked again.
	m.event("Notification", "A")
	if slices.Contains(m.fake.Calls, "PaneFocused %1") {
		t.Errorf("calls = %q, want no focus check on a red session", m.fake.Calls)
	}
}

func TestHookEventsThatDoNotTurnRedMakeNoFocusCheck(t *testing.T) {
	m := newMarkerHook(t)
	for _, kind := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"} {
		m.event(kind, "A")
		if slices.Contains(m.fake.Calls, "PaneFocused %1") {
			t.Errorf("%s: calls = %q, want no focus check", kind, m.fake.Calls)
		}
	}
}

// focusFailingTmux is a Fake whose focus check fails.
type focusFailingTmux struct{ *tmux.Fake }

func (f focusFailingTmux) PaneFocused(context.Context, string) (bool, error) {
	return false, errors.New("no client")
}

func TestHookFocusCheckFailureIsLoggedAndTurnsRed(t *testing.T) {
	m := newMarkerHook(t)
	newTmuxClient = func() tmux.Client { return focusFailingTmux{m.fake} } // useFakeTmux restores it
	m.event("SessionStart", "A")
	wantCalls(t, m.event("Notification", "A"), "SetWindowOption @7 @curia_marker red")
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "no client") || !strings.Contains(log[0], "%1") {
		t.Errorf("log = %q, want one line with the failed check", log)
	}
}

func TestHookPostToolUseClearsRed(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.event("Notification", "A")
	wantCalls(t, m.event("PostToolUse", "A"), "SetWindowOption @7 @curia_marker ")
}

func TestHookSessionEndClearsRed(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.event("Notification", "A")
	wantCalls(t, m.event("SessionEnd", "A"), "SetWindowOption @7 @curia_marker ")
}

func TestHookSessionStartEndingRedSessionClearsIt(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.event("Notification", "A")
	// B takes over pane %1; A is ended inside hook.Run while red.
	wantCalls(t, m.event("SessionStart", "B"), "SetWindowOption @7 @curia_marker ")
}

// A session already running at install time is first seen mid-run; it gets
// its window like a started one, so its marker shows (Ruling 29).
func TestHookFirstSeenMidRunGetsWindowAndMarker(t *testing.T) {
	m := newMarkerHook(t)
	wantCalls(t, m.event("Notification", "M"), "SetWindowOption @7 @curia_marker red")
	if s := getSession(t, m.db, "M"); s.TmuxSession != "s" || s.TmuxWindow != "@7" || s.State != model.StateWaiting {
		t.Errorf("M = %+v, want waiting in s/@7", s)
	}
}

func TestHookFirstSeenMidRunEndingRedSessionClearsIt(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.event("Notification", "A")
	// B, already running, shows up in pane %1; A is ended inside hook.Run while red.
	wantCalls(t, m.event("PostToolUse", "B"), "SetWindowOption @7 @curia_marker ")
}

// useFakeTmux makes the commands use a tmux.Fake and returns it.
func useFakeTmux(t *testing.T) *tmux.Fake {
	t.Helper()
	fake := &tmux.Fake{}
	prev := newTmuxClient
	newTmuxClient = func() tmux.Client { return fake }
	t.Cleanup(func() { newTmuxClient = prev })
	return fake
}

func TestHookAppliesMarker(t *testing.T) {
	hookHome(t)
	fake := useFakeTmux(t)
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		return hook.Result{Session: model.Session{ID: "A", TmuxWindow: "@7", State: model.StateWaiting, UpdatedAt: time.Now()}}, nil
	}
	t.Cleanup(func() { hookRun = prev })

	res := runHookCmd(t, strings.NewReader(`{}`), "hook", "Stop")
	assertQuietExitZero(t, res)
	want := []string{"SetWindowOption @7 @curia_marker "}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %q, want %q", fake.Calls, want)
	}
}

func TestHookWithoutWindowAppliesNothing(t *testing.T) {
	hookHome(t)
	fake := useFakeTmux(t)
	res := runHookCmd(t, strings.NewReader(`{"session_id":"A","cwd":"`+t.TempDir()+`"}`), "hook", "SessionStart")
	assertQuietExitZero(t, res)
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %q, want none", fake.Calls)
	}
}

// failingTmux fails every marker write.
type failingTmux struct{ tmux.Fake }

func (f *failingTmux) SetWindowOption(context.Context, string, string, string) error {
	return errors.New("no server")
}

func TestHookAttentionFailureIsLoggedExitZero(t *testing.T) {
	hookHome(t)
	prevC := newTmuxClient
	newTmuxClient = func() tmux.Client { return &failingTmux{} }
	t.Cleanup(func() { newTmuxClient = prevC })
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		return hook.Result{Session: model.Session{ID: "A", TmuxWindow: "@7", State: model.StateWaiting, UpdatedAt: time.Now()}}, nil
	}
	t.Cleanup(func() { hookRun = prev })

	res := runHookCmd(t, strings.NewReader(`{}`), "hook", "Stop")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 || !strings.Contains(res.log[0], "no server") {
		t.Errorf("log = %q, want one line with the failure", res.log)
	}
}

func TestHookSessionEndOfNonRedSessionMakesNoCall(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	wantCalls(t, m.event("SessionEnd", "A"))
}

func TestHookMovedSessionAppliesBothWindows(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.event("Notification", "A")
	// The pane is now in window @9; A starts again (resume) there.
	m.fake.Panes = []tmux.Pane{{ID: "%1", Session: "s", WindowID: "@9"}}
	wantCalls(t, m.event("SessionStart", "A"),
		"SetWindowOption @9 @curia_marker ", "SetWindowOption @7 @curia_marker ")
}

func TestHookFailedBeforeReadIsLoggedAndApplies(t *testing.T) {
	db := hookHome(t)
	fake := useFakeTmux(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`DROP TABLE sessions`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		return hook.Result{Session: model.Session{ID: "A", TmuxWindow: "@7"}}, nil
	}
	t.Cleanup(func() { hookRun = prev })

	res := runHookCmd(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Stop")
	assertQuietExitZero(t, res)
	// Both the read before the event and the marker update (which reads the
	// same table) fail and are logged; the marker update was attempted.
	if len(res.log) != 2 || !strings.Contains(res.log[0], "before event") || !strings.Contains(res.log[1], "attention marker") {
		t.Errorf("log = %q, want the failed read, then the failed marker update", res.log)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %q, want none (update failed before tmux)", fake.Calls)
	}
}

// hookStreams is what one in-process run of `curia hook` wrote, with stdout
// and stderr kept apart.
type hookStreams struct {
	exitCode, exits int
	stdout, stderr  string
	log             []string
}

// hookRunStreams runs `curia <args>` in-process like runHookCmd, capturing
// stdout and stderr separately.
func hookRunStreams(t *testing.T, stdin io.Reader, args ...string) hookStreams {
	t.Helper()
	res := hookStreams{exitCode: -1}
	prev := hookExit
	hookExit = func(code int) { res.exitCode = code; res.exits++ }
	var out, errOut bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		hookExit = prev
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	if err := rootCmd.Execute(); err != nil {
		t.Errorf("Execute returned %v", err)
	}
	res.stdout, res.stderr = out.String(), errOut.String()
	res.log = readLog(t)
	return res
}

// hookWantExitZero checks the run exited 0 exactly once and wrote nothing
// to stderr.
func hookWantExitZero(t *testing.T, res hookStreams) {
	t.Helper()
	if res.exitCode != 0 || res.exits != 1 {
		t.Errorf("exit code %d after %d exit calls, want one exit(0)", res.exitCode, res.exits)
	}
	if res.stderr != "" {
		t.Errorf("hook wrote to stderr: %q", res.stderr)
	}
}

// hookQueueAnswer raises a blocking ticket from session in the store at db
// and answers it with option A, so its answer is queued; it returns the id.
func hookQueueAnswer(t *testing.T, db, session string) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	r := decision.Request{
		Question: "Which store for events?",
		Context:  "internal/events/store.go writes about 200 events a minute.",
		Options: []model.Option{
			{Key: "A", Label: "Postgres table", Tradeoffs: "Durable."},
			{Key: "B", Label: "Redis stream", Tradeoffs: "No migration."},
		},
		Recommendation: "A",
		Rationale:      "Replays need 30 days of history.",
		Reversibility:  string(model.ReversCostly),
		Mode:           string(model.ModeBlocking),
		Scope:          string(model.ScopeRepo),
	}
	ctx := context.Background()
	now := time.Now()
	d, _, err := st.CreateDecision(ctx, decision.New(r, decision.Caller{SessionID: session}, now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.AnswerDecision(ctx, d.ID, model.Answer{Key: "A", Reasoning: "we already run it", Via: model.AnsweredCLI}, now); err != nil {
		t.Fatal(err)
	}
	return d.ID
}

// hookDelivery returns decision id's delivery state and path.
func hookDelivery(t *testing.T, db, id string) (model.Delivery, model.DeliveryVia) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	d, found, err := st.GetDecision(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetDecision(%s) = found %v, err %v", id, found, err)
	}
	return d.Delivery, d.DeliveredVia
}

func TestStdoutOnlyOnBlockedStop(t *testing.T) {
	db := hookHome(t)
	body := `{"session_id":"A","cwd":"` + t.TempDir() + `"}`
	hookWantExitZero(t, hookRunStreams(t, strings.NewReader(body), "hook", "SessionStart"))
	id := hookQueueAnswer(t, db, "A")

	res := hookRunStreams(t, strings.NewReader(body), "hook", "PostToolUse")
	hookWantExitZero(t, res)
	if res.stdout != "" {
		t.Errorf("PostToolUse with a queued answer wrote %q, want nothing", res.stdout)
	}
	if got, _ := hookDelivery(t, db, id); got != model.DeliveryQueued {
		t.Errorf("after PostToolUse %s is %s, want queued", id, got)
	}

	res = hookRunStreams(t, strings.NewReader(body), "hook", "Stop")
	hookWantExitZero(t, res)
	want := `{"decision":"block","reason":"Curia: decisions answered while you were working.\nDecision D-1 answered: option A [Postgres table]. Reasoning: we already run it. Continue."}` + "\n"
	if res.stdout != want {
		t.Errorf("Stop wrote\n %q\nwant\n %q", res.stdout, want)
	}
	// M8: the one log line names what the Stop delivered.
	if len(res.log) != 1 || !strings.Contains(res.log[0], `level=INFO msg="hook Stop delivered answers" ids=D-1`) {
		t.Errorf("log = %q, want one INFO line naming D-1", res.log)
	}
	if got, via := hookDelivery(t, db, id); got != model.DeliveryDelivered || via != model.ViaStopHook {
		t.Errorf("after Stop %s is %s via %q, want delivered via stop_hook", id, got, via)
	}

	res = hookRunStreams(t, strings.NewReader(body), "hook", "Stop")
	hookWantExitZero(t, res)
	if res.stdout != "" {
		t.Errorf("Stop with nothing queued wrote %q, want nothing", res.stdout)
	}
}

func TestHookExitsZeroAndSilentWhenDecisionQueryFails(t *testing.T) {
	hookHome(t)
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		return hook.Result{Session: model.Session{ID: "A", State: model.StateIdle}}, errors.New("count open blocking: boom")
	}
	t.Cleanup(func() { hookRun = prev })

	res := hookRunStreams(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Stop")
	hookWantExitZero(t, res)
	if res.stdout != "" {
		t.Errorf("stdout = %q, want nothing", res.stdout)
	}
	if len(res.log) != 1 || !strings.Contains(res.log[0], "boom") {
		t.Errorf("log = %q, want one line with the error", res.log)
	}
}

// A claim that committed is delivered even when Run also reports an error
// (a transcript it could not read): the answer is no longer queued.
func TestHookEmitsBlockBesideAnError(t *testing.T) {
	hookHome(t)
	line := `{"decision":"block","reason":"x"}` + "\n"
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		return hook.Result{Session: model.Session{ID: "A", State: model.StateWorking}, Stdout: []byte(line)}, errors.New("transcript: boom")
	}
	t.Cleanup(func() { hookRun = prev })

	res := hookRunStreams(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Stop")
	hookWantExitZero(t, res)
	if res.stdout != line {
		t.Errorf("stdout = %q, want %q", res.stdout, line)
	}
	if len(res.log) != 1 || !strings.Contains(res.log[0], "boom") {
		t.Errorf("log = %q, want one line with the error", res.log)
	}
}

func TestNothingPrintedAfterWatchdog(t *testing.T) {
	hookHome(t)
	prevDeadline, prev := hookDeadline, hookRun
	hookDeadline = 10 * time.Millisecond
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		time.Sleep(50 * time.Millisecond)
		return hook.Result{
			Session: model.Session{ID: "A", State: model.StateWorking},
			Stdout:  []byte(`{"decision":"block","reason":"x"}` + "\n"),
		}, nil
	}
	t.Cleanup(func() { hookDeadline, hookRun = prevDeadline, prev })

	res := hookRunStreams(t, strings.NewReader(`{"session_id":"A"}`), "hook", "Stop")
	hookWantExitZero(t, res)
	if res.stdout != "" {
		t.Errorf("stdout = %q, want nothing after the watchdog", res.stdout)
	}
	if len(res.log) != 1 || !strings.Contains(res.log[0], "hook timed out") {
		t.Errorf("log = %q, want one line saying the hook timed out", res.log)
	}
}

// hookOrderTmux is a Fake that records, at each tmux call, what the hook
// had written to stdout by then.
type hookOrderTmux struct {
	*tmux.Fake
	stdout *bytes.Buffer
	seen   []string
}

func (f *hookOrderTmux) PaneFocused(ctx context.Context, pane string) (bool, error) {
	f.seen = append(f.seen, "PaneFocused:"+f.stdout.String())
	return f.Fake.PaneFocused(ctx, pane)
}

func (f *hookOrderTmux) SetWindowOption(ctx context.Context, window, name, value string) error {
	f.seen = append(f.seen, "SetWindowOption:"+f.stdout.String())
	return f.Fake.SetWindowOption(ctx, window, name, value)
}

// The line is written before any tmux call, so a slow tmux cannot widen the
// gap between claiming an answer and telling Claude.
func TestHookEmitsBeforeTmux(t *testing.T) {
	db := hookHome(t)
	t.Setenv("TMUX_PANE", "%1")
	fake := useFakeTmux(t)
	fake.Panes = []tmux.Pane{{ID: "%1", Session: "s", WindowID: "@7"}}
	var out bytes.Buffer
	order := &hookOrderTmux{Fake: fake, stdout: &out}
	newTmuxClient = func() tmux.Client { return order } // useFakeTmux restores it
	seedSessions(t, db, model.Session{
		ID: "A", Agent: "claude", Pane: "%1", TmuxSession: "s", TmuxWindow: "@7",
		State: model.StateIdle, StartedAt: time.Now(), UpdatedAt: time.Now(), SeenAt: time.Now(),
	})
	line := `{"decision":"block","reason":"x"}` + "\n"
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		// A red window: the hook checks focus, then sets the marker.
		s := model.Session{ID: "A", Pane: "%1", TmuxWindow: "@7", State: model.StateWaiting, UpdatedAt: time.Now()}
		return hook.Result{Session: s, Stdout: []byte(line)}, nil
	}
	t.Cleanup(func() { hookRun = prev })

	prevExit := hookExit
	var codes []int
	hookExit = func(code int) { codes = append(codes, code) }
	var errOut bytes.Buffer
	rootCmd.SetIn(strings.NewReader(`{"session_id":"A"}`))
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs([]string{"hook", "Stop"})
	t.Cleanup(func() {
		hookExit = prevExit
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(codes, []int{0}) || errOut.Len() != 0 {
		t.Errorf("exit calls %v, stderr %q; want one exit(0) and no stderr", codes, errOut.String())
	}
	want := []string{"PaneFocused:" + line, "SetWindowOption:" + line}
	if !slices.Equal(order.seen, want) {
		t.Errorf("tmux calls saw stdout %q, want %q", order.seen, want)
	}
}

// Ruling 74: a compaction's SessionStart leaves a red session red and ends
// no other session in the pane, so no marker changes and tmux is not asked.
func TestHookCompactMakesNoMarkerCall(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.event("Notification", "A")
	seedSessions(t, m.db, model.Session{
		ID: "B", Agent: "claude", Pane: "%1", TmuxWindow: "@8", State: model.StateWaiting,
		StartedAt: time.Now(), UpdatedAt: time.Now(),
	})
	m.fake.Calls = nil
	body := `{"session_id":"A","cwd":"` + t.TempDir() + `","source":"compact"}`
	assertQuietExitZero(t, runHookCmd(t, strings.NewReader(body), "hook", "SessionStart"))
	if len(m.fake.Calls) != 0 {
		t.Errorf("tmux calls %q, want none", m.fake.Calls)
	}
	for _, id := range []string{"A", "B"} {
		if s := getSession(t, m.db, id); s.State != model.StateWaiting {
			t.Errorf("%s = %s, want still waiting", id, s.State)
		}
	}
}

// hookChildHome names the variable that makes TestHookClosedStdoutChild act
// as the child process of TestHookClosedStdoutExitsZero; its value is the
// child's HOME.
const hookChildHome = "CURIA_TEST_HOOK_CHILD_HOME"

// Claude Code may have closed its end of the hook's stdout. Go kills a
// process whose write to fd 1 hits a closed pipe, unless SIGPIPE is
// ignored, so this runs the hook in a child process whose stdout is a pipe
// with no reader: it must log the failed write and exit 0.
func TestHookClosedStdoutExitsZero(t *testing.T) {
	home := t.TempDir()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable() // this test binary
	if err != nil {
		t.Fatal(err)
	}
	// The context kills a hung child when the test ends.
	child := exec.CommandContext(t.Context(), self, "-test.run=^TestHookClosedStdoutChild$", "-test.count=1", "-test.timeout=30s")
	child.Env = append(os.Environ(), hookChildHome+"="+home)
	child.Stdout = w
	var stderr bytes.Buffer
	child.Stderr = &stderr
	err = child.Run()
	_ = w.Close()
	if err != nil {
		t.Fatalf("child: %v, want exit 0 (stderr %q)", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("child wrote to stderr: %q", stderr.String())
	}
	b, err := os.ReadFile(filepath.Clean(filepath.Join(home, ".local", "state", "curia", "curia.log")))
	if err != nil {
		t.Fatal(err)
	}
	log := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(log) != 1 || !strings.Contains(log[0], "hook output") || !strings.Contains(log[0], "broken pipe") {
		t.Errorf("log = %q, want one line with the failed write", log)
	}
}

// TestHookClosedStdoutChild is the child of TestHookClosedStdoutExitsZero.
// Run on its own it does nothing.
func TestHookClosedStdoutChild(t *testing.T) {
	home := os.Getenv(hookChildHome)
	if home == "" {
		return
	}
	t.Setenv("HOME", home)
	t.Setenv("CURIA_DB", filepath.Join(home, "curia.db"))
	prevRun, prevExit := hookRun, hookExit
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (hook.Result, error) {
		return hook.Result{
			Session: model.Session{ID: "A", State: model.StateWorking},
			Stdout:  []byte(`{"decision":"block","reason":"x"}` + "\n"),
		}, nil
	}
	code := -1
	hookExit = func(c int) { code = c }
	rootCmd.SetIn(strings.NewReader(`{"session_id":"A"}`))
	rootCmd.SetArgs([]string{"hook", "Stop"})
	t.Cleanup(func() {
		hookRun, hookExit = prevRun, prevExit
		rootCmd.SetIn(nil)
		rootCmd.SetArgs(nil)
	})
	if err := rootCmd.Execute(); err != nil || code != 0 {
		// stdout is the closed pipe: report on stderr.
		fmt.Fprintf(os.Stderr, "Execute = %v, exit code %d; want nil and 0\n", err, code)
		t.Fail()
	}
}

// hookLinkedWorktree makes a repository with one commit under a hermetic
// git and a linked worktree of it on branch, and returns the main
// repository's and the worktree's top levels as git prints them (symlinks
// resolved).
func hookLinkedWorktree(t *testing.T, branch string) (repo, worktree string) {
	t.Helper()
	dir := hermeticGit(t)
	gitInit(t, dir, "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, dir, "worktree", "add", "-q", "-b", branch, wt)
	repo, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if worktree, err = filepath.EvalSymlinks(wt); err != nil {
		t.Fatal(err)
	}
	return repo, worktree
}

// hookLatency runs body n times (body(i) gives the i-th payload) through
// hook.Run with the real hookEnv, and returns the sorted durations.
func hookLatency(t *testing.T, st *store.Store, env hook.Env, kind model.EventKind, n int, body func(i int) []byte) []time.Duration {
	t.Helper()
	durations := make([]time.Duration, 0, n)
	for i := range n {
		b := body(i)
		start := time.Now()
		if _, err := hook.Run(context.Background(), kind, b, env, st, config.Default()); err != nil {
			t.Fatal(err)
		}
		durations = append(durations, time.Since(start))
	}
	slices.Sort(durations)
	return durations
}

// TestHookLatencyUnchanged is TestHookLatency's method and threshold (200
// in-process PostToolUse runs, p95 under 20 ms) through the real hookEnv,
// with CURIA_TASK_ID set and a linked worktree as cwd. It also reports 200
// SessionStarts, each a new session and so one real git process, without
// gating them: a fork from the race-instrumented test binary on a host that
// other worktrees' checks load varies by tens of milliseconds at p95, before
// this change as after it. The whole-process gate covers SessionStart.
func TestHookLatencyUnchanged(t *testing.T) {
	_, wt := hookLinkedWorktree(t, "curia/T-7-x")
	t.Setenv("CURIA_TASK_ID", "T-7")
	db := filepath.Join(t.TempDir(), "curia.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger, buf := testLogger()
	env := hookEnv(context.Background(), unreachableTmux{}, logger)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	const line = `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":50000},"content":[{"type":"tool_use","id":"t","name":"Edit","input":{}}]}}` + "\n"
	if err = os.WriteFile(tr, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	const runs = 200
	starts := hookLatency(t, st, env, model.SessionStart, runs, func(i int) []byte {
		return fmt.Appendf(nil, `{"session_id":"S%d","transcript_path":%q,"cwd":%q,"source":"startup"}`, i, tr, wt)
	})
	tools := hookLatency(t, st, env, model.PostToolUse, runs, func(int) []byte {
		f, ferr := os.OpenFile(filepath.Clean(tr), os.O_APPEND|os.O_WRONLY, 0)
		if ferr != nil {
			t.Fatal(ferr)
		}
		if _, ferr = f.WriteString(line); ferr != nil {
			t.Fatal(ferr)
		}
		_ = f.Close()
		return fmt.Appendf(nil, `{"session_id":"S0","transcript_path":%q,"cwd":%q,"tool_name":"Edit","tool_input":{"file_path":"src/api.ts"}}`, tr, wt)
	})
	for _, m := range []struct {
		kind model.EventKind
		d    []time.Duration
	}{{model.SessionStart, starts}, {model.PostToolUse, tools}} {
		t.Logf("%s over %d runs: p50 %v, p95 %v, max %v", m.kind, runs, m.d[runs/2], m.d[runs*95/100], m.d[runs-1])
	}
	if p95 := tools[runs*95/100]; p95 >= 20*time.Millisecond {
		t.Errorf("PostToolUse p95 = %v, want < 20ms", p95)
	}
	if s := getSession(t, db, "S0"); s.TaskID != "T-7" || s.Worktree != wt {
		t.Errorf("S0: task %q worktree %q, want T-7 and %q", s.TaskID, s.Worktree, wt)
	}
	if lines := logLines(buf); lines != nil {
		t.Errorf("log = %q, want none", lines)
	}
}

// CURIA_TASK_ID is untrusted: only a well-formed task id is stored.
func TestHookReadsTaskFromEnvironment(t *testing.T) {
	db := hookHome(t)
	for i, tc := range []struct{ env, want string }{
		{"T-9", "T-9"},
		{"T-123456789", "T-123456789"},
		{"T-09", ""},
		{"D-1", ""},
		{"T-9;x", ""},
		{"T-9\n", ""},
		{" T-9", ""},
		{"t-9", ""},
		{"T-0", ""},
		{"T-1234567890", ""},
		{"", ""},
	} {
		t.Setenv("CURIA_TASK_ID", tc.env)
		id := fmt.Sprintf("S%d", i)
		body := `{"session_id":"` + id + `","cwd":"` + t.TempDir() + `","source":"startup"}`
		assertQuietExitZero(t, runHookCmd(t, strings.NewReader(body), "hook", "SessionStart"))
		if got := getSession(t, db, id).TaskID; got != tc.want {
			t.Errorf("CURIA_TASK_ID=%q: stored task %q, want %q", tc.env, got, tc.want)
		}
	}
	if log := readLog(t); log != nil {
		t.Errorf("log = %q, want none", log)
	}
}

package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
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
func runHookCmd(t *testing.T, stdin io.Reader, args ...string) hookResult {
	t.Helper()
	res := hookResult{exitCode: -1}
	prev := hookExit
	hookExit = func(code int) { res.exitCode = code; res.exits++ }
	var out bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
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
	res.output = out.String()
	res.log = readLog(t)
	return res
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
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (model.Session, error) {
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

// The permission dialog turns the window red when it opens, not when the
// Notification arrives about 6 s later.
func TestHookPermissionRequestSetsRed(t *testing.T) {
	m := newMarkerHook(t)
	m.event("SessionStart", "A")
	m.seen()
	wantCalls(t, m.event("PermissionRequest", "A"), "SetWindowOption @7 @curia_marker red")
	if s := getSession(t, m.db, "A"); s.State != model.StateWaiting {
		t.Errorf("A = %s, want waiting", s.State)
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
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (model.Session, error) {
		return model.Session{ID: "A", TmuxWindow: "@7", State: model.StateWaiting, UpdatedAt: time.Now()}, nil
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
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (model.Session, error) {
		return model.Session{ID: "A", TmuxWindow: "@7", State: model.StateWaiting, UpdatedAt: time.Now()}, nil
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
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (model.Session, error) {
		return model.Session{ID: "A", TmuxWindow: "@7"}, nil
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

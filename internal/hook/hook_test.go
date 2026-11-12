package hook_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/hook"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

var t0 = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func fixture(t *testing.T, kind model.EventKind) []byte {
	t.Helper()
	return fixtureFile(t, string(kind))
}

// fixtureFile reads testdata/<name>.json.
func fixtureFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", name+".json")))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseFixtures(t *testing.T) {
	base := model.Event{SessionID: "sess-1", Pane: "%3", Cwd: "/repo", TranscriptPath: "/p/t.jsonl", At: t0}
	with := func(kind model.EventKind, f func(*model.Event)) model.Event {
		ev := base
		ev.Kind = kind
		if f != nil {
			f(&ev)
		}
		return ev
	}
	tests := []model.Event{
		with(model.SessionStart, func(e *model.Event) { e.Model = "claude-haiku-5-5" }),
		with(model.UserPromptSubmit, nil),
		with(model.PreToolUse, func(e *model.Event) { e.ToolName, e.ToolTarget = "Edit", "src/api.ts" }),
		with(model.PostToolUse, func(e *model.Event) { e.ToolName, e.ToolTarget = "Grep", "func Parse" }),
		with(model.Notification, func(e *model.Event) {
			e.Message, e.NotificationType = "Claude needs your permission to use Bash", "permission_prompt"
		}),
		with(model.Stop, func(e *model.Event) {
			e.LastAssistantMessage = `The file notes.txt contains the text "alpha line 1".`
		}),
		// A subagent's closing text is not the session's answer.
		with(model.SubagentStop, nil),
		with(model.SessionEnd, nil),
	}
	for _, want := range tests {
		t.Run(string(want.Kind), func(t *testing.T) {
			got, err := hook.Parse(want.Kind, fixture(t, want.Kind), "%3", t0)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got != want {
				t.Errorf("Parse =\n %+v\nwant\n %+v", got, want)
			}
		})
	}
}

func TestParseNotificationTypes(t *testing.T) {
	for file, want := range map[string]string{
		"NotificationIdlePrompt":  "idle_prompt",
		"NotificationAuthSuccess": "auth_success",
	} {
		ev, err := hook.Parse(model.Notification, fixtureFile(t, file), "%3", t0)
		if err != nil || ev.NotificationType != want {
			t.Errorf("%s: NotificationType = %q, %v, want %q", file, ev.NotificationType, err, want)
		}
	}
}

// Ruling 31: the idle reminder and auth_success are not a request for input.
func TestInformationalNotificationChangesNothing(t *testing.T) {
	for _, file := range []string{"NotificationIdlePrompt", "NotificationAuthSuccess"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "curia.db")
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			env := (&fakeEnv{}).env("%3")
			run(t, st, env, model.SessionStart, payload("sess-1", "/p/t.jsonl", ""))
			idle := run(t, st, env, model.Stop, payload("sess-1", "", ""))
			later := env
			later.Now = func() time.Time { return t0.Add(time.Minute) }
			if got := run(t, st, later, model.Notification, fixtureFile(t, file)); got != idle {
				t.Errorf("after %s:\n %+v\nwant unchanged\n %+v", file, got, idle)
			}
			// The raw event is still recorded.
			if n := countEvents(t, path); n != 3 {
				t.Errorf("events = %d, want 3", n)
			}
		})
	}
}

// countEvents counts the raw events in the database at path. The sqlite
// driver is registered by the store package.
func countEvents(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParseToolTarget(t *testing.T) {
	long := strings.Repeat("é", 70)
	tests := []struct {
		name, input, want string
	}{
		{"file_path first", `{"command":"ls","pattern":"x","file_path":"a.go"}`, "a.go"},
		{"pattern before command", `{"command":"ls","pattern":"TODO"}`, "TODO"},
		{"command", `{"command":"go test ./..."}`, "go test ./..."},
		{"command cut to 60 runes", fmt.Sprintf(`{"command":%q}`, long), strings.Repeat("é", 60)},
		{"wrongly typed file_path skipped", `{"file_path":7,"pattern":"p"}`, "p"},
		{"empty file_path skipped", `{"file_path":"","command":"ls"}`, "ls"},
		{"none present", `{"url":"https://example.com"}`, ""},
		{"tool_input not an object", `"rm -rf /"`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"session_id":"s","tool_name":"Bash","tool_input":` + tc.input + `}`
			ev, err := hook.Parse(model.PreToolUse, []byte(payload), "", t0)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if ev.ToolTarget != tc.want {
				t.Errorf("ToolTarget = %q, want %q", ev.ToolTarget, tc.want)
			}
		})
	}
}

func TestParseToleratesMissingAndWronglyTypedFields(t *testing.T) {
	payload := `{"session_id":"s","cwd":5,"transcript_path":null,"tool_name":["Edit"],"message":{"x":1},"extra":true}`
	ev, err := hook.Parse(model.Notification, []byte(payload), "", t0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := model.Event{Kind: model.Notification, SessionID: "s", At: t0}
	if ev != want {
		t.Errorf("Parse = %+v, want %+v", ev, want)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		kind    model.EventKind
		payload string
	}{
		{"truncated JSON", model.PostToolUse, `{`},
		{"not an object", model.PostToolUse, `["session_id"]`},
		{"empty", model.PostToolUse, ``},
		{"null", model.PostToolUse, `null`},
		{"no session_id", model.PostToolUse, `{"cwd":"/repo"}`},
		{"empty session_id", model.PostToolUse, `{"session_id":""}`},
		{"session_id not a string", model.PostToolUse, `{"session_id":42}`},
		{"unknown event", model.EventKind("PreCompact"), `{"session_id":"s"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := hook.Parse(tc.kind, []byte(tc.payload), "", t0); err == nil {
				t.Error("Parse returned nil error")
			}
		})
	}
}

// fakeEnv records the calls Run makes to Git and PaneInfo.
type fakeEnv struct {
	mu        sync.Mutex
	gitCalls  []string
	paneCalls []string
}

func (f *fakeEnv) env(pane string) hook.Env {
	return hook.Env{
		Pane: pane,
		Now:  func() time.Time { return t0 },
		Git: func(cwd string) (string, string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.gitCalls = append(f.gitCalls, cwd)
			return "/src/curia", "feat/hook"
		},
		PaneInfo: func(pane string) (string, string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.paneCalls = append(f.paneCalls, pane)
			return "work", "@7"
		},
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "curia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func payload(id, transcript string, extra string) []byte {
	return fmt.Appendf(nil, `{"session_id":%q,"transcript_path":%q,"cwd":"/src/curia"%s}`, id, transcript, extra)
}

func run(t *testing.T, st *store.Store, env hook.Env, kind model.EventKind, body []byte) model.Session {
	t.Helper()
	sess, err := hook.Run(context.Background(), kind, body, env, st, config.Default())
	if err != nil {
		t.Fatalf("Run(%s): %v", kind, err)
	}
	return sess
}

func get(t *testing.T, st *store.Store, id string) model.Session {
	t.Helper()
	sess, found, err := st.GetSession(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s) = found %v, err %v", id, found, err)
	}
	return sess
}

func TestRunSessionStartRecordsSession(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	got := run(t, st, f.env("%3"), model.SessionStart, payload("A", "/p/a.jsonl", ""))

	stored := get(t, st, "A")
	if got != stored {
		t.Errorf("Run returned %+v, store has %+v", got, stored)
	}
	want := model.Session{
		ID: "A", Agent: "claude", Pane: "%3", TmuxSession: "work", TmuxWindow: "@7",
		Cwd: "/src/curia", Repo: "/src/curia", Branch: "feat/hook", TranscriptPath: "/p/a.jsonl",
		State: model.StateIdle, StartedAt: t0, UpdatedAt: t0, SeenAt: t0,
	}
	if stored != want {
		t.Errorf("stored =\n %+v\nwant\n %+v", stored, want)
	}
	if !slices.Equal(f.gitCalls, []string{"/src/curia"}) || !slices.Equal(f.paneCalls, []string{"%3"}) {
		t.Errorf("Git calls %q, PaneInfo calls %q", f.gitCalls, f.paneCalls)
	}
}

func TestGitAndTmuxOnlyOnSessionStart(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	env := f.env("%3")
	run(t, st, env, model.SessionStart, payload("A", "/p/a.jsonl", ""))
	for _, kind := range []model.EventKind{
		model.UserPromptSubmit, model.PreToolUse, model.PostToolUse, model.Notification,
		model.Stop, model.SubagentStop, model.SessionEnd,
	} {
		run(t, st, env, kind, payload("A", "/p/a.jsonl", `,"tool_name":"Edit"`))
	}
	if len(f.gitCalls) != 1 || len(f.paneCalls) != 1 {
		t.Errorf("Git called %d times, PaneInfo %d times, want once each (SessionStart)",
			len(f.gitCalls), len(f.paneCalls))
	}
}

// A session first seen mid-run (already running when Curia was installed)
// gets what a SessionStart would have given it, once (Ruling 29).
func TestFirstSeenMidRunIsEnrichedOnce(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	env := f.env("%2")
	got := run(t, st, env, model.Notification, payload("M", "", `,"message":"needs you"`))
	want := model.Session{
		ID: "M", Agent: "claude", Pane: "%2", TmuxSession: "work", TmuxWindow: "@7",
		Cwd: "/src/curia", Repo: "/src/curia", Branch: "feat/hook", LastUpdate: "needs you",
		State: model.StateWaiting, StartedAt: t0, UpdatedAt: t0,
	}
	if got != want || get(t, st, "M") != want {
		t.Errorf("first seen =\n %+v\nwant\n %+v", got, want)
	}
	for _, kind := range []model.EventKind{model.PreToolUse, model.Stop, model.Notification} {
		run(t, st, env, kind, payload("M", "", `,"tool_name":"Bash"`))
	}
	if !slices.Equal(f.gitCalls, []string{"/src/curia"}) || !slices.Equal(f.paneCalls, []string{"%2"}) {
		t.Errorf("Git calls %q, PaneInfo calls %q, want one each", f.gitCalls, f.paneCalls)
	}
}

func TestFirstSeenMidRunEndsPriorSessionInPane(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	run(t, st, f.env("%3"), model.SessionStart, payload("A", "", ""))
	run(t, st, f.env("%3"), model.PostToolUse, payload("B", "", `,"tool_name":"Read"`))
	if a := get(t, st, "A"); a.State != model.StateEnded {
		t.Errorf("A = %s, want ended by B first seen in its pane", a.State)
	}
	if b := get(t, st, "B"); b.State != model.StateWorking {
		t.Errorf("B = %s, want working", b.State)
	}
}

// TestFirstSeenKeepsWhatAConcurrentSessionStartFilled runs a SessionStart for
// the same session while the first-seen event is asking git, that is between
// its keyed read and its transaction.
func TestFirstSeenKeepsWhatAConcurrentSessionStartFilled(t *testing.T) {
	st := openStore(t)
	starter := (&fakeEnv{}).env("%2")
	starter.Git = func(string) (string, string) { return "/other", "main" }
	starter.PaneInfo = func(string) (string, string) { return "other", "@9" }
	env := (&fakeEnv{}).env("%2")
	git := env.Git
	env.Git = func(cwd string) (string, string) {
		run(t, st, starter, model.SessionStart, payload("M", "", ""))
		return git(cwd)
	}
	got := run(t, st, env, model.Notification, payload("M", "", `,"message":"needs you"`))
	if got.Repo != "/other" || got.Branch != "main" || got.TmuxSession != "other" || got.TmuxWindow != "@9" {
		t.Errorf("got repo %q branch %q tmux %q/%q, want the SessionStart's /other main other/@9",
			got.Repo, got.Branch, got.TmuxSession, got.TmuxWindow)
	}
	if got.State != model.StateWaiting {
		t.Errorf("State = %s, want waiting", got.State)
	}
}

func TestSessionStartEndsPriorSessionInPane(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	run(t, st, f.env("%3"), model.SessionStart, payload("A", "/p/a.jsonl", ""))
	run(t, st, f.env("%4"), model.SessionStart, payload("C", "/p/c.jsonl", ""))
	later := f.env("%3")
	later.Now = func() time.Time { return t0.Add(time.Minute) }
	run(t, st, later, model.SessionStart, payload("B", "/p/b.jsonl", ""))

	if a := get(t, st, "A"); a.State != model.StateEnded || !a.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("A = %s at %v, want ended at %v", a.State, a.UpdatedAt, t0.Add(time.Minute))
	}
	if b := get(t, st, "B"); b.State != model.StateIdle || b.Pane != "%3" {
		t.Errorf("B = %s in %q, want idle in %%3", b.State, b.Pane)
	}
	if c := get(t, st, "C"); c.State != model.StateIdle {
		t.Errorf("C in another pane = %s, want idle (not ended)", c.State)
	}
}

func TestHookWithoutTmuxPane(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	run(t, st, f.env(""), model.SessionStart, payload("A", "/p/a.jsonl", ""))
	run(t, st, f.env(""), model.SessionStart, payload("B", "/p/b.jsonl", ""))

	for _, id := range []string{"A", "B"} {
		s := get(t, st, id)
		if s.Pane != "" || s.TmuxSession != "" || s.TmuxWindow != "" {
			t.Errorf("%s: pane %q, tmux %q/%q, want none", id, s.Pane, s.TmuxSession, s.TmuxWindow)
		}
		// Without a pane there is nothing to end: B must not end A.
		if s.State != model.StateIdle {
			t.Errorf("%s: state %s, want idle", id, s.State)
		}
		if s.Repo != "/src/curia" || s.Branch != "feat/hook" {
			t.Errorf("%s: repo %q branch %q, want them from Git", id, s.Repo, s.Branch)
		}
	}
	if len(f.paneCalls) != 0 {
		t.Errorf("PaneInfo called %q without a pane", f.paneCalls)
	}
}

func TestPaneInfoPanicReachesCaller(t *testing.T) {
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	env.PaneInfo = func(string) (string, string) { panic("tmux exploded") }
	defer func() {
		if r := recover(); r != "tmux exploded" {
			t.Errorf("recovered %v, want the PaneInfo panic on the caller's goroutine", r)
		}
	}()
	_, _ = hook.Run(context.Background(), model.SessionStart, payload("A", "/p/a.jsonl", ""), env, st, config.Default())
	t.Error("Run returned instead of panicking")
}

// stopTranscript ends in a text line with 12+40000+1988 = 42000 tokens of
// context.
const stopTranscript = `{"type":"user","message":{"role":"user","content":"Fix the login bug"}}
{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":100,"output_tokens":5},"content":[{"type":"text","text":"Looking at the login handler."}]}}
{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":12,"cache_read_input_tokens":40000,"cache_creation_input_tokens":1988,"output_tokens":50},"content":[{"type":"text","text":"Fixed the login bug.\nThe session cookie was not refreshed."}]}}
`

// writeTranscript writes stopTranscript to a temp file and returns its path.
func writeTranscript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(stopTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStopReadsTranscript(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maxes   map[string]int
		wantMax int
	}{
		{"configured model", map[string]int{"claude-opus-5-5": 1000000}, 1000000},
		{"default", map[string]int{"other": 1}, 200000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			tr := writeTranscript(t)
			env := (&fakeEnv{}).env("%3")
			cfg := config.Default()
			cfg.ContextMax = tc.maxes
			run(t, st, env, model.SessionStart, payload("A", tr, ""))
			got, err := hook.Run(context.Background(), model.Stop, payload("A", tr, ""), env, st, cfg)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(tr)
			if got.State != model.StateIdle || got.Model != "claude-opus-5-5" || got.ContextUsed != 42000 ||
				got.ContextMax != tc.wantMax || got.LastUpdate != "Fixed the login bug." ||
				got.TranscriptOffset != info.Size() {
				t.Errorf("after Stop: %+v", got)
			}
			if stored := get(t, st, "A"); stored != got {
				t.Errorf("store has %+v, Run returned %+v", stored, got)
			}
		})
	}
}

// TestStopPrefersLastAssistantMessage: at Stop, Claude Code may not have
// flushed the final answer to the transcript yet, but the payload carries it
// (Ruling 36). Its first line is the Stop message; model and context still
// come from the transcript.
func TestStopPrefersLastAssistantMessage(t *testing.T) {
	for _, tc := range []struct {
		name, extra, want string
	}{
		{"first line of the payload's text", `,"last_assistant_message":"Done: notes.txt says alpha.\nSecond line."`, "Done: notes.txt says alpha."},
		{"CRLF", `,"last_assistant_message":"Done.\r\nMore"`, "Done."},
		{"absent falls back to the transcript", "", "Fixed the login bug."},
		{"empty falls back to the transcript", `,"last_assistant_message":""`, "Fixed the login bug."},
		{"empty first line falls back to the transcript", `,"last_assistant_message":"\nlater"`, "Fixed the login bug."},
		{"not a string falls back to the transcript", `,"last_assistant_message":7`, "Fixed the login bug."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			tr := writeTranscript(t)
			env := (&fakeEnv{}).env("%3")
			run(t, st, env, model.SessionStart, payload("A", tr, ""))
			got := run(t, st, env, model.Stop, payload("A", tr, tc.extra))
			if got.LastUpdate != tc.want || got.Model != "claude-opus-5-5" || got.ContextUsed != 42000 {
				t.Errorf("after Stop: LastUpdate %q model %q context %d, want %q from the transcript's opus at 42000",
					got.LastUpdate, got.Model, got.ContextUsed, tc.want)
			}
		})
	}
}

// TestStopMessageWithoutTranscript: a Stop whose session has no transcript
// path still takes the payload's text.
func TestStopMessageWithoutTranscript(t *testing.T) {
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", "", ""))
	if got := run(t, st, env, model.Stop, payload("A", "", `,"last_assistant_message":"hi"`)); got.LastUpdate != "hi" {
		t.Errorf("LastUpdate = %q, want hi", got.LastUpdate)
	}
}

// TestSessionStartModelKeptUntilTranscriptRead: an interactive SessionStart
// names the model; the session shows it until a transcript read replaces it.
func TestSessionStartModelKeptUntilTranscriptRead(t *testing.T) {
	st := openStore(t)
	tr := writeTranscript(t)
	env := (&fakeEnv{}).env("%3")
	if got := run(t, st, env, model.SessionStart, payload("A", tr, `,"model":"claude-haiku-5-5"`)); got.Model != "claude-haiku-5-5" {
		t.Errorf("after SessionStart: model %q, want claude-haiku-5-5", got.Model)
	}
	if got := run(t, st, env, model.UserPromptSubmit, payload("A", tr, "")); got.Model != "claude-haiku-5-5" {
		t.Errorf("after UserPromptSubmit: model %q, want it kept", got.Model)
	}
	if got := run(t, st, env, model.PostToolUse, payload("A", tr, `,"tool_name":"Read"`)); got.Model != "claude-opus-5-5" {
		t.Errorf("after a transcript read: model %q, want the transcript's claude-opus-5-5", got.Model)
	}
	// A SessionStart without one (a resume, headless, or /clear's new id)
	// leaves what the session has.
	if got := run(t, st, env, model.SessionStart, payload("A", tr, "")); got.Model != "claude-opus-5-5" {
		t.Errorf("after a SessionStart with no model: %q, want claude-opus-5-5 kept", got.Model)
	}
}

// TestParseReadsModelOnlyOnSessionStart: no other payload carries the
// session's model.
func TestParseReadsModelOnlyOnSessionStart(t *testing.T) {
	body := []byte(`{"session_id":"s","model":"claude-haiku-5-5"}`)
	for _, kind := range []model.EventKind{model.UserPromptSubmit, model.PostToolUse, model.Stop, model.SubagentStop} {
		if ev, err := hook.Parse(kind, body, "", t0); err != nil || ev.Model != "" {
			t.Errorf("Parse(%s).Model = %q, %v, want empty", kind, ev.Model, err)
		}
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestTranscriptReadResumesFromOffset(t *testing.T) {
	st := openStore(t)
	tr := writeTranscript(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", tr, ""))
	first := run(t, st, env, model.PostToolUse, payload("A", tr, `,"tool_name":"Read"`))
	if first.ContextUsed != 42000 {
		t.Fatalf("ContextUsed = %d, want 42000", first.ContextUsed)
	}

	// No new lines: the metrics stay, nothing is re-read.
	same := run(t, st, env, model.Notification, payload("A", tr, `,"message":"Waiting"`))
	if same.Model != "claude-opus-5-5" || same.ContextUsed != 42000 || same.TranscriptOffset != first.TranscriptOffset {
		t.Errorf("after a read with no new lines: %+v", same)
	}

	appendLine(t, tr, `{"type":"assistant","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3},"content":[{"type":"text","text":"Next."}]}}`)
	next := run(t, st, env, model.Stop, payload("A", tr, ""))
	info, _ := os.Stat(tr)
	if next.Model != "claude-sonnet-5-5" || next.ContextUsed != 6 || next.LastUpdate != "Next." ||
		next.TranscriptOffset != info.Size() {
		t.Errorf("after appended line: %+v", next)
	}
}

func TestEndedSessionIgnoresSubagentStopAndSessionEnd(t *testing.T) {
	st := openStore(t)
	tr := writeTranscript(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", tr, ""))
	ended := run(t, st, env, model.SessionEnd, payload("A", tr, ""))

	later := env
	later.Now = func() time.Time { return t0.Add(time.Hour) }
	for _, kind := range []model.EventKind{model.SubagentStop, model.SessionEnd} {
		if got := run(t, st, later, kind, payload("A", tr, "")); got != ended {
			t.Errorf("%s after SessionEnd changed the session:\n %+v\nwant\n %+v", kind, got, ended)
		}
	}
}

// TestNestedSessionDoesNotHideParent: a `claude -p` run from an agent's Bash
// tool inherits TMUX_PANE, so its SessionStart ends the outer session; the
// outer session's next activity brings it back (Ruling 27).
func TestNestedSessionDoesNotHideParent(t *testing.T) {
	st := openStore(t)
	env := (&fakeEnv{}).env("%7")
	steps := []struct {
		kind  model.EventKind
		id    string
		extra string
	}{
		{model.SessionStart, "OUTER", ""},
		{model.PreToolUse, "OUTER", `,"tool_name":"Bash"`},
		{model.SessionStart, "NESTED", ""},
		{model.SessionEnd, "NESTED", ""},
		{model.PostToolUse, "OUTER", `,"tool_name":"Bash"`},
		{model.Notification, "OUTER", `,"message":"Claude needs your permission to use Bash"`},
	}
	for _, s := range steps {
		run(t, st, env, s.kind, payload(s.id, "", s.extra))
	}
	listed, err := st.ListSessions(context.Background(), store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "OUTER" || listed[0].State != model.StateWaiting {
		t.Errorf("listed = %+v, want OUTER waiting only", listed)
	}
}

// TestHeadlessRunSequence: `claude -p` fires SessionStart, UserPromptSubmit,
// Stop and SessionEnd (live check), so it is working from its prompt on.
func TestHeadlessRunSequence(t *testing.T) {
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	for _, step := range []struct {
		kind model.EventKind
		want model.State
	}{
		{model.SessionStart, model.StateIdle},
		{model.UserPromptSubmit, model.StateWorking},
		{model.Stop, model.StateIdle},
		{model.SessionEnd, model.StateEnded},
	} {
		if got := run(t, st, env, step.kind, payload("H", "", "")); got.State != step.want {
			t.Errorf("after %s: %s, want %s", step.kind, got.State, step.want)
		}
	}
}

func TestRunRejectsBadPayloadWithoutWriting(t *testing.T) {
	st := openStore(t)
	_, err := hook.Run(context.Background(), model.PostToolUse, []byte("{"), (&fakeEnv{}).env("%3"), st, config.Default())
	if err == nil {
		t.Fatal("Run returned nil error for a malformed payload")
	}
	all, err := st.ListSessions(context.Background(), store.ListFilter{IncludeEnded: true})
	if err != nil || len(all) != 0 {
		t.Errorf("sessions after a bad payload: %v, %v", all, err)
	}
}

// TestParallelHooksLoseNoUpdates runs hooks for one session at once, each
// with its own Store as separate hook processes would have.
func TestParallelHooksLoseNoUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curia.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	env := (&fakeEnv{}).env("%3")
	run(t, first, env, model.SessionStart, payload("A", "/p/a.jsonl", ""))

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			st, err := store.Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = st.Close() }()
			_, err = hook.Run(context.Background(), model.PreToolUse,
				payload("A", "/p/a.jsonl", `,"tool_name":"Task"`), env, st, config.Default())
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if got := get(t, first, "A").SubagentsActive; got != n {
		t.Errorf("SubagentsActive = %d, want %d", got, n)
	}
}

func TestHookLatency(t *testing.T) {
	st := openStore(t)
	tr := writeTranscript(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", tr, ""))
	body := payload("A", tr, `,"tool_name":"Edit","tool_input":{"file_path":"src/api.ts"}`)

	const runs = 200
	durations := make([]time.Duration, 0, runs)
	for range runs {
		appendLine(t, tr, `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":50000},"content":[{"type":"tool_use","id":"t","name":"Edit","input":{}}]}}`)
		start := time.Now()
		if _, err := hook.Run(context.Background(), model.PostToolUse, body, env, st, config.Default()); err != nil {
			t.Fatal(err)
		}
		durations = append(durations, time.Since(start))
	}
	slices.Sort(durations)
	p50, p95 := durations[runs/2], durations[runs*95/100]
	t.Logf("PostToolUse over %d runs: p50 %v, p95 %v, max %v", runs, p50, p95, durations[runs-1])
	if p95 >= 20*time.Millisecond {
		t.Errorf("p95 = %v, want < 20ms", p95)
	}
}

package hook_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/decision"
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
		with(model.SessionStart, func(e *model.Event) { e.Model, e.Source = "claude-haiku-5-5", "startup" }),
		with(model.UserPromptSubmit, func(e *model.Event) { e.Prompt = "Fix the login bug" }),
		with(model.PreToolUse, func(e *model.Event) { e.ToolName, e.ToolTarget = "Edit", "src/api.ts" }),
		with(model.PostToolUse, func(e *model.Event) { e.ToolName, e.ToolTarget = "Grep", "func Parse" }),
		with(model.Notification, func(e *model.Event) {
			e.Message, e.NotificationType = "Claude needs your permission to use Bash", "permission_prompt"
		}),
		with(model.Stop, func(e *model.Event) {
			e.LastAssistantMessage = `The file notes.txt contains the text "alpha line 1".`
			e.BackgroundKnown = true // background_tasks is []
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

// Ruling 87, defect 7: real payloads of Claude Code 2.1.323 (live check
// 2, paths replaced). A helper agent's SubagentStop has an empty agent_type;
// an Agent tool subagent's names its type; a Stop lists the background
// subagents still running.
func TestParseSubagentPayloads(t *testing.T) {
	helper, err := hook.Parse(model.SubagentStop, fixtureFile(t, "SubagentStopHelper"), "%3", t0)
	if err != nil || !helper.HelperAgent {
		t.Errorf("helper SubagentStop: HelperAgent = %v, %v; want true", helper.HelperAgent, err)
	}
	agent, err := hook.Parse(model.SubagentStop, fixtureFile(t, "SubagentStop"), "%3", t0)
	if err != nil || agent.HelperAgent {
		t.Errorf("Agent subagent's SubagentStop: HelperAgent = %v, %v; want false", agent.HelperAgent, err)
	}
	// A payload with no agent_type at all (an older Claude Code) counts as
	// an Agent subagent, as before.
	old, err := hook.Parse(model.SubagentStop, []byte(`{"session_id":"sess-1"}`), "%3", t0)
	if err != nil || old.HelperAgent {
		t.Errorf("SubagentStop without agent_type: HelperAgent = %v, %v; want false", old.HelperAgent, err)
	}
	stop, err := hook.Parse(model.Stop, fixtureFile(t, "StopBackgroundRunning"), "%3", t0)
	if err != nil || !stop.BackgroundKnown || stop.RunningSubagents != 1 {
		t.Errorf("Stop with a running background subagent: known %v, running %d, %v; want true, 1",
			stop.BackgroundKnown, stop.RunningSubagents, err)
	}
	for payload, want := range map[string]bool{
		`{"session_id":"s","background_tasks":[]}`:                                    true,
		`{"session_id":"s"}`:                                                          false,
		`{"session_id":"s","background_tasks":null}`:                                  false,
		`{"session_id":"s","background_tasks":"x"}`:                                   false,
		`{"session_id":"s","background_tasks":[{"type":"subagent","status":"done"}]}`: true,
	} {
		ev, err := hook.Parse(model.Stop, []byte(payload), "%3", t0)
		if err != nil || ev.BackgroundKnown != want || ev.RunningSubagents != 0 {
			t.Errorf("%s: known %v, running %d, %v; want known %v, 0", payload, ev.BackgroundKnown, ev.RunningSubagents, err, want)
		}
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
		Git: func(cwd string) (string, string, string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.gitCalls = append(f.gitCalls, cwd)
			return "/src/curia", "feat/hook", ""
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

// openWithRaw opens a store in a temp dir and a plain connection to the
// same file, for writing rows past the store's checks.
func openWithRaw(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "curia.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return st, db
}

func payload(id, transcript string, extra string) []byte {
	return fmt.Appendf(nil, `{"session_id":%q,"transcript_path":%q,"cwd":"/src/curia"%s}`, id, transcript, extra)
}

func run(t *testing.T, st *store.Store, env hook.Env, kind model.EventKind, body []byte) model.Session {
	t.Helper()
	res, err := hook.Run(context.Background(), kind, body, env, st, config.Default())
	if err != nil {
		t.Fatalf("Run(%s): %v", kind, err)
	}
	return res.Session
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
	starter.Git = func(string) (string, string, string) { return "/other", "main", "" }
	starter.PaneInfo = func(string) (string, string) { return "other", "@9" }
	env := (&fakeEnv{}).env("%2")
	git := env.Git
	env.Git = func(cwd string) (string, string, string) {
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
			res, err := hook.Run(context.Background(), model.Stop, payload("A", tr, ""), env, st, cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := res.Session
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

func TestParseCompactSessionStart(t *testing.T) {
	got, err := hook.Parse(model.SessionStart, fixtureFile(t, "SessionStartCompact"), "%3", t0)
	want := model.Event{
		Kind: model.SessionStart, SessionID: "sess-1", Pane: "%3", Cwd: "/repo",
		TranscriptPath: "/p/t.jsonl", Source: "compact", At: t0,
	}
	if err != nil || got != want {
		t.Errorf("Parse = %+v, %v\nwant %+v", got, err, want)
	}
}

// TestParseReadsSourceOnlyOnSessionStart: source means something only on a
// SessionStart; a wrongly typed one reads as empty.
func TestParseReadsSourceOnlyOnSessionStart(t *testing.T) {
	body := []byte(`{"session_id":"s","source":"compact"}`)
	for _, kind := range []model.EventKind{model.UserPromptSubmit, model.PostToolUse, model.Stop, model.SessionEnd} {
		if ev, err := hook.Parse(kind, body, "", t0); err != nil || ev.Source != "" {
			t.Errorf("Parse(%s).Source = %q, %v, want empty", kind, ev.Source, err)
		}
	}
	if ev, err := hook.Parse(model.SessionStart, []byte(`{"session_id":"s","source":["compact"]}`), "", t0); err != nil || ev.Source != "" {
		t.Errorf("Parse with a non-string source: %q, %v, want empty", ev.Source, err)
	}
}

// Ruling 74: a compaction in the middle of a turn leaves the session as it
// was, asks neither git nor tmux, and ends no other session in the pane.
func TestCompactKeepsSession(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	env := f.env("%3")
	run(t, st, env, model.SessionStart, payload("A", "/p/t.jsonl", `,"model":"claude-haiku-5-5"`))
	run(t, st, env, model.UserPromptSubmit, payload("A", "/p/t.jsonl", ""))
	before := run(t, st, env, model.PreToolUse, payload("A", "/p/t.jsonl", `,"tool_name":"Task"`))
	other := model.Session{ID: "B", Agent: "claude", Pane: "%3", State: model.StateWorking, StartedAt: t0, UpdatedAt: t0}
	if err := st.UpsertSession(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	later := f.env("%3")
	later.Now = func() time.Time { return t0.Add(time.Minute) }
	a := run(t, st, later, model.SessionStart, payload("A", "/p/t.jsonl", `,"source":"compact"`))
	want := before
	want.UpdatedAt = t0.Add(time.Minute)
	if a != want || get(t, st, "A") != want {
		t.Errorf("A after compact:\n %+v\nwant\n %+v", a, want)
	}
	if b := get(t, st, "B"); b.State != model.StateWorking {
		t.Errorf("B in the same pane = %s, want working (not ended)", b.State)
	}
	if len(f.gitCalls) != 1 || len(f.paneCalls) != 1 {
		t.Errorf("Git called %d times, PaneInfo %d times, want once each (the first SessionStart)", len(f.gitCalls), len(f.paneCalls))
	}
}

// Ruling 74a: a compaction's SessionStart for an ended session is a start
// like any other: it is idle and seen, git and tmux are asked, and it ends
// the pane's other sessions.
func TestCompactRevivesEndedSession(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	env := f.env("%3")
	run(t, st, env, model.SessionStart, payload("A", "/p/t.jsonl", ""))
	run(t, st, env, model.UserPromptSubmit, payload("A", "/p/t.jsonl", ""))
	run(t, st, env, model.SessionEnd, payload("A", "/p/t.jsonl", ""))
	other := model.Session{ID: "B", Agent: "claude", Pane: "%3", State: model.StateWorking, StartedAt: t0, UpdatedAt: t0}
	if err := st.UpsertSession(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	later := f.env("%3")
	later.Now = func() time.Time { return t0.Add(time.Minute) }
	a := run(t, st, later, model.SessionStart, payload("A", "/p/t.jsonl", `,"source":"compact"`))
	at := t0.Add(time.Minute)
	if a.State != model.StateIdle || a.SeenAt != at || a.StartedAt != at || a.TmuxWindow != "@7" {
		t.Errorf("A after compact on ended: %+v, want idle, seen and started at %v in @7", a, at)
	}
	if b := get(t, st, "B"); b.State != model.StateEnded {
		t.Errorf("B in the same pane = %s, want ended", b.State)
	}
	if len(f.gitCalls) != 2 || len(f.paneCalls) != 2 {
		t.Errorf("Git called %d times, PaneInfo %d times, want twice each", len(f.gitCalls), len(f.paneCalls))
	}
}

// For a session with no row yet, compact is a first event like any other.
func TestCompactFirstSeenStartsSession(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	env := f.env("%3")
	run(t, st, env, model.SessionStart, payload("OLD", "", ""))
	got := run(t, st, env, model.SessionStart, fixtureFile(t, "SessionStartCompact"))
	want := model.Session{
		ID: "sess-1", Agent: "claude", Pane: "%3", TmuxSession: "work", TmuxWindow: "@7",
		Cwd: "/repo", Repo: "/src/curia", Branch: "feat/hook", TranscriptPath: "/p/t.jsonl",
		State: model.StateIdle, StartedAt: t0, UpdatedAt: t0, SeenAt: t0,
	}
	if got != want {
		t.Errorf("first seen at compact:\n %+v\nwant\n %+v", got, want)
	}
	if old := get(t, st, "OLD"); old.State != model.StateEnded {
		t.Errorf("OLD in the pane = %s, want ended", old.State)
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

// Decisions at Stop (plan sections 4.3 and 4.9).

// assumeExpiry is the deadline of an assume ticket (the config default).
const assumeExpiry = 24 * time.Hour

// request is a valid ticket request with four options; question makes it
// distinct from other requests.
func request(question string, mode model.Mode) decision.Request {
	r := decision.Request{
		Question: question,
		Context:  "internal/events/store.go writes about 200 events a minute.",
		Options: []model.Option{
			{Key: "A", Label: "Postgres table", Tradeoffs: "Durable and queryable. Needs a migration."},
			{Key: "B", Label: "Redis stream", Tradeoffs: "No migration. History is capped by memory."},
			{Key: "C", Label: "SQLite file", Tradeoffs: "Local only."},
			{Key: "D", Label: "Flat files", Tradeoffs: "No queries at all."},
		},
		Recommendation: "A",
		Rationale:      "Replays need 30 days of history, which Redis cannot hold at this volume.",
		Reversibility:  string(model.ReversCostly),
		Mode:           string(mode),
		Scope:          string(model.ScopeRepo),
		Tags:           []string{"storage", "events"},
	}
	if mode == model.ModeAssume {
		r.Assumption = "Postgres"
	}
	return r
}

// raise stores an open ticket from session, raised at t0.
func raise(t *testing.T, st *store.Store, session, question string, mode model.Mode) model.Decision {
	t.Helper()
	r := request(question, mode)
	if ps := decision.Validate(decision.Normalize(r)); ps != nil {
		t.Fatalf("test request %q is invalid: %v", question, ps)
	}
	d, dup, err := st.CreateDecision(context.Background(),
		decision.New(r, decision.Caller{SessionID: session, Repo: "/src/curia"}, t0, assumeExpiry))
	if err != nil || dup {
		t.Fatalf("CreateDecision(%q) = %s, duplicate %v, err %v", question, d.ID, dup, err)
	}
	return d
}

// queue raises a blocking ticket from session and answers it with option A
// and reasoning at t0, so its answer is queued for delivery.
func queue(t *testing.T, st *store.Store, session, question, reasoning string) model.Decision {
	t.Helper()
	id := raise(t, st, session, question, model.ModeBlocking).ID
	d, err := st.AnswerDecision(context.Background(), id,
		model.Answer{Key: "A", Reasoning: reasoning, Via: model.AnsweredCLI}, t0)
	if err != nil {
		t.Fatalf("AnswerDecision(%s): %v", id, err)
	}
	if d.Delivery != model.DeliveryQueued {
		t.Fatalf("%s delivery = %s, want queued", id, d.Delivery)
	}
	return d
}

func getDecision(t *testing.T, st *store.Store, id string) model.Decision {
	t.Helper()
	d, found, err := st.GetDecision(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetDecision(%s) = found %v, err %v", id, found, err)
	}
	return d
}

// wantDelivered checks that decision id was delivered by the Stop hook at t0.
func wantDelivered(t *testing.T, st *store.Store, id string) {
	t.Helper()
	d := getDecision(t, st, id)
	if d.Delivery != model.DeliveryDelivered || d.DeliveredVia != model.ViaStopHook || !d.DeliveredAt.Equal(t0) {
		t.Errorf("%s: delivery %s via %q at %v, want delivered via stop_hook at %v",
			id, d.Delivery, d.DeliveredVia, d.DeliveredAt, t0)
	}
}

// wantQueued checks that decision id is still queued.
func wantQueued(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if d := getDecision(t, st, id); d.Delivery != model.DeliveryQueued || d.DeliveredVia != "" {
		t.Errorf("%s: delivery %s via %q, want still queued", id, d.Delivery, d.DeliveredVia)
	}
}

// stopRun runs one Stop for session with the payload's extra fields and
// fails the test on an error.
func stopRun(t *testing.T, st *store.Store, env hook.Env, session, extra string) hook.Result {
	t.Helper()
	res, err := hook.Run(context.Background(), model.Stop, payload(session, "", extra), env, st, config.Default())
	if err != nil {
		t.Fatalf("Run(Stop): %v", err)
	}
	return res
}

// workingSession returns a store holding session A, working in pane %3,
// and the env that ran it.
func workingSession(t *testing.T) (*store.Store, hook.Env) {
	t.Helper()
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", "", ""))
	run(t, st, env, model.UserPromptSubmit, payload("A", "", ""))
	return st, env
}

// wantState checks the state Run returned and the stored one.
func wantState(t *testing.T, st *store.Store, res hook.Result, want model.State) {
	t.Helper()
	if res.Session.State != want {
		t.Errorf("Run returned state %s, want %s", res.Session.State, want)
	}
	if stored := get(t, st, res.Session.ID); stored != res.Session {
		t.Errorf("store has\n %+v\nRun returned\n %+v", stored, res.Session)
	}
}

// blockReason decodes a Stop's output, checking it is one JSON line with
// exactly the keys decision ("block") and reason, and returns the reason.
func blockReason(t *testing.T, out []byte) string {
	t.Helper()
	if bytes.Count(out, []byte("\n")) != 1 || !bytes.HasSuffix(out, []byte("\n")) {
		t.Fatalf("Stdout = %q, want one line ending in a newline", out)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("Stdout %q is not a JSON object: %v", out, err)
	}
	var verdict, reason string
	if len(fields) != 2 || json.Unmarshal(fields["decision"], &verdict) != nil ||
		json.Unmarshal(fields["reason"], &reason) != nil || verdict != "block" {
		t.Fatalf("Stdout = %s, want exactly decision \"block\" and a reason string", out)
	}
	return reason
}

func TestStopWithOpenBlockingNeedsDecision(t *testing.T) {
	st, env := workingSession(t)
	raise(t, st, "A", "Which store for events?", model.ModeBlocking)
	res := stopRun(t, st, env, "A", `,"last_assistant_message":"Waiting on D-1."`)
	if res.Stdout != nil {
		t.Errorf("Stdout = %q, want nil", res.Stdout)
	}
	wantState(t, st, res, model.StateNeedsDecision)
	if res.Session.LastUpdate != "Waiting on D-1." {
		t.Errorf("LastUpdate = %q, want the payload's message", res.Session.LastUpdate)
	}
}

func TestStopWithOnlyAssumeIsIdle(t *testing.T) {
	st, env := workingSession(t)
	raise(t, st, "A", "Which store for events?", model.ModeAssume)
	res := stopRun(t, st, env, "A", "")
	if res.Stdout != nil {
		t.Errorf("Stdout = %q, want nil", res.Stdout)
	}
	wantState(t, st, res, model.StateIdle)
}

func TestStopWithAnotherSessionsTicketIsIdle(t *testing.T) {
	st, env := workingSession(t)
	raise(t, st, "B", "Which store for events?", model.ModeBlocking)
	other := queue(t, st, "B", "Which queue for jobs?", "it is there")
	res := stopRun(t, st, env, "A", "")
	if res.Stdout != nil {
		t.Errorf("Stdout = %q, want nil", res.Stdout)
	}
	wantState(t, st, res, model.StateIdle)
	wantQueued(t, st, other.ID)
}

func TestStopClaimsQueuedAndBlocks(t *testing.T) {
	st, env := workingSession(t)
	d := queue(t, st, "A", "Which store for events?", "we already run it")
	if d.ID != "D-1" {
		t.Fatalf("ticket id %s, want D-1", d.ID)
	}
	res := stopRun(t, st, env, "A", "")
	want := `{"decision":"block","reason":"Curia: decisions answered while you were working.\nDecision D-1 answered: option A [Postgres table]. Reasoning: we already run it. Continue."}` + "\n"
	if string(res.Stdout) != want {
		t.Errorf("Stdout =\n %q\nwant\n %q", res.Stdout, want)
	}
	wantState(t, st, res, model.StateWorking)
	wantDelivered(t, st, "D-1")
}

// Ruling 82: an answer the user superseded while it was queued, or after it
// was delivered, is handed over at the next Stop as the retraction, never
// as the answer.
func TestStopDeliversRetractionOfSuperseded(t *testing.T) {
	const want = `{"decision":"block","reason":"Curia: decisions answered while you were working.\n` +
		`Decision D-1: the user retracted this answer. Do not act on it. ` +
		`If you still need a decision, raise the question again with request_decision."}` + "\n"
	ctx := context.Background()
	st, env := workingSession(t)
	queue(t, st, "A", "Which store for events?", "we already run it")
	if _, err := st.EditDecision(ctx, "D-1", model.DecisionEdit{Supersede: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if res := stopRun(t, st, env, "A", ""); string(res.Stdout) != want {
		t.Errorf("superseded while queued: Stdout =\n %q\nwant\n %q", res.Stdout, want)
	}

	st, env = workingSession(t)
	queue(t, st, "A", "Which store for events?", "we already run it")
	if res := stopRun(t, st, env, "A", ""); !strings.Contains(string(res.Stdout), "answered: option A") {
		t.Fatalf("first Stop = %q, want the answer", res.Stdout)
	}
	run(t, st, env, model.UserPromptSubmit, payload("A", "", ""))
	if _, err := st.EditDecision(ctx, "D-1", model.DecisionEdit{Supersede: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if res := stopRun(t, st, env, "A", ""); string(res.Stdout) != want {
		t.Errorf("superseded after delivery: Stdout =\n %q\nwant\n %q", res.Stdout, want)
	}
	wantDelivered(t, st, "D-1")
}

// Ruling 86, defect 3: the UserPromptSubmit of the typed line confirms the
// typed answer; a prompt that does not name it (the user's own, after the
// line was lost in a menu) leaves it unconfirmed.
func TestDefect3PromptNamingTypedAnswerConfirmsIt(t *testing.T) {
	ctx := context.Background()
	st, env := workingSession(t)
	raise(t, st, "A", "Which store for events?", model.ModeBlocking)
	stopRun(t, st, env, "A", "") // needs_decision in %3
	d, err := st.AnswerDecision(ctx, "D-1", model.Answer{Key: "A", Via: model.AnsweredCLI}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := st.ClaimTyping(ctx, d.ID, t0); err != nil || c.Refusal != store.TypingClaimed {
		t.Fatalf("ClaimTyping = %v, %v", c.Refusal, err)
	}
	prompt := func(text string) []byte {
		b, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		return payload("A", "", `,"prompt":`+string(b))
	}
	run(t, st, env, model.UserPromptSubmit, prompt("please go on"))
	if got := getDecision(t, st, "D-1"); !got.ConfirmedAt.IsZero() {
		t.Errorf("confirmed at %v by a prompt that does not name it", got.ConfirmedAt)
	}
	run(t, st, env, model.UserPromptSubmit, prompt(decision.TypedMessage(getDecision(t, st, "D-1"))))
	if got := getDecision(t, st, "D-1"); !got.ConfirmedAt.Equal(t0) {
		t.Errorf("confirmed at %v, want %v: the typed line was submitted", got.ConfirmedAt, t0)
	}
}

func TestSecondStopDoesNotBlock(t *testing.T) {
	st, env := workingSession(t)
	queue(t, st, "A", "Which store for events?", "we already run it")
	if res := stopRun(t, st, env, "A", ""); res.Stdout == nil {
		t.Fatal("first Stop printed nothing, want the block")
	}
	res := stopRun(t, st, env, "A", `,"stop_hook_active":true`)
	if res.Stdout != nil {
		t.Errorf("second Stop: Stdout = %q, want nil", res.Stdout)
	}
	wantState(t, st, res, model.StateIdle)
}

// A Stop that follows a blocked one carries stop_hook_active true (live
// probe A); the hook claims what is queued all the same.
func TestStopHookActiveIsNotSpecial(t *testing.T) {
	st, env := workingSession(t)
	queue(t, st, "A", "Which store for events?", "we already run it")
	first := stopRun(t, st, env, "A", `,"stop_hook_active":false`)
	if r := blockReason(t, first.Stdout); !strings.Contains(r, "D-1") {
		t.Errorf("first reason %q, want D-1", r)
	}
	if second := stopRun(t, st, env, "A", `,"stop_hook_active":true`); second.Stdout != nil {
		t.Errorf("second Stop: Stdout = %q, want nil", second.Stdout)
	}
	queue(t, st, "A", "Which queue for jobs?", "it is there")
	third := stopRun(t, st, env, "A", `,"stop_hook_active":true`)
	r := blockReason(t, third.Stdout)
	if !strings.Contains(r, "Decision D-2 ") || strings.Contains(r, "D-1") {
		t.Errorf("third reason %q, want D-2 only", r)
	}
	wantState(t, st, third, model.StateWorking)
	wantDelivered(t, st, "D-2")
}

func TestStopDeliversSeveralInOneBlock(t *testing.T) {
	st, env := workingSession(t)
	for i := range 3 {
		queue(t, st, "A", fmt.Sprintf("Which store for events, part %d?", i), fmt.Sprintf("reason %d", i))
	}
	res := stopRun(t, st, env, "A", "")
	reason := blockReason(t, res.Stdout)
	var delivered []model.Decision
	for _, id := range []string{"D-1", "D-2", "D-3"} {
		wantDelivered(t, st, id)
		delivered = append(delivered, getDecision(t, st, id))
	}
	if want := decision.StopReason(delivered); reason != want {
		t.Errorf("reason =\n %q\nwant\n %q", reason, want)
	}
	if lines := strings.Split(reason, "\n"); len(lines) != 4 {
		t.Errorf("reason has %d lines, want a header and three decisions: %q", len(lines), reason)
	}
	wantState(t, st, res, model.StateWorking)
}

// gitBarrier returns env with a Git that waits until two callers have
// reached it (or 5 seconds have passed, so a racer that failed earlier
// cannot hang the test).
func gitBarrier(env hook.Env) hook.Env {
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	git := env.Git
	env.Git = func(cwd string) (string, string, string) {
		mu.Lock()
		if arrived++; arrived == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
		case <-time.After(5 * time.Second):
		}
		return git(cwd)
	}
	return env
}

// stopRace runs a Stop for session id through each of handles at once and
// returns the results.
func stopRace(t *testing.T, handles [2]*store.Store, env hook.Env, id string) [2]hook.Result {
	t.Helper()
	var results [2]hook.Result
	var errs [2]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, st := range handles {
		wg.Go(func() {
			<-start
			results[i], errs[i] = hook.Run(context.Background(), model.Stop, payload(id, "", ""), env, st, config.Default())
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Stop %d for %s: %v", i, id, err)
		}
	}
	return results
}

// TestTwoStopHooksOneBlocks runs two Stops for one session at once, each
// with its own Store as two hook processes would have, with one answer
// queued: exactly one of them may deliver it. In even rounds the session is
// new, so both Stops ask git before their transaction, and git holds each
// until both have asked: both have done everything they do before the
// transaction (appending the event, looking the session up) before either
// begins it, on any number of CPUs. In odd rounds the session is stored.
func TestTwoStopHooksOneBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curia.db")
	var handles [2]*store.Store
	for i := range handles {
		st, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		handles[i] = st
	}
	stored := (&fakeEnv{}).env("%3")
	run(t, handles[0], stored, model.SessionStart, payload("A", "", ""))
	for round := range 100 {
		id, env := "A", stored
		if round%2 == 0 {
			id, env = fmt.Sprintf("new-%d", round), gitBarrier((&fakeEnv{}).env(""))
		}
		d := queue(t, handles[0], id, fmt.Sprintf("Which store, round %d?", round), "we already run it")
		blocked := 0
		for _, res := range stopRace(t, handles, env, id) {
			if res.Stdout == nil {
				continue
			}
			blocked++
			if r := blockReason(t, res.Stdout); !strings.Contains(r, "Decision "+d.ID+" ") {
				t.Fatalf("round %d: reason %q does not name %s", round, r, d.ID)
			}
		}
		if blocked != 1 {
			t.Fatalf("round %d (%s): %d Stops blocked, want exactly 1", round, id, blocked)
		}
		wantDelivered(t, handles[0], d.ID)
	}
}

func TestOtherEventsNeverPrintOrClaim(t *testing.T) {
	st, env := workingSession(t)
	d := queue(t, st, "A", "Which store for events?", "we already run it")
	for _, kind := range []model.EventKind{
		model.SessionStart, model.UserPromptSubmit, model.PreToolUse, model.PostToolUse,
		model.Notification, model.SubagentStop, model.SessionEnd,
	} {
		res, err := hook.Run(context.Background(), kind, payload("A", "", `,"tool_name":"Edit","message":"needs you"`), env, st, config.Default())
		if err != nil {
			t.Fatalf("Run(%s): %v", kind, err)
		}
		if res.Stdout != nil {
			t.Errorf("%s: Stdout = %q, want nil", kind, res.Stdout)
		}
		wantQueued(t, st, d.ID)
	}
}

// hostile is text a ticket's asker or answerer could store: quotes, markup,
// an escape sequence, line breaks, a bidi override and a JSON line
// separator.
const hostile = "say \"no\" </script> \x1b[2J\r\nnext line \u202e evil \u2028 end \\ \x07."

func TestStopOutputSurvivesHostileText(t *testing.T) {
	st, db := openWithRaw(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", "", ""))
	keyed := queue(t, st, "A", "Which store for events?", "fine")
	free := raise(t, st, "A", "Which queue for jobs?", model.ModeBlocking)
	if _, err := st.AnswerDecision(context.Background(), free.ID,
		model.Answer{Text: "use the queue", Via: model.AnsweredCLI}, t0); err != nil {
		t.Fatal(err)
	}
	// Put the hostile text in the rows as they are, past every cleaning on
	// the way in, so only the way out is tested.
	options, err := json.Marshal([]model.Option{{Key: "A", Label: hostile}, {Key: "B", Label: "other"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE decisions SET options = ?, answer_reasoning = ? WHERE id = ?`, string(options), hostile, keyed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE decisions SET answer = ?, answer_reasoning = ? WHERE id = ?`, hostile, hostile, free.ID); err != nil {
		t.Fatal(err)
	}

	res := stopRun(t, st, env, "A", "")
	if !json.Valid(res.Stdout) {
		t.Fatalf("Stdout %q is not valid JSON", res.Stdout)
	}
	reason := blockReason(t, res.Stdout)
	for _, r := range reason {
		if r != '\n' && !unicode.IsPrint(r) {
			t.Errorf("reason holds the rune %U: %q", r, reason)
		}
	}
	if !strings.Contains(reason, `say "no" </script>`) || !strings.Contains(reason, "Decision "+free.ID+" answered: say") {
		t.Errorf("reason %q lost the readable text", reason)
	}
	if !bytes.Contains(res.Stdout, []byte("</script>")) {
		t.Errorf("Stdout %q escapes HTML, want it off", res.Stdout)
	}
	wantDelivered(t, st, keyed.ID)
	wantDelivered(t, st, free.ID)
}

// Ruling 27: activity brings an ended session back, and its Stop delivers.
func TestStopOnEndedSessionResumesAndDelivers(t *testing.T) {
	st, env := workingSession(t)
	run(t, st, env, model.SessionEnd, payload("A", "", ""))
	d := queue(t, st, "A", "Which store for events?", "we already run it")
	res := stopRun(t, st, env, "A", "")
	if r := blockReason(t, res.Stdout); !strings.Contains(r, "Decision "+d.ID+" ") {
		t.Errorf("reason %q, want %s", r, d.ID)
	}
	wantState(t, st, res, model.StateWorking)
	wantDelivered(t, st, d.ID)
}

// A session first seen at its Stop (Ruling 29) gets its answers too.
func TestStopFirstSeenDelivers(t *testing.T) {
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	d := queue(t, st, "M", "Which store for events?", "we already run it")
	res := stopRun(t, st, env, "M", "")
	if res.Stdout == nil {
		t.Fatal("Stdout = nil, want the block")
	}
	wantState(t, st, res, model.StateWorking)
	if res.Session.TmuxWindow != "@7" {
		t.Errorf("TmuxWindow = %q, want @7 from the first-seen treatment", res.Session.TmuxWindow)
	}
	wantDelivered(t, st, d.ID)
}

// A claim that fails is rolled back: nothing is claimed and nothing
// printed, and the session is still updated, counting the open tickets.
func TestStopClaimFailureClaimsNothing(t *testing.T) {
	st, db := openWithRaw(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", "", ""))
	run(t, st, env, model.UserPromptSubmit, payload("A", "", ""))
	good := queue(t, st, "A", "Which queue for jobs?", "it is there")
	bad := queue(t, st, "A", "Which store for events?", "we already run it")
	raise(t, st, "A", "Which cache?", model.ModeBlocking)
	// Options that do not decode make the claim's scan fail after its UPDATE.
	if _, err := db.Exec(`UPDATE decisions SET options = 'not json' WHERE id = ?`, bad.ID); err != nil {
		t.Fatal(err)
	}

	later := env
	later.Now = func() time.Time { return t0.Add(time.Minute) }
	res, err := hook.Run(context.Background(), model.Stop, payload("A", "", `,"last_assistant_message":"Done."`), later, st, config.Default())
	if err == nil {
		t.Error("Run returned nil error, want the claim's")
	}
	if res.Stdout != nil {
		t.Errorf("Stdout = %q, want nil", res.Stdout)
	}
	wantState(t, st, res, model.StateNeedsDecision)
	if res.Session.LastUpdate != "Done." || !res.Session.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("session %+v, want the Stop applied", res.Session)
	}
	for _, id := range []string{good.ID, bad.ID} {
		var delivery, via string
		if err = db.QueryRow(`SELECT delivery, delivered_via FROM decisions WHERE id = ?`, id).Scan(&delivery, &via); err != nil {
			t.Fatal(err)
		}
		if delivery != string(model.DeliveryQueued) || via != "" {
			t.Errorf("%s: delivery %s via %q, want still queued", id, delivery, via)
		}
	}
}

// loadAverage is the machine's 1-minute load average, for diagnosis only.
func loadAverage() string {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil { // Linux
		if f := strings.Fields(string(b)); len(f) > 0 {
			return f[0]
		}
	}
	if b, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil { // macOS: "{ 1.0 2.0 3.0 }"
		if f := strings.Fields(strings.Trim(string(b), "{} \n")); len(f) > 0 {
			return f[0]
		}
	}
	return "unknown"
}

// seedLatencyDecisions stores 500 tickets: 50 from session A (half of them
// open and blocking, some answered and already delivered, none queued) and
// 450 from 20 other sessions, a third of them answered and queued.
func seedLatencyDecisions(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	for i := range 500 {
		session := "A"
		if i >= 50 {
			session = fmt.Sprintf("S%d", i%20)
		}
		mode := model.ModeBlocking
		if i%2 == 1 {
			mode = model.ModeAssume
		}
		d := raise(t, st, session, fmt.Sprintf("Latency question %d?", i), mode)
		if i%3 != 0 {
			continue
		}
		if _, err := st.AnswerDecision(ctx, d.ID, model.Answer{Key: "B", Reasoning: "load", Via: model.AnsweredCLI}, t0); err != nil {
			t.Fatal(err)
		}
		if session == "A" {
			if _, _, err := st.CheckDecision(ctx, d.ID, "A", t0); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkLatencySeed(t, st)
}

// checkLatencySeed checks that session A has 50 tickets, some open and
// blocking, and no queued answer.
func checkLatencySeed(t *testing.T, st *store.Store) {
	t.Helper()
	tickets, err := st.ListDecisions(context.Background(), store.DecisionFilter{SessionID: "A"})
	if err != nil {
		t.Fatal(err)
	}
	open := 0
	for _, d := range tickets {
		if d.Delivery == model.DeliveryQueued {
			t.Fatalf("%s from A is queued, want none", d.ID)
		}
		if d.Status == model.StatusOpen && d.Mode == model.ModeBlocking {
			open++
		}
	}
	if len(tickets) != 50 || open == 0 {
		t.Fatalf("A has %d tickets, %d open blocking; want 50 with some open", len(tickets), open)
	}
}

// TestStopLatencyWithDecisions is TestHookLatency for Stop, which is the
// one event Plan 2 adds store work to, on a store holding 500 decisions.
func TestStopLatencyWithDecisions(t *testing.T) {
	st := openStore(t)
	tr := writeTranscript(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", tr, ""))
	seedLatencyDecisions(t, st)
	body := payload("A", tr, `,"last_assistant_message":"Done."`)
	line := `{"type":"assistant","message":{"model":"claude-opus-5-5","usage":{"input_tokens":5,"cache_read_input_tokens":50000},"content":[{"type":"text","text":"Done."}]}}`

	stopOnce := func() time.Duration {
		appendLine(t, tr, line)
		start := time.Now()
		res, err := hook.Run(context.Background(), model.Stop, body, env, st, config.Default())
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if res.Stdout != nil || res.Session.State != model.StateNeedsDecision {
			t.Fatalf("Stop printed %q, state %s; want nothing and needs_decision", res.Stdout, res.Session.State)
		}
		return elapsed
	}
	stopOnce() // warm-up, untimed

	const runs = 200
	durations := make([]time.Duration, 0, runs)
	for range runs {
		durations = append(durations, stopOnce())
	}
	slices.Sort(durations)
	p50, p95, worst := durations[runs/2], durations[runs*95/100], durations[runs-1]
	t.Logf("Stop with 500 decisions over %d runs: p50 %v, p95 %v, max %v", runs, p50, p95, worst)
	if p95 >= 20*time.Millisecond {
		t.Errorf("p95 = %v, want < 20ms (p50 %v, max %v, load average %s)", p95, p50, worst, loadAverage())
	}
}

// Task link and main repository (plan section 4.5).

// taskEnv is f's env in pane %3 with TaskID set to task.
func taskEnv(f *fakeEnv, task string) hook.Env {
	env := f.env("%3")
	env.TaskID = task
	return env
}

func TestSessionStartLinksTask(t *testing.T) {
	st := openStore(t)
	got := run(t, st, taskEnv(&fakeEnv{}, "T-7"), model.SessionStart, payload("A", "/p/a.jsonl", ""))
	if got.TaskID != "T-7" || get(t, st, "A").TaskID != "T-7" {
		t.Errorf("TaskID returned %q, stored %q, want T-7", got.TaskID, get(t, st, "A").TaskID)
	}
}

// After /clear the same process starts a new session with the same
// environment, so the new row is linked to the task too.
func TestTaskSurvivesClear(t *testing.T) {
	st := openStore(t)
	env := taskEnv(&fakeEnv{}, "T-7")
	run(t, st, env, model.SessionStart, payload("A", "/p/a.jsonl", ""))
	run(t, st, env, model.UserPromptSubmit, payload("A", "/p/a.jsonl", ""))
	run(t, st, env, model.SessionStart, payload("B", "/p/b.jsonl", `,"source":"clear"`))
	if b := get(t, st, "B"); b.TaskID != "T-7" {
		t.Errorf("B after /clear: TaskID %q, want T-7", b.TaskID)
	}
	if a := get(t, st, "A"); a.State != model.StateEnded || a.TaskID != "T-7" {
		t.Errorf("A = %s with task %q, want ended and still T-7", a.State, a.TaskID)
	}
}

func TestTaskFilledMidRun(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	// A row with no task gets one on its next event.
	run(t, st, taskEnv(f, ""), model.SessionStart, payload("A", "", ""))
	if got := run(t, st, taskEnv(f, "T-7"), model.PostToolUse, payload("A", "", `,"tool_name":"Edit"`)); got.TaskID != "T-7" {
		t.Errorf("A after an event with T-7: TaskID %q, want T-7", got.TaskID)
	}
	// A session first seen mid-run takes it on its first event.
	if got := run(t, st, taskEnv(f, "T-7"), model.Notification, payload("M", "", `,"message":"needs you"`)); got.TaskID != "T-7" {
		t.Errorf("M first seen mid-run: TaskID %q, want T-7", got.TaskID)
	}
	// A session that has a task keeps it, on any event and on a new start.
	run(t, st, taskEnv(f, "T-1"), model.SessionStart, payload("K", "", ""))
	for _, kind := range []model.EventKind{model.UserPromptSubmit, model.PostToolUse, model.Stop, model.SessionStart} {
		if got := run(t, st, taskEnv(f, "T-7"), kind, payload("K", "", `,"source":"resume"`)); got.TaskID != "T-1" {
			t.Errorf("K after %s with T-7: TaskID %q, want T-1 kept", kind, got.TaskID)
		}
	}
}

func TestNoTaskWithoutEnv(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	for _, kind := range []model.EventKind{model.SessionStart, model.UserPromptSubmit, model.PostToolUse, model.Stop} {
		if got := run(t, st, taskEnv(f, ""), kind, payload("A", "", "")); got.TaskID != "" {
			t.Errorf("after %s without CURIA_TASK_ID: TaskID %q, want none", kind, got.TaskID)
		}
	}
	// A teammate's process may lack the variable: its absence never clears
	// a task already stored.
	run(t, st, taskEnv(f, "T-7"), model.SessionStart, payload("B", "", ""))
	for _, kind := range []model.EventKind{model.UserPromptSubmit, model.PostToolUse, model.Stop, model.SessionStart} {
		if got := run(t, st, taskEnv(f, ""), kind, payload("B", "", `,"source":"resume"`)); got.TaskID != "T-7" {
			t.Errorf("B after %s without CURIA_TASK_ID: TaskID %q, want T-7 kept", kind, got.TaskID)
		}
	}
}

func TestSessionInLinkedWorktreeStoresMainRepo(t *testing.T) {
	st := openStore(t)
	env := taskEnv(&fakeEnv{}, "T-7")
	env.Git = func(string) (string, string, string) {
		return "/src/api", "curia/T-7-x", "/src/.curia-worktrees/api-T-7"
	}
	got := run(t, st, env, model.SessionStart, payload("A", "", ""))
	stored := get(t, st, "A")
	if got != stored {
		t.Errorf("Run returned %+v, store has %+v", got, stored)
	}
	if stored.Repo != "/src/api" || stored.Branch != "curia/T-7-x" || stored.Worktree != "/src/.curia-worktrees/api-T-7" {
		t.Errorf("repo %q branch %q worktree %q, want the main repo, the branch and the worktree",
			stored.Repo, stored.Branch, stored.Worktree)
	}
	// A session that starts again (resume) in the main worktree takes all
	// three from git again: the worktree is cleared.
	env.Git = func(string) (string, string, string) { return "/src/api", "main", "" }
	if got = run(t, st, env, model.SessionStart, payload("A", "", `,"source":"resume"`)); got.Repo != "/src/api" ||
		got.Branch != "main" || got.Worktree != "" {
		t.Errorf("after resume in the main worktree: repo %q branch %q worktree %q, want /src/api main and none",
			got.Repo, got.Branch, got.Worktree)
	}
	// First seen mid-run, the worktree is filled like the repo.
	env.Git = func(string) (string, string, string) { return "/src/api", "curia/T-7-x", "/src/wt" }
	if got = run(t, st, env, model.PostToolUse, payload("M", "", `,"tool_name":"Edit"`)); got.Worktree != "/src/wt" {
		t.Errorf("first seen mid-run: worktree %q, want /src/wt", got.Worktree)
	}
}

// A compaction's SessionStart for a live session asks git nothing and
// changes neither the repository nor the task.
func TestCompactionKeepsRepoAndTask(t *testing.T) {
	st := openStore(t)
	f := &fakeEnv{}
	env := taskEnv(f, "T-7")
	git := env.Git
	env.Git = func(cwd string) (string, string, string) {
		_, _, _ = git(cwd) // record the call
		return "/src/api", "curia/T-7-x", "/src/wt"
	}
	run(t, st, env, model.SessionStart, payload("A", "/p/t.jsonl", ""))
	run(t, st, env, model.UserPromptSubmit, payload("A", "/p/t.jsonl", ""))
	later := taskEnv(f, "T-9")
	got := run(t, st, later, model.SessionStart, payload("A", "/p/t.jsonl", `,"source":"compact"`))
	if got.Repo != "/src/api" || got.Branch != "curia/T-7-x" || got.Worktree != "/src/wt" || got.TaskID != "T-7" {
		t.Errorf("after compact: repo %q branch %q worktree %q task %q, want /src/api curia/T-7-x /src/wt T-7",
			got.Repo, got.Branch, got.Worktree, got.TaskID)
	}
	if got.State != model.StateWorking {
		t.Errorf("after compact: %s, want working", got.State)
	}
	if len(f.gitCalls) != 1 {
		t.Errorf("Git called %d times, want once (the first SessionStart)", len(f.gitCalls))
	}
}

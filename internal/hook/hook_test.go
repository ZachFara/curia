package hook_test

import (
	"context"
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
	b, err := os.ReadFile(filepath.Join("testdata", string(kind)+".json"))
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
		with(model.SessionStart, nil),
		with(model.UserPromptSubmit, nil),
		with(model.PreToolUse, func(e *model.Event) { e.ToolName, e.ToolTarget = "Edit", "src/api.ts" }),
		with(model.PostToolUse, func(e *model.Event) { e.ToolName, e.ToolTarget = "Grep", "func Parse" }),
		with(model.Notification, func(e *model.Event) { e.Message = "Claude needs your permission to use Bash" }),
		with(model.Stop, nil),
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
		State: model.StateWorking, StartedAt: t0, UpdatedAt: t0,
	}
	if stored != want {
		t.Errorf("stored =\n %+v\nwant\n %+v", stored, want)
	}
	if !slices.Equal(f.gitCalls, []string{"/src/curia"}) || !slices.Equal(f.paneCalls, []string{"%3"}) {
		t.Errorf("Git calls %q, PaneInfo calls %q", f.gitCalls, f.paneCalls)
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
	if b := get(t, st, "B"); b.State != model.StateWorking || b.Pane != "%3" {
		t.Errorf("B = %s in %q, want working in %%3", b.State, b.Pane)
	}
	if c := get(t, st, "C"); c.State != model.StateWorking {
		t.Errorf("C in another pane = %s, want working", c.State)
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
		if s.State != model.StateWorking {
			t.Errorf("%s: state %s, want working", id, s.State)
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

func TestEndedSessionIgnoresLaterEvents(t *testing.T) {
	st := openStore(t)
	tr := writeTranscript(t)
	env := (&fakeEnv{}).env("%3")
	run(t, st, env, model.SessionStart, payload("A", tr, ""))
	ended := run(t, st, env, model.SessionEnd, payload("A", tr, ""))

	later := env
	later.Now = func() time.Time { return t0.Add(time.Hour) }
	got := run(t, st, later, model.Stop, payload("A", tr, ""))
	if got != ended {
		t.Errorf("Stop after SessionEnd changed the session:\n %+v\nwant\n %+v", got, ended)
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

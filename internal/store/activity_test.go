package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/usage"
)

func TestActivityEvents(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	add := func(sid string, k model.EventKind, sec int, raw string) {
		t.Helper()
		if err := s.AppendEvent(ctx, model.Event{Kind: k, SessionID: sid, At: t0.Add(time.Duration(sec) * time.Second)}, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	add("a", model.UserPromptSubmit, 0, `{"prompt":"secret"}`)
	add("a", model.PreToolUse, 5, `{"tool_name":"Bash","tool_input":{"command":"`+strings.Repeat("x", 300)+`"}}`)
	add("a", model.PreToolUse, 6, `{"tool_name":"Read","tool_input":{"file_path":"/x"}}`)
	add("a", model.PreToolUse, 7, `not json`)
	add("a", model.SubagentStop, 8, `{}`)
	add("b", model.Stop, 9, ``)
	got, err := s.ActivityEvents(ctx, t0.Add(time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	a := got["a"]
	if len(a) != 3 || a[0].Tool != "Bash" || len(a[0].Command) != 200 || a[1].Tool != "Read" || a[1].Command != "" || a[2].Tool != "" {
		t.Errorf("session a = %+v", a)
	}
	if len(got["b"]) != 1 || got["b"][0].Kind != model.Stop {
		t.Errorf("session b = %+v", got["b"])
	}
	one, err := s.ActivityEvents(ctx, t0, "b")
	if err != nil || len(one) != 1 || len(one["b"]) != 1 {
		t.Errorf("one session = %+v, %v", one, err)
	}
}

func probe(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "probe", name+".json")))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The probe's captured payloads (Claude Code 2.1.329) through the store
// read and the classifier: a background command, from its Pre to the task
// notification, is testing time and not waiting on me.
func TestActivityEventsBackgroundFromProbe(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	i := 0
	add := func(k model.EventKind, sec int, raw string) {
		t.Helper()
		i++
		if err := s.AppendEvent(ctx, model.Event{Kind: k, SessionID: "p", At: t0.Add(time.Duration(sec) * time.Second)}, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	add(model.UserPromptSubmit, 0, probe(t, "userpromptsubmit"))
	add(model.PreToolUse, 1, probe(t, "pretooluse-bash-background"))
	add(model.PostToolUse, 1, probe(t, "posttooluse-bash-background"))
	add(model.Stop, 3, probe(t, "stop-background-running"))
	add(model.UserPromptSubmit, 63, probe(t, "userpromptsubmit-task-notification"))
	add(model.Stop, 65, probe(t, "stop-background-done"))
	add(model.SessionEnd, 65, `{}`)
	got, err := s.ActivityEvents(ctx, t0, "")
	if err != nil {
		t.Fatal(err)
	}
	e := got["p"]
	if len(e) != 7 {
		t.Fatalf("events = %+v", e)
	}
	checkProbeFields(t, e)
	r, err := usage.NewActivityRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := usage.ClassifyActivity(e, r, usage.ActivityOpts{IdleCap: 30 * time.Minute, Now: t0.Add(time.Hour)})
	if a.ByClass[usage.ClassWaitingMe] != 0 || a.Background[usage.ClassOther] != 62*time.Second || a.Unfinished != 0 {
		t.Errorf("by class %v, background %v, unfinished %d", a.ByClass, a.Background, a.Unfinished)
	}
}

func TestActivityEventsHostileBackgroundFields(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	t0 := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	huge := strings.Repeat("i", 1<<20)
	for i, raw := range []string{
		`{"tool_name":"Bash","tool_response":{"backgroundTaskId":12345},"tool_use_id":["x"]}`,
		`{"tool_name":"Bash","tool_response":{"backgroundTaskId":"` + huge + `"},"tool_use_id":"` + huge + `"}`,
		`{"tool_name":"Bash","tool_response":"just text"}`,
	} {
		_ = s.AppendEvent(ctx, model.Event{Kind: model.PostToolUse, SessionID: "h", At: t0.Add(time.Duration(i) * time.Second)}, []byte(raw))
	}
	for i, raw := range []string{
		`{"background_tasks":"running"}`,
		`{"background_tasks":[1,"x",null,{"id":7},{"id":"` + huge + `"},{"id":"ok"}]}`,
		`{"background_tasks":{"id":"a"}}`,
	} {
		_ = s.AppendEvent(ctx, model.Event{Kind: model.Stop, SessionID: "h", At: t0.Add(time.Duration(10+i) * time.Second)}, []byte(raw))
	}
	_ = s.AppendEvent(ctx, model.Event{Kind: model.PreToolUse, SessionID: "h", At: t0.Add(20 * time.Second)}, []byte(`{"tool_name":"Bash","tool_input":{"run_in_background":"yes"}}`))
	_ = s.AppendEvent(ctx, model.Event{Kind: model.UserPromptSubmit, SessionID: "h", At: t0.Add(21 * time.Second)}, []byte(`{"prompt":"`+huge+`"}`))
	got, err := s.ActivityEvents(ctx, t0, "")
	if err != nil {
		t.Fatal(err)
	}
	e := got["h"]
	if len(e) != 8 {
		t.Fatalf("events = %d", len(e))
	}
	for _, x := range e[:3] {
		if x.TaskID != "" || x.ToolUseID != "" {
			t.Errorf("hostile id kept: %d bytes, %d bytes", len(x.TaskID), len(x.ToolUseID))
		}
	}
	if len(e[4].Listed) != 1 || e[4].Listed[0] != "ok" || len(e[3].Listed)+len(e[5].Listed) != 0 || e[7].Background || len(e[7].Prompt) > 1000 || e[6].Background {
		t.Errorf("stops/prompt = %+v %+v %+v %+v", e[3], e[4], e[5], len(e[7].Prompt))
	}
}

func checkProbeFields(t *testing.T, e []usage.ActivityEvent) {
	t.Helper()
	if !e[1].Background || e[1].ToolUseID == "" || e[2].TaskID != "bn1xn9x92" || e[2].ToolUseID != e[1].ToolUseID || e[0].Background {
		t.Errorf("pre/post = %+v %+v", e[1], e[2])
	}
	if len(e[3].Listed) != 1 || e[3].Listed[0] != "bn1xn9x92" || !e[3].HasList || !e[5].HasList || len(e[5].Listed) != 0 {
		t.Errorf("stops = %+v %+v", e[3], e[5])
	}
	if !strings.Contains(e[4].Prompt, "<task-id>bn1xn9x92</task-id>") {
		t.Errorf("notice prompt = %q", e[4].Prompt)
	}
}

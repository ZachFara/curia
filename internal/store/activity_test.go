package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
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

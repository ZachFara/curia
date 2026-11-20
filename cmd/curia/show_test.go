package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func showHome(t *testing.T) {
	t.Helper()
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello \u001b[31mred\u001b]0;evil\u0007"}]}}` + "\n"
	if err := os.WriteFile(tp, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	db, _ := fleetHome(t, []string{"%7"},
		model.Session{ID: "sa", Repo: "/src/web", Branch: "feat/x", TaskID: "T-1", Pane: "%7", State: model.StateIdle, TranscriptPath: tp, UpdatedAt: fleetNow},
		model.Session{ID: "sb", Repo: "/src/web", TaskID: "T-2", State: model.StateEnded, UpdatedAt: fleetNow},
		model.Session{ID: "sc", Repo: "/src/web", TaskID: "T-2", State: model.StateEnded, UpdatedAt: fleetNow})
	userTerminalAs(t, true)
	t.Cleanup(func() { showJSON = false })
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	base := fleetNow.Add(-time.Hour)
	for i, e := range []struct {
		k   model.EventKind
		raw string
	}{
		{model.UserPromptSubmit, `{}`},
		{model.PreToolUse, `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`},
		{model.PostToolUse, `{}`},
	} {
		if err := st.AppendEvent(context.Background(), model.Event{Kind: e.k, SessionID: "sa", At: base.Add(time.Duration(i*30) * time.Second)}, []byte(e.raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestShowResolvesSessionTaskAndPane(t *testing.T) {
	showHome(t)
	for _, arg := range []string{"sa", "T-1", "%7"} {
		out, err := runRoot(t, "show", arg)
		if err != nil {
			t.Fatalf("show %s: %v", arg, err)
		}
		for _, want := range []string{"sa", "Transcript", "hello red", "Tickets", "Messages", "Cost", "Activity", "testing"} {
			if !strings.Contains(out, want) {
				t.Errorf("show %s lacks %q:\n%s", arg, want, out)
			}
		}
		if strings.ContainsRune(out, 0x1b) || strings.Contains(out, "evil") {
			t.Errorf("show %s printed an escape sequence: %q", arg, out)
		}
	}
}

func TestShowRefusesAmbiguousAndUnknown(t *testing.T) {
	showHome(t)
	for arg, want := range map[string]string{"T-2": "more than one", "nope": "no session", "T-9": "no session", "%99": "pane"} {
		if _, err := runRoot(t, "show", arg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("show %s = %v, want %q", arg, err, want)
		}
	}
}

func TestShowJSONIsTheSameCleanedData(t *testing.T) {
	showHome(t)
	out, err := runRoot(t, "show", "sa", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Session  struct{ ID string }
		Tail     []string
		Activity []string
	}
	if err = json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Session.ID != "sa" || len(got.Tail) != 1 || got.Tail[0] != "hello red" || len(got.Activity) == 0 {
		t.Errorf("json = %+v", got)
	}
	if strings.Contains(out, `\u001b`) {
		t.Errorf("json carries an escape: %s", out)
	}
}

func TestShowRefusedInsideAnAgent(t *testing.T) {
	showHome(t)
	userTerminalAs(t, false)
	if _, err := runRoot(t, "show", "sa"); err == nil || !strings.Contains(err.Error(), "only the user may") {
		t.Errorf("show without a terminal = %v", err)
	}
}

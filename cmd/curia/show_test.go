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
		model.Session{ID: "sa", Repo: "/src/web", Branch: "feat/x", TaskID: "T-1", Pane: "%7", Socket: "/tmp/tmux-1/a", State: model.StateIdle, TranscriptPath: tp, UpdatedAt: fleetNow},
		model.Session{ID: "sd", Repo: "/src/web", Pane: "%7", Socket: "/tmp/tmux-1/b", State: model.StateIdle, UpdatedAt: fleetNow},
		model.Session{ID: "sb", Repo: "/src/web", TaskID: "T-2", State: model.StateEnded, UpdatedAt: fleetNow},
		model.Session{ID: "sc", Repo: "/src/web", TaskID: "T-2", State: model.StateEnded, UpdatedAt: fleetNow})
	userTerminalAs(t, true)
	t.Setenv("TMUX", "/tmp/tmux-1/a,1,0")
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

func TestShowPaneIsMatchedWithItsServer(t *testing.T) {
	showHome(t) // sa and sd both have pane %7, on servers a and b
	for env, want := range map[string]string{"/tmp/tmux-1/a,1,0": `"id": "sa"`, "/tmp/tmux-1/b,1,0": `"id": "sd"`} {
		t.Setenv("TMUX", env)
		out, err := runRoot(t, "show", "%7", "--json")
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("TMUX=%s: %v %q, want %s", env, err, out, want)
		}
	}
	t.Setenv("TMUX", "/tmp/tmux-1/c,1,0") // a third server: it has no %7 of its own, yet both rows are "elsewhere"
	if _, err := runRoot(t, "show", "%7"); err != nil {
		t.Logf("third server: %v", err)
	} else {
		t.Error("a third server resolved a pane recorded on another")
	}
	t.Setenv("TMUX", "")
	if _, err := runRoot(t, "show", "%7"); err == nil || !strings.Contains(err.Error(), "session id or a task id") {
		t.Errorf("no TMUX = %v", err)
	}
}

func TestShowBudget(t *testing.T) {
	showHome(t)
	db := os.Getenv("CURIA_DB")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	tk, err := st.CreateTask(ctx, model.Task{Title: "t", Repo: "/src/web", BaseBranch: "main", Status: model.TaskReady, BudgetUSD: 5, CreatedAt: fleetNow})
	if err != nil || tk.ID != "T-1" {
		t.Fatalf("task = %+v, %v; want T-1", tk, err)
	}
	usd := 4.0
	if _, err = st.Ingest(ctx, store.Reading{SessionID: "sa", CostUSD: &usd}, fleetNow); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "show", "sa")
	if err != nil || !strings.Contains(out, "budget $4.00/$5 80% !") {
		t.Errorf("show lacks the budget (%v):\n%s", err, out)
	}
	out, err = runRoot(t, "show", "sa", "--json")
	var got struct {
		Budget struct {
			USD   float64 `json:"budget_usd"`
			Spent float64 `json:"spent_usd"`
			Level string  `json:"level"`
		}
	}
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || got.Budget.USD != 5 || got.Budget.Spent != 4 || got.Budget.Level != "warn" {
		t.Errorf("show --json budget = %+v, %v:\n%s", got.Budget, err, out)
	}
}

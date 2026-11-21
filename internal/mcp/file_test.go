package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func (f *fixture) filedTasks() []model.Task {
	f.t.Helper()
	ts, err := f.st.ListTasks(context.Background(), store.TaskFilter{})
	if err != nil {
		f.t.Fatal(err)
	}
	return ts
}

func fileArgs(title string) map[string]any {
	return map[string]any{"title": title, "body": "Do the thing.", "reason": "I noticed it while working."}
}

func (f *fixture) level(l string, perDay int) {
	f.t.Helper()
	cfg := config.Default()
	cfg.Tasks.AgentFiled.Level, cfg.Tasks.AgentFiled.MaxPerDay = l, perDay
	f.tools = Tools(Deps{
		Store: f.st, Cfg: cfg, Now: func() time.Time { return f.now },
		Caller: func(context.Context) Caller { return f.caller },
	})
}

func TestFileTaskBacklogByDefault(t *testing.T) {
	f := newFixture(t)
	m := f.mustCall("file_task", fileArgs("Fix the flaky test"))
	if m["task"] != "T-1" || m["status"] != "backlog" {
		t.Fatalf("result = %v", m)
	}
	ts := f.filedTasks()
	if len(ts) != 1 || ts[0].Status != model.TaskBacklog || ts[0].FiledBy != "S1" || ts[0].Repo != "/r/a" ||
		ts[0].FiledReason != "I noticed it while working." || ts[0].Source != model.SourceLocal {
		t.Errorf("stored %+v", ts)
	}
}

func TestFileTaskReadyLevel(t *testing.T) {
	f := newFixture(t)
	f.level("ready", 5)
	if m := f.mustCall("file_task", fileArgs("x")); m["status"] != "ready" {
		t.Errorf("result = %v", m)
	}
	if got := f.filedTasks()[0].Status; got != model.TaskReady {
		t.Errorf("stored status = %s", got)
	}
}

func TestFileTaskOffRefuses(t *testing.T) {
	f := newFixture(t)
	f.level("off", 5)
	terr := f.wantError("file_task", fileArgs("x"), "filing_off")
	if !strings.Contains(terr.Message, "tasks.agent_filed.level") {
		t.Errorf("message %q does not name the setting", terr.Message)
	}
	if n := len(f.filedTasks()); n != 0 {
		t.Errorf("%d tasks stored", n)
	}
}

func TestFileTaskCap(t *testing.T) {
	f := newFixture(t)
	f.level("backlog", 2)
	f.mustCall("file_task", fileArgs("a"))
	f.mustCall("file_task", fileArgs("b"))
	f.wantError("file_task", fileArgs("c"), "cap_reached")
	f.caller.SessionID = "S2"
	f.mustCall("file_task", fileArgs("d"))
	f.caller.SessionID = "S1"
	f.now = f.now.Add(25 * time.Hour)
	f.mustCall("file_task", fileArgs("e"))
}

func TestFileTaskNeedsSessionRepoAndReason(t *testing.T) {
	f := newFixture(t)
	f.caller.SessionID = ""
	f.wantError("file_task", fileArgs("x"), "no_session")
	f.caller.SessionID, f.caller.Repo = "S1", ""
	f.wantError("file_task", fileArgs("x"), "no_session")
	f.caller.Repo = "/r/a"
	a := fileArgs("x")
	a["reason"] = " \x00 "
	f.wantError("file_task", a, "invalid_arguments")
	f.wantError("file_task", `{"title":"x"}`, "invalid_arguments")
}

func TestFileTaskHostileText(t *testing.T) {
	f := newFixture(t)
	a := fileArgs("Title\x1b[31m red\x00 \x1b]0;pwn\x07")
	a["body"] = "line\x1b[2J one\x00\nline two"
	a["reason"] = "why\x1b[1m"
	f.mustCall("file_task", a)
	tk := f.filedTasks()[0]
	for _, s := range []string{tk.Title, tk.Body, tk.FiledReason} {
		if strings.ContainsAny(s, "\x1b\x00\x07") {
			t.Errorf("stored %q holds a control character", s)
		}
	}
	if !strings.Contains(tk.Body, "line two") {
		t.Errorf("body %q lost its second line", tk.Body)
	}
	huge := strings.Repeat("x", 1<<20)
	for field, v := range map[string]string{"title": huge, "body": huge, "reason": huge} {
		a := fileArgs("ok")
		a[field] = v
		terr := f.wantError("file_task", a, "invalid_arguments")
		if len(terr.Problems) == 0 || terr.Problems[0].Field != field {
			t.Errorf("%s: problems %+v", field, terr.Problems)
		}
	}
	if n := len(f.filedTasks()); n != 1 {
		t.Errorf("%d tasks stored, want only the cleaned one", n)
	}
}

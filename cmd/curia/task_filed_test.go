package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

// taskFiledOne files T-1 as an agent would, with hostile text.
func taskFiledOne(t *testing.T, db, repo string) {
	t.Helper()
	st := taskOpen(t, db)
	_, err := st.FileTask(context.Background(), model.Task{
		Title: "Fix \x1b[31mthe thing", Body: "step one\x1b]0;pwn\x07\nstep two", Repo: repo, Status: model.TaskBacklog,
		CreatedAt: decisionsParse(t, taskNow), FiledBy: "sess-7", FiledReason: "found while testing",
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskReadyShowsFiledBodyFirst(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskFiledOne(t, db, repo)
	// No Enter (stdin ends): nothing is marked ready.
	out, err := taskRun(t, strings.NewReader(""), "task", "ready", "T-1")
	if err == nil {
		t.Fatalf("ready without Enter succeeded:\n%s", out)
	}
	if got := taskGet(t, db, "T-1"); got.Status != model.TaskBacklog {
		t.Errorf("status = %s after no Enter, want backlog", got.Status)
	}
	for _, want := range []string{"filed by agent sess-7", "found while testing", "step one", "step two"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("output holds an escape sequence: %q", out)
	}
	out, err = taskRun(t, strings.NewReader("\n"), "task", "ready", "T-1")
	if err != nil || !strings.HasSuffix(out, "T-1 ready\n") {
		t.Fatalf("ready after Enter = %v\n%s", err, out)
	}
	if got := taskGet(t, db, "T-1"); got.Status != model.TaskReady || got.FiledBy != "sess-7" {
		t.Errorf("task = %+v, want ready and still filed", got)
	}
}

func TestTaskLsShowsFiled(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskFiledOne(t, db, repo)
	out := taskMust(t, "task", "ls")
	for _, want := range []string{"[agent sess-7: found while testing]"} {
		if !strings.Contains(out, want) {
			t.Errorf("task ls lacks %q:\n%s", want, out)
		}
	}
	if j := taskMust(t, "task", "ls", "--json"); !strings.Contains(j, `"filed_by": "sess-7"`) || !strings.Contains(j, `"filed_reason": "found while testing"`) {
		t.Errorf("task ls --json lacks the filer:\n%s", j)
	}
}

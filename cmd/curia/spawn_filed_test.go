package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

// spawnFiled stores a task filed by session by, at status.
func (f *spawnFixture) spawnFiled(by string, status model.TaskStatus) string {
	f.t.Helper()
	tk, err := f.st.FileTask(context.Background(), model.Task{
		Title: "Filed", Repo: f.repo, BaseBranch: "main", Status: status, CreatedAt: f.now,
		FiledBy: by, FiledReason: "noticed",
	}, 5)
	if err != nil {
		f.t.Fatal(err)
	}
	return tk.ID
}

// An agent never spawns an agent-filed task, whatever its status, whoever
// filed it (F5 amendment, Ruling 141); the user does.
func TestAgentSpawnRefusesFiledTasks(t *testing.T) {
	f := spawnNewFixture(t)
	own := f.spawnFiled("S1", model.TaskBacklog)
	other := f.spawnFiled("S2", model.TaskBacklog)
	createdReady := f.spawnFiled("S2", model.TaskReady)
	marked := f.spawnFiled("S2", model.TaskBacklog)
	f.update(marked, func(tk *model.Task) { tk.Status = model.TaskReady }) // the owner marked it ready
	userTerminalAs(t, false)
	for _, id := range []string{own, other, createdReady, marked} {
		out, err := spawnRun(t, id)
		if err == nil || !strings.Contains(err.Error(), "filed by an agent: only the user spawns it") {
			t.Fatalf("agent spawn of %s = %v (%q), want the refusal", id, err, out)
		}
		f.wantNothingStarted(id)
	}
	userTerminalAs(t, true)
	if out, err := spawnRun(t, marked); err != nil {
		t.Fatalf("the user's spawn of a filed task: %v\n%s", err, out)
	}
}

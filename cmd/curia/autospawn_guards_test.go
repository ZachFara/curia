package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// An unreadable .curia/setup.sh (here: not executable) is not an approval: the task waits and nothing runs.
func TestAutoSpawnUnreadableSetupScriptCountsAsNotApproved(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	f.SetupScript("#!/bin/sh\ntouch RAN\n", false) // committed and approved, but not executable
	tk := autoReady(f, "a")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows = %q: an unreadable setup script was treated as approved", w)
	}
	if rs := autoRuns(t, f); len(rs) != 1 || rs[0].Check != "setup_unapproved" || rs[0].TaskID != tk.ID {
		t.Fatalf("log = %+v, want one setup_unapproved refusal", rs)
	}
}

// Idle sessions count as live agents for max_agents; ended ones do not.
func TestAutoSpawnMaxAgentsCountsIdleButNotEnded(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "max_agents = 2\n")
	for id, state := range map[string]model.State{"s1": model.StateWorking, "s2": model.StateIdle, "s3": model.StateEnded} {
		if err := f.St.UpsertSession(context.Background(), model.Session{ID: id, State: state, Agent: "claude", StartedAt: f.Now, UpdatedAt: f.Now}); err != nil {
			t.Fatal(err)
		}
	}
	autoReady(f, "a")
	if w := autoPass(t, f, autoEnv(f)); len(w) != 0 {
		t.Fatalf("windows = %q with a working and an idle session at max_agents = 2", w)
	}
	if rs := autoRuns(t, f); len(rs) != 1 || rs[0].Check != "max_agents" {
		t.Errorf("log = %+v, want one max_agents refusal", rs)
	}
}

// A second call inside the job's own 30 seconds does nothing.
func TestAutoSpawnTicksOnlyEveryThirtySeconds(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	e := autoEnv(f)
	autoPass(t, f, e) // the pass that records the tick; nothing is ready yet
	autoReady(f, "a")
	f.Now = f.Now.Add(autoEvery - time.Second)
	if err := e.autoSpawn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := f.Windows(); len(w) != 0 {
		t.Fatalf("windows = %q: the job ran again %v after its last pass", w, autoEvery-time.Second)
	}
	f.Now = f.Now.Add(time.Second)
	if err := e.autoSpawn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := f.Windows(); len(w) != 1 {
		t.Fatalf("windows = %q, want the start once the tick has passed", w)
	}
}

// Status: the positive line, only the last ten log rows, and every free text cleaned.
func TestAutoSpawnStatusShowsWouldStartTheLastTenAndCleansText(t *testing.T) {
	f := spawnNewFixture(t)
	autoEnable(t, "")
	hostile := "evil \x1b[2J\x1b]0;pwn\x07\nFORGED T-99 running"
	ctx := context.Background()
	for i := range 12 {
		if _, err := f.St.LogAuto(ctx, store.AutoRun{At: f.Now.Add(time.Duration(i) * time.Minute), Outcome: "refused", Check: "interval", TaskID: "T-" + string(rune('a'+i)), Detail: hostile}); err != nil {
			t.Fatal(err)
		}
	}
	tk := f.Task("plain", "b")
	statusExec(t, f.DB, `UPDATE tasks SET title = ? WHERE id = ?`, hostile, tk.ID) // as a store written by something else would hold it
	f.Update(tk.ID, func(x *model.Task) { x.Status = model.TaskReady })
	if err := f.St.MarkReady(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "auto-spawn", "status")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "refused interval"); n != 10 {
		t.Errorf("status lists %d log rows, want the last 10:\n%s", n, out)
	}
	if !strings.Contains(out, "would start at the next pass") {
		t.Errorf("status = %q, want the would-start line for the eligible task", out)
	}
	if strings.ContainsAny(out, "\x1b\x07") || strings.Contains(out, "\nFORGED") {
		t.Errorf("status shows raw text:\n%q", out)
	}
}

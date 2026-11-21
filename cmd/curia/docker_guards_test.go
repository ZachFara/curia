package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// dockerGuardRig is a rig with S1 (in state) running three labeled containers and the monitor's VM reading (900 of 1000 MB) taken vmAge ago.
func dockerGuardRig(t *testing.T, resources string, state model.State, vmAge time.Duration) *dockerRig {
	t.Helper()
	r := newDockerRig(t, "[resources]\n"+resources)
	r.write(r.ps, psRow("aaaaaaaaaaa1", "S1", "", "")+psRow("aaaaaaaaaaa2", "S1", "", "")+psRow("aaaaaaaaaaa3", "S1", "", ""))
	st, err := store.Open(filepath.Join(filepath.Dir(r.log), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	now := time.Unix(1700000000, 0)
	if err = st.UpsertSession(context.Background(), model.Session{ID: "S1", Pane: "%1", State: state, StartedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = st.AddDockerVM(context.Background(), time.Now().Add(-vmAge), 900, 1000); err != nil {
		t.Fatal(err)
	}
	return r
}

// An hours-old Docker VM reading is "could not measure", not a refusal: only a current one refuses a run.
func TestDockerWrapperIgnoresAStaleMonitorReading(t *testing.T) {
	const memOnly = "max_containers_per_session = 0\nmax_containers_total = 0\ndocker_memory_refuse_pct = 80\n"
	env := []string{"CURIA_SESSION_ID=S1"}
	if _, errOut, code := dockerGuardRig(t, memOnly, model.StateWorking, time.Second).run("", env, "run", "img"); code != 1 || !strings.Contains(errOut, "docker_memory_refuse_pct") {
		t.Fatalf("a current reading at 90%% of an 80%% threshold: exit %d, %q; want the refusal", code, errOut)
	}
	if _, errOut, code := dockerGuardRig(t, memOnly, model.StateWorking, 3*time.Hour).run("", env, "run", "img"); code != 0 {
		t.Errorf("a 3-hour-old reading refused the run (exit %d, %q); want it ignored", code, errOut)
	}
}

// Containers labeled with a session that is stale or ended count for nobody: such a session is not held to the per-session cap by them.
func TestDockerWrapperDoesNotCountStaleOrEndedSessionsAsLive(t *testing.T) {
	env := []string{"CURIA_SESSION_ID=S1"}
	if _, errOut, code := dockerGuardRig(t, "docker_memory_refuse_pct = 0\n", model.StateWorking, 0).run("", env, "run", "img"); code != 1 || !strings.Contains(errOut, "max_containers_per_session") {
		t.Fatalf("a live session with three containers: exit %d, %q; want the per-session refusal", code, errOut)
	}
	for _, state := range []model.State{model.StateStale, model.StateEnded} {
		r := dockerGuardRig(t, "docker_memory_refuse_pct = 0\n", state, 0)
		if _, errOut, code := r.run("", env, "run", "img"); code != 0 {
			t.Errorf("a %s session's containers were counted as live (exit %d, %q)", state, code, errOut)
		}
	}
}

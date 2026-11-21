package testkit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// LifecycleLead is the id of the agent session the lifecycle tests link to
// task T-1: a Claude Code session id, so pause keeps it for --resume.
const LifecycleLead = "1a2b3c4d-0000-4000-8000-000000000001"

// LifecycleSession links session id, in pane and state, to T-1, started at
// the launch and updated at updated, in T-1's window (@1).
func LifecycleSession(f *SpawnFixture, id, pane string, state model.State, updated time.Time) {
	f.T.Helper()
	s := model.Session{
		ID: id, Agent: "claude", State: state, Pane: pane, TaskID: "T-1", TmuxWindow: "@1",
		StartedAt: f.Start, UpdatedAt: updated,
	}
	if err := f.St.UpsertSession(context.Background(), s); err != nil {
		f.T.Fatal(err)
	}
}

// SetupSum is the hex SHA-256 of content.
func SetupSum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// SetupMarker is the file the test scripts create in the worktree.
func SetupMarker(f *SpawnFixture) string {
	return filepath.Join(f.Worktree("T-1"), "made-by-setup")
}

// SpawnKillServer kills the tmux server on socket and removes its socket file.
func SpawnKillServer(socket string) {
	out, err := exec.Command("tmux", "-L", socket, "display-message", "-p", "#{socket_path}").Output()
	dir := filepath.Dir(strings.TrimSpace(string(out)))
	if err != nil || dir == "." {
		dir = filepath.Join(os.Getenv("TMUX_TMPDIR"), fmt.Sprintf("tmux-%d", os.Getuid()))
	}
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	if root, rerr := os.OpenRoot(dir); rerr == nil {
		_ = root.Remove(socket)
		_ = root.Close()
	}
}

// SpawnWindowOf is the expected NewWindow call of a spawn of id with the
// launch arguments extra.
func (f *SpawnFixture) SpawnWindowOf(id string, extra ...string) string {
	return strings.Join(append([]string{
		"NewWindow api-server " + id + " " + f.Worktree(id) + " CURIA_TASK_ID=" + id,
		SpawnTestBin, "launch", id,
	}, extra...), " ")
}

// SetupTouch is a setup script that leaves SetupMarker in the worktree.
const SetupTouch = "#!/bin/sh\ntouch made-by-setup\n"

// SpawnOtherAgentNote is the line a spawn prints when the task's stored
// model was stored for another agent.
func SpawnOtherAgentNote(id, modelName, storedFor, agent string) string {
	return id + "'s stored model " + modelName + " was chosen for " + storedFor + "; " + agent +
		" starts without it (pass --model to choose)"
}

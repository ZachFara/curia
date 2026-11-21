package lifecycle

import (
	"errors"
	"os/exec"
	"slices"
	"strings"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

// LaunchLookPath finds the agent's binary; tests replace it.
var LaunchLookPath = exec.LookPath

// LaunchTeamVar is the variable that turns Claude Code's agent teams on.
const LaunchTeamVar = "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"

// LaunchAfterPauseNote is what a resumed agent is told when its branch ends
// in the pause's WIP commit (live check B, defect 6: the agent took its
// committed work for uncommitted files). It is fixed text and the task id.
func LaunchAfterPauseNote(id string) string {
	return "Curia paused this task and has now resumed it. The work that was uncommitted before the pause is in the " +
		"commit \"curia: WIP " + id + " (paused)\" at the tip of your branch, not in uncommitted files: " +
		"look at git log and git status before you go on."
}

// LaunchArgv is claude's argument vector: the model, the team switch, then
// the session to resume or the brief as the one positional argument; with
// afterPause, the resumed session gets LaunchAfterPauseNote as its prompt
// and the brief ends with it. The brief and the note start with fixed text,
// so claude never reads them as an option, and they are never seen by a
// shell or by tmux. No permission flag is ever added.
func LaunchArgv(t model.Task, modelName string, team bool, resume string, afterPause bool) ([]string, error) {
	argv := []string{"claude"}
	if modelName != "" {
		argv = append(argv, "--model", modelName)
	}
	if team {
		argv = append(argv, "--teammate-mode", "tmux")
	}
	if resume != "" {
		argv = append(argv, "--resume", resume)
		if afterPause {
			argv = append(argv, LaunchAfterPauseNote(t.ID))
		}
		return argv, nil
	}
	brief, err := launchBrief(t, team, afterPause)
	if err != nil {
		return nil, err
	}
	return append(argv, brief), nil
}

// LaunchCodexArgv is codex's argument vector (section 4.10): "codex resume
// <session>" to resume, with LaunchAfterPauseNote as its prompt after a
// pause, else the model and the brief as the one positional argument. An
// agent team is refused, and no sandbox or approval flag is ever added.
func LaunchCodexArgv(t model.Task, modelName string, team bool, resume string, afterPause bool) ([]string, error) {
	if team {
		return nil, errors.New("agent teams are Claude Code's: " + t.ID + " runs codex")
	}
	argv := []string{model.AgentCodex}
	if resume != "" {
		argv = append(argv, "resume", resume)
		if afterPause {
			argv = append(argv, LaunchAfterPauseNote(t.ID))
		}
		return argv, nil
	}
	if modelName != "" {
		argv = append(argv, "--model", modelName)
	}
	brief, err := launchBrief(t, false, afterPause)
	if err != nil {
		return nil, err
	}
	return append(argv, brief), nil
}

// launchBrief is the task's brief, ending with LaunchAfterPauseNote after a
// pause; a task with no branch yet has none.
func launchBrief(t model.Task, team, afterPause bool) (string, error) {
	if !task.ValidBranch(t.Branch) || !task.ValidBranch(t.BaseBranch) {
		return "", errors.New(t.ID + " has no branch and base branch yet: start it with curia spawn " + t.ID)
	}
	// Stored text is clean already; cleaning again is free (idempotent) and
	// keeps the rule that untrusted text is cleaned wherever it is sent.
	t.Title = sanitize.Line(t.Title, task.MaxTitle)
	t.Body = sanitize.Block(t.Body, task.MaxBody)
	brief := task.Brief(t, team)
	if afterPause {
		brief += "\n\n" + LaunchAfterPauseNote(t.ID)
	}
	return brief, nil
}

// LaunchEnv is env with CURIA_TASK_ID set to id and, with team, the agent
// teams variable on; an inherited value of either is replaced, since exec
// does not drop duplicates.
func LaunchEnv(env []string, id string, team bool) []string {
	drop := []string{"CURIA_TASK_ID"}
	if team {
		drop = append(drop, LaunchTeamVar)
	}
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(drop, name)
	})
	env = append(env, "CURIA_TASK_ID="+id)
	if team {
		env = append(env, LaunchTeamVar+"=1")
	}
	return env
}

// LaunchWithRC adds Remote Control to claude's argument vector when the user
// turned rc.launch on: --remote-control with an explicit name right after
// "claude", so the flag cannot take the brief for its optional value.
// UNVERIFIED (cmd/curia/rc.go, item 4): the flag was seen in --help only.
func LaunchWithRC(argv []string, on bool, name string) []string {
	if !on || argv[0] != model.AgentClaude {
		return argv
	}
	return append([]string{argv[0], "--remote-control", name}, argv[1:]...)
}

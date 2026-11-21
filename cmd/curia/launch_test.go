package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/testkit"
)

// launchUnexpectedExec is launchExec under test: no test may start the real
// claude, so a test that reaches launchExec without a recorder fails.
func launchUnexpectedExec(argv0 string, argv, _ []string) error {
	panic("launchExec called without a recorder: " + argv0 + " " + strings.Join(argv, " "))
}

func init() {
	launchExec = launchUnexpectedExec
}

// launchStub is the path lifecycle.LaunchLookPath returns under launchRecord.
const launchStub = "/stub/bin/claude"

// launchRecorder records the one exec a launch makes.
type launchRecorder struct {
	calls      int
	path       string
	argv, envv []string
}

// launchRecord makes launch find claude at launchStub and record its exec,
// which then returns nil.
func launchRecord(t *testing.T) *launchRecorder {
	t.Helper()
	rec := &launchRecorder{}
	prevExec, prevLook := launchExec, lifecycle.LaunchLookPath
	launchExec = func(argv0 string, argv, envv []string) error {
		rec.calls++
		rec.path, rec.argv, rec.envv = argv0, slices.Clone(argv), slices.Clone(envv)
		return nil
	}
	lifecycle.LaunchLookPath = func(file string) (string, error) {
		if file != "claude" {
			return "", errors.New("unexpected lookup of " + file)
		}
		return launchStub, nil
	}
	t.Cleanup(func() { launchExec, lifecycle.LaunchLookPath = prevExec, prevLook })
	return rec
}

// launchEnter is a stdin that holds one Enter and records that it was read.
type launchEnter struct{ read bool }

func (r *launchEnter) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	return copy(p, "\n"), nil
}

// launchRun runs `curia launch args...` in-process with stdin.
func launchRun(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(append([]string{"launch"}, args...))
	t.Cleanup(func() {
		launchModel, launchTeam, launchResume, launchAfterPause = "", false, "", false
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err = rootCmd.Execute()
	launchModel, launchTeam, launchResume, launchAfterPause = "", false, "", false
	return out.String(), errOut.String(), err
}

// launchTask stores task T-1 as a spawn leaves it (branch and worktree set)
// and returns it.
func launchTask(t *testing.T) model.Task {
	t.Helper()
	db := testkit.HookHome(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	tk, err := st.CreateTask(ctx, task.Normalize(model.Task{
		Title: "Fix ; the $(auth)", Body: "Line one;\n  indented `x`\n\n- item", Repo: "/src/api", BaseBranch: "main",
		Status: model.TaskReady, CreatedAt: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
	}))
	if err != nil {
		t.Fatal(err)
	}
	tk, err = st.UpdateTask(ctx, tk.ID, func(cur model.Task) (model.Task, error) {
		cur.Branch, cur.Worktree = task.Branch(cur.ID, cur.Title), "/src/.curia-worktrees/api-T-1"
		return cur, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestLaunchPassesBriefByExec(t *testing.T) {
	tk := launchTask(t)
	rec := launchRecord(t)
	t.Setenv("CURIA_TASK_ID", "T-99")
	if _, stderr, err := launchRun(t, &launchEnter{}, "T-1"); err != nil {
		t.Fatalf("launch: %v\n%s", err, stderr)
	}
	if rec.calls != 1 || rec.path != launchStub {
		t.Fatalf("exec calls %d of %q, want one of %q", rec.calls, rec.path, launchStub)
	}
	want := []string{"claude", task.Brief(tk, false)}
	if !slices.Equal(rec.argv, want) {
		t.Errorf("argv = %q, want %q", rec.argv, want)
	}
	var ids []string
	for _, kv := range rec.envv {
		if name, v, _ := strings.Cut(kv, "="); name == "CURIA_TASK_ID" {
			ids = append(ids, v)
		}
	}
	if !slices.Equal(ids, []string{"T-1"}) {
		t.Errorf("CURIA_TASK_ID entries = %q, want exactly T-1", ids)
	}
	if slices.ContainsFunc(rec.envv, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=") }) {
		t.Error("the team switch is set without --team")
	}
}

// launchUUID is a session id for --resume.
const launchUUID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func TestLaunchModelTeamResume(t *testing.T) {
	tk := launchTask(t)
	cases := []struct {
		args, want []string
		teamEnv    bool
	}{
		{[]string{"T-1", "--model", "opus"}, []string{"claude", "--model", "opus", task.Brief(tk, false)}, false},
		{[]string{"T-1", "--team"}, []string{"claude", "--teammate-mode", "tmux", task.Brief(tk, true)}, true},
		{
			[]string{"T-1", "--model", "claude-opus-4[1m]", "--team"},
			[]string{"claude", "--model", "claude-opus-4[1m]", "--teammate-mode", "tmux", task.Brief(tk, true)},
			true,
		},
		{[]string{"T-1", "--resume", launchUUID}, []string{"claude", "--resume", launchUUID}, false},
		{
			[]string{"T-1", "--resume", launchUUID, "--model", "opus", "--team"},
			[]string{"claude", "--model", "opus", "--teammate-mode", "tmux", "--resume", launchUUID},
			true,
		},
		// After a pause the agent is told its work is in the WIP commit.
		{[]string{"T-1", "--resume", launchUUID, "--after-pause"}, []string{"claude", "--resume", launchUUID, lifecycle.LaunchAfterPauseNote("T-1")}, false},
		{[]string{"T-1", "--after-pause"}, []string{"claude", task.Brief(tk, false) + "\n\n" + lifecycle.LaunchAfterPauseNote("T-1")}, false},
	}
	for _, c := range cases {
		rec := launchRecord(t)
		if _, stderr, err := launchRun(t, &launchEnter{}, c.args...); err != nil {
			t.Fatalf("launch %q: %v\n%s", c.args, err, stderr)
		}
		if !slices.Equal(rec.argv, c.want) {
			t.Errorf("launch %q: argv = %q, want %q", c.args, rec.argv, c.want)
		}
		if got := slices.Contains(rec.envv, "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1"); got != c.teamEnv {
			t.Errorf("launch %q: team variable set %v, want %v", c.args, got, c.teamEnv)
		}
	}
}

func TestLaunchNeverAddsPermissionFlags(t *testing.T) {
	launchTask(t)
	for _, args := range [][]string{
		{"T-1"}, {"T-1", "--team"}, {"T-1", "--model", "opus"}, {"T-1", "--resume", launchUUID, "--team"},
	} {
		rec := launchRecord(t)
		if _, stderr, err := launchRun(t, &launchEnter{}, args...); err != nil {
			t.Fatalf("launch %q: %v\n%s", args, err, stderr)
		}
		for _, a := range rec.argv {
			if strings.HasPrefix(a, "--dangerously") || strings.HasPrefix(a, "--permission") {
				t.Errorf("launch %q passes %q", args, a)
			}
		}
	}
}

func TestLaunchFailureWaitsAndExitsOne(t *testing.T) {
	launchTask(t)
	check := func(name string, args []string, wantLog string) {
		t.Helper()
		stdin := &launchEnter{}
		stdout, stderr, err := launchRun(t, stdin, args...)
		if err == nil {
			t.Errorf("%s: launch succeeded", name)
		}
		if stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
			t.Errorf("%s: stdout %q, stderr %q; want one line on stderr", name, stdout, stderr)
		}
		if strings.ContainsFunc(strings.TrimSuffix(stderr, "\n"), func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			t.Errorf("%s: stderr not cleaned: %q", name, stderr)
		}
		if !stdin.read {
			t.Errorf("%s: launch did not wait for Enter", name)
		}
		if log := strings.Join(readLog(t), "\n"); !strings.Contains(log, wantLog) {
			t.Errorf("%s: log lacks %q:\n%s", name, wantLog, log)
		}
	}
	rec := launchRecord(t)
	check("unknown task", []string{"T-9"}, "no task T-9")

	lifecycle.LaunchLookPath = func(string) (string, error) {
		return "", errors.New("exec: \"claude\": \x1b[31mexecutable file not found\x1b[0m in $PATH")
	}
	check("claude missing", []string{"T-1"}, "executable file not found")

	launchRecord(t)
	launchExec = func(string, []string, []string) error { return errors.New("exec format error") }
	check("exec fails", []string{"T-1"}, "exec format error")
	if rec.calls != 0 {
		t.Errorf("exec called %d times", rec.calls)
	}
}

func TestLaunchRefusesTaskWithoutBranch(t *testing.T) {
	db := testkit.HookHome(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateTask(context.Background(), model.Task{
		Title: "Never spawned", Repo: "/src/api", BaseBranch: "main", Status: model.TaskBacklog, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	rec := launchRecord(t)
	_, stderr, err := launchRun(t, &launchEnter{}, "T-1")
	if err == nil || rec.calls != 0 || !strings.Contains(stderr, "no branch") {
		t.Errorf("launch of a task with no branch: err %v, %d execs, stderr %q", err, rec.calls, stderr)
	}
}

func TestLaunchRejectsBadArguments(t *testing.T) {
	db := testkit.HookHome(t)
	rec := launchRecord(t)
	for _, args := range [][]string{
		{"T-1;x"},
		{"T-1", "--model", "a b"},
		{"T-1", "--resume", "not-a-uuid"},
		{},
		{"T-1", "T-2"},
		{"T-1", "--no-such-flag"},
	} {
		stdin := &launchEnter{}
		_, stderr, err := launchRun(t, stdin, args...)
		if err == nil {
			t.Errorf("launch %q succeeded", args)
		}
		if strings.Count(stderr, "\n") != 1 || !stdin.read {
			t.Errorf("launch %q: stderr %q, waited %v; want one line and a wait", args, stderr, stdin.read)
		}
		if _, serr := os.Stat(db); !os.IsNotExist(serr) {
			t.Fatalf("launch %q opened the store (%v)", args, serr)
		}
	}
	if rec.calls != 0 {
		t.Errorf("exec called %d times", rec.calls)
	}
}

// launchCodexStub is the path lifecycle.LaunchLookPath returns for codex under
// launchRecordCodex.
const launchCodexStub = "/stub/bin/codex"

// launchRecordCodex makes launch find codex at launchCodexStub (and no
// other binary) and record its exec, which then returns nil.
func launchRecordCodex(t *testing.T) *launchRecorder {
	t.Helper()
	rec := launchRecord(t)
	lifecycle.LaunchLookPath = func(file string) (string, error) {
		if file != "codex" {
			return "", errors.New("unexpected lookup of " + file)
		}
		return launchCodexStub, nil
	}
	return rec
}

// launchCodexTask stores task T-1 as a spawn with --agent codex leaves it,
// with a hostile title and body, and returns it.
func launchCodexTask(t *testing.T) model.Task {
	t.Helper()
	tk := launchTask(t)
	st, err := store.Open(os.Getenv("CURIA_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	tk, err = st.UpdateTask(context.Background(), tk.ID, func(cur model.Task) (model.Task, error) {
		cur.Agent = model.AgentCodex
		cur.Title = "--dangerously-bypass-approvals-and-sandbox ; $(rm -rf ~)"
		cur.Body = "-s danger-full-access\n--sandbox none\n`id` ; kill-server ;\n#{pane_id}"
		return cur, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// launchTaskIDs is every CURIA_TASK_ID value in env.
func launchTaskIDs(env []string) []string {
	var ids []string
	for _, kv := range env {
		if name, v, _ := strings.Cut(kv, "="); name == "CURIA_TASK_ID" {
			ids = append(ids, v)
		}
	}
	return ids
}

func TestLaunchCodexPassesBriefByExec(t *testing.T) {
	tk := launchCodexTask(t)
	rec := launchRecordCodex(t)
	t.Setenv("CURIA_TASK_ID", "T-99")
	t.Setenv(lifecycle.LaunchTeamVar, "")
	if _, stderr, err := launchRun(t, &launchEnter{}, "T-1"); err != nil {
		t.Fatalf("launch: %v\n%s", err, stderr)
	}
	if rec.calls != 1 || rec.path != launchCodexStub {
		t.Fatalf("exec calls %d of %q, want one of %q", rec.calls, rec.path, launchCodexStub)
	}
	brief := task.Brief(tk, false)
	if !slices.Equal(rec.argv, []string{"codex", brief}) {
		t.Errorf("argv = %q, want [codex <brief>]", rec.argv)
	}
	if len(rec.argv) == 2 && rec.argv[1] != brief {
		t.Errorf("the brief is not byte-equal to task.Brief")
	}
	if !strings.Contains(brief, tk.Title) || !strings.Contains(brief, tk.Body) {
		t.Fatalf("test setup: the brief lacks the hostile title or body")
	}
	if ids := launchTaskIDs(rec.envv); !slices.Equal(ids, []string{"T-1"}) {
		t.Errorf("CURIA_TASK_ID entries = %q, want exactly T-1", ids)
	}
	if slices.Contains(rec.envv, lifecycle.LaunchTeamVar+"=1") {
		t.Error("the team switch is set for codex")
	}
}

func TestLaunchCodexModelAndResume(t *testing.T) {
	tk := launchCodexTask(t)
	brief := task.Brief(tk, false)
	cases := []struct {
		args, want []string
	}{
		{[]string{"T-1", "--model", "o3"}, []string{"codex", "--model", "o3", brief}},
		{[]string{"T-1", "--resume", launchUUID}, []string{"codex", "resume", launchUUID}},
		// codex resume takes no model here (section 4.10: codex resume <uuid>).
		{[]string{"T-1", "--resume", launchUUID, "--model", "o3"}, []string{"codex", "resume", launchUUID}},
		// After a pause the agent is told its work is in the WIP commit.
		{[]string{"T-1", "--resume", launchUUID, "--after-pause"}, []string{"codex", "resume", launchUUID, lifecycle.LaunchAfterPauseNote("T-1")}},
		{[]string{"T-1", "--after-pause"}, []string{"codex", brief + "\n\n" + lifecycle.LaunchAfterPauseNote("T-1")}},
	}
	for _, c := range cases {
		rec := launchRecordCodex(t)
		if _, stderr, err := launchRun(t, &launchEnter{}, c.args...); err != nil {
			t.Fatalf("launch %q: %v\n%s", c.args, err, stderr)
		}
		if rec.path != launchCodexStub || !slices.Equal(rec.argv, c.want) {
			t.Errorf("launch %q: exec %q argv %q, want %q", c.args, rec.path, rec.argv, c.want)
		}
		if ids := launchTaskIDs(rec.envv); !slices.Equal(ids, []string{"T-1"}) {
			t.Errorf("launch %q: CURIA_TASK_ID entries = %q", c.args, ids)
		}
	}

	// --team is refused for a codex task: agent teams are Claude Code's.
	rec := launchRecordCodex(t)
	stdin := &launchEnter{}
	_, stderr, err := launchRun(t, stdin, "T-1", "--team")
	if err == nil || rec.calls != 0 || !strings.Contains(stderr, "agent teams are Claude Code's") || !stdin.read {
		t.Errorf("launch --team of a codex task: err %v, %d execs, stderr %q, waited %v", err, rec.calls, stderr, stdin.read)
	}
}

func TestLaunchCodexNeverAddsSandboxOrApprovalFlags(t *testing.T) {
	launchCodexTask(t)
	for _, args := range [][]string{
		{"T-1"},
		{"T-1", "--model", "o3"},
		{"T-1", "--resume", launchUUID},
		{"T-1", "--after-pause"},
		{"T-1", "--resume", launchUUID, "--after-pause", "--model", "o3"},
	} {
		rec := launchRecordCodex(t)
		if _, stderr, err := launchRun(t, &launchEnter{}, args...); err != nil {
			t.Fatalf("launch %q: %v\n%s", args, err, stderr)
		}
		for _, a := range rec.argv {
			for _, bad := range []string{"--dangerously", "--approve", "-s", "--sandbox", "-a", "--ask-for-approval", "--full-auto", "--yolo"} {
				if strings.HasPrefix(a, bad) {
					t.Errorf("launch %q passes %q", args, a)
				}
			}
		}
	}
	// A model or session id that reads as an option never reaches codex.
	for _, args := range [][]string{
		{"T-1", "--model", "--dangerously-bypass-approvals-and-sandbox"},
		{"T-1", "--model=-s"},
		{"T-1", "--model=--sandbox"},
		{"T-1", "--resume", "------------------------------------"},
	} {
		rec := launchRecordCodex(t)
		if _, _, err := launchRun(t, &launchEnter{}, args...); err == nil || rec.calls != 0 {
			t.Errorf("launch %q: err %v, %d execs; want a refusal", args, err, rec.calls)
		}
	}
}

func TestLaunchCodexMissing(t *testing.T) {
	launchCodexTask(t)
	rec := launchRecordCodex(t)
	lifecycle.LaunchLookPath = func(file string) (string, error) {
		return "", errors.New("exec: \"" + file + "\": \x1b[31mexecutable file not found\x1b[0m in $PATH")
	}
	stdin := &launchEnter{}
	stdout, stderr, err := launchRun(t, stdin, "T-1")
	if err == nil || rec.calls != 0 {
		t.Fatalf("launch with codex missing: err %v, %d execs", err, rec.calls)
	}
	if stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, "curia launch: codex not found") {
		t.Errorf("stdout %q, stderr %q; want one line saying codex is not found", stdout, stderr)
	}
	if strings.ContainsFunc(strings.TrimSuffix(stderr, "\n"), func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		t.Errorf("stderr not cleaned: %q", stderr)
	}
	if !stdin.read {
		t.Error("launch did not wait for Enter")
	}
	if log := strings.Join(readLog(t), "\n"); !strings.Contains(log, "codex not found") {
		t.Errorf("log lacks the failure:\n%s", log)
	}
}

func TestLaunchClaudeUnchanged(t *testing.T) {
	tk := launchTask(t)
	if tk.Agent != model.AgentClaude {
		t.Fatalf("a task never spawned has agent %q, want claude", tk.Agent)
	}
	cases := []struct {
		args, want []string
	}{
		{[]string{"T-1"}, []string{"claude", task.Brief(tk, false)}},
		{[]string{"T-1", "--model", "opus"}, []string{"claude", "--model", "opus", task.Brief(tk, false)}},
		{[]string{"T-1", "--team"}, []string{"claude", "--teammate-mode", "tmux", task.Brief(tk, true)}},
		{[]string{"T-1", "--resume", launchUUID, "--model", "opus"}, []string{"claude", "--model", "opus", "--resume", launchUUID}},
	}
	for _, c := range cases {
		rec := launchRecord(t)
		if _, stderr, err := launchRun(t, &launchEnter{}, c.args...); err != nil {
			t.Fatalf("launch %q: %v\n%s", c.args, err, stderr)
		}
		if rec.path != launchStub || !slices.Equal(rec.argv, c.want) {
			t.Errorf("launch %q: exec %q argv %q, want %q at %q", c.args, rec.path, rec.argv, c.want, launchStub)
		}
	}
}

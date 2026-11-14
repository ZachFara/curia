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

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// launchUnexpectedExec is launchExec under test: no test may start the real
// claude, so a test that reaches launchExec without a recorder fails.
func launchUnexpectedExec(argv0 string, argv, _ []string) error {
	panic("launchExec called without a recorder: " + argv0 + " " + strings.Join(argv, " "))
}

func init() {
	launchExec = launchUnexpectedExec
}

// launchStub is the path launchLookPath returns under launchRecord.
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
	prevExec, prevLook := launchExec, launchLookPath
	launchExec = func(argv0 string, argv, envv []string) error {
		rec.calls++
		rec.path, rec.argv, rec.envv = argv0, slices.Clone(argv), slices.Clone(envv)
		return nil
	}
	launchLookPath = func(file string) (string, error) {
		if file != "claude" {
			return "", errors.New("unexpected lookup of " + file)
		}
		return launchStub, nil
	}
	t.Cleanup(func() { launchExec, launchLookPath = prevExec, prevLook })
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
		launchModel, launchTeam, launchResume = "", false, ""
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err = rootCmd.Execute()
	launchModel, launchTeam, launchResume = "", false, ""
	return out.String(), errOut.String(), err
}

// launchTask stores task T-1 as a spawn leaves it (branch and worktree set)
// and returns it.
func launchTask(t *testing.T) model.Task {
	t.Helper()
	db := hookHome(t)
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

	launchLookPath = func(string) (string, error) {
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
	db := hookHome(t)
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
	db := hookHome(t)
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

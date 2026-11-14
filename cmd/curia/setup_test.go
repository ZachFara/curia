package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupSum is the hex SHA-256 of content.
func setupSum(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// setupMarker is the file the test scripts create in the worktree.
func setupMarker(f *spawnFixture) string {
	return filepath.Join(f.worktree("T-1"), "made-by-setup")
}

// setupApproval is the approval stored for the fixture's repository.
func setupApproval(f *spawnFixture) string {
	f.t.Helper()
	sum, err := f.st.SetupApproval(context.Background(), f.repo)
	if err != nil {
		f.t.Fatal(err)
	}
	return sum
}

const setupTouch = "#!/bin/sh\ntouch made-by-setup\n"

// setupUnexpectedConfirm is setupConfirm under test: no test may read the
// real terminal, so a test that reaches the question without setupAnswer
// fails.
func setupUnexpectedConfirm(prompt string) (string, error) {
	panic("setupConfirm called without setupAnswer: " + prompt)
}

func init() {
	setupConfirm = setupUnexpectedConfirm
}

// setupAnswer makes the terminal question answer answer (or fail with err)
// for the rest of the test, and returns the prompts asked.
func setupAnswer(t *testing.T, answer string, err error) *[]string {
	t.Helper()
	var prompts []string
	prev := setupConfirm
	setupConfirm = func(p string) (string, error) {
		prompts = append(prompts, p)
		return answer, err
	}
	t.Cleanup(func() { setupConfirm = prev })
	return &prompts
}

func TestSetupUnapprovedScriptNotRun(t *testing.T) {
	f := spawnNewFixture(t)
	f.commitSetup(setupTouch, true)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if worktreeExists(setupMarker(f)) || worktreeExists(f.setupLog()) {
		t.Error("an unapproved setup script ran")
	}
	if !strings.Contains(out, "not approved") || !strings.Contains(out, "curia setup approve T-1\n") {
		t.Errorf("output does not say how to approve the script:\n%s", out)
	}
	if w := f.windows(); len(w) != 1 {
		t.Errorf("windows = %q, want the spawn to go on", w)
	}
}

func TestSetupApproveShowsAndApproves(t *testing.T) {
	f := spawnNewFixture(t)
	lines := []string{"#!/bin/sh", "touch made-by-setup", "\tif true; then", "\t\techo indented # ünïcode", "\tfi"}
	for i := len(lines) + 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("echo line %d", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	f.commitSetup(content, true)
	f.task("Setup", "")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil || worktreeExists(setupMarker(f)) {
		t.Fatalf("first spawn: %v, marker %v\n%s", err, worktreeExists(setupMarker(f)), out)
	}

	prompts := setupAnswer(t, setupSum(content)[:8], nil)
	out, err := runRoot(t, "setup", "approve", "T-1")
	if err != nil {
		t.Fatalf("setup approve: %v\n%s", err, out)
	}
	want := "Repository: " + f.repo + "\n" +
		"Script: " + filepath.Join(f.worktree("T-1"), ".curia", "setup.sh") + "\n" +
		fmt.Sprintf("Size: 50 lines, %d bytes\n", len(content)) +
		"SHA-256: " + setupSum(content) + "\n"
	for i, l := range lines {
		want += fmt.Sprintf("%4d  %s\n", i+1, l)
	}
	want += "(end of script: 50 lines)\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("approve output = %q, want it to start with the whole script %q", out, want)
	}
	if len(*prompts) != 1 || !strings.Contains((*prompts)[0], "first 8 characters") {
		t.Errorf("prompts = %q, want one asking for the first 8 characters", *prompts)
	}
	if got := setupApproval(f); got != setupSum(content) {
		t.Fatalf("approval = %q, want %q", got, setupSum(content))
	}

	// The approved script runs at the next spawn (the first window is gone
	// and its launch claim has lapsed).
	f.fake.Panes = nil
	f.now = f.now.Add(spawnSetupLimit + time.Minute)
	if _, out, err = f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("second spawn: %v\n%s", err, out)
	}
	if !worktreeExists(setupMarker(f)) {
		t.Errorf("the approved script did not run:\n%s", out)
	}
}

func TestSetupApproveRefusesWhatTheDisplayWouldHide(t *testing.T) {
	many := make([]string, setupMaxLines)
	for i := range many {
		many[i] = "true"
	}
	cases := []struct{ name, body, want string }{
		{"escape sequence", "echo \x1b[2Khidden\n", "U+001B"},
		{"bidi override", "echo \u202eevil\n", "U+202E"},
		{"zero-width space", "echo a\u200bb\n", "U+200B"},
		{"NUL", "echo a\x00b\n", "U+0000"},
		{"carriage return", "echo a\rrm -rf x\n", "U+000D"},
		{"invalid UTF-8", "echo \xff\n", "not valid UTF-8"},
		{"over-long line", "echo " + strings.Repeat("x", setupLineMax) + "\n", "longer than 200"},
		{"trailing space", "echo a \n", "trailing spaces"},
		{"one line too many", strings.Join(many, "\n") + "\n", "more than 300 lines"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := spawnNewFixture(t)
			content := setupTouch + c.body
			f.commitSetup(content, true)
			f.task("Setup", "")
			_, out, err := f.spawn("T-1", spawnOptions{})
			if err != nil || !strings.Contains(out, c.want) || worktreeExists(setupMarker(f)) {
				t.Errorf("spawn = %v, marker %v, want the script skipped (%q):\n%s", err, worktreeExists(setupMarker(f)), c.want, out)
			}
			prompts := setupAnswer(t, setupSum(content)[:8], nil)
			out, err = runRoot(t, "setup", "approve", "T-1")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("setup approve = %v, want a refusal naming %q\n%s", err, c.want, out)
			}
			if len(*prompts) != 0 || setupApproval(f) != "" {
				t.Errorf("asked %d times, approval %q; want no question and nothing stored", len(*prompts), setupApproval(f))
			}
		})
	}
}

func TestSetupApproveNeedsTerminalConfirmation(t *testing.T) {
	f := spawnNewFixture(t)
	f.commitSetup(setupTouch, true)
	f.task("Setup", "")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	prefix := setupSum(setupTouch)[:8]
	noTTY := filepath.Join(t.TempDir(), "no-tty")
	cases := []struct {
		name   string
		answer string
		err    error
		real   bool // use setupAskTerminal on a path that is no terminal
	}{
		{name: "wrong prefix", answer: "00000000"},
		{name: "longer than the prefix", answer: prefix + "0"},
		{name: "empty", answer: ""},
		{name: "end of input", err: io.EOF},
		{name: "no terminal", real: true},
	}
	for _, c := range cases {
		if c.real {
			prev := setupConfirm
			setupConfirm = func(p string) (string, error) { return setupAskTerminal(noTTY, p) }
			// Piped stdin holding the right answer must not approve.
			rootCmd.SetIn(strings.NewReader(prefix + "\n"))
			out, err := runRoot(t, "setup", "approve", "T-1")
			setupConfirm = prev
			rootCmd.SetIn(nil)
			if err == nil || !strings.Contains(err.Error(), "no terminal") {
				t.Errorf("%s: setup approve = %v\n%s", c.name, err, out)
			}
		} else {
			setupAnswer(t, c.answer, c.err)
			if out, err := runRoot(t, "setup", "approve", "T-1"); err == nil || !strings.Contains(err.Error(), "not approved") {
				t.Errorf("%s: setup approve = %v\n%s", c.name, err, out)
			}
		}
		if got := setupApproval(f); got != "" {
			t.Fatalf("%s: approval stored (%q)", c.name, got)
		}
	}
	setupAnswer(t, strings.ToUpper(prefix), nil)
	if out, err := runRoot(t, "setup", "approve", "T-1"); err != nil {
		t.Fatalf("setup approve with the right prefix: %v\n%s", err, out)
	}
	if got := setupApproval(f); got != setupSum(setupTouch) {
		t.Errorf("approval = %q, want the script's hash", got)
	}
}

func TestSetupChangedAfterApprovalNotRun(t *testing.T) {
	f := spawnNewFixture(t)
	f.approve(setupTouch)
	changed := setupTouch + "echo one more line\n"
	f.commitSetup(changed, true)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if worktreeExists(setupMarker(f)) {
		t.Error("a script changed since its approval ran")
	}
	if !strings.Contains(out, "not approved") {
		t.Errorf("output does not say the script was skipped:\n%s", out)
	}
}

func TestSetupApproveRefusedInsideAgent(t *testing.T) {
	f := spawnNewFixture(t)
	f.commitSetup(setupTouch, true)
	f.task("Setup", "")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	t.Setenv("CLAUDECODE", "1")
	out, err := runRoot(t, "setup", "approve", "T-1")
	if err == nil || !strings.Contains(err.Error(), "agent session") {
		t.Fatalf("setup approve inside an agent = %v\n%s", err, out)
	}
	if got := setupApproval(f); got != "" {
		t.Errorf("approval = %q, want none", got)
	}
}

func TestSetupSwapBetweenCheckAndExecRunsCheckedContent(t *testing.T) {
	f := spawnNewFixture(t)
	approved := "#!/bin/sh\necho approved > who-ran\n"
	f.setupScript(approved, true)
	f.task("Setup", "")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	wt := f.worktree("T-1")
	whoRan := filepath.Join(wt, "who-ran")
	if err := os.Remove(whoRan); err != nil {
		t.Fatalf("the approved script did not run: %v", err)
	}

	s, err := setupRead(wt)
	if err != nil || s.Sum != setupSum(approved) {
		t.Fatalf("setupRead = %+v, %v", s, err)
	}
	// The file is swapped after the check: what runs is what was checked.
	worktreeWrite(t, filepath.Join(wt, ".curia", "setup.sh"), "#!/bin/sh\necho swapped > who-ran\n")
	runErr, err := setupExec(context.Background(), f.get("T-1"), s, f.setupLog())
	if err != nil || runErr != nil {
		t.Fatalf("setupExec: %v, %v", runErr, err)
	}
	if b, _ := os.ReadFile(filepath.Clean(whoRan)); string(b) != "approved\n" {
		t.Errorf("who-ran = %q, want the checked content to have run", b)
	}
}

func TestSetupSizeBound(t *testing.T) {
	f := spawnNewFixture(t)
	big := setupTouch + "# " + strings.Repeat("x", setupMaxBytes) + "\n"
	f.setupScript(big, true)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if worktreeExists(setupMarker(f)) {
		t.Error("a setup script over the size bound ran")
	}
	if !strings.Contains(out, "larger than 64 KiB") {
		t.Errorf("output does not say the script is too large:\n%s", out)
	}
	if out, err = runRoot(t, "setup", "approve", "T-1"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("setup approve of a script over the bound = %v\n%s", err, out)
	}
}

func TestSetupNeedsShebang(t *testing.T) {
	f := spawnNewFixture(t)
	f.setupScript("touch made-by-setup\n", true)
	f.task("Setup", "")
	_, out, err := f.spawn("T-1", spawnOptions{})
	if err != nil || !strings.Contains(out, "has no #! line") || worktreeExists(setupMarker(f)) {
		t.Errorf("spawn = %v, marker %v, output:\n%s", err, worktreeExists(setupMarker(f)), out)
	}
}

func TestSetupEnvironmentIsMinimal(t *testing.T) {
	f := spawnNewFixture(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret-1")
	t.Setenv("GITHUB_TOKEN", "ghp-secret-2")
	t.Setenv("CURIA_SETUP_TEST_EXTRA", "extra")
	f.setupScript("#!/bin/sh\nenv\n", true)
	f.task("Setup", "")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	b, err := os.ReadFile(f.setupLog())
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	for _, bad := range []string{"secret", "ANTHROPIC_API_KEY", "GITHUB_TOKEN", "CURIA_SETUP_TEST_EXTRA"} {
		if strings.Contains(env, bad) {
			t.Errorf("the setup environment holds %q:\n%s", bad, env)
		}
	}
	for _, want := range []string{"CURIA_TASK_ID=T-1\n", "CURIA_REPO=" + f.repo + "\n", "PATH=", "HOME="} {
		if !strings.Contains(env, want) {
			t.Errorf("the setup environment lacks %q:\n%s", want, env)
		}
	}
}

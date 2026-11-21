package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/testkit"
)

// setupApproval is the approval stored for the fixture's repository.
func setupApproval(f *spawnFixture) string {
	f.T.Helper()
	sum, err := f.St.SetupApproval(context.Background(), f.Repo)
	if err != nil {
		f.T.Fatal(err)
	}
	return sum
}

// setupUnexpectedConfirm is promptTerminal under test: no test may read the
// real terminal, so a test that reaches the question without setupAnswer
// fails.
func setupUnexpectedConfirm(prompt string) (string, error) {
	panic("promptTerminal called without setupAnswer: " + prompt)
}

func init() {
	promptTerminal = setupUnexpectedConfirm
}

// setupAnswer makes the terminal question answer answer (or fail with err)
// for the rest of the test, and returns the prompts asked.
func setupAnswer(t *testing.T, answer string, err error) *[]string {
	t.Helper()
	var prompts []string
	prev := promptTerminal
	promptTerminal = func(p string) (string, error) {
		prompts = append(prompts, p)
		return answer, err
	}
	t.Cleanup(func() { promptTerminal = prev })
	return &prompts
}

func TestSetupApproveShowsAndApproves(t *testing.T) {
	f := spawnNewFixture(t)
	lines := []string{"#!/bin/sh", "touch made-by-setup", "\tif true; then", "\t\techo indented # ünïcode", "\tfi"}
	for i := len(lines) + 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("echo line %d", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	f.CommitSetup(content, true)
	f.Task("Setup", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil || testkit.WorktreeExists(testkit.SetupMarker(f.SpawnFixture)) {
		t.Fatalf("first spawn: %v, marker %v\n%s", err, testkit.WorktreeExists(testkit.SetupMarker(f.SpawnFixture)), out)
	}

	prompts := setupAnswer(t, testkit.SetupSum(content)[:8], nil)
	out, err := runRoot(t, "setup", "approve", "T-1")
	if err != nil {
		t.Fatalf("setup approve: %v\n%s", err, out)
	}
	want := "Repository: " + f.Repo + "\n" +
		"Script: " + filepath.Join(f.Worktree("T-1"), ".curia", "setup.sh") + "\n" +
		fmt.Sprintf("Size: 50 lines, %d bytes\n", len(content)) +
		"SHA-256: " + testkit.SetupSum(content) + "\n"
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
	if got := setupApproval(f); got != testkit.SetupSum(content) {
		t.Fatalf("approval = %q, want %q", got, testkit.SetupSum(content))
	}

	// The approved script runs at the next spawn (the first window is gone
	// and its launch claim has lapsed).
	f.Fake.Panes = nil
	f.Now = f.Now.Add(lifecycle.SpawnSetupLimit + time.Minute)
	if _, out, err = f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("second spawn: %v\n%s", err, out)
	}
	if !testkit.WorktreeExists(testkit.SetupMarker(f.SpawnFixture)) {
		t.Errorf("the approved script did not run:\n%s", out)
	}
}

func TestSetupApproveRefusesWhatTheDisplayWouldHide(t *testing.T) {
	many := make([]string, lifecycle.SetupMaxLines)
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
		{"over-long line", "echo " + strings.Repeat("x", lifecycle.SetupLineMax) + "\n", "longer than 200"},
		{"trailing space", "echo a \n", "trailing spaces"},
		{"one line too many", strings.Join(many, "\n") + "\n", "more than 300 lines"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := spawnNewFixture(t)
			content := testkit.SetupTouch + c.body
			f.CommitSetup(content, true)
			f.Task("Setup", "")
			_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
			if err != nil || !strings.Contains(out, c.want) || testkit.WorktreeExists(testkit.SetupMarker(f.SpawnFixture)) {
				t.Errorf("spawn = %v, marker %v, want the script skipped (%q):\n%s", err, testkit.WorktreeExists(testkit.SetupMarker(f.SpawnFixture)), c.want, out)
			}
			prompts := setupAnswer(t, testkit.SetupSum(content)[:8], nil)
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
	f.CommitSetup(testkit.SetupTouch, true)
	f.Task("Setup", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	prefix := testkit.SetupSum(testkit.SetupTouch)[:8]
	noTTY := filepath.Join(t.TempDir(), "no-tty")
	cases := []struct {
		name   string
		answer string
		err    error
		real   bool // use promptTerminalAt on a path that is no terminal
	}{
		{name: "wrong prefix", answer: "00000000"},
		{name: "longer than the prefix", answer: prefix + "0"},
		{name: "empty", answer: ""},
		{name: "end of input", err: io.EOF},
		{name: "no terminal", real: true},
	}
	for _, c := range cases {
		if c.real {
			prev := promptTerminal
			promptTerminal = func(p string) (string, error) { return promptTerminalAt(noTTY, p) }
			// Piped stdin holding the right answer must not approve.
			rootCmd.SetIn(strings.NewReader(prefix + "\n"))
			out, err := runRoot(t, "setup", "approve", "T-1")
			promptTerminal = prev
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
	if got := setupApproval(f); got != testkit.SetupSum(testkit.SetupTouch) {
		t.Errorf("approval = %q, want the script's hash", got)
	}
}

func TestSetupApproveRefusedInsideAgent(t *testing.T) {
	f := spawnNewFixture(t)
	f.CommitSetup(testkit.SetupTouch, true)
	f.Task("Setup", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
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

func TestSetupSizeBound(t *testing.T) {
	f := spawnNewFixture(t)
	big := testkit.SetupTouch + "# " + strings.Repeat("x", lifecycle.SetupMaxBytes) + "\n"
	f.SetupScript(big, true)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if testkit.WorktreeExists(testkit.SetupMarker(f.SpawnFixture)) {
		t.Error("a setup script over the size bound ran")
	}
	if !strings.Contains(out, "larger than 64 KiB") {
		t.Errorf("output does not say the script is too large:\n%s", out)
	}
	if out, err = runRoot(t, "setup", "approve", "T-1"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("setup approve of a script over the bound = %v\n%s", err, out)
	}
}

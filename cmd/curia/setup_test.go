package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	lines := []string{"#!/bin/sh", "touch made-by-setup", "  # evil \x1b]0;title\x07 here"}
	for i := len(lines) + 1; i <= 50; i++ {
		lines = append(lines, fmt.Sprintf("echo line %d", i))
	}
	content := strings.Join(lines, "\n") + "\n"
	f.commitSetup(content, true)
	f.task("Setup", "")
	if _, out, err := f.spawn("T-1", spawnOptions{}); err != nil || worktreeExists(setupMarker(f)) {
		t.Fatalf("first spawn: %v, marker %v\n%s", err, worktreeExists(setupMarker(f)), out)
	}

	out, err := runRoot(t, "setup", "approve", "T-1")
	if err != nil {
		t.Fatalf("setup approve: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Script: " + filepath.Join(f.worktree("T-1"), ".curia", "setup.sh") + "\n",
		fmt.Sprintf("Size: %d bytes\n", len(content)),
		"SHA-256: " + setupSum(content) + "\n",
		"  touch made-by-setup\n", "    # evil  here\n", "  echo line 40\n", "(10 more lines not shown)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("approve output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "line 41") || strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\a') {
		t.Errorf("approve output shows more than 40 lines or is not cleaned: %q", out)
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

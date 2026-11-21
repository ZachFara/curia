package lifecycle_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/testkit"
)

func TestSetupUnapprovedScriptNotRun(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.CommitSetup(testkit.SetupTouch, true)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if testkit.WorktreeExists(testkit.SetupMarker(f)) || testkit.WorktreeExists(f.SetupLog()) {
		t.Error("an unapproved setup script ran")
	}
	if !strings.Contains(out, "not approved") || !strings.Contains(out, "curia setup approve T-1\n") {
		t.Errorf("output does not say how to approve the script:\n%s", out)
	}
	if w := f.Windows(); len(w) != 1 {
		t.Errorf("windows = %q, want the spawn to go on", w)
	}
}

func TestSetupChangedAfterApprovalNotRun(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.Approve(testkit.SetupTouch)
	changed := testkit.SetupTouch + "echo one more line\n"
	f.CommitSetup(changed, true)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if testkit.WorktreeExists(testkit.SetupMarker(f)) {
		t.Error("a script changed since its approval ran")
	}
	if !strings.Contains(out, "not approved") {
		t.Errorf("output does not say the script was skipped:\n%s", out)
	}
}

func TestSetupSwapBetweenCheckAndExecRunsCheckedContent(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	approved := "#!/bin/sh\necho approved > who-ran\n"
	f.SetupScript(approved, true)
	f.Task("Setup", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	wt := f.Worktree("T-1")
	whoRan := filepath.Join(wt, "who-ran")
	if err := os.Remove(whoRan); err != nil {
		t.Fatalf("the approved script did not run: %v", err)
	}

	s, err := lifecycle.SetupRead(wt)
	if err != nil || s.Sum != testkit.SetupSum(approved) {
		t.Fatalf("lifecycle.SetupRead = %+v, %v", s, err)
	}
	// The file is swapped after the check: what runs is what was checked.
	testkit.WorktreeWrite(t, filepath.Join(wt, ".curia", "setup.sh"), "#!/bin/sh\necho swapped > who-ran\n")
	runErr, err := lifecycle.SetupExec(context.Background(), f.Get("T-1"), s, f.SetupLog())
	if err != nil || runErr != nil {
		t.Fatalf("lifecycle.SetupExec: %v, %v", runErr, err)
	}
	if b, _ := os.ReadFile(filepath.Clean(whoRan)); string(b) != "approved\n" {
		t.Errorf("who-ran = %q, want the checked content to have run", b)
	}
}

func TestSetupNeedsShebang(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	f.SetupScript("touch made-by-setup\n", true)
	f.Task("Setup", "")
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil || !strings.Contains(out, "has no #! line") || testkit.WorktreeExists(testkit.SetupMarker(f)) {
		t.Errorf("spawn = %v, marker %v, output:\n%s", err, testkit.WorktreeExists(testkit.SetupMarker(f)), out)
	}
}

func TestSetupEnvironmentIsMinimal(t *testing.T) {
	f := testkit.NewSpawnFixture(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret-1")
	t.Setenv("GITHUB_TOKEN", "ghp-secret-2")
	t.Setenv("CURIA_SETUP_TEST_EXTRA", "extra")
	f.SetupScript("#!/bin/sh\nenv\n", true)
	f.Task("Setup", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	b, err := os.ReadFile(f.SetupLog())
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	for _, bad := range []string{"secret", "ANTHROPIC_API_KEY", "GITHUB_TOKEN", "CURIA_SETUP_TEST_EXTRA"} {
		if strings.Contains(env, bad) {
			t.Errorf("the setup environment holds %q:\n%s", bad, env)
		}
	}
	for _, want := range []string{"CURIA_TASK_ID=T-1\n", "CURIA_REPO=" + f.Repo + "\n", "PATH=", "HOME="} {
		if !strings.Contains(env, want) {
			t.Errorf("the setup environment lacks %q:\n%s", want, env)
		}
	}
}

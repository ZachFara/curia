package gitops

import (
	"context"
	"os/exec"
	"testing"
)

// TestReviewObjectIDLen: the length --head must have is asked of git: 40
// for a SHA-1 repository, 64 for a SHA-256 one.
func TestReviewObjectIDLen(t *testing.T) {
	for format, want := range map[string]int{"sha1": 40, "sha256": 64} {
		dir := t.TempDir()
		cmd := exec.Command("git", "init", "-q", "--object-format="+format, dir)
		cmd.Env = Env()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init --object-format=%s: %v %s", format, err, out)
		}
		if got, err := ObjectIDLen(context.Background(), dir); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", format, got, err, want)
		}
	}
}

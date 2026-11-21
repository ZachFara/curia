package testkit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
)

// LifecycleBranch is task T-1's branch ("Fix auth").
const LifecycleBranch = "curia/T-1-fix-auth"

// ReviewBranch is task T-1's branch in the review fixture.
const ReviewBranch = LifecycleBranch

// ReviewFixture is a spawn fixture with task T-1 "Fix auth" spawned (its
// worktree on curia/T-1-fix-auth), one commit on the branch adding auth.go,
// a local bare repository as origin, and review R-1 pending, opened by the
// lead session (not linked yet).
type ReviewFixture struct {
	*SpawnFixture
	Wt, Origin string
}

// NewReview is sf (task T-1 spawned) with a commit on the branch, a bare origin and review R-1 pending.
func NewReview(t *testing.T, sf *SpawnFixture) *ReviewFixture {
	t.Helper()
	f := &ReviewFixture{SpawnFixture: sf}
	f.Wt = f.Worktree("T-1")
	f.CommitOnBranch("auth.go", "package auth\n", "fix auth")
	f.Origin = filepath.Join(filepath.Dir(f.Repo), "origin.git")
	cmd := exec.Command("git", "init", "-q", "--bare", f.Origin)
	cmd.Env = gitops.Env()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v %s", err, out)
	}
	GitRun(t, f.Repo, "remote", "add", "origin", f.Origin)
	if _, _, err := f.St.CompleteTask(context.Background(), model.Review{
		TaskID: "T-1", SessionID: LifecycleLead, Summary: "Fixed the auth refresh.", Tests: model.TestsPassed,
		TestsDetail: "12 passed", CreatedAt: f.Now,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// CommitOnBranch commits file with content in the worktree.
func (f *ReviewFixture) CommitOnBranch(file, content, subject string) {
	f.T.Helper()
	WorktreeWrite(f.T, filepath.Join(f.Wt, file), content)
	GitRun(f.T, f.Wt, "add", "-A")
	GitRun(f.T, f.Wt, "commit", "-q", "-m", subject)
}

// CommitOnMain commits file with content in the main checkout.
func (f *ReviewFixture) CommitOnMain(file, content, subject string) {
	f.T.Helper()
	WorktreeWrite(f.T, filepath.Join(f.Repo, file), content)
	GitRun(f.T, f.Repo, "add", "-A")
	GitRun(f.T, f.Repo, "commit", "-q", "-m", subject)
}

// Head is the commit rev names in dir.
func (f *ReviewFixture) Head(dir, rev string) string {
	f.T.Helper()
	return WorktreeGit(f.T, dir, "rev-parse", rev)
}

// Status is the main checkout's porcelain status.
func (f *ReviewFixture) Status() string {
	f.T.Helper()
	return WorktreeGit(f.T, f.Repo, "status", "--porcelain", "--untracked-files=all")
}

// Review is R-1 as stored.
func (f *ReviewFixture) Review() model.Review {
	f.T.Helper()
	r, found, err := f.St.GetReview(context.Background(), "R-1")
	if err != nil || !found {
		f.T.Fatalf("GetReview(R-1): found %v, err %v", found, err)
	}
	return r
}

// WantPending fails unless R-1 is pending.
func (f *ReviewFixture) WantPending() {
	f.T.Helper()
	if r := f.Review(); r.Status != model.ReviewPending {
		f.T.Errorf("R-1 is %s, want pending", r.Status)
	}
}

// HasMergeHead reports whether the main checkout has a merge under way.
func (f *ReviewFixture) HasMergeHead() bool {
	ok, err := gitops.RefExists(context.Background(), f.Repo, "MERGE_HEAD")
	if err != nil {
		f.T.Fatal(err)
	}
	return ok
}

// WantMerged checks that R-1 is recorded merged at head and that the
// main checkout holds the merge, clean, with no merge under way.
func (f *ReviewFixture) WantMerged(head string) {
	f.T.Helper()
	if r := f.Review(); r.Status != model.ReviewMerged || r.Head != head {
		f.T.Errorf("R-1 is %s at %q, want merged at %s", r.Status, r.Head, head)
	}
	if got := f.Head(f.Repo, "main^2"); got != head {
		f.T.Errorf("main^2 = %s, want the approved %s", got, head)
	}
	if s := f.Status(); s != "" {
		f.T.Errorf("status = %q, want clean", s)
	}
	if f.HasMergeHead() {
		f.T.Error("MERGE_HEAD is left")
	}
	if _, err := os.Stat(filepath.Join(f.Repo, ".git", "index.lock")); err == nil {
		f.T.Error("index.lock is left")
	}
}

// LifecycleHooks makes core.hooksPath of the fixture's repository a
// directory holding one executable hook, name, with content. The hook is
// made executable through the index and a checkout, so no test writes a
// file with an execute bit.
func LifecycleHooks(f *SpawnFixture, name, content string) {
	f.T.Helper()
	dir := filepath.Join(filepath.Dir(f.Repo), "hooks")
	if err := os.Mkdir(dir, 0o700); err != nil {
		f.T.Fatal(err)
	}
	GitInit(f.T, dir, "main")
	WorktreeWrite(f.T, filepath.Join(dir, name), content)
	GitRun(f.T, dir, "add", "-A")
	GitRun(f.T, dir, "update-index", "--chmod=+x", name)
	GitRun(f.T, dir, "commit", "-q", "-m", "hook")
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		f.T.Fatal(err)
	}
	GitRun(f.T, dir, "checkout", "--", name)
	GitRun(f.T, f.Repo, "config", "core.hooksPath", dir)
}

// SpawnedFrom is f with its repository ignoring node_modules/ and .env and
// task T-1 "Fix auth" spawned with o (window pane %100, no wait) and no
// session linked yet.
func SpawnedFrom(f *SpawnFixture, o lifecycle.SpawnOptions) *SpawnFixture {
	f.T.Helper()
	WorktreeWrite(f.T, filepath.Join(f.Repo, ".gitignore"), "node_modules/\n.env\n")
	GitRun(f.T, f.Repo, "add", "-A")
	GitRun(f.T, f.Repo, "commit", "-q", "-m", "ignore")
	f.Task("Fix auth", "Make it work.")
	if _, out, err := f.Spawn("T-1", o); err != nil {
		f.T.Fatalf("spawn: %v\n%s", err, out)
	}
	return f
}

// Executable makes dir/name an executable file holding content,
// through the index and a checkout of a throwaway repository, so no test
// writes a file with an execute bit.
func Executable(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		GitInit(t, dir, "main")
	}
	path := filepath.Join(dir, name)
	WorktreeWrite(t, path, content)
	GitRun(t, dir, "add", name)
	GitRun(t, dir, "update-index", "--chmod=+x", name)
	GitRun(t, dir, "commit", "-q", "-m", name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	GitRun(t, dir, "checkout", "--", name)
}

// RecordGit puts a git on PATH that records each call's argv in a
// file and runs the real git, and returns a func that reads and clears the
// recorded calls.
func RecordGit(t *testing.T) func() [][]string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bin")
	calls := filepath.Join(filepath.Dir(dir), "calls")
	Executable(t, dir, "git", "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\037' \"$a\"; done >> '"+calls+
		"'\nprintf '\\036' >> '"+calls+"'\nexec '"+realGit+"' \"$@\"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() [][]string {
		t.Helper()
		b, err := os.ReadFile(filepath.Clean(calls))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(calls, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var out [][]string
		for c := range strings.SplitSeq(strings.TrimSuffix(string(b), "\x1e"), "\x1e") {
			out = append(out, strings.Split(strings.TrimSuffix(c, "\x1f"), "\x1f"))
		}
		return out
	}
}

// WaitFile waits up to 30 s for path to exist.
func WaitFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
	}
}

// LifecycleRead returns the content of path.
func LifecycleRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

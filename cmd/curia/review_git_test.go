package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/task"
)

// reviewBranch is task T-1's branch in the review fixture.
const reviewBranch = lifecycleBranch

// reviewFixture is a spawn fixture with task T-1 "Fix auth" spawned (its
// worktree on curia/T-1-fix-auth), one commit on the branch adding auth.go,
// a local bare repository as origin, and review R-1 pending, opened by the
// lead session (not linked yet).
type reviewFixture struct {
	*spawnFixture
	wt, origin string
}

func reviewNew(t *testing.T) *reviewFixture {
	t.Helper()
	f := &reviewFixture{spawnFixture: lifecycleSpawned(t, spawnOptions{})}
	f.wt = f.worktree("T-1")
	f.commitOnBranch("auth.go", "package auth\n", "fix auth")
	f.origin = filepath.Join(filepath.Dir(f.repo), "origin.git")
	cmd := exec.Command("git", "init", "-q", "--bare", f.origin)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v %s", err, out)
	}
	gitRun(t, f.repo, "remote", "add", "origin", f.origin)
	if _, _, err := f.st.CompleteTask(context.Background(), model.Review{
		TaskID: "T-1", SessionID: lifecycleLead, Summary: "Fixed the auth refresh.", Tests: model.TestsPassed,
		TestsDetail: "12 passed", CreatedAt: f.now,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// commitOnBranch commits file with content in the worktree.
func (f *reviewFixture) commitOnBranch(file, content, subject string) {
	f.t.Helper()
	worktreeWrite(f.t, filepath.Join(f.wt, file), content)
	gitRun(f.t, f.wt, "add", "-A")
	gitRun(f.t, f.wt, "commit", "-q", "-m", subject)
}

// commitOnMain commits file with content in the main checkout.
func (f *reviewFixture) commitOnMain(file, content, subject string) {
	f.t.Helper()
	worktreeWrite(f.t, filepath.Join(f.repo, file), content)
	gitRun(f.t, f.repo, "add", "-A")
	gitRun(f.t, f.repo, "commit", "-q", "-m", subject)
}

func (f *reviewFixture) head(dir, rev string) string {
	f.t.Helper()
	return worktreeGit(f.t, dir, "rev-parse", rev)
}

func (f *reviewFixture) status() string {
	f.t.Helper()
	return worktreeGit(f.t, f.repo, "status", "--porcelain", "--untracked-files=all")
}

func (f *reviewFixture) review() model.Review {
	f.t.Helper()
	r, found, err := f.st.GetReview(context.Background(), "R-1")
	if err != nil || !found {
		f.t.Fatalf("GetReview(R-1): found %v, err %v", found, err)
	}
	return r
}

func (f *reviewFixture) wantPending() {
	f.t.Helper()
	if r := f.review(); r.Status != model.ReviewPending {
		f.t.Errorf("R-1 is %s, want pending", r.Status)
	}
}

// approve runs reviewApprove on R-1 as the flag form does. seen "" is the
// branch's head now, as for a user who has just looked at it (a branch that
// does not resolve gives a commit id no branch is at).
func (f *reviewFixture) approve(how model.ApproveHow, seen string) (string, error) {
	f.t.Helper()
	if seen == "" {
		seen = strings.Repeat("0", 40)
		if res := runGit(context.Background(), f.repo, "rev-parse", "--verify", "-q", "refs/heads/"+reviewBranch); res.err == nil {
			seen = res.out
		}
	}
	return reviewApprove(context.Background(), f.st, f.cfg, "R-1", how, seen, model.AnsweredCLI, f.now)
}

// hasMergeHead reports whether the main checkout has a merge under way.
func (f *reviewFixture) hasMergeHead() bool {
	ok, err := refExists(context.Background(), f.repo, "MERGE_HEAD")
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

// reviewNoControl fails when s holds a control rune other than a line break.
func reviewNoControl(t *testing.T, what, s string) {
	t.Helper()
	for _, r := range s {
		if r != '\n' && (unicode.IsControl(r) || !unicode.IsPrint(r) && r != ' ') {
			t.Errorf("%s holds %U: %q", what, r, s)
			return
		}
	}
}

func TestDiffStatAgainstBase(t *testing.T) {
	f := reviewNew(t)
	f.commitOnBranch("README", "hello, auth\n", "readme")
	f.commitOnMain("main.txt", "moved on\n", "main moves on")
	stat, note, head := reviewDiffStat(context.Background(), f.get("T-1"), 100)
	for _, want := range []string{"auth.go", "README", "2 files changed"} {
		if !strings.Contains(stat, want) {
			t.Errorf("stat lacks %q:\n%s", want, stat)
		}
	}
	if strings.Contains(stat, "main.txt") {
		t.Errorf("stat holds the base's own change (two dots, not three):\n%s", stat)
	}
	if note != "" {
		t.Errorf("note = %q, want none", note)
	}
	if want := f.head(f.wt, "HEAD"); head != want {
		t.Errorf("head = %q, want the branch's %q", head, want)
	}
	// The width is git's --stat width.
	f.commitOnBranch(strings.Repeat("long-name-", 12)+".go", "x\n", "long")
	narrow, _, _ := reviewDiffStat(context.Background(), f.get("T-1"), 40)
	for l := range strings.SplitSeq(narrow, "\n") {
		if n := len([]rune(l)); n > 40 && !strings.Contains(l, "changed") {
			t.Errorf("at width 40 a line is %d wide: %q", n, l)
		}
	}
}

func TestDiffStatNotes(t *testing.T) {
	f := reviewNew(t)
	ctx := context.Background()
	worktreeWrite(t, filepath.Join(f.wt, "scratch.txt"), "not committed\n")
	if _, note, _ := reviewDiffStat(ctx, f.get("T-1"), 100); note != "the worktree has uncommitted changes that are not in this diff" {
		t.Errorf("dirty worktree: note = %q", note)
	}

	gone := model.Task{ID: "T-7", Repo: f.repo, BaseBranch: "main", Branch: "curia/T-7-gone"}
	if stat, note, head := reviewDiffStat(ctx, gone, 100); note != "branch curia/T-7-gone does not exist" || stat != "" || head != "" {
		t.Errorf("missing branch: stat %q, note %q, head %q", stat, note, head)
	}

	gitRun(t, f.repo, "branch", "curia/T-8-same", "main")
	same := model.Task{ID: "T-8", Repo: f.repo, BaseBranch: "main", Branch: "curia/T-8-same"}
	if _, note, _ := reviewDiffStat(ctx, same, 100); note != "the branch has no commits beyond main" {
		t.Errorf("no commits beyond base: note = %q", note)
	}

	none := model.Task{ID: "T-9", Repo: f.repo, BaseBranch: "main"}
	if _, note, _ := reviewDiffStat(ctx, none, 100); !strings.Contains(note, "no branch") {
		t.Errorf("no branch: note = %q", note)
	}

	hostile := model.Task{ID: "T-9", Repo: f.repo, BaseBranch: "main", Branch: "--output=/tmp/x"}
	if stat, note, _ := reviewDiffStat(ctx, hostile, 100); stat != "" || !strings.Contains(note, "not a valid branch name") {
		t.Errorf("hostile branch: stat %q, note %q", stat, note)
	}
}

func TestDiffStatIsCappedAndClean(t *testing.T) {
	f := reviewNew(t)
	for i := range 400 {
		worktreeWrite(t, filepath.Join(f.wt, "many", fmt.Sprintf("f%03d.txt", i)), "x\n")
	}
	worktreeWrite(t, filepath.Join(f.wt, "esc\x1b[31mred.txt"), "x\n")
	gitRun(t, f.wt, "add", "-A")
	gitRun(t, f.wt, "commit", "-q", "-m", "many")
	stat, _, _ := reviewDiffStat(context.Background(), f.get("T-1"), 100)
	lines := strings.Split(stat, "\n")
	// 402 files (auth.go, the ESC file, 400 more) and the summary: 403 lines.
	if len(lines) != 201 || lines[200] != "… 203 more lines" {
		t.Errorf("stat has %d lines ending %q, want 200 and \"… 203 more lines\"", len(lines), lines[len(lines)-1])
	}
	if len(stat) > 64<<10 {
		t.Errorf("stat is %d bytes, want at most 64 KiB", len(stat))
	}
	reviewNoControl(t, "stat", stat)
}

func TestDiffStatWorksWithoutWorktree(t *testing.T) {
	f := reviewNew(t)
	gitRun(t, f.repo, "worktree", "remove", f.wt) // as a pause leaves it
	stat, note, head := reviewDiffStat(context.Background(), f.get("T-1"), 100)
	if !strings.Contains(stat, "auth.go") || note != "" || head == "" {
		t.Errorf("without the worktree: stat %q, note %q, head %q; want the stat from the main repository", stat, note, head)
	}
}

func TestMergeHappyPath(t *testing.T) {
	f := reviewNew(t)
	mainBefore, branchHead := f.head(f.repo, "main"), f.head(f.wt, "HEAD")
	stat, head, err := reviewCheck(context.Background(), f.st, f.get("T-1"))
	if err != nil {
		t.Fatal(err)
	}
	if head != branchHead || !strings.Contains(stat, "auth.go") {
		t.Fatalf("reviewCheck = %q, %q; want the branch head %q and its stat", stat, head, branchHead)
	}
	if err = reviewMerge(context.Background(), f.get("T-1"), head); err != nil {
		t.Fatal(err)
	}
	if got := f.head(f.repo, "main^1"); got != mainBefore {
		t.Errorf("first parent = %s, want main's old head %s", got, mainBefore)
	}
	if got := f.head(f.repo, "main^2"); got != branchHead {
		t.Errorf("second parent = %s, want the branch head %s", got, branchHead)
	}
	if got := worktreeGit(t, f.repo, "log", "-1", "--format=%s"); got != "Merge branch '"+reviewBranch+"' into main" {
		t.Errorf("merge subject = %q", got)
	}
	if got := f.head(f.wt, "HEAD"); got != branchHead {
		t.Errorf("worktree HEAD = %s, want it untouched at %s", got, branchHead)
	}
	if got := worktreeGit(t, f.wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+reviewBranch {
		t.Errorf("worktree is on %q", got)
	}
	if s := f.status(); s != "" {
		t.Errorf("main checkout status = %q, want clean", s)
	}
}

func TestMergeRefusesMovedBranch(t *testing.T) {
	f := reviewNew(t)
	_, head, err := reviewCheck(context.Background(), f.st, f.get("T-1"))
	if err != nil {
		t.Fatal(err)
	}
	f.commitOnBranch("late.go", "package auth\n", "late") // after the checks, before the merge
	before := f.head(f.repo, "HEAD")
	if err = reviewMerge(context.Background(), f.get("T-1"), head); !errors.Is(err, errReviewBranchChanged) {
		t.Fatalf("reviewMerge = %v, want the changed-branch refusal", err)
	}
	if f.head(f.repo, "HEAD") != before || f.status() != "" {
		t.Error("the repository changed")
	}
}

func TestMergeIntoBaseThatMoved(t *testing.T) {
	f := reviewNew(t)
	f.commitOnMain("main.txt", "moved on\n", "main moves on")
	moved, branchHead := f.head(f.repo, "main"), f.head(f.wt, "HEAD")
	if _, err := f.approve(model.ApproveMerge, ""); err != nil {
		t.Fatal(err)
	}
	if f.head(f.repo, "main^1") != moved || f.head(f.repo, "main^2") != branchHead {
		t.Errorf("parents = %s %s, want %s %s", f.head(f.repo, "main^1"), f.head(f.repo, "main^2"), moved, branchHead)
	}
}

// reviewRefusal is one row of TestMergeRefusals: a change to the fixture
// and the refusal it must cause.
type reviewRefusal struct {
	name, want string
	setup      func(f *reviewFixture)
	seen       string
}

func reviewRefusals() []reviewRefusal {
	setBranch := func(b string) func(f *reviewFixture) {
		return func(f *reviewFixture) { f.update("T-1", func(tk *model.Task) { tk.Branch = b }) }
	}
	rows := []reviewRefusal{
		{name: "no branch", want: "T-1 has no branch", setup: setBranch("")},
		{name: "branch does not resolve", want: "branch " + reviewBranch + " does not exist", setup: func(f *reviewFixture) {
			gitRun(f.t, f.repo, "worktree", "remove", f.wt)
			gitRun(f.t, f.repo, "branch", "-D", reviewBranch)
		}},
		{name: "base -x", want: `refusing base "-x"`, setup: func(f *reviewFixture) {
			f.update("T-1", func(tk *model.Task) { tk.BaseBranch = "-x" })
		}},
		{name: "agent working", want: "the agent is still working; wait for it to stop", setup: func(f *reviewFixture) {
			lifecycleSession(f.spawnFixture, lifecycleLead, "%100", model.StateWorking, f.now)
		}},
		{name: "agent waiting", want: "the agent is still working; wait for it to stop", setup: func(f *reviewFixture) {
			lifecycleSession(f.spawnFixture, lifecycleLead, "%100", model.StateWaiting, f.now)
		}},
		{name: "dirty worktree", want: "uncommitted changes in ", setup: func(f *reviewFixture) {
			worktreeWrite(f.t, filepath.Join(f.wt, "auth.go"), "package auth // edited\n")
		}},
		{name: "no commits beyond base", want: "nothing to merge: " + reviewBranch + " has no commits beyond main", setup: func(f *reviewFixture) {
			gitRun(f.t, f.repo, "merge", "-q", "--ff-only", reviewBranch)
		}},
		{
			name: "another branch checked out", want: " has other checked out, not main: check out main there, or approve with --pr",
			setup: func(f *reviewFixture) { gitRun(f.t, f.repo, "switch", "-q", "-c", "other") },
		},
		{name: "detached HEAD", want: " has a detached HEAD checked out, not main", setup: func(f *reviewFixture) {
			gitRun(f.t, f.repo, "switch", "-q", "--detach")
		}},
		{name: "rebase under way", want: " is in the middle of a rebase", setup: func(f *reviewFixture) {
			if err := os.Mkdir(filepath.Join(f.repo, ".git", "rebase-merge"), 0o700); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "task stored dropped", want: "T-1 is dropped: nothing to approve", setup: func(f *reviewFixture) {
			f.update("T-1", func(tk *model.Task) { tk.Status = model.TaskDropped })
		}},
		{name: "branch moved since shown", want: errReviewBranchChanged.Error(), seen: strings.Repeat("0", 40)},
	}
	for _, b := range []string{"-x", "--upload-pack=touch /tmp/pwned", "curia/T-1;rm -rf", "a..b", "/abs", "curia/T-1 x", "$(id)"} {
		rows = append(rows, reviewRefusal{name: "hostile branch " + b, want: "not a valid branch name", setup: setBranch(b)})
	}
	return rows
}

func TestMergeRefusals(t *testing.T) {
	for _, row := range reviewRefusals() {
		t.Run(row.name, func(t *testing.T) {
			f := reviewNew(t)
			if row.setup != nil {
				row.setup(f)
			}
			before := f.head(f.repo, "HEAD")
			line, err := f.approve(model.ApproveMerge, row.seen)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("approve = %q, %v; want an error with %q", line, err, row.want)
			}
			if got := f.head(f.repo, "HEAD"); got != before {
				t.Errorf("HEAD moved from %s to %s", before, got)
			}
			f.wantPending()
		})
	}
}

func TestMergeRefusesWhileUserMerges(t *testing.T) {
	f := reviewNew(t)
	// The user's own merge, stopped on a conflict.
	gitRun(t, f.repo, "switch", "-q", "-c", "mine")
	f.commitOnMain("README", "mine\n", "mine")
	gitRun(t, f.repo, "switch", "-q", "main")
	f.commitOnMain("README", "theirs\n", "theirs")
	cmd := exec.Command("git", "-C", f.repo, "merge", "mine")
	cmd.Env = gitEnv()
	if err := cmd.Run(); err == nil {
		t.Fatal("the user's merge did not stop on a conflict")
	}
	status := f.status()
	_, err := f.approve(model.ApproveMerge, "")
	if err == nil || !strings.Contains(err.Error(), "is in the middle of a merge") {
		t.Fatalf("approve error = %v, want the refusal of a merge under way", err)
	}
	if !f.hasMergeHead() {
		t.Error("the user's merge was aborted")
	}
	if got := f.status(); got != status {
		t.Errorf("status changed from %q to %q", status, got)
	}
	f.wantPending()
}

func TestMergeConflictAborts(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprintf("unrelated local change %v", dirty), func(t *testing.T) {
			f := reviewNew(t)
			f.commitOnBranch("README", "branch side\n", "branch readme")
			f.commitOnMain("README", "main side\n", "main readme")
			if dirty {
				f.commitOnMain("notes.txt", "a\n", "notes")
				worktreeWrite(t, filepath.Join(f.repo, "notes.txt"), "my edit\n")
			}
			before, status := f.head(f.repo, "HEAD"), f.status()
			_, err := f.approve(model.ApproveMerge, "")
			if err == nil || !strings.HasSuffix(err.Error(), "; nothing was merged") || !strings.Contains(err.Error(), "CONFLICT") {
				t.Fatalf("approve error = %v, want git's conflict message ending in \"; nothing was merged\"", err)
			}
			if f.hasMergeHead() {
				t.Error("MERGE_HEAD is left")
			}
			if got := f.status(); got != status {
				t.Errorf("status = %q, want %q as before", got, status)
			}
			if got := f.head(f.repo, "HEAD"); got != before {
				t.Errorf("HEAD moved to %s", got)
			}
			f.wantPending()
		})
	}
}

func TestMergeHookFailureAborts(t *testing.T) {
	for _, hook := range []string{"commit-msg", "pre-merge-commit"} {
		t.Run(hook, func(t *testing.T) {
			f := reviewNew(t)
			lifecycleHooks(f.spawnFixture, hook, "#!/bin/sh\necho \"refused by "+hook+"\" >&2\nexit 1\n")
			before := f.head(f.repo, "HEAD")
			_, err := f.approve(model.ApproveMerge, "")
			if err == nil || !strings.Contains(err.Error(), "refused by "+hook) || !strings.HasSuffix(err.Error(), "; nothing was merged") {
				t.Fatalf("approve error = %v, want the hook's message and \"; nothing was merged\"", err)
			}
			if f.hasMergeHead() {
				t.Error("MERGE_HEAD is left")
			}
			if s := f.status(); s != "" {
				t.Errorf("status = %q, want clean", s)
			}
			if got := f.head(f.repo, "HEAD"); got != before {
				t.Errorf("HEAD moved to %s", got)
			}
			f.wantPending()
		})
	}
}

func TestMergeKeepsMyLocalChanges(t *testing.T) {
	f := reviewNew(t)
	f.commitOnMain("notes.txt", "a\n", "notes")
	worktreeWrite(t, filepath.Join(f.repo, "notes.txt"), "my edit\n")
	if _, err := f.approve(model.ApproveMerge, ""); err != nil {
		t.Fatalf("unrelated change: %v", err)
	}
	if got := lifecycleRead(t, filepath.Join(f.repo, "notes.txt")); got != "my edit\n" {
		t.Errorf("notes.txt = %q, want my edit kept", got)
	}
	if s := f.status(); s != "M notes.txt" { // trimmed: " M notes.txt"
		t.Errorf("status = %q, want notes.txt still modified", s)
	}

	g := reviewNew(t)
	g.commitOnBranch("README", "branch side\n", "branch readme")
	worktreeWrite(t, filepath.Join(g.repo, "README"), "my edit\n")
	before, status := g.head(g.repo, "HEAD"), g.status()
	_, err := g.approve(model.ApproveMerge, "")
	if err == nil || !strings.Contains(err.Error(), "would be overwritten") || !strings.HasSuffix(err.Error(), "; nothing was merged") {
		t.Fatalf("overlapping change: error = %v, want git's refusal", err)
	}
	if got := lifecycleRead(t, filepath.Join(g.repo, "README")); got != "my edit\n" {
		t.Errorf("README = %q, want my edit intact", got)
	}
	if g.head(g.repo, "HEAD") != before || g.status() != status {
		t.Error("HEAD or status changed")
	}
	g.wantPending()
}

// reviewExecutable makes dir/name an executable file holding content,
// through the index and a checkout of a throwaway repository, so no test
// writes a file with an execute bit.
func reviewExecutable(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		gitInit(t, dir, "main")
	}
	path := filepath.Join(dir, name)
	worktreeWrite(t, path, content)
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "update-index", "--chmod=+x", name)
	gitRun(t, dir, "commit", "-q", "-m", name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "checkout", "--", name)
}

// reviewRecordGit puts a git on PATH that records each call's argv in a
// file and runs the real git, and returns a func that reads and clears the
// recorded calls.
func reviewRecordGit(t *testing.T) func() [][]string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "bin")
	calls := filepath.Join(filepath.Dir(dir), "calls")
	reviewExecutable(t, dir, "git", "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\037' \"$a\"; done >> '"+calls+
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

// reviewForbidden are the argv elements no approval may pass to git.
var reviewForbidden = []string{
	"--force", "-f", "--no-verify", "reset", "checkout", "stash", "clean", "-X",
	"--allow-unrelated-histories", "--force-with-lease",
}

// reviewCheckArgv fails when a recorded git call holds a forbidden
// argument, or (some) when there was no call.
func reviewCheckArgv(t *testing.T, what string, calls [][]string, some bool) {
	t.Helper()
	if some && len(calls) == 0 {
		t.Errorf("%s: the recording git saw no call", what)
	}
	for _, c := range calls {
		for _, a := range c {
			if slices.Contains(reviewForbidden, a) || strings.HasPrefix(a, "--force") {
				t.Errorf("%s: git %q holds %q", what, c, a)
			}
		}
	}
}

func TestNeverForces(t *testing.T) {
	f := reviewNew(t)
	calls := reviewRecordGit(t)
	if _, err := f.approve(model.ApproveMerge, ""); err != nil {
		t.Fatal(err)
	}
	reviewCheckArgv(t, "merge", calls(), true)

	for _, row := range reviewRefusals() {
		g := reviewNew(t)
		if row.setup != nil {
			row.setup(g)
		}
		calls()
		if _, err := g.approve(model.ApproveMerge, row.seen); err == nil {
			t.Errorf("%s: approved", row.name)
		}
		reviewCheckArgv(t, row.name, calls(), false)
	}

	c := reviewNew(t)
	c.commitOnBranch("README", "branch side\n", "branch readme")
	c.commitOnMain("README", "main side\n", "main readme")
	calls()
	if _, err := c.approve(model.ApproveMerge, ""); err == nil {
		t.Error("conflict: approved")
	}
	reviewCheckArgv(t, "conflict", calls(), true)

	p := reviewNew(t)
	reviewUseGH(t, "https://github.com/o/r/pull/7\n")
	calls()
	if _, err := p.approve(model.ApprovePR, ""); err != nil {
		t.Fatal(err)
	}
	reviewCheckArgv(t, "pull request", calls(), true)
}

func TestPRPushesAndCallsGh(t *testing.T) {
	f := reviewNew(t)
	gh := reviewUseGH(t, "Creating pull request\nhttps://github.com/o/r/pull/7\n")
	head, mainBefore := f.head(f.wt, "HEAD"), f.head(f.repo, "main")
	line, err := f.approve(model.ApprovePR, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := worktreeGit(t, f.origin, "rev-parse", "refs/heads/"+reviewBranch); got != head {
		t.Errorf("origin has %s at %s, want %s", reviewBranch, got, head)
	}
	if _, err = worktreeRun(context.Background(), worktreeGitLimit, f.origin, "rev-parse", "--verify", "-q", "refs/heads/main"); err == nil {
		t.Error("the base was pushed")
	}
	tk, r := f.get("T-1"), f.review()
	want := []string{
		"pr", "create", "--base", "main", "--head", reviewBranch,
		"--title", task.PRTitle(tk), "--body", task.PRBody(tk, r, nil),
	}
	if len(gh.calls) != 1 || gh.calls[0].dir != f.repo || !slices.Equal(gh.calls[0].args, want) {
		t.Errorf("gh calls = %+v, want one in %s with %q", gh.calls, f.repo, want)
	}
	if want := "R-1 approved: pull request https://github.com/o/r/pull/7 opened for " + reviewBranch + "."; line != want {
		t.Errorf("line = %q, want %q", line, want)
	}
	if r.Status != model.ReviewApproved || r.PRURL != "https://github.com/o/r/pull/7" || r.Head != head ||
		r.ApprovedHow != model.ApprovePR || !strings.Contains(r.DiffStat, "auth.go") {
		t.Errorf("review = %+v, want approved with the URL, head and stat", r)
	}
	if got := f.head(f.repo, "main"); got != mainBefore {
		t.Errorf("main moved to %s in pull-request mode", got)
	}
}

func TestPRArgvIsDirect(t *testing.T) {
	f := reviewNew(t)
	hostile := "a; rm -rf / $(touch x) `id` @octocat\nnext line\r\n--title=evil"
	f.update("T-1", func(tk *model.Task) { tk.Title = "@all fix; $(x) `y`" })
	if _, _, err := f.st.CompleteTask(context.Background(), model.Review{
		TaskID: "T-1", SessionID: lifecycleLead, Summary: hostile + "\n~~~~\nescaped?", Tests: model.TestsFailed,
		TestsDetail: "1 failed; $(x)", CreatedAt: f.now,
	}); err != nil {
		t.Fatal(err)
	}
	gh := reviewUseGH(t, "https://github.com/o/r/pull/8\n")
	if _, err := f.approve(model.ApprovePR, ""); err != nil {
		t.Fatal(err)
	}
	tk := f.get("T-1")
	r := f.review()
	if len(gh.calls) != 1 || len(gh.calls[0].args) != 10 {
		t.Fatalf("gh calls = %+v, want one with 10 arguments", gh.calls)
	}
	args := gh.calls[0].args
	if args[7] != task.PRTitle(tk) || strings.Contains(args[7], "@") {
		t.Errorf("title = %q, want task.PRTitle %q without @", args[7], task.PRTitle(tk))
	}
	if args[9] != task.PRBody(tk, r, nil) {
		t.Errorf("body = %q, want task.PRBody byte for byte", args[9])
	}
	if !strings.HasSuffix(args[9], "\n~~~~\n") || strings.Count(args[9], "~~~~") != 2 {
		t.Errorf("body's fence is not intact: %q", args[9])
	}
	for i, a := range args {
		if i%2 == 1 && i > 1 && strings.HasPrefix(a, "-") {
			t.Errorf("argument %d %q reaches a flag position", i, a)
		}
	}
}

func TestPRRejectsOddURL(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{"see http://x", ""},
		{"javascript:alert(1)", ""},
		{"https://" + strings.Repeat("a", 300), ""},
		{"https://github.com/o/r/pull/1?x=y", ""},
		{"https://github.com/o/r/pull/1 extra", ""},
		{"https://github.com/o/r/pull/1\x1b[0m", ""},
		{"", ""},
		{"Creating pull request\nhttps://github.com/o/r/pull/1\n", "https://github.com/o/r/pull/1"},
		{"https://github.com/o/r/pull/1\nsee http://x", ""},
	} {
		if got := reviewPRURL([]byte(tc.out)); got != tc.want {
			t.Errorf("reviewPRURL(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
	f := reviewNew(t)
	reviewUseGH(t, "javascript:alert(1)\n")
	line, err := f.approve(model.ApprovePR, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.PRURL != "" || r.Status != model.ReviewApproved {
		t.Errorf("review = %+v, want approved with no URL", r)
	}
	if !strings.Contains(line, "gh printed no URL") || strings.Contains(line, "javascript") {
		t.Errorf("line = %q", line)
	}
}

// reviewRealGH is reviewGH as the command ships it, kept before the test
// default replaces it.
var reviewRealGH = reviewGH

func TestPRGhMissingOrFailing(t *testing.T) {
	f := reviewNew(t)
	reviewUseGH(t, "")
	reviewGHLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	_, err := f.approve(model.ApprovePR, "")
	if err == nil || !strings.Contains(err.Error(), "gh is not on PATH") || !strings.Contains(err.Error(), "--merge") {
		t.Fatalf("gh missing: error = %v", err)
	}
	if _, err = worktreeRun(context.Background(), worktreeGitLimit, f.origin, "rev-parse", "--verify", "-q", "refs/heads/"+reviewBranch); err == nil {
		t.Error("gh missing: the branch was pushed")
	}
	f.wantPending()

	// The real reviewGH, with a stand-in gh on PATH that fails after the push.
	g := reviewNew(t)
	bin := filepath.Join(t.TempDir(), "bin")
	reviewExecutable(t, bin, "gh", "#!/bin/sh\nprintf 'GraphQL: \\033[31mno\\033[0m permission\\nsecond line\\n' >&2\nexit 1\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	reviewGH, reviewGHLookPath = reviewRealGH, exec.LookPath
	_, err = g.approve(model.ApprovePR, "")
	if err == nil || !strings.Contains(err.Error(), "gh pr create failed") || !strings.Contains(err.Error(), "GraphQL: no permission") {
		t.Fatalf("gh failing: error = %v, want its first stderr line", err)
	}
	if strings.Contains(err.Error(), "second line") || strings.ContainsRune(err.Error(), '\x1b') {
		t.Errorf("gh failing: error = %q, want only the first line, cleaned", err)
	}
	if !strings.Contains(err.Error(), reviewBranch+" is pushed to origin") {
		t.Errorf("gh failing: error = %q, want it to say the branch is pushed", err)
	}
	if got := worktreeGit(t, g.origin, "rev-parse", "refs/heads/"+reviewBranch); got != g.head(g.wt, "HEAD") {
		t.Errorf("origin has %s, want the branch pushed", got)
	}
	g.wantPending()
}

func TestPRRealGhRunsInRepoWithArgv(t *testing.T) {
	f := reviewNew(t)
	bin := filepath.Join(t.TempDir(), "bin")
	argv := filepath.Join(filepath.Dir(bin), "argv")
	reviewExecutable(t, bin, "gh", "#!/bin/sh\npwd > '"+argv+"'\nfor a in \"$@\"; do printf '%s\\037' \"$a\"; done >> '"+argv+
		"'\necho https://github.com/o/r/pull/9\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prevGH, prevLook := reviewGH, reviewGHLookPath
	reviewGH, reviewGHLookPath = reviewRealGH, exec.LookPath
	t.Cleanup(func() { reviewGH, reviewGHLookPath = prevGH, prevLook })
	if _, err := f.approve(model.ApprovePR, ""); err != nil {
		t.Fatal(err)
	}
	dir, args, _ := strings.Cut(lifecycleRead(t, argv), "\n")
	if dir != f.repo {
		t.Errorf("gh ran in %q, want %q", dir, f.repo)
	}
	tk, r := f.get("T-1"), f.review()
	want := strings.Join([]string{
		"pr", "create", "--base", "main", "--head", reviewBranch,
		"--title", task.PRTitle(tk), "--body", task.PRBody(tk, r, nil),
	}, "\x1f") + "\x1f"
	if args != want {
		t.Errorf("gh argv = %q, want %q", args, want)
	}
	if r.PRURL != "https://github.com/o/r/pull/9" {
		t.Errorf("pr_url = %q", r.PRURL)
	}
}

func TestPRRefusedWhenBranchMovesAfterPush(t *testing.T) {
	f := reviewNew(t)
	head := f.head(f.wt, "HEAD")
	gh := reviewUseGH(t, "https://github.com/o/r/pull/7\n")
	// A commit lands on the branch while the push runs: the pre-push hook
	// stands in for the agent.
	lifecycleHooks(f.spawnFixture, "pre-push", "#!/bin/sh\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE\ncd '"+f.wt+"' && echo late > late.txt && git add late.txt && "+
		"git -c user.name=t -c user.email=t@example.com commit -q -m late --no-verify\n")
	_, err := f.approve(model.ApprovePR, "")
	if err == nil || !strings.Contains(err.Error(), "is pushed to origin at "+head[:7]+", but "+errReviewBranchChanged.Error()) {
		t.Fatalf("error = %v, want the pushed-but-changed refusal", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("gh ran: %+v", gh.calls)
	}
	if got := worktreeGit(t, f.origin, "rev-parse", "refs/heads/"+reviewBranch); got != head {
		t.Errorf("origin has %s, want exactly the approved %s", got, head)
	}
	f.wantPending()
}

func TestDiffCmdTable(t *testing.T) {
	f := reviewNew(t)
	var looked []string
	reviewUseDiffTools(t, func(name string) (string, error) {
		looked = append(looked, name)
		if name == "missing" {
			return "", exec.ErrNotFound
		}
		return "/opt/bin/" + name, nil
	})
	tk := f.get("T-1")
	rng := "main..." + reviewBranch
	for _, tc := range []struct {
		tool string
		want []string
	}{
		{"git", []string{"/opt/bin/git", "-C", f.repo, "diff", rng}},
		{"delta", []string{"/opt/bin/git", "-C", f.repo, "-c", "core.pager=delta", "diff", rng}},
		{"lazygit", []string{"/opt/bin/lazygit", "-p", f.wt}},
		{"nvim", []string{"/opt/bin/git", "-C", f.repo, "difftool", "--no-prompt", "--tool=nvimdiff", rng}},
	} {
		cmd, err := reviewDiffCmd(tk, tc.tool)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		if got := append([]string{cmd.Path}, cmd.Args[1:]...); !slices.Equal(got, tc.want) {
			t.Errorf("%s: argv = %q, want %q", tc.tool, got, tc.want)
		}
		if !slices.Contains(cmd.Env, "LC_ALL=C") || cmd.Stdin != nil || cmd.Stdout != nil {
			t.Errorf("%s: env or stdio not as the view needs", tc.tool)
		}
	}
	if !slices.Contains(looked, "delta") || !slices.Contains(looked, "nvim") {
		t.Errorf("looked up %q, want delta and nvim checked", looked)
	}
	gitRun(t, f.repo, "worktree", "remove", f.wt)
	if cmd, err := reviewDiffCmd(tk, "lazygit"); err != nil || !slices.Equal(cmd.Args[1:], []string{"-p", f.repo}) {
		t.Errorf("lazygit without worktree: %v, %v; want -p the repository", cmd, err)
	}
}

func TestDiffCmdRefusals(t *testing.T) {
	f := reviewNew(t)
	tk := f.get("T-1")
	reviewUseDiffTools(t, func(string) (string, error) { return "", exec.ErrNotFound })
	for tool, name := range map[string]string{"git": "git", "lazygit": "lazygit", "delta": "delta", "nvim": "nvim"} {
		if _, err := reviewDiffCmd(tk, tool); err == nil || !strings.Contains(err.Error(), "the diff tool "+name+" is not on PATH") {
			t.Errorf("%s not found: error = %v, want one naming %s", tool, err, name)
		}
	}
	reviewDiffLookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	if _, err := reviewDiffCmd(tk, "vimdiff; rm"); err == nil || !strings.Contains(err.Error(), "unknown diff tool") {
		t.Errorf("unknown tool: error = %v", err)
	}
	tk.Branch = "-x"
	if _, err := reviewDiffCmd(tk, "git"); err == nil {
		t.Error("hostile branch: no error")
	}
}

// reviewGHCall is one recorded gh call.
type reviewGHCall struct {
	dir  string
	args []string
}

// reviewGHRecorder records gh calls and answers with out and err.
type reviewGHRecorder struct{ calls []reviewGHCall }

// reviewUseGH makes reviewGH a recorder answering out, and finds gh.
func reviewUseGH(t *testing.T, out string) *reviewGHRecorder {
	t.Helper()
	rec := &reviewGHRecorder{}
	prevGH, prevLook := reviewGH, reviewGHLookPath
	reviewGH = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		rec.calls = append(rec.calls, reviewGHCall{dir: dir, args: slices.Clone(args)})
		return []byte(out), nil
	}
	reviewGHLookPath = func(string) (string, error) { return "/opt/bin/gh", nil }
	t.Cleanup(func() { reviewGH, reviewGHLookPath = prevGH, prevLook })
	return rec
}

// reviewUseDiffTools makes reviewDiffLookPath look.
func reviewUseDiffTools(t *testing.T, look func(string) (string, error)) {
	t.Helper()
	prev := reviewDiffLookPath
	reviewDiffLookPath = look
	t.Cleanup(func() { reviewDiffLookPath = prev })
}

// The test defaults: no test runs the real gh or a diff tool unless it sets
// its own (TestPRGhMissingOrFailing runs the real reviewGH against a
// stand-in gh).
func init() {
	reviewGH = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		panic("reviewGH called without a recorder: gh " + strings.Join(args, " "))
	}
	reviewGHLookPath = func(name string) (string, error) {
		panic("reviewGHLookPath called without a stub: " + name)
	}
	reviewDiffLookPath = func(name string) (string, error) {
		panic("reviewDiffLookPath called without a stub: " + name)
	}
}

// reviewGitCalls are the recorded git calls whose subcommand (after -C dir)
// is sub.
func reviewGitCalls(calls [][]string, sub string) [][]string {
	var out [][]string
	for _, c := range calls {
		if len(c) > 2 && c[0] == "-C" && c[2] == sub {
			out = append(out, c[2:])
		}
	}
	return out
}

// TestMergeCutShortRestoresCheckout: the approval's limit runs out while a
// pre-merge-commit hook runs, before git writes MERGE_HEAD. Curia stops git
// (SIGTERM, to its own process group), undoes what the merge staged with
// git reset --merge, the only reset it runs, and the checkout is as before:
// HEAD, git status with the user's own unstaged and untracked work, no
// MERGE_HEAD, no index.lock; the review stays pending.
func TestMergeCutShortRestoresCheckout(t *testing.T) {
	for _, hook := range []string{"#!/bin/sh\nsleep 4\nexit 0\n", "#!/bin/sh\ntrap '' TERM\nsleep 4\nexit 0\n"} {
		t.Run(strings.Split(hook, "\n")[1], func(t *testing.T) { reviewCutShortCase(t, hook) })
	}
}

// reviewCutShortCase is one case of TestMergeCutShortRestoresCheckout, with
// hook as the pre-merge-commit hook.
func reviewCutShortCase(t *testing.T, hook string) {
	f := reviewNew(t)
	f.commitOnMain("notes.txt", "a\n", "notes")
	worktreeWrite(t, filepath.Join(f.repo, "notes.txt"), "my edit\n")
	worktreeWrite(t, filepath.Join(f.repo, "mine.txt"), "my own uncommitted work\n")
	lifecycleHooks(f.spawnFixture, "pre-merge-commit", hook)
	before, status, head := f.head(f.repo, "HEAD"), f.status(), f.head(f.wt, "HEAD")
	calls := reviewRecordGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, head, model.AnsweredCLI, f.now)
	if err == nil || !strings.Contains(err.Error(), "cut short before it recorded the merge") ||
		!strings.Contains(err.Error(), "Curia undid what it had staged (git reset --merge)") ||
		!strings.HasSuffix(err.Error(), "Nothing was merged") {
		t.Fatalf("approve error = %v, want the cut-short merge undone", err)
	}
	f.wantAsBefore(before, status)
	if _, err := os.Stat(filepath.Join(f.repo, "auth.go")); err == nil {
		t.Error("auth.go, which the merge brought, is left")
	}
	for file, want := range map[string]string{"notes.txt": "my edit\n", "mine.txt": "my own uncommitted work\n"} {
		if got := lifecycleRead(t, filepath.Join(f.repo, file)); got != want {
			t.Errorf("%s = %q, want %q kept", file, got, want)
		}
	}
	f.wantPending()
	recorded := calls()
	if resets := reviewGitCalls(recorded, "reset"); len(resets) != 1 || !slices.Equal(resets[0], []string{"reset", "--merge"}) {
		t.Errorf("reset calls = %q, want exactly one git reset --merge", resets)
	}
	reviewCheckArgv(t, "cut short", slices.DeleteFunc(recorded, func(c []string) bool {
		return slices.Equal(c[2:], []string{"reset", "--merge"})
	}), true)
}

// wantAsBefore checks that the main checkout is at HEAD before with git
// status status, no merge under way and no index.lock.
func (f *reviewFixture) wantAsBefore(before, status string) {
	f.t.Helper()
	if got := f.head(f.repo, "HEAD"); got != before {
		f.t.Errorf("HEAD moved to %s", got)
	}
	if got := f.status(); got != status {
		f.t.Errorf("status = %q, want %q as before", got, status)
	}
	if f.hasMergeHead() {
		f.t.Error("MERGE_HEAD is left")
	}
	if _, err := os.Stat(filepath.Join(f.repo, ".git", "index.lock")); err == nil {
		f.t.Error("index.lock is left")
	}
}

// TestMergeRefusesStagedIndex: with staged changes in the user's checkout,
// nothing is merged and nothing is touched.
func TestMergeRefusesStagedIndex(t *testing.T) {
	f := reviewNew(t)
	f.commitOnMain("notes.txt", "a\n", "notes")
	worktreeWrite(t, filepath.Join(f.repo, "notes.txt"), "staged edit\n")
	gitRun(t, f.repo, "add", "notes.txt")
	before, status := f.head(f.repo, "HEAD"), f.status()
	calls := reviewRecordGit(t)
	_, err := f.approve(model.ApproveMerge, "")
	if err == nil || !strings.Contains(err.Error(), "staged changes in "+f.repo+": commit or unstage them first. Nothing was merged") {
		t.Fatalf("approve error = %v, want the staged-index refusal", err)
	}
	if f.head(f.repo, "HEAD") != before || f.status() != status {
		t.Error("HEAD or status changed")
	}
	recorded := calls()
	if m := reviewGitCalls(recorded, "merge"); len(m) != 0 {
		t.Errorf("git merge ran: %q", m)
	}
	if r := reviewGitCalls(recorded, "reset"); len(r) != 0 {
		t.Errorf("git reset ran: %q", r)
	}
	f.wantPending()
}

// reviewUseGHFunc makes reviewGH answer with answer, and finds gh.
func reviewUseGHFunc(t *testing.T, answer func(args []string) ([]byte, error)) *reviewGHRecorder {
	t.Helper()
	rec := &reviewGHRecorder{}
	prevGH, prevLook := reviewGH, reviewGHLookPath
	reviewGH = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		rec.calls = append(rec.calls, reviewGHCall{dir: dir, args: slices.Clone(args)})
		return answer(args)
	}
	reviewGHLookPath = func(string) (string, error) { return "/opt/bin/gh", nil }
	t.Cleanup(func() { reviewGH, reviewGHLookPath = prevGH, prevLook })
	return rec
}

// TestPRApproveAgainRecordsExistingPR: gh pr create fails because the pull
// request exists (an earlier approve's gh failed late, or the user opened
// it): gh pr view finds it open, from the branch into the base, and the
// review is recorded approved with its URL. One that is closed, into
// another base or with an odd URL is not taken, and the review stays
// pending.
func TestPRApproveAgainRecordsExistingPR(t *testing.T) {
	view := func(json string) func([]string) ([]byte, error) {
		return func(args []string) ([]byte, error) {
			if args[1] == "create" {
				return nil, errors.New("exit status 1: a pull request for branch \"" + reviewBranch + "\" into branch \"main\" already exists")
			}
			return []byte(json), nil
		}
	}
	pr := func(url, state, base string) string {
		return `{"url":"` + url + `","state":"` + state + `","baseRefName":"` + base + `","headRefName":"` + reviewBranch + `"}`
	}
	f := reviewNew(t)
	gh := reviewUseGHFunc(t, view(pr("https://github.com/o/r/pull/5", "OPEN", "main")))
	line, err := f.approve(model.ApprovePR, "")
	if err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.Status != model.ReviewApproved || r.PRURL != "https://github.com/o/r/pull/5" {
		t.Errorf("R-1 = %+v, want approved with the existing pull request", r)
	}
	if !strings.Contains(line, "https://github.com/o/r/pull/5") {
		t.Errorf("line = %q", line)
	}
	if len(gh.calls) != 2 || !slices.Equal(gh.calls[1].args, []string{"pr", "view", reviewBranch, "--json", "url,state,baseRefName,headRefName"}) {
		t.Errorf("gh calls = %+v", gh.calls)
	}
	for _, bad := range []string{
		pr("https://github.com/o/r/pull/5", "CLOSED", "main"), pr("https://github.com/o/r/pull/5", "MERGED", "main"),
		pr("https://github.com/o/r/pull/5", "OPEN", "release"), pr("javascript:alert(1)", "OPEN", "main"), "not json",
	} {
		g := reviewNew(t)
		reviewUseGHFunc(t, view(bad))
		if _, err := g.approve(model.ApprovePR, ""); err == nil || !strings.Contains(err.Error(), "gh pr create failed") {
			t.Errorf("%s: %v, want the failure", bad, err)
		}
		g.wantPending()
	}
}

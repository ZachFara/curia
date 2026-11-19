package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
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
		{name: "branch moved since shown", want: "the branch changed since you looked: " + reviewBranch + " is now at ", seen: strings.Repeat("0", 40)},
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

// TestMergeRefusesConflictBeforeMerging: a merge that would conflict is
// refused from git merge-tree's result before git merge runs, with the
// conflicting paths named; nothing in the checkout changes.
func TestMergeRefusesConflictBeforeMerging(t *testing.T) {
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
			calls := reviewRecordGit(t)
			_, err := f.approve(model.ApproveMerge, "")
			if err == nil || !strings.Contains(err.Error(), "merging "+reviewBranch+" into main would conflict in README") ||
				!strings.HasSuffix(err.Error(), "Nothing was merged") {
				t.Fatalf("approve error = %v, want the conflict refused before merging", err)
			}
			if m := reviewGitCalls(calls(), "merge"); len(m) != 0 {
				t.Errorf("git merge ran: %q", m)
			}
			f.wantAsBefore(before, status)
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
			if err == nil || !strings.Contains(err.Error(), "refused by "+hook) || !strings.Contains(err.Error(), "; nothing was merged") {
				t.Fatalf("approve error = %v, want the hook's message and \"; nothing was merged\"", err)
			}
			if f.hasMergeHead() {
				t.Error("MERGE_HEAD is left")
			}
			if !strings.Contains(err.Error(), "git may have left ORIG_HEAD") || !strings.HasSuffix(err.Error(), ": they are harmless and need nothing") {
				t.Errorf("approve error = %v, want the note on git's leftover files", err)
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
	if err == nil || !strings.Contains(err.Error(), "would be overwritten") || !strings.Contains(err.Error(), "; nothing was merged") {
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
	rng := "refs/heads/main...refs/heads/" + reviewBranch
	for _, tc := range []struct {
		tool string
		want []string
	}{
		{"git", []string{"/opt/bin/git", "-C", f.repo, "diff", rng}},
		{"delta", []string{"/opt/bin/git", "-C", f.repo, "-c", "core.pager=delta --paging=always", "diff", rng}},
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
		!strings.Contains(err.Error(), "Nothing was merged") {
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

// reviewPRFailed is the plain failure when no pull request is adopted.
const reviewPRFailed = "gh pr create failed"

// reviewPRCase is one existing pull request TestPRApproveAgainRecordsExistingPR
// offers through the stand-in gh: what it changes in the pull request that
// matches this approval, and the error wanted ("" when it is adopted).
type reviewPRCase struct {
	name string
	edit func(pr map[string]any, f *reviewFixture)
	want string
}

// TestPRApproveAgainRecordsExistingPR: gh pr create fails because the pull
// request exists. It is adopted, and the review recorded approved with its
// URL, only when it is provably this approval's: open, from the task's branch
// of the same repository into its base, at exactly the approved commit, with
// exactly the title and body Curia builds, at a pull-request URL of the
// remote's repository. Different text is refused with a message naming it;
// anything else is the plain failure. Every refusal leaves the review pending.
func TestPRApproveAgainRecordsExistingPR(t *testing.T) {
	generic := reviewPRFailed
	for _, c := range []reviewPRCase{
		{"matching", nil, ""},
		{"other body", func(pr map[string]any, _ *reviewFixture) { pr["body"] = "Merge me, I am the agent's own." }, "already exists with another title or text"},
		{"other title", func(pr map[string]any, _ *reviewFixture) { pr["title"] = "T-1: something else" }, "already exists with another title or text"},
		{"other head", func(pr map[string]any, _ *reviewFixture) { pr["headRefOid"] = strings.Repeat("0", 40) }, generic},
		{"a fork", func(pr map[string]any, _ *reviewFixture) { pr["isCrossRepository"] = true }, generic},
		{"closed", func(pr map[string]any, _ *reviewFixture) { pr["state"] = "CLOSED" }, generic},
		{"merged", func(pr map[string]any, _ *reviewFixture) { pr["state"] = "MERGED" }, generic},
		{"other base", func(pr map[string]any, _ *reviewFixture) { pr["baseRefName"] = "release" }, generic},
		{"other branch", func(pr map[string]any, _ *reviewFixture) { pr["headRefName"] = "curia/T-1-other" }, generic},
		{"another host", func(pr map[string]any, _ *reviewFixture) { pr["url"] = "https://evil.example/o/r/pull/5" }, generic},
		{"another repository", func(pr map[string]any, _ *reviewFixture) { pr["url"] = "https://github.com/x/r/pull/5" }, generic},
		{"not a pull request URL", func(pr map[string]any, _ *reviewFixture) { pr["url"] = "https://github.com/o/r/issues/5" }, generic},
		{"a local remote", func(_ map[string]any, f *reviewFixture) { gitRun(f.t, f.repo, "remote", "set-url", "origin", f.origin) }, generic},
	} {
		t.Run(c.name, func(t *testing.T) { reviewExistingPRCase(t, c) })
	}
}

// wantAdopted checks that the approval adopted the existing pull request.
func (f *reviewFixture) wantAdopted(line string, err error, gh *reviewGHRecorder) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	if got := f.review(); got.Status != model.ReviewApproved || got.PRURL != "https://github.com/o/r/pull/5" || !strings.Contains(line, got.PRURL) {
		f.t.Errorf("R-1 = %+v, line %q; want approved with the existing pull request", got, line)
	}
	if len(gh.calls) != 2 || !slices.Equal(gh.calls[1].args, []string{"pr", "view", reviewBranch, "--json", reviewPRFields}) {
		f.t.Errorf("gh calls = %+v", gh.calls)
	}
}

// reviewExistingPRCase is one case of TestPRApproveAgainRecordsExistingPR.
func reviewExistingPRCase(t *testing.T, c reviewPRCase) {
	f := reviewNew(t)
	gitRun(t, f.repo, "remote", "set-url", "origin", "https://github.com/o/r.git")
	gitRun(t, f.repo, "remote", "set-url", "--push", "origin", f.origin) // pushes still reach the local bare repository
	tk, r := f.get("T-1"), f.review()
	pr := map[string]any{
		"url": "https://github.com/o/r/pull/5", "state": "OPEN", "baseRefName": "main", "headRefName": reviewBranch,
		"headRefOid": f.head(f.wt, "HEAD"), "isCrossRepository": false, "title": task.PRTitle(tk), "body": task.PRBody(tk, r, nil),
	}
	if c.edit != nil {
		c.edit(pr, f)
	}
	view, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	gh := reviewUseGHFunc(t, func(args []string) ([]byte, error) {
		if args[1] == "create" {
			return nil, errors.New("exit status 1: a pull request for branch \"" + reviewBranch + "\" into branch \"main\" already exists")
		}
		return view, nil
	})
	line, err := f.approve(model.ApprovePR, "")
	if c.want == "" {
		f.wantAdopted(line, err, gh)
		return
	}
	if err == nil || !strings.Contains(err.Error(), c.want) {
		t.Fatalf("approve = %q, %v; want an error with %q", line, err, c.want)
	}
	if c.want != reviewPRFailed && !strings.Contains(err.Error(), "https://github.com/o/r/pull/5") {
		t.Errorf("error %q does not name the pull request", err)
	}
	for _, call := range gh.calls {
		if slices.Contains(call.args, "edit") {
			t.Errorf("gh %q ran", call.args)
		}
	}
	f.wantPending()
}

// TestDiffCmdFixedPager: the git and delta tools page through a fixed less
// that stays until the user leaves it; no pager variable of the caller's
// environment reaches them.
func TestDiffCmdFixedPager(t *testing.T) {
	f := reviewNew(t)
	for _, v := range reviewPagerVars {
		t.Setenv(v, "sh -c 'touch /tmp/pwned' F")
	}
	reviewUseDiffTools(t, func(name string) (string, error) { return "/opt/bin/" + name, nil })
	for tool, want := range map[string][]string{
		"git":   {"LESS=-R -X", "GIT_PAGER=less"},
		"delta": {"LESS=-R -X", "DELTA_PAGER=less -R -X"},
	} {
		cmd, err := reviewDiffCmd(f.get("T-1"), tool)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, kv := range cmd.Env {
			name, _, _ := strings.Cut(kv, "=")
			if slices.Contains(reviewPagerVars, name) {
				got = append(got, kv)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: pager variables %q, want exactly %q", tool, got, want)
		}
	}
}

// TestDiffToolStaysOnScreen runs the git tool's command in a pane of a
// private tmux server, for a diff that fits the screen and one that does
// not: either way the pager is still there a moment later (git's own LESS=FRX
// quit at once on the short one), and q leaves it.
func TestDiffToolStaysOnScreen(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	if _, err := exec.LookPath("less"); err != nil {
		t.Skip("less not on PATH")
	}
	for _, size := range []int{1, 400} {
		t.Run(fmt.Sprintf("%d lines", size), func(t *testing.T) { reviewDiffInTmux(t, size) })
	}
}

// reviewDiffInTmux is one case of TestDiffToolStaysOnScreen: a diff of
// lines added lines, in a 100 x 30 pane.
func reviewDiffInTmux(t *testing.T, lines int) {
	f := reviewNew(t)
	f.commitOnBranch("long.txt", strings.Repeat("a line of the diff\n", lines), "long")
	reviewUseDiffTools(t, exec.LookPath) // the real git
	cmd, err := reviewDiffCmd(f.get("T-1"), "git")
	if err != nil {
		t.Fatal(err)
	}
	sock := fmt.Sprintf("curia-test-%d-diff%d", os.Getpid(), lines)
	// The server takes the environment of the client that starts it: the diff
	// command's own, without git's repository variables (lefthook's
	// pre-commit exports GIT_DIR, which would point the pane's git elsewhere).
	tm := func(args ...string) string {
		c := exec.Command("tmux", append([]string{"-L", sock, "-f", os.DevNull}, args...)...)
		c.Env = cmd.Env
		out, _ := c.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() { spawnKillServer(sock) })
	tm("new-session", "-d", "-x", "100", "-y", "30", "sleep", "60")
	tm("set-option", "-g", "remain-on-exit", "on")
	// The pane runs the command as built: its argv, and the variables it sets
	// for itself (tmux's -e), on a terminal.
	args := []string{"respawn-pane", "-k", "-t", "=0:"}
	for _, kv := range cmd.Env {
		if name, _, _ := strings.Cut(kv, "="); slices.Contains(reviewPagerVars, name) || name == "PATH" || name == "HOME" {
			args = append(args, "-e", kv)
		}
	}
	tm(append(append(args, cmd.Path), cmd.Args[1:]...)...)
	time.Sleep(1500 * time.Millisecond)
	if dead := tm("display-message", "-p", "-t", "=0:", "#{pane_dead}"); dead != "0" {
		t.Fatalf("the diff left the screen at once (pane_dead %q): %q", dead, tm("capture-pane", "-p", "-t", "=0:"))
	}
	if screen := tm("capture-pane", "-p", "-t", "=0:"); !strings.Contains(screen, "a line of the diff") {
		t.Errorf("the diff is not on screen:\n%s", screen)
	}
	tm("send-keys", "-t", "=0:", "q")
	for deadline := time.Now().Add(5 * time.Second); tm("display-message", "-p", "-t", "=0:", "#{pane_dead}") != "1"; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("q did not leave the pager")
		}
	}
}

// TestDiffStatKeepsLeadingSpace: the stat's first line keeps the spaces git
// puts before a file name shorter than the others, so its columns line up.
func TestDiffStatKeepsLeadingSpace(t *testing.T) {
	f := reviewNew(t)
	f.commitOnBranch("a_much_longer_file_name.go", "package auth\n", "long name")
	stat, _, _ := reviewDiffStat(context.Background(), f.get("T-1"), 100)
	lines := strings.Split(stat, "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], " ") || strings.Index(lines[0], "|") != strings.Index(lines[1], "|") {
		t.Errorf("stat lines are not aligned:\n%s", stat)
	}
}

// TestReviewObjectIDLen: the length --head must have is asked of git: 40
// for a SHA-1 repository, 64 for a SHA-256 one.
func TestReviewObjectIDLen(t *testing.T) {
	for format, want := range map[string]int{"sha1": 40, "sha256": 64} {
		dir := t.TempDir()
		cmd := exec.Command("git", "init", "-q", "--object-format="+format, dir)
		cmd.Env = gitEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init --object-format=%s: %v %s", format, err, out)
		}
		if got, err := reviewObjectIDLen(context.Background(), dir); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", format, got, err, want)
		}
	}
}

// reviewHookAt installs hook name in f, which touches marker <dir>/in, then
// waits for <dir>/go to appear (or 30 s) and exits with code; it returns the
// marker and the release file.
func reviewHookAt(f *reviewFixture, name string, code int) (in, release string) {
	f.t.Helper()
	dir := f.t.TempDir()
	in, release = filepath.Join(dir, "in"), filepath.Join(dir, "go")
	lifecycleHooks(f.spawnFixture, name, fmt.Sprintf("#!/bin/sh\ntouch '%s'\ni=0\nwhile [ ! -e '%s' ] && [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done\nexit %d\n",
		in, release, code))
	return in, release
}

// reviewStageDuring waits for marker, then stages content in the main
// checkout's notes.txt, as the developer, an editor or a formatter may while
// a hook runs (git holds no index lock then), and touches release, if any.
func (f *reviewFixture) reviewStageDuring(marker, release, content string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(marker); err == nil {
				break
			}
		}
		worktreeWrite(f.t, filepath.Join(f.repo, "notes.txt"), content)
		gitRun(f.t, f.repo, "add", "notes.txt")
		if release != "" {
			worktreeWrite(f.t, release, "")
		}
	}()
	return done
}

// wantLeftAsIs checks that Curia ran neither git reset nor git merge --abort,
// said why, kept the developer's staged notes.txt (index and disk) and left
// the review pending with its intent.
func (f *reviewFixture) wantLeftAsIs(err error, recorded [][]string, content string) {
	f.t.Helper()
	if err == nil || !strings.Contains(err.Error(), "cannot attribute to its own merge") || strings.Contains(err.Error(), "is as it was") ||
		!strings.Contains(err.Error(), "git -C "+f.repo+" status") || !strings.Contains(err.Error(), "The review stays pending") {
		f.t.Errorf("approve error = %v, want the checkout left as it is, and why", err)
	}
	if r := reviewGitCalls(recorded, "reset"); len(r) != 0 {
		f.t.Errorf("git reset ran: %q", r)
	}
	for _, c := range reviewGitCalls(recorded, "merge") {
		if slices.Contains(c, "--abort") {
			f.t.Errorf("git merge --abort ran: %q", c)
		}
	}
	if got := lifecycleRead(f.t, filepath.Join(f.repo, "notes.txt")); got != content {
		f.t.Errorf("notes.txt = %q, want the developer's %q kept", got, content)
	}
	if staged := worktreeGit(f.t, f.repo, "show", ":notes.txt"); staged+"\n" != content {
		f.t.Errorf("staged notes.txt = %q, want the developer's %q", staged, content)
	}
	if r := f.review(); r.Status != model.ReviewPending || r.IntentAt.IsZero() {
		f.t.Errorf("R-1 = %+v, want pending with its intent kept", r)
	}
}

// TestMergeCutShortKeepsWorkStagedDuringHook: the developer stages an edit
// while the approval's pre-merge-commit hook runs, then the limit cuts the
// merge short. The index is no longer only the merge's result, so Curia runs
// no reset: the edit stays staged and on disk, the message says why and what
// to run, and the review stays pending with its intent.
func TestMergeCutShortKeepsWorkStagedDuringHook(t *testing.T) {
	f := reviewNew(t)
	f.commitOnMain("notes.txt", "a\n", "notes")
	in, _ := reviewHookAt(f, "pre-merge-commit", 0)
	const work = "the developer's staged work\n"
	done := f.reviewStageDuring(in, "", work)
	calls := reviewRecordGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, f.head(f.wt, "HEAD"), model.AnsweredCLI, f.now)
	<-done
	f.wantLeftAsIs(err, calls(), work)
	if !strings.Contains(err.Error(), "the index holds changes that are not the merge's") {
		t.Errorf("approve error = %v, want it to say the index holds other changes", err)
	}
}

// TestHookRefusalKeepsWorkStagedDuringHook: the same, with a hook that then
// refuses the merge commit (git leaves MERGE_HEAD): no git merge --abort runs
// over the staged edit; the merge stays under way for the developer to end.
func TestHookRefusalKeepsWorkStagedDuringHook(t *testing.T) {
	f := reviewNew(t)
	f.commitOnMain("notes.txt", "a\n", "notes")
	in, release := reviewHookAt(f, "pre-merge-commit", 1)
	const work = "staged while the hook ran\n"
	done := f.reviewStageDuring(in, release, work)
	calls := reviewRecordGit(t)
	_, err := f.approve(model.ApproveMerge, "")
	<-done
	f.wantLeftAsIs(err, calls(), work)
	if !f.hasMergeHead() {
		t.Error("MERGE_HEAD is gone: the merge was ended over the developer's change")
	}
}

// TestLimitDuringPostMerge: the limit ends while post-merge runs, after the
// merge commit. The merge is in main, so the review is recorded merged at once
// (M-9). With only the merge in the index, git merge --abort clears the state
// git left (MERGE_HEAD); with an edit staged during the hook nothing runs, the
// edit stays and the outcome says why.
func TestLimitDuringPostMerge(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged during the hook %v", staged), func(t *testing.T) { reviewPostMergeCase(t, staged) })
	}
}

// reviewPostMergeCase is one case of TestLimitDuringPostMerge.
func reviewPostMergeCase(t *testing.T, staged bool) {
	f := reviewNew(t)
	f.commitOnMain("notes.txt", "a\n", "notes")
	in, _ := reviewHookAt(f, "post-merge", 0)
	head := f.head(f.wt, "HEAD")
	const work = "staged during post-merge\n"
	done := make(<-chan struct{})
	if staged {
		done = f.reviewStageDuring(in, "", work)
	}
	calls := reviewRecordGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	line, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, head, model.AnsweredCLI, f.now)
	if err != nil || !strings.HasPrefix(line, "R-1 approved: merged "+reviewBranch+" into main ("+head[:7]+"), though git was stopped after the merge commit") {
		t.Fatalf("approve = %q, %v; want it recorded merged at once", line, err)
	}
	if r := f.review(); r.Status != model.ReviewMerged || r.Head != head || f.head(f.repo, "main^2") != head {
		t.Errorf("R-1 = %+v, main^2 %s; want merged at %s", r, f.head(f.repo, "main^2"), head)
	}
	if !staged {
		if f.hasMergeHead() || f.status() != "" {
			t.Errorf("after the abort: MERGE_HEAD %v, status %q; want neither", f.hasMergeHead(), f.status())
		}
		return
	}
	<-done
	if !strings.Contains(line, "cannot attribute to its own merge") || !f.hasMergeHead() {
		t.Errorf("line %q, MERGE_HEAD %v; want the checkout left as it is", line, f.hasMergeHead())
	}
	for _, c := range reviewGitCalls(calls(), "merge") {
		if slices.Contains(c, "--abort") {
			t.Errorf("git merge --abort ran over the staged edit: %q", c)
		}
	}
	if got := worktreeGit(t, f.repo, "show", ":notes.txt"); got+"\n" != work {
		t.Errorf("staged notes.txt = %q, want the developer's", got)
	}
}

// TestMergeCutShortByKilledGit: git alone is killed (SIGKILL) while
// pre-merge-commit runs; with only the merge in the index Curia undoes it
// with git reset --merge and the checkout is as before.
func TestMergeCutShortByKilledGit(t *testing.T) {
	f := reviewNew(t)
	worktreeWrite(t, filepath.Join(f.repo, "mine.txt"), "untracked\n")
	pidFile := filepath.Join(t.TempDir(), "gitpid")
	lifecycleHooks(f.spawnFixture, "pre-merge-commit", "#!/bin/sh\necho $PPID > '"+pidFile+".tmp'\nmv '"+pidFile+".tmp' '"+pidFile+"'\nsleep 2\nexit 0\n")
	before, status := f.head(f.repo, "HEAD"), f.status()
	killed := make(chan bool, 1)
	go func() {
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if b, err := os.ReadFile(filepath.Clean(pidFile)); err == nil {
				pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
				killed <- perr == nil && syscall.Kill(pid, syscall.SIGKILL) == nil
				return
			}
		}
		killed <- false
	}()
	_, err := f.approve(model.ApproveMerge, "")
	if !<-killed {
		t.Fatal("git was not killed during its hook")
	}
	if err == nil || !strings.Contains(err.Error(), "git merge failed (signal: killed)") ||
		!strings.Contains(err.Error(), "Curia undid what it had staged (git reset --merge)") {
		t.Fatalf("approve error = %v, want the killed merge undone", err)
	}
	f.wantAsBefore(before, status)
	f.wantPending()
}

// TestOrphanMergeKeepsIntent: an approval recorded its intent and started
// git, then its curia was killed; git runs on alone, its pre-merge-commit hook
// longer than the 10-minute hold. A second approval after the hold is refused
// before it claims anything, naming the earlier approval, and the intent stays
// exactly as it was; once the orphaned git has made the merge commit, the next
// approval records the review merged.
func TestOrphanMergeKeepsIntent(t *testing.T) {
	f := reviewNew(t)
	in, release := reviewHookAt(f, "pre-merge-commit", 0)
	head := f.head(f.wt, "HEAD")
	ctx := context.Background()
	if _, err := f.st.ClaimReviewIntent(ctx, "R-1", store.ReviewIntent{How: model.ApproveMerge, Head: head, DiffStat: " auth.go | 1 +"},
		f.now, reviewIntentHold); err != nil {
		t.Fatal(err)
	}
	orphan := make(chan gitResult, 1)
	go func() {
		orphan <- runGitGroup(ctx, f.repo, "merge", "--no-ff", "--no-edit", "--no-autostash", "-m", "Merge", head)
	}()
	reviewWaitFile(t, in)
	_, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, head, model.AnsweredCLI, f.now.Add(11*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "an earlier approval of R-1 (merge at "+head[:7]+", since ") ||
		!strings.Contains(err.Error(), "may still be running git there") {
		t.Errorf("approve during the orphan's hook: %v, want the refusal naming the earlier approval", err)
	}
	if r := f.review(); !r.IntentAt.Equal(f.now) || r.Head != head || r.ApprovedHow != model.ApproveMerge || r.DiffStat != " auth.go | 1 +" {
		t.Errorf("intent after the refusal = %+v, want the earlier one, untouched", r)
	}
	worktreeWrite(t, release, "")
	if res := <-orphan; res.err != nil {
		t.Fatalf("the orphaned merge: %v %s", res.err, res.stderr)
	}
	line, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, head, model.AnsweredCLI, f.now.Add(12*time.Minute))
	if err != nil || !strings.Contains(line, "is already merged into main; recorded") {
		t.Fatalf("approve after the orphan merged = %q, %v; want it recorded", line, err)
	}
	f.wantMerged(head)
}

// TestRefusalAfterClaimRestoresEarlierIntent: an approval that takes over a
// stale intent and is then refused (here: the merge would conflict) puts the
// earlier intent back exactly, rather than clearing it.
func TestRefusalAfterClaimRestoresEarlierIntent(t *testing.T) {
	f := reviewNew(t)
	head := f.head(f.wt, "HEAD")
	old := f.now.Add(-time.Hour)
	if _, err := f.st.ClaimReviewIntent(context.Background(), "R-1", store.ReviewIntent{How: model.ApproveMerge, Head: head, DiffStat: "earlier"},
		old, reviewIntentHold); err != nil {
		t.Fatal(err)
	}
	f.commitOnMain("auth.go", "package auth // main's own\n", "main side")
	if _, err := f.approve(model.ApproveMerge, head); err == nil || !strings.Contains(err.Error(), "would conflict") {
		t.Fatalf("approve = %v, want the conflict refusal", err)
	}
	if r := f.review(); !r.IntentAt.Equal(old) || r.Head != head || r.DiffStat != "earlier" {
		t.Errorf("intent after the refusal = %+v, want the earlier one back", r)
	}
}

// TestBaseTagDoesNotShadowBranch: a tag named like the base, at the branch's
// head, does not make the branch look merged: every read of the base names
// refs/heads/<base> (M-4), so the approval merges and records the real stat.
func TestBaseTagDoesNotShadowBranch(t *testing.T) {
	f := reviewNew(t)
	head := f.head(f.wt, "HEAD")
	gitRun(t, f.repo, "tag", "main", head)
	stat, _, _ := reviewDiffStat(context.Background(), f.get("T-1"), 100)
	if !strings.Contains(stat, "auth.go") {
		t.Errorf("stat = %q, want the branch's change against the branch main", stat)
	}
	line, err := f.approve(model.ApproveMerge, "")
	if err != nil || !strings.HasPrefix(line, "R-1 approved: merged "+reviewBranch+" into main") {
		t.Fatalf("approve = %q, %v; want the merge", line, err)
	}
	if got := f.head(f.repo, "refs/heads/main^2"); got != head {
		t.Errorf("the branch main's second parent = %s, want %s", got, head)
	}
	if r := f.review(); r.Status != model.ReviewMerged || !strings.Contains(r.DiffStat, "auth.go") {
		t.Errorf("R-1 = %+v, want merged with the real stat", r)
	}
}

// TestMergeIgnoresMergeOptions: the developer's merge configuration cannot
// turn the approval's merge into something else (C-1): with
// branch.main.mergeOptions --no-commit git would stop before committing and
// exit 0, with --squash refuse, with -s ours (or pull.twohead ours) make a
// merge commit without the branch's change. Each approval merges the approved
// head with its content, and records it merged.
func TestMergeIgnoresMergeOptions(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"branch.main.mergeOptions", "--no-commit"},
		{"branch.main.mergeOptions", "--squash"},
		{"branch.main.mergeOptions", "-s ours"},
		{"pull.twohead", "ours"},
		{"merge.ff", "only"},
	} {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			f := reviewNew(t)
			gitRun(t, f.repo, "config", c.key, c.value)
			head := f.head(f.wt, "HEAD")
			line, err := f.approve(model.ApproveMerge, "")
			if err != nil || !strings.HasPrefix(line, "R-1 approved: merged "+reviewBranch+" into main") {
				t.Fatalf("approve = %q, %v; want the merge", line, err)
			}
			f.wantMerged(head)
			if got := worktreeGit(t, f.repo, "show", "main:auth.go"); got != "package auth" {
				t.Errorf("main:auth.go = %q, want the branch's change merged", got)
			}
		})
	}
}

// TestMergeReportedDoneButNotInBase: git merge exits 0 but the base does not
// hold the approved head afterwards (here a post-merge hook resets it). Curia
// records nothing merged, says what it found and what to run, and keeps the
// review pending with its intent.
func TestMergeReportedDoneButNotInBase(t *testing.T) {
	f := reviewNew(t)
	lifecycleHooks(f.spawnFixture, "post-merge", "#!/bin/sh\ngit reset -q --hard HEAD^\n")
	before := f.head(f.repo, "HEAD")
	line, err := f.approve(model.ApproveMerge, "")
	if err == nil || !strings.Contains(err.Error(), "git merge reported success, but main does not contain "+reviewShort(f.head(f.wt, "HEAD"))) ||
		!strings.Contains(err.Error(), "git -C "+f.repo+" log") || !strings.Contains(err.Error(), "The review stays pending") {
		t.Fatalf("approve = %q, %v; want the missing merge reported", line, err)
	}
	if r := f.review(); r.Status != model.ReviewPending || r.IntentAt.IsZero() {
		t.Errorf("R-1 = %+v, want pending with its intent kept", r)
	}
	if got := f.head(f.repo, "HEAD"); got != before {
		t.Errorf("HEAD = %s, want the hook's reset to %s", got, before)
	}
}

// TestOrphanMergeRefusesEveryClaim: while an earlier approval's intent to
// merge stands and its orphaned git still runs (its pre-merge-commit hook
// longer than the hold, the merge's result staged), no other outcome may
// claim the review (C-2): a merge approval, a pull request approval, a drop
// and a change request are each refused with the same explanation, and the
// intent stays exactly as it was. Once the orphan has made the merge commit,
// approving (here with a pull request) records the review merged.
func TestOrphanMergeRefusesEveryClaim(t *testing.T) {
	f := reviewNew(t)
	gh := reviewUseGH(t, "https://github.com/o/r/pull/7\n")
	in, release := reviewHookAt(f, "pre-merge-commit", 0)
	head := f.head(f.wt, "HEAD")
	ctx := context.Background()
	if _, err := f.st.ClaimReviewIntent(ctx, "R-1", store.ReviewIntent{How: model.ApproveMerge, Head: head, DiffStat: " auth.go | 1 +"},
		f.now, reviewIntentHold); err != nil {
		t.Fatal(err)
	}
	orphan := make(chan gitResult, 1)
	go func() {
		orphan <- runGitGroup(ctx, f.repo, "merge", "--no-ff", "--no-edit", "--no-autostash", "-m", "Merge", head)
	}()
	reviewWaitFile(t, in)
	later := f.now.Add(11 * time.Minute)
	for _, c := range []struct {
		name, again string
		act         func() (string, error)
	}{
		{"merge approval", "approve again", func() (string, error) {
			return reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, head, model.AnsweredCLI, later)
		}},
		{"pull request approval", "approve again", func() (string, error) {
			return reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApprovePR, head, model.AnsweredCLI, later)
		}},
		{"drop", "drop again", func() (string, error) { return reviewDrop(ctx, f.st, "R-1", model.AnsweredCLI, later) }},
		{"change request", "request changes again", func() (string, error) {
			return reviewRequestChanges(ctx, f.st, f.fake, testLoggerOnly(), "R-1", "more", model.AnsweredCLI, later)
		}},
	} {
		line, err := c.act()
		f.wantOrphanRefusal(c.name, c.again, head, line, err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("gh ran: %+v", gh.calls)
	}
	worktreeWrite(t, release, "")
	if res := <-orphan; res.err != nil {
		t.Fatalf("the orphaned merge: %v %s", res.err, res.stderr)
	}
	line, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApprovePR, head, model.AnsweredCLI, later.Add(time.Minute))
	if err != nil || !strings.Contains(line, "is already merged into main; recorded") {
		t.Fatalf("approve after the orphan merged = %q, %v; want it recorded", line, err)
	}
	f.wantMerged(head)
}

// reviewOddKinds are working-tree states git hash-object cannot read (I-1),
// each set up in the main checkout after a commit that tracks the path: a
// tracked file deleted, a tracked symlink retargeted to nowhere, and an
// embedded repository (a gitlink) with a commit its gitlink does not record.
var reviewOddKinds = []struct {
	name  string
	setup func(f *reviewFixture)
}{
	{"deleted file", func(f *reviewFixture) {
		f.commitOnMain("notes.txt", "a\n", "notes")
		if err := os.Remove(filepath.Join(f.repo, "notes.txt")); err != nil {
			f.t.Fatal(err)
		}
	}},
	{"dangling symlink", func(f *reviewFixture) {
		lnk := filepath.Join(f.repo, "lnk")
		if err := os.Symlink("README", lnk); err != nil {
			f.t.Fatal(err)
		}
		gitRun(f.t, f.repo, "add", "lnk")
		gitRun(f.t, f.repo, "commit", "-q", "-m", "link")
		if err := os.Remove(lnk); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Symlink("nowhere", lnk); err != nil {
			f.t.Fatal(err)
		}
	}},
	{"submodule with a new commit", func(f *reviewFixture) {
		sub := filepath.Join(f.repo, "sub")
		gitInit(f.t, sub, "main")
		for i, file := range []string{"s", "t"} {
			worktreeWrite(f.t, filepath.Join(sub, file), file+"\n")
			gitRun(f.t, sub, "add", file)
			gitRun(f.t, sub, "commit", "-q", "-m", file)
			if i == 0 {
				gitRun(f.t, f.repo, "add", "sub")
				gitRun(f.t, f.repo, "commit", "-q", "-m", "submodule")
			}
		}
	}},
}

// reviewManyModified commits reviewModifiedMax+1 files on main and modifies
// each in the main checkout.
func (f *reviewFixture) reviewManyModified() {
	f.t.Helper()
	dir := filepath.Join(f.repo, "many")
	for i := range reviewModifiedMax + 1 {
		worktreeWrite(f.t, filepath.Join(dir, strconv.Itoa(i)), "a\n")
	}
	gitRun(f.t, f.repo, "add", "-A")
	gitRun(f.t, f.repo, "commit", "-q", "-m", "many")
	for i := range reviewModifiedMax + 1 {
		worktreeWrite(f.t, filepath.Join(dir, strconv.Itoa(i)), "b\n")
	}
}

// TestMergeWithOddWorkingTree: a working tree with a deleted tracked file, a
// dangling symlink, a submodule with new commits, or more modified files than
// Curia compares, merges (I-1): none of them is an error, and nothing raw from
// git reaches the user.
func TestMergeWithOddWorkingTree(t *testing.T) {
	kinds := append(slices.Clone(reviewOddKinds), struct {
		name  string
		setup func(f *reviewFixture)
	}{"too many modified files", (*reviewFixture).reviewManyModified})
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			f := reviewNew(t)
			k.setup(f)
			head := f.head(f.wt, "HEAD")
			line, err := f.approve(model.ApproveMerge, "")
			if err != nil || !strings.HasPrefix(line, "R-1 approved: merged "+reviewBranch+" into main") {
				t.Fatalf("approve = %q, %v; want the merge", line, err)
			}
			if r := f.review(); r.Status != model.ReviewMerged || f.head(f.repo, "main^2") != head {
				t.Errorf("R-1 = %+v, main^2 %s; want merged at %s", r, f.head(f.repo, "main^2"), head)
			}
		})
	}
}

// TestMergeCutShortWithOddWorkingTree: with all three odd kinds in the working
// tree, a merge cut short in pre-merge-commit is proven Curia's own and undone:
// each path is compared by its kind (deleted, symlink target, gitlink commit),
// and the checkout is as before.
func TestMergeCutShortWithOddWorkingTree(t *testing.T) {
	f := reviewNew(t)
	for _, k := range reviewOddKinds {
		k.setup(f)
	}
	lifecycleHooks(f.spawnFixture, "pre-merge-commit", "#!/bin/sh\nsleep 4\nexit 0\n")
	before, status := f.head(f.repo, "HEAD"), f.status()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, f.head(f.wt, "HEAD"), model.AnsweredCLI, f.now)
	if err == nil || !strings.Contains(err.Error(), "Curia undid what it had staged (git reset --merge)") {
		t.Fatalf("approve error = %v, want the cut-short merge undone", err)
	}
	f.wantAsBefore(before, status)
	if target, err := os.Readlink(filepath.Join(f.repo, "lnk")); err != nil || target != "nowhere" {
		t.Errorf("lnk -> %q (%v), want the developer's nowhere kept", target, err)
	}
	f.wantPending()
}

// TestMergeCutShortWithTooManyModified: with more modified files than Curia
// compares, the merge runs, and when it is cut short Curia undoes nothing and
// says plainly why: too many modified files to prove an undo.
func TestMergeCutShortWithTooManyModified(t *testing.T) {
	f := reviewNew(t)
	f.reviewManyModified()
	lifecycleHooks(f.spawnFixture, "pre-merge-commit", "#!/bin/sh\nsleep 4\nexit 0\n")
	calls := reviewRecordGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, f.head(f.wt, "HEAD"), model.AnsweredCLI, f.now)
	if err == nil || !strings.Contains(err.Error(), "has more than 500 modified files, too many for Curia to prove that undoing the merge "+
		"touches nothing else") || !strings.Contains(err.Error(), "The review stays pending") {
		t.Fatalf("approve error = %v, want the undo refused, saying why", err)
	}
	if r := reviewGitCalls(calls(), "reset"); len(r) != 0 {
		t.Errorf("git reset ran: %q", r)
	}
	if r := f.review(); r.Status != model.ReviewPending || r.IntentAt.IsZero() {
		t.Errorf("R-1 = %+v, want pending with its intent kept", r)
	}
}

// wantOrphanRefusal checks that action name was refused, naming the earlier
// approval of head (intent at f.now) and saying to act again (again), and
// that the earlier intent is untouched.
func (f *reviewFixture) wantOrphanRefusal(name, again, head, line string, err error) {
	f.t.Helper()
	if err == nil || !strings.Contains(err.Error(), "an earlier approval of R-1 (merge at "+head[:7]+", since ") ||
		!strings.Contains(err.Error(), "may still be running git there") || !strings.Contains(err.Error(), "then "+again) {
		f.t.Errorf("%s during the orphan's hook = %q, %v; want the refusal naming the earlier approval", name, line, err)
	}
	if r := f.review(); r.Status != model.ReviewPending || !r.IntentAt.Equal(f.now) || r.Head != head ||
		r.ApprovedHow != model.ApproveMerge || r.DiffStat != " auth.go | 1 +" {
		f.t.Errorf("after the %s: R-1 = %+v, want the earlier intent, untouched", name, r)
	}
}

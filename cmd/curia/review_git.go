package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// Limits of the gh calls and the push of an approval (plan section 4.6).
const (
	reviewPushLimit = 2 * time.Minute
	reviewGHLimit   = time.Minute
)

var (
	// reviewRemotePattern is the only form of remote name pushed to.
	reviewRemotePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// reviewURLPattern is the only form of pull-request URL kept from gh.
	reviewURLPattern = regexp.MustCompile(`^https://[A-Za-z0-9._/-]{1,200}$`)
)

// reviewGH runs gh in dir, in a process group of its own (gitops.RunProcessGroup), and
// returns its stdout (at most 64 KiB). Tests replace it.
var reviewGH = func(ctx context.Context, dir string, args ...string) (stdout []byte, err error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, err
	}
	out, errOut, err := gitops.RunProcessGroup(ctx, dir, append(gitops.Env(), "GH_PROMPT_DISABLED=1"), path, args...)
	if err != nil {
		first, _, _ := strings.Cut(errOut, "\n")
		if first = sanitize.Line(first, gitops.ErrMax); first != "" {
			return []byte(out), fmt.Errorf("%w: %s", err, first)
		}
		return []byte(out), err
	}
	return []byte(out), nil
}

// reviewGHLookPath finds gh before anything is pushed. Tests replace it.
var reviewGHLookPath = exec.LookPath

// reviewDiffLookPath finds a diff tool. Tests replace it.
var reviewDiffLookPath = exec.LookPath

// reviewDiffStat is section 4.5's diff stat for t, cleaned and capped, and the warning line that goes with it ("" when none).
// It also returns the full commit id of the branch the stat was taken at ("" when there is none), which the
// view shows and passes back on approval.
func reviewDiffStat(ctx context.Context, t model.Task, width int) (stat, note, head string) {
	if t.Branch == "" {
		return "", "the task has no branch (never spawned, or cleaned)", ""
	}
	path, onDisk, err := lifecycleWorktree(ctx, t)
	if err == nil {
		head, err = gitops.BranchHead(ctx, t)
	}
	if err != nil {
		return "", sanitize.Line(err.Error(), gitops.ErrMax), ""
	}
	if head == "" {
		return "", "branch " + t.Branch + " does not exist", ""
	}
	if stat, err = gitops.StatAt(ctx, t, head, width); err != nil {
		return "", sanitize.Line(err.Error(), gitops.ErrMax), head
	}
	return stat, reviewDiffNote(ctx, t, head, path, onDisk), head
}

// reviewDiffNote is the warning under the stat of t's branch at head: the
// worktree's uncommitted changes, else no commits beyond the base.
func reviewDiffNote(ctx context.Context, t model.Task, head, path string, onDisk bool) string {
	if onDisk {
		dirty, err := gitops.WorktreeDirty(ctx, path)
		switch {
		case err != nil:
			return sanitize.Line("cannot read the worktree: "+err.Error(), gitops.ErrMax)
		case dirty:
			return "the worktree has uncommitted changes that are not in this diff"
		}
	}
	n, err := gitops.Ahead(ctx, t, head)
	switch {
	case err != nil:
		return sanitize.Line(err.Error(), gitops.ErrMax)
	case n == 0:
		return "the branch has no commits beyond " + t.BaseBranch
	}
	return ""
}

// reviewCheck runs steps 1 to 5 of section 4.6's approve order and returns the diff stat and the branch's commit.
// The stat is taken at that commit, so it is the record of what was approved.
func reviewCheck(ctx context.Context, st *store.Store, t model.Task) (stat, head string, err error) {
	if t.Branch == "" {
		return "", "", fmt.Errorf("%s has no branch: nothing to approve (never spawned, or cleaned)", t.ID)
	}
	path, onDisk, err := lifecycleWorktree(ctx, t)
	if err != nil {
		return "", "", err
	}
	if head, err = gitops.BranchHead(ctx, t); err != nil {
		return "", "", err
	}
	if head == "" {
		return "", "", fmt.Errorf("branch %s does not exist in %s", t.Branch, sanitize.Line(t.Repo, 300))
	}
	ok, err := gitops.RefExists(ctx, t.Repo, "refs/heads/"+t.BaseBranch+"^{commit}")
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", fmt.Errorf("base %s does not resolve to a commit in %s", t.BaseBranch, sanitize.Line(t.Repo, 300))
	}
	if err = reviewCheckAgents(ctx, st, t); err != nil {
		return "", "", err
	}
	if err = reviewCheckClean(ctx, path, onDisk); err != nil {
		return "", "", err
	}
	n, err := gitops.Ahead(ctx, t, head)
	if err != nil {
		return "", "", err
	}
	if n == 0 {
		return "", "", fmt.Errorf("nothing to merge: %s has no commits beyond %s", t.Branch, t.BaseBranch)
	}
	if stat, err = gitops.StatAt(ctx, t, head, gitops.StatWidth); err != nil {
		return "", "", err
	}
	return stat, head, nil
}

// reviewCheckAgents is step 2: no live linked session is working or waiting.
func reviewCheckAgents(ctx context.Context, st *store.Store, t model.Task) error {
	live, err := st.TaskSessions(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, s := range live {
		if s.State == model.StateWorking || s.State == model.StateWaiting {
			return errors.New("the agent is still working; wait for it to stop")
		}
	}
	return nil
}

// reviewCheckClean is step 3: the worktree, when it is on disk, is clean.
func reviewCheckClean(ctx context.Context, path string, onDisk bool) error {
	if !onDisk {
		return nil
	}
	dirty, err := gitops.WorktreeDirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("uncommitted changes in %s: ask the agent to commit them, or commit them yourself", sanitize.Line(path, 300))
	}
	return nil
}

// reviewOpenPR is step 7: push, then gh pr create; url is "" when gh printed none.
// It pushes exactly commit head, the one approved, to the task's branch on remote (never forced, never the
// base) and refuses when the branch has moved since, before the push and again before gh runs. The task's
// title and the agent's summary reach gh only as the values of --title and --body (task.PRTitle, task.PRBody).
func reviewOpenPR(ctx context.Context, t model.Task, r model.Review, decided []model.Decision, remote, head string) (url string, err error) {
	if !gitops.HeadPattern.MatchString(head) {
		return "", fmt.Errorf("refusing commit %q", sanitize.Line(head, 80))
	}
	if !reviewRemotePattern.MatchString(remote) || strings.HasPrefix(remote, "-") {
		return "", fmt.Errorf("refusing remote %q", sanitize.Line(remote, 80))
	}
	if err = gitops.CheckNames(t); err != nil {
		return "", err
	}
	if _, err = reviewGHLookPath("gh"); err != nil {
		return "", errors.New("gh is not on PATH: install the GitHub CLI and log in, or approve with --merge. Nothing was pushed")
	}
	if err = reviewSameHead(ctx, t, head); err != nil {
		return "", err
	}
	if err = gitops.Change(ctx, reviewPushLimit, t.Repo, "push", remote, head+":refs/heads/"+t.Branch); err != nil {
		return "", fmt.Errorf("%w; %s was not pushed and no pull request was opened", err, t.Branch)
	}
	pushed := fmt.Sprintf("%s is pushed to %s at %s", t.Branch, remote, gitops.Short(head))
	if err = reviewSameHead(ctx, t, head); err != nil {
		return "", fmt.Errorf("%s, but %w", pushed, err)
	}
	title, body := task.PRTitle(t), task.PRBody(t, r, decided)
	gctx, cancel := context.WithTimeout(ctx, reviewGHLimit)
	defer cancel()
	out, err := reviewGH(gctx, t.Repo, "pr", "create", "--base", t.BaseBranch, "--head", t.Branch, "--title", title, "--body", body)
	if err == nil {
		return reviewPRURL(out), nil
	}
	// The pull request may exist already, opened by an earlier approve whose gh failed late: it is adopted
	// only when it is provably this approval's (reviewExistingPR).
	url, other := reviewExistingPR(context.WithoutCancel(ctx), t, remote, head, title, body)
	switch {
	case other != nil:
		return "", fmt.Errorf("%s, but %w", pushed, other)
	case url != "":
		return url, nil
	}
	return "", fmt.Errorf("%s, but gh pr create failed: %s. The review stays pending: approve again to retry",
		pushed, sanitize.Line(err.Error(), gitops.ErrMax))
}

// reviewPRView is what reviewExistingPR reads of gh pr view's JSON. It is untrusted: every field is
// compared with what this approval expects, or validated, before use.
type reviewPRView struct {
	URL               string `json:"url"`
	State             string `json:"state"`
	BaseRefName       string `json:"baseRefName"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	Title             string `json:"title"`
	Body              string `json:"body"`
}

// reviewPRFields are the fields reviewExistingPR asks gh pr view for.
const reviewPRFields = "url,state,baseRefName,headRefName,headRefOid,isCrossRepository,title,body"

// reviewExistingPR looks for the pull request of t's branch when gh pr create failed, and returns its URL
// when it is provably the one this approval would have opened: open, from t's branch of the same repository
// (not a fork) into t's base, at exactly commit head, with exactly title and body, at the URL of a pull
// request of the repository remote names. One that matches in all but its title or text was not opened by
// Curia for this review: other says so and names its URL, and the review stays pending; Curia never edits
// it. Anything else, a gh that fails included, is "" and nil.
func reviewExistingPR(ctx context.Context, t model.Task, remote, head, title, body string) (url string, other error) {
	pr, ok := reviewViewPR(ctx, t)
	if !ok || pr.State != "OPEN" || pr.IsCrossRepository || pr.BaseRefName != t.BaseBranch ||
		pr.HeadRefName != t.Branch || pr.HeadRefOid != head || !reviewRepoPRURL(ctx, t, remote, pr.URL) {
		return "", nil
	}
	if pr.Title != title || pr.Body != body {
		return "", fmt.Errorf("a pull request for %s already exists with another title or text than Curia gives this review: %s. "+
			"Curia did not open it, so it is neither recorded nor changed; the review stays pending. Look at it, then close it "+
			"and approve again, or approve with --merge", t.Branch, pr.URL)
	}
	return pr.URL, nil
}

// reviewViewPR is gh pr view's JSON for t's branch (argv, bounded by reviewGHLimit; gh's output is capped),
// and whether gh gave one.
func reviewViewPR(ctx context.Context, t model.Task) (reviewPRView, bool) {
	gctx, cancel := context.WithTimeout(ctx, reviewGHLimit)
	defer cancel()
	out, err := reviewGH(gctx, t.Repo, "pr", "view", t.Branch, "--json", reviewPRFields)
	var pr reviewPRView
	if err != nil || json.Unmarshal(out, &pr) != nil {
		return reviewPRView{}, false
	}
	return pr, true
}

// reviewRemoteURL is the form of a remote's URL whose host and repository path a pull request's URL must
// share: https://host/owner/repo, git@host:owner/repo or ssh://git@host/owner/repo, with or without .git.
var reviewRemoteURL = regexp.MustCompile(`^(?:https://|ssh://git@|git@)([A-Za-z0-9.-]{1,100})[/:]([A-Za-z0-9._-]{1,100})/([A-Za-z0-9._-]{1,100}?)(?:\.git)?/?$`)

// reviewRepoPRURL reports whether url is the URL of a pull request of the repository t's remote names:
// https://<its host>/<owner>/<repo>/pull/<number>. A remote whose URL has no such form (a local path) has
// no pull request URL that can be checked, so none is accepted.
func reviewRepoPRURL(ctx context.Context, t model.Task, remote, url string) bool {
	got, err := gitops.Output(ctx, gitops.GitLimit, t.Repo, "remote", "get-url", "--", remote)
	if err != nil {
		return false
	}
	m := reviewRemoteURL.FindStringSubmatch(got)
	if m == nil {
		return false
	}
	want := regexp.MustCompile(`^https://` + regexp.QuoteMeta(m[1]) + `/` + regexp.QuoteMeta(m[2]) + `/` +
		regexp.QuoteMeta(m[3]) + `/pull/[1-9][0-9]{0,9}$`)
	return want.MatchString(url)
}

// reviewSameHead refuses when t's branch is no longer at head.
func reviewSameHead(ctx context.Context, t model.Task, head string) error {
	now, err := gitops.BranchHead(ctx, t)
	if err != nil {
		return err
	}
	if now != head {
		return gitops.ErrBranchChanged
	}
	return nil
}

// reviewPRURL is the last line gh printed when it is a plain https URL,
// else "".
func reviewPRURL(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if reviewURLPattern.MatchString(last) {
		return last
	}
	return ""
}

// reviewPagerVars are the variables a pager may come from, which the diff
// command of the git and delta tools does not inherit: it sets its own.
var reviewPagerVars = []string{"GIT_PAGER", "PAGER", "LESS", "DELTA_PAGER", "BAT_PAGER", "LV"}

// reviewPagerEnv is the fixed pager of the git and delta diff tools: less, without -F (git's default LESS is
// FRX, whose -F quits at once when the diff fits one screen, so the view came back before the diff was seen
// and the next q, meant for the pager, closed the view), with -R (git's and delta's colors) and -X (no screen
// clearing on exit, so the last page stays readable). Constants only: nothing here comes from the config or
// the caller's environment. GIT_PAGER names the pager for the git tool; the delta tool's pager is delta,
// named by -c core.pager, which pages through DELTA_PAGER.
const (
	reviewLess       = "-R -X"
	reviewGitPager   = "less"
	reviewDeltaPager = "less -R -X"
)

// reviewDiffEnv is gitops.Env without reviewPagerVars, plus the fixed pager of
// tool (reviewPagerEnv).
func reviewDiffEnv(tool string) []string {
	env := slices.DeleteFunc(gitops.Env(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(reviewPagerVars, name)
	})
	env = append(env, "LESS="+reviewLess)
	switch tool {
	case "git":
		env = append(env, "GIT_PAGER="+reviewGitPager)
	case "delta":
		env = append(env, "DELTA_PAGER="+reviewDeltaPager)
	}
	return env
}

// reviewDiffCmd is the command of section 4.6's diff table for tool.
// It is not bound to a context: it runs as long as the user keeps the tool open; the git and delta tools
// page through a less that stays until the user leaves it, whatever the diff's length (reviewDiffEnv).
func reviewDiffCmd(t model.Task, tool string) (*exec.Cmd, error) {
	if err := gitops.CheckNames(t); err != nil {
		return nil, err
	}
	rng := "refs/heads/" + t.BaseBranch + "...refs/heads/" + t.Branch
	var bin string
	var args, needs []string
	switch tool {
	case "git":
		bin, args = "git", []string{"-C", t.Repo, "diff", rng}
	case "delta":
		bin, args, needs = "git", []string{"-C", t.Repo, "-c", "core.pager=delta --paging=always", "diff", rng}, []string{"delta"}
	case "lazygit":
		bin, args = "lazygit", []string{"-p", reviewDiffDir(t)}
	case "nvim":
		bin, args, needs = "git", []string{"-C", t.Repo, "difftool", "--no-prompt", "--tool=nvimdiff", rng}, []string{"nvim"}
	default:
		return nil, fmt.Errorf("unknown diff tool %q: review.diff_tool is git, delta, lazygit or nvim", sanitize.Line(tool, 40))
	}
	for _, n := range needs {
		if _, err := reviewDiffLookPath(n); err != nil {
			return nil, fmt.Errorf("the diff tool %s is not on PATH: install it, or set review.diff_tool", n)
		}
	}
	path, err := reviewDiffLookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("the diff tool %s is not on PATH: install it, or set review.diff_tool", bin)
	}
	cmd := exec.Command(path, args...)
	cmd.Env = reviewDiffEnv(tool)
	return cmd, nil
}

// reviewDiffDir is where lazygit opens: the task's worktree when it is on
// disk as a directory, else the main repository.
func reviewDiffDir(t model.Task) string {
	path, err := spawnWorktreePath(t.Repo, t.ID)
	if err != nil {
		return t.Repo
	}
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		return path
	}
	return t.Repo
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// taskUnexpectedEditor is taskEditor under test: no test may start a real
// editor, so a test that reaches the editor without installing its own
// stub fails.
func taskUnexpectedEditor(_ context.Context, argv []string, path string) error {
	panic("taskEditor called without a stub: " + strings.Join(append(argv, path), " "))
}

func init() {
	taskEditor = taskUnexpectedEditor
}

// taskNow is the fixed clock of the task tests.
const taskNow = "2026-11-13T12:00:00Z"

// taskHome isolates HOME and CURIA_DB, fixes the clock at taskNow and resets
// the task flags, stdin and taskEditor after the test. It returns the
// database path.
func taskHome(t *testing.T) string {
	t.Helper()
	db := hookHome(t)
	prevClock := clock
	now := decisionsParse(t, taskNow)
	clock = func() time.Time { return now }
	t.Cleanup(func() {
		clock = prevClock
		taskEditor = taskUnexpectedEditor
		taskResetFlags()
		rootCmd.SetIn(nil)
	})
	return db
}

// taskFlagNames are the flags of `curia task`'s subcommands.
var taskFlagNames = []string{"repo", "body", "base", "tag", "status", "json", "title", "backlog-md"}

// taskResetFlags restores every flag of `curia task`'s subcommands to its
// default and clears its Changed mark, which cobra keeps between runs. A
// repeatable flag is emptied with Replace, since its Set appends once it
// has been set.
func taskResetFlags() {
	for _, c := range append([]*cobra.Command{taskCmd}, taskCmd.Commands()...) {
		for _, name := range taskFlagNames {
			f := c.Flags().Lookup(name)
			if f == nil {
				continue
			}
			if r, ok := f.Value.(interface{ Replace([]string) error }); ok {
				_ = r.Replace([]string{})
			} else {
				_ = f.Value.Set(f.DefValue)
			}
			f.Changed = false
		}
	}
}

// taskSetEditor writes a config file under the test's HOME whose editor
// option is value.
func taskSetEditor(t *testing.T, value string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := configFile(home)
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(value) // a TOML basic string for these values
	if err = os.WriteFile(path, []byte("editor = "+string(quoted)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// taskRepo makes a repository with one commit on branch under a hermetic
// git and returns its top level as git prints it (symlinks resolved).
func taskRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := hermeticGit(t)
	gitInit(t, dir, branch)
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	repo, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// taskRun runs `curia <args>` in-process with stdin (nil: none) and returns
// stdout and stderr together.
func taskRun(t *testing.T, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	rootCmd.SetIn(stdin)
	defer taskResetFlags()
	return runRoot(t, args...)
}

// taskMust runs `curia <args>` and fails the test on an error.
func taskMust(t *testing.T, args ...string) string {
	t.Helper()
	out, err := taskRun(t, nil, args...)
	if err != nil {
		t.Fatalf("curia %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func taskOpen(t *testing.T, db string) *store.Store {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func taskGet(t *testing.T, db, id string) model.Task {
	t.Helper()
	tk, found, err := taskOpen(t, db).GetTask(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetTask(%s) = %v, found %v", id, err, found)
	}
	return tk
}

func taskCount(t *testing.T, db string) int {
	t.Helper()
	ts, err := taskOpen(t, db).ListTasks(context.Background(), store.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(ts)
}

// taskPutSession stores a session linked to task taskID.
func taskPutSession(t *testing.T, db, id, taskID, pane string, state model.State) {
	t.Helper()
	now := decisionsParse(t, taskNow)
	s := model.Session{ID: id, Agent: "claude", Pane: pane, State: state, TaskID: taskID, StartedAt: now, UpdatedAt: now}
	if err := taskOpen(t, db).UpsertSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// taskWriteFile writes content to dir/name (mode 0600) and returns the path.
func taskWriteFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// taskLsRow is the part of a `curia task ls --json` row the tests read.
type taskLsRow struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Repo         string   `json:"repo"`
	Status       string   `json:"status"`
	StoredStatus string   `json:"stored_status"`
	Sessions     []string `json:"sessions"`
}

func taskLs(t *testing.T, args ...string) []taskLsRow {
	t.Helper()
	out := taskMust(t, append([]string{"task", "ls", "--json"}, args...)...)
	var rows []taskLsRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return rows
}

func taskLsIDs(t *testing.T, args ...string) []string {
	t.Helper()
	ids := []string{}
	for _, r := range taskLs(t, args...) {
		ids = append(ids, r.ID)
	}
	return ids
}

func taskWantErr(t *testing.T, err error, out string, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error; output:\n%s", out)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not mention %q", err, p)
		}
	}
}

func TestTaskAddStoresMainRepoAndBase(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "trunk")
	sub := filepath.Join(repo, "pkg", "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	out := taskMust(t, "task", "add", "Fix the auth refresh", "--repo", sub, "--tag", "Auth", "--tag", "api")
	if out != "T-1 added (backlog)\n" {
		t.Errorf("output = %q, want %q", out, "T-1 added (backlog)\n")
	}
	got := taskGet(t, db, "T-1")
	if got.Repo != repo || got.BaseBranch != "trunk" || got.Status != model.TaskBacklog ||
		got.Source != model.SourceLocal || got.Title != "Fix the auth refresh" || !slices.Equal(got.Tags, []string{"api", "auth"}) {
		t.Errorf("stored %+v, want repo %s, base trunk, backlog, local, tags [api auth]", got, repo)
	}
	if !got.CreatedAt.Equal(decisionsParse(t, taskNow)) {
		t.Errorf("created_at = %v, want %s", got.CreatedAt, taskNow)
	}

	t.Chdir(sub) // --repo defaults to "."
	if out := taskMust(t, "task", "add", "Second"); out != "T-2 added (backlog)\n" {
		t.Errorf("output = %q", out)
	}
	if got := taskGet(t, db, "T-2"); got.Repo != repo || got.BaseBranch != "trunk" {
		t.Errorf("T-2 repo %q base %q, want %q trunk", got.Repo, got.BaseBranch, repo)
	}
}

func TestTaskAddRefusesLinkedWorktreeAndNonRepo(t *testing.T) {
	db := taskHome(t)
	_, wt := hookLinkedWorktree(t, "feat/wt")
	out, err := taskRun(t, nil, "task", "add", "In a worktree", "--repo", wt)
	taskWantErr(t, err, out, "repo", "linked worktree")
	out, err = taskRun(t, nil, "task", "add", "Nowhere", "--repo", hermeticGit(t))
	taskWantErr(t, err, out, "repo", "not inside a git repository")
	out, err = taskRun(t, nil, "task", "add", "Missing", "--repo", filepath.Join(t.TempDir(), "missing"))
	taskWantErr(t, err, out, "repo")
	if n := taskCount(t, db); n != 0 {
		t.Errorf("%d tasks stored, want none", n)
	}
}

func TestTaskAddDetachedHeadNeedsBase(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	gitRun(t, repo, "checkout", "-q", "--detach")
	out, err := taskRun(t, nil, "task", "add", "Detached", "--repo", repo)
	taskWantErr(t, err, out, "base_branch", "--base")
	if n := taskCount(t, db); n != 0 {
		t.Fatalf("%d tasks stored, want none", n)
	}
	taskMust(t, "task", "add", "Detached", "--repo", repo, "--base", "release/1.2")
	if got := taskGet(t, db, "T-1"); got.BaseBranch != "release/1.2" {
		t.Errorf("base = %q, want release/1.2", got.BaseBranch)
	}
}

func TestTaskAddBodyFromFileAndStdin(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	dir := t.TempDir()
	layout := "Steps:\n\n\t1. keep the tab\n    2. and the indent\n\nDone."
	file := taskWriteFile(t, dir, "body.md", layout+"\n")
	taskMust(t, "task", "add", "From a file", "--repo", repo, "--body", file)
	if got := taskGet(t, db, "T-1").Body; got != layout {
		t.Errorf("body from file = %q, want %q", got, layout)
	}
	out, err := taskRun(t, strings.NewReader("- a\n  - b\n"), "task", "add", "From stdin", "--repo", repo, "--body", "-")
	if err != nil {
		t.Fatalf("--body -: %v\n%s", err, out)
	}
	if got := taskGet(t, db, "T-2").Body; got != "- a\n  - b" {
		t.Errorf("body from stdin = %q", got)
	}

	big := strings.Repeat("x", 49<<10)
	fifo := filepath.Join(dir, "fifo")
	if err = syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"49 KiB file": taskWriteFile(t, dir, "big.md", big),
		"directory":   dir,
		"FIFO":        fifo,
		"missing":     filepath.Join(dir, "missing.md"),
	} {
		bout, berr := taskRun(t, nil, "task", "add", "Bad body", "--repo", repo, "--body", body)
		if berr == nil || !strings.Contains(berr.Error(), "body") {
			t.Errorf("%s: error %v, want one naming body\n%s", name, berr, bout)
		}
	}
	out, err = taskRun(t, strings.NewReader(big), "task", "add", "Bad body", "--repo", repo, "--body", "-")
	taskWantErr(t, err, out, "body")
	if n := taskCount(t, db); n != 2 {
		t.Errorf("%d tasks stored, want 2", n)
	}
}

func TestTaskAddReportsEveryProblem(t *testing.T) {
	db := taskHome(t)
	out, err := taskRun(t, nil, "task", "add", "", "--repo", hermeticGit(t), "--base", "-bad", "--tag", "Bad Tag!")
	taskWantErr(t, err, out, "title: is required", "repo:", "base_branch:", "tags:")
	repo := taskRepo(t, "main")
	out, err = taskRun(t, nil, "task", "add", strings.Repeat("t", 121), "--repo", repo, "--tag", "ok", "--tag", "-no")
	taskWantErr(t, err, out, "title:", "tags:")
	if n := taskCount(t, db); n != 0 {
		t.Errorf("%d tasks stored, want none", n)
	}
}

// taskTimeRe matches a JSON string that may hold a time.
var taskTimeRe = regexp.MustCompile(`"\d{4}-\d\d-\d\dT[^"]*"`)

// taskNormalise is e2e's normalise for `curia task ls --json`: every RFC
// 3339 time becomes "<time>" and the repository path, at the start of a
// string, becomes <root>.
func taskNormalise(out, repo string) string {
	out = taskTimeRe.ReplaceAllStringFunc(out, func(s string) string {
		if _, err := time.Parse(time.RFC3339Nano, s[1:len(s)-1]); err != nil {
			return s
		}
		return `"<time>"`
	})
	quoted, _ := json.Marshal(repo)
	return strings.ReplaceAll(out, strings.TrimSuffix(string(quoted), `"`), `"<root>`)
}

func TestTaskLsJSONGolden(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	body := taskWriteFile(t, t.TempDir(), "body.md", "Rotate the token\n\tbefore expiry.\n")
	taskMust(t, "task", "add", "Fix the auth refresh", "--repo", repo, "--body", body, "--tag", "auth")
	taskMust(t, "task", "add", "Add rate limits", "--repo", repo)
	taskMust(t, "task", "add", "Pick a cache", "--repo", repo, "--tag", "infra", "--tag", "cache")
	taskMust(t, "task", "add", "Old idea", "--repo", repo)
	taskMust(t, "task", "ready", "T-2")
	taskMust(t, "task", "ready", "T-3")
	taskMust(t, "task", "edit", "T-4", "--status", "dropped")
	taskPutSession(t, db, "s-work", "T-2", "%2", model.StateWorking)
	taskPutSession(t, db, "s-ask", "T-3", "%3", model.StateNeedsDecision)
	taskPutSession(t, db, "s-gone", "T-1", "%1", model.StateEnded)

	out := taskMust(t, "task", "ls", "--json")
	want, err := os.ReadFile(filepath.Join("testdata", "tasks.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := taskNormalise(out, repo); got != string(want) {
		t.Errorf("task ls --json, normalised:\n%s\nwant (testdata/tasks.golden.json):\n%s", got, want)
	}
}

func TestTaskLsDerivesFromSessions(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Work", "--repo", repo)
	taskMust(t, "task", "ready", "T-1")
	taskPutSession(t, db, "s1", "T-1", "%1", model.StateWorking)
	rows := taskLs(t)
	if len(rows) != 1 || rows[0].Status != "running" || rows[0].StoredStatus != "ready" || !slices.Equal(rows[0].Sessions, []string{"s1"}) {
		t.Fatalf("with a working session: %+v, want running, stored ready, sessions [s1]", rows)
	}
	taskPutSession(t, db, "s1", "T-1", "%1", model.StateEnded)
	rows = taskLs(t)
	if len(rows) != 1 || rows[0].Status != "ready" || len(rows[0].Sessions) != 0 {
		t.Errorf("after the session ended: %+v, want ready and no sessions", rows)
	}
}

func TestTaskLsStatusFilterAndAll(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	other := taskRepo(t, "main")
	taskMust(t, "task", "add", "Backlog", "--repo", repo)
	taskMust(t, "task", "add", "Ready", "--repo", repo)
	taskMust(t, "task", "add", "Running", "--repo", repo)
	taskMust(t, "task", "add", "Dropped", "--repo", repo)
	taskMust(t, "task", "add", "Elsewhere", "--repo", other)
	taskMust(t, "task", "ready", "T-2")
	taskMust(t, "task", "ready", "T-3")
	taskMust(t, "task", "edit", "T-4", "--status", "dropped")
	taskPutSession(t, db, "s3", "T-3", "%3", model.StateIdle)

	for _, c := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"T-1", "T-5", "T-2", "T-3"}},
		{[]string{"--status", "all"}, []string{"T-1", "T-5", "T-2", "T-3", "T-4"}},
		{[]string{"--status", "dropped"}, []string{"T-4"}},
		{[]string{"--status", "running"}, []string{"T-3"}},
		{[]string{"--status", "ready"}, []string{"T-2"}},
		{[]string{"--status", "needs_decision"}, []string{}},
		{[]string{"--repo", other}, []string{"T-5"}},
		{[]string{"--repo", repo, "--status", "backlog"}, []string{"T-1"}},
	} {
		if got := taskLsIDs(t, c.args...); !slices.Equal(got, c.want) {
			t.Errorf("task ls %v = %v, want %v", c.args, got, c.want)
		}
	}
	out, err := taskRun(t, nil, "task", "ls", "--status", "paused")
	taskWantErr(t, err, out, "--status")
}

func TestTaskLsTableIsSanitised(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Plain", "--repo", repo)
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE tasks SET title = ? WHERE id = 'T-1'`, "evil\x1b[31mred\u202eabc\x1b]0;x\x07"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"task", "ls"}, {"task", "ls", "--json"}} {
		out := taskMust(t, args...)
		if strings.ContainsAny(out, "\x1b\u202e\x07") {
			t.Errorf("%v printed a raw control or bidi rune: %q", args, out)
		}
		if !strings.Contains(out, "evil") {
			t.Errorf("%v lost the title: %q", args, out)
		}
	}
	if out := taskMust(t, "task", "ls"); !strings.HasPrefix(out, "ID") || !strings.Contains(out, "STATUS") ||
		!strings.Contains(out, "AGENT") {
		t.Errorf("table header missing: %q", out)
	}
}

func TestTaskLsTableNotes(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Paused one", "--repo", repo)
	taskMust(t, "task", "add", "Running one", "--repo", repo)
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE tasks SET status = 'ready', paused_at = ?, worktree = '/w/one' WHERE id = 'T-1'`,
		"2026-11-13T11:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE tasks SET status = 'ready', worktree = '/w/two' WHERE id = 'T-2'`); err != nil {
		t.Fatal(err)
	}
	taskPutSession(t, db, "s2", "T-2", "%7", model.StateWorking)
	taskPutSession(t, db, "s2b", "T-2", "%8", model.StateIdle)
	out := taskMust(t, "task", "ls")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("table:\n%s", out)
	}
	if !strings.Contains(lines[1], "ready (paused, worktree)") {
		t.Errorf("paused task with a worktree and no session: %q", lines[1])
	}
	if !strings.Contains(lines[2], "running") || strings.Contains(lines[2], "worktree") || !strings.Contains(lines[2], "%7 +1") {
		t.Errorf("running task: %q, want running, no worktree note, agent %%7 +1", lines[2])
	}
}

func TestTaskEditFlags(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Original", "--repo", repo, "--tag", "one")
	orig := taskGet(t, db, "T-1")
	body := taskWriteFile(t, t.TempDir(), "b.md", "New body\n")
	for _, c := range []struct {
		args  []string
		check func(model.Task) bool
	}{
		{[]string{"--title", "Renamed"}, func(g model.Task) bool { return g.Title == "Renamed" }},
		{[]string{"--body", body}, func(g model.Task) bool { return g.Body == "New body" && g.Title == "Renamed" }},
		{[]string{"--base", "develop"}, func(g model.Task) bool { return g.BaseBranch == "develop" && g.Body == "New body" }},
		{[]string{"--tag", "Two", "--tag", "three"}, func(g model.Task) bool {
			return slices.Equal(g.Tags, []string{"three", "two"}) && g.BaseBranch == "develop"
		}},
		{[]string{"--status", "dropped"}, func(g model.Task) bool {
			return g.Status == model.TaskDropped && g.Title == "Renamed" && g.Repo == orig.Repo
		}},
	} {
		out := taskMust(t, append([]string{"task", "edit", "T-1"}, c.args...)...)
		if out != "T-1 updated\n" {
			t.Errorf("edit %v output = %q", c.args, out)
		}
		if got := taskGet(t, db, "T-1"); !c.check(got) {
			t.Errorf("after edit %v: %+v", c.args, got)
		}
	}
	before := taskGet(t, db, "T-1")
	for _, c := range []struct {
		args  []string
		parts []string
	}{
		{[]string{"task", "edit", "T-99", "--title", "x"}, []string{"T-99", "no such task"}},
		{[]string{"task", "edit", "bogus", "--title", "x"}, []string{"invalid task id"}},
		{[]string{"task", "edit", "T-1", "--status", "running"}, []string{"--status"}},
		{[]string{"task", "edit", "T-1", "--title", ""}, []string{"title: is required"}},
		{[]string{"task", "edit", "T-1", "--base", "a..b", "--tag", "Bad!"}, []string{"base_branch:", "tags:"}},
	} {
		out, err := taskRun(t, nil, c.args...)
		taskWantErr(t, err, out, c.parts...)
	}
	if after := taskGet(t, db, "T-1"); !after.UpdatedAt.Equal(before.UpdatedAt) || after.Status != model.TaskDropped {
		t.Errorf("a refused edit changed the task: %+v", after)
	}
	out, err := taskRun(t, strings.NewReader(""), "task", "edit", "T-1")
	taskWantErr(t, err, out, "no editor set", `editor = "`, "--body")
	taskSetEditor(t, "/usr/bin/true")
	out, err = taskRun(t, strings.NewReader(""), "task", "edit", "T-1")
	taskWantErr(t, err, out, "nothing to change")
}

// taskEditorStub is a taskEditor that records the path it was given,
// checks the temp file is 0600 and holds one of the expected contents, and
// replaces its content with rewrite's result.
func taskEditorStub(t *testing.T, paths *[]string, rewrite func(content string) string) func(context.Context, []string, string) error {
	t.Helper()
	return func(_ context.Context, argv []string, path string) error {
		if want := []string{"/usr/bin/vim", "-f"}; !slices.Equal(argv, want) {
			t.Errorf("editor argv %q, want %q from the config", argv, want)
		}
		*paths = append(*paths, path)
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("temp file mode %v, want 0600", fi.Mode().Perm())
		}
		b, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return err
		}
		if string(b) != "Old title\n\nOld body\n" && string(b) != "New title\n\nNew body\n  indented\n" {
			t.Errorf("temp file holds %q", b)
		}
		return os.WriteFile(path, []byte(rewrite(string(b))), 0o600)
	}
}

func TestTaskEditThroughEditor(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	body := taskWriteFile(t, t.TempDir(), "b.md", "Old body\n")
	taskMust(t, "task", "add", "Old title", "--repo", repo, "--body", body)
	taskSetEditor(t, "/usr/bin/vim -f")
	tty, err := os.Open(os.DevNull) // a character device stands in for the terminal
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()

	var paths []string
	editWith := func(rewrite func(content string) string) {
		t.Helper()
		taskEditor = taskEditorStub(t, &paths, rewrite)
		if out, eerr := taskRun(t, tty, "task", "edit", "T-1"); eerr != nil {
			t.Fatalf("edit through editor: %v\n%s", eerr, out)
		}
	}

	editWith(func(string) string { return "New title\n\nNew body\n  indented\n" })
	got := taskGet(t, db, "T-1")
	if got.Title != "New title" || got.Body != "New body\n  indented" {
		t.Errorf("after the editor: title %q body %q", got.Title, got.Body)
	}

	later := decisionsParse(t, taskNow).Add(time.Hour)
	clock = func() time.Time { return later }
	editWith(func(s string) string { return s })         // unchanged
	editWith(func(string) string { return "  \n\n \n" }) // empty
	if after := taskGet(t, db, "T-1"); !after.UpdatedAt.Equal(got.UpdatedAt) || after.Title != "New title" {
		t.Errorf("an unchanged or empty file wrote the task: %+v", after)
	}

	taskEditor = func(context.Context, []string, string) error { return errors.New("editor exited 1") }
	out, err := taskRun(t, tty, "task", "edit", "T-1")
	taskWantErr(t, err, out, "editor")

	if len(paths) != 3 {
		t.Fatalf("editor ran %d times, want 3", len(paths))
	}
	for _, p := range paths {
		if _, serr := os.Stat(p); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("temp file %s still exists (%v)", p, serr)
		}
	}
}

func TestTaskEditWithoutEditorStartsNothing(t *testing.T) {
	taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Plain", "--repo", repo)
	tty, err := os.Open(os.DevNull) // a terminal: only the missing editor stops it
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	t.Setenv("VISUAL", "/usr/bin/false") // never read
	t.Setenv("EDITOR", "/usr/bin/false")
	// taskEditor is still the failing stub: reaching it would panic.
	out, err := taskRun(t, tty, "task", "edit", "T-1")
	taskWantErr(t, err, out, "no editor set", "config.toml")
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error is more than one line: %q", err)
	}
}

func TestTaskEditorRunsWithoutShell(t *testing.T) {
	db := taskHome(t)
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := taskWriteFile(t, dir, "editor.sh",
		"printf '%s\\n' \"$#\" \"$1\" > '"+args+"'\nprintf 'Scripted title\\n\\nScripted body\\n' > \"$1\"\n")
	// The option is split on spaces into a program (a bare name, looked up
	// on PATH) and its arguments; the path is one more argument, never
	// shell text.
	taskSetEditor(t, "sh "+script)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a; touch pwned $(touch pwned2)")
	if err = taskDefaultEditor(context.Background(), cfg.EditorArgs(), path); err != nil {
		t.Fatalf("editor: %v", err)
	}
	b, err := os.ReadFile(filepath.Clean(args))
	if err != nil {
		t.Fatal(err)
	}
	if want := "1\n" + path + "\n"; string(b) != want {
		t.Errorf("editor got %q, want %q", b, want)
	}
	for _, name := range []string{"pwned", "pwned2"} {
		if _, serr := os.Stat(filepath.Join(dir, name)); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("the path was run as shell text: %s exists", name)
		}
	}

	// The whole command with the real editor runner and the config's editor.
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Old", "--repo", repo)
	taskEditor = taskDefaultEditor
	tty, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	if out, rerr := taskRun(t, tty, "task", "edit", "T-1"); rerr != nil || out != "T-1 updated\n" {
		t.Fatalf("edit with the configured editor: %v %q", rerr, out)
	}
	if got := taskGet(t, db, "T-1"); got.Title != "Scripted title" || got.Body != "Scripted body" {
		t.Errorf("after the configured editor: %q %q", got.Title, got.Body)
	}

	for _, argv := range [][]string{{filepath.Join(dir, "missing-editor")}, {"/usr/bin/false"}, nil} {
		if err := taskDefaultEditor(context.Background(), argv, path); err == nil {
			t.Errorf("editor %q: no error", argv)
		}
	}
}

func TestTaskReady(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Ready me", "--repo", repo)
	if out := taskMust(t, "task", "ready", "T-1"); out != "T-1 ready\n" {
		t.Errorf("output = %q, want %q", out, "T-1 ready\n")
	}
	if got := taskGet(t, db, "T-1"); got.Status != model.TaskReady {
		t.Errorf("status = %s, want ready", got.Status)
	}
	out, err := taskRun(t, nil, "task", "ready", "T-7")
	taskWantErr(t, err, out, "T-7", "no such task")
}

package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// taskBacklogDir makes <repo>/backlog/tasks and returns it.
func taskBacklogDir(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(repo, "backlog", "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// taskBacklogMD is a Backlog.md task file with the given front matter
// lines and body.
func taskBacklogMD(front, body string) string {
	return "---\n" + front + "---\n" + body
}

// taskImportedByRef lists the stored tasks by source ref, failing unless
// there are want of them.
func taskImportedByRef(t *testing.T, db string, want int) map[string]model.Task {
	t.Helper()
	ts, err := taskOpen(t, db).ListTasks(t.Context(), store.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != want {
		t.Fatalf("stored %d tasks, want %d: %+v", len(ts), want, ts)
	}
	byRef := map[string]model.Task{}
	for _, tk := range ts {
		byRef[tk.SourceRef] = tk
	}
	return byRef
}

// taskSameFields compares the fields an import sets.
func taskSameFields(a, b model.Task) bool {
	return a.Title == b.Title && a.Body == b.Body && a.Repo == b.Repo && a.BaseBranch == b.BaseBranch &&
		a.Status == b.Status && a.Source == b.Source && a.SourceRef == b.SourceRef && slices.Equal(a.Tags, b.Tags)
}

func TestTaskImportBacklogMD(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	dir := taskBacklogDir(t, repo)
	taskWriteFile(t, dir, "task-1 - Fix-login.md", taskBacklogMD(
		"id: task-1\ntitle: Fix login\nstatus: To Do\nlabels:\n  - Auth\n  - api\n", "## Description\n\n\tKeep the tab.\n"))
	taskWriteFile(t, dir, "task-2 - Add-cache.md", taskBacklogMD("id: task-2\ntitle: 'Add cache'\nstatus: In Progress\n", "Body two\n"))
	taskWriteFile(t, dir, "add-search-index.md", taskBacklogMD("status: To Do\n", "No title in the front matter.\n"))
	taskWriteFile(t, dir, "task-4 - Shipped.md", taskBacklogMD("id: task-4\ntitle: Shipped\nstatus: Done\n", ""))
	taskWriteFile(t, dir, "notes.md", "just notes, no front matter\n")
	taskWriteFile(t, dir, "readme.txt", taskBacklogMD("title: not markdown\n", ""))
	outside := taskWriteFile(t, t.TempDir(), "outside.md", taskBacklogMD("id: task-9\ntitle: Outside the repo\n", ""))
	if err := os.Symlink(outside, filepath.Join(dir, "task-9 - Outside.md")); err != nil {
		t.Fatal(err)
	}

	if out := taskMust(t, "task", "import", "--backlog-md", repo); out != "imported 3, updated 0, skipped 3\n" {
		t.Errorf("first import printed %q", out)
	}
	byRef := taskImportedByRef(t, db, 3)
	want := model.Task{
		Title: "Fix login", Body: "## Description\n\n\tKeep the tab.", Repo: repo, BaseBranch: "main",
		Status: model.TaskBacklog, Source: model.SourceBacklogMD, SourceRef: "task-1", Tags: []string{"api", "auth"},
	}
	if one := byRef["task-1"]; !taskSameFields(one, want) {
		t.Errorf("task-1 stored as %+v, want %+v", one, want)
	}
	// The id field is the ref, else the file name; a slug name is a title.
	for ref, title := range map[string]string{"task-2": "Add cache", "add-search-index.md": "add search index"} {
		if got := byRef[ref].Title; got != title {
			t.Errorf("%s title %q, want %q", ref, got, title)
		}
	}

	t.Chdir(repo) // <dir> defaults to "."
	if out := taskMust(t, "task", "import", "--backlog-md"); out != "imported 0, updated 3, skipped 3\n" {
		t.Errorf("second import printed %q", out)
	}

	id := byRef["task-1"].ID
	taskMust(t, "task", "ready", id)
	taskWriteFile(t, dir, "task-1 - Fix-login.md", taskBacklogMD("id: task-1\ntitle: Fix the login flow\n", "New body\n"))
	taskMust(t, "task", "import", "--backlog-md", repo)
	if got := taskGet(t, db, id); got.Title != "Fix the login flow" || got.Body != "New body" || got.Status != model.TaskReady {
		t.Errorf("after the file changed: %+v, want the new title and body and status ready kept", got)
	}
	taskImportedByRef(t, db, 3)
}

func TestTaskImportBounds(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	dir := taskBacklogDir(t, repo)
	for i := range 501 {
		taskWriteFile(t, dir, fmt.Sprintf("task-%03d.md", i), taskBacklogMD(fmt.Sprintf("title: Task %d\n", i), ""))
	}
	out := taskMust(t, "task", "import", "--backlog-md", repo)
	if !strings.Contains(out, "first 500") || !strings.HasSuffix(out, "imported 500, updated 0, skipped 0\n") {
		t.Errorf("501 files printed %q", out)
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM tasks WHERE source_ref = 'task-500.md'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the 501st file by name was imported (%d, %v)", n, err)
	}

	small := taskRepo(t, "main")
	sdir := taskBacklogDir(t, small)
	taskWriteFile(t, sdir, "big.md", taskBacklogMD("title: Big\n", strings.Repeat("x", 300<<10)))
	taskWriteFile(t, sdir, "ok.md", taskBacklogMD("title: Fine\n", ""))
	if out := taskMust(t, "task", "import", "--backlog-md", small); out != "imported 1, updated 0, skipped 1\n" {
		t.Errorf("a 300 KiB file: printed %q", out)
	}
}

func TestTaskImportRefusals(t *testing.T) {
	taskHome(t)
	repo := taskRepo(t, "main")
	out, err := taskRun(t, nil, "task", "import", repo)
	taskWantErr(t, err, out, "--backlog-md")
	out, err = taskRun(t, nil, "task", "import", "--backlog-md", repo)
	taskWantErr(t, err, out, "backlog/tasks")
	out, err = taskRun(t, nil, "task", "import", "--backlog-md", hermeticGit(t))
	taskWantErr(t, err, out, "not inside a git repository")
	detached := taskRepo(t, "main")
	taskBacklogDir(t, detached)
	gitRun(t, detached, "checkout", "-q", "--detach")
	out, err = taskRun(t, nil, "task", "import", "--backlog-md", detached)
	taskWantErr(t, err, out, "HEAD is detached")
	_, wt := hookLinkedWorktree(t, "feat/x")
	out, err = taskRun(t, nil, "task", "import", "--backlog-md", wt)
	taskWantErr(t, err, out, "linked worktree")
	// backlog/tasks itself may not lead out of the repository.
	outside := t.TempDir()
	taskWriteFile(t, outside, "x.md", taskBacklogMD("title: Escaped\n", ""))
	if err = os.MkdirAll(filepath.Join(repo, "backlog"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(repo, "backlog", "tasks")); err != nil {
		t.Fatal(err)
	}
	out, err = taskRun(t, nil, "task", "import", "--backlog-md", repo)
	taskWantErr(t, err, out, "backlog/tasks")
}

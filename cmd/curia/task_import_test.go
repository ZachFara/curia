package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/linear"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
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

// ---- Linear import (Plan 5, Task 16) ----

// linearUnsetClient is linearClientFor under test until a test sets its
// own: building a Linear client in a test that did not ask for one panics,
// so no test can reach Linear (or any host) through the seam.
func linearUnsetClient(url, _ string) linear.Client {
	panic("linearClientFor called in a test that did not set it (use linearUseServer); url " + url)
}

func init() {
	linearClientFor = linearUnsetClient
}

// linearTestKey is the API key of the tests: distinctive, so a leak of any
// 8-byte piece of it is found.
const linearTestKey = "lin_api_TESTKEY0123456789abcdefSECRET"

// linearNode is one issue as a test server sends it.
type linearNode struct {
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
	URL         string `json:"url"`
	State       struct {
		Type string `json:"type"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

// linearIssue builds a node in state unstarted with the given labels.
func linearIssue(id, title, desc string, priority int, labels ...string) linearNode {
	n := linearNode{
		Identifier: id, Title: title, Description: desc, Priority: priority,
		URL: "https://linear.app/acme/issue/" + id + "/x",
	}
	n.State.Type = "unstarted"
	for _, l := range labels {
		n.Labels.Nodes = append(n.Labels.Nodes, struct {
			Name string `json:"name"`
		}{l})
	}
	return n
}

// linearPage is one response body: the nodes and, when next, a cursor.
func linearPage(t *testing.T, next bool, nodes ...linearNode) string {
	t.Helper()
	if nodes == nil {
		nodes = []linearNode{}
	}
	var page struct {
		Data struct {
			Issues struct {
				Nodes    []linearNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		} `json:"data"`
	}
	page.Data.Issues.Nodes = nodes
	page.Data.Issues.PageInfo.HasNextPage = next
	page.Data.Issues.PageInfo.EndCursor = "cursor-" + strconv.Itoa(len(nodes))
	b, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// linearReq is one request a test server received.
type linearReq struct {
	Method, Path, Auth, UserAgent, ContentType, Body string
}

// linearSrv is a local Linear stand-in on loopback; what it receives is
// kept in rec, apart from the server.
type linearSrv struct {
	*httptest.Server
	rec *linearRec
}

// linearRec records the requests and holds the answers. reply answers the
// request with index i (0-based); nil serves pages[i], else a 500.
type linearRec struct {
	mu    sync.Mutex
	reqs  []linearReq
	pages []string
	reply func(w http.ResponseWriter, r *http.Request, i int)
}

func (rec *linearRec) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rec.mu.Lock()
	i := len(rec.reqs)
	rec.reqs = append(rec.reqs, linearReq{
		Method: r.Method, Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization"),
		UserAgent: r.Header.Get("User-Agent"), ContentType: r.Header.Get("Content-Type"), Body: string(raw),
	})
	reply, pages := rec.reply, rec.pages
	rec.mu.Unlock()
	switch {
	case reply != nil:
		reply(w, r, i)
	case i < len(pages):
		_, _ = io.WriteString(w, pages[i])
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func linearServer(t *testing.T, pages ...string) *linearSrv {
	t.Helper()
	rec := &linearRec{pages: pages}
	s := &linearSrv{Server: httptest.NewServer(http.HandlerFunc(rec.serve)), rec: rec}
	t.Cleanup(s.Close)
	return s
}

// requests is a copy of what the server received so far.
func (s *linearSrv) requests() []linearReq {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return slices.Clone(s.rec.reqs)
}

// setPages replaces the pages the server serves from the next request on,
// counting requests from zero again.
func (s *linearSrv) setPages(pages ...string) {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.rec.pages, s.rec.reqs, s.rec.reply = pages, nil, nil
}

// setReply makes reply answer every request from now on.
func (s *linearSrv) setReply(reply func(w http.ResponseWriter, r *http.Request, i int)) {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.rec.reply = reply
}

// linearConfig writes the config file under the test's HOME (db is in it)
// with the [linear] section (team "" leaves it out).
func linearConfig(t *testing.T, db, url, team string) {
	t.Helper()
	body := fmt.Sprintf("[linear]\nurl = %q\n", url)
	if team != "" {
		body += fmt.Sprintf("team = %q\n", team)
	}
	pushWriteConfig(t, db, body)
}

// linearUseServer points the config's linear.url at srv, sets the key and
// makes linearClientFor build clients that dial only loopback addresses.
func linearUseServer(t *testing.T, db string, srv *linearSrv) {
	t.Helper()
	linearConfig(t, db, srv.URL+"/graphql", "")
	t.Setenv("CURIA_LINEAR_API_KEY", linearTestKey)
	linearUseLoopback(t)
}

// linearUseLoopback makes linearClientFor build the real client with the
// loopback-only HTTP client for the rest of the test.
func linearUseLoopback(t *testing.T) {
	t.Helper()
	prev := linearClientFor
	linearClientFor = func(url, key string) linear.Client {
		return linear.Client{URL: url, Key: key, HTTP: pushLoopbackClient}
	}
	t.Cleanup(func() { linearClientFor = prev })
}

// linearHome is taskHome with the import command's Linear flags reset after
// the test.
func linearHome(t *testing.T) string {
	t.Helper()
	db := taskHome(t)
	t.Cleanup(linearResetFlags)
	return db
}

// linearResetFlags restores --linear and --team, which taskResetFlags does
// not know.
func linearResetFlags() {
	for _, c := range taskCmd.Commands() {
		for _, name := range []string{"linear", "team"} {
			if f := c.Flags().Lookup(name); f != nil {
				_ = f.Value.Set(f.DefValue)
				f.Changed = false
			}
		}
	}
}

// linearRun is taskRun that also resets the Linear flags.
func linearRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	defer linearResetFlags()
	return taskRun(t, nil, args...)
}

// linearMust is linearRun failing the test on an error.
func linearMust(t *testing.T, args ...string) string {
	t.Helper()
	out, err := linearRun(t, args...)
	if err != nil {
		t.Fatalf("curia %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// linearSummary is the last line of the import's output.
func linearSummary(out string) string {
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	return lines[len(lines)-1]
}

// linearWantNoRequest fails when srv received anything.
func linearWantNoRequest(t *testing.T, srv *linearSrv) {
	t.Helper()
	if got := srv.requests(); len(got) != 0 {
		t.Errorf("the server saw %d requests, want none: %+v", len(got), got)
	}
}

// linearNoControl fails when s holds a control rune other than "\n" and
// "\t".
func linearNoControl(t *testing.T, field, s string) {
	t.Helper()
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || r == 0x1b || r == 0x9b {
			t.Errorf("%s holds the control rune %U: %q", field, r, s)
			return
		}
	}
}

// linearStandardPage is ENG-12 and ENG-13, valid, and one malformed
// identifier, with no next page.
func linearStandardPage(t *testing.T, titles ...string) string {
	t.Helper()
	if titles == nil {
		titles = []string{"Fix the login loop", "Write the guide"}
	}
	return linearPage(t, false,
		linearIssue("ENG-12", titles[0], "Users bounce between pages.", 1, "Bug", "Needs Design"),
		linearIssue("ENG-13", titles[1], "", 4),
		linearIssue("eng 7", "Malformed identifier", "Never imported.", 3),
	)
}

// linearWantOneRequest checks srv received exactly one request, a POST to
// /graphql with the key only in its Authorization header.
func linearWantOneRequest(t *testing.T, srv *linearSrv) {
	t.Helper()
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/graphql" || r.Auth != linearTestKey ||
		r.UserAgent != "curia" || r.ContentType != "application/json" {
		t.Errorf("request %+v", r)
	}
	if strings.Contains(r.Body, linearTestKey) {
		t.Error("the key is in the request body")
	}
}

func TestLinearImportCreatesTasks(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "trunk")
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := linearServer(t, linearStandardPage(t))
	linearUseServer(t, db, srv)

	out := linearMust(t, "task", "import", "--linear", "--repo", sub)
	if got := linearSummary(out); got != "imported 2, updated 0, unchanged 0, skipped 1" {
		t.Errorf("summary %q\n%s", got, out)
	}
	for _, want := range []string{"ENG-12", "ENG-13", "identifier missing or not of the form ABC-123"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not list %q:\n%s", want, out)
		}
	}
	byRef := taskImportedByRef(t, db, 2)
	for _, ref := range []string{"ENG-12", "ENG-13"} {
		tk := byRef[ref]
		if tk.Source != model.SourceLinear || tk.SourceRef != ref || tk.Status != model.TaskBacklog ||
			tk.Repo != repo || tk.BaseBranch != "trunk" {
			t.Errorf("%s stored as %+v, want source linear, status backlog, repo %s, base trunk", ref, tk, repo)
		}
	}
	linearWantOneRequest(t, srv)

	// --repo defaults to the current directory.
	t.Chdir(sub)
	srv.setPages(linearStandardPage(t))
	if got := linearSummary(linearMust(t, "task", "import", "--linear")); got != "imported 0, updated 0, unchanged 2, skipped 1" {
		t.Errorf("from the current directory: %q", got)
	}
}

func TestLinearImportMapsFields(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearPage(t, false,
		linearIssue("ENG-12", "Fix the login loop", "Users bounce.\n\n- [ ] reproduce", 1, "Bug", "Needs Design"),
		linearIssue("ENG-13", "No priority", "", 0),
		linearIssue("ENG-14", "Low", "", 4),
		linearIssue("ENG-15", "High", "", 2),
		linearIssue("ENG-16", "Medium", "", 3),
	))
	linearUseServer(t, db, srv)
	linearMust(t, "task", "import", "--linear", "--repo", repo)

	byRef := taskImportedByRef(t, db, 5)
	one := byRef["ENG-12"]
	wantBody := "Linear ENG-12: https://linear.app/acme/issue/ENG-12/x\n\nUsers bounce.\n\n- [ ] reproduce"
	if one.Body != wantBody {
		t.Errorf("body %q, want %q", one.Body, wantBody)
	}
	if !slices.Equal(one.Tags, []string{"bug", "needs-design"}) {
		t.Errorf("tags %v, want bug needs-design", one.Tags)
	}
	if one.Title != "Fix the login loop" {
		t.Errorf("title %q", one.Title)
	}
	for ref, want := range map[string]int{"ENG-12": 1, "ENG-13": 2, "ENG-14": 3, "ENG-15": 1, "ENG-16": 2} {
		if got := byRef[ref].Priority; got != want {
			t.Errorf("%s priority P%d, want P%d", ref, got, want)
		}
	}
	if got := byRef["ENG-13"].Body; got != "Linear ENG-13: https://linear.app/acme/issue/ENG-13/x" {
		t.Errorf("an issue without a description: body %q", got)
	}
}

func TestLinearImportAgainUpdatesOnly(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearStandardPage(t))
	linearUseServer(t, db, srv)
	linearMust(t, "task", "import", "--linear", "--repo", repo)
	byRef := taskImportedByRef(t, db, 2)

	// What I and spawn set on the tasks after the first import.
	st := taskOpen(t, db)
	for _, ref := range []string{"ENG-12", "ENG-13"} {
		if _, err := st.UpdateTask(t.Context(), byRef[ref].ID, func(tk model.Task) (model.Task, error) {
			tk.Status, tk.Priority, tk.Branch = model.TaskReady, 3, "curia/"+ref
			return tk, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	srv.setPages(linearStandardPage(t, "Fix the login loop for good", "Write the whole guide"))
	if got := linearSummary(linearMust(t, "task", "import", "--linear", "--repo", repo)); got != "imported 0, updated 2, unchanged 0, skipped 1" {
		t.Errorf("second import: %q", got)
	}
	after := taskImportedByRef(t, db, 2)
	for ref, title := range map[string]string{"ENG-12": "Fix the login loop for good", "ENG-13": "Write the whole guide"} {
		tk := after[ref]
		if tk.Title != title || tk.Status != model.TaskReady || tk.Priority != 3 || tk.Branch != "curia/"+ref || tk.ID != byRef[ref].ID {
			t.Errorf("%s after the second import: %+v, want title %q and status ready, P3, branch kept", ref, tk, title)
		}
	}

	// The same answer again changes nothing at all.
	srv.setPages(linearStandardPage(t, "Fix the login loop for good", "Write the whole guide"))
	if got := linearSummary(linearMust(t, "task", "import", "--linear", "--repo", repo)); got != "imported 0, updated 0, unchanged 2, skipped 1" {
		t.Errorf("third import: %q", got)
	}
	again := taskImportedByRef(t, db, 2)
	for ref := range after {
		if !reflect.DeepEqual(again[ref], after[ref]) {
			t.Errorf("%s changed on an import of the same answer:\n%+v\n%+v", ref, after[ref], again[ref])
		}
	}
}

func TestLinearImportLeavesVanishedIssues(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearStandardPage(t))
	linearUseServer(t, db, srv)
	linearMust(t, "task", "import", "--linear", "--repo", repo)
	before := taskImportedByRef(t, db, 2)

	// ENG-13 was closed or reassigned in Linear: it is no longer listed.
	srv.setPages(linearPage(t, false, linearIssue("ENG-12", "Fix the login loop", "Users bounce between pages.", 1, "Bug", "Needs Design")))
	if got := linearSummary(linearMust(t, "task", "import", "--linear", "--repo", repo)); got != "imported 0, updated 0, unchanged 1, skipped 0" {
		t.Errorf("summary %q", got)
	}
	after := taskImportedByRef(t, db, 2)
	if !reflect.DeepEqual(after["ENG-13"], before["ENG-13"]) {
		t.Errorf("the task of a vanished issue changed:\n%+v\n%+v", before["ENG-13"], after["ENG-13"])
	}
}

func TestLinearImportCleansText(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	title := strings.Repeat("\x1b[31mred\x1b[0m\x07", 3) + strings.Repeat("é", 300) + "\x1b]8;;https://evil.example\x07link\x1b]8;;\x07"
	desc := "\x1b[2Jcleared\r\nnext\x00line\n" + strings.Repeat("d", 1<<20)
	n := linearIssue("ENG-12", title, desc, 2, "x\x1b[2J", "Ok")
	n.URL = "javascript:alert(1)"
	srv := linearServer(t, linearPage(t, false, n))
	linearUseServer(t, db, srv)
	out := linearMust(t, "task", "import", "--linear", "--repo", repo)
	linearNoControl(t, "output", strings.ReplaceAll(out, "\t", " "))
	if strings.Contains(out, "evil.example") {
		t.Errorf("output holds the hyperlink target:\n%s", out)
	}

	tk := taskImportedByRef(t, db, 1)["ENG-12"]
	if n := utf8.RuneCountInString(tk.Title); n > 120 || n == 0 {
		t.Errorf("title of %d runes, want 1 to 120: %q", n, tk.Title)
	}
	if !strings.HasPrefix(tk.Title, "redredred") || strings.Contains(tk.Title, "evil") {
		t.Errorf("title %q", tk.Title)
	}
	if len(tk.Body) > task.MaxBody {
		t.Errorf("body of %d bytes, over %d", len(tk.Body), task.MaxBody)
	}
	if !strings.HasPrefix(tk.Body, "Linear ENG-12\n\ncleared\nnext") || strings.Contains(tk.Body, "javascript") {
		t.Errorf("body starts %q, want the identifier line without the URL", tk.Body[:min(60, len(tk.Body))])
	}
	if !slices.Equal(tk.Tags, []string{"ok", "x"}) { // the escape sequence goes whole
		t.Errorf("tags %q", tk.Tags)
	}
	for field, v := range map[string]string{"title": tk.Title, "body": tk.Body, "tags": strings.Join(tk.Tags, " "), "source_ref": tk.SourceRef} {
		linearNoControl(t, field, v)
		if field != "body" && strings.Contains(v, "\n") {
			t.Errorf("%s holds a line break: %q", field, v)
		}
	}
}

func TestLinearImportNeedsKey(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearStandardPage(t))
	linearConfig(t, db, srv.URL+"/graphql", "")
	linearUseLoopback(t)
	t.Setenv("CURIA_LINEAR_API_KEY", "") // restored after the test
	if err := os.Unsetenv("CURIA_LINEAR_API_KEY"); err != nil {
		t.Fatal(err)
	}
	out, err := linearRun(t, "task", "import", "--linear", "--repo", repo)
	taskWantErr(t, err, out, "set CURIA_LINEAR_API_KEY to a Linear API key (read access is enough)", "keychain")
	for _, key := range []string{"", "a b", "key\nInjected: 1", strings.Repeat("k", 201)} {
		t.Setenv("CURIA_LINEAR_API_KEY", key)
		out, err = linearRun(t, "task", "import", "--linear", "--repo", repo)
		taskWantErr(t, err, out, "set CURIA_LINEAR_API_KEY to a Linear API key (read access is enough)", "keychain")
		if key != "" && (strings.Contains(err.Error()+out, key) || strings.Contains(err.Error()+out, strconv.Itoa(len(key)))) {
			t.Errorf("the refusal quotes the key or its length: %v %s", err, out)
		}
	}
	linearWantNoRequest(t, srv)
}

// linearWantNoKey fails when any 8-byte piece of the test key is in one of
// the texts.
func linearWantNoKey(t *testing.T, what string, texts ...string) {
	t.Helper()
	all := strings.Join(texts, "\n")
	for i := 0; i+8 <= len(linearTestKey); i++ {
		if piece := linearTestKey[i : i+8]; strings.Contains(all, piece) {
			t.Errorf("%s holds %q, a piece of the key:\n%s", what, piece, all)
			return
		}
	}
}

func TestLinearImportNeverPrintsTheKey(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	replies := map[string]func(w http.ResponseWriter, r *http.Request, i int){
		"401": func(w http.ResponseWriter, r *http.Request, _ int) {
			msg, _ := json.Marshal("bad key " + r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"errors":[{"message":`+string(msg)+`}]}`)
		},
		"echo": func(w http.ResponseWriter, r *http.Request, _ int) {
			key := r.Header.Get("Authorization")
			msg, _ := json.Marshal("Authentication failed for " + key + " \x07(" + key + ")")
			_, _ = io.WriteString(w, `{"errors":[{"message":`+string(msg)+`}]}`)
		},
	}
	for name, reply := range replies {
		srv := linearServer(t)
		srv.setReply(reply)
		linearUseServer(t, db, srv)
		out, err := linearRun(t, "task", "import", "--linear", "--repo", repo)
		if err == nil {
			t.Fatalf("%s: no error\n%s", name, out)
		}
		if !strings.HasPrefix(err.Error(), "linear: ") {
			t.Errorf("%s: error %q does not start with linear:", name, err)
		}
		linearWantNoKey(t, name+": stdout, stderr and the error", out, err.Error())
		linearWantNoKey(t, name+": the log", readLog(t)...)
		if len(srv.requests()) != 1 {
			t.Errorf("%s: %d requests, want 1", name, len(srv.requests()))
		}
	}
	if n := taskCount(t, db); n != 0 {
		t.Errorf("%d tasks stored after refusals", n)
	}
	raw, err := os.ReadFile(filepath.Clean(db))
	if err != nil {
		t.Fatal(err)
	}
	linearWantNoKey(t, "the store", string(raw))
}

func TestLinearImportUnauthorizedNamesTheVariable(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t)
	srv.setReply(func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusUnauthorized) })
	linearUseServer(t, db, srv)
	out, err := linearRun(t, "task", "import", "--linear", "--repo", repo)
	taskWantErr(t, err, out, "linear: status 401", "CURIA_LINEAR_API_KEY", "nothing was imported")
}

func TestLinearImportTeamFromFlagAndConfig(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t)
	linearUseServer(t, db, srv)
	teamOf := func(t *testing.T, args ...string) string {
		t.Helper()
		srv.setPages(linearPage(t, false))
		linearMust(t, append([]string{"task", "import", "--linear", "--repo", repo}, args...)...)
		reqs := srv.requests()
		if len(reqs) != 1 {
			t.Fatalf("%d requests, want 1", len(reqs))
		}
		var body struct {
			Variables struct {
				Filter map[string]json.RawMessage `json:"filter"`
			} `json:"variables"`
		}
		if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
			t.Fatal(err)
		}
		return string(body.Variables.Filter["team"])
	}
	if got := teamOf(t); got != "" {
		t.Errorf("no team configured: filter team %s", got)
	}
	linearConfig(t, db, srv.URL+"/graphql", "OPS")
	if got := teamOf(t); got != `{"key":{"eq":"OPS"}}` {
		t.Errorf("team from the config: %s", got)
	}
	if got := teamOf(t, "--team", "ENG"); got != `{"key":{"eq":"ENG"}}` {
		t.Errorf("the flag should win: %s", got)
	}
	srv.setPages()
	for _, bad := range []string{"eng", "ENG-1", "ABCDEFGHIJK", "E\"}"} {
		out, err := linearRun(t, "task", "import", "--linear", "--repo", repo, "--team", bad)
		taskWantErr(t, err, out, "--team")
	}
	linearWantNoRequest(t, srv)
}

func TestLinearImportServerDown(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t)
	linearUseServer(t, db, srv)
	srv.Close() // the port is closed now
	out, err := linearRun(t, "task", "import", "--linear", "--repo", repo)
	taskWantErr(t, err, out, "linear: connection refused", "nothing was imported")
	if n := taskCount(t, db); n != 0 {
		t.Errorf("%d tasks stored", n)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the error quotes the address: %v", err)
	}
}

func TestLinearImportSecondPageFails(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	first := linearPage(t, true, linearIssue("ENG-12", "One", "", 1), linearIssue("ENG-13", "Two", "", 2))
	srv := linearServer(t, first) // the second request gets a 500
	linearUseServer(t, db, srv)
	out, err := linearRun(t, "task", "import", "--linear", "--repo", repo)
	taskWantErr(t, err, out, "linear: status 500", "nothing was imported")
	if len(srv.requests()) != 2 {
		t.Errorf("%d requests, want 2", len(srv.requests()))
	}
	// The client returns nothing when any page fails, so nothing of the
	// first page is stored either: the import is all or nothing.
	if n := taskCount(t, db); n != 0 {
		t.Errorf("%d tasks stored after a failed page, want 0", n)
	}
	// A retry once Linear answers imports everything.
	srv.setPages(first, linearPage(t, false, linearIssue("ENG-14", "Three", "", 3)))
	if got := linearSummary(linearMust(t, "task", "import", "--linear", "--repo", repo)); got != "imported 3, updated 0, unchanged 0, skipped 0" {
		t.Errorf("the retry: %q", got)
	}
}

func TestLinearImportCapped(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	var pages []string
	for p := range 5 {
		var nodes []linearNode
		for i := range 50 {
			nodes = append(nodes, linearIssue(fmt.Sprintf("ENG-%d", p*50+i+1), fmt.Sprintf("Issue %d", p*50+i+1), "", 3))
		}
		pages = append(pages, linearPage(t, true, nodes...))
	}
	srv := linearServer(t, pages...)
	linearUseServer(t, db, srv)
	out := linearMust(t, "task", "import", "--linear", "--repo", repo)
	if !strings.Contains(out, "first 200 of your assigned issues") || !strings.Contains(out, "narrow with --team") {
		t.Errorf("the cap is not reported:\n%s", out[max(0, len(out)-400):])
	}
	if got := linearSummary(out); got != "imported 200, updated 0, unchanged 0, skipped 0" {
		t.Errorf("summary %q", got)
	}
	if n := len(srv.requests()); n != 4 {
		t.Errorf("%d requests, want 4", n)
	}
	taskImportedByRef(t, db, 200)

	srv.setPages(linearStandardPage(t))
	if out := linearMust(t, "task", "import", "--linear", "--repo", repo); strings.Contains(out, "first 200") {
		t.Errorf("a short answer reports the cap:\n%s", out)
	}
}

func TestLinearImportURLTheClientRefuses(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearStandardPage(t))
	t.Setenv("CURIA_LINEAR_API_KEY", linearTestKey)
	linearUseLoopback(t)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	// Each passes the config file's check; the client is stricter.
	for _, url := range []string{"http://localhost:" + port + "/graphql", srv.URL + "/v1/graphql", srv.URL} {
		linearConfig(t, db, url, "")
		out, err := linearRun(t, "task", "import", "--linear", "--repo", repo)
		taskWantErr(t, err, out, "linear.url", "config.toml", "https://<host>/graphql", "nothing was sent")
	}
	linearWantNoRequest(t, srv)
}

func TestImportNeedsExactlyOneSource(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearStandardPage(t))
	linearUseServer(t, db, srv)
	for _, args := range [][]string{
		{repo},
		{"--backlog-md", "--linear", repo},
		{"--linear", repo},
		{"--backlog-md", "--team", "ENG", repo},
		{"--backlog-md", "--repo", repo},
	} {
		out, err := linearRun(t, append([]string{"task", "import"}, args...)...)
		if err == nil {
			t.Errorf("import %v: no error\n%s", args, out)
			continue
		}
		if !strings.Contains(err.Error(), "--linear") {
			t.Errorf("import %v: error %q does not name --linear", args, err)
		}
	}
	linearWantNoRequest(t, srv)
}

func TestLinearStatusIsNeverMapped(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	started := linearIssue("ENG-20", "Already started", "", 2)
	started.State.Type = "started"
	backlog := linearIssue("ENG-21", "In the backlog", "", 2)
	backlog.State.Type = "backlog"
	srv := linearServer(t, linearPage(t, false, started, backlog))
	linearUseServer(t, db, srv)
	out := linearMust(t, "task", "import", "--linear", "--repo", repo)
	// Task 5's client refuses a state the filter did not ask for, so a
	// started issue is skipped and never imported with Linear's status.
	if got := linearSummary(out); got != "imported 1, updated 0, unchanged 0, skipped 1" {
		t.Errorf("summary %q", got)
	}
	if !strings.Contains(out, "state is not backlog or unstarted") {
		t.Errorf("the skipped issue's reason is missing:\n%s", out)
	}
	if tk := taskImportedByRef(t, db, 1)["ENG-21"]; tk.Status != model.TaskBacklog {
		t.Errorf("ENG-21 status %q, want backlog", tk.Status)
	}
	for _, r := range srv.requests() {
		if strings.Contains(strings.ToLower(r.Body), "mutation") {
			t.Errorf("a request holds a mutation: %s", r.Body)
		}
	}
}

func TestLinearImportIsUserOnly(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearStandardPage(t))
	linearUseServer(t, db, srv)
	userTerminalAs(t, false)
	out, err := linearRun(t, "task", "import", "--linear", "--repo", repo)
	taskWantErr(t, err, out, "terminal")
	linearWantNoRequest(t, srv)
	if n := taskCount(t, db); n != 0 {
		t.Errorf("%d tasks stored", n)
	}
}

func TestLinearImportReadsBuiltPages(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	var pages []string
	for _, name := range []string{"built-issues.json", "built-issues-page2.json"} {
		b, err := os.ReadFile(filepath.Clean(filepath.Join("..", "..", "internal", "linear", "testdata", name)))
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, string(b))
	}
	srv := linearServer(t, pages...)
	// linearImport itself, with a client the test builds: no config file.
	c := linear.Client{URL: srv.URL + "/graphql", Key: linearTestKey, HTTP: pushLoopbackClient}
	res, err := linearImport(t.Context(), taskOpen(t, db), c, "", repo, "main", decisionsParse(t, taskNow))
	if err != nil {
		t.Fatal(err)
	}
	if res.imported != 4 || res.updated != 0 || res.unchanged != 0 || res.skipped != 1 || res.capped || len(res.rows) != 5 {
		t.Errorf("Task 5's built pages: %+v", res)
	}
	byRef := taskImportedByRef(t, db, 4)
	if got := byRef["OPS-9"].Body; got != "Linear OPS-9\n\nOld runners use the retired image." {
		t.Errorf("OPS-9 (a lookalike URL) body %q", got)
	}
	if got := byRef["ENG-103"].Title; got != "Show the red banner" {
		t.Errorf("ENG-103 title %q", got)
	}
}

// TestLinearImportGoesThroughTheSeam: the command builds its client only
// through linearClientFor, so a test that did not set the seam cannot
// connect anywhere (its default panics before anything is sent).
func TestLinearImportGoesThroughTheSeam(t *testing.T) {
	db := linearHome(t)
	repo := taskRepo(t, "main")
	srv := linearServer(t, linearStandardPage(t))
	linearConfig(t, db, srv.URL+"/graphql", "")
	t.Setenv("CURIA_LINEAR_API_KEY", linearTestKey)
	var got any
	func() {
		defer func() { got = recover() }()
		_, _ = linearRun(t, "task", "import", "--linear", "--repo", repo)
	}()
	if msg, ok := got.(string); !ok || !strings.Contains(msg, "linearClientFor called in a test that did not set it") {
		t.Errorf("recovered %v, want the seam's panic", got)
	}
	linearWantNoRequest(t, srv)
}

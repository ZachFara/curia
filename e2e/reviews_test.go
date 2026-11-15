package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The Plan 4 acceptance test: the built binary as the user, tmux and Claude
// Code would run it, with a real SQLite file, a real git repository, private
// tmux servers, Plan 3's stub `claude` (testdata/tasks/claude-stub.sh) in the
// windows curia spawn and curia restore open, a stand-in ntfy server and the
// real curia serve, both on 127.0.0.1. A task is spawned, completed, pushed
// to the phone, answered from a push button, sent back, completed again,
// approved by a real git merge (once refused because the branch moved), and
// restored after its tmux server went down. Nothing reaches another host.

// The sessions of the script besides T-1's agent (tasksLead, which the stub
// derives from the task id).
const (
	reviewsNested = "55555555-5555-4555-8555-555555555555" // ended by the next session in its pane: no reason recorded
	reviewsExited = "66666666-6666-4666-8666-666666666666" // ended by the user's own exit
)

const (
	// reviewsWait bounds every wait for a condition another process brings
	// about (a push reaching the stand-in ntfy, a line typed, a session
	// reporting): long enough for a loaded runner, so only a condition that
	// never comes fails. The brief's "within 3 seconds" for a push is logged
	// as the time it took, not asserted.
	reviewsWait = 60 * time.Second
	// reviewsServeStop bounds curia serve's exit after SIGTERM (the brief's
	// 6 seconds).
	reviewsServeStop = 6 * time.Second
	// reviewsKeyPause is how long the review view is left alone before a
	// key: longer than its 250 ms dwell, which ignores an earlier one.
	reviewsKeyPause = 400 * time.Millisecond
	// reviewsAfterAction is longer than the 2 s quiet period the review view
	// keeps after an action.
	reviewsAfterAction = 2500 * time.Millisecond
	// reviewsRefusalTries bounds the attempts at the approval the moved
	// branch refuses (see approveInView).
	reviewsRefusalTries = 5
	reviewsBranchToken  = "<worktree>"
	reviewsToken        = "TOKEN"
)

// reviewsFixtures and reviewsGolden are the test's fixture and golden
// directories, relative to the repository root; reviewsReply is ntfy's
// recorded reply to an accepted publish (probe N1).
var (
	reviewsFixtures = filepath.Join("testdata", "reviews")
	reviewsGolden   = filepath.Join("e2e", "testdata", "reviews")
	reviewsReply    = filepath.Join("internal", "notify", "testdata", "ntfy", "publish-reply.json")
)

// TestReviewsEndToEnd runs Plan 4's acceptance script, as built.
func TestReviewsEndToEnd(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	reviewsStatSources(t, root)
	r := newReviewsRun(t, root, build(t, root))

	r.spawn(t)              // 1
	r.firstReview(t)        // 2
	r.blockingPushes(t)     // 3
	r.answerFromPhone(t)    // 4
	r.refusedInsideAgent(t) // 5
	r.requestChanges(t)     // 6
	r.approveByMerge(t)     // 7
	r.restore(t)            // 8
	r.twoServers(t)         // Task 14: a pane id of another server is never typed into
	r.finalState(t)         // 9
}

// reviewsRun is one run of the script. It reuses Plan 3's run for the
// stub, the user's terminal and the listings, with this test's repository.
type reviewsRun struct {
	*tasksRun
	ntfy       *reviewsNtfy
	agent      *mcpClient
	serve      reviewsServe
	token      string // the one-time token of D-2's push
	transcript string // T-1's agent's transcript, as Claude Code reports it
}

func newReviewsRun(t *testing.T, root, bin string) *reviewsRun {
	t.Helper()
	tmp := t.TempDir()
	tr := &tasksRun{h: newHome(t, bin), root: root, tmp: tmp, repo: filepath.Join(tmp, "src", "api-server")}
	tr.stubDir, tr.panes = filepath.Join(tmp, "bin"), filepath.Join(tmp, "panes")
	for _, d := range []string{tr.repo, tr.stubDir, tr.panes, filepath.Join(tmp, "claude"), filepath.Join(tmp, "transcripts")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := &reviewsRun{tasksRun: tr, transcript: filepath.Join(tmp, "transcripts", tasksLead+".jsonl")}
	r.makeRepo(t)
	tr.makeStub(t, bin)
	r.ntfy = startReviewsNtfy(t, readFile(t, filepath.Join(root, reviewsReply)))
	cfg := strings.ReplaceAll(readFile(t, filepath.Join(root, reviewsFixtures, "config.toml.tmpl")), "<ntfy_url>", r.ntfy.srv.URL)
	tasksWrite(t, filepath.Join(tr.h.dir, "home", ".config", "curia", "config.toml"), cfg)
	tr.srv = startTmux(t, filepath.Join(tmp, "tmuxhome"), tr.paneFile("p0"), tr.paneFile("p1"), tr.paneFile("p2"))
	tr.setEnv(t)
	tr.term = startTerminal(t)
	for _, kv := range tr.h.env {
		if strings.HasPrefix(kv, "CURIA_NTFY_TOKEN=") || strings.HasPrefix(kv, "CLAUDECODE=") {
			t.Fatalf("the run's environment holds %s", kv)
		}
	}
	return r
}

// makeRepo creates api-server with one commit on main and records its path
// forms and T-1's worktree, as Plan 3's test does.
func (r *reviewsRun) makeRepo(t *testing.T) {
	t.Helper()
	gitInit(t, r.repo, r.tmp)
	r.git(t, r.repo, "symbolic-ref", "HEAD", "refs/heads/main")
	r.git(t, r.repo, "config", "user.name", "Curia Test")
	r.git(t, r.repo, "config", "user.email", "test@example.com")
	tasksWrite(t, filepath.Join(r.repo, "README.md"), "# api-server\n")
	r.git(t, r.repo, "add", "-A")
	r.git(t, r.repo, "commit", "-q", "-m", "init")
	resolved, err := filepath.EvalSymlinks(r.repo)
	if err != nil {
		t.Fatal(err)
	}
	r.repos = []string{resolved, r.repo}
	r.parents = []string{filepath.Dir(resolved), filepath.Dir(r.repo)}
	r.worktree = filepath.Join(filepath.Dir(resolved), ".curia-worktrees", "api-server-T-1")
}

// Step 1: the task is added and spawned; the stub runs in window T-1 and
// reports, and Claude Code's transcript path comes with a compaction.
func (r *reviewsRun) spawn(t *testing.T) {
	t.Helper()
	if got := r.user(t, "task", "add", "Fix the auth refresh", "--repo", r.repo); got != "T-1 added (backlog)\n" {
		t.Errorf("curia task add = %q, want %q", got, "T-1 added (backlog)\n")
	}
	r.leadPane = r.spawnedPane(t, "T-1", r.user(t, "spawn", "T-1"))
	// The stub's own hooks carry no transcript_path, which Claude Code
	// sends with every event and curia restore needs. A compaction's
	// SessionStart is the event that records a new one on a live session.
	tasksWrite(t, r.transcript, "")
	r.agentHook(t, "SessionStart", map[string]any{"source": "compact", "transcript_path": r.transcript})
	s := r.sessions(t)[tasksLead]
	if s.TaskID != "T-1" || s.Pane != r.leadPane || s.State != "idle" || s.Worktree != r.worktree {
		t.Errorf("curia ls --json shows T-1's agent as %+v, want task T-1, idle in %s, worktree %s", s, r.leadPane, r.worktree)
	}
	r.wantTasks(t, map[string]string{"T-1": "running"})
	r.wantBadge(t, "", "after the spawn")
}

// Step 2: an assumption, a commit, complete_task: R-1 is listed, counted,
// marked and pushed, with no button.
func (r *reviewsRun) firstReview(t *testing.T) {
	t.Helper()
	script := readLines(t, filepath.Join(r.root, reviewsFixtures, "client_agent.jsonl"))
	summary, _ := json.Marshal(strings.TrimSuffix(readFile(t, filepath.Join(r.root, reviewsFixtures, "summary.txt")), "\n"))
	for i, line := range script {
		script[i] = strings.ReplaceAll(line, `"<summary.txt>"`, string(summary))
	}
	env := append(slices.Clone(r.h.env), "CLAUDE_PROJECT_DIR="+r.worktree, "TMUX_PANE="+r.leadPane)
	r.agent = startClient(t, r.h, "client_agent", r.worktree, script, env)
	r.agent.handshake(t)
	r.agent.call(t) // tools/list: the six tools
	r.agent.call(t) // request_decision: D-1, assume
	r.agentWorks(t, "auth/refresh.go", "package auth\n\n// rotate is called on every use.\n")
	r.agent.call(t) // complete_task: R-1
	r.agentStops(t)
	r.wantTasks(t, map[string]string{"T-1": "review"})
	r.wantBadge(t, "R1", "with R-1 pending")
	r.wantMarker(t, "blue", "with R-1 pending")
	r.wantPush(t, r.ntfy.wait(t, 1)[0], "push_r1.golden.json")
	if out := r.h.mustRun(t, "reviews", "ls"); !strings.Contains(out, "R-1") || !strings.Contains(out, "agent: passed") {
		t.Errorf("curia reviews ls =\n%s\nwant R-1 with the agent's test report", out)
	}
}

// Step 3: two blocking tickets: the reversible one is pushed with three
// buttons, the recommended first; the irreversible one with none.
func (r *reviewsRun) blockingPushes(t *testing.T) {
	t.Helper()
	r.agent.call(t) // request_decision: D-2, blocking, A B C, B recommended
	r.token = r.wantPush(t, r.ntfy.wait(t, 2)[1], "push_d2.golden.json")
	r.agent.call(t) // request_decision: D-3, blocking, irreversible
	r.wantPush(t, r.ntfy.wait(t, 3)[2], "push_d3.golden.json")
	r.wantBadge(t, "D2 R1", "with D-2 and D-3 open")
	r.wantTasks(t, map[string]string{"T-1": "review"}) // a pending review comes before open tickets
}

// Step 4: curia serve answers D-2 from the push's button through the path
// curia decide takes: typed into the stub. The token works once; a spent
// token, a wrong one and the irreversible ticket are refused alike.
func (r *reviewsRun) answerFromPhone(t *testing.T) {
	t.Helper()
	r.serve = r.startServe(t)
	answer := r.serve.base + "/v1/answer"
	ok, refused := `{"decision":"D-2","key":"B"}`, `{"error":"unauthorized"}`
	if code, body := reviewsPost(t, answer, r.token, ok); code != http.StatusOK ||
		body != `{"decision":"D-2","status":"answered","delivery":"delivered"}` {
		t.Errorf("POST /v1/answer with D-2's token = %d %s, want 200 and D-2 answered, delivered", code, body)
	}
	if d := r.decision(t, "D-2"); d.Status != "answered" || d.AnsweredVia != "ntfy" || d.AnswerKey != "B" {
		t.Errorf("D-2 after the phone answer = %+v, want answered B via ntfy", d)
	}
	r.waitTyped(t, r.inputLines(t, 1))
	for name, req := range map[string][2]string{
		"the same token again":             {r.token, ok},
		"a wrong token":                    {strings.Repeat("A", 43), ok},
		"D-2's token for irreversible D-3": {r.token, `{"decision":"D-3","key":"B"}`},
	} {
		if code, body := reviewsPost(t, answer, req[0], req[1]); code != http.StatusUnauthorized || body != refused {
			t.Errorf("POST /v1/answer with %s = %d %s, want 401 %s", name, code, body, refused)
		}
	}
	if d := r.decision(t, "D-3"); d.Status != "open" {
		t.Errorf("D-3 after the refused answers = %+v, want open", d)
	}
	if code, body := reviewsGet(t, r.serve.base+"/healthz"); code != http.StatusOK || body != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 ok", code, body)
	}
	r.wantBadge(t, "D1 R1", "after D-2's answer")
	r.agent.call(t) // withdraw_decision: D-3
	r.wantBadge(t, "R1", "after D-3 was withdrawn")
}

// Step 5: inside an agent session the review is not approved; listing it
// is allowed there.
func (r *reviewsRun) refusedInsideAgent(t *testing.T) {
	t.Helper()
	agent := []string{"CLAUDECODE=1"}
	if stderr := r.fails(t, agent, "review", "T-1", "--approve"); !strings.Contains(stderr, "inside an agent session") {
		t.Errorf("curia review --approve with CLAUDECODE=1: stderr %q, want the agent refusal", stderr)
	}
	stdout, stderr, err := r.curia(agent, "reviews", "ls", "--json")
	if err != nil || stderr != "" || !strings.Contains(stdout, `"status": "pending"`) {
		t.Errorf("curia reviews ls --json with CLAUDECODE=1: %v, stderr %q, stdout:\n%s\nwant R-1 pending", err, stderr, stdout)
	}
}

// Step 6: the change request reaches the agent as a message, typed into
// its idle prompt; the task runs again.
func (r *reviewsRun) requestChanges(t *testing.T) {
	t.Helper()
	out := r.user(t, "review", "T-1", "--request-changes", "--note", "handle the empty list")
	if want := "R-1: changes requested; M-1 delivered (send_keys)\n"; out != want {
		t.Errorf("curia review --request-changes = %q, want %q", out, want)
	}
	r.waitTyped(t, r.inputLines(t, 2))
	if m := r.message(t, "M-1"); m.Sender != "user" || m.DeliveredVia != "send_keys" || m.ConfirmedAt == nil {
		t.Errorf("M-1 = %+v, want the user's, typed and confirmed", m)
	}
	r.wantTasks(t, map[string]string{"T-1": "running"})
	r.wantBadge(t, "", "after the change request")
	r.wantMarker(t, "", "after the change request")
}

// Step 7: a second commit and complete_task open R-2; the review view
// approves only the branch head it showed: once the agent moved the branch
// it refuses, then it merges what it shows.
func (r *reviewsRun) approveByMerge(t *testing.T) {
	t.Helper()
	r.agentWorks(t, "auth/refresh_test.go", "package auth\n\n// TestEmptyList covers an empty token list.\n")
	r.agent.call(t) // complete_task: R-2
	r.agentStops(t)
	r.wantPush(t, r.ntfy.wait(t, 4)[3], "push_r2.golden.json")
	r.wantBadge(t, "R1", "with R-2 pending")
	r.wantMarker(t, "blue", "with R-2 pending")
	r.checkAgentClient(t)

	printed, head := r.approveInView(t)
	for _, want := range []string{
		"approve failed: the branch changed since you looked; showing the new state\n",
		"R-2 approved: merged " + tasksBranch + " into main (" + head[:7] + "). 1 unconfirmed assumption expired. " +
			"Remove the worktree and branch with: curia clean T-1\n",
		"curia exited 0\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("curia review T-1 left on its terminal:\n%s\nwant it to hold %q", printed, want)
		}
	}
	merges := strings.Fields(r.git(t, r.repo, "log", "--merges", "--format=%P", "main"))
	if len(merges) != 2 || merges[1] != head {
		t.Errorf("merges on main (parents) = %q, want one merge whose second parent is %s", merges, head)
	}
	if d := r.decision(t, "D-1"); d.Status != "expired" {
		t.Errorf("D-1 after the merge = %+v, want expired", d)
	}
	r.wantGolden(t, "tasks.golden.json", r.h.mustRun(t, "task", "ls", "--status", "all", "--json"))
	r.wantBadge(t, "", "after the merge")
	r.wantMarker(t, "", "after the merge")
}

// approveInView approves R-2 in curia review's view at a terminal, and
// returns what the command left on its terminal and the head it merged.
// The first approval is refused: the branch moves between the prompt and
// its "y". The view rereads the detail every 5 seconds, and a reread that
// comes in between cancels the prompt instead ("the review changed"), which
// is also right; then the test moves the branch and tries again.
func (r *reviewsRun) approveInView(t *testing.T) (printed, head string) {
	t.Helper()
	pane := r.view(t, "review", "T-1")
	head = r.branchHead(t)
	for try := 1; ; try++ {
		r.screen(t, pane, "branch at "+head[:7])
		time.Sleep(reviewsKeyPause)
		r.keys(t, pane, "a")
		r.screen(t, pane, "into main? y confirms")
		asked := time.Now()
		r.git(t, r.worktree, "commit", "-q", "--allow-empty", "-m", "tidy up "+strconv.Itoa(try))
		head = r.branchHead(t)
		time.Sleep(time.Until(asked.Add(reviewsKeyPause)))
		r.keys(t, pane, "y")
		if strings.Contains(r.screen(t, pane, "the branch changed since you looked", "the review changed; read it again"),
			"the branch changed since you looked") {
			t.Logf("the moved branch was refused on try %d", try)
			break
		}
		if try == reviewsRefusalTries {
			t.Fatalf("the review's own reread beat the moved branch %d times", try)
		}
		r.keys(t, pane, "j") // the key after a canceled prompt is refused
		time.Sleep(reviewsAfterAction)
	}
	r.screen(t, pane, "branch at "+head[:7]) // the refusal shows the new state
	time.Sleep(reviewsAfterAction)
	r.keys(t, pane, "a")
	r.screen(t, pane, "into main? y confirms")
	time.Sleep(reviewsKeyPause)
	r.keys(t, pane, "y")
	r.screen(t, pane, "R-2 approved: merged")
	r.keys(t, pane, "q")
	return r.screen(t, pane, "curia exited"), head
}

// Step 8: tmux goes down and comes back on the same socket; curia restore
// offers the session Claude Code ended with reason "other", not the one the
// user exited, and skips one ended with no reason; it opens one window,
// resuming the session with the stub, and types nothing.
func (r *reviewsRun) restore(t *testing.T) {
	t.Helper()
	r.otherSessions(t)
	r.tmuxGoesDown(t)
	plan := r.user(t, "restore", "--dry-run")
	r.wantPlan(t, plan)
	stdout, stderr, code := r.term.run(t, r.h, r.h.env, "restore")
	if code != 1 || !strings.Contains(stderr, "pass --yes") || !strings.Contains(stdout, "SESSION") {
		t.Errorf("curia restore without --yes: exit %d, stdout %q, stderr %q, want the plan, then exit 1 asking for --yes", code, stdout, stderr)
	}
	if out, err := r.srv.run("has-session", "-t", "=api-server"); err == nil {
		t.Fatalf("a window was opened before the restore was confirmed: %s", out)
	}
	out := r.user(t, "restore", "--yes")
	m := regexp.MustCompile(`(?m)^restored 00000001 in api-server:T-1 \((%[0-9]+)\)$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("curia restore --yes printed:\n%s\nwant a line restored 00000001 in api-server:T-1 (%%n)", out)
	}
	r.leadPane = m[1]
	if got := r.srv.mustRun(t, "list-windows", "-t", "=api-server", "-F", "#{window_name}"); got != "T-1\n" {
		t.Errorf("windows of api-server after the restore = %q, want T-1 alone", got)
	}
	r.wantResumed(t)
	if s := r.sessions(t)[tasksLead]; s.State != "idle" || s.Pane != r.leadPane {
		t.Errorf("T-1's agent after the restore = %+v, want idle in %s", s, r.leadPane)
	}
	if again := r.user(t, "restore", "--yes"); strings.Contains(again, "restored") || !strings.Contains(again, "nothing to restore") {
		t.Errorf("a second curia restore --yes printed:\n%s\nwant nothing restored", again)
	}
	if got := readFile(t, r.inputFile()); got != r.inputLines(t, 2) {
		t.Errorf("input-T-1.txt after the restore = %q, want nothing typed", got)
	}
}

// otherSessions starts two sessions in the agents' pane %1, one after the
// other: the second ends the first with no reason recorded (a nested
// session), then the user exits the second.
func (r *reviewsRun) otherSessions(t *testing.T) {
	t.Helper()
	dir := filepath.Join(r.tmp, "src")
	for _, id := range []string{reviewsNested, reviewsExited} {
		path := filepath.Join(r.tmp, "transcripts", id+".jsonl")
		tasksWrite(t, path, "")
		r.paneHook(t, id, dir, "%1", "", "SessionStart", map[string]any{"source": "startup", "transcript_path": path})
	}
	r.paneHook(t, reviewsExited, dir, "%1", "", "SessionEnd", map[string]any{"reason": "prompt_input_exit"})
	ss := r.sessions(t)
	if _, listed := ss[reviewsNested]; listed {
		t.Errorf("session %s is listed after a session started in its pane", reviewsNested)
	}
}

// tmuxGoesDown kills the agents' tmux server and starts a new one on the
// same socket. Claude Code dies with its pane and reports SessionEnd with
// reason "other" (live check A); the stub would report the user's own exit
// when its input ends, so it is killed first, and the SessionEnd it cannot
// send is sent for it.
func (r *reviewsRun) tmuxGoesDown(t *testing.T) {
	t.Helper()
	socket := r.socket(t)
	pid, err := strconv.Atoi(strings.TrimSpace(r.srv.mustRun(t, "display-message", "-p", "-t", r.leadPane, "#{pane_pid}")))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	r.waitPaneGone(t, r.leadPane)
	r.srv.mustRun(t, "kill-server")
	r.agentHook(t, "SessionEnd", map[string]any{"reason": "other"})
	if _, listed := r.sessions(t)[tasksLead]; listed {
		t.Fatalf("T-1's agent is listed after its SessionEnd")
	}
	r.srv.mustRun(t, "new-session", "-d", "-s", "curia", "-x", "200", "-y", "50", "--", "/bin/sh", "-c", "while :; do sleep 60; done")
	r.srv.tmuxVar = strings.TrimSpace(r.srv.mustRun(t, "display-message", "-p", "-t", "curia:", "#{socket_path},#{pid},0"))
	r.h.env = envWith(r.h.env, "TMUX="+r.srv.tmuxVar)
	for _, kv := range r.h.env {
		name, value, _ := strings.Cut(kv, "=")
		if name == "PATH" || name == "HOME" || name == "CURIA_DB" || name == "CLAUDE_CONFIG_DIR" {
			r.srv.mustRun(t, "set-environment", "-g", name, value)
		}
	}
	r.srv.mustRun(t, "set-option", "-g", "default-size", "200x50")
	if got := r.socket(t); got != socket {
		t.Fatalf("the new server is on %s, want the old socket %s", got, socket)
	}
}

// wantPlan checks the dry run's plan: T-1's agent restored into window
// T-1, the nested session skipped, the exited one not shown.
func (r *reviewsRun) wantPlan(t *testing.T, plan string) {
	t.Helper()
	rows := map[string]string{}
	for line := range strings.Lines(plan) {
		if f := strings.Fields(line); len(f) > 0 {
			rows[f[0]] = line
		}
	}
	if row := rows["00000001"]; !strings.Contains(row, "api-server:T-1") || !strings.HasSuffix(row, "restore\n") {
		t.Errorf("curia restore --dry-run plan:\n%s\nwant 00000001 restored into api-server:T-1", plan)
	}
	if row := rows["55555555"]; !strings.Contains(row, "skip: ended without a recorded reason") {
		t.Errorf("curia restore --dry-run plan:\n%s\nwant 55555555 skipped as ended without a reason", plan)
	}
	if _, shown := rows["66666666"]; shown {
		t.Errorf("curia restore --dry-run plan:\n%s\nwant no row for the session the user exited", plan)
	}
}

// wantResumed waits for the stub's second start and compares it with
// stub_resume.golden.txt: --resume, the session id and no brief.
func (r *reviewsRun) wantResumed(t *testing.T) {
	t.Helper()
	path := filepath.Join(r.stubDir, "argv-T-1.txt")
	var got string
	for deadline := time.Now().Add(reviewsWait); strings.Count(got, "\ntask T-1\n") < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("argv-T-1.txt holds %d starts, want 2:\n%s", strings.Count(got, "\ntask T-1\n"), got)
		}
		if b, err := os.ReadFile(filepath.Clean(path)); err == nil {
			got = string(b)
		}
	}
	blocks := strings.SplitAfter(strings.ReplaceAll(got, r.worktree, reviewsBranchToken), "\ntask T-1\n")
	if want := r.reviewsGolden(t, "stub_resume.golden.txt"); len(blocks) != 3 || blocks[1] != want {
		t.Errorf("the stub's starts, normalized:\n%s\nwant the second (stub_resume.golden.txt):\n%s", got, want)
	}
}

// twoServers: a second private server's pane %1 shows Claude Code's idle
// prompt, like the restored agent's pane %1. A message sent through that
// server is not typed into it; sent through the agent's own server it is
// typed, and the one held back is handed over by the agent's next Stop.
func (r *reviewsRun) twoServers(t *testing.T) {
	t.Helper()
	if r.leadPane != "%1" {
		t.Fatalf("the restored agent is in %s, want %%1, the pane id the second server also has", r.leadPane)
	}
	other := startTmux(t, filepath.Join(r.tmp, "tmuxhome-b"), r.paneFile("b0"), r.paneFile("b1"), r.paneFile("b2"))
	env := envWith(r.h.env, "TMUX="+other.tmuxVar, "TMUX_TMPDIR="+other.dir)
	stdout, stderr, code := r.term.run(t, r.h, env, "send", "%1", "check the empty list case again")
	if want := "M-2 queued: the agent is on another tmux server (" + r.srv.name + "); nothing typed"; code != 0 || stderr != "" ||
		!strings.HasPrefix(stdout, want) {
		t.Errorf("curia send %%1 through the second server: exit %d, stdout %q, stderr %q, want %q...", code, stdout, stderr, want)
	}
	r.wantNothingTyped(t, 2)
	if got := readFile(t, r.paneFile("b1")); got != "" {
		t.Errorf("the second server's %%1 received %q, want nothing", got)
	}
	if got := r.user(t, "send", "%1", "back on the restored session."); got != "M-3 delivered (send_keys)\n" {
		t.Errorf("curia send %%1 through the agent's server = %q, want %q", got, "M-3 delivered (send_keys)\n")
	}
	r.waitTyped(t, r.inputLines(t, 3))
	for deadline := time.Now().Add(reviewsWait); r.message(t, "M-2").DeliveredVia != "stop_hook"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("M-2 = %+v, want it handed over by the Stop hook after M-3", r.message(t, "M-2"))
		}
	}
}

// Step 9: every review as stored, the agent's MCP responses (checked in
// step 7), nothing typed into another pane, no token in the log, no error,
// and curia serve stops on SIGTERM.
func (r *reviewsRun) finalState(t *testing.T) {
	t.Helper()
	r.wantGolden(t, "reviews.golden.json", r.h.mustRun(t, "reviews", "ls", "--status", "all", "--json"))
	for _, name := range []string{"p0", "p1", "p2", "b0", "b2"} {
		if got := readFile(t, r.paneFile(name)); got != "" {
			t.Errorf("pane %s received %q, want nothing", name, got)
		}
	}
	r.ntfy.wait(t, 4) // exactly R-1, D-2, D-3 and R-2
	r.stopServe(t)
	log := readFile(t, r.h.logPath)
	if strings.Contains(log, r.token) {
		t.Errorf("the log holds the one-time token")
	}
	checkNoErrors(t, r.h.logPath)
}

// checkAgentClient ends the agent's MCP client and compares its responses
// with client_agent.golden.jsonl.
func (r *reviewsRun) checkAgentClient(t *testing.T) {
	t.Helper()
	r.agent.close(t)
	want := strings.Split(strings.TrimSuffix(r.reviewsGolden(t, "client_agent.golden.jsonl"), "\n"), "\n")
	for i, line := range want {
		if line == toolsListLine {
			want[i] = compactGolden(t, r.root, "tools_list.golden.json")
		}
	}
	got := make([]string, len(r.agent.responses))
	for i, line := range r.agent.responses {
		got[i] = clientNormalise(line)
	}
	compareLines(t, r.agent.name, got, want)
}

// agentWorks is the agent's turn up to complete_task: a tool call, and a
// commit of path on its branch (made by the test, in the worktree).
func (r *reviewsRun) agentWorks(t *testing.T, path, content string) {
	t.Helper()
	r.agentHook(t, "PreToolUse", map[string]any{
		"permission_mode": "default", "tool_name": "Bash", "tool_input": map[string]string{"command": "git commit"},
	})
	tasksWrite(t, filepath.Join(r.worktree, path), content)
	r.git(t, r.worktree, "add", path)
	r.git(t, r.worktree, "commit", "-q", "-m", "add "+path)
}

// agentStops is the end of the agent's turn after complete_task.
func (r *reviewsRun) agentStops(t *testing.T) {
	t.Helper()
	r.agentHook(t, "Stop", map[string]any{
		"permission_mode": "default", "stop_hook_active": false, "last_assistant_message": "Ready for review.",
	})
}

// agentHook runs `curia hook event` for T-1's agent in its pane, with
// CURIA_TASK_ID as its window gives it.
func (r *reviewsRun) agentHook(t *testing.T, event string, fields map[string]any) {
	t.Helper()
	r.paneHook(t, tasksLead, r.worktree, r.leadPane, "T-1", event, fields)
}

// paneHook runs `curia hook event` for session id in pane, with cwd,
// CURIA_TASK_ID task (none when empty) and fields in the payload; it
// fails unless the hook exits 0 and prints nothing.
func (r *reviewsRun) paneHook(t *testing.T, id, cwd, pane, task, event string, fields map[string]any) {
	t.Helper()
	payload := map[string]any{"session_id": id, "cwd": cwd, "hook_event_name": event}
	maps.Copy(payload, fields)
	h := *r.h
	h.env = append(slices.Clone(r.h.env), "TMUX_PANE="+pane)
	if task != "" {
		h.env = append(h.env, "CURIA_TASK_ID="+task)
	}
	stdout, stderr, _, err := h.run(hookPayload(t, payload), "", "hook", event)
	if err != nil || stdout != "" || stderr != "" {
		t.Errorf("curia hook %s for %s: err %v, stdout %q, stderr %q, want exit 0 and no output", event, id, err, stdout, stderr)
	}
}

// wantMarker fails unless T-1's window has @curia_marker want ("" for none).
func (r *reviewsRun) wantMarker(t *testing.T, want, when string) {
	t.Helper()
	got := strings.TrimSpace(r.srv.mustRun(t, "show-options", "-wqv", "-t", r.leadPane, "@curia_marker"))
	if got != want {
		t.Errorf("@curia_marker of T-1's window %s = %q, want %q", when, got, want)
	}
}

// reviewsDecision is a ticket as `curia decisions ls --json` shows it.
type reviewsDecision struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AnswerKey   string `json:"answer_key"`
	AnsweredVia string `json:"answered_via"`
}

// decision is ticket id in `curia decisions ls --status all --json`.
func (r *reviewsRun) decision(t *testing.T, id string) reviewsDecision {
	t.Helper()
	out := r.h.mustRun(t, "decisions", "ls", "--status", "all", "--json")
	var ds []reviewsDecision
	if err := json.Unmarshal([]byte(out), &ds); err != nil {
		t.Fatalf("curia decisions ls --json: %v\n%s", err, out)
	}
	for _, d := range ds {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("curia decisions ls --json has no %s:\n%s", id, out)
	return reviewsDecision{}
}

// inputLines is the first n lines of pane_t1.golden.txt.
func (r *reviewsRun) inputLines(t *testing.T, n int) string {
	t.Helper()
	lines := slices.Collect(strings.Lines(r.reviewsGolden(t, "pane_t1.golden.txt")))
	if len(lines) < n {
		t.Fatalf("pane_t1.golden.txt has %d lines, want at least %d", len(lines), n)
	}
	return strings.Join(lines[:n], "")
}

// wantNothingTyped fails if, once a typing would have landed, the stub's
// input record holds more than the golden file's first n lines. It checks
// an absence, which no condition ends: curia send has already refused
// before it returned, so the pause only gives a wrong typing time to show,
// and a slow machine can make it pass wrongly, never fail.
func (r *reviewsRun) wantNothingTyped(t *testing.T, n int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got, want := readFile(t, r.inputFile()), r.inputLines(t, n); got != want {
		t.Errorf("input-T-1.txt = %q, want %q: nothing typed", got, want)
	}
}

// branchHead is the commit T-1's branch is at.
func (r *reviewsRun) branchHead(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(r.git(t, r.repo, "rev-parse", "refs/heads/"+tasksBranch))
}

// socket is the agents' server's socket path.
func (r *reviewsRun) socket(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(r.srv.mustRun(t, "display-message", "-p", "-t", "curia:", "#{socket_path}"))
}

func (r *reviewsRun) reviewsGolden(t *testing.T, name string) string {
	t.Helper()
	return readFile(t, filepath.Join(r.root, reviewsGolden, name))
}

// commitID matches a full commit id as a JSON string.
var commitID = regexp.MustCompile(`"[0-9a-f]{40}"`)

// wantGolden compares a JSON listing, normalized (times, the repository,
// commit ids), with golden file name.
func (r *reviewsRun) wantGolden(t *testing.T, name, out string) {
	t.Helper()
	got := commitID.ReplaceAllString(r.normalise(out), `"<commit>"`)
	if want := r.reviewsGolden(t, name); got != want {
		t.Errorf("listing, normalized:\n%s\nwant (%s):\n%s", got, name, want)
	}
}

// wantPush checks one request the stand-in ntfy received: a JSON POST to
// the base URL with no access token, whose body, with its buttons' one
// token put back as TOKEN, is golden file name. It returns that token, ""
// for a push with no button.
func (r *reviewsRun) wantPush(t *testing.T, got reviewsRequest, name string) string {
	t.Helper()
	if got.method != http.MethodPost || got.path != "/" || got.contentType != "application/json" || got.auth != "" {
		t.Errorf("push %s: %s %s, Content-Type %q, Authorization %q, want a JSON POST to / with no access token",
			name, got.method, got.path, got.contentType, got.auth)
	}
	var body struct {
		Actions []struct {
			Headers map[string]string `json:"headers"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("push %s: %v\n%s", name, err, got.body)
	}
	token := ""
	for _, a := range body.Actions {
		tok, _ := strings.CutPrefix(a.Headers["Authorization"], "Bearer ")
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(tok) || (token != "" && tok != token) {
			t.Errorf("push %s: button token %q, want one 43-character base64url token for every button", name, tok)
		}
		token = tok
	}
	norm := got.body
	if token != "" {
		norm = strings.ReplaceAll(norm, token, reviewsToken)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, []byte(r.reviewsGolden(t, name))); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if norm != want.String() {
		t.Errorf("push received, token normalized:\n%s\nwant (%s):\n%s", norm, name, want.String())
	}
	return token
}

// reviewsServe is the curia serve the test started.
type reviewsServe struct {
	base           string // http://<the address it listens on>
	pid            int
	out, err, code string // the files of its stdout, stderr and exit code
}

// reviewsServeScript starts curia serve at the terminal, in the background
// of the window's shell so the test can signal it: its pid goes to E2E_PID
// and its exit code to E2E_RC, each by a rename.
const reviewsServeScript = `PATH=$E2E_PATH; export PATH
TMUX=$E2E_TMUX; export TMUX; unset TMUX_PANE E2E_TMUX E2E_PATH
"$0" serve --listen 127.0.0.1:0 </dev/null >"$E2E_OUT" 2>"$E2E_ERR" &
echo $! >"$E2E_PID.part" && mv "$E2E_PID.part" "$E2E_PID"
wait $!
echo $? >"$E2E_RC.part" && mv "$E2E_RC.part" "$E2E_RC"`

// serveListening is curia serve's first line.
var serveListening = regexp.MustCompile(`^curia serve: listening on (127\.0\.0\.1:[0-9]+)\n`)

// startServe runs curia serve on 127.0.0.1, port 0, at a terminal, and
// waits for it to listen.
func (r *reviewsRun) startServe(t *testing.T) reviewsServe {
	t.Helper()
	base := filepath.Join(r.term.dir, "serve")
	s := reviewsServe{out: base + ".out", err: base + ".err", code: base + ".rc"}
	r.window(t, reviewsServeScript, []string{"E2E_OUT=" + s.out, "E2E_ERR=" + s.err, "E2E_PID=" + base + ".pid", "E2E_RC=" + s.code})
	for deadline := time.Now().Add(terminalDeadline); ; time.Sleep(20 * time.Millisecond) {
		if exists(s.code) {
			t.Fatalf("curia serve exited %s: stdout %q, stderr %q", readFile(t, s.code), readFile(t, s.out), readFile(t, s.err))
		}
		if m := serveListening.FindStringSubmatch(reviewsRead(s.out)); m != nil && exists(base+".pid") {
			s.base = "http://" + m[1]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("curia serve did not listen within %s: stdout %q", terminalDeadline, reviewsRead(s.out))
		}
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, base+".pid")))
	if err != nil {
		t.Fatal(err)
	}
	s.pid = pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return s
}

// stopServe sends curia serve SIGTERM and requires it to exit 0 within
// reviewsServeStop with nothing on stderr.
func (r *reviewsRun) stopServe(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(r.serve.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(reviewsServeStop); !exists(r.serve.code); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("curia serve did not exit within %s of SIGTERM", reviewsServeStop)
		}
	}
	if code, stderr := strings.TrimSpace(readFile(t, r.serve.code)), readFile(t, r.serve.err); code != "0" || stderr != "" {
		t.Errorf("curia serve after SIGTERM: exit %s, stderr %q, want 0 and nothing", code, stderr)
	}
}

// reviewsViewScript runs "$0" "$@" with the terminal attached, then says
// how it exited and keeps the window, so its last screen can be read.
const reviewsViewScript = `PATH=$E2E_PATH; export PATH
TMUX=$E2E_TMUX; export TMUX; unset TMUX_PANE E2E_TMUX E2E_PATH
"$0" "$@"
echo "curia exited $?"
exec sleep 600`

// view runs curia with args on the terminal of a new 200 by 50 window of
// the terminal server, and returns its pane.
func (r *reviewsRun) view(t *testing.T, args ...string) string {
	t.Helper()
	pane := r.window(t, reviewsViewScript, nil, args...)
	r.term.srv.mustRun(t, "resize-window", "-t", pane, "-x", "200", "-y", "50")
	return pane
}

// window opens a window of the terminal server running script with the
// run's environment as terminal.run passes it, plus extra, and returns its
// pane.
func (r *reviewsRun) window(t *testing.T, script string, extra []string, args ...string) string {
	t.Helper()
	cmd := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "term", "-c", r.h.dir}
	tmuxVar := ""
	for _, kv := range r.h.env {
		name, value, _ := strings.Cut(kv, "=")
		switch name {
		case "TMUX":
			tmuxVar = value
		case "TMUX_PANE":
		case "PATH":
			cmd = append(cmd, "-e", "E2E_PATH="+value)
		default:
			cmd = append(cmd, "-e", kv)
		}
	}
	cmd = append(cmd, "-e", "E2E_TMUX="+tmuxVar)
	for _, kv := range extra {
		cmd = append(cmd, "-e", kv)
	}
	cmd = append(append(cmd, "--", "/bin/sh", "-c", script, r.h.bin), args...)
	return strings.TrimSpace(r.term.srv.mustRun(t, cmd...))
}

// keys types key into the terminal server's pane.
func (r *reviewsRun) keys(t *testing.T, pane, key string) {
	t.Helper()
	r.term.srv.mustRun(t, "send-keys", "-t", pane, "-l", "--", key)
}

// screen waits up to terminalDeadline for the terminal server's pane to
// show one of wants, and returns the screen, wrapped lines joined.
func (r *reviewsRun) screen(t *testing.T, pane string, wants ...string) string {
	t.Helper()
	var got string
	for deadline := time.Now().Add(terminalDeadline); ; time.Sleep(20 * time.Millisecond) {
		got, _ = r.term.srv.run("capture-pane", "-p", "-J", "-t", pane)
		if slices.ContainsFunc(wants, func(w string) bool { return strings.Contains(got, w) }) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never showed %q; it shows:\n%s", pane, wants, got)
		}
	}
}

// reviewsRead is the file at path, or "" when it cannot be read yet.
func reviewsRead(path string) string {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ""
	}
	return string(b)
}

// reviewsHTTP is the client the test answers with: loopback only, no proxy.
var reviewsHTTP = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}

// reviewsPost POSTs body as JSON to target with the bearer token, as a push
// button does, and returns the status and body.
func reviewsPost(t *testing.T, target, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, reviewsLoopback(t, target), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return reviewsDo(t, req)
}

// reviewsGet GETs target and returns the status and body.
func reviewsGet(t *testing.T, target string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, reviewsLoopback(t, target), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	return reviewsDo(t, req)
}

func reviewsDo(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := reviewsHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// reviewsLoopback fails the test unless target is an http URL on 127.0.0.1,
// and returns it.
func reviewsLoopback(t *testing.T, target string) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Fatalf("%q is not an http URL on 127.0.0.1", target)
	}
	return u.String()
}

// reviewsRequest is one request the stand-in ntfy received.
type reviewsRequest struct {
	method, path, contentType, auth, body string
}

// reviewsNtfy stands in for an ntfy server: an httptest server on
// 127.0.0.1 that records every request and answers each with the reply
// ntfy 2.38.0 gave to an accepted publish.
type reviewsNtfy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []reviewsRequest
}

func startReviewsNtfy(t *testing.T, reply string) *reviewsNtfy {
	t.Helper()
	n := &reviewsNtfy{}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(req.Body, 64<<10))
		n.mu.Lock()
		n.reqs = append(n.reqs, reviewsRequest{
			method: req.Method, path: req.URL.Path, contentType: req.Header.Get("Content-Type"),
			auth: req.Header.Get("Authorization"), body: string(b),
		})
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(n.srv.Close)
	if u, err := url.Parse(n.srv.URL); err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatalf("the stand-in ntfy is on %s, want 127.0.0.1", n.srv.URL)
	}
	return n
}

// wait waits up to reviewsWait for the stand-in to hold want requests,
// logs how long that took, and returns them; it fails when it holds more,
// or fewer once the wait is over.
func (n *reviewsNtfy) wait(t *testing.T, want int) []reviewsRequest {
	t.Helper()
	began := time.Now()
	for deadline := began.Add(reviewsWait); ; time.Sleep(20 * time.Millisecond) {
		n.mu.Lock()
		got := slices.Clone(n.reqs)
		n.mu.Unlock()
		switch {
		case len(got) > want:
			t.Fatalf("the stand-in ntfy holds %d requests, want %d", len(got), want)
		case len(got) == want:
			t.Logf("the stand-in ntfy held %d requests after %v", want, time.Since(began).Round(time.Millisecond))
			return got
		case time.Now().After(deadline):
			t.Fatalf("the stand-in ntfy holds %d requests after %v, want %d", len(got), reviewsWait, want)
		}
	}
}

// waitTyped waits up to reviewsWait for the stub's input record to hold
// exactly want (Plan 3's waitInput, with a deadline for a loaded runner).
func (r *reviewsRun) waitTyped(t *testing.T, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(reviewsWait); got != want; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("input-T-1.txt after %v:\n%q\nwant:\n%q", reviewsWait, got, want)
		}
		got = reviewsRead(r.inputFile())
	}
}

// reviewsStatSources stats every fixture and golden file of this test and
// the stub it borrows from Plan 3's, so a changed one cannot reuse a cached
// pass (build stats the binary's sources, ntfy's recorded reply included).
func reviewsStatSources(t *testing.T, root string) {
	t.Helper()
	tasksStatSources(t, root)
	for _, dir := range []string{reviewsFixtures, reviewsGolden} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			_, err = os.Stat(path)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The Stop hook's cost with a store that holds Plan 4's rows.
const (
	latencyReviews  = 100
	latencyReviewer = "00000000-0000-4000-9000-000000000001" // T-1's session, timed
)

// TestStopHookLatencyWithReviews is the latency gate of Plan 4's script: on
// a fresh store with no tmux, latencyReviews tasks each with a pending
// review opened by complete_task, and their pushes all sent to a stand-in
// ntfy;
// then, after one untimed warm-up, at least minTimedRuns sequential `curia
// hook Stop` processes for the session of T-1 are timed, stdio as files.
// Their p95 must be under p95Budget.
func TestStopHookLatencyWithReviews(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	h := newHome(t, build(t, root))
	ntfy := startReviewsNtfy(t, readFile(t, filepath.Join(root, reviewsReply)))
	cfg := strings.ReplaceAll(readFile(t, filepath.Join(root, reviewsFixtures, "config.toml.tmpl")), "<ntfy_url>", ntfy.srv.URL)
	tasksWrite(t, filepath.Join(h.dir, "home", ".config", "curia", "config.toml"), cfg)
	repo := filepath.Join(h.dir, "repo")
	gitInit(t, repo, h.dir)
	for i := range latencyReviews {
		tasksWrite(t, filepath.Join(repo, "backlog", "tasks", fmt.Sprintf("task-%d - Latency task %d.md", i+1, i+1)),
			fmt.Sprintf("---\nid: task-%d\ntitle: Latency task %d\nstatus: To Do\n---\n\nWork.\n", i+1, i+1))
	}
	if out := startTerminal(t).mustRun(t, h, "task", "import", "--backlog-md", repo); !strings.Contains(out, strconv.Itoa(latencyReviews)) {
		t.Fatalf("curia task import printed %q, want %d tasks imported", out, latencyReviews)
	}
	for i := range latencyReviews {
		latencyComplete(t, h, repo, i+1)
	}
	latencyFlush(t, h, repo, ntfy)
	stop := hookPayload(t, map[string]any{
		"session_id": latencyReviewer, "cwd": repo, "hook_event_name": "Stop",
		"stop_hook_active": false, "last_assistant_message": "Ready for review.",
	})
	timed := *h
	timed.env = append(slices.Clone(h.env), "CURIA_TASK_ID=T-1")
	timed.timedStop(t, stop) // warm-up, untimed
	var runs []time.Duration
	for len(runs) < minTimedRuns {
		runs = append(runs, timed.timedStop(t, stop))
	}
	t.Logf("%-12s %s (load average %s)", "Stop", stats(runs), loadAverage())
	if p := percentile(runs, 95); p >= p95Budget {
		t.Errorf("p95 wall time of curia hook Stop with %d pending reviews = %v, want under %v (load average %s)",
			latencyReviews, p, p95Budget, loadAverage())
	}
	if got, want := h.mustRun(t, "status"), "R"+strconv.Itoa(latencyReviews); got != want {
		t.Errorf("curia status after the pass = %q, want %q", got, want)
	}
	checkNoErrors(t, h.logPath)
}

// latencyFlush makes sure every push has been sent. Each setup curia mcp
// stops right after its one call, so its push may still be queued when it
// ends, held for the next Curia process (section 4.7). One more curia mcp,
// which flushes when it starts, is kept open until the stand-in holds
// exactly one push per review.
func latencyFlush(t *testing.T, h *home, repo string, ntfy *reviewsNtfy) {
	t.Helper()
	c := startClient(t, h, "flush", repo, nil, slices.Clone(h.env))
	ntfy.wait(t, latencyReviews)
	c.close(t)
}

// latencyComplete starts the session of task T-n, with CURIA_TASK_ID as
// curia launch sets it, and has it call complete_task through its own
// `curia mcp`.
func latencyComplete(t *testing.T, h *home, repo string, n int) {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-9000-%012d", n)
	task := "T-" + strconv.Itoa(n)
	start := hookPayload(t, map[string]any{"session_id": id, "cwd": repo, "hook_event_name": "SessionStart", "source": "startup"})
	if _, stderr, _, err := h.run(start, "CURIA_TASK_ID="+task, "hook", "SessionStart"); err != nil || stderr != "" {
		t.Fatalf("SessionStart of %s: %v %q", task, err, stderr)
	}
	call := string(hookPayload(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "complete_task", "arguments": map[string]any{"summary": "Done.", "tests_status": "passed"}},
	}))
	c := startClient(t, h, "latency", repo, []string{call}, append(slices.Clone(h.env), "CURIA_SESSION_ID="+id))
	if resp, want := c.call(t), fmt.Sprintf(`\"review\":\"R-%d\",\"task\":\"%s\"`, n, task); !strings.Contains(resp, want) {
		t.Fatalf("complete_task for %s = %s, want R-%d", task, resp, n)
	}
	c.close(t)
}

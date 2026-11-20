package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Plan 3 acceptance test: the built binary as the user, tmux and Claude
// Code would run it, against real git repositories, a real SQLite file, a
// private tmux server and a stub `claude` (testdata/tasks/claude-stub.sh)
// that stands in for the agent in the windows curia spawn opens, and for the
// router model when called with -p.

// The sessions of the script: the stub derives T-1's and T-2's from their
// task ids; the other two are started by hook in panes of the test's server.
const (
	tasksLead   = "00000001-0000-4000-8000-000000000001" // T-1's agent, the team's lead
	tasksSecond = "00000002-0000-4000-8000-000000000002" // T-2's agent
	tasksWeb    = "77777777-7777-4777-8777-777777777777" // an agent in another repository, in %1
	tasksMate   = "88888888-8888-4888-8888-888888888888" // the lead's teammate, in %0
	tasksTeam   = "session-00000001"                     // the team's directory (Ruling 98)
	tasksBranch = "curia/T-1-fix-the-auth-refresh"
)

// Tokens of the fixtures and golden files.
const (
	tasksWorktreeToken = "<worktree>"
	tasksWebToken      = "<web>"
	tasksLeadToken     = "<lead>"
)

// tasksBody is T-1's brief body, as written to the --body file.
const tasksBody = "The refresh endpoint returns 500 once the access token has expired.\n" +
	"Rotate the refresh token on every use and add a test for the expired case.\n"

// tasksFixtures and tasksGolden are the test's fixture and golden
// directories, relative to the repository root.
var (
	tasksFixtures = filepath.Join("testdata", "tasks")
	tasksGolden   = filepath.Join("e2e", "testdata", "tasks")
)

// TestTasksEndToEnd runs steps 1 to 14 of Plan 3's acceptance script, as
// built: tasks added and spawned (worktree, branch, window, brief, setup
// script approved through the controlling terminal), messages typed into
// the idle stub, handed over at a Stop and fetched by check_messages, a
// standing rule found as precedent, a task filed by message, curia send and
// read between two agent sessions, one message routed by the stub router,
// a team config making two panes one unit, then pause, resume and clean.
func TestTasksEndToEnd(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	tasksStatSources(t, root)
	for _, tool := range []string{"tmux", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not installed: the acceptance test needs it", tool)
		}
	}
	r := newTasksRun(t, root, build(t, root))

	r.addTasks(t)            // 1
	r.approveSetup(t)        // the setup script is run only once approved (Rulings 108, 109)
	r.spawnFirst(t)          // 2
	r.spawnAgainRefused(t)   // 3
	r.typedIntoIdlePane(t)   // 4
	r.deliveredByStopHook(t) // 5
	r.checkMessages(t)       // check_messages (Task 26)
	r.ruleAsPrecedent(t)     // 6
	r.taskByMessage(t)       // 7
	r.agentSendAndRead(t)    // 8
	r.routedByModel(t)       // 9
	r.team(t)                // 10
	r.pause(t)               // 11
	r.resume(t)              // 12
	r.clean(t)               // 13
	r.finalState(t)          // 14
}

// tasksRun is one run of the script.
type tasksRun struct {
	h        *home
	srv      *tmuxServer
	root     string // the curia repository
	tmp      string
	repo     string   // api-server, as created
	repos    []string // api-server symlink-resolved, then as created
	parents  []string // their parent directories, in the same order
	web      string   // web-app, symlink-resolved
	worktree string   // T-1's worktree, symlink-resolved (as git reports it)
	stubDir  string   // claude, curia and what the stub records
	panes    string   // where the test server's panes record what they receive
	hooks    []replayLine
	leadPane string
	lead     *mcpClient
	mate     *mcpClient
	// term is where the user's commands run: they need a controlling
	// terminal (Ruling 112).
	term *terminal
}

func newTasksRun(t *testing.T, root, bin string) *tasksRun {
	t.Helper()
	tmp := t.TempDir()
	r := &tasksRun{h: newHome(t, bin), root: root, tmp: tmp, repo: filepath.Join(tmp, "src", "api-server")}
	r.stubDir, r.panes = filepath.Join(tmp, "bin"), filepath.Join(tmp, "panes")
	webPlain := filepath.Join(tmp, "src", "web-app")
	for _, d := range []string{r.repo, webPlain, r.stubDir, r.panes, filepath.Join(tmp, "claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.makeRepos(t, webPlain)
	r.makeStub(t, bin)
	r.srv = startTmux(t, filepath.Join(tmp, "tmuxhome"), r.paneFile("mate"), r.paneFile("web"), r.paneFile("spare"))
	r.setEnv(t)
	r.hooks = r.readHooks(t)
	r.term = startTerminal(t)
	return r
}

// makeRepos creates api-server (one commit on main: a README and an
// executable .curia/setup.sh) and web-app (one commit on main), and records
// their path forms.
func (r *tasksRun) makeRepos(t *testing.T, webPlain string) {
	t.Helper()
	setup := readFile(t, filepath.Join(r.root, tasksFixtures, "setup.sh"))
	for _, dir := range []string{r.repo, webPlain} {
		gitInit(t, dir, r.tmp)
		r.git(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
		// The identity is the repository's, so curia's own commit (pause)
		// finds it too.
		r.git(t, dir, "config", "user.name", "Curia Test")
		r.git(t, dir, "config", "user.email", "test@example.com")
		tasksWrite(t, filepath.Join(dir, "README.md"), "# "+filepath.Base(dir)+"\n")
	}
	tasksWrite(t, filepath.Join(r.repo, ".curia", "setup.sh"), setup)
	r.git(t, r.repo, "add", "-A")
	// The execute bit goes through the index, so the test writes no
	// executable file: the worktree's checkout makes it one.
	r.git(t, r.repo, "update-index", "--chmod=+x", ".curia/setup.sh")
	r.git(t, r.repo, "commit", "-q", "-m", "init")
	r.git(t, webPlain, "add", "-A")
	r.git(t, webPlain, "commit", "-q", "-m", "init")
	resolved, err := filepath.EvalSymlinks(r.repo)
	if err != nil {
		t.Fatal(err)
	}
	if r.web, err = filepath.EvalSymlinks(webPlain); err != nil {
		t.Fatal(err)
	}
	r.repos = []string{resolved, r.repo}
	r.parents = []string{filepath.Dir(resolved), filepath.Dir(r.repo)}
	r.worktree = filepath.Join(filepath.Dir(resolved), ".curia-worktrees", "api-server-T-1")
}

// makeStub fills the stub directory: claude, a link to the committed stub
// script (so the test writes no executable file), curia, a link to the
// binary, the screen the stub draws and the router's envelope.
func (r *tasksRun) makeStub(t *testing.T, bin string) {
	t.Helper()
	links := map[string]string{
		"claude": filepath.Join(r.root, tasksFixtures, "claude-stub.sh"),
		"curia":  bin,
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(r.stubDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	tasksWrite(t, filepath.Join(r.stubDir, "screen.txt"), claudePrompt)
	tasksWrite(t, filepath.Join(r.stubDir, "router-envelope.json"),
		readFile(t, filepath.Join(r.root, tasksFixtures, "router-envelope.json")))
}

// setEnv points every run of the binary, and every process the private
// server starts (the spawned windows, so curia launch and the stub), at the
// test's home, database, Claude directory and stub directory.
func (r *tasksRun) setEnv(t *testing.T) {
	t.Helper()
	path := r.stubDir + string(os.PathListSeparator) + os.Getenv("PATH")
	keep := slices.DeleteFunc(slices.Clone(r.h.env), func(kv string) bool {
		return strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "TMUX_TMPDIR=")
	})
	keep = append(keep, "PATH="+path, "CLAUDE_CONFIG_DIR="+filepath.Join(r.tmp, "claude"),
		"TMUX_TMPDIR="+r.srv.dir, "TMUX="+r.srv.tmuxVar)
	r.h.env = keep
	for _, kv := range r.h.env {
		name, value, _ := strings.Cut(kv, "=")
		if name == "PATH" || name == "HOME" || name == "CURIA_DB" || name == "CLAUDE_CONFIG_DIR" {
			r.srv.mustRun(t, "set-environment", "-g", name, value)
		}
	}
	// Plan 2's panes are 200 by 50; so are the windows curia spawn opens.
	r.srv.mustRun(t, "set-option", "-g", "default-size", "200x50")
}

// paneFile is the file the test server's pane for name records into.
func (r *tasksRun) paneFile(name string) string { return filepath.Join(r.panes, "pane_"+name+".txt") }

// Step 1: two tasks; T-1 made ready.
func (r *tasksRun) addTasks(t *testing.T) {
	t.Helper()
	body := filepath.Join(r.tmp, "body.md")
	tasksWrite(t, body, tasksBody)
	if got := r.user(t, "task", "add", "Fix the auth refresh", "--repo", r.repo, "--body", body); got != "T-1 added (backlog)\n" {
		t.Errorf("curia task add = %q, want %q", got, "T-1 added (backlog)\n")
	}
	if got := r.user(t, "task", "add", "Add request logging", "--repo", r.repo); got != "T-2 added (backlog)\n" {
		t.Errorf("curia task add = %q, want %q", got, "T-2 added (backlog)\n")
	}
	if got := r.user(t, "task", "ready", "T-1"); got != "T-1 ready\n" {
		t.Errorf("curia task ready T-1 = %q, want %q", got, "T-1 ready\n")
	}
	r.wantTasks(t, map[string]string{"T-1": "ready", "T-2": "backlog"})
	r.wantBadge(t, "", "after adding tasks")
}

// approveSetup: T-2's spawn skips the unapproved setup script; the user
// approves it with curia setup approve, typing the start of its hash on the
// controlling terminal (a window of the test server); then T-2's agent
// exits, as Claude Code does when the user leaves it.
func (r *tasksRun) approveSetup(t *testing.T) {
	t.Helper()
	out := r.user(t, "spawn", "T-2")
	if !strings.Contains(out, "Setup: .curia/setup.sh skipped: this content is not approved") {
		t.Errorf("curia spawn T-2 = %q, want the unapproved setup script skipped", out)
	}
	if exists(filepath.Join(r.h.dir, "home", "setup-ran")) {
		t.Errorf("the unapproved setup script ran")
	}
	pane := r.spawnedPane(t, "T-2", out)
	r.wantStates(t, map[string]string{tasksSecond: "idle"})

	sum := sha256.Sum256([]byte(readFile(t, filepath.Join(r.root, tasksFixtures, "setup.sh"))))
	approveOut := filepath.Join(r.tmp, "approve.txt")
	win := strings.TrimSpace(r.srv.mustRun(t, "new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "curia", "--",
		"/bin/sh", "-c", `"$0" setup approve T-2 >"$1" 2>&1`, r.h.bin, approveOut))
	r.waitScreen(t, win, "type the first 8 characters of its SHA-256")
	r.srv.mustRun(t, "send-keys", "-t", win, "-l", "--", hex.EncodeToString(sum[:])[:8])
	r.srv.mustRun(t, "send-keys", "-t", win, "Enter")
	r.waitPaneGone(t, win)
	if got := readFile(t, approveOut); !strings.Contains(got, "\nApproved for ") || !strings.Contains(got, "SHA-256: "+hex.EncodeToString(sum[:])) {
		t.Errorf("curia setup approve T-2 printed:\n%s\nwant the script's hash shown and the approval", got)
	}

	// T-2's agent goes away with its pane: its session is left stale, and
	// from now on no message may be routed to it (I1).
	r.srv.mustRun(t, "kill-pane", "-t", pane)
	r.waitPaneGone(t, pane)
	r.wantStates(t, map[string]string{tasksSecond: "stale"})
	r.wantTasks(t, map[string]string{"T-1": "ready", "T-2": "ready"})
}

// Step 2: T-1 runs in its own worktree on its branch, after its approved
// setup script ran there, with the brief as the stub's one argument.
func (r *tasksRun) spawnFirst(t *testing.T) {
	t.Helper()
	out := r.user(t, "spawn", "T-1")
	r.leadPane = r.spawnedPane(t, "T-1", out)
	if !strings.Contains(out, "Setup: ran .curia/setup.sh (log: ") {
		t.Errorf("curia spawn T-1 = %q, want the approved setup script run", out)
	}
	if got, want := readFile(t, filepath.Join(r.h.dir, "home", "setup-ran")), "T-1 "+r.worktree+"\n"; got != want {
		t.Errorf("setup-ran = %q, want %q", got, want)
	}
	if got := strings.TrimSpace(r.git(t, r.worktree, "rev-parse", "--abbrev-ref", "HEAD")); got != tasksBranch {
		t.Errorf("worktree %s is on %q, want %q", r.worktree, got, tasksBranch)
	}
	r.wantArgv(t, 1)
	r.wantTasks(t, map[string]string{"T-1": "running", "T-2": "ready"})
	s := r.sessions(t)[tasksLead]
	if s.TaskID != "T-1" || s.Repo != r.repos[0] || s.Worktree != r.worktree || s.Pane != r.leadPane {
		t.Errorf("curia ls --json shows T-1's session as %+v, want task T-1, repo %s, worktree %s, pane %s",
			s, r.repos[0], r.worktree, r.leadPane)
	}
	r.wantBadge(t, "", "after spawning T-1")
	// The agent in another repository, idle in %1.
	r.quietHook(t, "SessionStart", "%1")
}

// spawnedPane checks spawn's last line for task id and returns its pane.
func (r *tasksRun) spawnedPane(t *testing.T, id, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	last := lines[len(lines)-1]
	n := strings.TrimPrefix(id, "T-")
	want := regexp.MustCompile(`^` + id + ` running in api-server:` + id + ` \((%[0-9]+)\), session 0000000` + n + `$`)
	m := want.FindStringSubmatch(last)
	if m == nil {
		t.Fatalf("curia spawn %s printed:\n%s\nwant its last line to match %s", id, out, want)
	}
	return m[1]
}

// Step 3: a second spawn of T-1 is refused and names the window.
func (r *tasksRun) spawnAgainRefused(t *testing.T) {
	t.Helper()
	stderr := r.fails(t, nil, "spawn", "T-1")
	if !strings.Contains(stderr, "api-server:T-1 ("+r.leadPane+")") {
		t.Errorf("second curia spawn T-1: stderr %q, want it to name the window api-server:T-1 (%s)", stderr, r.leadPane)
	}
}

// Step 4: an addressed message is typed into the idle stub's prompt, and its
// submission confirms it.
func (r *tasksRun) typedIntoIdlePane(t *testing.T) {
	t.Helper()
	if got := r.user(t, "say", "T-1: use pnpm, not npm"); got != "M-1 delivered (send_keys)\n" {
		t.Errorf("curia say T-1: = %q, want %q", got, "M-1 delivered (send_keys)\n")
	}
	r.waitInput(t, r.goldenInputLines(t, 1))
	m := r.message(t, "M-1")
	if m.Status != "delivered" || m.DeliveredVia != "send_keys" || m.ConfirmedAt == nil {
		t.Errorf("M-1 = %+v, want delivered by send_keys and confirmed", m)
	}
	r.wantBadge(t, "", "after M-1 was submitted")
}

// Step 5: a message to a working session waits, is counted in the badge,
// and blocks its next Stop exactly once.
func (r *tasksRun) deliveredByStopHook(t *testing.T) {
	t.Helper()
	r.quietHook(t, "PreToolUse", tasksLeadToken)
	out := r.user(t, "say", "@api-server keep the public API unchanged (v1)")
	if !strings.HasPrefix(out, "M-2 queued") || strings.Count(out, "\n") != 1 {
		t.Errorf("curia say @api-server = %q, want one line starting %q", out, "M-2 queued")
	}
	r.wantNothingTyped(t, 1)
	r.wantBadge(t, "Q1", "while M-2 waits")
	if got, want := r.hook(t, "Stop", tasksLeadToken), r.golden(t, "stop_t1.golden.json"); got != want {
		t.Errorf("curia hook Stop for T-1 printed:\n%q\nwant (stop_t1.golden.json):\n%q", got, want)
	}
	r.quietHook(t, "Stop", tasksLeadToken)
	r.wantBadge(t, "", "after the Stop delivered M-2")
	r.wantStates(t, map[string]string{tasksLead: "idle"})
}

// checkMessages: a message queued while the agent works is fetched by its
// own check_messages, with the line typed earlier shown again; its next
// Stop has nothing to deliver.
func (r *tasksRun) checkMessages(t *testing.T) {
	t.Helper()
	r.quietHook(t, "PreToolUse", tasksLeadToken)
	if out := r.user(t, "say", "T-1: also run the linter"); !strings.HasPrefix(out, "M-3 queued") {
		t.Errorf("curia say T-1: = %q, want a line starting %q", out, "M-3 queued")
	}
	r.lead = r.startClient(t, "client_lead", r.leadPane)
	r.lead.handshake(t)
	r.lead.call(t) // check_messages: M-3 new, M-1 typed earlier
	r.quietHook(t, "Stop", tasksLeadToken)
	r.wantNothingTyped(t, 1)
	r.wantBadge(t, "", "after check_messages")
}

// Step 6: a rule by message, found as precedent by the agent.
func (r *tasksRun) ruleAsPrecedent(t *testing.T) {
	t.Helper()
	out := r.user(t, "say", "@api-server rule: always run make check before committing")
	if want := "M-4 stored as rule D-1 (repo api-server)\n"; out != want {
		t.Errorf("curia say rule: = %q, want %q", out, want)
	}
	r.lead.call(t) // search_decisions "make check committing": D-1, a rule
}

// Step 7: a task by message.
func (r *tasksRun) taskByMessage(t *testing.T) {
	t.Helper()
	out := r.user(t, "say", "@api-server task: add a rate limiter to the login route")
	if want := "M-5 filed as T-3 (backlog)\n"; out != want {
		t.Errorf("curia say task: = %q, want %q", out, want)
	}
	r.wantTasks(t, map[string]string{"T-1": "running", "T-2": "ready", "T-3": "backlog"})
}

// Step 8: inside an agent session say is refused; send from the agent in
// %1 is typed as another agent's; a pane with no session is refused; read
// prints the lead's pane.
func (r *tasksRun) agentSendAndRead(t *testing.T) {
	t.Helper()
	agent := []string{"CLAUDECODE=1", "TMUX_PANE=%1"}
	if stderr := r.fails(t, agent, "say", "T-1: hello"); !strings.Contains(stderr, "inside an agent session") {
		t.Errorf("curia say with CLAUDECODE=1: stderr %q, want the agent refusal", stderr)
	}
	// The sender is read from its processes (Ruling 113): the send runs
	// under the web-app agent's pane process, as a tool of that agent would.
	stdout, stderr, code := r.sendFromPane(t, "%1", r.leadPane, "tests are green")
	if code != 0 || stderr != "" || stdout != "M-6 delivered (send_keys)\n" {
		t.Errorf("curia send %s from %%1: stdout %q, stderr %q, exit %d, want %q", r.leadPane, stdout, stderr, code,
			"M-6 delivered (send_keys)\n")
	}
	r.waitInput(t, r.goldenInputLines(t, 2))
	if m := r.message(t, "M-6"); m.Sender != "agent" || m.SenderSession != tasksWeb || m.ConfirmedAt == nil {
		t.Errorf("M-6 = %+v, want sent by agent %s and confirmed", m, tasksWeb)
	}
	// An agent writes no task and spawns only what the user marked ready
	// (Ruling 113): T-3 is backlog.
	if stderr := r.fails(t, agent, "task", "add", "an agent's task", "--repo", r.repo); !strings.Contains(stderr, "only the user may") {
		t.Errorf("curia task add from an agent: stderr %q, want the user-only refusal", stderr)
	}
	if stderr := r.fails(t, agent, "spawn", "T-3"); !strings.Contains(stderr, "an agent may spawn only a task the user marked ready") {
		t.Errorf("curia spawn T-3 from an agent: stderr %q, want the refusal of a backlog task", stderr)
	}
	r.fails(t, agent, "send", "%99", "x")
	r.fails(t, nil, "read", "%99")
	lines := strings.Split(strings.TrimSuffix(r.h.mustRun(t, "read", r.leadPane, "--lines", "5"), "\n"), "\n")
	if len(lines) != 5 || lines[4] != "  ⏵⏵ auto mode on (shift+tab to cycle)" || strings.ContainsAny(strings.Join(lines, ""), "\x1b\r") {
		t.Errorf("curia read %s --lines 5 = %q, want the last five lines of the prompt, cleaned", r.leadPane, lines)
	}
	if got := r.message(t, "M-7"); got.ID != "" {
		t.Errorf("a refused say or send stored %+v", got)
	}
}

// Step 9: no addressed message called the router; an unaddressed one calls
// it once, with --safe-mode and no tmux or task variable, and without a
// terminal is not sent.
func (r *tasksRun) routedByModel(t *testing.T) {
	t.Helper()
	record := filepath.Join(r.stubDir, "router.txt")
	if exists(record) {
		t.Errorf("the router was called by an addressed message:\n%s", readFile(t, record))
	}
	before := r.h.mustRun(t, "ls", "--json")
	stderr := r.userFails(t, "say", "please look at the flaky test")
	if !strings.Contains(stderr, "M-7 not sent") || !strings.Contains(stderr, "address it, for example @api-server") {
		t.Errorf("unaddressed curia say: stderr %q, want M-7 not sent and the advice to address it", stderr)
	}
	calls := strings.Count(readFile(t, record), "\nend\n")
	got := readFile(t, record)
	if calls != 1 || !strings.Contains(got, "\narg --safe-mode\n") {
		t.Errorf("router record:\n%s\nwant one call with --safe-mode", got)
	}
	for _, name := range []string{"TMUX", "TMUX_PANE", "CURIA_TASK_ID", "CLAUDECODE", "CURIA_DB"} {
		if strings.Contains(got, "\nenv "+name+"=") {
			t.Errorf("the router's environment holds %s:\n%s", name, got)
		}
	}
	if prompt := readFile(t, filepath.Join(r.stubDir, "router-prompt.txt")); !strings.Contains(prompt, `"please look at the flaky test"`) {
		t.Errorf("router prompt %q does not hold the message as a JSON string", prompt)
	}
	if after := r.h.mustRun(t, "ls", "--json"); after != before {
		t.Errorf("the router call changed the sessions:\nbefore %s\nafter %s", before, after)
	}
}

// Step 10: a team config makes the lead's pane and its teammate's one unit;
// only the lead raises a ticket.
func (r *tasksRun) team(t *testing.T) {
	t.Helper()
	// The teammate's pane is a split of the lead's window, as Claude Code
	// makes it (probe P5): a team config names a session in another window,
	// or on another task, nobody's teammate (Ruling 115).
	r.srv.mustRun(t, "join-pane", "-d", "-s", "%0", "-t", r.leadPane)
	r.quietHook(t, "SessionStart", "%0")
	dir := filepath.Join(r.tmp, "claude", "teams", tasksTeam)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := strings.ReplaceAll(readFile(t, filepath.Join(r.root, tasksFixtures, "team", "config.json")), tasksWorktreeToken, r.worktree)
	tasksWrite(t, filepath.Join(dir, "config.json"), cfg)
	ss := r.sessions(t)
	lead, mate := ss[tasksLead], ss[tasksMate]
	if lead.TeamID != tasksTeam || lead.TeamRole != "lead" || lead.TeamMember != "team-lead" {
		t.Errorf("the lead in curia ls --json: %+v, want team %s, role lead, member team-lead", lead, tasksTeam)
	}
	if mate.TeamID != tasksTeam || mate.TeamRole != "teammate" || mate.TeamMember != "alpha" || mate.TaskID != "T-1" {
		t.Errorf("the teammate in curia ls --json: %+v, want team %s, role teammate, member alpha, task T-1", mate, tasksTeam)
	}
	r.mate = r.startClient(t, "client_mate", "%0")
	r.mate.handshake(t)
	r.mate.call(t) // request_decision: ask_lead
	r.lead.call(t) // request_decision: D-2
	var ds []struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
	}
	out := r.h.mustRun(t, "decisions", "ls", "--json")
	if err := json.Unmarshal([]byte(out), &ds); err != nil || len(ds) != 1 || ds[0].ID != "D-2" || ds[0].TaskID != "T-1" {
		t.Errorf("curia decisions ls --json = %s (%v), want only D-2, task T-1", out, err)
	}
	r.wantBadge(t, "D1", "with D-2 open")
	r.wantTasks(t, map[string]string{"T-1": "needs_decision"})
	r.checkClients(t)
}

// checkClients ends both MCP clients and compares their responses with
// their golden files.
func (r *tasksRun) checkClients(t *testing.T) {
	t.Helper()
	for _, c := range []*mcpClient{r.lead, r.mate} {
		c.close(t)
		want := strings.Split(strings.TrimSuffix(r.golden(t, c.name+".golden.jsonl"), "\n"), "\n")
		got := make([]string, len(c.responses))
		for i, line := range c.responses {
			got[i] = clientNormalise(line)
		}
		compareLines(t, c.name, got, want)
	}
}

// Step 11: pause closes both panes, commits the uncommitted file and removes
// the worktree; T-1 is ready and paused.
func (r *tasksRun) pause(t *testing.T) {
	t.Helper()
	tasksWrite(t, filepath.Join(r.worktree, "notes.txt"), "rotation done; tests next\n")
	out := r.user(t, "pause", "T-1")
	for _, want := range []string{
		"Closed pane api-server:T-1 (" + r.leadPane + ") of session 00000001\n",
		"Closed pane api-server:T-1 (%0) of session 88888888\n",
		`"curia: WIP T-1 (paused)" on ` + tasksBranch + "\n",
		"Removed worktree " + r.worktree + " (branch " + tasksBranch + " kept)\n",
		"Paused T-1: curia resume T-1 brings it back (it continues session 00000001)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("curia pause T-1 printed:\n%s\nwant it to hold %q", out, want)
		}
	}
	if exists(r.worktree) {
		t.Errorf("the worktree %s is still there after pause", r.worktree)
	}
	if got := r.git(t, r.repo, "log", "-1", "--format=%s", "--name-only", tasksBranch); got != "curia: WIP T-1 (paused)\n\nnotes.txt\n" {
		t.Errorf("the branch's last commit = %q, want the WIP commit holding notes.txt", got)
	}
	panes := r.srv.mustRun(t, "list-panes", "-a", "-F", "#{pane_id}")
	for _, p := range []string{r.leadPane, "%0"} {
		if slices.Contains(strings.Fields(panes), p) {
			t.Errorf("pane %s is still open after pause: %q", p, panes)
		}
	}
	r.wantTasks(t, map[string]string{"T-1": "ready"})
	if row := r.task(t, "T-1"); row.PausedAt == nil || row.Pane != "" || row.ResumeSession != tasksLead {
		t.Errorf("T-1 after pause = %+v, want paused_at set, no pane and resume_session %s", row, tasksLead)
	}
}

// Step 12: resume brings the worktree back with the file and resumes the
// lead's session, with no brief.
func (r *tasksRun) resume(t *testing.T) {
	t.Helper()
	out := r.user(t, "resume", "T-1")
	if !strings.HasPrefix(out, "Resume: session 00000001 on branch "+tasksBranch+"\n") {
		t.Errorf("curia resume T-1 printed:\n%s\nwant it to start with the session it resumes", out)
	}
	r.leadPane = r.spawnedPane(t, "T-1", out)
	if got := readFile(t, filepath.Join(r.worktree, "notes.txt")); got != "rotation done; tests next\n" {
		t.Errorf("notes.txt after resume = %q", got)
	}
	if got, want := readFile(t, filepath.Join(r.h.dir, "home", "setup-ran")), "T-1 "+r.worktree+"\nT-1 "+r.worktree+"\n"; got != want {
		t.Errorf("setup-ran after resume = %q, want %q", got, want)
	}
	r.wantArgv(t, 2)
	r.wantTasks(t, map[string]string{"T-1": "needs_decision"}) // D-2 is still open
	if row := r.task(t, "T-1"); row.PausedAt != nil || row.Pane != r.leadPane {
		t.Errorf("T-1 after resume = %+v, want no paused_at and pane %s", row, r.leadPane)
	}
}

// Step 13: clean refuses while the agent runs and the branch is unmerged;
// once the pane is gone and the branch merged, it removes both.
func (r *tasksRun) clean(t *testing.T) {
	t.Helper()
	if stderr := r.userFails(t, "clean", "T-1"); !strings.Contains(stderr, "has a live session 00000001") {
		t.Errorf("curia clean T-1 with its agent running: stderr %q, want it to name the live session", stderr)
	}
	r.srv.mustRun(t, "kill-pane", "-t", r.leadPane)
	r.waitPaneGone(t, r.leadPane)
	if stderr := r.userFails(t, "clean", "T-1"); !strings.Contains(stderr, "is not merged into main") {
		t.Errorf("curia clean T-1 unmerged: stderr %q, want the refusal of an unmerged branch", stderr)
	}
	r.git(t, r.repo, "merge", "-q", "--ff-only", tasksBranch)
	out := r.user(t, "clean", "T-1")
	for _, want := range []string{
		"Branch " + tasksBranch + " is merged into main\n",
		"Removed worktree " + r.worktree + "\n",
		"Deleted branch " + tasksBranch + " (was ",
		"Cleaned T-1 (marked dropped, so the board no longer offers it; curia task edit T-1 --status ready brings it back)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("curia clean T-1 printed:\n%s\nwant it to hold %q", out, want)
		}
	}
	if exists(r.worktree) {
		t.Errorf("the worktree %s is still there after clean", r.worktree)
	}
	if got := r.git(t, r.repo, "branch", "--list", tasksBranch); got != "" {
		t.Errorf("branch %s is still there after clean: %q", tasksBranch, got)
	}
}

// Step 14: every task and message as stored, the stub's record, and no
// error in the log.
func (r *tasksRun) finalState(t *testing.T) {
	t.Helper()
	if got, want := r.normalise(r.h.mustRun(t, "task", "ls", "--status", "all", "--json")), r.golden(t, "tasks.golden.json"); got != want {
		t.Errorf("curia task ls --status all --json, normalised:\n%s\nwant (tasks.golden.json):\n%s", got, want)
	}
	if got, want := r.normalise(r.h.mustRun(t, "messages", "ls", "--status", "all", "--json")), r.golden(t, "messages.golden.json"); got != want {
		t.Errorf("curia messages ls --status all --json, normalised:\n%s\nwant (messages.golden.json):\n%s", got, want)
	}
	if got := readFile(t, r.paneFile("mate")) + readFile(t, r.paneFile("web")); got != "" {
		t.Errorf("something was typed into the teammate's or the other agent's pane: %q", got)
	}
	checkNoErrors(t, r.h.logPath)
}

// git runs git in dir with git's repository variables dropped and HOME in
// the test's temp dir, and returns its stdout.
func (r *tasksRun) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.tmp, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1"}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// curia runs the binary with the run's environment plus env.
func (r *tasksRun) curia(env []string, args ...string) (stdout, stderr string, err error) {
	h := *r.h
	h.env = append(slices.Clone(r.h.env), env...)
	stdout, stderr, _, err = h.run(nil, "", args...)
	return stdout, stderr, err
}

// paneLoopScript is the agents' server's pane program (startTmux): it
// records each line typed into it in "$1" and draws "$2", the prompt.
const paneLoopScript = `: > "$1"; while :; do printf '\033[H\033[2J%s' "$2"; IFS= read -r line || exit 0; printf '%s\n' "$line" >> "$1"; done`

// sendFromPane runs `curia send target text` as a child of pane's first
// process, with CLAUDECODE set as in an agent's tool shell, by respawning
// the pane: it runs the send, writes its stdout, stderr and exit code, and
// then becomes the pane's recording program again. It returns what the
// send printed and its exit code.
func (r *tasksRun) sendFromPane(t *testing.T, pane, target, text string) (stdout, stderr string, code int) {
	t.Helper()
	base := filepath.Join(r.tmp, "send-from-"+strings.TrimPrefix(pane, "%"))
	script := `CLAUDECODE=1 "$0" send "$1" "$2" </dev/null >"$3.out" 2>"$3.err"; echo $? >"$3.rc.part"; mv "$3.rc.part" "$3.rc"; ` +
		`exec /bin/sh -c "$4" sh "$5" "$6"`
	r.srv.mustRun(t, "respawn-pane", "-k", "-t", pane, "--", "/bin/sh", "-c", script, r.h.bin, target, text, base,
		paneLoopScript, r.paneFile("web"), claudePrompt)
	for deadline := time.Now().Add(terminalDeadline); !exists(base + ".rc"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("curia send from %s did not finish within %s", pane, terminalDeadline)
		}
	}
	code, err := strconv.Atoi(strings.TrimSpace(readFile(t, base+".rc")))
	if err != nil {
		t.Fatal(err)
	}
	return readFile(t, base+".out"), readFile(t, base+".err"), code
}

// user runs curia as the user at a terminal (a window of the terminal
// server) and returns its stdout, failing unless it exits 0 with nothing
// on stderr.
func (r *tasksRun) user(t *testing.T, args ...string) string {
	t.Helper()
	return r.term.mustRun(t, r.h, args...)
}

// userFails is user requiring exit status 1 and nothing on stdout; it
// returns stderr.
func (r *tasksRun) userFails(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, code := r.term.run(t, r.h, r.h.env, args...)
	if code != 1 || stdout != "" {
		t.Errorf("curia %s at a terminal: stdout %q, stderr %q, exit %d, want exit status 1 and nothing on stdout",
			strings.Join(args, " "), stdout, stderr, code)
	}
	return stderr
}

// fails runs curia with env and requires exit status 1 and nothing on
// stdout; it returns stderr.
func (r *tasksRun) fails(t *testing.T, env []string, args ...string) string {
	t.Helper()
	stdout, stderr, err := r.curia(env, args...)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || stdout != "" {
		t.Errorf("curia %s: stdout %q, stderr %q, err %v, want exit status 1 and nothing on stdout",
			strings.Join(args, " "), stdout, stderr, err)
	}
	return stderr
}

// readHooks reads testdata/tasks/hooks.jsonl with the paths in place of
// their tokens.
func (r *tasksRun) readHooks(t *testing.T) []replayLine {
	t.Helper()
	var lines []replayLine
	for _, text := range readLines(t, filepath.Join(r.root, tasksFixtures, "hooks.jsonl")) {
		for token, path := range map[string]string{tasksWorktreeToken: r.worktree, tasksWebToken: r.web} {
			quoted, _ := json.Marshal(path)
			text = strings.ReplaceAll(text, token, string(quoted[1:len(quoted)-1]))
		}
		var l replayLine
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			t.Fatalf("hooks.jsonl line %q: %v", text, err)
		}
		lines = append(lines, l)
	}
	return lines
}

// hook runs the next line of the hook script, which must be event in pane
// (tasksLeadToken for the lead's pane), and returns its stdout; it fails
// unless the hook exits 0 with nothing on stderr. The lead's hooks carry
// CURIA_TASK_ID, as its window's do.
func (r *tasksRun) hook(t *testing.T, event, pane string) string {
	t.Helper()
	if len(r.hooks) == 0 {
		t.Fatalf("hooks.jsonl has no line left for %s in %s", event, pane)
	}
	l := r.hooks[0]
	r.hooks = r.hooks[1:]
	if l.Event != event || l.Pane != pane {
		t.Fatalf("next hooks.jsonl line is %s in %s, want %s in %s", l.Event, l.Pane, event, pane)
	}
	env := []string{"TMUX_PANE=" + pane}
	if pane == tasksLeadToken {
		env = []string{"TMUX_PANE=" + r.leadPane, "CURIA_TASK_ID=T-1"}
	}
	h := *r.h
	h.env = append(slices.Clone(r.h.env), env...)
	stdout, stderr, _, err := h.run(l.Payload, "", "hook", event)
	if err != nil || stderr != "" {
		t.Errorf("curia hook %s in %s: err %v, stderr %q, want exit 0 and no stderr", event, pane, err, stderr)
	}
	return stdout
}

// quietHook is hook for an event that must print nothing.
func (r *tasksRun) quietHook(t *testing.T, event, pane string) {
	t.Helper()
	if out := r.hook(t, event, pane); out != "" {
		t.Errorf("curia hook %s in %s printed %q, want nothing", event, pane, out)
	}
}

// startClient starts `curia mcp` for the script in name.jsonl in T-1's
// worktree, as Claude Code starts it in pane.
func (r *tasksRun) startClient(t *testing.T, name, pane string) *mcpClient {
	t.Helper()
	script := readLines(t, filepath.Join(r.root, tasksFixtures, name+".jsonl"))
	env := append(slices.Clone(r.h.env), "CLAUDE_PROJECT_DIR="+r.worktree, "TMUX_PANE="+pane)
	return startClient(t, r.h, name, r.worktree, script, env)
}

// tasksSession is a session as `curia ls --json` shows it.
type tasksSession struct {
	State      string `json:"state"`
	Pane       string `json:"tmux_pane"`
	Repo       string `json:"repo"`
	Worktree   string `json:"worktree"`
	TaskID     string `json:"task_id"`
	TeamID     string `json:"team_id"`
	TeamRole   string `json:"team_role"`
	TeamMember string `json:"team_member"`
}

// sessions is `curia ls --json` by session id.
func (r *tasksRun) sessions(t *testing.T) map[string]tasksSession {
	t.Helper()
	out := r.h.mustRun(t, "ls", "--json")
	var rows []struct {
		ID string `json:"id"`
		tasksSession
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("curia ls --json: %v\n%s", err, out)
	}
	got := map[string]tasksSession{}
	for _, row := range rows {
		got[row.ID] = row.tasksSession
	}
	return got
}

// wantStates fails unless `curia ls --json` shows each session in want in
// its state; "ended" means it is not listed.
func (r *tasksRun) wantStates(t *testing.T, want map[string]string) {
	t.Helper()
	got := r.sessions(t)
	for id, state := range want {
		s, listed := got[id]
		switch {
		case state == "ended" && listed:
			t.Errorf("session %s is listed (%s), want it ended", id, s.State)
		case state != "ended" && s.State != state:
			t.Errorf("session %s is %q in curia ls --json, want %q", id, s.State, state)
		}
	}
}

// tasksRow is a task as `curia task ls --json` shows it.
type tasksRow struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	Pane          string     `json:"pane"`
	ResumeSession string     `json:"resume_session"`
	PausedAt      *time.Time `json:"paused_at"`
}

// task is task id in `curia task ls --status all --json`.
func (r *tasksRun) task(t *testing.T, id string) tasksRow {
	t.Helper()
	out := r.h.mustRun(t, "task", "ls", "--status", "all", "--json")
	var rows []tasksRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("curia task ls --json: %v\n%s", err, out)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("curia task ls --json has no %s:\n%s", id, out)
	return tasksRow{}
}

// wantTasks fails unless `curia task ls --json` shows each task in want in
// its derived status.
func (r *tasksRun) wantTasks(t *testing.T, want map[string]string) {
	t.Helper()
	for id, status := range want {
		if got := r.task(t, id).Status; got != status {
			t.Errorf("%s is %q in curia task ls --json, want %q", id, got, status)
		}
	}
}

// wantBadge fails unless `curia status` prints want.
func (r *tasksRun) wantBadge(t *testing.T, want, when string) {
	t.Helper()
	if got := r.h.mustRun(t, "status"); got != want {
		t.Errorf("curia status %s = %q, want %q", when, got, want)
	}
}

// tasksMessage is a message as `curia messages ls --json` shows it.
type tasksMessage struct {
	ID            string     `json:"id"`
	Sender        string     `json:"sender"`
	SenderSession string     `json:"sender_session"`
	Status        string     `json:"status"`
	DeliveredVia  string     `json:"delivered_via"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`
}

// message is message id in `curia messages ls --status all --json`, or the
// zero row when there is none.
func (r *tasksRun) message(t *testing.T, id string) tasksMessage {
	t.Helper()
	out := r.h.mustRun(t, "messages", "ls", "--status", "all", "--json")
	var rows []tasksMessage
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("curia messages ls --json: %v\n%s", err, out)
	}
	for _, m := range rows {
		if m.ID == id {
			return m
		}
	}
	return tasksMessage{}
}

// inputFile is what the stub of T-1 records of the lines typed into it.
func (r *tasksRun) inputFile() string { return filepath.Join(r.stubDir, "input-T-1.txt") }

// goldenInputLines is the first n lines of pane_t1.golden.txt.
func (r *tasksRun) goldenInputLines(t *testing.T, n int) string {
	t.Helper()
	lines := slices.Collect(strings.Lines(r.golden(t, "pane_t1.golden.txt")))
	if len(lines) < n {
		t.Fatalf("pane_t1.golden.txt has %d lines, want at least %d", len(lines), n)
	}
	return strings.Join(lines[:n], "")
}

// waitInput waits up to typedDeadline for the stub's input record to hold
// exactly want.
func (r *tasksRun) waitInput(t *testing.T, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(typedDeadline); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(r.inputFile()); err == nil {
			got = string(b)
		}
		if got == want || time.Now().After(deadline) {
			break
		}
	}
	if got != want {
		t.Errorf("input-T-1.txt after %v:\n%q\nwant:\n%q", typedDeadline, got, want)
	}
}

// wantNothingTyped fails if, after a typing would have landed, the stub's
// input record holds more than the golden file's first n lines.
func (r *tasksRun) wantNothingTyped(t *testing.T, n int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got, want := readFile(t, r.inputFile()), r.goldenInputLines(t, n); got != want {
		t.Errorf("input-T-1.txt = %q, want %q: nothing typed", got, want)
	}
}

// wantArgv fails unless the stub's record of T-1's starts equals the first
// n of stub_argv.golden.txt's blocks (each ends with its "task" line).
func (r *tasksRun) wantArgv(t *testing.T, n int) {
	t.Helper()
	var got string
	path := filepath.Join(r.stubDir, "argv-T-1.txt")
	for deadline := time.Now().Add(clientDeadline); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Clean(path)); err == nil {
			got = string(b)
		}
		if strings.Count(got, "\ntask T-1\n") >= n || time.Now().After(deadline) {
			break
		}
	}
	got = strings.ReplaceAll(got, r.worktree, tasksWorktreeToken)
	blocks := strings.SplitAfter(r.golden(t, "stub_argv.golden.txt"), "\ntask T-1\n")
	if want := strings.Join(blocks[:min(n, len(blocks))], ""); got != want {
		t.Errorf("argv-T-1.txt, normalised:\n%s\nwant (stub_argv.golden.txt, %d starts):\n%s", got, n, want)
	}
}

// waitScreen waits up to clientDeadline for pane's screen to hold want.
func (r *tasksRun) waitScreen(t *testing.T, pane, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(clientDeadline); !strings.Contains(got, want); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pane %s never showed %q; it shows:\n%s", pane, want, got)
		}
		got, _ = r.srv.run("capture-pane", "-p", "-t", pane)
	}
}

// waitPaneGone waits up to clientDeadline for pane to leave the server's
// list.
func (r *tasksRun) waitPaneGone(t *testing.T, pane string) {
	t.Helper()
	for deadline := time.Now().Add(clientDeadline); ; time.Sleep(20 * time.Millisecond) {
		out := r.srv.mustRun(t, "list-panes", "-a", "-F", "#{pane_id}")
		if !slices.Contains(strings.Fields(out), pane) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane %s is still open after %v", pane, clientDeadline)
		}
	}
}

func (r *tasksRun) golden(t *testing.T, name string) string {
	t.Helper()
	return readFile(t, filepath.Join(r.root, tasksGolden, name))
}

// normalise replaces what cannot be known in advance in a JSON listing:
// every time, the repository as a whole string (both forms) and its parent
// at the start of a string (both forms).
func (r *tasksRun) normalise(out string) string {
	for _, repo := range r.repos {
		quoted, _ := json.Marshal(repo)
		out = strings.ReplaceAll(out, string(quoted), `"`+repoToken+`"`)
	}
	for _, p := range r.parents {
		out = normalise(out, p)
	}
	return out
}

// tasksWrite writes content to path (mode 0600), creating its directory.
func tasksWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tasksStatSources stats every fixture and golden file of this test, so a
// changed one cannot reuse a cached pass (build stats the binary's sources).
func tasksStatSources(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{tasksFixtures, tasksGolden} {
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

// Step 15 of the plan's script: the Stop hook's cost with a store that holds
// tasks and messages.
const (
	latencyTasks     = 200
	latencyDelivered = 500
	// latencyMessageRuns is this gate's sample count: it stays at 400,
	// not minTimedRuns, because each sample here is two processes (a send,
	// then the timed Stop) and 1000 of them would add about 27 s to make
	// check (Ruling 114 allows that exception).
	latencyMessageRuns = 400
	latencyPane        = "%0" // the terminal server's sleeping pane
)

// TestStopHookLatencyWithMessages is step 15: on a fresh store with no tmux,
// latencyTasks tasks and latencyDelivered delivered messages for one
// session whose hooks carry CURIA_TASK_ID; then, after one untimed warm-up,
// at least latencyMessageRuns sequential `curia hook Stop` processes are timed,
// stdio as files, each claiming the one message queued before it. Their p95
// must be under p95Budget.
func TestStopHookLatencyWithMessages(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	h := newHome(t, build(t, root))
	repo := filepath.Join(h.dir, "repo")
	gitInit(t, repo, h.dir)
	// The tasks come from one Backlog.md import and the messages from the
	// user at a terminal (Ruling 112): both are the user's to make.
	term := startTerminal(t)
	for i := range latencyTasks {
		tasksWrite(t, filepath.Join(repo, "backlog", "tasks", fmt.Sprintf("task-%d - Latency task %d.md", i+1, i+1)),
			fmt.Sprintf("---\nid: task-%d\ntitle: Latency task %d\nstatus: To Do\n---\n\nWork.\n", i+1, i+1))
	}
	if out := term.mustRun(t, h, "task", "import", "--backlog-md", repo); !strings.Contains(out, fmt.Sprint(latencyTasks)) {
		t.Fatalf("curia task import printed %q, want %d tasks imported", out, latencyTasks)
	}
	// The session is in the terminal server's pane %0, which only sleeps, so
	// the user's sends can see that its agent's pane is live (Ruling 119)
	// and nothing is ever typed into it; the timed hooks run with no TMUX,
	// as before.
	sender := *h
	sender.env = append(slices.Clone(h.env), "TMUX="+term.tmuxVar)
	send := term.userSender(t, &sender, latencyPane)
	hookH := *h
	hookH.env = append(slices.Clone(h.env), "TMUX_PANE="+latencyPane, "CURIA_TASK_ID=T-1")
	start := hookPayload(t, map[string]any{
		"session_id": sessionLatency, "cwd": repo, "hook_event_name": "SessionStart", "source": "startup",
	})
	if out := hookH.mustRunStdin(t, start, "hook", "SessionStart"); out != "" {
		t.Fatalf("curia hook SessionStart printed %q, want nothing", out)
	}
	stop := hookPayload(t, map[string]any{
		"session_id": sessionLatency, "cwd": repo, "hook_event_name": "Stop",
		"stop_hook_active": false, "last_assistant_message": "Done for now.",
	})
	// The delivered messages, handed over by Stops of at most eight each.
	for i := range latencyDelivered {
		send(fmt.Sprintf("Background message number %d.", i+1))
		if (i+1)%8 == 0 || i+1 == latencyDelivered {
			latencyBlock(t, &hookH, stop)
		}
	}
	send("Warm-up message.")
	latencyBlock(t, &hookH, stop) // warm-up, untimed
	timedMessages := 0
	p := medianP95(t.Logf, func() time.Duration {
		var runs []time.Duration
		for len(runs) < latencyMessageRuns {
			timedMessages++
			send(fmt.Sprintf("Timed message number %d.", timedMessages))
			runs = append(runs, latencyBlock(t, &hookH, stop))
		}
		t.Logf("%-12s %s (load average %s)", "Stop", stats(runs), loadAverage())
		return percentile(runs, 95)
	})
	if p >= p95Budget {
		t.Errorf("median p95 wall time of curia hook Stop with %d tasks and %d messages = %v, want under %v (load average %s)",
			latencyTasks, latencyDelivered+timedMessages, p, p95Budget, loadAverage())
	}
	if out := h.mustRun(t, "messages", "ls", "--json"); out != "[]\n" {
		t.Errorf("curia messages ls --json after the pass = %s, want no open message", out)
	}
	checkNoErrors(t, h.logPath)
}

// latencyBlock runs one `curia hook Stop` with payload, checks it exited 0,
// wrote nothing to stderr and printed one block line, and returns its wall
// time.
func latencyBlock(t *testing.T, h *home, payload []byte) time.Duration {
	t.Helper()
	stdout, stderr, took, err := h.run(payload, "", "hook", "Stop")
	if err != nil || stderr != "" || !strings.HasPrefix(stdout, `{"decision":"block","reason":"Curia: messages for you.\n`) ||
		strings.Count(stdout, "\n") != 1 {
		t.Fatalf("curia hook Stop: err %v, stdout %q, stderr %q, want exit 0 and one block line", err, stdout, stderr)
	}
	return took
}

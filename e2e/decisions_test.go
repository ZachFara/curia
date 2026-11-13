package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The Plan 2 acceptance test: the built binary as Claude Code (hooks and
// one `curia mcp` per session) and the user (`curia decide`) would run it,
// against a private tmux server whose panes record what is typed into them.

const (
	// repoToken stands for the temp git repository in the hook fixture and
	// the golden files.
	repoToken = "<repo>"
	// toolsListLine is client_a.golden.jsonl's line for the tools/list
	// response, which is e2e/testdata/decisions/tools_list.golden.json.
	toolsListLine = "<tools_list.golden.json>"
	// clientDeadline bounds the wait for one MCP response, and for the
	// server to exit once its stdin is closed.
	clientDeadline = 5 * time.Second
	// typedDeadline bounds the wait for a typed answer to reach a pane.
	typedDeadline = 2 * time.Second
	// maxSocketPath is about the longest unix socket path the OS accepts.
	maxSocketPath = 100
)

// claudePrompt is the bottom of Claude Code's screen with its input prompt
// empty (as captured from Claude Code 2.1.323): a rule, "❯" and a no-break
// space, a rule and the permission mode line.
var claudePrompt = "✻ Cooked for 7s\n\n" + strings.Repeat("─", 80) + "\n❯\u00a0\n" +
	strings.Repeat("─", 80) + "\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"

// The sessions of the script, one per pane, and the one of the latency pass.
const (
	sessionS1      = "11111111-1111-4111-8111-111111111111"
	sessionS2      = "22222222-2222-4222-8222-222222222222"
	sessionS3      = "33333333-3333-4333-8333-333333333333"
	sessionLatency = "44444444-4444-4444-8444-444444444444"
)

// decisionsFixtures and decisionsGolden are the test's fixture and golden
// directories, relative to the repository root.
var (
	decisionsFixtures = filepath.Join("testdata", "decisions")
	decisionsGolden   = filepath.Join("e2e", "testdata", "decisions")
)

// TestDecisionsEndToEnd runs steps 1 to 11 of Plan 2's acceptance script:
// tickets raised over MCP, answered with `curia decide`, delivered by
// typing into an idle pane, by blocking a Stop, and by check_decision, and
// found again as precedent.
func TestDecisionsEndToEnd(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	decisionsStatSources(t, root)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	r := newDecisionsRun(t, root, build(t, root))

	r.startSessions(t)            // 1
	r.raiseFirstTicket(t)         // 2
	r.needsDecision(t)            // 3
	r.refusedInsideAgent(t)       // 4
	r.typedIntoIdlePane(t)        // 5
	r.deliveredByStopHook(t)      // 6
	r.foundAsPrecedent(t)         // 7
	r.assumptions(t)              // 8
	r.paneGone(t)                 // 9
	r.checkDecisionsList(t)       // 10
	r.checkClients(t)             // every client's responses
	checkNoErrors(t, r.h.logPath) // 11
}

// decisionsRun is one run of the script.
type decisionsRun struct {
	h        *home
	srv      *tmuxServer
	root     string
	repo     string // the temp git repository, as created
	panesDir string // where the panes write what they receive
	hooks    []replayLine
	a, b, c  *mcpClient
}

func newDecisionsRun(t *testing.T, root, bin string) *decisionsRun {
	t.Helper()
	tmp := t.TempDir()
	r := &decisionsRun{h: newHome(t, bin), root: root, repo: filepath.Join(tmp, "repo"), panesDir: filepath.Join(tmp, "panes")}
	for _, d := range []string{r.repo, r.panesDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitInit(t, r.repo, tmp)
	r.srv = startTmux(t, filepath.Join(tmp, "tmuxhome"), r.paneFile("s1"), r.paneFile("s2"), r.paneFile("s3"))
	// Every run of the binary reaches the private server and no other.
	r.h.env = slices.DeleteFunc(r.h.env, func(kv string) bool { return strings.HasPrefix(kv, "TMUX_TMPDIR=") })
	r.h.env = append(r.h.env, "TMUX_TMPDIR="+r.srv.dir, "TMUX="+r.srv.tmuxVar)
	r.hooks = readHookScript(t, root, r.repo)
	return r
}

// paneFile is the file the pane of session s ("s1", "s2" or "s3") writes.
func (r *decisionsRun) paneFile(s string) string {
	return filepath.Join(r.panesDir, "pane_"+s+".txt")
}

// Step 1: S1 starts in %0, S2 starts in %1 and runs a tool, S3 starts in %2.
func (r *decisionsRun) startSessions(t *testing.T) {
	t.Helper()
	r.quietHook(t, "SessionStart", "%0")
	r.quietHook(t, "SessionStart", "%1")
	r.quietHook(t, "PreToolUse", "%1")
	r.quietHook(t, "SessionStart", "%2")
	r.wantStates(t, map[string]string{sessionS1: "idle", sessionS2: "working", sessionS3: "idle"})
}

// Step 2: client A opens as Claude Code does, lists the tools, searches,
// raises D-1 and raises it again.
func (r *decisionsRun) raiseFirstTicket(t *testing.T) {
	t.Helper()
	r.a = r.startClient(t, "client_a", "TMUX_PANE=%0")
	r.a.handshake(t)
	r.a.call(t) // tools/list
	r.a.call(t) // search_decisions "postgres events": nothing yet
	r.a.call(t) // request_decision: D-1
	r.a.call(t) // the same request: D-1, duplicate
}

// Step 3: D-1 shows in the badge, and S1's Stop leaves it needs_decision.
func (r *decisionsRun) needsDecision(t *testing.T) {
	t.Helper()
	if got := r.h.mustRun(t, "status"); got != "D1" {
		t.Errorf("curia status = %q, want %q", got, "D1")
	}
	r.quietHook(t, "Stop", "%0")
	r.wantStates(t, map[string]string{sessionS1: "needs_decision"})
}

// Step 4: inside an agent session, curia decide refuses and D-1 stays open.
func (r *decisionsRun) refusedInsideAgent(t *testing.T) {
	t.Helper()
	stdout, _, _, err := r.h.run(nil, "CLAUDECODE=1", "decide", "D-1", "--accept")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("curia decide with CLAUDECODE=1: err %v, want exit status 1", err)
	}
	if stdout != "" {
		t.Errorf("curia decide with CLAUDECODE=1 printed %q on stdout, want nothing", stdout)
	}
	var open []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	out := r.h.mustRun(t, "decisions", "ls", "--json")
	if err := json.Unmarshal([]byte(out), &open); err != nil {
		t.Fatalf("curia decisions ls --json: %v\n%s", err, out)
	}
	if len(open) != 1 || open[0].ID != "D-1" || open[0].Status != "open" {
		t.Errorf("curia decisions ls --json after the refusal = %+v, want only D-1, open", open)
	}
}

// Step 5: answering D-1 types it into S1's pane, which is idle once the
// answer is stored, and the badge clears; the typed line's submission
// confirms it.
func (r *decisionsRun) typedIntoIdlePane(t *testing.T) {
	t.Helper()
	out := r.h.mustRun(t, "decide", "D-1", "--option", "A",
		"--reason", "Replays need 30 days of history; we already run Postgres (see ops).", "--scope", "repo")
	if !strings.HasSuffix(out, "delivered (send_keys)\n") {
		t.Errorf("curia decide D-1 = %q, want one line ending %q", out, "delivered (send_keys)")
	}
	r.waitForPane(t, "s1", r.goldenPaneLines(t, 1))
	// Typed but not confirmed: the badge counts it as not delivered yet.
	if got := r.h.mustRun(t, "status"); got != "Q1" {
		t.Errorf("curia status after typing D-1 = %q, want %q", got, "Q1")
	}
	// Claude Code submits the typed line: its UserPromptSubmit names D-1,
	// which confirms the typing (Ruling 86), so S1 may be typed into again,
	// and the badge clears.
	r.quietHook(t, "UserPromptSubmit", "%0")
	if got := r.h.mustRun(t, "status"); got != "" {
		t.Errorf("curia status after D-1 was submitted = %q, want nothing", got)
	}
}

// Step 6: D-2's answer waits while S2 works, then blocks S2's next Stop
// exactly once.
func (r *decisionsRun) deliveredByStopHook(t *testing.T) {
	t.Helper()
	r.b = r.startClient(t, "client_b", "TMUX_PANE=%1")
	r.b.handshake(t)
	r.b.call(t) // request_decision: D-2
	out := r.h.mustRun(t, "decide", "D-2", "--text", "Keep the v1 endpoint for one release.", "--reason", "Two clients still call it.")
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "queued: agent is working") {
		t.Errorf("curia decide D-2 = %q, want one line holding %q", out, "queued: agent is working")
	}
	if got := readFile(t, r.paneFile("s2")); got != "" {
		t.Errorf("pane_s2.txt = %q, want nothing typed into a working session", got)
	}
	want := readGolden(t, r.root, "stop_s2.golden.json")
	if got := r.hook(t, "Stop", "%1"); got != want {
		t.Errorf("curia hook Stop for S2 printed:\n%q\nwant (stop_s2.golden.json):\n%q", got, want)
	}
	r.wantStates(t, map[string]string{sessionS2: "working"})
	r.quietHook(t, "Stop", "%1")
	r.wantStates(t, map[string]string{sessionS2: "idle"})
	if got := readFile(t, r.paneFile("s2")); got != "" {
		t.Errorf("pane_s2.txt = %q, want nothing: the Stop hook delivered D-2", got)
	}
}

// Step 7: a ticket like D-1 from S2, in the same repo, is shown D-1 as
// precedent and withdrawn citing it; search finds D-1.
func (r *decisionsRun) foundAsPrecedent(t *testing.T) {
	t.Helper()
	r.b.call(t) // request_decision: D-3, precedent D-1
	r.b.call(t) // withdraw_decision D-3, precedent_id D-1
	r.b.call(t) // search_decisions "postgres events": D-1
}

// Step 8: a confirmed assumption delivers nothing; an overridden one is
// typed with the revert instruction.
func (r *decisionsRun) assumptions(t *testing.T) {
	t.Helper()
	r.a.call(t) // request_decision: D-4, assume
	if got, want := r.h.mustRun(t, "decide", "D-4", "--accept"), "D-4 confirmed · nothing to deliver\n"; got != want {
		t.Errorf("curia decide D-4 --accept = %q, want %q", got, want)
	}
	r.a.call(t) // request_decision: D-5, assume
	r.quietHook(t, "Stop", "%0")
	out := r.h.mustRun(t, "decide", "D-5", "--option", "B")
	if !strings.HasSuffix(out, "delivered (send_keys)\n") {
		t.Errorf("curia decide D-5 = %q, want one line ending %q", out, "delivered (send_keys)")
	}
	// Both lines and nothing between them: D-4's confirmation typed nothing.
	r.waitForPane(t, "s1", r.goldenPaneLines(t, 2))
}

// Step 9: S3's pane dies before D-6 is answered; the answer stays queued
// until S3's own check_decision reads it.
func (r *decisionsRun) paneGone(t *testing.T) {
	t.Helper()
	r.c = r.startClient(t, "client_c", "CURIA_SESSION_ID="+sessionS3)
	r.c.handshake(t)
	r.c.call(t) // request_decision: D-6
	r.quietHook(t, "Stop", "%2")
	r.wantStates(t, map[string]string{sessionS3: "needs_decision"})
	r.srv.mustRun(t, "kill-pane", "-t", "%2")
	out := r.h.mustRun(t, "decide", "D-6", "--accept")
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "pane gone") {
		t.Errorf("curia decide D-6 = %q, want one line holding %q", out, "pane gone")
	}
	r.wantStates(t, map[string]string{sessionS3: "stale"})
	r.c.call(t) // check_decision D-6: the answer
}

// Step 10: every ticket, as stored.
func (r *decisionsRun) checkDecisionsList(t *testing.T) {
	t.Helper()
	got := decisionsNormalise(r.h.mustRun(t, "decisions", "ls", "--status", "all", "--json"), r.repoForms(t))
	if want := readGolden(t, r.root, "decisions.golden.json"); got != want {
		t.Errorf("curia decisions ls --status all --json, normalised:\n%s\nwant (decisions.golden.json):\n%s", got, want)
	}
}

// checkClients ends every client and compares its responses with its
// golden file.
func (r *decisionsRun) checkClients(t *testing.T) {
	t.Helper()
	toolsList := compactGolden(t, r.root, "tools_list.golden.json")
	for _, c := range []*mcpClient{r.a, r.b, r.c} {
		c.close(t)
		want := strings.Split(strings.TrimSuffix(readGolden(t, r.root, c.name+".golden.jsonl"), "\n"), "\n")
		for i, line := range want {
			if line == toolsListLine {
				want[i] = toolsList
			}
		}
		got := make([]string, len(c.responses))
		for i, line := range c.responses {
			got[i] = clientNormalise(line)
		}
		compareLines(t, c.name, got, want)
	}
}

// compareLines reports every line of got that differs from want.
func compareLines(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d responses, want %d:\n%s", name, len(got), len(want), strings.Join(got, "\n"))
	}
	for i := range min(len(got), len(want)) {
		if got[i] != want[i] {
			t.Errorf("%s response %d, normalised:\n%s\nwant (%s.golden.jsonl line %d):\n%s", name, i+1, got[i], name, i+1, want[i])
		}
	}
}

// repoForms is the repository's path as created and symlink-resolved, the
// resolved one first (it is the longer when they differ).
func (r *decisionsRun) repoForms(t *testing.T) []string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(r.repo)
	if err != nil {
		t.Fatal(err)
	}
	return []string{resolved, r.repo}
}

// goldenPaneLines is the first n lines of pane_s1.golden.txt, each with
// its newline.
func (r *decisionsRun) goldenPaneLines(t *testing.T, n int) string {
	t.Helper()
	lines := slices.Collect(strings.Lines(readGolden(t, r.root, "pane_s1.golden.txt")))
	if len(lines) < n {
		t.Fatalf("pane_s1.golden.txt has %d lines, want at least %d", len(lines), n)
	}
	return strings.Join(lines[:n], "")
}

// waitForPane waits up to typedDeadline for pane s's file to hold exactly
// want.
func (r *decisionsRun) waitForPane(t *testing.T, s, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(typedDeadline); ; time.Sleep(20 * time.Millisecond) {
		if got = readFile(t, r.paneFile(s)); got == want || time.Now().After(deadline) {
			break
		}
	}
	if got != want {
		t.Errorf("pane_%s.txt after %v:\n%q\nwant:\n%q", s, typedDeadline, got, want)
	}
}

// wantStates fails unless `curia ls --json` shows each session in want in
// its state.
func (r *decisionsRun) wantStates(t *testing.T, want map[string]string) {
	t.Helper()
	out := r.h.mustRun(t, "ls", "--json")
	var rows []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("curia ls --json: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, row := range rows {
		got[row.ID] = row.State
	}
	for id, state := range want {
		if got[id] != state {
			t.Errorf("session %s is %q in curia ls --json, want %q:\n%s", id, got[id], state, out)
		}
	}
}

// hook runs the next line of the hook script, which must be event in
// pane, and returns its stdout; it fails unless the hook exits 0 with
// nothing on stderr.
func (r *decisionsRun) hook(t *testing.T, event, pane string) string {
	t.Helper()
	if len(r.hooks) == 0 {
		t.Fatalf("hooks.jsonl has no line left for %s in %s", event, pane)
	}
	l := r.hooks[0]
	r.hooks = r.hooks[1:]
	if l.Event != event || l.Pane != pane {
		t.Fatalf("next hooks.jsonl line is %s in %s, want %s in %s", l.Event, l.Pane, event, pane)
	}
	stdout, stderr, _, err := r.h.run(l.Payload, "TMUX_PANE="+pane, "hook", event)
	if err != nil || stderr != "" {
		t.Errorf("curia hook %s in %s: err %v, stderr %q, want exit 0 and no stderr", event, pane, err, stderr)
	}
	return stdout
}

// quietHook is hook for an event that must print nothing.
func (r *decisionsRun) quietHook(t *testing.T, event, pane string) {
	t.Helper()
	if out := r.hook(t, event, pane); out != "" {
		t.Errorf("curia hook %s in %s printed %q, want nothing", event, pane, out)
	}
}

// startClient starts `curia mcp` for the script in name.jsonl, in the
// repository, as Claude Code starts it there, with extraEnv saying who it
// serves.
func (r *decisionsRun) startClient(t *testing.T, name string, extraEnv ...string) *mcpClient {
	t.Helper()
	script := readLines(t, filepath.Join(r.root, decisionsFixtures, name+".jsonl"))
	env := append(slices.Clone(r.h.env), "CLAUDE_PROJECT_DIR="+r.repo)
	return startClient(t, r.h, name, r.repo, script, append(env, extraEnv...))
}

// mcpClient drives one `curia mcp` process over real pipes: one request
// line from its script at a time, one response line back.
type mcpClient struct {
	name      string
	cmd       *exec.Cmd
	stdin     *os.File
	stderr    *os.File
	lines     chan string
	script    []string
	responses []string
	closed    bool
}

// startClient starts `curia mcp` in dir with env. Its stdin and stdout
// are os pipes, its stderr a temp file.
func startClient(t *testing.T, h *home, name, dir string, script, env []string) *mcpClient {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(h.dir, name+"-stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(h.bin, "mcp")
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start curia mcp for %s: %v", name, err)
	}
	_, _ = inR.Close(), outW.Close() // the child holds its own copies
	c := &mcpClient{name: name, cmd: cmd, stdin: inW, stderr: stderr, lines: make(chan string, 64), script: script}
	go readResponses(outR, c.lines)
	t.Cleanup(func() {
		if !c.closed { // the test failed first: leave no process behind
			_ = c.stdin.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		_ = stderr.Close()
	})
	return c
}

// readResponses sends every line read from r to lines, and closes lines at
// the end of r.
func readResponses(r *os.File, lines chan<- string) {
	defer func() { _ = r.Close() }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines <- sc.Text()
	}
	close(lines)
}

// handshake opens the session as Claude Code does: server/discover (to
// which the server answers -32601), initialize, then the initialized
// notification.
func (c *mcpClient) handshake(t *testing.T) {
	t.Helper()
	c.call(t)
	c.call(t)
}

// call sends the script's next lines up to and including the next request
// (a line with an id; notifications before it get no response), reads its
// response within clientDeadline and returns it.
func (c *mcpClient) call(t *testing.T) string {
	t.Helper()
	for {
		if len(c.script) == 0 {
			t.Fatalf("%s.jsonl has no request left", c.name)
		}
		line := c.script[0]
		c.script = c.script[1:]
		if _, err := c.stdin.WriteString(line + "\n"); err != nil {
			t.Fatalf("%s: write request: %v", c.name, err)
		}
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("%s.jsonl line %q: %v", c.name, line, err)
		}
		if msg.ID != nil {
			break
		}
	}
	select {
	case resp, ok := <-c.lines:
		if !ok {
			t.Fatalf("%s: curia mcp closed stdout before responding; stderr %q", c.name, readFile(t, c.stderr.Name()))
		}
		c.responses = append(c.responses, resp)
		return resp
	case <-time.After(clientDeadline):
		t.Fatalf("%s: no response within %v", c.name, clientDeadline)
	}
	return ""
}

// close closes the server's stdin and requires it to exit 0 within
// clientDeadline with nothing more on stdout and nothing on stderr, and
// every script line to have been sent.
func (c *mcpClient) close(t *testing.T) {
	t.Helper()
	if len(c.script) != 0 {
		t.Errorf("%s: %d script lines never sent", c.name, len(c.script))
	}
	_ = c.stdin.Close()
	deadline := time.After(clientDeadline)
	for open := true; open; {
		select {
		case extra, ok := <-c.lines:
			if ok {
				t.Errorf("%s: unrequested stdout line %q", c.name, extra)
			}
			open = ok
		case <-deadline:
			t.Fatalf("%s: curia mcp did not exit within %v of its stdin closing", c.name, clientDeadline)
		}
	}
	err := c.cmd.Wait()
	c.closed = true
	if err != nil {
		t.Errorf("%s: curia mcp exited with %v, want 0", c.name, err)
	}
	if s := readFile(t, c.stderr.Name()); s != "" {
		t.Errorf("%s: curia mcp wrote %q to stderr, want nothing", c.name, s)
	}
}

// tmuxServer is a private tmux server on its own socket, started with no
// configuration, whose panes write what they receive to files.
type tmuxServer struct {
	name    string   // socket name
	dir     string   // TMUX_TMPDIR
	env     []string // the environment of the test's own tmux commands
	tmuxVar string   // TMUX for the binary: socket path, server pid, session
}

// startTmux starts the server with one window per pane file, each pane
// running `cat` into its file, and checks the panes are %0, %1 and %2.
func startTmux(t *testing.T, home string, paneFiles ...string) *tmuxServer {
	t.Helper()
	// A unix socket path holds about 100 bytes, so not t.TempDir().
	dir, err := os.MkdirTemp("", "ct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &tmuxServer{
		name: fmt.Sprintf("curia-test-%d", os.Getpid()),
		dir:  dir,
		env:  []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMUX_TMPDIR=" + dir},
	}
	if sock := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), s.name); len(sock) > maxSocketPath {
		t.Fatalf("tmux socket path %s is %d bytes, over %d", sock, len(sock), maxSocketPath)
	}
	t.Cleanup(func() { _, _ = s.run("kill-server") })
	// argv after -- runs with no shell; the file and the screen are
	// arguments, never part of the script. Each pane draws Claude Code's
	// empty input prompt, the only screen curia decide types into (Ruling
	// 85), and draws it again after each line it records.
	pane := func(file string) []string {
		return []string{
			"--", "/bin/sh", "-c",
			`: > "$1"; while :; do printf '\033[H\033[2J%s' "$2"; IFS= read -r line || exit 0; printf '%s\n' "$line" >> "$1"; done`,
			"sh", file, claudePrompt,
		}
	}
	s.mustRun(t, append([]string{"new-session", "-d", "-s", "curia", "-x", "200", "-y", "50"}, pane(paneFiles[0])...)...)
	for _, f := range paneFiles[1:] {
		s.mustRun(t, append([]string{"new-window", "-d", "-t", "curia"}, pane(f)...)...)
	}
	if got := s.mustRun(t, "list-panes", "-a", "-F", "#{pane_id}"); got != "%0\n%1\n%2\n" {
		t.Fatalf("panes of the fresh server = %q, want %%0, %%1 and %%2", got)
	}
	s.tmuxVar = strings.TrimSpace(s.mustRun(t, "display-message", "-p", "-t", "%0", "#{socket_path},#{pid},0"))
	for _, f := range paneFiles { // each shell has opened its file for cat
		for deadline := time.Now().Add(clientDeadline); !exists(f); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("pane file %s was never created", f)
			}
		}
	}
	return s
}

// run runs tmux on the private server.
func (s *tmuxServer) run(args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-L", s.name, "-f", os.DevNull}, args...)...)
	cmd.Env = s.env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, errOut.String())
	}
	return out.String(), err
}

func (s *tmuxServer) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := s.run(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gitInit makes dir an empty git repository, with git's repository
// variables dropped (the environment is built from nothing) and HOME in
// tmp, so no user or system configuration and no other repository is used.
func gitInit(t *testing.T, dir, tmp string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", dir)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

// readHookScript reads testdata/decisions/hooks.jsonl, with the repository
// in place of repoToken.
func readHookScript(t *testing.T, root, repo string) []replayLine {
	t.Helper()
	quoted, err := json.Marshal(repo)
	if err != nil {
		t.Fatal(err)
	}
	escaped := string(quoted[1 : len(quoted)-1])
	var lines []replayLine
	for _, text := range readLines(t, filepath.Join(root, decisionsFixtures, "hooks.jsonl")) {
		var l replayLine
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			t.Fatalf("hooks.jsonl line %q: %v", text, err)
		}
		l.Payload = bytes.ReplaceAll(l.Payload, []byte(repoToken), []byte(escaped))
		lines = append(lines, l)
	}
	return lines
}

// readLines reads the non-empty lines of a file.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	var lines []string
	for l := range strings.Lines(readFile(t, path)) {
		if l = strings.TrimSuffix(l, "\n"); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readGolden(t *testing.T, root, name string) string {
	t.Helper()
	return readFile(t, filepath.Join(root, decisionsGolden, name))
}

// compactGolden is a golden JSON file in the one-line form the server
// writes: json.Compact drops only the whitespace between tokens and keeps
// key order and every string as written.
func compactGolden(t *testing.T, root, name string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(readGolden(t, root, name))); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return b.String()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// rfc3339 matches an RFC 3339 time anywhere, including inside the escaped
// JSON text of an MCP tool result.
var rfc3339 = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)

// serverVersion matches the version in initialize's serverInfo.
var serverVersion = regexp.MustCompile(`("serverInfo":\{"name":"curia","version":)"[^"]*"`)

// clientNormalise replaces what cannot be known in advance in one MCP
// response: every time becomes <time> (so "<time>", or \"<time>\" inside a
// tool result's text) and serverInfo.version becomes <version>.
func clientNormalise(line string) string {
	line = rfc3339.ReplaceAllString(line, "<time>")
	return serverVersion.ReplaceAllString(line, `${1}"<version>"`)
}

// argsHash matches an args_hash value in `curia decisions ls --json`.
var argsHash = regexp.MustCompile(`"args_hash": "[0-9a-f]{64}"`)

// decisionsNormalise replaces what cannot be known in advance in `curia
// decisions ls --json`: times (by the replay's normalise), the repository
// path in each of repos (a whole JSON string) and args_hash values.
func decisionsNormalise(out string, repos []string) string {
	out = normalise(out, repos[0])
	for _, repo := range repos {
		quoted, _ := json.Marshal(repo)
		out = strings.ReplaceAll(out, string(quoted), `"`+repoToken+`"`)
	}
	return argsHash.ReplaceAllString(out, `"args_hash": "<hash>"`)
}

// decisionsStatSources stats every fixture and golden file of this test,
// so a changed one cannot reuse a cached pass (build stats the sources of
// the binary).
func decisionsStatSources(t *testing.T, root string) {
	t.Helper()
	for _, dir := range []string{decisionsFixtures, decisionsGolden} {
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

// checkNoErrors fails if the log holds a level=ERROR line.
func checkNoErrors(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(data)) {
		if strings.Contains(line, "level=ERROR") {
			t.Errorf("log %s holds an error: %s", path, line)
		}
	}
}

// latencyTickets is how many tickets the latency pass raises first.
const latencyTickets = 20

// TestStopHookLatencyWithTickets is step 12: on a fresh store with no tmux,
// one session raises latencyTickets tickets through one MCP client, then
// at least minTimedRuns sequential `curia hook Stop` processes for it are
// timed, stdio as files, after one untimed warm-up. Their p95 must be
// under p95Budget.
func TestStopHookLatencyWithTickets(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	h := newHome(t, build(t, root))
	start := hookPayload(t, map[string]any{
		"session_id": sessionLatency, "cwd": h.dir, "hook_event_name": "SessionStart", "source": "startup",
	})
	if out := h.mustRunStdin(t, start, "hook", "SessionStart"); out != "" {
		t.Fatalf("curia hook SessionStart printed %q, want nothing", out)
	}
	raiseLatencyTickets(t, h)

	stop := hookPayload(t, map[string]any{
		"session_id": sessionLatency, "cwd": h.dir, "hook_event_name": "Stop",
		"stop_hook_active": false, "last_assistant_message": "Raised the tickets; waiting for answers.",
	})
	h.timedStop(t, stop) // warm-up, untimed
	var runs []time.Duration
	for len(runs) < minTimedRuns {
		runs = append(runs, h.timedStop(t, stop))
	}
	t.Logf("%-12s %s (load average %s)", "Stop", stats(runs), loadAverage())
	if p := percentile(runs, 95); p >= p95Budget {
		t.Errorf("p95 wall time of curia hook Stop with %d tickets = %v, want under %v (load average %s)",
			latencyTickets, p, p95Budget, loadAverage())
	}
	var rows []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	out := h.mustRun(t, "ls", "--json")
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || rows[0].State != "needs_decision" {
		t.Errorf("curia ls --json after the pass = %s (%v), want the one session needs_decision", out, err)
	}
	checkNoErrors(t, h.logPath)
}

// raiseLatencyTickets raises latencyTickets distinct tickets, alternately
// blocking and assume, for sessionLatency through one MCP client.
func raiseLatencyTickets(t *testing.T, h *home) {
	t.Helper()
	var script []string
	for i := range latencyTickets {
		args := map[string]any{
			"question":       fmt.Sprintf("Which cache size should service number %d use?", i+1),
			"options":        []map[string]string{{"key": "A", "label": "64 MB"}, {"key": "B", "label": "256 MB"}},
			"recommendation": "A", "rationale": "Small services rarely need more.", "reversibility": "cheap",
			"mode": "blocking",
		}
		if i%2 == 1 {
			args["mode"], args["assumption"] = "assume", "The service uses 64 MB."
		}
		script = append(script, string(hookPayload(t, map[string]any{
			"jsonrpc": "2.0", "id": i + 1, "method": "tools/call",
			"params": map[string]any{"name": "request_decision", "arguments": args},
		})))
	}
	env := append(slices.Clone(h.env), "CURIA_SESSION_ID="+sessionLatency)
	c := startClient(t, h, "latency", h.dir, script, env)
	for i := range latencyTickets {
		want := fmt.Sprintf(`\"id\":\"D-%d\",\"status\":\"open\"`, i+1)
		if resp := c.call(t); !strings.Contains(resp, want) || !strings.Contains(resp, `"isError":false`) {
			t.Fatalf("request_decision %d = %s, want a new open D-%d", i+1, resp, i+1)
		}
	}
	c.close(t)
}

// hookPayload marshals v as one line of JSON.
func hookPayload(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mustRunStdin is mustRun with stdin.
func (h *home) mustRunStdin(t *testing.T, stdin []byte, args ...string) string {
	t.Helper()
	stdout, stderr, _, err := h.run(stdin, "", args...)
	if err != nil || stderr != "" {
		t.Fatalf("curia %s: err %v, stderr %q, want exit 0 and no stderr", strings.Join(args, " "), err, stderr)
	}
	return stdout
}

// timedStop runs one `curia hook Stop` with payload, checks it exited 0
// and printed nothing, and returns its wall time.
func (h *home) timedStop(t *testing.T, payload []byte) time.Duration {
	t.Helper()
	stdout, stderr, took, err := h.run(payload, "", "hook", "Stop")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("curia hook Stop: err %v, stdout %q, stderr %q, want exit 0 and no output", err, stdout, stderr)
	}
	return took
}

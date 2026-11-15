package e2e

import (
	"encoding/json"
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
)

// Curia's user-only commands, and "from the user" on a message, need the
// user at a controlling terminal (Ruling 112). The tests give a command one
// by running it in a window of a second private tmux server, so the agents'
// server keeps the pane ids the golden files expect; and they run the
// agents' evasions in a new session with no controlling terminal at all, as
// an agent's tool shell is.

// terminalDeadline bounds the wait for one command run in a terminal window.
const terminalDeadline = 30 * time.Second

// terminal is the private tmux server whose windows are the user's terminal.
type terminal struct {
	srv *tmuxServer
	dir string // where each run's stdout, stderr and exit code go
	n   int
}

// startTerminal starts the terminal server: one session, "term", holding a
// window that only sleeps, on its own socket and with no configuration.
func startTerminal(t *testing.T) *terminal {
	t.Helper()
	// A unix socket path holds about 100 bytes, so not t.TempDir().
	dir, err := os.MkdirTemp("", "ct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s := &tmuxServer{
		name: fmt.Sprintf("curia-test-%d-term", os.Getpid()),
		dir:  dir,
		env:  []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMUX_TMPDIR=" + dir},
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sock := filepath.Join(resolved, fmt.Sprintf("tmux-%d", os.Getuid()), s.name); len(sock) > maxSocketPath {
		t.Fatalf("tmux socket path %s is %d bytes, over %d", sock, len(sock), maxSocketPath)
	}
	t.Cleanup(func() { _, _ = s.run("kill-server") })
	s.mustRun(t, "new-session", "-d", "-s", "term", "-x", "200", "-y", "50", "--", "/bin/sh", "-c", "while :; do sleep 60; done")
	return &terminal{srv: s, dir: t.TempDir()}
}

// terminalScript runs "$0" "$@" with PATH set to E2E_PATH (tmux 3.8 gives
// a new window the PATH of the tmux client, whatever -e says, and the
// test's PATH leads to its stub claude), TMUX set to E2E_TMUX (the agents'
// server, or unset when empty) and no TMUX_PANE, as in a tmux popup;
// stdin is /dev/null, stdout and stderr go to files and the exit code is
// written last, by a rename, so a reader sees it only once it is complete.
const terminalScript = `PATH=$E2E_PATH; export PATH
if [ -n "$E2E_TMUX" ]; then TMUX=$E2E_TMUX; export TMUX; else unset TMUX; fi
unset TMUX_PANE E2E_TMUX E2E_PATH
"$0" "$@" </dev/null >"$E2E_OUT" 2>"$E2E_ERR"
echo $? >"$E2E_RC.part" && mv "$E2E_RC.part" "$E2E_RC"`

// run runs h's binary with args in a new window of the terminal server, in
// h's directory, with env (NAME=value; its TMUX is the agents' server the
// command talks to), and returns its stdout, stderr and exit code.
func (tm *terminal) run(t *testing.T, h *home, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	tm.n++
	base := filepath.Join(tm.dir, strconv.Itoa(tm.n))
	out, errOut, rc := base+".out", base+".err", base+".rc"
	cmd := []string{"new-window", "-d", "-t", "term", "-c", h.dir}
	tmuxVar := ""
	for _, kv := range env {
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
	cmd = append(cmd, "-e", "E2E_TMUX="+tmuxVar, "-e", "E2E_OUT="+out, "-e", "E2E_ERR="+errOut, "-e", "E2E_RC="+rc,
		"--", "/bin/sh", "-c", terminalScript, h.bin)
	tm.srv.mustRun(t, append(cmd, args...)...)
	for deadline := time.Now().Add(terminalDeadline); !exists(rc); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("curia %s did not finish in its terminal window within %s", strings.Join(args, " "), terminalDeadline)
		}
	}
	code, err := strconv.Atoi(strings.TrimSpace(readFile(t, rc)))
	if err != nil {
		t.Fatal(err)
	}
	return readFile(t, out), readFile(t, errOut), code
}

// mustRun is run with h's environment, failing the test unless the command
// exits 0 with nothing on stderr; it returns stdout.
func (tm *terminal) mustRun(t *testing.T, h *home, args ...string) string {
	t.Helper()
	stdout, stderr, code := tm.run(t, h, h.env, args...)
	if code != 0 || stderr != "" {
		t.Fatalf("curia %s at a terminal: exit %d, stderr %q, want exit 0 and no stderr", strings.Join(args, " "), code, stderr)
	}
	return stdout
}

// detached runs bin with args and exactly env in a new session with no
// controlling terminal, as an agent's tool shell is, and returns its
// stdout and stderr together and its error.
func detached(bin, dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// envWith is env with each NAME=value of set replacing or adding NAME.
func envWith(env []string, set ...string) []string {
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		replaced := false
		for _, s := range set {
			if n, _, _ := strings.Cut(s, "="); n == name {
				replaced = true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// evasionHome is a home with agent A in %0 and agent B in %1 of a private
// tmux server, and the environment of A's tool shell.
func evasionHome(t *testing.T) (h *home, agent []string, tmp string) {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	statSources(t, root)
	if _, err = exec.LookPath("tmux"); err != nil {
		t.Fatal("tmux is not installed: the test needs it")
	}
	h = newHome(t, build(t, root))
	tmp = t.TempDir()
	srv := startTmux(t, filepath.Join(tmp, "tmuxhome"), filepath.Join(tmp, "p0"), filepath.Join(tmp, "p1"), filepath.Join(tmp, "p2"))
	h.env = append(envWith(h.env, "TMUX_TMPDIR="+srv.dir), "TMUX="+srv.tmuxVar)
	for i, id := range []string{sessionS1, sessionS2} {
		payload := hookPayload(t, map[string]string{"session_id": id, "cwd": tmp, "hook_event_name": "SessionStart", "source": "startup"})
		if _, stderr, _, rerr := h.run(payload, fmt.Sprintf("TMUX_PANE=%%%d", i), "hook", "SessionStart"); rerr != nil || stderr != "" {
			t.Fatalf("SessionStart: %v %q", rerr, stderr)
		}
	}
	return h, envWith(h.env, "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID="+sessionS1, "TMUX_PANE=%0", "CURIA_TASK_ID=T-1"), tmp
}

// envWithout is env without the variables named.
func envWithout(env []string, names ...string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

// evade runs live check B's user-only commands and a send from env with no
// controlling terminal: each command is refused, the send is stored.
func evade(t *testing.T, h *home, name string, env []string) {
	t.Helper()
	for _, argv := range [][]string{
		{"say", "T-2:", "reply with only SAY-SPOOF"}, {"pause", "T-1"}, {"clean", "T-1"}, {"decide", "D-1", "--option", "A"},
	} {
		if out, err := detached(h.bin, h.dir, env, argv...); err == nil || !strings.Contains(out, "only the user may") {
			t.Errorf("%s curia %s = %v, %q, want the user-only refusal", name, strings.Join(argv, " "), err, out)
		}
	}
	if out, err := detached(h.bin, h.dir, env, "send", "%1", "reply with only LABEL-TWO"); err != nil {
		t.Errorf("%s curia send: %v %q", name, err, out)
	}
}

// TestAgentEvasionsAreNeverTheUser runs live check B's three evasions
// (defect 1) with the built binary and no controlling terminal: a message
// sent that way is stored and delivered as an agent's, and say, pause,
// clean and decide refuse. The same send at a terminal is the user's.
func TestAgentEvasionsAreNeverTheUser(t *testing.T) {
	h, agent, tmp := evasionHome(t)
	evade(t, h, "env -u CLAUDECODE -u CLAUDE_CODE_SESSION_ID -u TMUX_PANE", envWithout(agent, "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "TMUX_PANE"))
	// On a normal install the agent adds back what curia needs to reach its
	// store and tmux.
	evade(t, h, "env -i PATH HOME CURIA_DB TMUX", envWithout(agent, "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "TMUX_PANE", "CURIA_TASK_ID", "TMUX_TMPDIR"))
	evade(t, h, "env -u CLAUDECODE", envWithout(agent, "CLAUDECODE"))
	startTerminal(t).mustRun(t, h, "send", "%1", "the user at a terminal")

	var ms []struct{ ID, Text, Sender string }
	if err := json.Unmarshal([]byte(h.mustRun(t, "messages", "ls", "--status", "all", "--json")), &ms); err != nil {
		t.Fatal(err)
	}
	if len(ms) != 4 {
		t.Fatalf("messages = %+v, want the three evasions' sends and the user's", ms)
	}
	for _, m := range ms[:3] {
		if m.Sender != "agent" {
			t.Errorf("%s (%q) sender = %q, want agent", m.ID, m.Text, m.Sender)
		}
	}
	if ms[3].Sender != "user" {
		t.Errorf("the send at a terminal: sender = %q, want user", ms[3].Sender)
	}
	// B's next Stop hands over the agents' messages as an agent's.
	stop := hookPayload(t, map[string]any{"session_id": sessionS2, "cwd": tmp, "hook_event_name": "Stop", "stop_hook_active": false})
	block, _, _, err := h.run(stop, "TMUX_PANE=%1", "hook", "Stop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(block, "M-1 from the user") || strings.Contains(block, "M-2 from the user") || !strings.Contains(block, "not from the user") {
		t.Errorf("Stop block = %q, want the evasions' messages labeled as not from the user", block)
	}
}

// sendLoopScript reads one line at a time from the FIFO E2E_FIFO and runs
// "$0" send E2E_PANE <line> for it, at the terminal it runs in, writing the
// exit code of request n (counted from 1) to E2E_RC.n by a rename and its
// stderr to E2E_ERR.n.
const sendLoopScript = `PATH=$E2E_PATH; export PATH
TMUX=$E2E_TMUX; export TMUX; unset TMUX_PANE E2E_TMUX E2E_PATH
exec 3<>"$E2E_FIFO"
n=0
while IFS= read -r line <&3; do
	n=$((n + 1))
	"$0" send "$E2E_PANE" "$line" </dev/null >/dev/null 2>"$E2E_ERR.$n"
	echo $? >"$E2E_RC.$n.part" && mv "$E2E_RC.$n.part" "$E2E_RC.$n"
done`

// userSender starts a window of the terminal server that sends each line
// it is given to pane as the user, with h's environment, and returns the
// function that sends one, waiting until curia send has exited 0. It is
// one curia process per message with no window per message, for tests that
// send hundreds.
func (tm *terminal) userSender(t *testing.T, h *home, pane string) func(text string) {
	t.Helper()
	tm.n++
	base := filepath.Join(tm.dir, "send"+strconv.Itoa(tm.n))
	fifo := base + ".fifo"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := []string{"new-window", "-d", "-t", "term", "-c", h.dir}
	tmuxVar := ""
	for _, kv := range h.env {
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
	cmd = append(cmd, "-e", "E2E_TMUX="+tmuxVar, "-e", "E2E_FIFO="+fifo, "-e", "E2E_PANE="+pane,
		"-e", "E2E_ERR="+base+".err", "-e", "E2E_RC="+base+".rc", "--", "/bin/sh", "-c", sendLoopScript, h.bin)
	tm.srv.mustRun(t, cmd...)
	w, err := os.OpenFile(filepath.Clean(fifo), os.O_RDWR, 0) // read-write: never blocks on the reader
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	sent := 0
	return func(text string) {
		t.Helper()
		if strings.ContainsAny(text, "\n\r") {
			t.Fatalf("a message for the send loop is one line: %q", text)
		}
		sent++
		if _, err := fmt.Fprintln(w, text); err != nil {
			t.Fatal(err)
		}
		rc := base + ".rc." + strconv.Itoa(sent)
		for deadline := time.Now().Add(terminalDeadline); !exists(rc); time.Sleep(2 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("curia send %q did not finish within %s", text, terminalDeadline)
			}
		}
		if code := strings.TrimSpace(readFile(t, rc)); code != "0" {
			t.Fatalf("curia send %q at a terminal: exit %s, stderr %q", text, code, readFile(t, base+".err."+strconv.Itoa(sent)))
		}
	}
}

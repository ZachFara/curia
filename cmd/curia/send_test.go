package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// sendHome is readHome with target session t1 idle in pane %1, sender
// session a2 idle in pane %2 and a live shell pane %9 with no session.
func sendHome(t *testing.T, extra ...model.Session) (string, *tmux.Fake) {
	t.Helper()
	sessions := append([]model.Session{
		readSession("t1", model.StateIdle, "%1"),
		readSession("a2", model.StateIdle, "%2"),
	}, extra...)
	return readHome(t, []string{"%1", "%2", "%9"}, sessions...)
}

// sendMessages returns every stored message.
func sendMessages(t *testing.T, db string) []model.Message {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ms, err := st.ListMessages(context.Background(), store.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// sendOne returns the one stored message, failing unless there is exactly
// one.
func sendOne(t *testing.T, db string) model.Message {
	t.Helper()
	ms := sendMessages(t, db)
	if len(ms) != 1 {
		t.Fatalf("messages = %+v, want exactly one", ms)
	}
	return ms[0]
}

// sendTyped returns the SendKeys calls fake received.
func sendTyped(fake *tmux.Fake) []string {
	var out []string
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "SendKeys") {
			out = append(out, c)
		}
	}
	return out
}

func TestSendQueuesAndDelivers(t *testing.T) {
	db, fake := sendHome(t)
	out, err := readRun(t, "", "send", "%1", "use", "pnpm,", "not npm")
	if err != nil {
		t.Fatal(err)
	}
	m := sendOne(t, db)
	if m.Kind != model.KindInstruction || m.RoutedBy != model.RoutedSend || m.Sender != model.SenderUser ||
		m.SenderSession != "" || m.TargetSession != "t1" || m.Text != "use pnpm, not npm" {
		t.Errorf("message = %+v, want an instruction routed by send from the user to t1", m)
	}
	if m.Status != model.MsgDelivered || m.DeliveredVia != model.ViaSendKeys {
		t.Errorf("message status %s via %s, want delivered via send_keys", m.Status, m.DeliveredVia)
	}
	if want := []string{"SendKeys %1 " + message.TypedLine(m)}; !slices.Equal(sendTyped(fake), want) {
		t.Errorf("typed %q, want %q", sendTyped(fake), want)
	}
	if want := "M-1 delivered (send_keys)\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
}

func TestSendToWorkingSessionQueues(t *testing.T) {
	db, fake := readHome(t, []string{"%1"}, readSession("t1", model.StateWorking, "%1"))
	out, err := readRun(t, "", "send", "%1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if m := sendOne(t, db); m.Status != model.MsgQueued {
		t.Errorf("status = %s, want queued", m.Status)
	}
	if typed := sendTyped(fake); len(typed) != 0 {
		t.Errorf("typed %q, want nothing", typed)
	}
	if !strings.HasPrefix(out, "M-1 queued: agent is working") || !strings.Contains(out, "check_messages") {
		t.Errorf("stdout = %q, want the queued note", out)
	}
}

func TestSendRefusesNonAgentPane(t *testing.T) {
	for _, pane := range readRefusedPanes {
		t.Run(pane, func(t *testing.T) {
			db, fake := readRefusalHome(t)
			out, err := readRun(t, "", "send", pane, "hello")
			if err == nil {
				t.Errorf("send %q: no error, stdout %q", pane, out)
			}
			if ms := sendMessages(t, db); len(ms) != 0 {
				t.Errorf("send %q stored %+v", pane, ms)
			}
			if typed := sendTyped(fake); len(typed) != 0 {
				t.Errorf("send %q typed %q", pane, typed)
			}
		})
	}
}

func TestSendRefusesSharedPane(t *testing.T) {
	db, fake := sendHome(t, readSession("t1b", model.StateWorking, "%1"))
	_, err := readRun(t, "", "send", "%1", "hello")
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Errorf("send to a shared pane = %v, want a refusal naming the shared pane", err)
	}
	if ms := sendMessages(t, db); len(ms) != 0 {
		t.Errorf("stored %+v", ms)
	}
	if typed := sendTyped(fake); len(typed) != 0 {
		t.Errorf("typed %q", typed)
	}
	// An ended or stale session in the pane does not make it shared.
	db, _ = sendHome(t, readSession("old", model.StateEnded, "%1"), readSession("older", model.StateStale, "%1"))
	if _, err := readRun(t, "", "send", "%1", "hello"); err != nil {
		t.Errorf("send beside ended and stale rows: %v", err)
	}
	if m := sendOne(t, db); m.TargetSession != "t1" {
		t.Errorf("target = %q, want t1", m.TargetSession)
	}
}

// sendAgentShell is the pid of the first process of a2's pane %2.
const sendAgentShell = fakePID + 2

// sendUnderAgent makes the caller's processes those of a tool shell of the
// Claude Code agent a2 runs in pane %2: curia's parent a shell, then claude,
// then the pane's shell (pid sendAgentShell), then tmux.
func sendUnderAgent(t *testing.T, fake *tmux.Fake) {
	t.Helper()
	paneShell := sendAgentShell
	i := slices.IndexFunc(fake.Panes, func(p tmux.Pane) bool { return p.ID == "%2" })
	if i < 0 {
		t.Fatal("no pane %2")
	}
	fake.Panes[i].PID = paneShell
	userProcessesAs(t, map[int]userProc{
		os.Getppid(): {PPID: fakePID + 1, Comm: "/bin/zsh"}, fakePID + 1: {PPID: paneShell, Comm: "claude"},
		paneShell: {PPID: fakePID + 3, Comm: "-zsh"}, fakePID + 3: {PPID: 1, Comm: "tmux"},
	})
}

func TestSendFromAgentIsLabelled(t *testing.T) {
	db, fake := sendHome(t)
	sendUnderAgent(t, fake)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("TMUX_PANE", "%2")
	out, err := readRun(t, "", "send", "%1", "the api tests are green")
	if err != nil {
		t.Fatal(err)
	}
	m := sendOne(t, db)
	if m.Sender != model.SenderAgent || m.SenderSession != "a2" || m.RoutedBy != model.RoutedSend ||
		m.Kind != model.KindInstruction {
		t.Errorf("message = %+v, want an instruction from agent session a2", m)
	}
	typed := sendTyped(fake)
	if len(typed) != 1 || !strings.Contains(typed[0], "from another agent session, not from the user") {
		t.Errorf("typed %q, want one line labeled as from another agent session", typed)
	}
	if out != "M-1 delivered (send_keys)\n" {
		t.Errorf("stdout = %q", out)
	}
}

func TestSendAgentCannotPassAsUser(t *testing.T) {
	// Any sign of an agent caller labels the message as an agent's, even
	// with CLAUDECODE cleared.
	for _, env := range [][2]string{
		{"CLAUDE_CODE_SESSION_ID", "a2"},
		{"CLAUDE_CODE_SESSION_ID", "nobody"},
		{"CURIA_SESSION_ID", "a2"},
		{"TMUX_PANE", "%2"},
	} {
		t.Run(env[0]+"="+env[1], func(t *testing.T) {
			db, _ := sendHome(t)
			t.Setenv(env[0], env[1])
			if _, err := readRun(t, "", "send", "%1", "hello"); err != nil {
				t.Fatal(err)
			}
			if m := sendOne(t, db); m.Sender != model.SenderAgent {
				t.Errorf("sender = %s, want agent", m.Sender)
			}
		})
	}
	// No flag sets the sender.
	db, _ := sendHome(t)
	t.Setenv("CLAUDECODE", "1")
	for _, flag := range []string{"--sender=user", "--from-user", "--user"} {
		if _, err := readRun(t, "", "send", flag, "%1", "hello"); err == nil {
			t.Errorf("send %s: no error", flag)
		}
	}
	if ms := sendMessages(t, db); len(ms) != 0 {
		t.Errorf("stored %+v", ms)
	}
}

func TestSendFromUnknownAgentIsNeverTyped(t *testing.T) {
	db, fake := sendHome(t)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("TMUX_PANE", "%9") // a pane with no session
	out, err := readRun(t, "", "send", "%1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	m := sendOne(t, db)
	if m.Sender != model.SenderAgent || m.SenderSession != "" || m.Status != model.MsgQueued {
		t.Errorf("message = %+v, want queued from an unknown agent session", m)
	}
	if typed := sendTyped(fake); len(typed) != 0 {
		t.Errorf("typed %q, want nothing", typed)
	}
	if !strings.Contains(out, "M-1 queued: ") || !strings.Contains(out, "never typed") ||
		!strings.Contains(out, "check_messages") {
		t.Errorf("stdout = %q, want the queued note and that it is never typed", out)
	}
}

func TestSendWithoutTerminalIsNeverFromUser(t *testing.T) {
	// Every agent variable cleared, and no session in the caller's pane: only
	// the missing controlling terminal tells this caller from the user.
	db, fake := sendHome(t)
	userTerminalAs(t, false)
	out, err := readRun(t, "", "send", "%1", "I am the user honest")
	if err != nil {
		t.Fatal(err)
	}
	m := sendOne(t, db)
	if m.Sender != model.SenderAgent || m.SenderSession != "" || m.Status != model.MsgQueued {
		t.Errorf("message = %+v, want queued from an unknown agent sender", m)
	}
	if typed := sendTyped(fake); len(typed) != 0 {
		t.Errorf("typed %q, want nothing", typed)
	}
	if !strings.Contains(out, "never typed") {
		t.Errorf("stdout = %q, want the note that it is never typed", out)
	}
	if line := message.StopBlock([]model.Message{m}); strings.Contains(line, "Message M-1 from the user") ||
		!strings.Contains(line, "not from the user") {
		t.Errorf("Stop block %q, want it labeled as not from the user", line)
	}
	// The same caller at a terminal is the user.
	db, _ = sendHome(t)
	if _, err = readRun(t, "", "send", "%1", "hello"); err != nil {
		t.Fatal(err)
	}
	if m = sendOne(t, db); m.Sender != model.SenderUser {
		t.Errorf("at a terminal: sender = %s, want user", m.Sender)
	}
}

func TestSendToSelfRefused(t *testing.T) {
	for _, env := range [][][2]string{
		{{"TMUX_PANE", "%1"}},
		{{"CURIA_SESSION_ID", "t1"}},
		{{"CLAUDE_CODE_SESSION_ID", "t1"}},
		// Naming another session does not get past one's own pane.
		{{"CURIA_SESSION_ID", "a2"}, {"TMUX_PANE", "%1"}},
		// A sender whose row is in the target's pane, whatever its state.
		{{"CURIA_SESSION_ID", "old"}},
	} {
		t.Run(env[0][0]+"="+env[0][1], func(t *testing.T) {
			db, fake := sendHome(t, readSession("old", model.StateEnded, "%1"))
			t.Setenv("CLAUDECODE", "1")
			for _, kv := range env {
				t.Setenv(kv[0], kv[1])
			}
			_, err := readRun(t, "", "send", "%1", "hello")
			if err == nil || !strings.Contains(err.Error(), "own") {
				t.Errorf("send to self = %v, want a refusal", err)
			}
			if ms := sendMessages(t, db); len(ms) != 0 {
				t.Errorf("stored %+v", ms)
			}
			if typed := sendTyped(fake); len(typed) != 0 {
				t.Errorf("typed %q", typed)
			}
		})
	}
}

func TestSendTextFromStdinAndBounds(t *testing.T) {
	db, _ := sendHome(t)
	if _, err := readRun(t, "\n  from \x1b[31mstdin\x1b[0m\n\n", "send", "%1", "-"); err != nil {
		t.Fatal(err)
	}
	if m := sendOne(t, db); m.Text != "from stdin" {
		t.Errorf("text = %q, want %q", m.Text, "from stdin")
	}
	for name, c := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"empty stdin":     {"", []string{"-"}, "800"},
		"blank stdin":     {" \n\t\n", []string{"-"}, "800"},
		"blank argument":  {"", []string{" "}, "800"},
		"801 runes":       {"", []string{strings.Repeat("é", 801)}, "800"},
		"801 from stdin":  {strings.Repeat("a", 801), []string{"-"}, "800"},
		"over 64 KiB":     {strings.Repeat("a", sendStdinMax+1), []string{"-"}, "64 KiB"},
		"only escapes":    {"", []string{"\x1b[31m\x07"}, "800"},
		"no text at all":  {"", nil, "arg"},
		"argument joined": {"", []string{strings.Repeat("a", 400), strings.Repeat("b", 400)}, "800"},
	} {
		t.Run(name, func(t *testing.T) {
			cdb, fake := sendHome(t)
			_, err := readRun(t, c.stdin, append([]string{"send", "%1"}, c.args...)...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("send = %v, want an error naming %q", err, c.want)
			}
			if ms := sendMessages(t, cdb); len(ms) != 0 {
				t.Errorf("stored %+v", ms)
			}
			if typed := sendTyped(fake); len(typed) != 0 {
				t.Errorf("typed %q", typed)
			}
		})
	}
	// Exactly 800 runes is accepted.
	db, _ = sendHome(t)
	if _, err := readRun(t, "", "send", "%1", strings.Repeat("é", 800)); err != nil {
		t.Errorf("800 runes: %v", err)
	}
	if m := sendOne(t, db); len([]rune(m.Text)) != 800 {
		t.Errorf("stored %d runes, want 800", len([]rune(m.Text)))
	}
}

func TestSendOutcomeLine(t *testing.T) {
	for _, c := range []struct {
		o    model.MessageOutcome
		want string
	}{
		{
			model.MessageOutcome{Message: model.Message{ID: "M-12", Status: model.MsgDelivered, DeliveredVia: model.ViaSendKeys}},
			"M-12 delivered (send_keys)",
		},
		{
			model.MessageOutcome{Message: model.Message{ID: "M-12", Status: model.MsgQueued}, Note: "agent is working"},
			"M-12 queued: agent is working",
		},
		{
			model.MessageOutcome{Message: model.Message{ID: "M-3", Status: model.MsgQueued}, Note: "bad\x1b[2Jnote\nnext"},
			"M-3 queued: badnote next",
		},
		{model.MessageOutcome{
			Message: model.Message{ID: "M-4", Status: model.MsgDelivered, DeliveredVia: model.ViaSendKeys},
			Note:    "typing may not have finished",
		}, "M-4 delivered (send_keys): typing may not have finished"},
	} {
		if got := sayOutcomeLine(c.o); got != c.want {
			t.Errorf("sayOutcomeLine(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

func TestSendRateLimitedCommand(t *testing.T) {
	db, fake := sendHome(t, readSession("t3", model.StateWorking, "%3"))
	fake.Panes = append(fake.Panes, tmux.Pane{ID: "%3", Session: "work", WindowID: "@3", ServerStart: at(8, 0)})
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("TMUX_PANE", "%2") // agent a2
	for i := range store.SendQueuedLimit {
		if _, err := readRun(t, "", "send", "%3", fmt.Sprintf("message %d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := readRun(t, "", "send", "%3", "one more")
	if err == nil || err.Error() != "not sent: the target already has 5 queued messages from agents, the most it may hold; send again once it has read them" {
		t.Errorf("sixth send = %v, want the plain refusal", err)
	}
	if ms := sendMessages(t, db); len(ms) != store.SendQueuedLimit {
		t.Errorf("stored %d messages, want %d", len(ms), store.SendQueuedLimit)
	}
	// The user is not limited.
	t.Setenv("CLAUDECODE", "")
	t.Setenv("TMUX_PANE", "")
	if _, err = readRun(t, "", "send", "%3", "from the user"); err != nil {
		t.Errorf("the user's send: %v", err)
	}
}

// Ruling 113: the sender of curia send is read from its processes; a
// claimed session counts only when it is the one the ancestry finds.
// sendAncestryHome is sendHome with two more live agents, t3 in %3 and t4
// in %4.
func sendAncestryHome(t *testing.T) (string, *tmux.Fake) {
	t.Helper()
	db, fake := sendHome(t, readSession("t3", model.StateIdle, "%3"), readSession("t4", model.StateIdle, "%4"))
	for _, p := range []string{"%3", "%4"} {
		fake.Panes = append(fake.Panes, tmux.Pane{ID: p, Session: "work", WindowID: "@" + p[1:], ServerStart: at(8, 0)})
	}
	return db, fake
}

func TestSendSenderFromAncestry(t *testing.T) {
	t.Run("claim matches the ancestry", sendAncestryMatches)
	t.Run("claims another live session", sendAncestryOtherClaim)
	t.Run("variables cleared, ancestry finds the agent", sendAncestryCleared)
	t.Run("ancestry unreadable", sendAncestryUnreadable)
	t.Run("rotating claimed ids hits the shared bucket", sendAncestryRotating)
}

// sendAncestryMatches: claim matches the ancestry.
func sendAncestryMatches(t *testing.T) {
	db, fake := sendAncestryHome(t)
	sendUnderAgent(t, fake)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "a2")
	if _, err := readRun(t, "", "send", "%1", "hello"); err != nil {
		t.Fatal(err)
	}
	if m := sendOne(t, db); m.Sender != model.SenderAgent || m.SenderSession != "a2" {
		t.Errorf("message = %+v, want verified a2", m)
	}
}

// sendAncestryOtherClaim: claims another live session.
func sendAncestryOtherClaim(t *testing.T) {
	db, fake := sendAncestryHome(t)
	sendUnderAgent(t, fake)
	t.Setenv("CURIA_SESSION_ID", "t3")
	if _, err := readRun(t, "", "send", "%1", "hello"); err != nil {
		t.Fatal(err)
	}
	if m := sendOne(t, db); m.Sender != model.SenderAgent || m.SenderSession != "" {
		t.Errorf("message = %+v, want an unknown agent sender, not t3", m)
	}
	if typed := sendTyped(fake); len(typed) != 0 {
		t.Errorf("typed %q, want nothing", typed)
	}
}

// sendAncestryCleared: variables cleared, ancestry finds the agent.
func sendAncestryCleared(t *testing.T) {
	db, fake := sendAncestryHome(t)
	sendUnderAgent(t, fake)
	if _, err := readRun(t, "", "send", "%1", "hello"); err != nil {
		t.Fatal(err)
	}
	if m := sendOne(t, db); m.Sender != model.SenderAgent || m.SenderSession != "a2" {
		t.Errorf("message = %+v, want verified a2", m)
	}
}

// sendAncestryUnreadable: ancestry unreadable.
func sendAncestryUnreadable(t *testing.T) {
	db, _ := sendAncestryHome(t)
	userProcessesAs(t, map[int]userProc{os.Getppid(): {PPID: fakePID + 1, Comm: "/bin/zsh"}}) // fakePID + 1 cannot be read
	t.Setenv("TMUX_PANE", "%2")
	if _, err := readRun(t, "", "send", "%1", "hello"); err != nil {
		t.Fatal(err)
	}
	if m := sendOne(t, db); m.Sender != model.SenderAgent || m.SenderSession != "" {
		t.Errorf("message = %+v, want an unknown agent sender", m)
	}
}

// sendAncestryRotating: rotating claimed ids hits the shared bucket.
func sendAncestryRotating(t *testing.T) {
	db, fake := sendAncestryHome(t)
	sendUnderAgent(t, fake)
	claims := []string{"t1", "t3", "t4", "t3", "t4"}
	targets := []string{"%3", "%4", "%1", "%1", "%3"}
	for i, claim := range claims {
		t.Setenv("CURIA_SESSION_ID", claim)
		if _, err := readRun(t, "", "send", targets[i], fmt.Sprintf("message %d", i+1)); err != nil {
			t.Fatalf("send %d claiming %s: %v", i+1, claim, err)
		}
	}
	t.Setenv("CURIA_SESSION_ID", "t1")
	_, err := readRun(t, "", "send", "%4", "one more")
	if err == nil || !strings.Contains(err.Error(), "cannot identify") {
		t.Errorf("sixth rotated send = %v, want the shared bucket's refusal", err)
	}
	for _, m := range sendMessages(t, db) {
		if m.SenderSession != "" {
			t.Errorf("message %s stored as from %q, want unknown", m.ID, m.SenderSession)
		}
	}
}

const (
	sendSockA = "/tmp/tmux-1/a"
	sendSockB = "/tmp/tmux-1/b"
)

// sendOnServer is readSession on the tmux server at socket.
func sendOnServer(id, pane, socket string) model.Session {
	s := readSession(id, model.StateIdle, pane)
	s.Socket = socket
	return s
}

// A pane id is matched only with its tmux server (Ruling 133).
func TestSendPaneMatchesCallersServer(t *testing.T) {
	both := []model.Session{sendOnServer("sa", "%7", sendSockA), sendOnServer("sb", "%7", sendSockB)}
	for _, c := range []struct {
		name, tmuxEnv string
		sessions      []model.Session
		want          string // target session, or "" for a refusal
	}{
		{"caller on A", sendSockA + ",1,0", both, "sa"},
		{"caller on B", sendSockB + ",1,0", both, "sb"},
		{"third server", "/tmp/tmux-1/c,1,0", both, ""},
		{"no TMUX", "", both, ""},
		{"only on other server", sendSockA + ",1,0", both[1:], ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, fake := readHome(t, []string{"%7"}, c.sessions...)
			t.Setenv("TMUX", c.tmuxEnv)
			_, err := readRun(t, "", "send", "%7", "hello")
			if c.want == "" {
				if err == nil {
					t.Fatal("send: no error, want a refusal")
				}
				if ms := sendMessages(t, db); len(ms) != 0 || len(sendTyped(fake)) != 0 {
					t.Errorf("refused send stored %+v, typed %q", ms, sendTyped(fake))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m := sendOne(t, db); m.TargetSession != c.want {
				t.Errorf("target = %q, want %q", m.TargetSession, c.want)
			}
		})
	}
}

package main

import (
	"context"
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

func TestSendFromAgentIsLabelled(t *testing.T) {
	db, fake := sendHome(t)
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
		if got := sendOutcomeLine(c.o); got != c.want {
			t.Errorf("sendOutcomeLine(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
}

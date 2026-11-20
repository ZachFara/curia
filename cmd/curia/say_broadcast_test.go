package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/route"
	"github.com/ZachFara/curia/internal/tmux"
)

// sayBcSession is an idle session in pane on the server at socket.
func sayBcSession(id, pane, repo, socket string) model.Session {
	s := saySession(id, pane, repo, "main")
	s.Socket = socket
	return s
}

// sayBcWorld is sayWorld with the caller on tmux server A.
func sayBcWorld(t *testing.T, sessions ...model.Session) (string, *tmux.Fake, *decideClock) {
	t.Helper()
	db, fake, c := sayWorld(t, sessions...)
	t.Setenv("TMUX", sendSockA+",1,0")
	return db, fake, c
}

// sayBcTargets are the target sessions of the broadcast rows in db.
func sayBcTargets(t *testing.T, db string) []string {
	t.Helper()
	var out []string
	for _, m := range sayMessages(t, db) {
		if m.BroadcastID == "" {
			t.Errorf("message %s has no broadcast id", m.ID)
		}
		out = append(out, m.TargetSession)
	}
	return out
}

func TestSayBroadcastAllSkipsEndedAndOtherServer(t *testing.T) {
	sessions := []model.Session{
		sayBcSession("A1", "%1", "/work/api-server", sendSockA),
		sayBcSession("A2", "%2", "/work/web", sendSockA),
		sayBcSession("B1", "%3", "/work/api-server", sendSockB), // the same pane ids on another server
		sayBcSession("E1", "%4", "/work/web", sendSockA),
	}
	sessions[3].State = model.StateEnded
	db, fake, c := sayBcWorld(t, sessions...)
	fake.Panes = fake.Panes[:3] // %3 exists here too, but B1 is registered on the other server
	sayUseRouter(t, sayRouterNever(t))
	sayDriveView(t, c, sayEnter)
	out, _, err := sayRun(t, strings.NewReader(""), "@all", "pull", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := sayBcTargets(t, db); len(got) != 2 || got[0] != "A1" || got[1] != "A2" {
		t.Errorf("targets = %v, want [A1 A2]", got)
	}
	ms := sayMessages(t, db)
	if ms[0].BroadcastID != ms[1].BroadcastID || ms[0].RoutedBy == model.RoutedConfirmed || ms[0].Text != "pull main" {
		t.Errorf("messages = %+v, want one shared broadcast id and an unlabeled routing", ms)
	}
	if len(saySent(fake)) != 2 || !strings.Contains(out, "2 sessions: 2 delivered") {
		t.Errorf("typed %q, output %q; want one line per session and the tally", saySent(fake), out)
	}
}

func TestSayBroadcastRepoUsesExactNameOnly(t *testing.T) {
	sessions := []model.Session{
		sayBcSession("A1", "%1", "/work/a.b", sendSockA),
		sayBcSession("A2", "%2", "/work/aXb", sendSockA), // "." is no wildcard
		sayBcSession("A3", "%3", "/work/A.B", sendSockA), // case-insensitive, as @name is
		sayBcSession("A4", "%4", "/work/a.b.c", sendSockA),
	}
	db, _, c := sayBcWorld(t, sessions...)
	sayUseRouter(t, sayRouterNever(t))
	sayDriveView(t, c, sayEnter)
	if _, _, err := sayRun(t, strings.NewReader(""), "@a.b/*", "hello"); err != nil {
		t.Fatal(err)
	}
	if got := sayBcTargets(t, db); len(got) != 2 || got[0] != "A1" || got[1] != "A3" {
		t.Errorf("targets = %v, want [A1 A3]", got)
	}
}

func TestSayBroadcastRefusals(t *testing.T) {
	db, _, _ := sayWorld(t) // no TMUX: no server to match panes on
	sayUseRouter(t, sayRouterNever(t))
	if _, _, err := sayRun(t, strings.NewReader(""), "@all", "hi"); err == nil || !strings.Contains(err.Error(), "TMUX is not set") {
		t.Errorf("without TMUX: err = %v, want the TMUX refusal", err)
	}
	t.Setenv("TMUX", sendSockA+",1,0")
	for _, args := range [][]string{{"@all", "rule:", "x"}, {"@all", "task:", "x"}, {"@all", "T-1:", "x"}, {"@nosuch/*", "x"}} {
		if _, _, err := sayRun(t, strings.NewReader(""), args...); err == nil {
			t.Errorf("say %q: no error", args)
		}
	}
	// No terminal to confirm on: a broadcast is never sent.
	if _, _, err := sayRun(t, strings.NewReader(""), "@all", "hi"); err == nil || !strings.Contains(err.Error(), "confirmation") {
		t.Errorf("headless: err = %v, want the confirmation refusal", err)
	}
	if ms := sayMessages(t, db); len(ms) != 0 {
		t.Errorf("messages = %+v, want none", ms)
	}
}

// sayBcRouter is a router that answers result.
func sayBcRouter(t *testing.T, result string) {
	t.Helper()
	sayUseRouter(t, sayReply(sayEnvelope(t, result)))
}

func TestSayBroadcastFromRouterAlwaysAsksEvenWithAutoSend(t *testing.T) {
	sessions := []model.Session{
		sayBcSession("A1", "%1", "/work/api-server", sendSockA),
		sayBcSession("A2", "%2", "/work/web", sendSockA),
	}
	db, fake, c := sayBcWorld(t, sessions...)
	sayEarned(t, db, 3) // auto-send on and earned; the text names one agent but the router says all
	sayBcRouter(t, `{"target":"s1","kind":"instruction","confidence":0.99,"runner_up":"","scope":"all"}`)
	before := len(sayMessages(t, db))
	labelsBefore, err := sayOpen(t, db).RouteLabels(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	src := sayNewSource(sayOpen(t, db), nil, nil, sayBcConfig(t), model.AnsweredCLI)
	plan, err := src.propose(context.Background(), "tell the api-server agent to stop")
	if err != nil {
		t.Fatal(err)
	}
	if plan.p.Auto || len(plan.p.Targets) != 2 || plan.p.Candidates[0].Type != route.TargetBroadcast ||
		!strings.Contains(plan.p.Candidates[0].Label, "(2)") {
		t.Fatalf("proposal = %+v, want a confirmed-only broadcast of 2 sessions with the count in its label", plan.p)
	}

	// Left alone, the view sends nothing; only Enter does, once.
	sayBcSay(t, c, "tell the api-server agent to stop")
	if got := len(sayMessages(t, db)); got != before || len(saySent(fake)) != 0 {
		t.Fatalf("%d new messages, typed %q: a broadcast was sent without a confirmation", got-before, saySent(fake))
	}
	sayBcSay(t, c, "tell the api-server agent to stop", sayEnter)
	ms := sayMessages(t, db)[before:]
	if len(ms) != 2 || ms[0].BroadcastID == "" || ms[0].RoutedBy != model.RoutedManual {
		t.Errorf("messages = %+v, want two rows of one broadcast routed manual (no label)", ms)
	}
	if labels, lerr := sayOpen(t, db).RouteLabels(context.Background(), 0, 100); lerr != nil || len(labels) != len(labelsBefore) {
		t.Errorf("labels = %d, %v; want the %d from before and none from the broadcast", len(labels), lerr, len(labelsBefore))
	}
}

func TestSayBroadcastRouterRepoScopeWithOddRepoName(t *testing.T) {
	sessions := []model.Session{
		sayBcSession("A1", "%1", "/work/my repo (v2) [x]*", sendSockA),
		sayBcSession("A2", "%2", "/work/web", sendSockA),
		sayBcSession("A3", "%3", "/work/my repo (v2) [x]*", sendSockA),
	}
	db, _, c := sayBcWorld(t, sessions...)
	sayBcRouter(t, `{"target":"r1","kind":"instruction","confidence":0.9,"runner_up":"","scope":"repo"}`)
	sayDriveView(t, c, sayEnter)
	if _, _, err := sayRun(t, strings.NewReader(""), "everyone", "on", "that", "repo", "pause"); err != nil {
		t.Fatal(err)
	}
	if got := sayBcTargets(t, db); len(got) != 2 || got[0] != "A1" || got[1] != "A3" {
		t.Errorf("targets = %v, want [A1 A3]", got)
	}
}

func TestSayBroadcastInventedScopeAsksTheUser(t *testing.T) {
	db, _, _ := sayBcWorld(t, sayBcSession("A1", "%1", "/work/api-server", sendSockA), sayBcSession("A2", "%2", "/work/web", sendSockA))
	for _, scope := range []string{`"everyone"`, `"ALL"`, `"all\nrm -rf"`, `{"all":true}`, `7`} {
		sayBcRouter(t, `{"target":"s1","kind":"instruction","confidence":0.99,"runner_up":"","scope":`+scope+`}`)
		src := sayNewSource(sayOpen(t, db), nil, nil, sayBcConfig(t), model.AnsweredCLI)
		plan, err := src.propose(context.Background(), "tell everyone to stop")
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.p.Targets) != 0 || plan.p.Auto || plan.p.Routed != model.RoutedManual || !strings.Contains(plan.p.Note, "pick a target") {
			t.Errorf("scope %s: proposal = %+v, want a manual pick and no broadcast", scope, plan.p)
		}
	}
}

func TestSayBroadcastTwoHundredSessions(t *testing.T) {
	sessions := make([]model.Session, 200)
	for i := range sessions {
		sessions[i] = sayBcSession(fmt.Sprintf("S%03d", i), fmt.Sprintf("%%%d", i+1), "/work/api-server", sendSockA)
	}
	db, fake, c := sayBcWorld(t, sessions...)
	sayUseRouter(t, sayRouterNever(t))
	sayDriveView(t, c, sayEnter) // one confirmation
	out, _, err := sayRun(t, strings.NewReader(""), "@all", "continue")
	if err != nil {
		t.Fatal(err)
	}
	ms := sayMessages(t, db)
	if len(ms) != 200 || len(saySent(fake)) != 200 || !strings.Contains(out, "200 sessions: 200 delivered") {
		t.Errorf("%d rows, %d typed lines, output %q; want 200 of each, from one confirmation", len(ms), len(saySent(fake)), out)
	}
	// An agent's per-sender limit (20 in 10 minutes) never binds the user's broadcast: it counts agent sends only.
	st := sayOpen(t, db)
	if _, err = st.CreateBroadcast(context.Background(), []model.Message{{
		Text: "x", Kind: model.KindInstruction, Sender: model.SenderAgent, RoutedBy: model.RoutedSend,
		TargetSession: "S000", Status: model.MsgQueued, CreatedAt: sayNow,
	}}); err == nil {
		t.Error("an agent's broadcast row was stored")
	}
}

func TestSendBroadcastRefusedFromAgent(t *testing.T) {
	db, fake := sendHome(t)
	t.Setenv("TMUX", sendSockA+",1,0")
	sendUnderAgent(t, fake)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("TMUX_PANE", "%2")
	sayIsTerminal = func() bool { return true }
	t.Cleanup(func() { sayIsTerminal = func() bool { return false } })
	for _, addr := range []string{"@all", "@work/*", "@ALL:"} {
		out, err := readRun(t, "y\n", "send", addr, "do", "this")
		if err == nil || !strings.Contains(err.Error(), "an agent cannot broadcast") {
			t.Errorf("send %s: err = %v, out %q; want the agent refusal", addr, err, out)
		}
	}
	if ms := sendMessages(t, db); len(ms) != 0 || len(sendTyped(fake)) != 0 {
		t.Errorf("messages %+v, typed %q; want nothing", ms, sendTyped(fake))
	}
}

func TestSendBroadcastByUserAsksFirst(t *testing.T) {
	db, fake := sendHome(t)
	t.Setenv("TMUX", sendSockA+",1,0")
	sayIsTerminal = func() bool { return true }
	t.Cleanup(func() { sayIsTerminal = func() bool { return false } })
	out, err := readRun(t, "n\n", "send", "@all", "hello")
	if err != nil || !strings.Contains(out, "send to 2 sessions? [y/N]") || !strings.Contains(out, "nothing sent") {
		t.Fatalf("out = %q, err = %v; want the count, the question and nothing sent", out, err)
	}
	if len(sendMessages(t, db)) != 0 {
		t.Fatal("a declined broadcast stored messages")
	}
	if out, err = readRun(t, "y\n", "send", "@all", "hello"); err != nil || !strings.Contains(out, "2 sessions: 2 delivered") {
		t.Fatalf("out = %q, err = %v; want the tally", out, err)
	}
	if ms := sendMessages(t, db); len(ms) != 2 || ms[0].BroadcastID != ms[1].BroadcastID || len(sendTyped(fake)) != 2 {
		t.Errorf("messages %+v, typed %q; want two rows of one broadcast, each typed", ms, sendTyped(fake))
	}
}

func TestMessagesLsShowsABroadcastAsOneEntry(t *testing.T) {
	db, _ := sendHome(t)
	t.Setenv("TMUX", sendSockA+",1,0")
	sayIsTerminal = func() bool { return true }
	t.Cleanup(func() { sayIsTerminal = func() bool { return false } })
	if _, err := readRun(t, "y\n", "send", "@all", "hello"); err != nil {
		t.Fatal(err)
	}
	out, err := readRun(t, "", "messages", "ls", "--status", "all")
	if err != nil || strings.Count(out, "B-1") != 1 || !strings.Contains(out, "2 sessions") ||
		!strings.Contains(out, "M-1") || !strings.Contains(out, "t1") {
		t.Fatalf("out = %q, err = %v; want one B-1 entry with its two targets under it", out, err)
	}
	out, err = readRun(t, "", "messages", "ls", "--status", "all", "--json")
	var rows []struct {
		ID          string `json:"id"`
		BroadcastID string `json:"broadcast_id"`
	}
	if err != nil || json.NewDecoder(bytes.NewReader([]byte(out))).Decode(&rows) != nil || len(rows) != 2 || rows[0].BroadcastID != "B-1" {
		t.Errorf("json = %q, %v; want rows carrying broadcast_id B-1", out, err)
	}
	_ = db
}

func sayBcConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// sayBcSay runs `curia say <text>` in the view, pressing keys.
func sayBcSay(t *testing.T, c *decideClock, text string, keys ...tea.KeyMsg) {
	t.Helper()
	sayDriveView(t, c, keys...)
	if _, _, err := sayRun(t, strings.NewReader(""), strings.Fields(text)...); err != nil {
		t.Fatal(err)
	}
}

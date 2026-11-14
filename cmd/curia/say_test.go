package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// sayUnexpectedRouter is sayRouter under test: no test may start claude,
// so a test that reaches the router without its own stub fails.
func sayUnexpectedRouter(context.Context, string, []byte) ([]byte, error) {
	panic("sayRouter called in a test without a stub: it would start claude")
}

// sayUnexpectedSleep is saySleep under test: a test that reaches the hold
// without its own stub fails instead of sleeping.
func sayUnexpectedSleep(context.Context, time.Duration) error {
	panic("saySleep called in a test without a stub")
}

func init() {
	sayRouter = sayUnexpectedRouter
	saySleep = sayUnexpectedSleep
	sayIsTerminal = func() bool { return false }
}

// sayNow is the fixed clock of the say tests.
var sayNow = at(12, 0)

// sayRepo is the repository of the default session.
const sayRepo = "/work/api-server"

// saySession is session id idle in pane, in repo on branch.
func saySession(id, pane, repo, branch string) model.Session {
	return model.Session{
		ID: id, Agent: "claude", State: model.StateIdle, Pane: pane, TmuxSession: "work", TmuxWindow: "@1",
		Repo: repo, Branch: branch, StartedAt: at(9, 0), UpdatedAt: at(11, 0),
	}
}

// sayWorld isolates HOME and the database, fixes the clock at sayNow, and
// seeds sessions (by default S1 idle in %1, in sayRepo on main) in a
// tmux.Fake that has their panes and shows an idle prompt. It returns the
// database path, the fake and the clock.
func sayWorld(t *testing.T, sessions ...model.Session) (string, *tmux.Fake, *decideClock) {
	t.Helper()
	db := hookHome(t)
	if len(sessions) == 0 {
		sessions = []model.Session{saySession("S1", "%1", sayRepo, "main")}
	}
	seedSessions(t, db, sessions...)
	fake := useFakeTmux(t)
	for _, s := range sessions {
		fake.Panes = append(fake.Panes, tmux.Pane{ID: s.Pane, Session: "work", WindowID: "@1", ServerStart: at(8, 0)})
	}
	fake.Screen = decideIdleScreen
	c := &decideClock{t: sayNow}
	prev := clock
	clock = c.now
	t.Cleanup(func() { clock = prev })
	return db, fake, c
}

// sayConfig writes the config file under the test's HOME.
func sayConfig(t *testing.T, content string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := configFile(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sayAutoConfig turns auto-send on.
const sayAutoConfig = "[routing]\nauto_send = true\n"

// sayUseRouter makes f the router for the test and returns how many times
// it was called.
func sayUseRouter(t *testing.T, f func(context.Context, string, []byte) ([]byte, error)) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	sayRouter = func(ctx context.Context, dir string, prompt []byte) ([]byte, error) {
		n.Add(1)
		return f(ctx, dir, prompt)
	}
	t.Cleanup(func() { sayRouter = sayUnexpectedRouter })
	return &n
}

// sayRouterNever is a router that fails the test when called.
func sayRouterNever(t *testing.T) func(context.Context, string, []byte) ([]byte, error) {
	return func(context.Context, string, []byte) ([]byte, error) {
		t.Error("the router model was called")
		return nil, errors.New("not expected")
	}
}

// sayReply is a router that answers with b.
func sayReply(b []byte) func(context.Context, string, []byte) ([]byte, error) {
	return func(context.Context, string, []byte) ([]byte, error) { return b, nil }
}

// sayFixture reads testdata/router/name.
func sayFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "router", name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sayEnvelope is the real case-0 envelope with its result replaced.
func sayEnvelope(t *testing.T, result string) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(sayFixture(t, "envelope-main-case0.json"), &env); err != nil {
		t.Fatal(err)
	}
	env["result"] = result
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// saySureS1 is the model sure that the message is an instruction for s1,
// with no runner-up.
const saySureS1 = `{"target":"s1","kind":"instruction","confidence":0.95,"runner_up":"","runner_up_confidence":0}`

// sayRun runs `curia say <args>` in-process with stdin.
func sayRun(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(append([]string{"say"}, args...))
	t.Cleanup(func() {
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// sayOpen opens the test's store.
func sayOpen(t *testing.T, db string) *store.Store {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// sayMessages lists every message.
func sayMessages(t *testing.T, db string) []model.Message {
	t.Helper()
	ms, err := sayOpen(t, db).ListMessages(context.Background(), store.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

// sayLast is the newest message, failing the test when there is none.
func sayLast(t *testing.T, db string) model.Message {
	t.Helper()
	ms := sayMessages(t, db)
	if len(ms) == 0 {
		t.Fatal("no message stored")
	}
	return ms[len(ms)-1]
}

// saySent is the SendKeys calls of fake.
func saySent(fake *tmux.Fake) []string {
	var out []string
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "SendKeys ") {
			out = append(out, c)
		}
	}
	return out
}

// sayTasks creates n backlog tasks in repo, T-1 to T-n.
func sayTasks(t *testing.T, db string, n int) {
	t.Helper()
	st := sayOpen(t, db)
	for i := range n {
		if _, err := st.CreateTask(context.Background(), model.Task{
			Title: "task " + string(rune('a'+i%26)), Repo: sayRepo, BaseBranch: "main", Status: model.TaskBacklog, CreatedAt: at(8, 0),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// saySeedEligible marks the oldest n messages auto-eligible.
func saySeedEligible(t *testing.T, db string, n int) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE messages SET auto_eligible = 1 WHERE n <= ?`, n); err != nil {
		t.Fatal(err)
	}
}

// sayDriveView makes curia say see a terminal and replaces the program with
// a driver: it runs the view's start, then for each key moves the clock on
// a second, presses the key and runs what it starts, synchronously.
func sayDriveView(t *testing.T, c *decideClock, keys ...tea.KeyMsg) {
	t.Helper()
	prevProgram, prevTerminal := runProgram, sayIsTerminal
	sayIsTerminal = func() bool { return true }
	runProgram = func(m tea.Model, _ ...tea.ProgramOption) error {
		m = sayPump(t, m, m.Init())
		for _, k := range keys {
			c.advance(time.Second)
			var cmd tea.Cmd
			m, cmd = m.Update(k)
			m = sayPump(t, m, cmd)
		}
		return nil
	}
	t.Cleanup(func() { runProgram, sayIsTerminal = prevProgram, prevTerminal })
}

// sayPump runs cmd and feeds its message back until there is none or the
// view quits.
func sayPump(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	for range 20 {
		if cmd == nil {
			return m
		}
		msg := cmd()
		if _, quit := msg.(tea.QuitMsg); quit || msg == nil {
			return m
		}
		m, cmd = m.Update(msg)
	}
	t.Fatal("the view did not settle")
	return m
}

var (
	sayEnter = tea.KeyMsg{Type: tea.KeyEnter}
	sayTab   = tea.KeyMsg{Type: tea.KeyTab}
)

func TestSayRefusesInsideAgent(t *testing.T) {
	db, fake, _ := sayWorld(t)
	calls := sayUseRouter(t, sayRouterNever(t))
	t.Setenv("CLAUDECODE", "1")
	_, _, err := sayRun(t, strings.NewReader(""), "@api-server", "use", "pnpm")
	if err == nil || !strings.Contains(err.Error(), "inside an agent session") {
		t.Fatalf("err = %v, want the agent refusal", err)
	}
	if ms := sayMessages(t, db); len(ms) != 0 {
		t.Errorf("messages = %+v, want none", ms)
	}
	if calls.Load() != 0 || len(fake.Calls) != 0 {
		t.Errorf("router calls %d, tmux calls %q, want none", calls.Load(), fake.Calls)
	}
}

func TestSayAddressedInstructionIsSentWithoutModel(t *testing.T) {
	db, fake, _ := sayWorld(t)
	sayUseRouter(t, sayRouterNever(t))
	out, _, err := sayRun(t, strings.NewReader(""), "@api-server", "use", "pnpm")
	if err != nil {
		t.Fatal(err)
	}
	if out != "M-1 delivered (send_keys)\n" {
		t.Errorf("output = %q, want the delivery line", out)
	}
	m := sayLast(t, db)
	if m.RoutedBy != model.RoutedAddress || m.Text != "use pnpm" || m.TargetSession != "S1" || m.TargetRepo != sayRepo ||
		m.Status != model.MsgDelivered || m.DeliveredVia != model.ViaSendKeys || m.HasConfidence {
		t.Errorf("message = %+v, want use pnpm to S1, routed by address, delivered by send_keys, no confidence", m)
	}
	want := "SendKeys %1 Message M-1 from the user: use pnpm."
	if sent := saySent(fake); len(sent) != 1 || sent[0] != want {
		t.Errorf("typed %q, want %q", sent, want)
	}
}

func TestSayTaskIdAddress(t *testing.T) {
	t.Run("lead session", func(t *testing.T) {
		s := saySession("S1", "%1", sayRepo, "curia/T-3-x")
		s.TaskID = "T-3"
		db, fake, _ := sayWorld(t, s, saySession("S2", "%2", sayRepo, "main"))
		sayTasks(t, db, 3)
		sayUseRouter(t, sayRouterNever(t))
		if _, _, err := sayRun(t, strings.NewReader(""), "T-3:", "stop", "and", "commit"); err != nil {
			t.Fatal(err)
		}
		m := sayLast(t, db)
		if m.TargetSession != "S1" || m.TargetTask != "T-3" || m.Text != "stop and commit" || m.Status != model.MsgDelivered {
			t.Errorf("message = %+v, want stop and commit delivered to S1 for T-3", m)
		}
		if sent := saySent(fake); len(sent) != 1 || !strings.HasPrefix(sent[0], "SendKeys %1 ") {
			t.Errorf("typed %q, want one line into %%1", sent)
		}
	})
	t.Run("no live agent", func(t *testing.T) {
		db, fake, _ := sayWorld(t)
		sayTasks(t, db, 3)
		sayUseRouter(t, sayRouterNever(t))
		_, _, err := sayRun(t, strings.NewReader(""), "T-3:", "stop", "and", "commit")
		if err == nil || !strings.Contains(err.Error(), "T-3 has no live agent") {
			t.Fatalf("err = %v, want T-3 has no live agent", err)
		}
		ms := sayMessages(t, db)
		if len(ms) != 1 || ms[0].Status != model.MsgCanceled || ms[0].RoutedBy != model.RoutedAddress {
			t.Errorf("messages = %+v, want one canceled, routed by address", ms)
		}
		if sent := saySent(fake); len(sent) != 0 {
			t.Errorf("typed %q, want nothing", sent)
		}
	})
}

func TestSayTeamGoesToLead(t *testing.T) {
	sessions := teamsCapturedSessions()[:3] // lead in %16, alpha-s in %17, beta-s in %18
	branches := []string{"main", "feat-alpha", "feat-beta"}
	for i := range sessions {
		sessions[i].Repo, sessions[i].Branch, sessions[i].TaskID = sayRepo, branches[i], ""
		sessions[i].TmuxSession, sessions[i].TmuxWindow = "work", "@1"
	}
	db, _, _ := sayWorld(t, sessions...)
	teamsWriteCaptured(t, mcpTeams(t))
	sayUseRouter(t, sayRouterNever(t))
	if _, _, err := sayRun(t, strings.NewReader(""), "@feat-alpha", "check", "the", "logs"); err != nil {
		t.Fatal(err)
	}
	if m := sayLast(t, db); m.TargetSession != teamsCapturedLead || m.RoutedBy != model.RoutedAddress {
		t.Errorf("message = %+v, want it addressed to the lead %s", m, teamsCapturedLead)
	}
	if s := getSession(t, db, "alpha-s"); s.TeamRole != model.RoleTeammate {
		t.Errorf("alpha-s role = %q, want teammate (teams refreshed before routing)", s.TeamRole)
	}
}

func TestSayRuleBecomesPrecedent(t *testing.T) {
	db, fake, _ := sayWorld(t)
	sayUseRouter(t, sayRouterNever(t))
	out, _, err := sayRun(t, strings.NewReader(""), "@api-server", "rule:", "always", "use", "pnpm")
	if err != nil {
		t.Fatal(err)
	}
	if out != "M-1 stored as rule D-1 (repo api-server)\n" {
		t.Errorf("output = %q, want the rule line", out)
	}
	sayWantRule(t, db)
	m := sayLast(t, db)
	if m.Kind != model.KindRule || m.Status != model.MsgDelivered || m.DeliveredVia != model.ViaApplied || m.ResultRef != "D-1" {
		t.Errorf("message = %+v, want a rule applied as D-1", m)
	}
	if sent := saySent(fake); len(sent) != 0 {
		t.Errorf("typed %q, want nothing", sent)
	}
}

// sayWantRule checks that D-1 is "always use pnpm" stored as a decided rule
// of sayRepo, found as precedent.
func sayWantRule(t *testing.T, db string) {
	t.Helper()
	d := decisionsGet(t, db, "D-1")
	if d.Status != model.StatusAnswered || len(d.Options) != 0 || d.Scope != model.ScopeRepo || d.Repo != sayRepo ||
		d.Answer != "always use pnpm" || d.AnsweredVia != model.AnsweredCLI {
		t.Errorf("D-1 = %+v, want a decided rule with scope repo %s, via cli", d, sayRepo)
	}
	found, err := sayOpen(t, db).SearchDecisions(context.Background(), store.PrecedentQuery{Text: "pnpm", Repo: sayRepo})
	if err != nil || len(found) != 1 || found[0].ID != "D-1" {
		t.Errorf("SearchDecisions = %+v, %v, want D-1", found, err)
	}
}

func TestSayGlobalRuleNeedsNoRepo(t *testing.T) {
	db, _, c := sayWorld(t)
	sayUseRouter(t, sayReply(sayFixture(t, "envelope-built-prose.json"))) // no suggestion: a manual pick
	// Candidates s1, r1, global: two Tabs reach global.
	sayDriveView(t, c, sayTab, sayTab, sayEnter)
	out, _, err := sayRun(t, strings.NewReader(""), "rule:", "never", "force", "push")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "applied D-1") {
		t.Errorf("output = %q, want the applied rule", out)
	}
	d := decisionsGet(t, db, "D-1")
	if d.Scope != model.ScopeGlobal || d.Repo != "" || d.Answer != "never force push" || d.AnsweredVia != model.AnsweredPopup {
		t.Errorf("D-1 = %+v, want a global rule with no repo, answered in the popup", d)
	}
	if m := sayLast(t, db); m.TargetRepo != "" || m.RoutedBy != model.RoutedManual || m.ResultRef != "D-1" {
		t.Errorf("message = %+v, want a manual pick of global applied as D-1", m)
	}
}

func TestSayTaskIsFiled(t *testing.T) {
	repo := filepath.Join(hermeticGit(t), "api-server")
	gitInit(t, repo, "trunk")
	db, fake, _ := sayWorld(t, saySession("S1", "%1", repo, "trunk"))
	sayUseRouter(t, sayRouterNever(t))
	out, _, err := sayRun(t, strings.NewReader(""), "@api-server", "task:", "fix", "the", "flaky", "login", "test")
	if err != nil {
		t.Fatal(err)
	}
	if out != "M-1 filed as T-1 (backlog)\n" {
		t.Errorf("output = %q, want the task line", out)
	}
	tk, found, err := sayOpen(t, db).GetTask(context.Background(), "T-1")
	if err != nil || !found {
		t.Fatalf("T-1: found %v, err %v", found, err)
	}
	if tk.Title != "fix the flaky login test" || tk.Body != "fix the flaky login test" || tk.Repo != repo ||
		tk.BaseBranch != "trunk" || tk.Status != model.TaskBacklog {
		t.Errorf("T-1 = %+v, want a backlog task in %s from trunk", tk, repo)
	}
	if m := sayLast(t, db); m.ResultRef != "T-1" || m.DeliveredVia != model.ViaApplied {
		t.Errorf("message = %+v, want applied as T-1", m)
	}
	if sent := saySent(fake); len(sent) != 0 {
		t.Errorf("typed %q, want nothing", sent)
	}
}

func TestSayAmbiguousAddressWithoutTerminalFails(t *testing.T) {
	db, fake, _ := sayWorld(t, saySession("S1", "%1", sayRepo, "main"), saySession("S2", "%2", sayRepo, "feat-x"))
	sayUseRouter(t, sayRouterNever(t))
	_, _, err := sayRun(t, strings.NewReader(""), "@api-server", "use", "pnpm")
	if err == nil {
		t.Fatal("say succeeded, want it to list both sessions")
	}
	for _, want := range []string{"@api-server names 2 targets", "api-server · main", "api-server · feat-x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to hold %q", err, want)
		}
	}
	ms := sayMessages(t, db)
	if len(ms) != 1 || ms[0].Status != model.MsgCanceled {
		t.Errorf("messages = %+v, want one canceled", ms)
	}
	if sent := saySent(fake); len(sent) != 0 {
		t.Errorf("typed %q, want nothing", sent)
	}
}

func TestSayUnaddressedWithoutTerminalFails(t *testing.T) {
	db, fake, _ := sayWorld(t)
	calls := sayUseRouter(t, sayReply(sayEnvelope(t, `{"target":"s1","kind":"instruction","confidence":0.8,"runner_up":"r1","runner_up_confidence":0.1}`)))
	_, _, err := sayRun(t, strings.NewReader(""), "use", "pnpm")
	if err == nil || !strings.Contains(err.Error(), "address it, for example @api-server") {
		t.Fatalf("err = %v, want it to say to address the message", err)
	}
	if calls.Load() != 1 {
		t.Errorf("router calls = %d, want 1", calls.Load())
	}
	ms := sayMessages(t, db)
	if len(ms) != 1 {
		t.Fatalf("messages = %+v, want one", ms)
	}
	m := ms[0]
	if m.Status != model.MsgCanceled || m.SuggestedTarget != "session:S1" || m.SuggestedKind != model.KindInstruction ||
		m.RunnerUp != "repo:"+sayRepo || !m.HasConfidence || m.Confidence != 0.8 || m.RoutedBy != model.RoutedManual {
		t.Errorf("message = %+v, want canceled with the suggestion recorded and no label", m)
	}
	if sent := saySent(fake); len(sent) != 0 {
		t.Errorf("typed %q, want nothing", sent)
	}
}

// sayCase0World is case 0's world: S1 idle in %1 working on T-42, the 42nd
// task, so the real envelope's s1 and T-42 are offered.
func sayCase0World(t *testing.T) (string, *tmux.Fake, *decideClock) {
	t.Helper()
	s := saySession("S1", "%1", sayRepo, "curia/T-42-auth-refactor")
	s.TaskID = "T-42"
	db, fake, c := sayWorld(t, s)
	sayTasks(t, db, 42)
	return db, fake, c
}

func TestSayConfirmedInView(t *testing.T) {
	db, fake, c := sayCase0World(t)
	calls := sayUseRouter(t, sayReply(sayFixture(t, "envelope-main-case0.json")))
	sayDriveView(t, c, sayEnter)
	out, _, err := sayRun(t, strings.NewReader(""), "the", "token", "refresh", "should", "retry", "once")
	if err != nil {
		t.Fatal(err)
	}
	m := sayLast(t, db)
	if m.RoutedBy != model.RoutedConfirmed || m.SuggestedTarget != "session:S1" || m.RunnerUp != "task:T-42" ||
		!m.HasConfidence || m.Confidence != 0.7 || m.TargetSession != "S1" || m.Status != model.MsgDelivered {
		t.Errorf("message = %+v, want confirmed s1 with confidence 0.7, delivered", m)
	}
	if !strings.HasPrefix(out, "M-1 instruction → ") || !strings.HasSuffix(out, " · delivered\n") {
		t.Errorf("output = %q, want the view's outcome line", out)
	}
	if calls.Load() != 1 || len(saySent(fake)) != 1 {
		t.Errorf("router calls %d, typed %q, want one each", calls.Load(), saySent(fake))
	}
}

func TestSayOverriddenInView(t *testing.T) {
	db, _, c := sayCase0World(t)
	sayUseRouter(t, sayReply(sayFixture(t, "envelope-main-case0.json")))
	sayDriveView(t, c, sayTab, sayEnter)
	if _, _, err := sayRun(t, strings.NewReader(""), "the", "token", "refresh", "should", "retry", "once"); err != nil {
		t.Fatal(err)
	}
	m := sayLast(t, db)
	if m.RoutedBy != model.RoutedOverridden || m.TargetTask != "T-42" || m.TargetSession != "S1" ||
		m.SuggestedTarget != "session:S1" || m.Confidence != 0.7 {
		t.Errorf("message = %+v, want overridden to the runner-up T-42 (its lead S1)", m)
	}
}

// sayEarned turns auto-send on and seeds 50 accepted labels and eligible
// earlier messages.
func sayEarned(t *testing.T, db string, eligible int) {
	t.Helper()
	sayConfig(t, sayAutoConfig)
	routingSeed(t, db, 50, func(int) bool { return false })
	saySeedEligible(t, db, eligible)
}

func TestSayAutoSendHoldsAndUndoes(t *testing.T) {
	t.Run("undo", sayTestAutoUndo)
	t.Run("release", sayTestAutoRelease)
}

// sayTestAutoUndo: an interrupt during the hold undoes the held message.
func sayTestAutoUndo(t *testing.T) {
	db, fake, _ := sayWorld(t)
	sayEarned(t, db, 1)
	sayUseRouter(t, sayReply(sayEnvelope(t, saySureS1)))
	saySleep = func(_ context.Context, d time.Duration) error {
		m := sayLast(t, db)
		if d != ui.SayHold || m.Status != model.MsgHeld || !m.HoldUntil.Equal(sayNow.Add(ui.SayHold)) {
			t.Errorf("sleep %v over %+v, want the 5 s hold of a held row", d, m)
		}
		return context.Canceled // an interrupt
	}
	t.Cleanup(func() { saySleep = sayUnexpectedSleep })
	_, _, err := sayRun(t, strings.NewReader(""), "use", "pnpm")
	if err == nil || !strings.Contains(err.Error(), "M-51 undone") {
		t.Fatalf("err = %v, want M-51 undone", err)
	}
	if m := sayLast(t, db); m.Status != model.MsgUndone || m.RoutedBy != model.RoutedAuto {
		t.Errorf("message = %+v, want undone, routed auto", m)
	}
	if sent := saySent(fake); len(sent) != 0 {
		t.Errorf("typed %q, want nothing", sent)
	}
}

// sayTestAutoRelease: the hold ends and the message is delivered.
func sayTestAutoRelease(t *testing.T) {
	db, fake, _ := sayWorld(t)
	sayEarned(t, db, 1)
	sayUseRouter(t, sayReply(sayEnvelope(t, saySureS1)))
	saySleep = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { saySleep = sayUnexpectedSleep })
	out, _, err := sayRun(t, strings.NewReader(""), "use", "pnpm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "M-51 delivered (send_keys)\n") {
		t.Errorf("output = %q, want M-51 delivered", out)
	}
	m := sayLast(t, db)
	if m.Status != model.MsgDelivered || m.RoutedBy != model.RoutedAuto || !m.AutoEligible || m.Confidence != 0.95 {
		t.Errorf("message = %+v, want delivered, routed auto, auto-eligible", m)
	}
	if sent := saySent(fake); len(sent) != 1 {
		t.Errorf("typed %q, want one line", sent)
	}
}

// sayTestSource opens a say source over the test's store, as curia say does.
func sayTestSource(t *testing.T, db string, fake tmux.Client) *saySource {
	t.Helper()
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	log, _ := testLogger()
	return sayNewSource(sayOpen(t, db), fake, log, cfg, model.AnsweredCLI)
}

func TestSayAutoNeverForRule(t *testing.T) {
	db, _, _ := sayWorld(t)
	sayEarned(t, db, 1)
	sayUseRouter(t, sayReply(sayFixture(t, "envelope-main-case1.json"))) // global, rule, 0.93
	src := sayTestSource(t, db, &tmux.Fake{})
	p, err := src.Propose(context.Background(), "never force push")
	if err != nil {
		t.Fatal(err)
	}
	if p.Auto || p.Kind != model.KindRule || p.Candidates[0].Key != "global" || !strings.Contains(p.Note, "a rule is always confirmed") {
		t.Errorf("proposal = %+v, want a confirm proposal of a global rule", p)
	}
}

func TestSayAutoSampledAsks(t *testing.T) {
	db, fake, _ := sayWorld(t)
	sayEarned(t, db, 10) // the next eligible message is the 11th
	sayUseRouter(t, sayReply(sayEnvelope(t, saySureS1)))
	src := sayTestSource(t, db, fake)
	p, err := src.Propose(context.Background(), "use pnpm")
	if err != nil {
		t.Fatal(err)
	}
	if p.Auto || p.Routed != model.RoutedConfirmed || !strings.Contains(p.Note, "sampled") {
		t.Errorf("proposal = %+v, want a sampled confirm proposal", p)
	}
	if _, _, serr := sayRun(t, strings.NewReader(""), "use", "pnpm"); serr == nil {
		t.Fatal("say without a terminal sent a sampled message")
	}
	if m := sayLast(t, db); m.Status != model.MsgCanceled || !m.AutoEligible {
		t.Errorf("message = %+v, want canceled and auto-eligible", m)
	}
	// With 11 eligible messages the next one is not sampled.
	if p, err = src.Propose(context.Background(), "use pnpm"); err != nil || !p.Auto {
		t.Errorf("next proposal = %+v, %v, want auto", p, err)
	}
}

func TestSayAutoTargetEndedSinceList(t *testing.T) {
	db, fake, _ := sayWorld(t)
	sayEarned(t, db, 1)
	sayUseRouter(t, sayReply(sayEnvelope(t, saySureS1)))
	src := sayTestSource(t, db, fake)
	ctx := context.Background()
	p, err := src.Propose(ctx, "use pnpm")
	if err != nil || !p.Auto {
		t.Fatalf("proposal = %+v, %v, want auto", p, err)
	}
	ended := saySession("S1", "%1", sayRepo, "main")
	ended.State = model.StateEnded
	seedSessions(t, db, ended)
	if _, err := src.Hold(ctx, ui.Choice{Proposal: p, Target: p.Candidates[0], Kind: p.Kind}); err == nil {
		t.Fatal("Hold held a message for an ended session")
	}
	if m := sayLast(t, db); m.Status != model.MsgCanceled {
		t.Errorf("message = %+v, want canceled, never held", m)
	}
	if sent := saySent(fake); len(sent) != 0 {
		t.Errorf("typed %q, want nothing", sent)
	}
}

func TestAutoTripsAndStaysOff(t *testing.T) {
	db, _, c := sayWorld(t)
	sayConfig(t, sayAutoConfig)
	// 50 labels, the newest two overridden: 48 accepted, earned.
	routingSeed(t, db, 50, func(i int) bool { return i >= 48 })
	if err := sayOpen(t, db).SetRoutingEarned(context.Background(), at(10, 0)); err != nil {
		t.Fatal(err)
	}
	sayUseRouter(t, sayReply(sayEnvelope(t, saySureS1))) // the first eligible one is sampled: asked
	sayDriveView(t, c, sayTab, sayEnter)                 // overridden: 47 of the newest 50 accepted
	out, _, err := sayRun(t, strings.NewReader(""), "use", "pnpm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, sayTrippedNotice) {
		t.Errorf("output = %q, want the trip notice", out)
	}
	if _, tripped := routingState(t, db); tripped.IsZero() {
		t.Fatal("routing_tripped_at not set")
	}
	routingSeed(t, db, 50, func(int) bool { return false })
	p, err := sayTestSource(t, db, &tmux.Fake{}).Propose(context.Background(), "use pnpm")
	if err != nil || p.Auto || !strings.Contains(p.Note, "tripped") {
		t.Errorf("proposal after 50 accepted labels = %+v, %v, want auto-send still off", p, err)
	}
}

func TestSaySweepsOrphanedHeld(t *testing.T) {
	db, _, _ := sayWorld(t)
	held, err := sayOpen(t, db).CreateMessage(context.Background(), model.Message{
		Text: "orphan", Kind: model.KindInstruction, Sender: model.SenderUser, RoutedBy: model.RoutedAuto,
		Status: model.MsgHeld, CreatedAt: sayNow.Add(-45 * time.Second), HoldUntil: sayNow.Add(-40 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	sayUseRouter(t, sayRouterNever(t))
	if _, _, err := sayRun(t, strings.NewReader(""), "@api-server", "use", "pnpm"); err != nil {
		t.Fatal(err)
	}
	if m := sayMessages(t, db)[0]; m.ID != held.ID || m.Status != model.MsgCanceled {
		t.Errorf("orphan = %+v, want canceled", m)
	}
}

func TestSayTextBounds(t *testing.T) {
	for _, c := range []struct {
		name  string
		stdin string
		args  []string
		want  string
	}{
		{"empty", "", []string{""}, "1 to 800 characters"},
		{"blank stdin", " \n\t\n", []string{"-"}, "1 to 800 characters"},
		{"801 runes", "", []string{strings.Repeat("é", 801)}, "1 to 800 characters"},
		{"65 KiB on stdin", strings.Repeat("x", 65<<10), []string{"-"}, "over 64 KiB"},
		{"address only", "", []string{"@api-server"}, "empty after its address"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, fake, _ := sayWorld(t)
			sayUseRouter(t, sayRouterNever(t))
			_, _, err := sayRun(t, strings.NewReader(c.stdin), c.args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to name %q", err, c.want)
			}
			if ms := sayMessages(t, db); len(ms) != 0 {
				t.Errorf("messages = %+v, want none", ms)
			}
			if sent := saySent(fake); len(sent) != 0 {
				t.Errorf("typed %q, want nothing", sent)
			}
		})
	}
}

func TestSayReadsStdin(t *testing.T) {
	db, _, _ := sayWorld(t)
	sayUseRouter(t, sayRouterNever(t))
	if _, _, err := sayRun(t, strings.NewReader("@api-server use pnpm\n"), "-"); err != nil {
		t.Fatal(err)
	}
	if m := sayLast(t, db); m.Text != "use pnpm" || m.Status != model.MsgDelivered {
		t.Errorf("message = %+v, want use pnpm delivered", m)
	}
}

func TestSayFlagLikeTextIsText(t *testing.T) {
	db, _, _ := sayWorld(t)
	sayUseRouter(t, sayRouterNever(t))
	if _, _, err := sayRun(t, strings.NewReader(""), "@api-server", "never", "use", "--force"); err != nil {
		t.Fatal(err)
	}
	if m := sayLast(t, db); m.Text != "never use --force" {
		t.Errorf("text = %q, want the flag kept as text", m.Text)
	}
}

func TestSayQueuedOutcomeSaysWhy(t *testing.T) {
	s := saySession("S1", "%1", sayRepo, "main")
	s.State = model.StateWorking
	db, fake, _ := sayWorld(t, s)
	sayUseRouter(t, sayRouterNever(t))
	out, _, err := sayRun(t, strings.NewReader(""), "@api-server", "use", "pnpm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "M-1 queued: ") || !strings.HasSuffix(out, "tell the agent to call check_messages\n") {
		t.Errorf("output = %q, want the queued line and its reason", out)
	}
	if m := sayLast(t, db); m.Status != model.MsgQueued {
		t.Errorf("message = %+v, want queued", m)
	}
	if sent := saySent(fake); len(sent) != 0 {
		t.Errorf("typed %q into a working session", sent)
	}
}

// sayTrigger makes every INSERT into table fail.
func sayTrigger(t *testing.T, db, table string) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`CREATE TRIGGER say_fail_` + table + ` BEFORE INSERT ON ` + table +
		` BEGIN SELECT RAISE(ABORT, 'test: insert refused'); END`); err != nil {
		t.Fatal(err)
	}
}

// sayChoice proposes text through src and picks the first candidate.
func sayChoice(t *testing.T, src *saySource, text string) ui.Choice {
	t.Helper()
	p, err := src.Propose(context.Background(), text)
	if err != nil {
		t.Fatal(err)
	}
	return ui.Choice{Proposal: p, Target: p.Candidates[0], Kind: p.Kind}
}

// TestSaySendErrorMeansNothingStored pins the view's contract (Task 23,
// decision 3): Send errs only when no message row was stored.
func TestSaySendErrorMeansNothingStored(t *testing.T) {
	t.Run("store fails before the row", sayTestSendStoreFails)
	t.Run("refused target", sayTestSendRefused)
	t.Run("rule fails after the row", sayTestSendRuleFails)
	t.Run("delivery fails after the row", sayTestSendDeliveryFails)
}

func sayTestSendStoreFails(t *testing.T) {
	db, fake, _ := sayWorld(t)
	src := sayTestSource(t, db, fake)
	c := sayChoice(t, src, "@api-server use pnpm")
	sayTrigger(t, db, "messages")
	if _, err := src.Send(context.Background(), c); err == nil {
		t.Error("Send succeeded although no row was stored")
	}
	if ms := sayMessages(t, db); len(ms) != 0 {
		t.Errorf("messages = %+v, want none", ms)
	}
}

func sayTestSendRefused(t *testing.T) {
	db, fake, _ := sayWorld(t)
	sayTasks(t, db, 3)
	src := sayTestSource(t, db, fake)
	_, err := src.Send(context.Background(), sayChoice(t, src, "T-3: stop"))
	if err == nil || !strings.Contains(err.Error(), "T-3 has no live agent") {
		t.Errorf("Send err = %v, want the refusal", err)
	}
	if ms := sayMessages(t, db); len(ms) != 0 {
		t.Errorf("messages = %+v, want none", ms)
	}
}

func sayTestSendRuleFails(t *testing.T) {
	db, fake, _ := sayWorld(t)
	src := sayTestSource(t, db, fake)
	c := sayChoice(t, src, "@api-server rule: always use pnpm")
	sayTrigger(t, db, "decisions")
	out, err := src.Send(context.Background(), c)
	if err != nil {
		t.Fatalf("Send err = %v, want the outcome's note (a row is stored)", err)
	}
	if out.Message.Status != model.MsgCanceled || !strings.Contains(out.Note, "the rule was not stored") {
		t.Errorf("outcome = %+v, want the message canceled and the failure told", out)
	}
	if m := sayLast(t, db); m.Status != model.MsgCanceled {
		t.Errorf("message = %+v, want canceled", m)
	}
}

func sayTestSendDeliveryFails(t *testing.T) {
	db, _, _ := sayWorld(t)
	src := sayTestSource(t, db, unreachableTmux{})
	out, err := src.Send(context.Background(), sayChoice(t, src, "@api-server use pnpm"))
	if err != nil {
		t.Fatalf("Send err = %v, want the outcome's note", err)
	}
	if out.Message.Status != model.MsgQueued || !strings.HasPrefix(out.Note, "queued: ") {
		t.Errorf("outcome = %+v, want queued with the reason", out)
	}
}

func TestSayOutputIsSanitised(t *testing.T) {
	db, _, _ := sayWorld(t,
		saySession("S1", "%1", sayRepo, "\x1b[31mred\x1b[0m"),
		saySession("S2", "%2", sayRepo, "bell\a\u202eevil\x1b]0;title\a"))
	sayTasks(t, db, 1)
	sayUseRouter(t, sayRouterNever(t))
	_, stderr, err := sayRun(t, strings.NewReader(""), "@api-server", "\x1b[2Juse", "pnpm")
	if err == nil {
		t.Fatal("say succeeded, want the ambiguous listing")
	}
	for _, s := range []string{err.Error(), stderr} {
		if strings.ContainsAny(s, "\x1b\a\u202e\r") {
			t.Errorf("output %q holds a control or bidi character", s)
		}
	}
	if !strings.Contains(err.Error(), "red") || !strings.Contains(err.Error(), "evil") {
		t.Errorf("err = %q, want the cleaned labels", err)
	}
}

func TestSayViewWithNoTextOpensInput(t *testing.T) {
	db, _, c := sayWorld(t)
	sayUseRouter(t, sayRouterNever(t))
	keys := []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("@api-server use pnpm")}, sayEnter, sayEnter}
	sayDriveView(t, c, keys...)
	out, _, err := sayRun(t, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if m := sayLast(t, db); m.RoutedBy != model.RoutedAddress || m.Status != model.MsgDelivered {
		t.Errorf("message = %+v, want addressed and delivered", m)
	}
	if !strings.HasPrefix(out, "M-1 instruction → ") {
		t.Errorf("output = %q, want the outcome line", out)
	}
}

func TestSayWithoutTextOrTerminalFails(t *testing.T) {
	sayWorld(t)
	if _, _, err := sayRun(t, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "give the message") {
		t.Errorf("err = %v, want it to ask for the message", err)
	}
}

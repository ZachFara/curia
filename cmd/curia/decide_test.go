package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// decideSessionS1 is the asking session of the seeded tickets, idle in live
// pane %1 of window @1.
func decideSessionS1() model.Session {
	return model.Session{
		ID: "s1", Agent: "claude", State: model.StateIdle, Pane: "%1", TmuxSession: "work",
		TmuxWindow: "@1", StartedAt: at(9, 0), UpdatedAt: at(11, 0),
	}
}

// decideHome seeds specs as decisionsHome does, with session s1 idle in
// live pane %1 of a tmux.Fake, and resets the decide flags afterwards. It
// returns the database path and the fake.
func decideHome(t *testing.T, specs ...decisionsSpec) (string, *tmux.Fake) {
	t.Helper()
	db := decisionsHome(t, specs...)
	seedSessions(t, db, decideSessionS1())
	fake := useFakeTmux(t)
	fake.Panes = []tmux.Pane{{ID: "%1", Session: "work", WindowID: "@1", ServerStart: at(8, 0)}}
	fake.Screen = decideIdleScreen
	decideResetFlags(t)
	return db, fake
}

// decideIdleScreen is the bottom of Claude Code's screen with its input
// prompt empty, the only screen an answer is typed into (Ruling 85).
var decideIdleScreen = "✻ Cooked for 7s\n\n" + strings.Repeat("─", 80) + "\n❯\u00a0\n" +
	strings.Repeat("─", 80) + "\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"

// decideResetFlags restores the decide flag variables after the test.
func decideResetFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		decideOption, decideText, decideReason, decideScope = "", "", "", ""
		decideAccept, decideJSON = false, false
	})
}

// decideRun runs `curia decide <args>` in-process with stdin and returns
// stdout and stderr apart.
func decideRun(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetIn(stdin)
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(append([]string{"decide"}, args...))
	t.Cleanup(func() {
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// decideOpenSpecs are an open blocking ticket (D-1) and an open assume
// ticket (D-2), both of session s1 with options A Postgres and B Redis,
// recommendation A.
func decideOpenSpecs() []decisionsSpec { return decisionsSpecs()[:2] }

func TestDecideRefusesInsideAgent(t *testing.T) {
	// No database yet: refusing must not create one.
	db := hookHome(t)
	decideResetFlags(t)
	t.Setenv("CLAUDECODE", "1")
	prev := runProgram
	runProgram = func(tea.Model, ...tea.ProgramOption) error {
		t.Error("decide ran the view inside an agent session")
		return nil
	}
	t.Cleanup(func() { runProgram = prev })
	for _, args := range [][]string{
		{"D-1", "--option", "A"}, {"D-1", "--accept"}, {"D-1", "--text", "x"}, {}, {"D-1"}, {"117", "--accept"},
	} {
		_, _, err := decideRun(t, strings.NewReader(""), args...)
		if err == nil || !strings.Contains(err.Error(), "curia decide cannot run inside an agent session") {
			t.Errorf("decide %v = %v, want the refusal", args, err)
		}
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("decide inside an agent opened the store: %v", err)
	}

	// With tickets: the ticket stays open.
	db, fake := decideHome(t, decideOpenSpecs()...)
	t.Setenv("CLAUDECODE", "1")
	if _, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "A", "--reason", "r"); err == nil {
		t.Error("decide inside an agent: no error")
	}
	if d := decisionsGet(t, db, "D-1"); d.Status != model.StatusOpen {
		t.Errorf("D-1 = %s, want open", d.Status)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", fake.Calls)
	}
}

func TestDecideOptionAnswers(t *testing.T) {
	db, fake := decideHome(t, decideOpenSpecs()...)
	out, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "A", "--reason", "r", "--scope", "global")
	if err != nil {
		t.Fatal(err)
	}
	if want := "D-1 answered: A: Postgres · delivered (send_keys)\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	d := decisionsGet(t, db, "D-1")
	if d.Status != model.StatusAnswered || d.AnswerKey != "A" || d.AnswerReasoning != "r" ||
		d.AnsweredVia != model.AnsweredCLI || d.Scope != model.ScopeGlobal {
		t.Errorf("D-1 = %s key %q reasoning %q via %s scope %s, want answered A r cli global",
			d.Status, d.AnswerKey, d.AnswerReasoning, d.AnsweredVia, d.Scope)
	}
	if d.Delivery != model.DeliveryDelivered || d.DeliveredVia != model.ViaSendKeys {
		t.Errorf("D-1 delivery = %s via %s, want delivered via send_keys", d.Delivery, d.DeliveredVia)
	}
	want := "SendKeys %1 Decision D-1 answered: option A, Postgres. Reasoning: r. Continue."
	if !slices.Contains(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
}

func TestDecideAcceptConfirmsAssume(t *testing.T) {
	db, fake := decideHome(t, decideOpenSpecs()...)
	out, _, err := decideRun(t, strings.NewReader(""), "D-2", "--accept")
	if err != nil {
		t.Fatal(err)
	}
	if want := "D-2 confirmed · nothing to deliver\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if d := decisionsGet(t, db, "D-2"); d.Status != model.StatusConfirmed || d.AnswerKey != "A" {
		t.Errorf("D-2 = %s key %q, want confirmed A", d.Status, d.AnswerKey)
	}
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "SendKeys") {
			t.Errorf("typed a confirmation: %q", c)
		}
	}
}

func TestDecideTextAndReasonFromStdin(t *testing.T) {
	db, _ := decideHome(t, decideOpenSpecs()...)
	s := decideSessionS1()
	s.State = model.StateWorking
	seedSessions(t, db, s)
	out, _, err := decideRun(t, strings.NewReader("because"), "D-1", "--text", "use the queue", "--reason", "-")
	if err != nil {
		t.Fatal(err)
	}
	if want := "D-1 answered: use the queue · queued: agent is working; the Stop hook will deliver it, or tell the agent to call check_decision D-1\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	d := decisionsGet(t, db, "D-1")
	if d.Answer != "use the queue" || d.AnswerKey != "" || d.AnswerReasoning != "because" || d.Delivery != model.DeliveryQueued {
		t.Errorf("D-1 = answer %q key %q reasoning %q delivery %s", d.Answer, d.AnswerKey, d.AnswerReasoning, d.Delivery)
	}
}

func TestDecideReasonFromStdinIsCapped(t *testing.T) {
	db, _ := decideHome(t, decideOpenSpecs()...)
	big := strings.Repeat("x", 64<<10+1)
	if _, _, err := decideRun(t, strings.NewReader(big), "D-1", "--option", "A", "--reason", "-"); err == nil ||
		!strings.Contains(err.Error(), "64 KiB") {
		t.Errorf("err = %v, want the 64 KiB limit", err)
	}
	if d := decisionsGet(t, db, "D-1"); d.Status != model.StatusOpen {
		t.Errorf("D-1 = %s, want open", d.Status)
	}
}

func TestDecideJSON(t *testing.T) {
	decideHome(t, decideOpenSpecs()...)
	out, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "B", "--reason", "fast", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Errorf("stdout is not one line: %q", out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("not one JSON object: %v\n%s", err, out)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"answer", "answer_key", "delivered_via", "delivery", "id", "note", "status"}; !slices.Equal(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
	want := map[string]any{
		"id": "D-1", "status": "answered", "answer_key": "B", "answer": "B: Redis",
		"delivery": "delivered", "delivered_via": "send_keys", "note": "",
	}
	for k, v := range want {
		if obj[k] != v {
			t.Errorf("%s = %v, want %v", k, obj[k], v)
		}
	}
}

func TestDecideFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"D-1"},
		{"D-1", "--option", "A", "--accept"},
		{"D-1", "--option", "A", "--text", "x"},
		{"D-1", "--accept", "--text", "x"},
		{"--option", "A"},
		{"--accept"},
		{"--text", "x"},
		{"--json"},
		{"D-1", "--option", "A", "--scope", "x"},
		{"117", "--option", "A"},
		{"D-1", "D-2", "--option", "A"},
	} {
		db, fake := decideHome(t, decideOpenSpecs()...)
		prev := runProgram
		runProgram = func(tea.Model, ...tea.ProgramOption) error {
			t.Errorf("decide %v ran the view", args)
			return nil
		}
		out, _, err := decideRun(t, strings.NewReader(""), args...)
		runProgram = prev
		if err == nil {
			t.Errorf("decide %v = %q, want an error", args, out)
		}
		if out != "" {
			t.Errorf("decide %v printed %q", args, out)
		}
		for _, id := range []string{"D-1", "D-2"} {
			if d := decisionsGet(t, db, id); d.Status != model.StatusOpen {
				t.Errorf("decide %v: %s = %s, want open", args, id, d.Status)
			}
		}
		if len(fake.Calls) != 0 {
			t.Errorf("decide %v: tmux calls = %q", args, fake.Calls)
		}
		decideOption, decideText, decideReason, decideScope = "", "", "", ""
		decideAccept, decideJSON = false, false
	}
}

func TestDecideAlreadyAnswered(t *testing.T) {
	decideHome(t, decideOpenSpecs()...)
	if _, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "A", "--reason", "r"); err != nil {
		t.Fatal(err)
	}
	decideOption, decideReason = "", ""
	out, stderr, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "B", "--reason", "r")
	if err == nil || err.Error() != "D-1 is already answered" {
		t.Errorf("second answer = %v, want %q", err, "D-1 is already answered")
	}
	if out != "" || !strings.Contains(stderr, "D-1 is already answered") {
		t.Errorf("second answer printed %q, stderr %q; want the message on stderr only", out, stderr)
	}
}

func TestDecideExpiredOrWithdrawn(t *testing.T) {
	specs := decisionsSpecs()
	decideHome(t, specs...)
	// D-5 is withdrawn, D-6 expired.
	for id, want := range map[string]string{"D-5": "D-5 was withdrawn", "D-6": "D-6 has expired", "D-99": "no decision D-99"} {
		_, _, err := decideRun(t, strings.NewReader(""), id, "--accept")
		if err == nil || err.Error() != want {
			t.Errorf("decide %s = %v, want %q", id, err, want)
		}
		decideAccept = false
	}
	// An assume ticket past its deadline that no sweep has expired yet.
	prev := clock
	clock = func() time.Time { return decisionsParse(t, "2026-11-13T00:00:00Z") }
	t.Cleanup(func() { clock = prev })
	if _, _, err := decideRun(t, strings.NewReader(""), "D-2", "--accept"); err == nil || err.Error() != "D-2 has expired" {
		t.Errorf("decide D-2 = %v, want %q", err, "D-2 has expired")
	}
}

func TestDecideUnknownKeyNamesValidOnes(t *testing.T) {
	db, _ := decideHome(t, decideOpenSpecs()...)
	_, _, err := decideRun(t, strings.NewReader(""), "D-2", "--option", "Z")
	if err == nil || !strings.Contains(err.Error(), "A, B") {
		t.Errorf("err = %v, want the valid keys A, B", err)
	}
	if d := decisionsGet(t, db, "D-2"); d.Status != model.StatusOpen {
		t.Errorf("D-2 = %s, want open", d.Status)
	}
}

func TestDecideOutputIsSanitised(t *testing.T) {
	db, _ := decideHome(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.CreateDecision(context.Background(), model.Decision{
		SessionID: "s1", Question: "q?", Options: []model.Option{
			{Key: "A", Label: "Post\x1b[31mgres\nDROP"}, {Key: "B", Label: "Redis"},
		},
		Recommendation: "A", Rationale: "r", Reversibility: model.ReversCheap, Mode: model.ModeBlocking,
		Scope: model.ScopeRepo, Status: model.StatusOpen, Delivery: model.DeliveryNone, ArgsHash: "h",
		CreatedAt: fleetNow,
	})
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "A", "--reason", "r")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 1 || strings.ContainsRune(out, 0x1b) {
		t.Errorf("stdout = %q, want one line with no ESC", out)
	}
	if !strings.HasPrefix(out, "D-1 answered: A: Postgres DROP · ") {
		t.Errorf("stdout = %q", out)
	}
}

// decideClock is a settable clock for driving the view.
type decideClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *decideClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *decideClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// decideDriveView replaces the terminal program with one that loads the
// view, waits a second of the view's clock, presses key and runs the answer
// it sends. It returns the number of program options passed.
func decideDriveView(t *testing.T, key string) *int {
	t.Helper()
	c := &decideClock{t: decisionsParse(t, decisionsNow)}
	prevClock, prevProgram := clock, runProgram
	clock = c.now
	var opts int
	runProgram = func(m tea.Model, o ...tea.ProgramOption) error {
		opts = len(o)
		m, _ = m.Update(m.Init()())
		c.advance(time.Second)
		m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd == nil {
			t.Fatalf("key %q sent no answer; view:\n%s", key, m.View())
		}
		_, _ = m.Update(cmd())
		return nil
	}
	t.Cleanup(func() { clock, runProgram = prevClock, prevProgram })
	return &opts
}

func TestDecideViaSSH(t *testing.T) {
	for _, c := range []struct {
		ssh  string
		want model.AnsweredVia
	}{{"10.0.0.1 5000 10.0.0.2 22", model.AnsweredSSH}, {"", model.AnsweredPopup}} {
		db, _ := decideHome(t, decisionsSpecs()[1]) // D-1: an open, cheap assume ticket
		t.Setenv("SSH_CONNECTION", c.ssh)
		opts := decideDriveView(t, "2")
		if _, _, err := decideRun(t, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		if *opts != 1 {
			t.Errorf("program options = %d, want the alternate screen only", *opts)
		}
		d := decisionsGet(t, db, "D-1")
		if d.Status != model.StatusOverridden || d.AnsweredVia != c.want {
			t.Errorf("SSH_CONNECTION %q: D-1 = %s via %s, want overridden via %s", c.ssh, d.Status, d.AnsweredVia, c.want)
		}
	}
}

func TestDecideSourceExpiresAndSorts(t *testing.T) {
	const z = ".000000000Z"
	db, fake := decideHome(t,
		decisionsSpec{"blocking new", model.ModeBlocking, "2026-11-11T08:00:00" + z, "", decisionsEnd{}},
		decisionsSpec{"assume due", model.ModeAssume, "2026-11-11T09:00:00" + z, "2026-11-11T11:00:00" + z, decisionsEnd{}},
		decisionsSpec{"assume live", model.ModeAssume, "2026-11-11T07:00:00" + z, "2026-11-12T07:00:00" + z, decisionsEnd{}},
		decisionsSpec{"blocking old", model.ModeBlocking, "2026-11-11T06:00:00" + z, "", decisionsEnd{}},
	)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	log, _ := testLogger()
	src := newDecideSource(st, fake, log)
	tickets, err := src.Tickets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tk := range tickets {
		ids = append(ids, tk.ID)
	}
	if want := []string{"D-4", "D-1", "D-3"}; !slices.Equal(ids, want) {
		t.Errorf("tickets = %q, want %q", ids, want)
	}
	if d := decisionsGet(t, db, "D-2"); d.Status != model.StatusExpired {
		t.Errorf("D-2 = %s, want expired", d.Status)
	}
}

// The view never sends an answer twice, and a retry of one already stored
// is refused by the store and delivers nothing more.
func TestDecideSourceAnswerIsNotRedelivered(t *testing.T) {
	db, fake := decideHome(t, decideOpenSpecs()...)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	log, _ := testLogger()
	src := newDecideSource(st, fake, log)
	a := model.Answer{Key: "A", Reasoning: "r", Via: model.AnsweredPopup}
	out, err := src.Answer(context.Background(), "D-1", a)
	if err != nil || out.Note != "" || out.Decision.DeliveredVia != model.ViaSendKeys {
		t.Fatalf("first answer = %+v, %v, want delivered", out, err)
	}
	fake.Calls = nil
	_, err = src.Answer(context.Background(), "D-1", a)
	if !errors.Is(err, store.ErrNotOpen) || err.Error() != "D-1 is already answered" {
		t.Errorf("retry err = %v, want %q matching store.ErrNotOpen", err, "D-1 is already answered")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("retry tmux calls = %q, want none", fake.Calls)
	}
}

// A delivery that fails is not a command failure: the answer is stored and
// the line says what happened, with a WARN in the log and no ERROR.
func TestDecideDeliveryFailureIsReportedNotFailed(t *testing.T) {
	db, _ := decideHome(t, decideOpenSpecs()...)
	useDownTmux(t)
	out, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "A", "--reason", "r")
	if err != nil {
		t.Fatalf("decide = %v, want success", err)
	}
	if want := "D-1 answered: A: Postgres · queued: tmux unreachable; queued for the Stop hook, or tell the agent to call check_decision D-1\n"; out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if d := decisionsGet(t, db, "D-1"); d.Status != model.StatusAnswered || d.Delivery != model.DeliveryQueued {
		t.Errorf("D-1 = %s, %s, want answered, queued", d.Status, d.Delivery)
	}
	log := strings.Join(readLog(t), "\n")
	if !strings.Contains(log, "level=WARN") || strings.Contains(log, "level=ERROR") {
		t.Errorf("log = %q, want WARN and no ERROR", log)
	}
}

// decidePartialTmux acts out a line typed but not submitted.
func decidePartialTmux(t *testing.T) *tmux.Fake {
	t.Helper()
	fake := useFakeTmux(t)
	fake.Panes = []tmux.Pane{{ID: "%1", Session: "work", WindowID: "@1", ServerStart: at(8, 0)}}
	fake.SendKeysErr = tmux.ErrPartialSend
	fake.Screen = decideIdleScreen
	return fake
}

func TestDecidePartialSendTellsTheUser(t *testing.T) {
	db, _ := decideHome(t, decideOpenSpecs()...)
	fake := decidePartialTmux(t)
	out, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "A", "--reason", "r")
	if err != nil {
		t.Fatalf("decide = %v, want success", err)
	}
	for _, want := range []string{"D-1 answered: A: Postgres · typing may not have finished: check the agent's pane", "check_decision D-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want it to hold %q", out, want)
		}
	}
	n := 0
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "SendKeys") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("SendKeys calls = %d, want 1", n)
	}
	if d := decisionsGet(t, db, "D-1"); d.Delivery != model.DeliveryDelivered || d.DeliveredVia != model.ViaSendKeys {
		t.Errorf("D-1 delivery = %s via %q, want still claimed by send_keys", d.Delivery, d.DeliveredVia)
	}
}

func TestDecideHelpInsideAgentIsExempt(t *testing.T) {
	db := hookHome(t)
	decideResetFlags(t)
	t.Setenv("CLAUDECODE", "1")
	t.Cleanup(func() {
		if c, _, err := rootCmd.Find([]string{"decide"}); err == nil {
			_ = c.Flags().Set("help", "false")
		}
	})
	out, _, err := decideRun(t, strings.NewReader(""), "--help")
	if err != nil || !strings.Contains(out, "Usage:") {
		t.Errorf("decide --help = %q, %v, want usage", out, err)
	}
	if _, statErr := os.Stat(db); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("help touched the database: %v", statErr)
	}
}

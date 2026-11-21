package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/mcp"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// rcSeq is a fake tmux whose captures after the first SendKeys come from
// after, in order (the last one repeats); before it, from the Fake's Screen.
type rcSeq struct {
	*tmux.Fake
	after []string
	n     int
}

func (r *rcSeq) CapturePane(ctx context.Context, pane string) (tmux.Capture, error) {
	if len(sendTyped(r.Fake)) > 0 && len(r.after) > 0 {
		r.Screen = r.after[min(r.n, len(r.after)-1)]
		r.n++
	}
	return r.Fake.CapturePane(ctx, pane)
}

// rcHome is a fleet of idle sessions in %1 and %2 with fake time: every sleep
// advances rcNow, and slept collects them.
func rcHome(t *testing.T, after ...string) (string, *rcSeq, *[]time.Duration) {
	t.Helper()
	s1, s2 := readSession("s1", model.StateIdle, "%1"), readSession("s2", model.StateIdle, "%2")
	s1.Branch, s1.Repo = "feat/x", "curia"
	db, fake := readHome(t, []string{"%1", "%2"}, s1, s2)
	seq := &rcSeq{Fake: fake, after: after}
	newTmuxClient = func() tmux.Client { return seq }
	now, slept := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), &[]time.Duration{}
	prevNow, prevSleep := rcNow, rcSleep
	rcNow = func() time.Time { return now }
	rcSleep = func(_ context.Context, d time.Duration) { now = now.Add(d); *slept = append(*slept, d) }
	t.Cleanup(func() { rcNow, rcSleep = prevNow, prevSleep })
	sayIsTerminal = func() bool { return true }
	t.Cleanup(func() { sayIsTerminal = func() bool { return false } })
	return db, seq, slept
}

func rcStored(t *testing.T, db string) model.Session {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s, _, err := st.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rcFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("..", "..", "internal", "deliver", "testdata", "prompt", name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRCTypesOneFixedLineIntoAnIdleEmptyPrompt(t *testing.T) {
	db, fake, _ := rcHome(t, decideIdleScreen)
	out, err := readRun(t, "\n", "rc", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sendTyped(fake.Fake), []string{"SendKeys %1 /rc feat/x · curia"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("typed %q, want exactly %q", got, want)
	}
	if !strings.Contains(out, "outside this machine") {
		t.Errorf("output %q lacks the warning that Remote Control exposes the session", out)
	}
	if !strings.Contains(out, "s1") || !strings.Contains(out, "confirmed") {
		t.Errorf("output %q, want s1 reported confirmed", out)
	}
	s := rcStored(t, db)
	if s.RC != "unknown" || s.RCHow != "typed" || s.RCCheckedAt.IsZero() {
		t.Errorf("stored rc = %q %q at %v, want unknown (no indicator seen), typed, a time", s.RC, s.RCHow, s.RCCheckedAt)
	}
}

func TestRCNeverTypesIntoARefusedScreen(t *testing.T) {
	for _, f := range []string{
		"lc3-draft-oneline.txt", "lc3-dialog-permission-early.txt", "lc3-busy-running-turn.txt",
		"lc3-shell-after-exit.txt", "lc3-dialog-trust-folder.txt", "lc5-survey-idle.txt", "lc3-menu-rewind.txt",
	} {
		t.Run(f, func(t *testing.T) {
			_, fake, _ := rcHome(t)
			fake.Screen = rcFixture(t, f)
			out, err := readRun(t, "\n", "rc", "s1")
			if err != nil {
				t.Fatal(err)
			}
			if typed := sendTyped(fake.Fake); len(typed) != 0 {
				t.Errorf("typed %q into %s", typed, f)
			}
			if !strings.Contains(out, "nothing typed") {
				t.Errorf("output %q does not say why nothing was typed", out)
			}
		})
	}
}

func TestRCDoesNotTypeIntoABusyStoredSession(t *testing.T) {
	_, fake, _ := rcHome(t)
	st, _ := store.Open(os.Getenv("CURIA_DB"))
	s := readSession("w", model.StateWorking, "%3")
	_ = st.UpsertSession(context.Background(), s)
	_ = st.Close()
	fake.Panes = append(fake.Panes, tmux.Pane{ID: "%3", Session: "work", WindowID: "@3", ServerStart: time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)})
	if _, err := readRun(t, "\n", "rc", "w"); err != nil {
		t.Fatal(err)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 0 {
		t.Errorf("typed %q into a working session", typed)
	}
}

func TestRCConfirmationNeedsTheEmptyPromptAndIsBounded(t *testing.T) {
	panel := "Remote Control\n\n" + strings.Repeat("─", 80) + "\nEsc to close\n"
	db, fake, slept := rcHome(t, panel) // the panel never closes
	out, err := readRun(t, "\n", "rc", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not confirmed") {
		t.Errorf("output %q, want not confirmed when the prompt never comes back", out)
	}
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	if total < 5*time.Second || total > 6*time.Second || len(*slept) > 12 {
		t.Errorf("waited %v in %d sleeps, want about 5s in a bounded number", total, len(*slept))
	}
	if s := rcStored(t, db); s.RCHow != "unconfirmed" {
		t.Errorf("rc_how = %q, want unconfirmed", s.RCHow)
	}
	// An unconfirmed line blocks further typing.
	if _, err = readRun(t, "\n", "rc", "s1"); err != nil {
		t.Fatal(err)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 1 {
		t.Errorf("typed %q, want the first line only", typed)
	}
}

func TestRCConfirmedOnceThePromptReturnsAndUnknownWhenItCannotTell(t *testing.T) {
	_, _, slept := rcHome(t, "working…\n", decideIdleScreen)
	out, err := readRun(t, "\n", "rc", "s1")
	if err != nil || !strings.Contains(out, "confirmed") || strings.Contains(out, "not confirmed") {
		t.Fatalf("out %q err %v, want confirmed once the prompt came back", out, err)
	}
	if len(*slept) != 1 {
		t.Errorf("slept %d times, want it to stop at the first empty prompt", len(*slept))
	}
	_, _, _ = rcHome(t, "some other program\n")
	out, err = readRun(t, "\n", "rc", "s1")
	if err != nil || !strings.Contains(out, "unknown") {
		t.Errorf("out %q err %v, want unknown for a screen it cannot read", out, err)
	}
}

func TestRCIndicatorTableOnlyReadsTheFooter(t *testing.T) {
	cases := []struct {
		name, screen, state string
	}{
		{"indicator in the footer", decideIdleScreen + "  /rc active\n", "on"},
		{"disconnect notice", decideIdleScreen + "  Remote Control not started here\n", "off"},
		{"nothing", decideIdleScreen, ""},
		{"agent output far above", "/rc active\n" + strings.Repeat("text\n", 10) + decideIdleScreen, ""},
	}
	for _, c := range cases {
		if got, _ := rcReadIndicator(c.screen); got != c.state {
			t.Errorf("%s: state %q, want %q", c.name, got, c.state)
		}
	}
}

func TestRCCheckTypesNothingAndNeverClaimsOnWithoutTheIndicator(t *testing.T) {
	db, fake, _ := rcHome(t)
	fake.Screen = decideIdleScreen + "  Remote Control not started here\n"
	out, err := readRun(t, "", "rc", "--check")
	if err != nil {
		t.Fatal(err)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 0 {
		t.Errorf("--check typed %q", typed)
	}
	for _, c := range fake.Calls {
		if strings.HasPrefix(c, "SendKeys") {
			t.Errorf("--check called %q", c)
		}
	}
	if !strings.Contains(out, "s1") || strings.Contains(out, " on") {
		t.Errorf("output %q, want per-session lines and no 'on'", out)
	}
	if s := rcStored(t, db); s.RC != "off" {
		t.Errorf("stored rc = %q, want off from the disconnect notice", s.RC)
	}
	fake.Screen = decideIdleScreen
	if _, err = readRun(t, "", "rc", "--check"); err != nil {
		t.Fatal(err)
	}
	if s := rcStored(t, db); s.RC == "on" {
		t.Error("rc is on without the indicator")
	}
	// an idle empty prompt is where typing WOULD be allowed
	if typed := sendTyped(fake.Fake); len(typed) != 0 {
		t.Errorf("--check typed %q on an idle screen", typed)
	}
}

func TestRCCheckClearsAnUnconfirmedBlockWhenThePromptIsBack(t *testing.T) {
	db, fake, _ := rcHome(t, "Remote Control\nEsc to close\n")
	if _, err := readRun(t, "\n", "rc", "s1"); err != nil {
		t.Fatal(err)
	}
	fake.after = nil
	fake.Screen = decideIdleScreen + "  /rc active\n"
	if _, err := readRun(t, "", "rc", "--check"); err != nil {
		t.Fatal(err)
	}
	if s := rcStored(t, db); s.RC != "on" || s.RCHow != "indicator" {
		t.Errorf("stored rc = %q %q, want on via the indicator", s.RC, s.RCHow)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 1 {
		t.Errorf("typed %q, want only the first rc line", typed)
	}
}

func TestRCAllListsTheSessionsAndNeedsY(t *testing.T) {
	_, fake, _ := rcHome(t, decideIdleScreen)
	out, err := readRun(t, "n\n", "rc", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "s1") || !strings.Contains(out, "s2") || !strings.Contains(out, "[y/N]") {
		t.Errorf("output %q, want the sessions listed and a y/N question", out)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 0 {
		t.Errorf("typed %q after a no", typed)
	}
	if _, err = readRun(t, "y\n", "rc", "--all"); err != nil {
		t.Fatal(err)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 2 || !strings.HasPrefix(typed[0], "SendKeys %1 /rc") || !strings.HasPrefix(typed[1], "SendKeys %2 /rc") {
		t.Errorf("typed %q, want one /rc line into each of %%1 and %%2", typed)
	}
}

func TestRCRefusesWithoutATerminalAndForAnAgent(t *testing.T) {
	_, fake, _ := rcHome(t)
	sayIsTerminal = func() bool { return false }
	if _, err := readRun(t, "y\n", "rc", "--all"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("err = %v, want a refusal for no terminal", err)
	}
	userTerminalAs(t, false)
	if _, err := readRun(t, "y\n", "rc", "s1"); err == nil || !strings.Contains(err.Error(), "only the user may") {
		t.Errorf("err = %v, want the user guard", err)
	}
	t.Setenv("CLAUDECODE", "1")
	userTerminalAs(t, true)
	if _, err := readRun(t, "y\n", "rc", "--check"); err == nil || !strings.Contains(err.Error(), "agent session") {
		t.Errorf("--check as an agent: err = %v, want refused", err)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 0 {
		t.Errorf("typed %q", typed)
	}
}

func TestRCNameIsCleanedAndBounded(t *testing.T) {
	long := strings.Repeat("title ", 20)
	cases := []struct {
		task, title, branch, repo, want string
	}{
		{"T-3", long, "", "curia", "T-3 title title title title title title titl · curia"},
		{"", "", "feat/x", "curia", "feat/x · curia"},
		{"", "", "x\n/exit\x1b[31m", "r", "x /exit · r"},
		{"", "", "--dangerously", "r", "dangerously · r"},
		{"", "", "`rm -rf ~`; $(id)", "", "rm -rf id"},
		{"", "", "", "", ""},
	}
	for _, c := range cases {
		got := rcName(model.Session{TaskID: c.task, Branch: c.branch, Repo: c.repo}, c.title)
		if got != c.want {
			t.Errorf("rcName(%q, %q, %q) = %q, want %q", c.task, c.branch, c.repo, got, c.want)
		}
		if strings.ContainsAny(got, "\n\r\x1b`$;") || len([]rune(got)) > 120 {
			t.Errorf("rcName = %q holds a control, shell character or is long", got)
		}
	}
}

func TestRCHostileSessionIDInTheArgumentIsRefused(t *testing.T) {
	_, fake, _ := rcHome(t)
	out, err := readRun(t, "\n", "rc", "s1\n/exit")
	if err == nil {
		t.Fatalf("out %q: a hostile target was accepted", out)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error %q holds a line break", err)
	}
	if typed := sendTyped(fake.Fake); len(typed) != 0 {
		t.Errorf("typed %q", typed)
	}
}

func TestRCStaysOutOfTheMCPToolList(t *testing.T) {
	var got []string
	for _, tool := range mcp.Tools(mcp.Deps{}) {
		got = append(got, tool.Name)
	}
	want := "search_decisions request_decision check_decision withdraw_decision check_messages complete_task file_task"
	if strings.Join(got, " ") != want {
		t.Errorf("MCP tools = %q, want exactly %q: Remote Control adds none", got, want)
	}
}

func TestLaunchAddsTheRemoteControlFlagOnlyWithTheKeyOn(t *testing.T) {
	argv := []string{"claude", "--model", "opus", "the brief"}
	if got := launchWithRC(argv, false, "T-1 title"); strings.Join(got, "|") != strings.Join(argv, "|") {
		t.Errorf("key off: argv = %q, want it unchanged", got)
	}
	got := launchWithRC(argv, true, "T-1 title")
	if len(got) != 6 || got[0] != "claude" || got[1] != "--remote-control" || got[2] != "T-1 title" || got[5] != "the brief" {
		t.Errorf("key on: argv = %q, want --remote-control with an explicit name right after claude", got)
	}
	if got = launchWithRC([]string{"codex", "x"}, true, "n"); got[0] != "codex" || len(got) != 2 {
		t.Errorf("codex argv = %q, want it unchanged", got)
	}
}

func TestLaunchPassesTheFlagOnlyWhenTheUserFileTurnsTheKeyOn(t *testing.T) {
	tk := launchTask(t)
	rec := launchRecord(t)
	if _, stderr, err := launchRun(t, &launchEnter{}, tk.ID); err != nil {
		t.Fatalf("launch: %v\n%s", err, stderr)
	}
	if slices.Contains(rec.argv, "--remote-control") {
		t.Errorf("argv %q holds --remote-control with the key off", rec.argv)
	}
	home, _ := os.UserHomeDir()
	file := configFile(home)
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("[rc]\nlaunch = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := launchRun(t, &launchEnter{}, tk.ID); err != nil {
		t.Fatalf("launch: %v\n%s", err, stderr)
	}
	if len(rec.argv) < 3 || rec.argv[1] != "--remote-control" || !strings.HasPrefix(rec.argv[2], tk.ID) {
		t.Errorf("argv %q, want --remote-control <task id and title> after claude", rec.argv)
	}
}

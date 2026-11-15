package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/team"
)

// reviewRunCmd runs `curia args...` in-process with stdin and returns
// stdout and stderr apart; the review flags are reset afterwards.
func reviewRunCmd(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetIn(strings.NewReader(stdin))
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	defer func() {
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		reviewApproveFlag, reviewMergeFlag, reviewPRFlag = false, false, false
		reviewChangesFlag, reviewDropFlag, reviewJSONFlag = false, false, false
		reviewNoteFlag = ""
	}()
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// reviewWithLead is reviewNew with the lead session linked to T-1, idle in
// the task's window (pane %100, window @100), which shows Claude Code's idle
// prompt.
func reviewWithLead(t *testing.T, state model.State) *reviewFixture {
	t.Helper()
	f := reviewNew(t)
	f.fake.Panes[0].WindowID = "@100"
	f.fake.Screen = decideIdleScreen
	reviewSession(f, lifecycleLead, "%100", "@100", state, f.now)
	return f
}

// reviewSession links session id, in pane and window, to T-1.
func reviewSession(f *reviewFixture, id, pane, window string, state model.State, updated time.Time) {
	f.t.Helper()
	if err := f.st.UpsertSession(context.Background(), model.Session{
		ID: id, Agent: "claude", State: state, Pane: pane, TmuxWindow: window, TmuxSession: "work", TaskID: "T-1",
		StartedAt: f.start, UpdatedAt: updated,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// reviewTicket opens a ticket of mode on T-1.
func reviewTicket(f *reviewFixture, question string, mode model.Mode) model.Decision {
	f.t.Helper()
	d := model.Decision{
		SessionID: lifecycleLead, TaskID: "T-1", Repo: f.repo, Question: question,
		Options: []model.Option{{Key: "A", Label: "Yes"}, {Key: "B", Label: "No"}}, Recommendation: "A",
		Rationale: "Simple.", Reversibility: model.ReversCostly, Mode: mode, Scope: model.ScopeTask,
		Status: model.StatusOpen, ArgsHash: "hash-" + question, Delivery: model.DeliveryNone, CreatedAt: f.now,
	}
	if mode == model.ModeAssume {
		d.Assumption = "Yes"
		d.ExpiresAt = f.now.Add(24 * time.Hour)
	}
	stored, _, err := f.st.CreateDecision(context.Background(), d)
	if err != nil {
		f.t.Fatal(err)
	}
	return stored
}

func (f *reviewFixture) decision(id string) model.Decision {
	f.t.Helper()
	d, found, err := f.st.GetDecision(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("GetDecision(%s): %v %v", id, found, err)
	}
	return d
}

func (f *reviewFixture) messages() []model.Message {
	f.t.Helper()
	ms, err := f.st.ListMessages(context.Background(), store.MessageFilter{})
	if err != nil {
		f.t.Fatal(err)
	}
	return ms
}

// reviewConfig writes the config file with content.
func reviewConfig(t *testing.T, content string) {
	t.Helper()
	worktreeWrite(t, configFile(os.Getenv("HOME")), content)
}

func TestReviewRefusesInsideAgent(t *testing.T) {
	db := hookHome(t)
	t.Setenv("CLAUDECODE", "1")
	prev := reviewIsTerminal
	reviewIsTerminal = func() bool { return true }
	t.Cleanup(func() { reviewIsTerminal = prev })
	for _, args := range [][]string{
		{"review"},
		{"review", "R-1"},
		{"review", "R-1", "--approve"},
		{"review", "T-1", "--approve", "--pr"},
		{"review", "R-1", "--request-changes", "--note", "fix it"},
		{"review", "R-1", "--drop"},
		{"review", "R-1", "--json"},
	} {
		_, _, err := reviewRunCmd(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "inside an agent session") {
			t.Errorf("%q: error = %v, want the refusal", args, err)
		}
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the store file exists (%v); nothing may be opened", err)
	}
	// The action functions refuse too, whoever calls them.
	if _, err := reviewDrop(context.Background(), nil, "R-1", model.AnsweredCLI, time.Now()); err == nil {
		t.Error("reviewDrop ran inside an agent session")
	}
	if _, err := reviewApprove(context.Background(), nil, config.Default(), "R-1", "", "", model.AnsweredCLI, time.Now()); err == nil {
		t.Error("reviewApprove ran inside an agent session")
	}
	if _, err := reviewRequestChanges(context.Background(), nil, nil, nil, "R-1", "x", model.AnsweredCLI, time.Now()); err == nil {
		t.Error("reviewRequestChanges ran inside an agent session")
	}
}

func TestReviewApproveMerges(t *testing.T) {
	f := reviewNew(t)
	head := f.head(f.wt, "HEAD")
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--approve")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	want := "R-1 approved: merged " + reviewBranch + " into main (" + head[:7] + "). Remove the worktree and branch with: curia clean T-1\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	r := f.review()
	if r.Status != model.ReviewMerged || r.Head != head || r.ApprovedHow != model.ApproveMerge ||
		r.ClosedVia != model.AnsweredCLI || !strings.Contains(r.DiffStat, "auth.go") {
		t.Errorf("review = %+v, want merged at %s with its stat, how merge, via cli", r, head)
	}
	if got := f.head(f.repo, "main^2"); got != head {
		t.Errorf("main^2 = %s, want %s", got, head)
	}
}

func TestReviewApproveExpiresAssumptions(t *testing.T) {
	f := reviewNew(t)
	a1 := reviewTicket(f, "Assume v2 endpoint?", model.ModeAssume)
	a2 := reviewTicket(f, "Assume 5 minute margin?", model.ModeAssume)
	b := reviewTicket(f, "Rotate the key?", model.ModeBlocking)
	stdout, stderr, err := reviewRunCmd(t, "", "review", "T-1", "--approve", "--merge")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "). 2 unconfirmed assumptions expired. Remove the worktree") {
		t.Errorf("stdout = %q, want 2 expired", stdout)
	}
	for _, id := range []string{a1.ID, a2.ID} {
		if d := f.decision(id); d.Status != model.StatusExpired {
			t.Errorf("%s is %s, want expired", id, d.Status)
		}
	}
	if d := f.decision(b.ID); d.Status != model.StatusOpen {
		t.Errorf("blocking %s is %s, want open", b.ID, d.Status)
	}
}

func TestReviewApprovePRFlagAndConfig(t *testing.T) {
	f := reviewNew(t)
	reviewUseGH(t, "https://github.com/o/r/pull/3\n")
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--approve", "--pr")
	if err != nil {
		t.Fatalf("--pr: %v\n%s", err, stderr)
	}
	if r := f.review(); r.Status != model.ReviewApproved || r.PRURL != "https://github.com/o/r/pull/3" || r.ApprovedHow != model.ApprovePR {
		t.Errorf("--pr: review = %+v", r)
	}
	if stdout != "R-1 approved: pull request https://github.com/o/r/pull/3 opened for "+reviewBranch+".\n" {
		t.Errorf("--pr: stdout = %q", stdout)
	}

	g := reviewNew(t)
	reviewConfig(t, "[review]\napprove = \"pr\"\n")
	gh := reviewUseGH(t, "https://github.com/o/r/pull/4\n")
	if _, stderr, err = reviewRunCmd(t, "", "review", "R-1", "--approve"); err != nil {
		t.Fatalf("config pr: %v\n%s", err, stderr)
	}
	if r := g.review(); r.Status != model.ReviewApproved || r.PRURL != "https://github.com/o/r/pull/4" || len(gh.calls) != 1 {
		t.Errorf("config pr: review = %+v, gh calls %d", r, len(gh.calls))
	}

	h := reviewNew(t)
	reviewConfig(t, "[review]\napprove = \"pr\"\n")
	gh = reviewUseGH(t, "")
	if _, stderr, err = reviewRunCmd(t, "", "review", "R-1", "--approve", "--merge"); err != nil {
		t.Fatalf("--merge over config: %v\n%s", err, stderr)
	}
	if r := h.review(); r.Status != model.ReviewMerged || len(gh.calls) != 0 {
		t.Errorf("--merge over config: review = %+v, gh calls %d", r, len(gh.calls))
	}
}

func TestReviewApproveFailureStaysPending(t *testing.T) {
	f := reviewNew(t)
	f.commitOnBranch("README", "branch side\n", "branch readme")
	f.commitOnMain("README", "main side\n", "main readme")
	before := f.head(f.repo, "HEAD")
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--approve")
	if err == nil || !strings.Contains(stderr, "; nothing was merged") {
		t.Fatalf("err %v, stderr %q; want the failure", err, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	f.wantPending()
	if f.head(f.repo, "HEAD") != before || f.status() != "" {
		t.Error("the repository changed")
	}
	// A working agent: refused before any git that changes something.
	g := reviewWithLead(t, model.StateWorking)
	if _, stderr, err = reviewRunCmd(t, "", "review", "R-1", "--approve"); err == nil ||
		!strings.Contains(stderr, "the agent is still working; wait for it to stop") {
		t.Errorf("working agent: err %v, stderr %q", err, stderr)
	}
	g.wantPending()
}

func TestReviewRequestChangesDelivers(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--request-changes", "--note", "add a test\nfor the 401 path")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	r := f.review()
	if r.Status != model.ReviewChanges || r.Notes != "add a test\nfor the 401 path" || r.ClosedVia != model.AnsweredCLI || r.NoteMessage != "M-1" {
		t.Errorf("review = %+v, want changes_requested with the note and M-1", r)
	}
	m := reviewWantNoteMessage(t, f, "add a test\nfor the 401 path")
	if want := []string{"SendKeys %100 " + message.TypedLine(m)}; !slices.Equal(sendTyped(f.fake), want) {
		t.Errorf("typed %q, want %q", sendTyped(f.fake), want)
	}
	if stdout != "R-1: changes requested; M-1 delivered (send_keys)\n" {
		t.Errorf("stdout = %q", stdout)
	}
}

// reviewWantNoteMessage checks that the one stored message is R-1's change
// request with note, from the user to the lead, and returns it.
func reviewWantNoteMessage(t *testing.T, f *reviewFixture, note string) model.Message {
	t.Helper()
	ms := f.messages()
	if len(ms) != 1 {
		t.Fatalf("messages = %+v, want one", ms)
	}
	m := ms[0]
	if m.Kind != model.KindInstruction || m.RoutedBy != model.RoutedAddress || m.Sender != model.SenderUser ||
		m.TargetSession != lifecycleLead || m.TargetTask != "T-1" || m.TargetRepo != f.repo ||
		m.Text != task.NoteMessage("R-1", "T-1", note) {
		t.Errorf("message = %+v", m)
	}
	return m
}

func TestReviewRequestChangesQueuesForWorkingAgent(t *testing.T) {
	f := reviewWithLead(t, model.StateWorking)
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--request-changes", "--note", "add a test")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.HasPrefix(stdout, "R-1: changes requested; M-1 queued: ") {
		t.Errorf("stdout = %q, want the queued line", stdout)
	}
	if typed := sendTyped(f.fake); len(typed) != 0 {
		t.Errorf("typed %q into a working agent", typed)
	}
	if ms := f.messages(); len(ms) != 1 || ms[0].Status != model.MsgQueued {
		t.Errorf("messages = %+v, want one queued", ms)
	}
}

func TestReviewRequestChangesTargetsLeadNotTeammate(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	reviewWithTeammate(f, f.now.Add(time.Minute)) // updated last, so listed first
	line, err := reviewRequestChanges(context.Background(), f.st, f.fake, testLoggerOnly(), "R-1", "add a test", model.AnsweredPopup, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if ms := f.messages(); len(ms) != 1 || ms[0].TargetSession != lifecycleLead {
		t.Errorf("messages = %+v, want one to the lead", ms)
	}
	if r := f.review(); r.ClosedVia != model.AnsweredPopup {
		t.Errorf("closed_via = %s, want popup", r.ClosedVia)
	}
	if !strings.HasPrefix(line, "R-1: changes requested; M-1 ") {
		t.Errorf("line = %q", line)
	}
}

func testLoggerOnly() *slog.Logger {
	l, _ := testLogger()
	return l
}

func TestReviewRequestChangesWithoutLiveAgent(t *testing.T) {
	f := reviewWithLead(t, model.StateEnded)
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--request-changes", "--note", "add a test")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	ms := f.messages()
	if len(ms) != 1 || ms[0].TargetSession != lifecycleLead || ms[0].Status != model.MsgQueued {
		t.Errorf("messages = %+v, want one queued for the review's session", ms)
	}
	if !strings.HasPrefix(stdout, "R-1: changes requested; M-1 queued") || len(sendTyped(f.fake)) != 0 {
		t.Errorf("stdout = %q, typed %q", stdout, sendTyped(f.fake))
	}

	g := reviewNew(t)
	if _, _, err = g.st.CompleteTask(context.Background(), model.Review{
		TaskID: "T-1", Summary: "Done.", Tests: model.TestsNotRun, CreatedAt: g.now,
	}); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = reviewRunCmd(t, "", "review", "R-1", "--request-changes", "--note", "add a test")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if stdout != "R-1: changes requested; the note is saved but T-1 has no agent session to send it to\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if ms := g.messages(); len(ms) != 0 {
		t.Errorf("messages = %+v, want none", ms)
	}
	if r := g.review(); r.Status != model.ReviewChanges || r.Notes != "add a test" || r.NoteMessage != "" {
		t.Errorf("review = %+v, want the note saved and no message", r)
	}
}

func TestReviewNoteFromStdinAndBounds(t *testing.T) {
	f := reviewWithLead(t, model.StateWorking)
	for _, note := range []string{"", "   \n\x1b[0m ", strings.Repeat("é", 601)} {
		if _, _, err := reviewRunCmd(t, note, "review", "R-1", "--request-changes", "--note", "-"); err == nil {
			t.Errorf("note %d runes: accepted", len([]rune(note)))
		}
		f.wantPending()
	}
	if _, _, err := reviewRunCmd(t, strings.Repeat("a", 64<<10+1), "review", "R-1", "--request-changes", "--note", "-"); err == nil {
		t.Error("over 64 KiB on stdin: accepted")
	}
	f.wantPending()
	if ms := f.messages(); len(ms) != 0 {
		t.Errorf("messages = %+v, want none", ms)
	}
	note := strings.Repeat("é", 600)
	if _, stderr, err := reviewRunCmd(t, note+"\n", "review", "R-1", "--request-changes", "--note", "-"); err != nil {
		t.Fatalf("600 runes: %v\n%s", err, stderr)
	}
	if r := f.review(); r.Notes != note || r.Status != model.ReviewChanges {
		t.Errorf("review = %+v, want the note from stdin", r)
	}
}

func TestReviewDrop(t *testing.T) {
	f := reviewWithLead(t, model.StateWorking)
	stdout, stderr, err := reviewRunCmd(t, "", "review", "T-1", "--drop")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if stdout != "R-1 dropped; T-1 is dropped. Its agent is not stopped.\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if r := f.review(); r.Status != model.ReviewDropped || r.ClosedVia != model.AnsweredCLI {
		t.Errorf("review = %+v", r)
	}
	if tk := f.get("T-1"); tk.Status != model.TaskDropped {
		t.Errorf("task is %s, want dropped", tk.Status)
	}
	if slices.ContainsFunc(f.fake.Calls, func(c string) bool { return strings.HasPrefix(c, "KillPane") }) {
		t.Error("the agent's pane was killed")
	}
}

func TestReviewActionReappliesMarker(t *testing.T) {
	for _, args := range [][]string{
		{"review", "R-1", "--approve"},
		{"review", "R-1", "--request-changes", "--note", "more"},
		{"review", "R-1", "--drop"},
	} {
		f := reviewWithLead(t, model.StateIdle)
		f.fake.Calls = nil
		if _, stderr, err := reviewRunCmd(t, "", args...); err != nil {
			t.Fatalf("%q: %v\n%s", args, err, stderr)
		}
		var set []string
		for _, c := range f.fake.Calls {
			if strings.HasPrefix(c, "SetWindowOption @100 @curia_marker") {
				set = append(set, c)
			}
		}
		if len(set) == 0 || strings.HasSuffix(set[len(set)-1], "blue") {
			t.Errorf("%q: marker calls %q, want the window's marker set and not blue", args, set)
		}
	}
}

func TestReviewFlagErrors(t *testing.T) {
	f := reviewNew(t)
	for _, args := range [][]string{
		{"review", "R-1", "--approve", "--drop"},
		{"review", "R-1", "--approve", "--request-changes", "--note", "x"},
		{"review", "R-1", "--pr"},
		{"review", "R-1", "--merge"},
		{"review", "R-1", "--approve", "--merge", "--pr"},
		{"review", "R-1", "--note", "x"},
		{"review", "R-1", "--drop", "--note", "x"},
		{"review", "R-1", "--request-changes"},
		{"review", "R-1", "--json", "--drop"},
		{"review", "--approve"},
		{"review", "--json"},
		{"review", "T-01", "--approve"},
		{"review", "R-0", "--drop"},
		{"review", "-x", "--drop"},
		{"review", "R-1", "R-2", "--drop"},
		{"review", "R-1;x", "--drop"},
	} {
		if _, _, err := reviewRunCmd(t, "", args...); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	f.wantPending()
	if ms := f.messages(); len(ms) != 0 {
		t.Errorf("messages = %+v", ms)
	}
	if tk := f.get("T-1"); tk.Status == model.TaskDropped {
		t.Error("task dropped")
	}
}

func TestReviewResolveByTaskAndByReview(t *testing.T) {
	f := reviewNew(t)
	ctx := context.Background()
	for _, arg := range []string{"T-1", "R-1"} {
		r, err := reviewResolve(ctx, f.st, arg)
		if err != nil || r.ID != "R-1" {
			t.Errorf("reviewResolve(%s) = %s, %v; want R-1", arg, r.ID, err)
		}
	}
	if _, err := reviewResolve(ctx, f.st, "R-9"); err == nil || err.Error() != "no review R-9" {
		t.Errorf("R-9: %v", err)
	}
	if _, err := reviewApprove(ctx, f.st, f.cfg, "R-1", model.ApproveMerge, "", model.AnsweredCLI, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewResolve(ctx, f.st, "R-1"); err == nil || err.Error() != "R-1 is merged" {
		t.Errorf("closed: %v, want \"R-1 is merged\"", err)
	}
	if _, err := reviewResolve(ctx, f.st, "T-1"); err == nil || err.Error() != "T-1 has no review waiting" {
		t.Errorf("no pending: %v", err)
	}
	if _, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--drop"); err == nil || !strings.Contains(stderr, "R-1 is merged") {
		t.Errorf("command on a closed review: %v %q", err, stderr)
	}
}

func TestReviewJSON(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	a := reviewTicket(f, "Assume v2?", model.ModeAssume)
	b := reviewTicket(f, "Rotate?", model.ModeBlocking)
	d := reviewTicket(f, "Keep cookie?", model.ModeBlocking)
	if _, err := f.st.AnswerDecision(context.Background(), d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, f.now); err != nil {
		t.Fatal(err)
	}
	head, statusBefore := f.head(f.wt, "HEAD"), f.status()
	stdout, stderr, err := reviewRunCmd(t, "", "review", "T-1", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var got map[string]any
	if err = json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	reviewWantKeys(t, got, []string{
		"n", "id", "task_id", "session_id", "summary", "tests_status", "tests_detail", "status", "approved_how",
		"diff_stat", "head", "pr_url", "my_notes", "note_message", "closed_via", "created_at", "updated_at", "closed_at",
		"task", "diff_note", "open_assumptions", "open_blocking", "decided", "sessions", "teammates", "duration_seconds", "cost_usd",
	})
	reviewWantJSONValues(t, got, head, a.ID, b.ID, d.ID)
	f.wantPending()
	if f.status() != statusBefore || len(f.messages()) != 0 {
		t.Error("--json changed something")
	}
}

// reviewWantJSONValues checks --json's values for R-1 at head with one
// ticket in each list.
func reviewWantJSONValues(t *testing.T, got map[string]any, head, assumption, blocking, decided string) {
	t.Helper()
	if got["id"] != "R-1" || got["status"] != "pending" || got["head"] != head || got["closed_at"] != nil || got["cost_usd"] != nil {
		t.Errorf("fields = %v", got)
	}
	if s, _ := got["diff_stat"].(string); !strings.Contains(s, "auth.go") {
		t.Errorf("diff_stat = %v", got["diff_stat"])
	}
	for k, want := range map[string]string{"open_assumptions": assumption, "open_blocking": blocking, "decided": decided} {
		if v, _ := got[k].([]any); !slices.Equal(v, []any{want}) {
			t.Errorf("%s = %v, want [%s]", k, got[k], want)
		}
	}
	if tk, _ := got["task"].(map[string]any); tk["id"] != "T-1" || tk["branch"] != reviewBranch || tk["base_branch"] != "main" {
		t.Errorf("task = %v", got["task"])
	}
	if got["sessions"] != 1.0 || got["teammates"] != 0.0 || got["duration_seconds"] != 0.0 {
		t.Errorf("run = %v %v %v", got["sessions"], got["teammates"], got["duration_seconds"])
	}
}

// reviewWantKeys checks that got has exactly the keys want.
func reviewWantKeys(t *testing.T, got map[string]any, want []string) {
	t.Helper()
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %q, want %q", keys, want)
	}
}

func TestReviewWithoutTerminalNeedsAnAction(t *testing.T) {
	reviewNew(t)
	prev, prevProgram := reviewIsTerminal, runProgram
	reviewIsTerminal = func() bool { return false }
	ran := false
	runProgram = func(tea.Model, ...tea.ProgramOption) error { ran = true; return nil }
	t.Cleanup(func() { reviewIsTerminal, runProgram = prev, prevProgram })
	for _, args := range [][]string{{"review"}, {"review", "T-1"}} {
		_, _, err := reviewRunCmd(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "curia reviews ls") || !strings.Contains(err.Error(), "--approve") {
			t.Errorf("%q: error = %v", args, err)
		}
	}
	if ran {
		t.Error("the view ran without a terminal")
	}
}

// reviewPump runs cmd and the commands its messages lead to, feeding each
// message to m. A command that does not return within a moment is one of
// the view's refresh ticks, which are dropped.
func reviewPump(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	pending := []tea.Cmd{cmd}
	for steps := 0; len(pending) > 0; steps++ {
		if steps > 100 {
			t.Fatal("the view did not settle")
		}
		c := pending[0]
		pending = pending[1:]
		if c == nil {
			continue
		}
		ch := make(chan tea.Msg, 1)
		go func() { ch <- c() }()
		select {
		case msg := <-ch:
			if batch, ok := msg.(tea.BatchMsg); ok {
				pending = append(pending, batch...)
			} else if msg != nil {
				var next tea.Cmd
				m, next = m.Update(msg)
				pending = append(pending, next)
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
	return m
}

func TestReviewViewApprovesAndPrintsOutcome(t *testing.T) {
	f := reviewNew(t)
	head := f.head(f.wt, "HEAD")
	prev, prevProgram := reviewIsTerminal, runProgram
	reviewIsTerminal = func() bool { return true }
	runProgram = func(m tea.Model, opts ...tea.ProgramOption) error {
		if _, ok := m.(reviewCapture); !ok || len(opts) != 1 {
			t.Fatalf("model is %T with %d options, want the capture in the alt screen", m, len(opts))
		}
		m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
		m = reviewPump(t, m, m.Init())
		for _, k := range []string{"a", "y"} {
			f.now = f.now.Add(3 * time.Second)
			var cmd tea.Cmd
			m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
			m = reviewPump(t, m, cmd)
		}
		return nil
	}
	t.Cleanup(func() { reviewIsTerminal, runProgram = prev, prevProgram })
	stdout, stderr, err := reviewRunCmd(t, "", "review", "T-1")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if want := "R-1 approved: merged " + reviewBranch + " into main (" + head[:7] + "). Remove the worktree and branch with: curia clean T-1\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if r := f.review(); r.Status != model.ReviewMerged || r.ClosedVia != model.AnsweredPopup || r.Head != head {
		t.Errorf("review = %+v, want merged at %s via popup", r, head)
	}
	if _, _, err = reviewRunCmd(t, "", "review", "R-7"); err == nil || !strings.Contains(err.Error(), "no review R-7") {
		t.Errorf("view of no review: %v", err)
	}
}

func TestReviewOutputIsSanitised(t *testing.T) {
	f := reviewNew(t)
	f.update("T-1", func(tk *model.Task) { tk.Title = "evil \x1b]0;pwned\x07 \x1b[31mtitle" })
	lifecycleHooks(f.spawnFixture, "commit-msg", "#!/bin/sh\nprintf 'hook \\033[2J\\033]0;x\\007 says no\\n' >&2\nexit 1\n")
	for _, args := range [][]string{{"review", "R-1", "--json"}, {"review", "R-1", "--approve"}} {
		stdout, stderr, _ := reviewRunCmd(t, "", args...)
		for _, s := range []string{stdout, stderr} {
			if strings.ContainsAny(s, "\x1b\x07") {
				t.Errorf("%q printed a control character: %q", args, s)
			}
		}
		if args[2] == "--approve" && !strings.Contains(stderr, "says no") {
			t.Errorf("stderr = %q, want the hook's message, cleaned", stderr)
		}
	}
	f.wantPending()
}

// reviewWithTeammate adds teammate mate-1 to the lead's team, in the lead's window.
func reviewWithTeammate(f *reviewFixture, updated time.Time) {
	f.t.Helper()
	reviewSession(f, "mate-1", "%101", "@100", model.StateIdle, updated)
	if _, err := f.st.SetTeams(context.Background(), map[string]team.Membership{
		lifecycleLead: {TeamID: "team-1", Role: model.RoleLead, Member: "lead"},
		"mate-1":      {TeamID: "team-1", Role: model.RoleTeammate, Member: "tester"},
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestReviewSourceDetail(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	reviewWithTeammate(f, f.now)
	a := reviewTicket(f, "Assume v2?", model.ModeAssume)
	b := reviewTicket(f, "Rotate?", model.ModeBlocking)
	d := reviewTicket(f, "Keep cookie?", model.ModeBlocking)
	if _, err := f.st.AnswerDecision(context.Background(), d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, f.now); err != nil {
		t.Fatal(err)
	}
	// An assume ticket past its deadline is no longer open, though the sweep has not closed it yet.
	late := reviewTicket(f, "Assume staging?", model.ModeAssume)
	reviewSetExpiry(t, f, late.ID, f.now.Add(-time.Minute))
	// An earlier session of the task, ended, started 72 minutes before the others.
	early := f.start.Add(-72 * time.Minute)
	if err := f.st.UpsertSession(context.Background(), model.Session{
		ID: "early", Agent: "claude", State: model.StateEnded, TaskID: "T-1", StartedAt: early, UpdatedAt: early,
	}); err != nil {
		t.Fatal(err)
	}
	src := newReviewSource(f.st, f.fake, f.cfg, testLoggerOnly(), model.AnsweredPopup)
	det, err := src.Detail(context.Background(), "R-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := [][]string{reviewIDs(det.Assumptions), reviewIDs(det.Blocking), reviewIDs(det.Decided)}; !slices.EqualFunc(got,
		[][]string{{a.ID}, {b.ID}, {d.ID}}, slices.Equal) {
		t.Errorf("lists = %q, want %q %q %q", got, a.ID, b.ID, d.ID)
	}
	if det.Sessions != 3 || det.Teammates != 1 || det.CostKnown || det.Approve != model.ApproveMerge {
		t.Errorf("run = %+v", det)
	}
	if det.Head != f.head(f.wt, "HEAD") || !strings.Contains(det.DiffStat, "auth.go") || det.Task.ID != "T-1" {
		t.Errorf("head %q, stat %q", det.Head, det.DiffStat)
	}
	// Duration: from the earliest linked start to the review's creation.
	if r := f.review(); det.Duration != r.CreatedAt.Sub(early) || det.Duration < 72*time.Minute {
		t.Errorf("duration = %v, want %v", det.Duration, r.CreatedAt.Sub(early))
	}
}

func TestReviewSourceApproveRefusesChangedBranch(t *testing.T) {
	f := reviewNew(t)
	src := newReviewSource(f.st, f.fake, f.cfg, testLoggerOnly(), model.AnsweredPopup)
	det, err := src.Detail(context.Background(), "R-1")
	if err != nil {
		t.Fatal(err)
	}
	f.commitOnBranch("late.go", "package auth\n", "late commit") // lands after the user looked
	before := f.head(f.repo, "HEAD")
	if _, err = src.Approve(context.Background(), "R-1", det.Head); !errors.Is(err, errReviewBranchChanged) {
		t.Fatalf("Approve with the old head: %v, want the changed-branch refusal", err)
	}
	if f.head(f.repo, "HEAD") != before {
		t.Error("main moved")
	}
	f.wantPending()
	again, err := src.Detail(context.Background(), "R-1")
	if err != nil || again.Head == det.Head || !strings.Contains(again.DiffStat, "late.go") {
		t.Fatalf("new detail: %v, head %q, stat %q", err, again.Head, again.DiffStat)
	}
	line, err := src.Approve(context.Background(), "R-1", again.Head)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.review(); r.Status != model.ReviewMerged || r.Head != again.Head || r.ClosedVia != model.AnsweredPopup ||
		!strings.Contains(r.DiffStat, "late.go") {
		t.Errorf("review = %+v, want merged at the new head via popup", r)
	}
	if !strings.Contains(line, "("+again.Head[:7]+")") {
		t.Errorf("line = %q", line)
	}
}

func TestReviewSourceQueueRunsNoGit(t *testing.T) {
	f := reviewNew(t)
	calls := reviewRecordGit(t)
	src := newReviewSource(f.st, f.fake, f.cfg, testLoggerOnly(), model.AnsweredPopup)
	rows, err := src.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Review.ID != "R-1" || rows[0].Task.ID != "T-1" {
		t.Errorf("rows = %+v", rows)
	}
	if c := calls(); len(c) != 0 {
		t.Errorf("Queue ran git: %q", c)
	}
	// A task stored dropped (by curia task) has no review to show.
	f.update("T-1", func(tk *model.Task) { tk.Status = model.TaskDropped })
	if rows, err = src.Queue(context.Background()); err != nil || len(rows) != 0 {
		t.Errorf("dropped task: rows %+v, err %v", rows, err)
	}
}

// reviewSetExpiry moves ticket id's deadline to at, with a raw UPDATE.
func reviewSetExpiry(t *testing.T, f *reviewFixture, id string, at time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err = raw.Exec(`UPDATE decisions SET expires_at = ? WHERE id = ?`,
		at.UTC().Format("2006-01-02T15:04:05.000000000Z"), id); err != nil {
		t.Fatal(err)
	}
}

func TestReviewSourceDiffCmd(t *testing.T) {
	f := reviewNew(t)
	reviewUseDiffTools(t, func(name string) (string, error) { return filepath.Join("/opt/bin", name), nil })
	src := newReviewSource(f.st, f.fake, f.cfg, testLoggerOnly(), model.AnsweredPopup)
	cmd, err := src.DiffCmd(context.Background(), "R-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/bin/git", "-C", f.repo, "diff", "main..." + reviewBranch}; !slices.Equal(append([]string{cmd.Path}, cmd.Args[1:]...), want) {
		t.Errorf("argv = %q, want %q", cmd.Args, want)
	}
}

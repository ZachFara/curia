package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/review"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/team"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/ui"
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
		reviewNoteFlag, reviewHeadFlag = "", ""
	}()
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// reviewBranch is task T-1's branch in the review fixture.
const reviewBranch = testkit.ReviewBranch

// reviewFixture is testkit's review fixture; its approve goes through the
// command's guard.
type reviewFixture struct{ *testkit.ReviewFixture }

func reviewNew(t *testing.T) *reviewFixture {
	t.Helper()
	return &reviewFixture{testkit.NewReview(t, lifecycleSpawned(t, lifecycle.SpawnOptions{}).SpawnFixture)}
}

// approve runs reviewApprove on R-1 as the flag form does, seeing the
// branch's head now, as a user who has just looked at it (a branch that does
// not resolve gives a commit id no branch is at).
func (f *reviewFixture) approve(how model.ApproveHow) (string, error) {
	f.T.Helper()
	seen := strings.Repeat("0", 40)
	if out, err := gitops.Output(context.Background(), gitops.GitLimit, f.Repo, "rev-parse", "--verify", "-q", "refs/heads/"+reviewBranch); err == nil {
		seen = out
	}
	return reviewApprove(context.Background(), review.Approval{Store: f.St, Config: f.Cfg, ReviewID: "R-1", How: how, Seen: seen, Via: model.AnsweredCLI, Now: f.Now})
}

// headArg is --head and the branch's head now: the commit a user who has
// just looked approves.
func (f *reviewFixture) headArg() []string {
	f.T.Helper()
	return []string{"--head", f.Head(f.Wt, "HEAD")}
}

// reviewWithLead is reviewNew with the lead session linked to T-1, idle in
// the task's window (pane %100, window @100), which shows Claude Code's idle
// prompt.
func reviewWithLead(t *testing.T, state model.State) *reviewFixture {
	t.Helper()
	f := reviewNew(t)
	f.Fake.Panes[0].WindowID = "@100"
	f.Fake.Screen = decideIdleScreen
	reviewSession(f, testkit.LifecycleLead, "%100", "@100", state, f.Now)
	return f
}

// reviewSession links session id, in pane and window, to T-1.
func reviewSession(f *reviewFixture, id, pane, window string, state model.State, updated time.Time) {
	f.T.Helper()
	if err := f.St.UpsertSession(context.Background(), model.Session{
		ID: id, Agent: "claude", State: state, Pane: pane, TmuxWindow: window, TmuxSession: "work", TaskID: "T-1",
		StartedAt: f.Start, UpdatedAt: updated,
	}); err != nil {
		f.T.Fatal(err)
	}
}

// reviewTicket opens a ticket of mode on T-1.
func reviewTicket(f *reviewFixture, question string, mode model.Mode) model.Decision {
	f.T.Helper()
	d := model.Decision{
		SessionID: testkit.LifecycleLead, TaskID: "T-1", Repo: f.Repo, Question: question,
		Options: []model.Option{{Key: "A", Label: "Yes"}, {Key: "B", Label: "No"}}, Recommendation: "A",
		Rationale: "Simple.", Reversibility: model.ReversCostly, Mode: mode, Scope: model.ScopeTask,
		Status: model.StatusOpen, ArgsHash: "hash-" + question, Delivery: model.DeliveryNone, CreatedAt: f.Now,
	}
	if mode == model.ModeAssume {
		d.Assumption = "Yes"
		d.ExpiresAt = f.Now.Add(24 * time.Hour)
	}
	stored, _, err := f.St.CreateDecision(context.Background(), d)
	if err != nil {
		f.T.Fatal(err)
	}
	return stored
}

func (f *reviewFixture) decision(id string) model.Decision {
	f.T.Helper()
	d, found, err := f.St.GetDecision(context.Background(), id)
	if err != nil || !found {
		f.T.Fatalf("GetDecision(%s): %v %v", id, found, err)
	}
	return d
}

func (f *reviewFixture) messages() []model.Message {
	f.T.Helper()
	ms, err := f.St.ListMessages(context.Background(), store.MessageFilter{})
	if err != nil {
		f.T.Fatal(err)
	}
	return ms
}

// reviewConfig writes the config file with content.
func reviewConfig(t *testing.T, content string) {
	t.Helper()
	testkit.WorktreeWrite(t, configFile(os.Getenv("HOME")), content)
}

func TestReviewRefusesInsideAgent(t *testing.T) {
	db := testkit.HookHome(t)
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
	if _, err := reviewApprove(context.Background(), review.Approval{Config: config.Default(), ReviewID: "R-1", Via: model.AnsweredCLI, Now: time.Now()}); err == nil {
		t.Error("reviewApprove ran inside an agent session")
	}
	if _, err := reviewRequestChanges(context.Background(), nil, nil, nil, "R-1", "x", model.AnsweredCLI, time.Now()); err == nil {
		t.Error("reviewRequestChanges ran inside an agent session")
	}
}

func TestReviewApproveMerges(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	stdout, stderr, err := reviewRunCmd(t, "", append([]string{"review", "R-1", "--approve"}, f.headArg()...)...)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	want := "R-1 approved: merged " + reviewBranch + " into main (" + head[:7] + "). Remove the worktree and branch with: curia clean T-1\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	r := f.Review()
	if r.Status != model.ReviewMerged || r.Head != head || r.ApprovedHow != model.ApproveMerge ||
		r.ClosedVia != model.AnsweredCLI || !strings.Contains(r.DiffStat, "auth.go") {
		t.Errorf("review = %+v, want merged at %s with its stat, how merge, via cli", r, head)
	}
	if got := f.Head(f.Repo, "main^2"); got != head {
		t.Errorf("main^2 = %s, want %s", got, head)
	}
}

func TestReviewApproveExpiresAssumptions(t *testing.T) {
	f := reviewNew(t)
	a1 := reviewTicket(f, "Assume v2 endpoint?", model.ModeAssume)
	a2 := reviewTicket(f, "Assume 5 minute margin?", model.ModeAssume)
	b := reviewTicket(f, "Rotate the key?", model.ModeBlocking)
	stdout, stderr, err := reviewRunCmd(t, "", append([]string{"review", "T-1", "--approve", "--merge"}, f.headArg()...)...)
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
	stdout, stderr, err := reviewRunCmd(t, "", append([]string{"review", "R-1", "--approve", "--pr"}, f.headArg()...)...)
	if err != nil {
		t.Fatalf("--pr: %v\n%s", err, stderr)
	}
	if r := f.Review(); r.Status != model.ReviewApproved || r.PRURL != "https://github.com/o/r/pull/3" || r.ApprovedHow != model.ApprovePR {
		t.Errorf("--pr: review = %+v", r)
	}
	if stdout != "R-1 approved: pull request https://github.com/o/r/pull/3 opened for "+reviewBranch+".\n" {
		t.Errorf("--pr: stdout = %q", stdout)
	}

	g := reviewNew(t)
	reviewConfig(t, "[review]\napprove = \"pr\"\n")
	gh := reviewUseGH(t, "https://github.com/o/r/pull/4\n")
	if _, stderr, err = reviewRunCmd(t, "", append([]string{"review", "R-1", "--approve"}, g.headArg()...)...); err != nil {
		t.Fatalf("config pr: %v\n%s", err, stderr)
	}
	if r := g.Review(); r.Status != model.ReviewApproved || r.PRURL != "https://github.com/o/r/pull/4" || len(gh.calls) != 1 {
		t.Errorf("config pr: review = %+v, gh calls %d", r, len(gh.calls))
	}

	h := reviewNew(t)
	reviewConfig(t, "[review]\napprove = \"pr\"\n")
	gh = reviewUseGH(t, "")
	if _, stderr, err = reviewRunCmd(t, "", append([]string{"review", "R-1", "--approve", "--merge"}, h.headArg()...)...); err != nil {
		t.Fatalf("--merge over config: %v\n%s", err, stderr)
	}
	if r := h.Review(); r.Status != model.ReviewMerged || len(gh.calls) != 0 {
		t.Errorf("--merge over config: review = %+v, gh calls %d", r, len(gh.calls))
	}
}

func TestReviewApproveFailureStaysPending(t *testing.T) {
	f := reviewNew(t)
	f.CommitOnBranch("README", "branch side\n", "branch readme")
	f.CommitOnMain("README", "main side\n", "main readme")
	before := f.Head(f.Repo, "HEAD")
	stdout, stderr, err := reviewRunCmd(t, "", append([]string{"review", "R-1", "--approve"}, f.headArg()...)...)
	if err == nil || !strings.Contains(stderr, "Nothing was merged") {
		t.Fatalf("err %v, stderr %q; want the failure", err, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	f.WantPending()
	if f.Head(f.Repo, "HEAD") != before || f.Status() != "" {
		t.Error("the repository changed")
	}
	// A working agent: refused before any git that changes something.
	g := reviewWithLead(t, model.StateWorking)
	if _, stderr, err = reviewRunCmd(t, "", append([]string{"review", "R-1", "--approve"}, g.headArg()...)...); err == nil ||
		!strings.Contains(stderr, "the agent is still working; wait for it to stop") {
		t.Errorf("working agent: err %v, stderr %q", err, stderr)
	}
	g.WantPending()
}

func TestReviewRequestChangesDelivers(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--request-changes", "--note", "add a test\nfor the 401 path")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	r := f.Review()
	if r.Status != model.ReviewChanges || r.Notes != "add a test\nfor the 401 path" || r.ClosedVia != model.AnsweredCLI || r.NoteMessage != "M-1" {
		t.Errorf("review = %+v, want changes_requested with the note and M-1", r)
	}
	m := reviewWantNoteMessage(t, f, "add a test\nfor the 401 path")
	if want := []string{"SendKeys %100 " + message.TypedLine(m)}; !slices.Equal(sendTyped(f.Fake), want) {
		t.Errorf("typed %q, want %q", sendTyped(f.Fake), want)
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
		m.TargetSession != testkit.LifecycleLead || m.TargetTask != "T-1" || m.TargetRepo != f.Repo ||
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
	if typed := sendTyped(f.Fake); len(typed) != 0 {
		t.Errorf("typed %q into a working agent", typed)
	}
	if ms := f.messages(); len(ms) != 1 || ms[0].Status != model.MsgQueued {
		t.Errorf("messages = %+v, want one queued", ms)
	}
}

func TestReviewRequestChangesTargetsLeadNotTeammate(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	reviewWithTeammate(f, f.Now.Add(time.Minute)) // updated last, so listed first
	line, err := reviewRequestChanges(context.Background(), f.St, f.Fake, testLoggerOnly(), "R-1", "add a test", model.AnsweredPopup, f.Now)
	if err != nil {
		t.Fatal(err)
	}
	if ms := f.messages(); len(ms) != 1 || ms[0].TargetSession != testkit.LifecycleLead {
		t.Errorf("messages = %+v, want one to the lead", ms)
	}
	if r := f.Review(); r.ClosedVia != model.AnsweredPopup {
		t.Errorf("closed_via = %s, want popup", r.ClosedVia)
	}
	if !strings.HasPrefix(line, "R-1: changes requested; M-1 ") {
		t.Errorf("line = %q", line)
	}
}

func testLoggerOnly() *slog.Logger {
	l, _ := testkit.TestLogger()
	return l
}

func TestReviewRequestChangesWithoutLiveAgent(t *testing.T) {
	f := reviewWithLead(t, model.StateEnded)
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--request-changes", "--note", "add a test")
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	ms := f.messages()
	if len(ms) != 1 || ms[0].TargetSession != testkit.LifecycleLead || ms[0].Status != model.MsgQueued {
		t.Errorf("messages = %+v, want one queued for the review's session", ms)
	}
	if !strings.HasPrefix(stdout, "R-1: changes requested; M-1 queued") || len(sendTyped(f.Fake)) != 0 {
		t.Errorf("stdout = %q, typed %q", stdout, sendTyped(f.Fake))
	}

	g := reviewNew(t)
	if _, _, err = g.St.CompleteTask(context.Background(), model.Review{
		TaskID: "T-1", Summary: "Done.", Tests: model.TestsNotRun, CreatedAt: g.Now,
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
	if r := g.Review(); r.Status != model.ReviewChanges || r.Notes != "add a test" || r.NoteMessage != "" {
		t.Errorf("review = %+v, want the note saved and no message", r)
	}
}

func TestReviewNoteFromStdinAndBounds(t *testing.T) {
	f := reviewWithLead(t, model.StateWorking)
	for _, note := range []string{"", "   \n\x1b[0m ", strings.Repeat("é", 601)} {
		if _, _, err := reviewRunCmd(t, note, "review", "R-1", "--request-changes", "--note", "-"); err == nil {
			t.Errorf("note %d runes: accepted", len([]rune(note)))
		}
		f.WantPending()
	}
	if _, _, err := reviewRunCmd(t, strings.Repeat("a", 64<<10+1), "review", "R-1", "--request-changes", "--note", "-"); err == nil {
		t.Error("over 64 KiB on stdin: accepted")
	}
	f.WantPending()
	if ms := f.messages(); len(ms) != 0 {
		t.Errorf("messages = %+v, want none", ms)
	}
	note := strings.Repeat("é", 600)
	if _, stderr, err := reviewRunCmd(t, note+"\n", "review", "R-1", "--request-changes", "--note", "-"); err != nil {
		t.Fatalf("600 runes: %v\n%s", err, stderr)
	}
	if r := f.Review(); r.Notes != note || r.Status != model.ReviewChanges {
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
	if r := f.Review(); r.Status != model.ReviewDropped || r.ClosedVia != model.AnsweredCLI {
		t.Errorf("review = %+v", r)
	}
	if tk := f.Get("T-1"); tk.Status != model.TaskDropped {
		t.Errorf("task is %s, want dropped", tk.Status)
	}
	if slices.ContainsFunc(f.Fake.Calls, func(c string) bool { return strings.HasPrefix(c, "KillPane") }) {
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
		if args[2] == "--approve" {
			args = append(args, f.headArg()...)
		}
		f.Fake.Calls = nil
		if _, stderr, err := reviewRunCmd(t, "", args...); err != nil {
			t.Fatalf("%q: %v\n%s", args, err, stderr)
		}
		var set []string
		for _, c := range f.Fake.Calls {
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
		{"review", "R-1", "--head", "abcdef1"},
		{"review", "R-1", "--drop", "--head", "abcdef1"},
	} {
		if _, _, err := reviewRunCmd(t, "", args...); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	f.WantPending()
	if ms := f.messages(); len(ms) != 0 {
		t.Errorf("messages = %+v", ms)
	}
	if tk := f.Get("T-1"); tk.Status == model.TaskDropped {
		t.Error("task dropped")
	}
}

func TestReviewResolveByTaskAndByReview(t *testing.T) {
	f := reviewNew(t)
	ctx := context.Background()
	for _, arg := range []string{"T-1", "R-1"} {
		r, err := review.Resolve(ctx, f.St, arg)
		if err != nil || r.ID != "R-1" {
			t.Errorf("review.Resolve(%s) = %s, %v; want R-1", arg, r.ID, err)
		}
	}
	if _, err := review.Resolve(ctx, f.St, "R-9"); err == nil || err.Error() != "no review R-9" {
		t.Errorf("R-9: %v", err)
	}
	if _, err := f.approve(model.ApproveMerge); err != nil {
		t.Fatal(err)
	}
	if _, err := review.Resolve(ctx, f.St, "R-1"); err == nil || err.Error() != "R-1 is merged" {
		t.Errorf("closed: %v, want \"R-1 is merged\"", err)
	}
	if _, err := review.Resolve(ctx, f.St, "T-1"); err == nil || err.Error() != "T-1 has no review waiting" {
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
	if _, err := f.St.AnswerDecision(context.Background(), d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, f.Now); err != nil {
		t.Fatal(err)
	}
	head, statusBefore := f.Head(f.Wt, "HEAD"), f.Status()
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
		"branch_cost_usd",
	})
	reviewWantJSONValues(t, got, head, a.ID, b.ID, d.ID)
	f.WantPending()
	if f.Status() != statusBefore || len(f.messages()) != 0 {
		t.Error("--json changed something")
	}
}

// reviewCostlensTask is T-1 as the store holds it.
func reviewCostlensTask(t *testing.T, f *reviewFixture) model.Task {
	t.Helper()
	tk, found, err := f.St.GetTask(context.Background(), "T-1")
	if err != nil || !found || tk.Worktree == "" || tk.Worktree == tk.Repo {
		t.Fatalf("T-1 = %+v %v %v", tk, found, err)
	}
	return tk
}

func TestReviewDetailNeverWaits(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	cfg, bin := costlensSetup(t)
	f.Cfg.CostLens.Analyzer = cfg.CostLens.Analyzer
	fake := &costlensFake{release: make(chan struct{}), out: costlensSummary(t, reviewBranch)}
	costlensUse(t, costlensLookIn(bin), fake)
	src := newReviewSource(f.St, f.Fake, f.Cfg, testLoggerOnly(), model.AnsweredPopup, nil)

	type result struct {
		d   ui.ReviewDetail
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := src.Detail(context.Background(), "R-1")
		done <- result{d, err}
	}()
	var first result
	select { // the run is held until released: a Detail that waited for it would never return
	case first = <-done:
	case <-time.After(10 * time.Second):
		close(fake.release)
		t.Fatal("Detail waited for the analyzer")
	}
	if first.err != nil || first.d.BranchCostKnown || first.d.Head == "" {
		t.Fatalf("first Detail = %+v %v, want no branch cost yet", first.d, first.err)
	}
	costlensWaitCalls(t, fake, 1)
	// P5-R4: the analyzer is asked about the worktree, never the repository.
	if tk := reviewCostlensTask(t, f); fake.call(0)[2] != tk.Worktree {
		t.Errorf("--project = %q, want the worktree %q", fake.call(0)[2], tk.Worktree)
	}
	close(fake.release)
	costlensProcWait(t)
	d, err := src.Detail(context.Background(), "R-1")
	if err != nil || !d.BranchCostKnown || !costlensIs503(d.BranchCostUSD) {
		t.Errorf("Detail after the run = %v %v, %v", d.BranchCostUSD, d.BranchCostKnown, err)
	}
	if d.Head != first.d.Head || fake.n() != 1 {
		t.Errorf("head %q then %q, runs %d", first.d.Head, d.Head, fake.n())
	}
}

// costlensProcWait waits for the process cache's run in flight, at most 10 s.
func costlensProcWait(t *testing.T) {
	t.Helper()
	costlensProcMu.Lock()
	c := costlensProc
	costlensProcMu.Unlock()
	if c != nil {
		costlensWait(t, c)
	}
}

// reviewBranchCostJSON runs curia review T-1 --json and returns its
// branch_cost_usd and how long it took.
func reviewBranchCostJSON(t *testing.T) (any, time.Duration) {
	t.Helper()
	start := time.Now()
	stdout, stderr, err := reviewRunCmd(t, "", "review", "T-1", "--json")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	var got map[string]any
	if err = json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("%v: %s", err, stdout)
	}
	v, present := got["branch_cost_usd"]
	if !present {
		t.Fatalf("no branch_cost_usd in %s", stdout)
	}
	return v, took
}

// reviewFloat is v as a JSON number, or NaN.
func reviewFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return math.NaN()
}

func TestReviewJSONHasBranchCost(t *testing.T) {
	reviewWithLead(t, model.StateIdle)
	cfg, bin := costlensSetup(t)
	reviewConfig(t, "[costlens]\nanalyzer = "+strconv.Quote(cfg.CostLens.Analyzer)+"\n")
	// The run ends 200 ms after it starts: --json waits for it.
	fake := &costlensFake{release: make(chan struct{}), out: costlensSummary(t, reviewBranch)}
	costlensUse(t, costlensLookIn(bin), fake)
	timer := time.AfterFunc(200*time.Millisecond, func() { close(fake.release) })
	defer timer.Stop()
	if v, _ := reviewBranchCostJSON(t); v == nil || !costlensIs503(reviewFloat(v)) {
		t.Errorf("branch_cost_usd = %v, want 5.03", v)
	}
	if n := fake.n(); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}

	// A failed run: null.
	failing := &costlensFake{err: errors.New("exit status 1")}
	costlensUse(t, costlensLookIn(bin), failing)
	if v, _ := reviewBranchCostJSON(t); v != nil || failing.n() != 1 {
		t.Errorf("failed run: branch_cost_usd = %v, runs %d", v, failing.n())
	}

	// No node: null, nothing run, and nothing waited for (a run would be held for good).
	held := &costlensFake{release: make(chan struct{})}
	defer close(held.release)
	costlensUse(t, costlensLookIn(t.TempDir()), held)
	if v, took := reviewBranchCostJSON(t); v != nil || held.n() != 0 || took > costlensLimit/2 {
		t.Errorf("no analyzer: branch_cost_usd = %v, runs %d, took %v", v, held.n(), took)
	}
}

func TestReviewWithoutBranchAsksNothing(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	cfg, bin := costlensSetup(t)
	f.Cfg.CostLens.Analyzer = cfg.CostLens.Analyzer
	reviewConfig(t, "[costlens]\nanalyzer = "+strconv.Quote(cfg.CostLens.Analyzer)+"\n")
	fake := &costlensFake{out: costlensSummary(t, reviewBranch)}
	costlensUse(t, costlensLookIn(bin), fake)
	for _, set := range []string{
		`UPDATE tasks SET branch = '' WHERE id = 'T-1'`,
		`UPDATE tasks SET branch = ?, worktree = '' WHERE id = 'T-1'`,
	} {
		raw, err := sql.Open("sqlite", f.DB)
		if err != nil {
			t.Fatal(err)
		}
		var args []any
		if strings.Contains(set, "?") {
			args = []any{reviewBranch}
		}
		if _, err = raw.Exec(set, args...); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		src := newReviewSource(f.St, f.Fake, f.Cfg, testLoggerOnly(), model.AnsweredPopup, nil)
		if d, err := src.Detail(context.Background(), "R-1"); err != nil || d.BranchCostKnown {
			t.Errorf("%s: Detail = %v %v", set, d.BranchCostKnown, err)
		}
		if v, _ := reviewBranchCostJSON(t); v != nil {
			t.Errorf("%s: branch_cost_usd = %v", set, v)
		}
		costlensProcWait(t)
		if n := fake.n(); n != 0 {
			t.Errorf("%s: costlensRun called %d times", set, n)
		}
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
// message to m. Timer ticks are dropped (reviewIsTick), and so is a command
// still running after reviewPumpLimit (a review action held in a hook).
func reviewPump(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	return reviewPumping(t, m, cmd, false)
}

// reviewPumpLimit is how long reviewPump waits for one command: long enough
// for a git call on a loaded machine (the gate runs packages in parallel),
// shorter than an approval held in a hook, which it leaves running.
const reviewPumpLimit = 2 * time.Second

// reviewIsTick reports whether v is one of the views' timer ticks, which
// reviewPump drops so a refresh chain cannot run on: a message whose type
// ends in TickMsg (or the dashboard's tickMsg), also inside the dashboard's
// wrapper of a child's message.
func reviewIsTick(v reflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	if name := v.Type().Name(); strings.HasSuffix(name, "TickMsg") || name == "tickMsg" {
		return true
	}
	if v.Kind() == reflect.Struct {
		if f := v.FieldByName("msg"); f.IsValid() && f.Kind() == reflect.Interface {
			return reviewIsTick(f.Elem())
		}
	}
	return false
}

// reviewPumpActions is reviewPump that also waits for a command a review
// action is running (reviewPumpWait), so the action's outcome is fed back.
func reviewPumpActions(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	return reviewPumping(t, m, cmd, true)
}

func reviewPumping(t *testing.T, m tea.Model, cmd tea.Cmd, actions bool) tea.Model {
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
		wait := (<-chan tea.Msg)(ch)
		if actions {
			wait = reviewPumpWait(ch)
		}
		select {
		case msg := <-wait:
			if batch, ok := msg.(tea.BatchMsg); ok {
				pending = append(pending, batch...)
			} else if msg != nil && !reviewIsTick(reflect.ValueOf(msg)) {
				var next tea.Cmd
				m, next = m.Update(msg)
				pending = append(pending, next)
			}
		case <-time.After(reviewPumpLimit):
		}
	}
	return m
}

// reviewPumpWait is ch, or, when its command is still running after 300 ms
// while a review action is in flight (an approval can take seconds), ch
// waited on for up to 30 s more: the view's timer ticks start no action.
func reviewPumpWait(ch chan tea.Msg) <-chan tea.Msg {
	select {
	case msg := <-ch:
		out := make(chan tea.Msg, 1)
		out <- msg
		return out
	case <-time.After(250 * time.Millisecond):
	}
	reviewFlight.mu.Lock()
	busy := reviewFlight.n > 0
	reviewFlight.mu.Unlock()
	if !busy {
		return ch
	}
	out := make(chan tea.Msg, 1)
	select {
	case msg := <-ch:
		out <- msg
	case <-time.After(30 * time.Second):
	}
	return out
}

func TestReviewViewApprovesAndPrintsOutcome(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	prev, prevProgram := reviewIsTerminal, runProgram
	reviewIsTerminal = func() bool { return true }
	runProgram = func(m tea.Model, opts ...tea.ProgramOption) error {
		if _, ok := m.(reviewCapture); !ok || len(opts) != 2 {
			t.Fatalf("model is %T with %d options, want the capture in the alt screen, with the signal context", m, len(opts))
		}
		m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
		m = reviewPump(t, m, m.Init())
		for _, k := range []string{"a", "y"} {
			f.Now = f.Now.Add(3 * time.Second)
			var cmd tea.Cmd
			m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
			m = reviewPumpActions(t, m, cmd)
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
	if r := f.Review(); r.Status != model.ReviewMerged || r.ClosedVia != model.AnsweredPopup || r.Head != head {
		t.Errorf("review = %+v, want merged at %s via popup", r, head)
	}
	if _, _, err = reviewRunCmd(t, "", "review", "R-7"); err == nil || !strings.Contains(err.Error(), "no review R-7") {
		t.Errorf("view of no review: %v", err)
	}
}

func TestReviewOutputIsSanitised(t *testing.T) {
	f := reviewNew(t)
	f.Update("T-1", func(tk *model.Task) { tk.Title = "evil \x1b]0;pwned\x07 \x1b[31mtitle" })
	testkit.LifecycleHooks(f.SpawnFixture, "commit-msg", "#!/bin/sh\nprintf 'hook \\033[2J\\033]0;x\\007 says no\\n' >&2\nexit 1\n")
	for _, args := range [][]string{{"review", "R-1", "--json"}, append([]string{"review", "R-1", "--approve"}, f.headArg()...)} {
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
	f.WantPending()
}

// reviewWithTeammate adds teammate mate-1 to the lead's team, in the lead's window.
func reviewWithTeammate(f *reviewFixture, updated time.Time) {
	f.T.Helper()
	reviewSession(f, "mate-1", "%101", "@100", model.StateIdle, updated)
	if _, err := f.St.SetTeams(context.Background(), map[string]team.Membership{
		testkit.LifecycleLead: {TeamID: "team-1", Role: model.RoleLead, Member: "lead"},
		"mate-1":              {TeamID: "team-1", Role: model.RoleTeammate, Member: "tester"},
	}); err != nil {
		f.T.Fatal(err)
	}
}

func TestReviewSourceDetail(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	reviewWithTeammate(f, f.Now)
	a := reviewTicket(f, "Assume v2?", model.ModeAssume)
	b := reviewTicket(f, "Rotate?", model.ModeBlocking)
	d := reviewTicket(f, "Keep cookie?", model.ModeBlocking)
	if _, err := f.St.AnswerDecision(context.Background(), d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, f.Now); err != nil {
		t.Fatal(err)
	}
	// An assume ticket past its deadline is no longer open, though the sweep has not closed it yet.
	late := reviewTicket(f, "Assume staging?", model.ModeAssume)
	reviewSetExpiry(t, f, late.ID, f.Now.Add(-time.Minute))
	// An earlier session of the task, ended, started 72 minutes before the others.
	early := f.Start.Add(-72 * time.Minute)
	if err := f.St.UpsertSession(context.Background(), model.Session{
		ID: "early", Agent: "claude", State: model.StateEnded, TaskID: "T-1", StartedAt: early, UpdatedAt: early,
	}); err != nil {
		t.Fatal(err)
	}
	src := newReviewSource(f.St, f.Fake, f.Cfg, testLoggerOnly(), model.AnsweredPopup, nil)
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
	if det.Head != f.Head(f.Wt, "HEAD") || !strings.Contains(det.DiffStat, "auth.go") || det.Task.ID != "T-1" {
		t.Errorf("head %q, stat %q", det.Head, det.DiffStat)
	}
	// Duration: from the earliest linked start to the review's creation.
	if r := f.Review(); det.Duration != r.CreatedAt.Sub(early) || det.Duration < 72*time.Minute {
		t.Errorf("duration = %v, want %v", det.Duration, r.CreatedAt.Sub(early))
	}
}

func TestReviewSourceApproveRefusesChangedBranch(t *testing.T) {
	f := reviewNew(t)
	src := newReviewSource(f.St, f.Fake, f.Cfg, testLoggerOnly(), model.AnsweredPopup, nil)
	det, err := src.Detail(context.Background(), "R-1")
	if err != nil {
		t.Fatal(err)
	}
	f.CommitOnBranch("late.go", "package auth\n", "late commit") // lands after the user looked
	before := f.Head(f.Repo, "HEAD")
	if _, err = src.Approve(context.Background(), "R-1", det.Head); !errors.Is(err, gitops.ErrBranchChanged) {
		t.Fatalf("Approve with the old head: %v, want the changed-branch refusal", err)
	}
	if f.Head(f.Repo, "HEAD") != before {
		t.Error("main moved")
	}
	f.WantPending()
	again, err := src.Detail(context.Background(), "R-1")
	if err != nil || again.Head == det.Head || !strings.Contains(again.DiffStat, "late.go") {
		t.Fatalf("new detail: %v, head %q, stat %q", err, again.Head, again.DiffStat)
	}
	line, err := src.Approve(context.Background(), "R-1", again.Head)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.Review(); r.Status != model.ReviewMerged || r.Head != again.Head || r.ClosedVia != model.AnsweredPopup ||
		!strings.Contains(r.DiffStat, "late.go") {
		t.Errorf("review = %+v, want merged at the new head via popup", r)
	}
	if !strings.Contains(line, "("+again.Head[:7]+")") {
		t.Errorf("line = %q", line)
	}
}

func TestReviewSourceQueueRunsNoGit(t *testing.T) {
	f := reviewNew(t)
	calls := testkit.RecordGit(t)
	src := newReviewSource(f.St, f.Fake, f.Cfg, testLoggerOnly(), model.AnsweredPopup, nil)
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
	f.Update("T-1", func(tk *model.Task) { tk.Status = model.TaskDropped })
	if rows, err = src.Queue(context.Background()); err != nil || len(rows) != 0 {
		t.Errorf("dropped task: rows %+v, err %v", rows, err)
	}
}

// reviewSetExpiry moves ticket id's deadline to at, with a raw UPDATE.
func reviewSetExpiry(t *testing.T, f *reviewFixture, id string, at time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", f.DB)
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
	src := newReviewSource(f.St, f.Fake, f.Cfg, testLoggerOnly(), model.AnsweredPopup, nil)
	cmd, err := src.DiffCmd(context.Background(), "R-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/bin/git", "-C", f.Repo, "diff", "refs/heads/main...refs/heads/" + reviewBranch}; !slices.Equal(append([]string{cmd.Path}, cmd.Args[1:]...), want) {
		t.Errorf("argv = %q, want %q", cmd.Args, want)
	}
}

// The variables a signal test passes to its child (TestReviewChild).
const (
	reviewChildArgs = "CURIA_TEST_REVIEW_ARGS" // curia's arguments, joined by \x1f; "view" first drives the view
	reviewChildHome = "CURIA_TEST_REVIEW_HOME"
	reviewChildDB   = "CURIA_TEST_REVIEW_DB"
)

// TestReviewChild is no test of its own: the signal tests run it in a child
// process, where it runs curia with the arguments they pass, as the user at
// a terminal, against their home and store. With "view" first it runs the
// review view, approves with a then y, and ends the program a second later,
// while the merge's hook (3 s) runs (as a closed popup does), leaving the
// approval in flight.
func TestReviewChild(t *testing.T) {
	joined := os.Getenv(reviewChildArgs)
	if joined == "" {
		t.Skip("run by the signal tests in a child process")
	}
	t.Setenv("HOME", os.Getenv(reviewChildHome))
	t.Setenv("CURIA_DB", os.Getenv(reviewChildDB))
	userTerminalAs(t, true)
	useFakeTmux(t)
	args := strings.Split(joined, "\x1f")
	if args[0] == "view" {
		args = args[1:]
		reviewChildView(t)
	}
	stdout, stderr, err := reviewRunCmd(t, "", args...)
	fmt.Printf("child err=%v\nstdout=%s\nstderr=%s\n", err, stdout, stderr)
}

// reviewChildView makes runProgram drive the review view: a then y, then
// it returns a second later, with the approval still in flight.
func reviewChildView(t *testing.T) {
	t.Helper()
	clk := &decideClock{t: time.Now()}
	prevClock, prevTerm, prevProgram := clock, reviewIsTerminal, runProgram
	clock, reviewIsTerminal = clk.now, func() bool { return true }
	runProgram = func(m tea.Model, _ ...tea.ProgramOption) error {
		m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
		m = reviewPump(t, m, m.Init())
		for _, k := range []string{"a", "y"} {
			clk.advance(3 * time.Second)
			var cmd tea.Cmd
			m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
			m = reviewPump(t, m, cmd)
		}
		time.Sleep(time.Second)
		return nil
	}
	t.Cleanup(func() { clock, reviewIsTerminal, runProgram = prevClock, prevTerm, prevProgram })
}

// reviewChild starts curia args in a child process (TestReviewChild) in a
// process group of its own, against f's home and store, with a hook named
// hook that touches a marker, sleeps for 3 s and succeeds; it returns once
// the hook has started, with the marker's path (the hook touches the same
// path with ".done" when it ends). The child is killed if it is still
// running when the test ends.
func reviewChild(t *testing.T, f *reviewFixture, hook string, args ...string) (cmd *exec.Cmd, out *bytes.Buffer, marker string) {
	t.Helper()
	marker = filepath.Join(t.TempDir(), "hook-started")
	testkit.LifecycleHooks(f.SpawnFixture, hook, "#!/bin/sh\ntouch '"+marker+"'\nsleep 3\ntouch '"+marker+".done'\nexit 0\n")
	self, err := os.Executable() // this test binary
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(self, "-test.run", "^TestReviewChild$", "-test.v")
	cmd.Env = append(os.Environ(), reviewChildArgs+"="+strings.Join(args, "\x1f"),
		reviewChildHome+"="+os.Getenv("HOME"), reviewChildDB+"="+f.DB)
	out = &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	testkit.WaitFile(t, marker)
	return cmd, out, marker
}

// reviewChildWait waits up to 60 s for the child to exit.
func reviewChildWait(t *testing.T, cmd *exec.Cmd, out *bytes.Buffer) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Logf("child exited: %v\n%s", err, out)
	case <-time.After(60 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("the child did not exit\n%s", out)
	}
}

// TestApproveSurvivesHangup: the terminal of curia review --approve goes
// away (a hang-up to its process group) while a pre-merge-commit hook runs:
// the merge completes, and the review is recorded merged before curia exits.
func TestApproveSurvivesHangup(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	cmd, out, _ := reviewChild(t, f, "pre-merge-commit", "review", "R-1", "--approve", "--merge", "--head", head)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	reviewChildWait(t, cmd, out)
	f.WantMerged(head)
}

// TestApproveSurvivesTerminate: only curia gets SIGTERM, mid-merge: it
// waits for the merge and records it.
func TestApproveSurvivesTerminate(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	cmd, out, _ := reviewChild(t, f, "pre-merge-commit", "review", "R-1", "--approve", "--merge", "--head", head)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	reviewChildWait(t, cmd, out)
	f.WantMerged(head)
	// M-2: it said it waits, and logged the outcome.
	if !strings.Contains(out.String(), "a review action is still running; waiting") {
		t.Errorf("the child did not say it waits:\n%s", out)
	}
	if log := strings.Join(readLog(t), "\n"); !strings.Contains(log, "review action done") || !strings.Contains(log, "R-1 approved: merged") {
		t.Errorf("the log lacks the outcome:\n%s", log)
	}
}

// TestApproveViewSurvivesHangup: the review view's program ends (the popup
// closed) while its approval merges, and a hang-up follows: curia review
// waits for the approval and records it before it exits.
func TestApproveViewSurvivesHangup(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	cmd, out, _ := reviewChild(t, f, "pre-merge-commit", "view", "review", "T-1")
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	reviewChildWait(t, cmd, out)
	f.WantMerged(head)
}

// TestDashboardWaitsForReviewAction: the dashboard's program returns while
// the approval it started still merges: curia ui returns only once the
// review is recorded.
func TestDashboardWaitsForReviewAction(t *testing.T) {
	f := reviewWithLead(t, model.StateIdle)
	head := f.Head(f.Wt, "HEAD")
	testkit.LifecycleHooks(f.SpawnFixture, "pre-merge-commit", "#!/bin/sh\nsleep 2\nexit 0\n")
	prev := runProgram
	runProgram = func(m tea.Model, _ ...tea.ProgramOption) error {
		m = uiToSessionList(m)
		for _, k := range []string{"r", "a", "y"} {
			f.Now = f.Now.Add(2 * time.Second)
			var cmd tea.Cmd
			m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
			m = reviewPump(t, m, cmd) // drops the approval's command, still running, after 300 ms
		}
		return nil
	}
	t.Cleanup(func() { runProgram = prev })
	if _, err := runRoot(t, "ui"); err != nil {
		t.Fatal(err)
	}
	f.WantMerged(head)
}

// TestApproveRecordsMergeFoundDone: the approving curia is killed (SIGKILL,
// nothing can hold it) after git made the merge commit, while the
// post-merge hook runs. git, in its own process group, finishes; the review
// stays pending with the approval's intent; the next approve finds the head
// in the base and records the review merged, with the stat of the intent.
func TestApproveRecordsMergeFoundDone(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	cmd, out, marker := reviewChild(t, f, "post-merge", "review", "R-1", "--approve", "--merge", "--head", head)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	reviewChildWait(t, cmd, out)
	testkit.WaitFile(t, marker+".done")
	// git exits after its hook: wait for it to leave no merge under way.
	for deadline := time.Now().Add(30 * time.Second); f.HasMergeHead() || f.Head(f.Repo, "main^2") != head; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the orphaned git did not finish its merge")
		}
	}
	if r := f.Review(); r.Status != model.ReviewPending || r.Head != head || r.IntentAt.IsZero() || r.ApprovedHow != model.ApproveMerge {
		t.Fatalf("after the kill R-1 = %+v, want pending with the intent to merge %s", r, head)
	}
	line, err := f.approve(model.ApproveMerge)
	if err != nil {
		t.Fatal(err)
	}
	if want := "R-1 approved: " + reviewBranch + " (" + head[:7] + ") is already merged into main; recorded. " +
		"Remove the worktree and branch with: curia clean T-1"; line != want {
		t.Errorf("line = %q, want %q", line, want)
	}
	f.WantMerged(head)
	if r := f.Review(); r.ApprovedHow != model.ApproveMerge || !strings.Contains(r.DiffStat, "auth.go") || r.ClosedVia != model.AnsweredCLI {
		t.Errorf("R-1 = %+v, want merged by merge with the intent's stat", r)
	}
}

// TestSecondApproverRefusedWhileFirstMerges: while one curia merges, a
// second approval, a change request and a drop of the same review are
// refused, naming the approval in flight; the first then records its merge.
func TestSecondApproverRefusedWhileFirstMerges(t *testing.T) {
	f := reviewNew(t)
	f.Now = time.Now()
	head := f.Head(f.Wt, "HEAD")
	cmd, out, _ := reviewChild(t, f, "pre-merge-commit", "review", "R-1", "--approve", "--merge", "--head", head)
	_, err := f.approve(model.ApproveMerge)
	if err == nil || !strings.Contains(err.Error(), "R-1 is being approved (merge at "+head[:7]+") by another curia since ") {
		t.Errorf("second approve: %v, want the in-flight refusal", err)
	}
	if _, err = reviewDrop(context.Background(), f.St, "R-1", model.AnsweredCLI, f.Now); err == nil ||
		!strings.Contains(err.Error(), "is being approved") || !strings.Contains(err.Error(), "If that curia was killed, drop again after ") {
		t.Errorf("drop during the approval: %v, want the in-flight refusal", err)
	}
	if _, err = reviewRequestChanges(context.Background(), f.St, f.Fake, testLoggerOnly(), "R-1", "more", model.AnsweredCLI, f.Now); err == nil ||
		!strings.Contains(err.Error(), "is being approved") || !strings.Contains(err.Error(), "If that curia was killed, request changes again after ") {
		t.Errorf("request changes during the approval: %v, want the in-flight refusal", err)
	}
	reviewChildWait(t, cmd, out)
	f.WantMerged(head)
	if strings.Count(testkit.WorktreeGit(t, f.Repo, "log", "--merges", "--format=%H"), "\n") != 0 {
		t.Error("more than one merge commit")
	}
}

// TestStaleIntentDoesNotBlock: an intent older than the hold, left by an
// approval that was killed before git merged, does not refuse a new one.
func TestStaleIntentDoesNotBlock(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	if _, err := f.St.ClaimReviewIntent(context.Background(), "R-1", store.ReviewIntent{How: model.ApproveMerge, Head: head},
		f.Now.Add(-review.IntentHold), review.IntentHold); err != nil {
		t.Fatal(err)
	}
	if _, err := f.approve(model.ApproveMerge); err != nil {
		t.Fatal(err)
	}
	f.WantMerged(head)
}

// TestApproveFailureClearsIntent: a merge or a pull request that fails with
// nothing done leaves the review pending with no intent.
func TestApproveFailureClearsIntent(t *testing.T) {
	f := reviewNew(t)
	f.CommitOnBranch("README", "branch side\n", "branch readme")
	f.CommitOnMain("README", "main side\n", "main readme")
	if _, err := f.approve(model.ApproveMerge); err == nil {
		t.Fatal("conflict: approved")
	}
	if r := f.Review(); r.Status != model.ReviewPending || !r.IntentAt.IsZero() || r.Head != "" || r.ApprovedHow != "" || r.DiffStat != "" {
		t.Errorf("after a conflict R-1 = %+v, want pending with no intent", r)
	}
	g := reviewNew(t)
	bin := filepath.Join(t.TempDir(), "bin")
	testkit.Executable(t, bin, "gh", "#!/bin/sh\necho 'no permission' >&2\nexit 1\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prevGH, prevLook := review.GH, review.GHLookPath
	review.GH, review.GHLookPath = reviewRealGH, exec.LookPath
	t.Cleanup(func() { review.GH, review.GHLookPath = prevGH, prevLook })
	if _, err := g.approve(model.ApprovePR); err == nil {
		t.Fatal("failing gh: approved")
	}
	if r := g.Review(); r.Status != model.ReviewPending || !r.IntentAt.IsZero() || r.Head != "" {
		t.Errorf("after gh failed R-1 = %+v, want pending with no intent", r)
	}
}

// TestApproveFlagNeedsHead: without --head the flag form approves nothing:
// it prints the branch's head, its stat line and the exact command that
// approves that commit, and exits 1.
func TestApproveFlagNeedsHead(t *testing.T) {
	f := reviewNew(t)
	head, before := f.Head(f.Wt, "HEAD"), f.Head(f.Repo, "HEAD")
	for _, c := range []struct {
		args []string
		flag string
	}{{[]string{"review", "R-1", "--approve"}, ""}, {[]string{"review", "T-1", "--approve", "--pr"}, " --pr"}} {
		stdout, stderr, err := reviewRunCmd(t, "", c.args...)
		if err == nil || stdout != "" {
			t.Fatalf("%q: approved: %q, %v", c.args, stdout, err)
		}
		msg := err.Error()
		for _, want := range []string{
			"--approve needs --head <commit>", reviewBranch + " is at " + head + " (1 file changed, 1 insertion(+))",
			"\n  curia review R-1 --approve" + c.flag + " --head " + head,
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q: error %q lacks %q", c.args, msg, want)
			}
		}
		if !strings.Contains(stderr, head) {
			t.Errorf("%q: stderr %q does not show the head", c.args, stderr)
		}
	}
	if f.Head(f.Repo, "HEAD") != before {
		t.Error("main moved")
	}
	if r := f.Review(); r.Status != model.ReviewPending || !r.IntentAt.IsZero() {
		t.Errorf("R-1 = %+v, want pending with no intent", r)
	}
}

// TestApproveFlagRefusesMovedBranch: a commit that lands on the branch after
// the user looked is never merged: --head (whole or a prefix) no longer
// names the branch's head.
func TestApproveFlagRefusesMovedBranch(t *testing.T) {
	f := reviewNew(t)
	shown, before := f.Head(f.Wt, "HEAD"), f.Head(f.Repo, "HEAD")
	f.CommitOnBranch("evil.go", "package auth // added after the developer looked\n", "sneak")
	for _, h := range []string{shown} {
		_, _, err := reviewRunCmd(t, "", "review", "R-1", "--approve", "--head", h)
		if err == nil || !strings.Contains(err.Error(), "the branch changed since you looked: "+reviewBranch+" is now at ") {
			t.Errorf("--head %s: %v, want the changed-branch refusal", h, err)
		}
	}
	if f.Head(f.Repo, "HEAD") != before {
		t.Error("main moved")
	}
	if _, err := os.Stat(filepath.Join(f.Repo, "evil.go")); err == nil {
		t.Error("the unseen commit was merged")
	}
	f.WantPending()
}

// TestApproveFlagRefusesPrefix: --head must be the branch head's full commit
// id, of the length the repository's object format gives; a prefix, however
// unique, and anything else are refused before git merges, with the full head
// shown. The full id merges.
func TestApproveFlagRefusesPrefix(t *testing.T) {
	f := reviewNew(t)
	head := f.Head(f.Wt, "HEAD")
	for _, bad := range []string{
		head[:7], head[:12], head[:39], "ABCDEF" + head[6:], "HEAD", "main", "-x", "--upload-pack=x", head + "0",
		"g" + head[1:], head + strings.Repeat("0", 24),
	} {
		_, _, err := reviewRunCmd(t, "", "review", "R-1", "--approve", "--head", bad)
		if err == nil || !strings.Contains(err.Error(), "give the full commit id you looked at (40 lower-case hex digits; a prefix is not enough)") ||
			!strings.Contains(err.Error(), reviewBranch+" is at "+head) {
			t.Errorf("--head %q: %v, want the refusal showing the full head", bad, err)
		}
	}
	other := f.Head(f.Repo, "main")
	if _, _, err := reviewRunCmd(t, "", "review", "R-1", "--approve", "--head", other); err == nil ||
		!strings.Contains(err.Error(), "the branch changed since you looked") {
		t.Errorf("--head of another commit: %v", err)
	}
	f.WantPending()
	stdout, stderr, err := reviewRunCmd(t, "", "review", "R-1", "--approve", "--head", head)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	if !strings.HasPrefix(stdout, "R-1 approved: merged "+reviewBranch+" into main ("+head[:7]+")") {
		t.Errorf("stdout = %q", stdout)
	}
	f.WantMerged(head)
}

// A held signal while a dashboard pause (or a review action, or both) runs
// names what runs, and the command that shows its outcome.
func TestFlightWordsNameWhatRuns(t *testing.T) {
	for _, c := range []struct {
		pause, review bool
		what, follow  string
	}{
		{false, true, "a review action", "curia reviews ls"},
		{true, false, "a pause", "curia task ls"},
		{true, true, "a pause and a review action", "curia task ls and curia reviews ls"},
	} {
		f := &reviewInFlight{}
		if c.pause {
			defer f.beginPause()()
		}
		if c.review {
			defer f.begin()()
		}
		if what, follow := f.words(); what != c.what || follow != c.follow {
			t.Errorf("pause %v review %v: words = %q, %q; want %q, %q", c.pause, c.review, what, follow, c.what, c.follow)
		}
	}
	saved := reviewFlight
	defer func() { reviewFlight = saved }()
	reviewFlight = &reviewInFlight{}
	end := reviewFlight.beginPause()
	sig, cancel := context.WithCancel(context.Background())
	cancel()
	time.AfterFunc(50*time.Millisecond, end)
	var buf strings.Builder
	if err := reviewAfterProgram(sig, &buf); err != nil || !strings.Contains(buf.String(), "a pause is still running") || strings.Contains(buf.String(), "review") {
		t.Errorf("waiting line for a pause = %q, %v", buf.String(), err)
	}
}

// The gh and diff-tool seam helpers of internal/review's tests, for the command's tests.

// reviewGHCall is one recorded gh call.
type reviewGHCall struct {
	dir  string
	args []string
}

// reviewGHRecorder records gh calls and answers with out and err.
type reviewGHRecorder struct{ calls []reviewGHCall }

// reviewUseGH makes review.GH a recorder answering out, and finds gh.
func reviewUseGH(t *testing.T, out string) *reviewGHRecorder {
	t.Helper()
	rec := &reviewGHRecorder{}
	prevGH, prevLook := review.GH, review.GHLookPath
	review.GH = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		rec.calls = append(rec.calls, reviewGHCall{dir: dir, args: slices.Clone(args)})
		return []byte(out), nil
	}
	review.GHLookPath = func(string) (string, error) { return "/opt/bin/gh", nil }
	t.Cleanup(func() { review.GH, review.GHLookPath = prevGH, prevLook })
	return rec
}

// reviewUseDiffTools makes review.DiffLookPath look.
func reviewUseDiffTools(t *testing.T, look func(string) (string, error)) {
	t.Helper()
	prev := review.DiffLookPath
	review.DiffLookPath = look
	t.Cleanup(func() { review.DiffLookPath = prev })
}

// The test defaults: no test runs the real gh or a diff tool unless it sets
// its own (TestPRGhMissingOrFailing runs the real reviewGH against a
// stand-in gh).
func init() {
	review.GH = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		panic("review.GH called without a recorder: gh " + strings.Join(args, " "))
	}
	review.GHLookPath = func(name string) (string, error) {
		panic("review.GHLookPath called without a stub: " + name)
	}
	review.DiffLookPath = func(name string) (string, error) {
		panic("review.DiffLookPath called without a stub: " + name)
	}
}

// reviewRealGH is review.GH as the command ships it, kept before the test default replaces it.
var reviewRealGH = review.GH

package ui

// The review view is tested from inside the package: its refreshes are
// scheduled through the unexported tick seam and the diff tool runs through
// the run seam, so a test feeds the tick messages after moving the fake
// clock instead of sleeping, and no tool is started.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/task"
)

// reviewStart is the fixed start of the review tests' clock.
var reviewStart = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

const (
	// reviewGap is the pace of a person reading: past every quiet period.
	reviewGap = 2500 * time.Millisecond
	// reviewTyping is the pace of typing at normal speed.
	reviewTyping = 80 * time.Millisecond
	// reviewRepeat is the gap between the repeats of a held key.
	reviewRepeat = 30 * time.Millisecond
)

// fakeReviews serves reviews and records every call but Queue. A review
// that is approved, sent back or dropped leaves the queue, as it does in
// the store.
type fakeReviews struct {
	mu                                   sync.Mutex
	details                              []ReviewDetail
	queueErr, detailErr, actErr, diffErr error
	line                                 string
	diff                                 *exec.Cmd
	calls                                []string
	heads                                []string // the head given to each Approve
}

func (f *fakeReviews) Queue(context.Context) ([]ReviewRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.queueErr != nil {
		return nil, f.queueErr
	}
	rows := make([]ReviewRow, 0, len(f.details))
	for _, d := range f.details {
		rows = append(rows, ReviewRow{Review: d.Review, Task: d.Task})
	}
	return rows, nil
}

func (f *fakeReviews) Detail(_ context.Context, id string) (ReviewDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Detail "+id)
	if f.detailErr != nil {
		return ReviewDetail{}, f.detailErr
	}
	for _, d := range f.details {
		if d.Review.ID == id {
			return d, nil
		}
	}
	return ReviewDetail{}, errors.New("no review " + id)
}

func (f *fakeReviews) act(call, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.actErr != nil {
		return "", f.actErr
	}
	f.details = slices.DeleteFunc(slices.Clone(f.details), func(d ReviewDetail) bool { return d.Review.ID == id })
	return f.line, nil
}

// Approve refuses, as the command does, when the review's branch is no
// longer at the head that was shown.
func (f *fakeReviews) Approve(_ context.Context, id, head string) (string, error) {
	f.mu.Lock()
	f.heads = append(f.heads, head)
	moved := false
	for _, d := range f.details {
		moved = moved || (d.Review.ID == id && d.Head != head)
	}
	if moved {
		f.calls = append(f.calls, "Approve "+id)
		f.mu.Unlock()
		return "", errors.New(rvBranchChanged)
	}
	f.mu.Unlock()
	return f.act("Approve "+id, id)
}

// rvBranchChanged is the command's refusal of an approval whose branch
// moved after it was shown.
const rvBranchChanged = "the branch changed since you looked; showing the new state"

func (f *fakeReviews) approvedHeads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.heads)
}

func (f *fakeReviews) RequestChanges(_ context.Context, id, note string) (string, error) {
	return f.act("RequestChanges "+id+" "+note, id)
}

func (f *fakeReviews) Drop(_ context.Context, id string) (string, error) {
	return f.act("Drop "+id, id)
}

func (f *fakeReviews) DiffCmd(_ context.Context, id string) (*exec.Cmd, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "DiffCmd "+id)
	return f.diff, f.diffErr
}

func (f *fakeReviews) set(ds ...ReviewDetail) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.details = slices.Clone(ds)
}

// edit changes review R-1 in place.
func (f *fakeReviews) edit(fn func(*ReviewDetail)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.details {
		if f.details[i].Review.ID == "R-1" {
			fn(&f.details[i])
		}
	}
}

func (f *fakeReviews) fail(queue, detail, act error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queueErr, f.detailErr, f.actErr = queue, detail, act
}

func (f *fakeReviews) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// actions are the recorded calls but Detail.
func (f *fakeReviews) actions() []string {
	return slices.DeleteFunc(f.recorded(), func(c string) bool { return strings.HasPrefix(c, "Detail ") })
}

func wantActions(t *testing.T, src *fakeReviews, want ...string) {
	t.Helper()
	if got := src.actions(); !slices.Equal(got, want) {
		t.Errorf("source actions = %q, want %q", got, want)
	}
}

// rvDetail is review R-n of task T-(40+n), waiting for 12 minutes, with a
// diff, one decided ticket and three sessions, approved by a merge.
func rvDetail(n int) ReviewDetail {
	id, tid := fmt.Sprintf("R-%d", n), fmt.Sprintf("T-%d", 40+n)
	at := reviewStart.Add(-12 * time.Minute)
	return ReviewDetail{
		Review: model.Review{
			N: int64(n), ID: id, TaskID: tid, SessionID: "S" + tid,
			Summary: "Fixed the token refresh race in " + tid + ".",
			Tests:   model.TestsPassed, Status: model.ReviewPending,
			CreatedAt: at, UpdatedAt: at,
		},
		Task: model.Task{
			ID: tid, Title: "Fix the auth refresh", Repo: "/src/api-server",
			BaseBranch: "main", Branch: "curia/" + tid + "-fix-the-auth-refresh",
		},
		DiffStat: " auth/refresh.go | 12 ++++++------\n 1 file changed, 6 insertions(+), 6 deletions(-)",
		Decided: []model.Decision{{
			ID: "D-4", Question: "Retry the refresh on 401?", AnswerKey: "A",
			Options: []model.Option{{Key: "A", Label: "Retry once"}}, Status: model.StatusAnswered,
		}},
		Sessions: 3, Teammates: 2, Duration: 72 * time.Minute,
		Approve: model.ApproveMerge,
	}
}

func rvAssume(id, question, assumption string) model.Decision {
	return model.Decision{ID: id, Question: question, Assumption: assumption, Mode: model.ModeAssume, Status: model.StatusOpen}
}

func rvBlocking(id, question string) model.Decision {
	return model.Decision{ID: id, Question: question, Mode: model.ModeBlocking, Status: model.StatusOpen}
}

// reviewRun drives one review view with the fake clock.
type reviewRun struct {
	t       *testing.T
	clk     *sayClock
	m       tea.Model
	quit    bool
	closed  bool
	ticks   []time.Duration
	tools   []*exec.Cmd
	toolErr error
}

// newReviewRun opens a view of width x height and runs its first refresh;
// the clock is where the view opened.
func newReviewRun(t *testing.T, src *fakeReviews, opts ReviewOptions, width, height int) *reviewRun {
	t.Helper()
	r := &reviewRun{t: t, clk: &sayClock{t: reviewStart}}
	m := NewReview(src, r.clk.now, opts)
	m.tick = func(d time.Duration, msg tea.Msg) tea.Cmd {
		r.ticks = append(r.ticks, d)
		if c, ok := msg.(reviewConfirmMsg); ok { // burstGap passes with no other key, unless a test steps
			return func() tea.Msg { return c }
		}
		return nil
	}
	m.run = func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
		return func() tea.Msg {
			r.tools = append(r.tools, c)
			return fn(r.toolErr)
		}
	}
	r.m = m
	r.feed(tea.WindowSizeMsg{Width: width, Height: height})
	r.run(r.m.Init())
	return r
}

// openReview is newReviewRun at 200 x 60 with the clock moved on by
// reviewGap: the first review has been read.
func openReview(t *testing.T, src *fakeReviews, opts ReviewOptions) *reviewRun {
	t.Helper()
	r := newReviewRun(t, src, opts, 200, 60)
	r.clk.advance(reviewGap)
	return r
}

func (r *reviewRun) feed(msg tea.Msg) {
	r.t.Helper()
	var cmd tea.Cmd
	r.m, cmd = r.m.Update(msg)
	r.run(cmd)
}

// step feeds msg and returns its command without running it: the call it
// starts has not returned yet.
func (r *reviewRun) step(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	r.m, cmd = r.m.Update(msg)
	return cmd
}

func (r *reviewRun) run(cmd tea.Cmd) {
	r.t.Helper()
	for steps := 0; cmd != nil; steps++ {
		if steps > 10 {
			r.t.Fatal("commands did not settle")
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				r.run(c)
			}
			return
		case tea.QuitMsg:
			r.quit = true
			return
		case ReviewClosedMsg:
			r.closed = true
			return
		default:
			r.m, cmd = r.m.Update(msg)
		}
	}
}

func rvKey(k string) tea.KeyMsg {
	switch k {
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return sayKey(k)
}

// at sends keys, each d after the one before.
func (r *reviewRun) at(d time.Duration, keys ...string) {
	r.t.Helper()
	for _, k := range keys {
		r.clk.advance(d)
		r.feed(rvKey(k))
	}
}

func (r *reviewRun) typeText(s string) {
	r.t.Helper()
	for _, c := range s {
		r.at(reviewTyping, string(c))
	}
}

func (r *reviewRun) paste(s string) {
	r.t.Helper()
	r.clk.advance(reviewTyping)
	r.feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true})
}

// heldFor is how long hold holds a key.
const heldFor = 3 * time.Second

// hold presses k now, then again after first and every reviewRepeat until
// heldFor has passed since the press: a held key whose first repeat comes
// after first.
func (r *reviewRun) hold(k string, first time.Duration) {
	r.t.Helper()
	r.at(0, k)
	r.at(first, k)
	for held := first + reviewRepeat; held < heldFor; held += reviewRepeat {
		r.at(reviewRepeat, k)
	}
}

// queueTick is the one-second refresh firing.
func (r *reviewRun) queueTick() {
	r.t.Helper()
	r.feed(reviewTickMsg{})
}

// detailTick is the five-second detail refresh firing.
func (r *reviewRun) detailTick() {
	r.t.Helper()
	r.feed(reviewDetailTickMsg{})
}

func (r *reviewRun) view() string { return r.m.View() }

func (r *reviewRun) review() ReviewModel {
	r.t.Helper()
	m, ok := r.m.(ReviewModel)
	if !ok {
		r.t.Fatalf("model is %T", r.m)
	}
	return m
}

func (r *reviewRun) wantIn(s string) {
	r.t.Helper()
	if !strings.Contains(r.view(), s) {
		r.t.Errorf("view has no %q:\n%s", s, r.view())
	}
}

func (r *reviewRun) wantNotIn(s string) {
	r.t.Helper()
	if strings.Contains(r.view(), s) {
		r.t.Errorf("view has %q:\n%s", s, r.view())
	}
}

// status is the last line of the view.
func (r *reviewRun) status() string {
	lines := strings.Split(r.view(), "\n")
	return lines[len(lines)-1]
}

func (r *reviewRun) wantStatus(s string) {
	r.t.Helper()
	if got := r.status(); got != s {
		r.t.Errorf("status line = %q, want %q\n%s", got, s, r.view())
	}
}

// wantShown fails unless review id is the one on screen.
func (r *reviewRun) wantShown(id string) {
	r.t.Helper()
	if got := r.review().selectedID(); got != id {
		r.t.Errorf("shown review = %q, want %q", got, id)
	}
}

// wantPrompt fails unless the prompt or input open is mode.
func (r *reviewRun) wantPrompt(mode reviewMode) {
	r.t.Helper()
	if got := r.review().mode; got != mode {
		r.t.Errorf("mode = %d, want %d\n%s", got, mode, r.view())
	}
}

const (
	mergePrompt = "merge curia/T-41-fix-the-auth-refresh into main? y confirms, any other key cancels"
	pushPrompt  = "push curia/T-41-fix-the-auth-refresh and open a pull request? y confirms, any other key cancels"
	dropPrompt  = "drop T-41? its agent is not stopped. y confirms, any other key cancels"
)

func TestReviewShowsDetailInOrder(t *testing.T) {
	d := rvDetail(1)
	d.Assumptions = []model.Decision{rvAssume("D-9", "Keep the old token format?", "keep it")}
	d.Blocking = []model.Decision{rvBlocking("D-11", "Which issuer is canonical?")}
	d.Review.TestsDetail = "42 tests, 0 failures"
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	view := r.view()
	order := []string{
		"R-1 · T-41 Fix the auth refresh · api-server · curia/T-41-fix-the-auth-refresh → main · waiting 12m",
		"review 1 of 1",
		"D-9 unconfirmed assumption: Keep the old token format? → proceeding with: keep it",
		"D-11 open blocking ticket: Which issuer is canonical?",
		"Summary (the agent's):",
		"Fixed the token refresh race in T-41.",
		"Tests (the agent's report): passed",
		"42 tests, 0 failures",
		"Diff against main:",
		" auth/refresh.go | 12 ++++++------",
		" 1 file changed, 6 insertions(+), 6 deletions(-)",
		"Decided tickets on this task:",
		"D-4 answered: Retry the refresh on 401? → A Retry once",
		"Sessions: 3 (lead and 2 teammates) · duration 1h12m · cost not recorded",
		"a merge into main",
	}
	at := -1
	for _, s := range order {
		i := strings.Index(view, s)
		if i < 0 {
			t.Fatalf("view has no %q:\n%s", s, view)
		}
		if i <= at {
			t.Errorf("%q is out of section 4.5's order:\n%s", s, view)
		}
		at = i
	}
	if lines := strings.Split(view, "\n"); !strings.HasSuffix(lines[0], "review 1 of 1") {
		t.Errorf("first line = %q, want the position at its right", lines[0])
	}
	if got := src.recorded(); !slices.Equal(got, []string{"Detail R-1"}) {
		t.Errorf("calls = %q, want one Detail", got)
	}
}

func TestReviewSessionsLineForms(t *testing.T) {
	d := rvDetail(1)
	d.Sessions, d.Teammates, d.Duration = 2, 1, 45*time.Minute
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.wantIn("Sessions: 2 (lead and 1 teammate) · duration 45m · cost not recorded")

	d.Sessions, d.Teammates, d.Duration = 1, 0, 0
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.wantIn("Sessions: 1 · duration unknown · cost not recorded")
}

func TestReviewAssumptionsAreRedAndFirst(t *testing.T) {
	d := rvDetail(1)
	d.Assumptions = []model.Decision{
		rvAssume("D-4", "Keep the old token format?", "keep it"),
		rvAssume("D-9", "Log refresh failures?", "log at warn"),
	}
	d.Blocking = []model.Decision{rvBlocking("D-11", "Which issuer is canonical?")}
	d.Review.Summary = strings.Repeat("summary line\n", 80)
	src := &fakeReviews{}
	src.set(d)
	r := newReviewRun(t, src, ReviewOptions{}, 200, 24)
	r.clk.advance(reviewGap)

	body := r.review().body
	want := []string{
		"D-4 unconfirmed assumption: Keep the old token format? → proceeding with: keep it",
		"D-9 unconfirmed assumption: Log refresh failures? → proceeding with: log at warn",
		"D-11 open blocking ticket: Which issuer is canonical?",
	}
	if len(body) < len(want) {
		t.Fatalf("body has %d lines", len(body))
	}
	for i, w := range want {
		if body[i].text != w || body[i].kind != lineAlert {
			t.Errorf("body line %d = %+v, want the red line %q", i, body[i], w)
		}
	}
	if got := styleOf(lineAlert).GetForeground(); got != lipgloss.Color("1") {
		t.Errorf("alert lines are drawn in %v, want red (1)", got)
	}
	lines := strings.Split(r.view(), "\n")
	if !strings.HasPrefix(lines[1], want[0]) {
		t.Errorf("line under the head = %q, want the first assumption", lines[1])
	}

	// Scrolled past them, a red line under the head still says they are there.
	r.at(reviewGap, "pgdown", "pgdown")
	lines = strings.Split(r.view(), "\n")
	if lines[1] != "2 unconfirmed assumptions, 1 open blocking ticket above (k scrolls up)" {
		t.Errorf("line under the head when scrolled = %q, want the pinned warning\n%s", lines[1], r.view())
	}
	if r.review().top(200)[1].kind != lineAlert {
		t.Error("the pinned warning is not red")
	}
}

func TestReviewCostNotRecorded(t *testing.T) {
	d := rvDetail(1)
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.wantIn("cost not recorded")
	r.wantNotIn("$")

	d.CostKnown, d.CostUSD = true, 4.12
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.wantIn("Sessions: 3 (lead and 2 teammates) · duration 1h12m · cost $4.12")
	r.wantNotIn("cost not recorded")
}

func TestReviewDiffNoteShown(t *testing.T) {
	d := rvDetail(1)
	d.DiffNote = "the worktree has uncommitted changes that are not in this diff"
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	view := r.view()
	stat, note := strings.Index(view, "1 file changed"), strings.Index(view, d.DiffNote)
	if stat < 0 || note < stat {
		t.Errorf("the diff note is not under the diff stat:\n%s", view)
	}
	if i := slices.IndexFunc(r.review().body, func(l bodyLine) bool { return l.text == d.DiffNote }); i < 0 || r.review().body[i].kind != lineWarn {
		t.Errorf("the diff note is not drawn as a warning: %+v", r.review().body)
	}

	d.DiffStat, d.DiffNote = "", "branch curia/T-41-fix-the-auth-refresh does not exist"
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.wantIn("Diff against main:\n" + d.DiffNote)
}

func TestReviewEmptyQueueAndNoReviewForTask(t *testing.T) {
	src := &fakeReviews{}
	r := openReview(t, src, ReviewOptions{})
	r.wantIn("no review is waiting")
	r.wantStatus("q quit")

	src.set(rvDetail(1))
	r = openReview(t, src, ReviewOptions{TaskID: "T-42"})
	r.wantIn("T-42 has no review waiting")
	r.wantNotIn("R-1")
	r.at(reviewGap, "a")
	wantActions(t, src)

	src.set(rvDetail(1), rvDetail(2))
	r = openReview(t, src, ReviewOptions{TaskID: "T-42"})
	r.wantShown("R-2")
	r.wantIn("review 1 of 1")
}

func TestReviewMovesBetweenReviews(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1), rvDetail(2), rvDetail(3))
	r := openReview(t, src, ReviewOptions{})
	r.wantShown("R-1")
	r.at(reviewGap, "]")
	r.wantShown("R-2")
	r.wantIn("review 2 of 3")
	r.wantIn("Fixed the token refresh race in T-42.")
	r.at(reviewGap, "n")
	r.wantShown("R-3")
	r.at(reviewGap, "]") // the last: stays
	r.wantShown("R-3")
	r.at(reviewGap, "[")
	r.wantShown("R-2")
	r.at(reviewGap, "p")
	r.wantShown("R-1")
	want := []string{"Detail R-1", "Detail R-2", "Detail R-3", "Detail R-2", "Detail R-1"}
	if got := src.recorded(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestReviewScrollsLongSummary(t *testing.T) {
	d := rvDetail(1)
	var b strings.Builder
	for i := 1; i <= 500; i++ { // 1892 runes: inside task.MaxSummary
		fmt.Fprintf(&b, "%d\n", i)
	}
	d.Review.Summary = b.String()
	src := &fakeReviews{}
	src.set(d)
	for _, width := range []int{40, 80, 200} {
		r := newReviewRun(t, src, ReviewOptions{}, width, 30)
		r.clk.advance(reviewGap)
		if n := len(strings.Split(r.view(), "\n")); n != 30 {
			t.Errorf("width %d: %d lines, want the height 30", width, n)
		}
		r.wantIn("\n1\n2\n")
		r.wantNotIn("\n500\n")
		r.at(reviewGap, "j", "down")
		r.wantNotIn("Summary (the agent's):")
		for range 40 {
			r.at(reviewGap, "pgdown")
		}
		r.wantIn("\n499\n500\n")
		r.wantIn("Sessions: 3")
		checkClean(t, fmt.Sprintf("width %d scrolled", width), r.view(), width)
		r.at(reviewGap, "k", "up", "pgup")
		r.wantIn("a merge into main")
	}
}

// A summary or test detail longer than the store allows is cut where it
// is drawn, with an ellipsis.
func TestReviewBoundsAgentText(t *testing.T) {
	d := rvDetail(1)
	d.Review.Summary = strings.Repeat("s", task.MaxSummary) + "TAIL"
	d.Review.TestsDetail = strings.Repeat("t", task.MaxTestsDetail) + "TAIL"
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.wantNotIn("TAIL")
	r.wantIn("s…")
	r.wantIn("t…")
}

func TestReviewKeepsShownReviewAcrossRefresh(t *testing.T) {
	src := &fakeReviews{}
	one, two, three := rvDetail(1), rvDetail(2), rvDetail(3)
	src.set(one, two, three)
	r := openReview(t, src, ReviewOptions{})
	r.at(reviewGap, "]")
	r.wantShown("R-2")
	src.set(three, one, two)
	r.queueTick()
	r.wantShown("R-2")
	r.wantIn("review 3 of 3")

	// An approve prompt open on R-2 survives a reorder and confirms R-2.
	r.at(reviewGap, "a")
	r.wantPrompt(reviewConfirmApprove)
	src.set(two, three, one)
	r.queueTick()
	r.wantPrompt(reviewConfirmApprove)
	r.at(reviewGap, "y")
	wantActions(t, src, "Approve R-2")

	// The shown review leaving puts its neighbor on screen.
	src.set(rvDetail(4), rvDetail(5))
	r.clk.advance(reviewGap)
	r.queueTick()
	r.wantShown("R-4")
}

func TestApproveNeedsConfirmationAfterDwell(t *testing.T) {
	src := &fakeReviews{line: "R-1 approved: merged curia/T-41-fix-the-auth-refresh into main (abc1234)."}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})

	// a and y in one read: the y is dropped with the burst.
	r.feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ay")})
	r.wantPrompt(reviewConfirmApprove)
	wantActions(t, src)
	// a y read together with the prompt's opening is too early.
	r.feed(rvKey("y"))
	wantActions(t, src)
	r.wantPrompt(reviewConfirmApprove)
	r.wantIn(mergePrompt)

	// Any other key cancels, q and Esc included, and closes nothing.
	for _, k := range []string{"n", "q", "esc", "enter", "Y", " "} {
		r.at(reviewGap, k)
		r.wantPrompt(reviewBrowse)
		if r.quit || r.closed {
			t.Fatalf("%q in the prompt closed the view", k)
		}
		r.at(reviewGap, "a")
		r.wantPrompt(reviewConfirmApprove)
	}
	wantActions(t, src)

	// A y in a burst of keys cancels: the prompt it answers was not drawn yet.
	r.clk.advance(reviewGap)
	r.feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yy")})
	r.wantPrompt(reviewBrowse)
	wantActions(t, src)

	r.at(reviewGap, "a")
	r.at(reviewGap, "y")
	wantActions(t, src, "Approve R-1")
	r.wantIn("R-1 approved: merged curia/T-41-fix-the-auth-refresh into main (abc1234).")
	r.wantIn("no review is waiting")
}

// An acted-on review leaves the screen when the action returns, and a queue
// read started before that cannot bring it back.
func TestActionDropsStaleReads(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1), rvDetail(2))
	r := openReview(t, src, ReviewOptions{})
	read := r.step(reviewTickMsg{})
	stale := read() // R-1 still pending
	r.at(0, "a")
	r.clk.advance(reviewGap)
	confirm := r.step(rvKey("y"))
	approve := r.step(confirm())
	r.step(approve()) // its refresh is not run yet
	r.wantShown("R-2")
	r.feed(stale)
	r.wantShown("R-2")
	r.wantIn("review 1 of 1")
	wantActions(t, src, "Approve R-1")
}

func TestApproveHeldConfirmKeyNeverConfirms(t *testing.T) {
	for _, first := range []time.Duration{400 * time.Millisecond, 500 * time.Millisecond, 660 * time.Millisecond} {
		for _, k := range []string{"a", "x"} {
			src := &fakeReviews{}
			src.set(rvDetail(1))
			r := openReview(t, src, ReviewOptions{})
			r.at(0, k)
			r.clk.advance(100 * time.Millisecond)
			r.hold("y", first) // pressed too early, then held
			wantActions(t, src)
			r.clk.advance(400 * time.Millisecond) // let go: under the quiet period
			r.feed(rvKey("y"))
			wantActions(t, src)
			r.clk.advance(quietPeriod)
			r.feed(rvKey("y"))
			want := map[string]string{"a": "Approve R-1", "x": "Drop R-1"}[k]
			wantActions(t, src, want)
		}
	}
}

func TestApproveHeldActionKeyNeverConfirms(t *testing.T) {
	for _, first := range []time.Duration{400 * time.Millisecond, 500 * time.Millisecond, 660 * time.Millisecond} {
		src := &fakeReviews{}
		src.set(rvDetail(1))
		r := openReview(t, src, ReviewOptions{})
		r.hold("a", first)
		r.wantPrompt(reviewBrowse) // the first repeat canceled the prompt; the rest are too soon
		r.at(100*time.Millisecond, "y")
		wantActions(t, src)
	}
}

func TestApprovePromptNamesAssumptions(t *testing.T) {
	d := rvDetail(1)
	d.Assumptions = []model.Decision{rvAssume("D-4", "q4", "a4"), rvAssume("D-9", "q9", "a9")}
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	lines := strings.Split(r.view(), "\n")
	n := len(lines)
	if lines[n-2] != "2 unconfirmed assumptions (D-4, D-9) will expire." || lines[n-1] != mergePrompt {
		t.Errorf("last lines = %q, want the expiry line above the prompt", lines[n-2:])
	}
	if b := r.review().bottom(200); b[len(b)-2].kind != lineAlert {
		t.Error("the expiry line is not red")
	}

	d.Assumptions = d.Assumptions[:1]
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.wantIn("\n1 unconfirmed assumption (D-4) will expire.\n" + mergePrompt)

	d.Assumptions = nil
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.wantNotIn("will expire")
	r.wantStatus(mergePrompt)

	// A drop names none: it expires nothing.
	d.Assumptions = []model.Decision{rvAssume("D-4", "q4", "a4")}
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "x")
	r.wantNotIn("will expire")
	r.wantStatus(dropPrompt)
}

func TestApprovePromptSaysMergeOrPR(t *testing.T) {
	d := rvDetail(1)
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.wantIn("a merge into main")
	r.at(0, "a")
	r.wantStatus(mergePrompt)

	d.Approve = model.ApprovePR
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.wantIn("a push and open a pull request")
	r.at(0, "a")
	r.wantStatus(pushPrompt)

	d.Approve = "rebase"
	src.set(d)
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.wantPrompt(reviewBrowse)
	r.at(reviewGap, "y")
	wantActions(t, src)
}

func TestActionKeysWaitForDwell(t *testing.T) {
	for _, k := range []string{"a", "c", "x", "o"} {
		src := &fakeReviews{diff: exec.Command("true")}
		src.set(rvDetail(1))
		r := newReviewRun(t, src, ReviewOptions{}, 200, 60)
		r.at(100*time.Millisecond, k)
		r.wantPrompt(reviewBrowse)
		r.wantStatus(noticeTooSoon)
		// The refusal restarted the dwell.
		r.at(200*time.Millisecond, k)
		r.wantPrompt(reviewBrowse)
		r.wantStatus(noticeTooSoon)
		wantActions(t, src)
		r.at(dwell, k)
		if k == "o" {
			wantActions(t, src, "DiffCmd R-1")
		} else if r.review().mode == reviewBrowse {
			t.Errorf("%s: no prompt after the dwell", k)
		}
	}

	// A detail that changes what is shown restarts the dwell.
	src := &fakeReviews{}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	src.edit(func(d *ReviewDetail) { d.DiffStat += "\n auth/token.go | 2 +-" })
	r.detailTick()
	r.at(100*time.Millisecond, "a")
	r.wantPrompt(reviewBrowse)
	r.wantStatus(noticeTooSoon)

	// No action before the detail is read.
	src.fail(nil, errors.New("git is slow"), nil)
	r = newReviewRun(t, src, ReviewOptions{}, 200, 60)
	r.at(reviewGap, "a")
	r.wantPrompt(reviewBrowse)
	r.wantStatus(noticeReviewReading)
}

func TestPromptCanceledWhenReviewChanges(t *testing.T) {
	changes := map[string]func(*fakeReviews){
		"updated": func(src *fakeReviews) {
			src.edit(func(d *ReviewDetail) { d.Review.UpdatedAt = d.Review.UpdatedAt.Add(time.Second) })
		},
		"left": func(src *fakeReviews) { src.set(rvDetail(2)) },
		"diff": func(src *fakeReviews) {
			src.edit(func(d *ReviewDetail) { d.DiffStat = " auth/other.go | 1 +" })
		},
		"assumption": func(src *fakeReviews) {
			src.edit(func(d *ReviewDetail) { d.Assumptions = []model.Decision{rvAssume("D-12", "q", "a")} })
		},
	}
	for name, change := range changes {
		for _, k := range []string{"a", "x", "c"} {
			src := &fakeReviews{}
			src.set(rvDetail(1), rvDetail(2))
			r := openReview(t, src, ReviewOptions{})
			r.at(0, k)
			if k == "c" {
				r.typeText("fix")
			}
			change(src)
			r.queueTick()
			r.detailTick()
			r.wantPrompt(reviewBrowse)
			r.wantStatus(noticeReviewChanged)
			confirm := "y"
			if k == "c" {
				confirm = "enter"
			}
			r.at(reviewGap, confirm)
			wantActions(t, src)
			r.wantStatus(noticeReviewChanged)
			if t.Failed() {
				t.Fatalf("%s, %s", name, k)
			}
		}
	}

	// The note survives a change: c on the same review offers it again.
	src := &fakeReviews{}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "c")
	r.typeText("add a test")
	src.edit(func(d *ReviewDetail) { d.Review.UpdatedAt = d.Review.UpdatedAt.Add(time.Second) })
	r.queueTick()
	r.at(reviewGap, "c") // the first key after the change is refused
	r.wantPrompt(reviewBrowse)
	r.at(reviewGap, "c")
	r.wantPrompt(reviewNote)
	r.wantIn("> add a test_")
}

func TestRequestChangesSendsNote(t *testing.T) {
	src := &fakeReviews{line: "R-1: changes requested; M-12 delivered (send_keys)"}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "c")
	r.wantPrompt(reviewNote)
	r.typeText("please add a test for the 401 path")
	r.wantIn("> please add a test for the 401 path_")
	r.at(reviewGap, "enter")
	wantActions(t, src, "RequestChanges R-1 please add a test for the 401 path")
	r.wantIn("R-1: changes requested; M-12 delivered (send_keys)")

	// A blank note sends nothing.
	src = &fakeReviews{}
	src.set(rvDetail(1))
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "c")
	r.at(reviewGap, "enter")
	r.typeText("   ")
	r.at(reviewGap, "enter")
	wantActions(t, src)
	r.wantPrompt(reviewNote)

	// Esc cancels and keeps nothing; text in the input is never a command.
	r.typeText("ayxq")
	r.at(reviewGap, "esc")
	r.wantPrompt(reviewBrowse)
	r.at(reviewGap, "c")
	r.wantIn("> _")
	wantActions(t, src)
	if r.quit || r.closed {
		t.Error("q typed into the note closed the view")
	}
}

func TestNoteInputCap(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "c")
	for i := range 700 {
		r.at(reviewTyping, string(rune('a'+i%26)))
	}
	r.at(reviewGap, "enter")
	got := src.actions()
	if len(got) != 1 {
		t.Fatalf("actions = %q, want one RequestChanges", got)
	}
	note := strings.TrimPrefix(got[0], "RequestChanges R-1 ")
	if n := utf8.RuneCountInString(note); n != model.MaxReviewNote {
		t.Errorf("note is %d runes, want %d", n, model.MaxReviewNote)
	}

	// A paste is cut the same way.
	src = &fakeReviews{}
	src.set(rvDetail(1))
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "c")
	r.paste(strings.Repeat("宽", 1000))
	r.at(reviewGap, "enter")
	if got := src.actions(); len(got) != 1 || utf8.RuneCountInString(strings.TrimPrefix(got[0], "RequestChanges R-1 ")) != model.MaxReviewNote {
		t.Errorf("pasted note not cut to %d runes: %d actions", model.MaxReviewNote, len(got))
	}
}

func TestNotePasteIsCleanedText(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "c")
	r.paste("fix\x1b[31m it\u202e\nand y\r\n")
	wantActions(t, src)
	r.wantPrompt(reviewNote)
	// Typed without bracketed paste: a burst of runes, then its line break.
	r.clk.advance(reviewTyping)
	r.feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("then q")})
	r.at(50*time.Millisecond, "enter")
	r.clk.advance(10 * time.Millisecond)
	r.feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ok")})
	wantActions(t, src)
	r.at(reviewGap, "enter")
	wantActions(t, src, "RequestChanges R-1 fix it and y then q ok")
}

func TestHeldEnterDoesNotSendNote(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "c")
	r.at(100*time.Millisecond, "enter", "enter", "enter")
	r.typeText("fix it")
	wantActions(t, src)
	r.clk.advance(reviewGap)
	r.at(0, "enter")
	r.at(300*time.Millisecond, "enter", "enter", "enter")
	wantActions(t, src, "RequestChanges R-1 fix it")

	// An Enter held from the start never sends, however the keyboard repeats.
	for _, first := range []time.Duration{400 * time.Millisecond, 500 * time.Millisecond, 660 * time.Millisecond} {
		src = &fakeReviews{}
		src.set(rvDetail(1))
		r = openReview(t, src, ReviewOptions{})
		r.at(0, "c")
		r.paste("fix it")
		r.clk.advance(100 * time.Millisecond)
		r.hold("enter", first) // pressed with the paste, then held
		wantActions(t, src)
		r.clk.advance(quietPeriod)
		r.feed(rvKey("enter"))
		wantActions(t, src, "RequestChanges R-1 fix it")
		// Its repeats after the send act on nothing.
		r.hold("enter", first)
		wantActions(t, src, "RequestChanges R-1 fix it")
	}
}

func TestDropNeedsConfirmation(t *testing.T) {
	src := &fakeReviews{line: "R-1 dropped; T-41 is dropped. Its agent is not stopped."}
	src.set(rvDetail(1), rvDetail(2))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "x")
	r.wantStatus(dropPrompt)
	r.at(100*time.Millisecond, "y") // too early
	wantActions(t, src)
	r.wantPrompt(reviewConfirmDrop)
	r.at(200*time.Millisecond, "y") // under the quiet period after the early y
	wantActions(t, src)
	r.at(reviewGap, "n")
	r.wantPrompt(reviewBrowse)
	wantActions(t, src)
	r.at(reviewGap, "x")
	r.at(reviewGap, "y")
	wantActions(t, src, "Drop R-1")
	r.wantIn("R-1 dropped; T-41 is dropped. Its agent is not stopped.")
	r.wantShown("R-2")
}

func TestReviewPasteIsNotACommand(t *testing.T) {
	for _, s := range []string{"a", "y", "x", "c", "o", "q", "ay", "]"} {
		src := &fakeReviews{diff: exec.Command("true")}
		src.set(rvDetail(1), rvDetail(2))
		r := openReview(t, src, ReviewOptions{})
		r.paste(s)
		r.wantPrompt(reviewBrowse)
		r.wantShown("R-1")
		if r.quit || r.closed {
			t.Errorf("pasted %q closed the view", s)
		}
		// The paste starts the quiet period: an a past the dwell does nothing.
		r.at(500*time.Millisecond, "a")
		r.wantPrompt(reviewBrowse)
		wantActions(t, src)
	}

	// A paste in a prompt cancels it and confirms nothing.
	for _, k := range []string{"a", "x"} {
		src := &fakeReviews{}
		src.set(rvDetail(1))
		r := openReview(t, src, ReviewOptions{})
		r.at(0, k)
		r.clk.advance(reviewGap)
		r.paste("y")
		r.wantPrompt(reviewBrowse)
		r.at(500*time.Millisecond, k) // past the dwell, inside the quiet the paste started
		r.wantPrompt(reviewBrowse)
		r.at(reviewTyping, "y")
		wantActions(t, src)
	}
}

func TestKeysIgnoredWhileActionRuns(t *testing.T) {
	src := &fakeReviews{line: "R-1 approved: merged"}
	src.set(rvDetail(1), rvDetail(2))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.clk.advance(reviewGap)
	confirm := r.step(rvKey("y")) // burstGap passes with no other key
	if confirm == nil {
		t.Fatal("y waits for nothing")
	}
	approve := r.step(confirm()) // Approve has not returned
	if approve == nil {
		t.Fatal("y started nothing")
	}
	r.wantStatus("working…")
	for _, k := range []string{"q", "esc", "ctrl+c", "a", "y", "x", "]", "c"} {
		r.clk.advance(reviewGap)
		if cmd := r.step(rvKey(k)); cmd != nil {
			t.Errorf("%q while the action runs returned a command", k)
		}
	}
	r.paste("y")
	r.wantShown("R-1")
	if !strings.HasPrefix(r.status(), "working…") || !strings.Contains(r.status(), "cannot close") {
		t.Errorf("status line = %q, want working and why it does not close", r.status())
	}
	wantActions(t, src) // Approve itself runs only now
	r.run(approve)
	wantActions(t, src, "Approve R-1")
	r.wantIn("R-1 approved: merged")
	if r.quit || r.closed {
		t.Error("a close key pressed while the action ran closed the view")
	}
}

func TestQuietAfterAction(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1), rvDetail(2))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.at(reviewGap, "y")
	wantActions(t, src, "Approve R-1")
	r.wantShown("R-2")
	// Keys under 2 s apart are ignored, each restarting the quiet.
	for _, k := range []string{"a", "x", "c", "j", "]", "y", "a"} {
		r.at(1500*time.Millisecond, k)
		r.wantPrompt(reviewBrowse)
		r.wantStatus(noticeTooSoon)
	}
	wantActions(t, src, "Approve R-1")
	r.at(answeredQuiet, "a")
	r.wantPrompt(reviewConfirmApprove)

	// q and Esc close within the quiet.
	for _, k := range []string{"q", "esc"} {
		closing := &fakeReviews{}
		closing.set(rvDetail(1), rvDetail(2))
		cr := openReview(t, closing, ReviewOptions{})
		cr.at(0, "x")
		cr.at(reviewGap, "y")
		cr.at(100*time.Millisecond, k)
		if !cr.quit {
			t.Errorf("%q in the quiet after an action did not close", k)
		}
	}

	// The quiet follows a refused action too.
	src = &fakeReviews{}
	src.set(rvDetail(1))
	src.fail(nil, nil, errors.New("the agent is still working; wait for it to stop"))
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.at(reviewGap, "y")
	r.at(time.Second, "a")
	r.wantPrompt(reviewBrowse)
}

func TestActionErrorShown(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1))
	src.fail(nil, nil, errors.New("nothing to merge: curia/T-41-fix-the-auth-refresh has no commits beyond main"))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.at(reviewGap, "y")
	r.wantIn("approve failed: nothing to merge: curia/T-41-fix-the-auth-refresh has no commits beyond main")
	r.wantShown("R-1")
	r.at(reviewGap, "x")
	r.at(reviewGap, "y")
	r.wantIn("drop failed: nothing to merge")
	r.at(reviewGap, "c")
	r.typeText("add a test")
	r.at(reviewGap, "enter")
	r.wantIn("request changes failed: nothing to merge")
	wantActions(t, src, "Approve R-1", "Drop R-1", "RequestChanges R-1 add a test")
	// The note that was not sent is offered again.
	r.at(reviewGap, "c")
	r.wantIn("> add a test_")
}

func TestOpenDiffRunsSourceCommand(t *testing.T) {
	tool := exec.Command("git", "diff")
	src := &fakeReviews{diff: tool}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "o")
	wantActions(t, src, "DiffCmd R-1")
	if len(r.tools) != 1 || r.tools[0] != tool {
		t.Errorf("ran %v, want the source's command once", r.tools)
	}
	// A q that closed the tool, pressed twice, does not close the view.
	r.at(100*time.Millisecond, "q")
	if r.quit {
		t.Error("q right after the diff tool closed the view")
	}

	r.toolErr = errors.New("exit status 1")
	r.at(reviewGap, "o")
	r.wantStatus("diff tool failed: exit status 1")

	src = &fakeReviews{diffErr: errors.New("lazygit is not on PATH")}
	src.set(rvDetail(1))
	r = openReview(t, src, ReviewOptions{})
	r.at(0, "o")
	r.wantStatus("diff failed: lazygit is not on PATH")
	if len(r.tools) != 0 {
		t.Error("a tool ran after DiffCmd failed")
	}
}

func TestReviewEmbeddedCloseSendsMessage(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		src := &fakeReviews{}
		src.set(rvDetail(1))
		r := openReview(t, src, ReviewOptions{Embedded: true})
		r.at(reviewGap, k)
		if !r.closed || r.quit {
			t.Errorf("%q embedded: closed %v quit %v, want ReviewClosedMsg", k, r.closed, r.quit)
		}
		r = openReview(t, src, ReviewOptions{})
		r.at(reviewGap, k)
		if r.closed || !r.quit {
			t.Errorf("%q alone: closed %v quit %v, want quit", k, r.closed, r.quit)
		}
	}
	src := &fakeReviews{}
	r := openReview(t, src, ReviewOptions{Embedded: true})
	r.wantStatus("q close")
}

// rvHostile holds escape sequences, bidi overrides, control characters and
// wide characters in every drawn field.
func rvHostile() ReviewDetail {
	evil := "\x1b[31mred\x1b]0;pwned\a\u202e\u2066 \u009b31m 数据库迁移宽字符\t"
	d := rvDetail(1)
	d.Review.ID += evil
	d.Review.Summary = strings.Repeat("sum "+evil+"\n", 500)
	d.Review.TestsDetail = "1 failed " + evil
	d.Review.Tests = model.TestsFailed
	d.Task.ID += evil
	d.Task.Title = hostile + strings.Repeat("宽", 60)
	d.Task.Branch = "curia/T-41" + evil + strings.Repeat("x", 80)
	d.Task.BaseBranch = "main" + evil
	d.Task.Repo = "/src/" + evil
	d.DiffStat = " file" + evil + ".go | 3 ++-\n\tindented\x1b[2J\n" + strings.Repeat("宽 | 1 +\n", 300)
	d.DiffNote = "branch " + evil + " does not exist"
	d.Assumptions = []model.Decision{rvAssume("D-9"+evil, hostile+strings.Repeat("问", 120), evil+strings.Repeat("y", 200))}
	d.Blocking = []model.Decision{rvBlocking("D-11", hostile)}
	d.Decided[0].Question, d.Decided[0].Answer = hostile, evil
	return d
}

// rvClean fails when view holds a control, format or bidi character or a
// line wider than width.
func rvClean(t *testing.T, name, view string, width int) {
	t.Helper()
	checkClean(t, name, view, width)
	wantClean(t, view)
}

func TestReviewTextCannotDrawEscapes(t *testing.T) {
	for _, width := range []int{40, 80, 200} {
		src := &fakeReviews{line: "R-1 approved \x1b[31m\u202e: merged " + strings.Repeat("宽", 100)}
		d := rvHostile()
		src.set(d)
		r := newReviewRun(t, src, ReviewOptions{}, width, 30)
		r.clk.advance(reviewGap)
		name := func(s string) string { return fmt.Sprintf("width %d %s", width, s) }
		rvClean(t, name("detail"), r.view(), width)
		for range 30 {
			r.at(reviewGap, "pgdown")
		}
		rvClean(t, name("scrolled"), r.view(), width)
		r.at(reviewGap, "a")
		rvClean(t, name("approve prompt"), r.view(), width)
		r.at(reviewGap, "esc", "x")
		rvClean(t, name("drop prompt"), r.view(), width)
		r.at(reviewGap, "esc", "c")
		r.paste("note " + hostile + strings.Repeat("宽", 50))
		rvClean(t, name("note"), r.view(), width)
		r.at(reviewGap, "esc")
		src.fail(errors.New("db \x1b[31m\u202e locked"), errors.New("git \x1b]0;x\a died"), nil)
		r.queueTick()
		rvClean(t, name("queue error"), r.view(), width)
		r.detailTick()
		rvClean(t, name("detail error"), r.view(), width)
		src.fail(nil, nil, nil)
		r.at(reviewGap, "a")
		r.at(reviewGap, "y")
		rvClean(t, name("outcome"), r.view(), width)
		src.fail(nil, nil, errors.New("merge \x1b[31mfailed\u202e"+strings.Repeat("宽", 200)))
		src.set(rvHostile())
		r.clk.advance(reviewGap)
		r.queueTick()
		r.at(reviewGap, "x")
		r.at(reviewGap, "y")
		rvClean(t, name("error"), r.view(), width)
	}
	src := &fakeReviews{}
	r := newReviewRun(t, src, ReviewOptions{TaskID: "T-42\x1b[31m\u202e"}, 40, 10)
	rvClean(t, "no review for a hostile task id", r.view(), 40)
}

func TestReviewNarrowWidth(t *testing.T) {
	d := rvDetail(1)
	d.Assumptions = []model.Decision{rvAssume("D-4", "Keep the old token format for every client?", "keep it")}
	d.Review.Summary = "A long summary line that has to wrap at forty columns, with 数据库迁移宽字符 in it."
	for _, width := range []int{40, 80, 200} {
		src := &fakeReviews{line: "R-1 approved: merged curia/T-41-fix-the-auth-refresh into main (abc1234)."}
		src.set(d, rvDetail(2))
		r := newReviewRun(t, src, ReviewOptions{}, width, 24)
		r.clk.advance(reviewGap)
		checkClean(t, fmt.Sprintf("width %d", width), r.view(), width)
		r.wantIn("Summary (the agent's):")
		r.at(0, "a")
		checkClean(t, fmt.Sprintf("width %d prompt", width), r.view(), width)
		r.wantIn("y confirms")
		r.at(reviewGap, "y")
		checkClean(t, fmt.Sprintf("width %d outcome", width), r.view(), width)
		r.at(reviewGap, "c")
		r.typeText("please add a test")
		checkClean(t, fmt.Sprintf("width %d note", width), r.view(), width)
		r.wantIn("add a test_")
		if n := len(strings.Split(r.view(), "\n")); n != 24 {
			t.Errorf("width %d: %d lines, want the height 24", width, n)
		}
	}
}

func TestReviewSourceErrorKeepsLastDetail(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	src.fail(nil, errors.New("git diff timed out"), nil)
	r.detailTick()
	r.wantIn("Fixed the token refresh race in T-41.")
	r.wantStatus("reading R-1 failed: git diff timed out")
	// The last good detail still acts.
	r.at(reviewGap, "a")
	r.wantPrompt(reviewConfirmApprove)
	r.at(reviewGap, "esc")

	src.fail(errors.New("database is locked"), nil, nil)
	r.queueTick()
	r.wantShown("R-1")
	r.wantIn("Fixed the token refresh race in T-41.")
	r.wantStatus("refresh failed: database is locked")
}

func TestReviewRefreshSchedule(t *testing.T) {
	src := &fakeReviews{}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	if !slices.Contains(r.ticks, refreshEvery) || !slices.Contains(r.ticks, reviewDetailEvery) {
		t.Errorf("ticks = %v, want the queue and detail chains", r.ticks)
	}
	// A queue refresh with no change reads no detail; a new UpdatedAt does.
	r.queueTick()
	src.edit(func(d *ReviewDetail) { d.Review.UpdatedAt = d.Review.UpdatedAt.Add(time.Second) })
	r.queueTick()
	r.detailTick()
	want := []string{"Detail R-1", "Detail R-1", "Detail R-1"}
	if got := src.recorded(); !slices.Equal(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// rvHead1 and rvHead2 are the branch heads of the changed-branch test.
const (
	rvHead1 = "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rvHead2 = "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestApproveRefusedWhenBranchChanged(t *testing.T) {
	src := &fakeReviews{}
	d := rvDetail(1)
	d.Head = rvHead1
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.wantIn("Diff against main, branch at 1111111:")

	// A commit lands on the branch after the detail was read: the view
	// still shows the old head when y confirms.
	r.at(0, "a")
	src.edit(func(d *ReviewDetail) { d.Head = rvHead2 })
	r.at(reviewGap, "y")
	if got := src.approvedHeads(); !slices.Equal(got, []string{rvHead1}) {
		t.Errorf("Approve heads = %q, want the head on screen %q", got, rvHead1)
	}
	r.wantIn("approve failed: " + rvBranchChanged)
	r.wantShown("R-1")
	// The view read the detail again at once and shows the new state.
	r.wantIn("Diff against main, branch at 2222222:")
	r.wantNotIn("branch at 1111111")

	// Approving again, now that the new head has been seen, approves it.
	r.at(answeredQuiet+reviewGap, "a")
	r.at(reviewGap, "y")
	if got := src.approvedHeads(); !slices.Equal(got, []string{rvHead1, rvHead2}) {
		t.Errorf("Approve heads = %q, want %q", got, []string{rvHead1, rvHead2})
	}
	wantActions(t, src, "Approve R-1", "Approve R-1")
	if got := r.review().Result(); len(got) != 2 || got[0] != "approve failed: "+rvBranchChanged {
		t.Errorf("Result() = %q, want the refusal then the approval", got)
	}
}

func TestPromptCanceledWhenOnlyHeadChanges(t *testing.T) {
	src := &fakeReviews{}
	d := rvDetail(1)
	d.Head = rvHead1
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.wantPrompt(reviewConfirmApprove)
	src.edit(func(d *ReviewDetail) { d.Head = rvHead2 }) // same diff stat, new commit
	r.detailTick()
	r.wantPrompt(reviewBrowse)
	r.wantStatus(noticeReviewChanged)
	r.at(reviewGap, "y")
	wantActions(t, src)
}

func TestReviewResultListsEveryOutcome(t *testing.T) {
	src := &fakeReviews{line: "done"}
	src.set(rvDetail(1), rvDetail(2))
	r := openReview(t, src, ReviewOptions{})
	if got := r.review().Result(); len(got) != 0 {
		t.Errorf("Result() before any action = %q, want none", got)
	}
	r.at(0, "x")
	r.at(reviewGap, "y")
	src.fail(nil, nil, errors.New("refused"))
	r.at(answeredQuiet+reviewGap, "x")
	r.at(reviewGap, "y")
	if got, want := r.review().Result(), []string{"done", "drop failed: refused"}; !slices.Equal(got, want) {
		t.Errorf("Result() = %q, want %q", got, want)
	}
}

// TestConfirmRefusesYAndEnterTogether: a paste the terminal does not
// bracket delivers "y" and a line break as two keys 1 ms apart. The "y" is
// refused (the burst rule), whether the Enter comes before the y's wait for
// burstGap ends or the y's wait message comes first only in the test; the
// prompt stays open with its wait restarted, and nothing is approved or
// dropped. A "y" alone after the quiet period then confirms.
func TestConfirmRefusesYAndEnterTogether(t *testing.T) {
	for _, k := range []string{"a", "x"} {
		src := &fakeReviews{line: "done"}
		src.set(rvDetail(1))
		r := openReview(t, src, ReviewOptions{})
		r.at(0, k)
		r.clk.advance(reviewGap)
		wait := r.step(rvKey("y"))
		r.clk.advance(time.Millisecond)
		r.feed(rvKey("enter"))
		r.run(wait) // the y's wait ends after the Enter came: it acts on nothing
		wantActions(t, src)
		if n := r.review().notice; n != noticeTooSoon {
			t.Errorf("%s: notice = %q, want %q", k, n, noticeTooSoon)
		}
		if mode := r.review().mode; mode == reviewBrowse {
			t.Errorf("%s: the prompt closed", k)
		}
		r.clk.advance(time.Millisecond)
		r.feed(rvKey("y")) // still the same paste: too soon after the Enter
		wantActions(t, src)
		r.at(quietPeriod, "y")
		want := "Approve R-1"
		if k == "x" {
			want = "Drop R-1"
		}
		wantActions(t, src, want)
	}
}

// TestConfirmAcceptsYAlone: a "y" on its own, after the dwell and with no
// key within burstGap of it, confirms once burstGap has passed.
func TestConfirmAcceptsYAlone(t *testing.T) {
	src := &fakeReviews{line: "R-1 approved: merged"}
	src.set(rvDetail(1))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.clk.advance(reviewGap)
	wait := r.step(rvKey("y"))
	wantActions(t, src) // nothing before burstGap has passed
	if !slices.Contains(r.ticks, burstGap) {
		t.Errorf("ticks = %v, want a wait of burstGap", r.ticks)
	}
	r.run(wait)
	wantActions(t, src, "Approve R-1")
}

// TestConfirmActedBeforeLaterKey: what is intended for a "y" followed 200
// ms later by an unrelated key: the confirmation has already acted on the
// "y" (200 ms is past burstGap: two presses, not one paste), and the later
// key is ignored while the action runs. The same holds when the y's wait
// message is late and the key comes first.
func TestConfirmActedBeforeLaterKey(t *testing.T) {
	src := &fakeReviews{line: "R-1 approved: merged"}
	src.set(rvDetail(1), rvDetail(2))
	r := openReview(t, src, ReviewOptions{})
	r.at(0, "a")
	r.at(reviewGap, "y") // its wait ends, with no other key, before the next key
	r.at(200*time.Millisecond, "x")
	wantActions(t, src, "Approve R-1")
	r.wantPrompt(reviewBrowse)

	late := &fakeReviews{line: "R-1 approved: merged"}
	late.set(rvDetail(1), rvDetail(2))
	l := openReview(t, late, ReviewOptions{})
	l.at(0, "a")
	l.clk.advance(reviewGap)
	_ = l.step(rvKey("y")) // its wait message never comes
	l.at(200*time.Millisecond, "x")
	wantActions(t, late, "Approve R-1")
	if mode := l.review().mode; mode == reviewConfirmDrop {
		t.Error("the later x opened the drop prompt")
	}
}

// TestDiffToolReturnHoldsCloseKeys: after the diff tool returns, q and Esc
// (the keys that closed the pager, pressed again, or typed for the pager
// after a tool that came back at once) do not close the view during the
// quiet period, and each restarts it; after a pause q closes. Alone and
// embedded in the dashboard.
func TestDiffToolReturnHoldsCloseKeys(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		src := &fakeReviews{diff: exec.Command("git", "diff")}
		src.set(rvDetail(1))
		r := openReview(t, src, ReviewOptions{Embedded: embedded})
		r.at(0, "o") // the tool returns at once
		for _, k := range []string{"q", "esc", "q"} {
			r.at(quietPeriod-50*time.Millisecond, k)
			if r.quit || r.closed {
				t.Fatalf("embedded %v: %q %v after the previous key closed the view", embedded, k, quietPeriod-50*time.Millisecond)
			}
		}
		r.at(quietPeriod+time.Millisecond, "q")
		if !r.quit && !r.closed {
			t.Errorf("embedded %v: q after the quiet period did not close the view", embedded)
		}
	}
}

// TestDecidedHeadingUnderAssumption: with an unconfirmed assumption and no
// decided ticket, the heading says the decided ones are none, not the
// decisions (live check defect 8).
func TestDecidedHeadingUnderAssumption(t *testing.T) {
	d := rvDetail(1)
	d.Decided = nil
	d.Assumptions = []model.Decision{{ID: "D-1", Question: "Footer wording?", Assumption: "Open daily.", Mode: model.ModeAssume, Status: model.StatusOpen}}
	src := &fakeReviews{}
	src.set(d)
	r := openReview(t, src, ReviewOptions{})
	r.wantIn("D-1 unconfirmed assumption")
	r.wantIn("Decided tickets on this task: none")
	if strings.Contains(r.view(), "Decisions on this task: none") {
		t.Errorf("the old heading is shown:\n%s", r.view())
	}
}

package ui_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/ui"
)

// decideNow is the fixed clock of the decide view tests.
var decideNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// answerCall is one call of fakeDecideSource.Answer.
type answerCall struct {
	id string
	a  model.Answer
}

// fakeDecideSource serves tickets and records answers. An answered ticket
// leaves the queue, as it does in the store.
type fakeDecideSource struct {
	mu        sync.Mutex
	tickets   []model.Ticket
	err       error // returned by Tickets when set
	answerErr error // returned by Answer when set
	note      string
	answers   []answerCall
	reads     int
}

func (f *fakeDecideSource) Tickets(context.Context) ([]model.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	return slices.Clone(f.tickets), nil
}

func (f *fakeDecideSource) Answer(_ context.Context, id string, a model.Answer) (model.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, answerCall{id, a})
	if f.answerErr != nil {
		return model.Outcome{}, f.answerErr
	}
	i := slices.IndexFunc(f.tickets, func(t model.Ticket) bool { return t.ID == id })
	if i < 0 {
		return model.Outcome{}, errors.New("not found")
	}
	d := f.tickets[i].Decision
	f.tickets = slices.Delete(f.tickets, i, i+1)
	d.AnswerKey, d.Answer = a.Key, a.Text
	return model.Outcome{Decision: d, Note: f.note}, nil
}

func (f *fakeDecideSource) set(ts ...model.Ticket) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickets = ts
}

func (f *fakeDecideSource) calls() []answerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.answers)
}

// ticket returns open ticket D-n of session "S1" in repo /src/curia with
// options A, B and C, B recommended.
func ticket(n int, mode model.Mode, rev model.Reversibility) model.Ticket {
	id := "D-" + strconv.Itoa(n)
	t := model.Ticket{Decision: model.Decision{
		N: int64(n), ID: id, SessionID: "S1", Repo: "/src/curia",
		Question: "Where should " + id + " keep the job queue?",
		Context:  "The worker restarts often.",
		Options: []model.Option{
			{Key: "A", Label: "Postgres table", Tradeoffs: "one more table to migrate"},
			{Key: "B", Label: "Redis queue", Tradeoffs: "a new service to run"},
			{Key: "C", Label: "Files on disk", Tradeoffs: "no locking across hosts"},
		},
		Recommendation: "B",
		Rationale:      "Redis is already deployed for caching.",
		Reversibility:  rev,
		Mode:           mode,
		Scope:          model.ScopeTask,
		Status:         model.StatusOpen,
		CreatedAt:      decideNow.Add(-time.Duration(10-n) * time.Minute),
	}}
	if mode == model.ModeAssume {
		t.Assumption = "Redis queue"
	}
	return t
}

func assumeTickets(n int) []model.Ticket {
	var ts []model.Ticket
	for i := 1; i <= n; i++ {
		ts = append(ts, ticket(i, model.ModeAssume, model.ReversCheap))
	}
	return ts
}

var popup = ui.DecideOptions{Via: model.AnsweredPopup}

// newDecide returns a decide view over src after its first refresh.
func newDecide(t *testing.T, src *fakeDecideSource, opts ui.DecideOptions) tea.Model {
	t.Helper()
	m := ui.NewDecide(src, func() time.Time { return decideNow }, opts)
	return reload(t, m)
}

// reload runs a refresh like the one Init starts and feeds its result back;
// the refresh must schedule the next one.
func reload(t *testing.T, m tea.Model) tea.Model {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned no command")
	}
	m, next := m.Update(cmd())
	if next == nil {
		t.Fatal("refresh did not schedule the next one")
	}
	return m
}

func dkey(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

func paste(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true}
}

// send feeds each message in turn and runs the commands it returns (the
// answer and the refresh after it) to completion.
func send(t *testing.T, m tea.Model, msgs ...tea.Msg) tea.Model {
	t.Helper()
	for _, msg := range msgs {
		var cmd tea.Cmd
		m, cmd = m.Update(msg)
		m = drive(t, m, cmd)
	}
	return m
}

func drive(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	for steps := 0; cmd != nil; steps++ {
		if steps > 10 {
			t.Fatal("commands did not settle")
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				m = drive(t, m, c)
			}
			return m
		case tea.QuitMsg, ui.DecideClosedMsg:
			return m
		default:
			m, cmd = m.Update(msg)
		}
	}
	return m
}

// dpress sends keys (see dkey) through send.
func dpress(t *testing.T, m tea.Model, keys ...string) tea.Model {
	t.Helper()
	for _, k := range keys {
		m = send(t, m, dkey(k))
	}
	return m
}

func lastLine(m tea.Model) string {
	lines := strings.Split(m.View(), "\n")
	return lines[len(lines)-1]
}

func wantCalls(t *testing.T, src *fakeDecideSource, want ...answerCall) {
	t.Helper()
	if got := src.calls(); !slices.Equal(got, want) {
		t.Errorf("Answer calls = %+v, want %+v", got, want)
	}
}

func TestDecideListsTicketsInSourceOrder(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap), ticket(1, model.ModeBlocking, model.ReversCostly),
		ticket(2, model.ModeBlocking, model.ReversCheap))
	m := newDecide(t, src, popup)
	lines := strings.Split(m.View(), "\n")
	if lines[0] != "3 open · 2 blocking" {
		t.Errorf("header = %q, want %q", lines[0], "3 open · 2 blocking")
	}
	for i, want := range []string{"D-3 a", "D-1 B", "D-2 B"} {
		if !strings.Contains(lines[1+i], want) {
			t.Errorf("queue line %d = %q, want it to hold %q", i, lines[1+i], want)
		}
	}
	if !strings.HasPrefix(lines[1], ">") || strings.HasPrefix(lines[2], ">") {
		t.Errorf("selection mark not on the first ticket only:\n%s", m.View())
	}
	for _, want := range []string{"9m", "curia", "Where should D-1 keep the job queue?"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("queue line %q lacks %q", lines[2], want)
		}
	}
}

func TestDecideShowsSelectedTicket(t *testing.T) {
	tk := ticket(1, model.ModeBlocking, model.ReversCostly)
	tk.Scope = model.ScopeRepo
	tk.CacheExpiresAt = decideNow.Add(12*time.Minute + 30*time.Second)
	tk.CacheRebuildUSD = 1.68
	src := &fakeDecideSource{}
	src.set(tk, ticket(2, model.ModeBlocking, model.ReversCheap))
	view := newDecide(t, src, popup).View()
	for _, want := range []string{
		"Where should D-1 keep the job queue?",
		"answer within 12m to keep cache warm (rebuild ≈ $1.68)",
		"costly · blocking · scope repo",
		"The worker restarts often.",
		"1  A  Postgres table", "2  B  Redis queue", "3  C  Files on disk",
		"one more table to migrate", "a new service to run",
		"Redis is already deployed for caching.",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "(recommended)") != strings.Contains(l, "Redis queue") {
			t.Errorf("line %q: (recommended) must mark exactly the Redis queue option", l)
		}
	}
	if strings.Contains(view, "assumes:") {
		t.Errorf("blocking ticket shows an assumption:\n%s", view)
	}
}

func TestDecideShowsAssumptionAndDiffers(t *testing.T) {
	tk := ticket(3, model.ModeAssume, model.ReversCheap)
	tk.DiffersFrom, tk.DiffersWhy = "D-12", "this repo has no Redis"
	src := &fakeDecideSource{}
	src.set(tk)
	view := newDecide(t, src, popup).View()
	for _, want := range []string{"assumes: Redis queue", "differs from D-12: this repo has no Redis", "cheap · assume · scope task"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
}

func TestNumberKeyAnswers(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	dpress(t, m, "2")
	wantCalls(t, src, answerCall{"D-3", model.Answer{Key: "B", Via: model.AnsweredPopup}})

	src = &fakeDecideSource{}
	src.set(ticket(1, model.ModeBlocking, model.ReversCostly))
	m = newDecide(t, src, ui.DecideOptions{Via: model.AnsweredSSH})
	dpress(t, m, "2", "enter")
	wantCalls(t, src, answerCall{"D-1", model.Answer{Key: "B", Via: model.AnsweredSSH}})
}

func TestNumberBeyondOptionsIgnored(t *testing.T) {
	tk := ticket(3, model.ModeAssume, model.ReversCheap)
	tk.Options = tk.Options[:2]
	src := &fakeDecideSource{}
	src.set(tk)
	m := newDecide(t, src, popup)
	dpress(t, m, "4", "3")
	wantCalls(t, src)
}

func TestEnterAcceptsRecommendation(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	dpress(t, newDecide(t, src, popup), "enter")
	wantCalls(t, src, answerCall{"D-3", model.Answer{Key: "B", Via: model.AnsweredPopup}})
}

func TestFreeTextAnswer(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap), ticket(4, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	m = dpress(t, m, "w", "use the", " ", "queue")
	if s := lastLine(m); !strings.Contains(s, "use the queue") {
		t.Errorf("status line %q does not show the text being typed", s)
	}
	m = dpress(t, m, "enter")
	wantCalls(t, src, answerCall{"D-3", model.Answer{Text: "use the queue", Via: model.AnsweredPopup}})

	m = dpress(t, m, "j", "k") // the queue changed under the answer; move to read it
	dpress(t, m, "w", "never mind", "esc", "1")
	if got := src.calls(); len(got) != 2 || got[1].a.Key != "A" {
		t.Errorf("after Esc, calls = %+v, want only the earlier answer and option A", got)
	}
}

func TestFreeTextBackspaceAndEmptyEnter(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	m = dpress(t, m, "w", "enter") // nothing typed: nothing to send, the input stays open
	wantCalls(t, src)
	dpress(t, m, "nox", "backspace", "enter")
	wantCalls(t, src, answerCall{"D-3", model.Answer{Text: "no", Via: model.AnsweredPopup}})
}

func TestStagedReasoningAndScopeTravel(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	m = dpress(t, m, "r", "because", "enter", "s", "s")
	view := m.View()
	for _, want := range []string{"staged reasoning: because", "staged scope: repo"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	dpress(t, m, "1")
	wantCalls(t, src, answerCall{"D-3", model.Answer{Key: "A", Reasoning: "because", Scope: model.ScopeRepo, Via: model.AnsweredPopup}})
}

func TestScopeCycles(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	dpress(t, m, "s", "s", "s", "s", "1") // task, repo, global, back to the ticket's own
	wantCalls(t, src, answerCall{"D-3", model.Answer{Key: "A", Via: model.AnsweredPopup}})
}

func TestMovingDropsStaged(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(1, model.ModeBlocking, model.ReversCostly), ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	m = dpress(t, m, "r", "why not", "enter", "s", "j")
	if strings.Contains(m.View(), "staged") {
		t.Errorf("staged reasoning or scope still shown after moving:\n%s", m.View())
	}
	dpress(t, m, "1")
	wantCalls(t, src, answerCall{"D-3", model.Answer{Key: "A", Via: model.AnsweredPopup}})
}

func TestIrreversibleNeedsConfirmation(t *testing.T) {
	tk := ticket(9, model.ModeBlocking, model.ReversIrreversible)
	src := &fakeDecideSource{}
	src.set(tk, tk) // the second copy stays queued after the first answer
	m := newDecide(t, src, popup)
	m = dpress(t, m, "2", "enter")
	wantCalls(t, src)
	if s := lastLine(m); s != "answer D-9 with B? y sends, any other key cancels" {
		t.Errorf("status line = %q, want the confirmation", s)
	}
	m = dpress(t, m, "y")
	wantCalls(t, src, answerCall{"D-9", model.Answer{Key: "B", Via: model.AnsweredPopup}})

	m = dpress(t, m, "1", "enter", "n")
	if got := src.calls(); len(got) != 1 {
		t.Errorf("calls = %+v after n, want only the confirmed one", got)
	}
	if s := lastLine(m); !strings.Contains(s, "canceled") {
		t.Errorf("status line = %q, want the cancel shown", s)
	}
}

func TestIrreversibleFreeTextNeedsConfirmation(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(9, model.ModeAssume, model.ReversIrreversible))
	m := newDecide(t, src, popup)
	m = dpress(t, m, "w", "keep it", "enter")
	wantCalls(t, src)
	if s := lastLine(m); !strings.HasPrefix(s, "answer D-9 with") || !strings.Contains(s, "keep it") {
		t.Errorf("status line = %q, want the confirmation", s)
	}
	dpress(t, m, "y")
	wantCalls(t, src, answerCall{"D-9", model.Answer{Text: "keep it", Via: model.AnsweredPopup}})
}

func TestBlockingAnswerPromptsForReasoning(t *testing.T) {
	tk := ticket(1, model.ModeBlocking, model.ReversCostly)
	src := &fakeDecideSource{}
	src.set(tk, tk, tk)
	m := newDecide(t, src, popup)
	m = dpress(t, m, "1")
	wantCalls(t, src)
	if s := lastLine(m); !strings.Contains(s, "reasoning for A") {
		t.Errorf("status line = %q, want the reasoning input", s)
	}
	m = dpress(t, m, "because", "enter")
	want := []answerCall{{"D-1", model.Answer{Key: "A", Reasoning: "because", Via: model.AnsweredPopup}}}
	wantCalls(t, src, want...)

	m = dpress(t, m, "1", "enter")
	want = append(want, answerCall{"D-1", model.Answer{Key: "A", Via: model.AnsweredPopup}})
	wantCalls(t, src, want...)

	m = dpress(t, m, "1", "esc")
	wantCalls(t, src, want...)
	if s := lastLine(m); !strings.Contains(s, "canceled") {
		t.Errorf("status line = %q, want the cancel shown", s)
	}

	// Reasoning staged first: the answer is sent at once.
	dpress(t, m, "r", "staged", "enter", "3")
	want = append(want, answerCall{"D-1", model.Answer{Key: "C", Reasoning: "staged", Via: model.AnsweredPopup}})
	wantCalls(t, src, want...)
}

func TestRefreshNeverRedirectsAnAnswer(t *testing.T) {
	src := &fakeDecideSource{}
	ts := assumeTickets(3)
	src.set(ts...)
	m := newDecide(t, src, popup)
	m = dpress(t, m, "j") // D-2
	src.set(ts[0], ts[2])
	m = reload(t, m)
	if !strings.Contains(strings.Split(m.View(), "\n")[2], "> D-3") {
		t.Fatalf("cursor did not move to D-3:\n%s", m.View())
	}
	m = dpress(t, m, "1")
	wantCalls(t, src)
	if s := lastLine(m); s != "queue changed; press again" {
		t.Errorf("status line = %q, want the notice", s)
	}
	dpress(t, m, "1")
	wantCalls(t, src, answerCall{"D-3", model.Answer{Key: "A", Via: model.AnsweredPopup}})
}

func TestRefreshNeverRedirectsOtherAnswerKeys(t *testing.T) {
	for _, keys := range [][]string{{"enter"}, {"w"}} {
		src := &fakeDecideSource{}
		ts := assumeTickets(2)
		src.set(ts...)
		m := newDecide(t, src, popup)
		src.set(ts[1])
		m = reload(t, m)
		m = dpress(t, m, keys...)
		wantCalls(t, src)
		if s := lastLine(m); s != "queue changed; press again" {
			t.Errorf("%v: status line = %q, want the notice", keys, s)
		}
	}
}

func TestRefreshReplacingTicketNeedsSecondPress(t *testing.T) {
	src := &fakeDecideSource{}
	tk := ticket(1, model.ModeAssume, model.ReversCheap)
	src.set(tk)
	m := newDecide(t, src, popup)
	changed := tk
	changed.Options = slices.Clone(tk.Options)
	changed.Options[0].Label = "Drop the queue"
	src.set(changed)
	m = reload(t, m)
	m = dpress(t, m, "1")
	wantCalls(t, src)
	if s := lastLine(m); s != "queue changed; press again" {
		t.Errorf("status line = %q, want the notice", s)
	}

	// A ticket appearing in an empty queue is new on screen too.
	src = &fakeDecideSource{}
	m = newDecide(t, src, popup)
	src.set(tk)
	m = reload(t, m)
	m = dpress(t, m, "enter")
	wantCalls(t, src)
	dpress(t, m, "enter")
	wantCalls(t, src, answerCall{"D-1", model.Answer{Key: "B", Via: model.AnsweredPopup}})
}

// An answer waiting for reasoning or confirmation is never sent once a
// refresh has taken its ticket off the screen.
func TestRefreshNeverRedirectsPendingAnswer(t *testing.T) {
	for _, tc := range []struct {
		rev  model.Reversibility
		keys []string // the answer up to the last key
		last string
	}{
		{model.ReversCostly, []string{"1", "because"}, "enter"},
		{model.ReversIrreversible, []string{"1", "enter"}, "y"},
	} {
		src := &fakeDecideSource{}
		a, b := ticket(1, model.ModeBlocking, tc.rev), ticket(2, model.ModeBlocking, tc.rev)
		src.set(a, b)
		m := newDecide(t, src, popup)
		m = dpress(t, m, tc.keys...)
		src.set(b)
		m = reload(t, m)
		m = dpress(t, m, tc.last)
		wantCalls(t, src)
		if s := lastLine(m); s != "queue changed; press again" {
			t.Errorf("%s: status line = %q, want the notice", tc.rev, s)
		}
	}
}

func TestSelectionFollowsTicketAcrossReorder(t *testing.T) {
	src := &fakeDecideSource{}
	ts := assumeTickets(3)
	src.set(ts...)
	m := newDecide(t, src, popup)
	m = dpress(t, m, "j") // D-2
	src.set(ts[2], ts[0], ts[1])
	m = reload(t, m)
	if !strings.Contains(strings.Split(m.View(), "\n")[3], "> D-2") {
		t.Errorf("selection did not follow D-2:\n%s", m.View())
	}
	dpress(t, m, "1")
	wantCalls(t, src, answerCall{"D-2", model.Answer{Key: "A", Via: model.AnsweredPopup}})
}

func TestOutcomeShownAndQueueRefreshed(t *testing.T) {
	src := &fakeDecideSource{note: "session is working; the answer is queued"}
	src.set(assumeTickets(2)...)
	m := newDecide(t, src, popup)
	m = dpress(t, m, "2")
	if s := lastLine(m); s != "D-1 answered: B · session is working; the answer is queued" {
		t.Errorf("status line = %q, want the outcome note", s)
	}
	if src.reads != 2 {
		t.Errorf("Tickets read %d times, want 2 (the load and after the answer)", src.reads)
	}
	if lines := strings.Split(m.View(), "\n"); lines[0] != "1 open · 0 blocking" || strings.Contains(lines[1], "D-1") {
		t.Errorf("answered ticket still queued:\n%s", m.View())
	}

	// The next ticket was never on screen when the key was pressed.
	m = dpress(t, m, "1")
	wantCalls(t, src, answerCall{"D-1", model.Answer{Key: "B", Via: model.AnsweredPopup}})
	src.note = ""
	m = dpress(t, m, "1")
	if s := lastLine(m); s != "D-2 answered: A · delivered" {
		t.Errorf("status line = %q, want the delivered outcome", s)
	}
}

func TestAnswerErrorShown(t *testing.T) {
	src := &fakeDecideSource{answerErr: errors.New("store: decision is not open")}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	m, cmd := m.Update(dkey("1"))
	if cmd == nil {
		t.Fatal("answer key started no answer")
	}
	m, cmd = m.Update(cmd())
	if s := lastLine(m); !strings.Contains(s, "store: decision is not open") {
		t.Errorf("status line = %q, want the error", s)
	}
	m = drive(t, m, cmd) // the refresh after a failed answer keeps the error
	if s := lastLine(m); !strings.Contains(s, "store: decision is not open") {
		t.Errorf("status line = %q after the refresh, want the error", s)
	}
	if !strings.Contains(m.View(), "D-3 a") {
		t.Errorf("queue gone after the error:\n%s", m.View())
	}
}

func TestDecideSourceErrorKeepsTickets(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	src.err = errors.New("database is locked")
	m = reload(t, m)
	if !strings.Contains(m.View(), "D-3 a") || !strings.Contains(lastLine(m), "database is locked") {
		t.Errorf("want the queue kept and the error shown:\n%s", m.View())
	}
	dpress(t, m, "1") // the ticket on screen did not change
	wantCalls(t, src, answerCall{"D-3", model.Answer{Key: "A", Via: model.AnsweredPopup}})
}

func TestDecideTickRefreshes(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(assumeTickets(2)...)
	m := ui.NewDecide(src, func() time.Time { return decideNow }, popup)
	m2, tick := m.Update(m.Init()())
	if tick == nil {
		t.Fatal("refresh scheduled no tick")
	}
	src.set(assumeTickets(1)...)
	start := time.Now()
	m2, refresh := m2.Update(tick())
	if refresh == nil {
		t.Fatal("tick started no refresh")
	}
	m2, _ = m2.Update(refresh())
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Errorf("tick fired after %v, want about a second", d)
	}
	if strings.Contains(m2.View(), "D-2") {
		t.Errorf("tick did not re-read the source:\n%s", m2.View())
	}
}

func TestSessionFilter(t *testing.T) {
	a, b, c := ticket(1, model.ModeBlocking, model.ReversCheap), ticket(2, model.ModeBlocking, model.ReversCheap),
		ticket(3, model.ModeAssume, model.ReversCheap)
	b.SessionID = "S2"
	src := &fakeDecideSource{}
	src.set(a, b, c)
	view := newDecide(t, src, ui.DecideOptions{SessionID: "S2", Via: model.AnsweredPopup}).View()
	if !strings.HasPrefix(view, "1 open · 1 blocking\n") {
		t.Errorf("header wrong:\n%s", view)
	}
	if !strings.Contains(view, "D-2") || strings.Contains(view, "D-1") || strings.Contains(view, "D-3") {
		t.Errorf("view does not show only S2's ticket:\n%s", view)
	}
}

func TestEmbeddedCloseSendsMessage(t *testing.T) {
	for _, k := range []string{"q", "esc"} {
		src := &fakeDecideSource{}
		src.set(assumeTickets(1)...)
		m := newDecide(t, src, ui.DecideOptions{Embedded: true, Via: model.AnsweredPopup})
		_, cmd := m.Update(dkey(k))
		if cmd == nil {
			t.Fatalf("embedded %q: no command", k)
		}
		if _, ok := cmd().(ui.DecideClosedMsg); !ok {
			t.Errorf("embedded %q: command is not DecideClosedMsg", k)
		}
		m = newDecide(t, src, popup)
		_, cmd = m.Update(dkey(k))
		if cmd == nil {
			t.Fatalf("%q: no command", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%q: command is not tea.Quit", k)
		}
	}
	src := &fakeDecideSource{}
	m := newDecide(t, src, ui.DecideOptions{Embedded: true})
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("ctrl+c did nothing")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c did not quit")
	}
}

func TestEscInsideInputDoesNotClose(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(assumeTickets(1)...)
	m := newDecide(t, src, popup)
	for _, open := range []string{"w", "r"} {
		m = dpress(t, m, open)
		var cmd tea.Cmd
		m, cmd = m.Update(dkey("esc"))
		if cmd != nil {
			t.Errorf("%q then Esc returned a command", open)
		}
	}
}

func TestEmptyQueue(t *testing.T) {
	src := &fakeDecideSource{}
	m := newDecide(t, src, popup)
	view := m.View()
	if !strings.HasPrefix(view, "0 open · 0 blocking\n") || !strings.Contains(view, "no open decisions") {
		t.Errorf("empty view wrong:\n%s", view)
	}
	dpress(t, m, "1", "2", "enter", "w", "x", "enter", "y")
	wantCalls(t, src)
}

func TestTicketTextCannotDrawEscapes(t *testing.T) {
	const evil = "\x1b[2J\x1b]0;x\x07\u202e"
	tk := ticket(1, model.ModeAssume, model.ReversCheap)
	tk.Question = "Q" + evil + "\nQ2"
	tk.Context = "C" + evil + "\nC2"
	tk.Rationale = "R" + evil + "\nR2"
	tk.Assumption = "A" + evil + "\nA2"
	tk.Options[0].Label = "L" + evil + "\nL2"
	tk.Options[0].Key = "K" + evil
	tk.Options[0].Tradeoffs = "T" + evil + "\nT2"
	tk.Repo = "/src/re" + evil + "po"
	tk.DiffersFrom, tk.DiffersWhy = "D-5"+evil, "W"+evil+"\nW2"
	src := &fakeDecideSource{}
	src.set(tk)
	m := newDecide(t, src, popup)
	m = dpress(t, m, "r", "x"+evil, "enter")
	src.answerErr = errors.New("bad" + evil)
	m = dpress(t, m, "1")
	view := m.View()
	for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
		if strings.Contains(view, bad) {
			t.Errorf("view contains %q:\n%q", bad, view)
		}
	}
	lines := strings.Split(view, "\n")
	if !strings.Contains(lines[1], "D-1") || !strings.Contains(lines[1], "Q Q2") {
		t.Errorf("queue line = %q, want the question on one line", lines[1])
	}
	if lines[2] != "" {
		t.Errorf("line after the queue = %q, want the blank separator", lines[2])
	}
}

// checkFits checks that view fits size and keeps its header and the
// selected first queue line on screen.
func checkFits(t *testing.T, name, view string, size tea.WindowSizeMsg) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > size.Height {
		t.Errorf("%s: %d lines, want at most %d", name, len(lines), size.Height)
	}
	for _, l := range lines {
		if w := lipgloss.Width(l); w > size.Width {
			t.Errorf("%s: line is %d cells: %q", name, w, l)
		}
	}
	if lines[0] != "2 open · 1 blocking" {
		t.Errorf("%s: header = %q", name, lines[0])
	}
	if !strings.Contains(lines[1], "> D-1 B") {
		t.Errorf("%s: first queue line = %q", name, lines[1])
	}
}

func TestDecideNarrowWidth(t *testing.T) {
	long := ticket(1, model.ModeBlocking, model.ReversCostly)
	long.Question = strings.Repeat("Should the queue live in a much longer named place? ", 3)
	long.Context = strings.Repeat("context words go on and on here. ", 30)
	long.Rationale = strings.Repeat("because reasons ", 40)
	long.Repo = "/src/a-repository-with-a-really-long-name"
	long.Options[0].Label = strings.Repeat("label ", 13)
	long.Options[1].Tradeoffs = strings.Repeat("tradeoff ", 30)
	long.Options = append(long.Options, model.Option{Key: "superlon", Label: "x", Tradeoffs: strings.Repeat("y", 300)})
	long.CacheExpiresAt, long.CacheRebuildUSD = decideNow.Add(3*time.Minute), 12.5
	src := &fakeDecideSource{}
	src.set(long, ticket(2, model.ModeAssume, model.ReversCheap))
	for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 30}, {Width: 40, Height: 12}, {Width: 100, Height: 50}} {
		m := newDecide(t, src, popup)
		m, _ = m.Update(size)
		for _, state := range [][]string{nil, {"r", "typed text"}, {"esc", "1", "typed text"}} {
			m = dpress(t, m, state...)
			checkFits(t, fmt.Sprintf("%dx%d %q", size.Width, size.Height, state), m.View(), size)
		}
	}
	m := newDecide(t, src, popup)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	view := m.View()
	for _, want := range []string{"1  A  label", "2  B  Redis queue (recommended)", "3  C  Files on disk", "4  superlon  x"} {
		if !strings.Contains(view, want) {
			t.Errorf("at 40x30 the options are not readable; lacks %q:\n%s", want, view)
		}
	}
}

func TestPasteIsNotACommand(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(9, model.ModeAssume, model.ReversIrreversible), ticket(8, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	for _, p := range []string{"1", "2", "w", "j", "s", "r"} {
		m = send(t, m, paste(p))
	}
	if _, cmd := m.Update(paste("q")); cmd != nil {
		t.Error("pasted q returned a command")
	}
	m = send(t, m, paste("\r")) // a pasted line break is not Enter
	wantCalls(t, src)
	if v := m.View(); strings.Contains(v, "staged") || !strings.Contains(strings.Split(v, "\n")[1], "> D-9") {
		t.Errorf("a paste acted as a key:\n%s", v)
	}
	m = dpress(t, m, "1") // asks to confirm
	m = send(t, m, paste("y"))
	wantCalls(t, src)
	m = dpress(t, m, "w")
	m = send(t, m, paste("pasted text"))
	dpress(t, m, "enter", "y")
	wantCalls(t, src, answerCall{"D-9", model.Answer{Text: "pasted text", Via: model.AnsweredPopup}})
}

func TestInputCaps(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(ticket(3, model.ModeAssume, model.ReversCheap))
	m := newDecide(t, src, popup)
	m = dpress(t, m, "r")
	m = send(t, m, paste(strings.Repeat("r", 1200)))
	m = dpress(t, m, "enter", "w", strings.Repeat("x", 600))
	m = send(t, m, paste("more"))
	dpress(t, m, "enter")
	got := src.calls()
	if len(got) != 1 {
		t.Fatalf("calls = %d, want 1", len(got))
	}
	if n := utf8.RuneCountInString(got[0].a.Text); n != 500 {
		t.Errorf("answer text has %d runes, want 500", n)
	}
	if n := utf8.RuneCountInString(got[0].a.Reasoning); n != 1000 {
		t.Errorf("reasoning has %d runes, want 1000", n)
	}
}

func TestDecideFastTypedKeysEachCount(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(assumeTickets(2)...)
	m := newDecide(t, src, popup)
	dpress(t, m, "j2") // one message with both runes
	wantCalls(t, src, answerCall{"D-2", model.Answer{Key: "B", Via: model.AnsweredPopup}})
}

// A read that finishes after a newer one is not shown: the answered ticket
// does not come back.
func TestDecideStaleRefreshIgnored(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(assumeTickets(2)...)
	m := newDecide(t, src, popup)
	stale := m.Init()() // read while D-1 is still open
	m = dpress(t, m, "1")
	m, _ = m.Update(stale)
	if lines := strings.Split(m.View(), "\n"); lines[0] != "1 open · 0 blocking" {
		t.Errorf("stale read shown:\n%s", m.View())
	}
}

// Reasoning typed while a refresh replaced the ticket is not staged for the
// new one.
func TestReasoningTypedForReplacedTicketNotStaged(t *testing.T) {
	src := &fakeDecideSource{}
	ts := assumeTickets(2)
	src.set(ts...)
	m := newDecide(t, src, popup)
	m = dpress(t, m, "r", "for D-1")
	src.set(ts[1])
	m = reload(t, m)
	m = dpress(t, m, "enter")
	if s := lastLine(m); s != "queue changed; press again" {
		t.Errorf("status line = %q, want the notice", s)
	}
	if strings.Contains(m.View(), "staged") {
		t.Errorf("reasoning staged on the new ticket:\n%s", m.View())
	}
}

// Closing while an answer is in flight waits for it, so a delivery is not
// cut short.
func TestCloseWaitsForAnswerInFlight(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(assumeTickets(2)...)
	m := newDecide(t, src, popup)
	m, answer := m.Update(dkey("1"))
	m, cmd := m.Update(dkey("q"))
	if cmd != nil {
		t.Fatal("q closed with an answer in flight")
	}
	if s := lastLine(m); s != "closing once the answer is sent" {
		t.Errorf("status line = %q", s)
	}
	_, cmd = m.Update(answer())
	if cmd == nil {
		t.Fatal("no close after the answer")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("did not quit after the answer")
	}
	wantCalls(t, src, answerCall{"D-1", model.Answer{Key: "A", Via: model.AnsweredPopup}})
}

// A second answer key while the first answer is in flight sends nothing.
func TestNoSecondAnswerWhileSending(t *testing.T) {
	src := &fakeDecideSource{}
	src.set(assumeTickets(2)...)
	m := newDecide(t, src, popup)
	m, answer := m.Update(dkey("1"))
	m, cmd := m.Update(dkey("2"))
	if cmd != nil {
		t.Error("second answer key started a command")
	}
	if s := lastLine(m); s != "still sending the last answer" {
		t.Errorf("status line = %q", s)
	}
	drive(t, m, answer)
	wantCalls(t, src, answerCall{"D-1", model.Answer{Key: "A", Via: model.AnsweredPopup}})
}

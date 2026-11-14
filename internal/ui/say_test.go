package ui

// The say view is tested from inside the package: its countdown ticks are
// scheduled through the unexported tick seam, so a test feeds sayTickMsg
// after moving the fake clock instead of sleeping.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/route"
)

// sayStart is the fixed start of the say tests' clock.
var sayStart = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// sayGap is the pace of a person reading: past every quiet period.
const sayGap = 2500 * time.Millisecond

// sayTypingGap is the pace of typing at normal speed.
const sayTypingGap = 80 * time.Millisecond

// sayRepeat is the gap between the repeats of a held key.
const sayRepeat = 30 * time.Millisecond

type sayClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *sayClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *sayClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeSay records every call. Propose returns prop with the text filled in.
type fakeSay struct {
	mu                                         sync.Mutex
	prop                                       Proposal
	propErr, sendErr, holdErr, relErr, undoErr error
	cancelErr                                  error
	out                                        model.MessageOutcome
	calls                                      []string
	choices                                    []Choice
	proposed                                   []string
}

func (f *fakeSay) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fakeSay) Propose(_ context.Context, text string) (Proposal, error) {
	f.record("Propose")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposed = append(f.proposed, text)
	p := f.prop
	p.Text = text
	return p, f.propErr
}

func (f *fakeSay) Send(_ context.Context, c Choice) (model.MessageOutcome, error) {
	f.record("Send")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.choices = append(f.choices, c)
	return f.out, f.sendErr
}

func (f *fakeSay) Hold(_ context.Context, c Choice) (model.Message, error) {
	f.record("Hold")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.choices = append(f.choices, c)
	return model.Message{ID: "M-7", Status: model.MsgHeld, Kind: c.Kind}, f.holdErr
}

func (f *fakeSay) Release(_ context.Context, id string) (model.MessageOutcome, error) {
	f.record("Release " + id)
	return f.out, f.relErr
}

func (f *fakeSay) Undo(_ context.Context, id string) error {
	f.record("Undo " + id)
	return f.undoErr
}

func (f *fakeSay) Cancel(_ context.Context, text string) error {
	f.record("Cancel " + text)
	return f.cancelErr
}

func (f *fakeSay) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeSay) lastChoice(t *testing.T) Choice {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.choices) == 0 {
		t.Fatal("no choice was sent")
	}
	return f.choices[len(f.choices)-1]
}

func wantSayCalls(t *testing.T, src *fakeSay, want ...string) {
	t.Helper()
	if got := src.recorded(); !slices.Equal(got, want) {
		t.Errorf("source calls = %q, want %q", got, want)
	}
}

// sayCands are the targets of the tests: the pick, the runner-up and the
// rest, in the order a model proposal lists them.
func sayCands() []route.Candidate {
	api, web := "/src/api-server", "/src/web"
	return []route.Candidate{
		{
			Key: "s1", Type: route.TargetSession, Ref: "session:A", Label: "api-server · T-42 auth refactor",
			Session: model.Session{ID: "A", Repo: api, TaskID: "T-42"}, Repo: api,
		},
		{
			Key: "s2", Type: route.TargetSession, Ref: "session:B", Label: "web · main · working",
			Session: model.Session{ID: "B", Repo: web}, Repo: web,
		},
		{
			Key: "T-42", Type: route.TargetTask, Ref: "task:T-42", Label: "T-42 auth refactor · api-server · running",
			Task: model.Task{ID: "T-42", Repo: api}, Repo: api,
		},
		{Key: "r1", Type: route.TargetRepo, Ref: "repo:" + api, Label: "repo · api-server", Repo: api},
		{Key: "global", Type: route.TargetGlobal, Ref: "global", Label: "global (every repository)"},
	}
}

func modelProposal() Proposal {
	return Proposal{Kind: model.KindInstruction, Candidates: sayCands(), Confidence: 0.93, Routed: model.RoutedConfirmed}
}

func sentOutcome() model.MessageOutcome {
	return model.MessageOutcome{Message: model.Message{ID: "M-7", Kind: model.KindInstruction, Status: model.MsgDelivered}}
}

// sayRun drives one say view: commands run at once and their results are
// fed back; ticks are recorded instead of scheduled.
type sayRun struct {
	t     *testing.T
	clk   *sayClock
	m     tea.Model
	ticks []time.Duration
	quit  bool
}

func newSayRun(t *testing.T, src *fakeSay, text string, width int) *sayRun {
	t.Helper()
	r := &sayRun{t: t, clk: &sayClock{t: sayStart}}
	m := NewSay(src, r.clk.now, text)
	m.tick = func(d time.Duration) tea.Cmd {
		r.ticks = append(r.ticks, d)
		return nil
	}
	r.m = m
	r.feed(tea.WindowSizeMsg{Width: width, Height: 20})
	r.run(r.m.Init())
	return r
}

// proposed returns a view that was opened with the input line, given text
// and submitted it, the clock moved on by sayGap: the proposal has been read.
func proposed(t *testing.T, src *fakeSay, text string) *sayRun {
	t.Helper()
	r := newSayRun(t, src, "", 80)
	r.typeText(text)
	r.at(sayGap, "enter")
	r.clk.advance(sayGap)
	return r
}

func (r *sayRun) feed(msg tea.Msg) {
	r.t.Helper()
	var cmd tea.Cmd
	r.m, cmd = r.m.Update(msg)
	r.run(cmd)
}

// step feeds msg and returns its command without running it.
func (r *sayRun) step(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	r.m, cmd = r.m.Update(msg)
	return cmd
}

func (r *sayRun) run(cmd tea.Cmd) {
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
		default:
			r.m, cmd = r.m.Update(msg)
		}
	}
}

// at sends keys, each d after the one before.
func (r *sayRun) at(d time.Duration, keys ...string) {
	r.t.Helper()
	for _, k := range keys {
		r.clk.advance(d)
		r.feed(sayKey(k))
	}
}

func (r *sayRun) typeText(s string) {
	r.t.Helper()
	for _, c := range s {
		r.at(sayTypingGap, string(c))
	}
}

func (r *sayRun) paste(s string) {
	r.t.Helper()
	r.clk.advance(sayTypingGap)
	r.feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true})
}

func (r *sayRun) tick() {
	r.t.Helper()
	r.feed(sayTickMsg{})
}

func (r *sayRun) view() string { return r.m.View() }

func (r *sayRun) say() SayModel {
	r.t.Helper()
	m, ok := r.m.(SayModel)
	if !ok {
		r.t.Fatalf("model is %T", r.m)
	}
	return m
}

func (r *sayRun) wantLine(s string) {
	r.t.Helper()
	for _, l := range strings.Split(r.view(), "\n") {
		if l == s {
			return
		}
	}
	r.t.Errorf("no line %q in view:\n%s", s, r.view())
}

func (r *sayRun) wantNoLine(s string) {
	r.t.Helper()
	for _, l := range strings.Split(r.view(), "\n") {
		if l == s {
			r.t.Errorf("line %q in view:\n%s", s, r.view())
		}
	}
}

func (r *sayRun) wantIn(s string) {
	r.t.Helper()
	if !strings.Contains(r.view(), s) {
		r.t.Errorf("view has no %q:\n%s", s, r.view())
	}
}

func sayKey(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

const proposalLine = "→ api-server · T-42 auth refactor · instruction (0.93)"

func TestSayStartsWithInputWhenNoText(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := newSayRun(t, src, "", 80)
	r.wantLine("say: _")
	r.typeText("fix it")
	r.wantLine("say: fix it_")
	wantSayCalls(t, src)
	r.at(sayGap, "enter")
	wantSayCalls(t, src, "Propose")
	if !slices.Equal(src.proposed, []string{"fix it"}) {
		t.Errorf("proposed %q", src.proposed)
	}

	src2 := &fakeSay{prop: modelProposal()}
	r2 := newSayRun(t, src2, "", 80)
	r2.typeText("never mind")
	r2.at(sayGap, "esc")
	if !r2.quit {
		t.Error("Esc on the input did not quit")
	}
	wantSayCalls(t, src2)
	if line, ok := r2.say().Result(); line != "" || ok {
		t.Errorf("Result() = %q, %v; want \"\", false", line, ok)
	}
}

// An empty input is not proposed, and Backspace edits the input.
func TestSayInputEditing(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := newSayRun(t, src, "", 80)
	r.at(sayGap, " ", "enter")
	wantSayCalls(t, src)
	r.typeText("fixx")
	r.at(sayTypingGap, "backspace")
	r.at(sayGap, "enter")
	if !slices.Equal(src.proposed, []string{"fix"}) {
		t.Errorf("proposed %q, want fix", src.proposed)
	}
}

func TestSayInputCap(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := newSayRun(t, src, "", 80)
	for range 900 {
		r.at(time.Millisecond, "a")
	}
	r.at(sayGap, "enter")
	if len(src.proposed) != 1 || utf8.RuneCountInString(src.proposed[0]) != message.MaxText {
		t.Fatalf("proposed %d texts, the first %d runes; want one of %d", len(src.proposed),
			utf8.RuneCountInString(strings.Join(src.proposed, "")), message.MaxText)
	}
}

// A paste into the input is text: escape sequences, control characters and
// bidi overrides are removed, line breaks become spaces, it is bounded by
// the message limit, and a pasted line break never submits.
func TestSayPasteIntoInput(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := newSayRun(t, src, "", 80)
	r.typeText("fix")
	r.paste(" the \x1b[31mred\x1b[0m\r\nbutton\u202e now\x07\r")
	wantSayCalls(t, src)
	r.wantLine("say: fix the red button now _")
	r.paste(strings.Repeat("é", 2*message.MaxText))
	wantSayCalls(t, src)
	r.at(sayGap, "enter")
	if len(src.proposed) != 1 {
		t.Fatalf("proposed %q", src.proposed)
	}
	got := src.proposed[0]
	if n := utf8.RuneCountInString(got); n > message.MaxText {
		t.Errorf("proposed %d runes, want at most %d", n, message.MaxText)
	}
	if !strings.HasPrefix(got, "fix the red button now é") || strings.ContainsAny(got, "\x1b\r\n\a\u202e") {
		t.Errorf("proposed %q", got)
	}
}

// Text pasted without bracketed paste comes as bursts of runes with an
// Enter for each line break: the Enters are spaces, not a submit.
func TestSayBurstPasteDoesNotSubmit(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := newSayRun(t, src, "", 80)
	r.clk.advance(sayGap)
	r.feed(sayKey("fix the"))
	r.feed(sayKey("enter"))
	r.feed(sayKey("login bug"))
	r.at(time.Millisecond, "enter")
	wantSayCalls(t, src)
	r.wantLine("say: fix the login bug _")
	r.at(sayGap, "enter")
	if !slices.Equal(src.proposed, []string{"fix the login bug"}) {
		t.Errorf("proposed %q", src.proposed)
	}
}

func TestSayShowsProposal(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := proposed(t, src, "fix the login bug")
	r.wantLine(proposalLine)
	r.wantLine("say: fix the login bug")

	p := modelProposal()
	p.Routed, p.Confidence = model.RoutedAddress, 0
	r2 := proposed(t, &fakeSay{prop: p}, "@api-server: fix it")
	r2.wantLine("→ api-server · T-42 auth refactor · instruction")
}

// Started with text (curia say <text>), the view proposes at once.
func TestSayStartsWithText(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := newSayRun(t, src, "fix it", 80)
	wantSayCalls(t, src, "Propose")
	r.wantLine(proposalLine)
}

func TestSayEnterSendsUnchanged(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := proposed(t, src, "fix it")
	r.at(0, "enter")
	wantSayCalls(t, src, "Propose", "Send")
	c := src.lastChoice(t)
	if c.Changed || c.Target.Key != "s1" || c.Kind != model.KindInstruction || c.Proposal.Text != "fix it" {
		t.Errorf("choice = %+v", c)
	}
	r.wantLine("M-7 instruction → api-server · T-42 auth refactor · delivered")
}

func TestSayTabCyclesRunnerUpFirst(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := proposed(t, src, "fix it")
	r.at(sayGap, "tab")
	r.wantLine("→ web · main · working · instruction")
	r.at(sayGap, "enter")
	c := src.lastChoice(t)
	if !c.Changed || c.Target.Key != "s2" {
		t.Errorf("choice = %+v, want s2 changed", c)
	}

	src2 := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r2 := proposed(t, src2, "fix it")
	for range len(sayCands()) {
		r2.at(sayGap, "tab")
	}
	r2.wantLine(proposalLine)
	r2.at(sayGap, "shift+tab")
	r2.wantLine("→ global (every repository) · instruction")
	r2.at(sayGap, "tab", "enter")
	c = src2.lastChoice(t)
	if c.Changed || c.Target.Key != "s1" {
		t.Errorf("choice = %+v, want s1 unchanged", c)
	}
}

func TestSayKChangesKind(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := proposed(t, src, "always run gofumpt")
	r.at(sayGap, "k")
	r.wantLine("→ api-server · T-42 auth refactor · rule")
	r.at(sayGap, "enter")
	c := src.lastChoice(t)
	if !c.Changed || c.Kind != model.KindRule || c.Target.Key != "s1" {
		t.Errorf("choice = %+v, want rule changed", c)
	}
}

// k skips the kinds route.Settle refuses for the target: global takes only
// a rule.
func TestSayKSkipsRefusedKinds(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := proposed(t, src, "always run gofumpt")
	r.at(sayGap, "shift+tab")
	r.wantLine("→ global (every repository) · instruction")
	r.at(sayGap, "k")
	r.wantLine("→ global (every repository) · rule")
	r.at(sayGap, "k")
	r.wantLine("→ global (every repository) · rule")
	r.wantIn("takes no other kind")
}

func TestSayEscCancels(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := proposed(t, src, "fix it")
	r.at(sayGap, "esc")
	wantSayCalls(t, src, "Propose", "Cancel fix it")
	if !r.quit {
		t.Error("Esc on the proposal did not quit")
	}
}

// Esc while the router runs cancels too, and the late proposal is dropped.
func TestSayEscWhileRoutingCancels(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := newSayRun(t, src, "", 80)
	r.typeText("fix it")
	r.clk.advance(sayGap)
	propose := r.step(sayKey("enter"))
	r.wantLine("routing…")
	result := propose()
	r.at(sayGap, "esc")
	r.feed(result)
	wantSayCalls(t, src, "Propose", "Cancel fix it")
	if !r.quit {
		t.Error("Esc while routing did not quit")
	}
}

// q is not a command on the proposal: it is typed text and never sends or
// cancels.
func TestSayQNeverSends(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := proposed(t, src, "fix it")
	r.at(sayGap, "q")
	r.at(sayGap, "q")
	wantSayCalls(t, src, "Propose")
	if r.quit {
		t.Error("q quit the proposal")
	}
}

// heldEnter is the auto-repeat of a held Enter: the first repeat first
// after the press, then one every sayRepeat for d.
func (r *sayRun) heldEnter(first, d time.Duration) {
	r.t.Helper()
	r.at(first, "enter")
	for held := time.Duration(0); held < d; held += sayRepeat {
		r.at(sayRepeat, "enter")
	}
}

func TestSayHeldEnterDoesNotSend(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "", 80)
	r.typeText("fix it")
	r.at(sayGap, "enter")
	r.at(400*time.Millisecond, "enter")
	r.at(300*time.Millisecond, "enter")
	wantSayCalls(t, src, "Propose")
	r.wantLine(noticeTooSoon)
	r.at(time.Second, "enter")
	wantSayCalls(t, src, "Propose", "Send")
}

// The orchestrator's sequences: a held Enter that submits the input and
// goes on repeating while the proposal appears never sends, whatever the
// keyboard's repeat delay.
func TestSayHeldEnterAcrossProposalNeverSends(t *testing.T) {
	for _, first := range []time.Duration{400 * time.Millisecond, 500 * time.Millisecond, 660 * time.Millisecond} {
		for _, appear := range []int{-1, 0, 1, 5, 30} { // repeats before the proposal comes
			src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
			r := newSayRun(t, src, "", 80)
			r.typeText("fix it")
			r.clk.advance(sayGap)
			propose := r.step(sayKey("enter"))
			result := propose()
			if appear < 0 { // at once, before the first repeat
				r.feed(result)
			}
			r.at(first, "enter")
			for i := 0; i < 60; i++ {
				if i == appear {
					r.feed(result)
				}
				r.at(sayRepeat, "enter")
			}
			wantSayCalls(t, src, "Propose")
			r.at(time.Second, "enter")
			wantSayCalls(t, src, "Propose", "Send")
		}
	}
}

// The Enter that ran curia say <text> in the shell, held, never sends the
// proposal that appears at once.
func TestSayHeldEnterFromCommandLine(t *testing.T) {
	for _, first := range []time.Duration{400 * time.Millisecond, 500 * time.Millisecond, 660 * time.Millisecond} {
		p := modelProposal()
		p.Routed, p.Confidence = model.RoutedAddress, 0
		src := &fakeSay{prop: p, out: sentOutcome()}
		r := newSayRun(t, src, "@api-server fix it", 80)
		r.heldEnter(first, time.Second)
		wantSayCalls(t, src, "Propose")
		r.at(time.Second, "enter")
		wantSayCalls(t, src, "Propose", "Send")
	}
}

func TestSayEnterBeforeDwellIgnored(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "", 80)
	r.typeText("fix it")
	r.clk.advance(sayGap)
	propose := r.step(sayKey("enter"))
	r.clk.advance(sayGap) // the router took its time
	r.feed(propose())
	r.at(100*time.Millisecond, "enter")
	wantSayCalls(t, src, "Propose")
	r.wantLine(noticeTooSoon)
	r.at(sayGap, "enter")
	wantSayCalls(t, src, "Propose", "Send")
}

// Tab and k restart the dwell: an Enter right after them does not send
// what the user has not seen.
func TestSayChangeRestartsDwell(t *testing.T) {
	for _, k := range []string{"tab", "shift+tab", "k"} {
		src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
		r := proposed(t, src, "fix it")
		r.at(sayGap, k)
		r.at(100*time.Millisecond, "enter")
		wantSayCalls(t, src, "Propose")
		r.at(sayGap, "enter")
		wantSayCalls(t, src, "Propose", "Send")
	}
}

func TestSayPasteIsNotACommand(t *testing.T) {
	for _, p := range []string{"\r", "k", "u", "\t", "\x1b", "q"} {
		src := &fakeSay{prop: modelProposal()}
		r := proposed(t, src, "fix it")
		r.paste(p)
		wantSayCalls(t, src, "Propose")
		r.wantLine(proposalLine)
		r.at(sayTypingGap, "enter") // inside the quiet period the paste started
		wantSayCalls(t, src, "Propose")

		p2 := modelProposal()
		p2.Auto = true
		src2 := &fakeSay{prop: p2}
		r2 := proposed(t, src2, "fix it")
		r2.paste(p)
		wantSayCalls(t, src2, "Propose", "Hold")
	}
}

// Text typed on the proposal is never a command: the k in it does not
// change the kind, and an Enter during the quiet period it starts does not
// send.
func TestSayTypedTextIsNotACommand(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := proposed(t, src, "fix it")
	r.typeText("make it quick")
	r.wantLine(proposalLine)
	r.at(300*time.Millisecond, "enter")
	r.at(time.Second, "tab")
	wantSayCalls(t, src, "Propose")
	r.wantLine(proposalLine)
	r.at(sayGap, "enter")
	c := src.lastChoice(t)
	if c.Changed {
		t.Errorf("choice = %+v, want unchanged", c)
	}
}

func autoProposal() Proposal {
	p := modelProposal()
	p.Auto = true
	return p
}

const holdLine = "→ api-server · T-42 auth refactor (u to undo, %ds)"

func countdown(n int) string {
	return fmt.Sprintf(holdLine, n)
}

func TestSayAutoHoldsThenReleases(t *testing.T) {
	src := &fakeSay{prop: autoProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "fix it", 80)
	wantSayCalls(t, src, "Propose", "Hold")
	if c := src.lastChoice(t); c.Changed || c.Target.Key != "s1" {
		t.Errorf("held choice = %+v", c)
	}
	for n := 5; n >= 1; n-- {
		r.wantLine(countdown(n))
		r.clk.advance(time.Second)
		r.tick()
	}
	wantSayCalls(t, src, "Propose", "Hold", "Release M-7")
	for range 3 {
		r.clk.advance(time.Second)
		r.tick()
	}
	wantSayCalls(t, src, "Propose", "Hold", "Release M-7")
	r.wantLine("M-7 instruction → api-server · T-42 auth refactor · delivered")
	if len(r.ticks) != 5 {
		t.Errorf("ticks scheduled %v, want five", r.ticks)
	}
	for _, d := range r.ticks {
		if d != time.Second {
			t.Errorf("tick after %v, want 1s", d)
		}
	}
	if line, ok := r.say().Result(); !ok || !strings.HasPrefix(line, "M-7 ") {
		t.Errorf("Result() = %q, %v", line, ok)
	}
}

// The countdown follows the clock, not the number of ticks: a late tick
// releases, an early one does not.
func TestSayAutoCountsFromClock(t *testing.T) {
	src := &fakeSay{prop: autoProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "fix it", 80)
	r.clk.advance(4900 * time.Millisecond)
	r.wantLine(countdown(1))
	r.tick()
	wantSayCalls(t, src, "Propose", "Hold")
	if got := r.ticks[len(r.ticks)-1]; got != 100*time.Millisecond {
		t.Errorf("next tick after %v, want 100ms", got)
	}
	r.clk.advance(100 * time.Millisecond)
	r.tick()
	wantSayCalls(t, src, "Propose", "Hold", "Release M-7")
}

func TestSayAutoUndo(t *testing.T) {
	for _, k := range []string{"u", "esc"} {
		src := &fakeSay{prop: autoProposal(), out: sentOutcome()}
		r := newSayRun(t, src, "fix it", 80)
		r.clk.advance(time.Second)
		r.tick()
		r.at(time.Second, k)
		wantSayCalls(t, src, "Propose", "Hold", "Undo M-7")
		for range 6 {
			r.clk.advance(time.Second)
			r.tick()
		}
		wantSayCalls(t, src, "Propose", "Hold", "Undo M-7")
		r.wantLine("M-7 undone")
		if line, ok := r.say().Result(); ok || line != "M-7 undone" {
			t.Errorf("Result() = %q, %v", line, ok)
		}
	}
}

// After the 5 seconds nothing undoes: a u that comes before the tick
// releases instead.
func TestSayAutoUndoTooLate(t *testing.T) {
	src := &fakeSay{prop: autoProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "fix it", 80)
	r.at(SayHold, "u")
	r.tick()
	r.at(0, "u")
	wantSayCalls(t, src, "Propose", "Hold", "Release M-7")
}

// u while the hold is being stored undoes as soon as it is stored.
func TestSayUndoBeforeHoldStored(t *testing.T) {
	src := &fakeSay{prop: autoProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "", 80)
	r.typeText("fix it")
	r.at(sayGap, "enter")
	wantSayCalls(t, src, "Propose", "Hold")

	src2 := &fakeSay{prop: autoProposal()}
	r2 := newSayRun(t, src2, "", 80)
	r2.typeText("fix it")
	r2.clk.advance(sayGap)
	propose := r2.step(sayKey("enter"))
	hold := r2.step(propose())
	r2.wantIn("holding…")
	r2.at(100*time.Millisecond, "u")
	r2.feed(hold())
	wantSayCalls(t, src2, "Propose", "Hold", "Undo M-7")
}

// Ctrl+C during the hold is an interrupt: it undoes, then quits.
func TestSayInterruptDuringHoldUndoes(t *testing.T) {
	src := &fakeSay{prop: autoProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "fix it", 80)
	r.at(time.Second, "ctrl+c")
	wantSayCalls(t, src, "Propose", "Hold", "Undo M-7")
	if !r.quit {
		t.Error("Ctrl+C did not quit after the undo")
	}
}

func TestSayAutoIgnoresOtherKeys(t *testing.T) {
	src := &fakeSay{prop: autoProposal(), out: sentOutcome()}
	r := newSayRun(t, src, "fix it", 80)
	r.at(100*time.Millisecond, "enter", "k", "tab", "q", " ", "x")
	r.heldEnter(400*time.Millisecond, time.Second)
	wantSayCalls(t, src, "Propose", "Hold")
	if r.quit {
		t.Error("a key quit the hold")
	}
}

// No tick is scheduled outside the hold.
func TestSayTicksOnlyInHold(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := proposed(t, src, "fix it")
	r.at(sayGap, "tab", "k", "enter")
	r.tick()
	if len(r.ticks) != 0 {
		t.Errorf("ticks scheduled %v outside the hold", r.ticks)
	}
	wantSayCalls(t, src, "Propose", "Send")
}

func TestSayProposeErrorShown(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), propErr: errors.New("store is locked")}
	r := proposed(t, src, "fix it")
	r.wantLine("routing failed: store is locked")
	r.at(sayGap, "enter")
	wantSayCalls(t, src, "Propose")
	if r.quit {
		t.Error("Enter quit")
	}
	r.at(sayGap, "esc")
	if !r.quit {
		t.Error("Esc did not quit")
	}
	wantSayCalls(t, src, "Propose")
}

// A refused send is shown; the proposal stays for another target or kind,
// and a held key cannot send again at once.
func TestSaySendErrorShown(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), sendErr: errors.New("T-42 has no live agent")}
	r := proposed(t, src, "fix it")
	r.at(0, "enter")
	r.wantLine("send failed: T-42 has no live agent")
	r.wantLine(proposalLine)
	r.at(sayTypingGap, "enter")
	wantSayCalls(t, src, "Propose", "Send")
	src.mu.Lock()
	src.sendErr, src.out = nil, sentOutcome()
	src.mu.Unlock()
	r.at(sayGap, "tab")
	r.at(sayGap, "enter")
	wantSayCalls(t, src, "Propose", "Send", "Send")
	if line, ok := r.say().Result(); !ok || line == "" {
		t.Errorf("Result() = %q, %v", line, ok)
	}
}

func TestSayManualPickList(t *testing.T) {
	p := Proposal{
		Kind: model.KindInstruction, Candidates: sayCands(), Routed: model.RoutedManual,
		Note: "the router gave no answer; pick a target",
	}
	src := &fakeSay{prop: p, out: sentOutcome()}
	r := proposed(t, src, "fix it")
	r.wantIn("the router gave no answer; pick a target")
	seen := map[string]bool{}
	for range sayCands() {
		for _, c := range sayCands() {
			if strings.Contains(r.view(), "→ "+c.Label+" ·") {
				seen[c.Key] = true
			}
		}
		r.at(sayGap, "tab")
	}
	if len(seen) != len(sayCands()) {
		t.Errorf("Tab showed %v, want every candidate", seen)
	}
	r.wantNoLine(proposalLine) // no number without a model
}

// untrustedProposal holds escape sequences, bidi overrides, control
// characters and wide characters in every drawn field.
func untrustedProposal() Proposal {
	cands := sayCands()
	cands[0].Label = "api\x1b[31m-server\x1b]0;pwned\a · \u202eT-42 \u4f60\u597d\u4e16\u754c auth\r\nrefactor " + strings.Repeat("宽", 40)
	cands[1].Label = "\x1b[2J\u202dweb\u2066 \u009b31m main"
	cands[0].Key = "s1\x1b[0m"
	p := modelProposal()
	p.Candidates = cands
	p.Note = "auto-send is \x1b[5mnot\x1b[0m earned \u202e" + strings.Repeat("注意 ", 30)
	return p
}

func checkClean(t *testing.T, name, view string, width int) {
	t.Helper()
	if strings.ContainsAny(view, "\x1b\u202e\u202d\u2066\a\r\u009b") {
		t.Errorf("%s: view holds a control or bidi character: %q", name, view)
	}
	for _, l := range strings.Split(view, "\n") {
		if w := lipgloss.Width(l); w > width {
			t.Errorf("%s: line is %d cells, want at most %d: %q", name, w, width, l)
		}
	}
}

func TestSayTextCannotDrawEscapes(t *testing.T) {
	for _, width := range []int{40, 80, 200} {
		src := &fakeSay{prop: untrustedProposal(), out: sentOutcome()}
		src.out.Note = "not delivered \x1b[31m\u202e: the pane is busy"
		src.out.Message.ID = "M-7\x1b[0m"
		r := newSayRun(t, src, "", width)
		r.paste("say \x1b[31mthis\u202e")
		checkClean(t, "input", r.view(), width)
		r.at(sayGap, "enter")
		checkClean(t, "proposal", r.view(), width)
		r.at(sayGap, "tab")
		checkClean(t, "runner-up", r.view(), width)
		r.at(sayGap, "enter")
		checkClean(t, "outcome", r.view(), width)
		if line, _ := r.say().Result(); strings.ContainsAny(line, "\x1b\u202e") {
			t.Errorf("Result() holds an escape: %q", line)
		}

		src2 := &fakeSay{prop: untrustedProposal()}
		src2.prop.Auto = true
		r2 := newSayRun(t, src2, "fix it", width)
		checkClean(t, "hold", r2.view(), width)
		if !strings.Contains(r2.view(), "(u to undo, 5s)") {
			t.Errorf("width %d: the countdown is cut:\n%s", width, r2.view())
		}
	}
}

func TestSayNarrowWidth(t *testing.T) {
	p := modelProposal()
	p.Note = "auto-send is not earned: 12 of the newest 50 labels"
	src := &fakeSay{prop: p, out: sentOutcome()}
	r := newSayRun(t, src, "", 40)
	r.typeText(strings.Repeat("fix the login bug ", 5))
	checkClean(t, "input", r.view(), 40)
	r.at(sayGap, "enter")
	checkClean(t, "proposal", r.view(), 40)
	r.wantIn("instruction (0.93)") // the kind and the number are never cut
	r.at(sayGap, "k")
	checkClean(t, "kind", r.view(), 40)
	r.at(sayGap, "enter")
	checkClean(t, "outcome", r.view(), 40)
}

func TestSayResultAfterQuit(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := proposed(t, src, "fix it")
	r.at(0, "enter")
	r.at(sayGap, "q")
	if !r.quit {
		t.Fatal("q did not quit after the send")
	}
	line, ok := r.say().Result()
	if !ok || line != "M-7 instruction → api-server · T-42 auth refactor · delivered" {
		t.Errorf("Result() = %q, %v", line, ok)
	}

	src2 := &fakeSay{prop: modelProposal()}
	r2 := proposed(t, src2, "fix it")
	r2.at(0, "esc")
	if line, ok := r2.say().Result(); line != "" || ok {
		t.Errorf("Result() after a cancel = %q, %v", line, ok)
	}
}

// Ruling 93: nothing typed after a send is read as a command for 2 s, and
// each key restarts that window; Esc closes at once.
func TestSayKeysAfterSendIgnored(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r := proposed(t, src, "fix it")
	r.at(0, "enter")
	r.typeText("and quickly")
	r.heldEnter(400*time.Millisecond, time.Second)
	r.at(1500*time.Millisecond, "q")
	if r.quit {
		t.Fatal("a key within 2 s of the last one closed the view")
	}
	wantSayCalls(t, src, "Propose", "Send")
	r.wantLine("M-7 instruction → api-server · T-42 auth refactor · delivered")
	r.at(sayGap, "q")
	if !r.quit {
		t.Error("q after a pause did not close")
	}

	src2 := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r2 := proposed(t, src2, "fix it")
	r2.at(0, "enter")
	r2.at(sayTypingGap, "esc")
	if !r2.quit {
		t.Error("Esc did not close at once")
	}
}

// An outcome with a note (not delivered) is shown with the note.
func TestSayOutcomeNote(t *testing.T) {
	src := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	src.out.Note = "queued: the agent is working; it gets the message when it stops"
	r := proposed(t, src, "fix it")
	r.at(0, "enter")
	if line, ok := r.say().Result(); !ok || line != "M-7 instruction → api-server · T-42 auth refactor · "+src.out.Note {
		t.Errorf("Result() = %q, %v", line, ok)
	}
	r.wantIn("queued: the agent is")

	src2 := &fakeSay{prop: modelProposal()}
	src2.out = model.MessageOutcome{Message: model.Message{
		ID: "M-8", Kind: model.KindTask, DeliveredVia: model.ViaApplied, ResultRef: "T-43",
	}}
	r2 := proposed(t, src2, "add a retry")
	r2.at(sayGap, "k", "k")
	r2.at(sayGap, "enter")
	r2.wantIn("M-8 task → api-server · T-42 auth refactor · applied T-43")
}

// Ctrl+C on the proposal cancels like Esc; while a send is in flight it
// waits for the result, then quits.
func TestSayInterrupt(t *testing.T) {
	src := &fakeSay{prop: modelProposal()}
	r := proposed(t, src, "fix it")
	r.at(sayGap, "ctrl+c")
	wantSayCalls(t, src, "Propose", "Cancel fix it")
	if !r.quit {
		t.Error("Ctrl+C did not quit")
	}

	src2 := &fakeSay{prop: modelProposal(), out: sentOutcome()}
	r2 := proposed(t, src2, "fix it")
	send := r2.step(sayKey("enter"))
	r2.at(sayTypingGap, "ctrl+c")
	if r2.quit {
		t.Fatal("quit with the send in flight")
	}
	r2.feed(send())
	if !r2.quit {
		t.Error("did not quit once the send returned")
	}
	if _, ok := r2.say().Result(); !ok {
		t.Error("Result() is not ok after the send")
	}
}

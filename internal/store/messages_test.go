package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// instruction is a queued instruction from the user to session, created at
// t0, with text.
func instruction(text, session string) model.Message {
	return model.Message{
		Text: text, Kind: model.KindInstruction, Sender: model.SenderUser,
		TargetSession: session, RoutedBy: model.RoutedAddress, Status: model.MsgQueued, CreatedAt: t0,
	}
}

// held is instruction held until t0 + 5 s.
func held(text, session string) model.Message {
	m := instruction(text, session)
	m.Status, m.HoldUntil = model.MsgHeld, t0.Add(5*time.Second)
	return m
}

// createMessage stores m and fails the test on an error.
func createMessage(t *testing.T, s *store.Store, m model.Message) model.Message {
	t.Helper()
	got, err := s.CreateMessage(context.Background(), m)
	if err != nil {
		t.Fatalf("CreateMessage(%q): %v", m.Text, err)
	}
	return got
}

// queuedMessage stores a queued instruction for session.
func queuedMessage(t *testing.T, s *store.Store, text, session string) model.Message {
	t.Helper()
	return createMessage(t, s, instruction(text, session))
}

func getMessage(t *testing.T, s *store.Store, id string) model.Message {
	t.Helper()
	m, ok, err := s.GetMessage(context.Background(), id)
	if err != nil {
		t.Fatalf("GetMessage(%s): %v", id, err)
	}
	if !ok {
		t.Fatalf("GetMessage(%s): not found", id)
	}
	return m
}

// assertMessage compares two messages, times with Equal and everything else
// with DeepEqual.
func assertMessage(t *testing.T, got, want model.Message) {
	t.Helper()
	times := func(m *model.Message) []*time.Time {
		return []*time.Time{&m.CreatedAt, &m.HoldUntil, &m.DeliveredAt, &m.ConfirmedAt}
	}
	names := []string{"CreatedAt", "HoldUntil", "DeliveredAt", "ConfirmedAt"}
	gt, wt := times(&got), times(&want)
	for i := range gt {
		if !gt[i].Equal(*wt[i]) {
			t.Errorf("%s %s = %v, want %v", want.ID, names[i], *gt[i], *wt[i])
		}
		*gt[i], *wt[i] = time.Time{}, time.Time{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("message:\n got %+v\nwant %+v", got, want)
	}
}

// assertMessageStatus fails unless message id is stored with status and via.
func assertMessageStatus(t *testing.T, s *store.Store, id string, status model.MessageStatus, via model.DeliveryVia) model.Message {
	t.Helper()
	got := getMessage(t, s, id)
	if got.Status != status || got.DeliveredVia != via {
		t.Errorf("%s: status %s via %q, want %s via %q", id, got.Status, got.DeliveredVia, status, via)
	}
	return got
}

func messageIDs(ms []model.Message) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// claimMessageTyping is ClaimMessageTyping, failing the test on an error.
func claimMessageTyping(t *testing.T, s *store.Store, id string, at time.Time) store.MessageClaim {
	t.Helper()
	c, err := s.ClaimMessageTyping(context.Background(), id, at)
	if err != nil {
		t.Fatalf("ClaimMessageTyping(%s): %v", id, err)
	}
	return c
}

func TestMessageRoundTrip(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateIdle)
	full := model.Message{
		N: 99, ID: "M-99", // ignored: the store allocates
		Text: "the api tests are green on my branch", Kind: model.KindInstruction,
		Sender: model.SenderAgent, SenderSession: "s9",
		TargetSession: "s1", TargetTask: "T-4", TargetRepo: "/src/curia",
		SuggestedTarget: "session:s1", RunnerUp: "task:T-5", SuggestedKind: model.KindInstruction,
		Confidence: 0.875, HasConfidence: true, RoutedBy: model.RoutedConfirmed, AutoEligible: true,
		Status:    model.MsgHeld,
		CreatedAt: t0.Add(123456789 * time.Nanosecond), HoldUntil: t0.Add(5*time.Second + 7*time.Nanosecond),
		// Ignored: only a transition sets them.
		DeliveredVia: model.ViaCheck, ResultRef: "D-1", DeliveredAt: t0, ConfirmedAt: t0,
	}
	got := createMessage(t, s, full)
	want := full
	want.N, want.ID = 1, "M-1"
	want.DeliveredVia, want.ResultRef, want.DeliveredAt, want.ConfirmedAt = "", "", time.Time{}, time.Time{}
	assertMessage(t, got, want)
	assertMessage(t, getMessage(t, s, "M-1"), want)

	// The delivery columns, through the transitions that set them.
	if _, err := s.ReleaseMessage(ctx, "M-1"); err != nil {
		t.Fatal(err)
	}
	typedAt, checkedAt := tClaim.Add(3*time.Nanosecond), tClaim.Add(time.Second+9*time.Nanosecond)
	if c := claimMessageTyping(t, s, "M-1", typedAt); c.Refusal != store.TypingClaimed {
		t.Fatalf("claim = %v", c.Refusal)
	}
	if _, err := s.CheckMessages(ctx, "s1", checkedAt); err != nil {
		t.Fatal(err)
	}
	want.Status, want.DeliveredVia, want.DeliveredAt, want.ConfirmedAt = model.MsgDelivered, model.ViaSendKeys, typedAt, checkedAt
	assertMessage(t, getMessage(t, s, "M-1"), want)

	// NULL confidence reads back as HasConfidence false, whatever Confidence
	// held; a rule's result_ref is stored when it is applied.
	rule := model.Message{
		Text: "use pnpm, not npm", Kind: model.KindRule, Sender: model.SenderUser, TargetRepo: "/src/curia",
		Confidence: 0.5, RoutedBy: model.RoutedManual, Status: model.MsgQueued, CreatedAt: t0,
	}
	got = createMessage(t, s, rule)
	if got.HasConfidence || got.Confidence != 0 {
		t.Errorf("no confidence stored as %v, %v; want 0, false", got.Confidence, got.HasConfidence)
	}
	applied, err := s.ApplyMessage(ctx, got.ID, "D-4", tClaim)
	if err != nil {
		t.Fatal(err)
	}
	wantRule := got
	wantRule.Status, wantRule.DeliveredVia, wantRule.ResultRef, wantRule.DeliveredAt = model.MsgDelivered, model.ViaApplied, "D-4", tClaim
	assertMessage(t, applied, wantRule)
	assertMessage(t, getMessage(t, s, got.ID), wantRule)
}

func TestCreateAllocatesMessageIDs(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i, want := range []string{"M-1", "M-2", "M-3"} {
		if got := queuedMessage(t, s, fmt.Sprintf("message %d", i), "s1"); got.ID != want || got.N != int64(i+1) {
			t.Errorf("message %d = %s (n %d), want %s", i, got.ID, got.N, want)
		}
	}
	// Only queued, held and canceled are created; held needs its hold.
	for _, bad := range []model.Message{
		func() model.Message { m := instruction("delivered", "s1"); m.Status = model.MsgDelivered; return m }(),
		func() model.Message { m := instruction("undone", "s1"); m.Status = model.MsgUndone; return m }(),
		func() model.Message { m := held("no hold", "s1"); m.HoldUntil = time.Time{}; return m }(),
	} {
		if got, err := s.CreateMessage(ctx, bad); err == nil {
			t.Errorf("CreateMessage(%q) = %s, want an error", bad.Text, got.ID)
		}
	}
	canceled := instruction("escaped before sending", "s1")
	canceled.Status = model.MsgCanceled
	if got := createMessage(t, s, canceled); got.ID != "M-4" || got.Status != model.MsgCanceled {
		t.Errorf("canceled = %s %s, want M-4 canceled", got.ID, got.Status)
	}
	if n := len(listAll(t, s)); n != 4 {
		t.Errorf("stored %d messages, want 4: a refused create stored something", n)
	}
}

// listAll is every stored message, in id order.
func listAll(t *testing.T, s *store.Store) []model.Message {
	t.Helper()
	all, err := s.ListMessages(context.Background(), store.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func TestConcurrentCreatesAllocateGaplessMessageIDs(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 4)
	const each = 10
	errs := make([]error, len(hs))
	var wg sync.WaitGroup
	for k, h := range hs {
		wg.Go(func() { errs[k] = createEach(h, k, each) })
	}
	wg.Wait()
	for k, err := range errs {
		if err != nil {
			t.Errorf("handle %d: %v", k, err)
		}
	}
	all := listAll(t, hs[0])
	if len(all) != len(hs)*each {
		t.Fatalf("stored %d messages, want %d", len(all), len(hs)*each)
	}
	for i, m := range all {
		if want := fmt.Sprintf("M-%d", i+1); m.ID != want {
			t.Fatalf("message %d is %s, want %s", i, m.ID, want)
		}
	}
}

// createEach creates each instructions through h, retrying a create that
// found the database busy before it ran.
func createEach(h *store.Store, k, each int) error {
	for i := range each {
		err := retryBusy(func() error {
			_, err := h.CreateMessage(context.Background(), instruction(fmt.Sprintf("from %d, %d", k, i), "s1"))
			return err
		}, busyAtBegin)
		if err != nil {
			return err
		}
	}
	return nil
}

// transition is one call that may change a message's status.
type transition struct {
	name string
	run  func(s *store.Store, id string) error
	from model.MessageStatus // the one status it accepts
	to   model.MessageStatus
	err  error // when the message is in any other status
}

var transitions = []transition{
	{"release", func(s *store.Store, id string) error {
		_, err := s.ReleaseMessage(context.Background(), id)
		return err
	}, model.MsgHeld, model.MsgQueued, store.ErrNotHeld},
	{"undo", func(s *store.Store, id string) error {
		_, err := s.UndoMessage(context.Background(), id)
		return err
	}, model.MsgHeld, model.MsgUndone, store.ErrNotHeld},
	{"cancel", func(s *store.Store, id string) error {
		_, err := s.CancelMessage(context.Background(), id)
		return err
	}, model.MsgQueued, model.MsgCanceled, store.ErrNotQueued},
	{"apply", func(s *store.Store, id string) error {
		_, err := s.ApplyMessage(context.Background(), id, "T-1", tClaim)
		return err
	}, model.MsgQueued, model.MsgDelivered, store.ErrNotQueued},
}

// inStatus stores a task message for s1 and moves it to status through the
// store's own transitions.
func inStatus(t *testing.T, s *store.Store, status model.MessageStatus) model.Message {
	t.Helper()
	ctx := context.Background()
	m := held("add a flaky-test task", "")
	m.Kind = model.KindTask
	m = createMessage(t, s, m)
	var err error
	switch status {
	case model.MsgHeld:
	case model.MsgUndone:
		_, err = s.UndoMessage(ctx, m.ID)
	case model.MsgQueued:
		_, err = s.ReleaseMessage(ctx, m.ID)
	case model.MsgDelivered:
		if _, err = s.ReleaseMessage(ctx, m.ID); err == nil {
			_, err = s.ApplyMessage(ctx, m.ID, "T-1", tClaim)
		}
	case model.MsgCanceled:
		if _, err = s.ReleaseMessage(ctx, m.ID); err == nil {
			_, err = s.CancelMessage(ctx, m.ID)
		}
	}
	if err != nil {
		t.Fatalf("setup %s: %v", status, err)
	}
	return assertMessageStatus(t, s, m.ID, status, getMessage(t, s, m.ID).DeliveredVia)
}

// One row per line of section 4.9's table of transitions; every other
// transition returns its typed error and changes nothing.
func TestMessageTransitionsTable(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	// (none) -> queued, held, canceled: CreateMessage (TestCreateAllocatesMessageIDs).
	for _, tr := range transitions {
		for _, from := range []model.MessageStatus{model.MsgHeld, model.MsgQueued, model.MsgDelivered, model.MsgUndone, model.MsgCanceled} {
			t.Run(tr.name+" from "+string(from), func(t *testing.T) { checkTransition(t, s, tr, from) })
		}
		if err := tr.run(s, "M-999"); !errors.Is(err, store.ErrNoMessage) {
			t.Errorf("%s of a missing message = %v, want ErrNoMessage", tr.name, err)
		}
	}
	// queued -> delivered by applied sets the path, the result and the time.
	m := inStatus(t, s, model.MsgDelivered)
	if m.DeliveredVia != model.ViaApplied || m.ResultRef != "T-1" || !m.DeliveredAt.Equal(tClaim) {
		t.Errorf("applied = via %q ref %q at %v", m.DeliveredVia, m.ResultRef, m.DeliveredAt)
	}
	// An instruction is delivered to its session, never applied.
	ins := queuedMessage(t, s, "use pnpm", "s1")
	if _, err := s.ApplyMessage(ctx, ins.ID, "D-1", tClaim); err == nil {
		t.Error("ApplyMessage(instruction) = nil, want an error")
	}
	assertMessage(t, getMessage(t, s, ins.ID), ins)
	// held -> canceled is SweepHeld (TestSweepCancelsOrphanedHeld); queued ->
	// delivered by a claim is covered by the claim tests.
}

// checkTransition runs tr on a message in status from: it moves the message
// to tr.to when from is the status tr accepts, and otherwise returns tr.err
// and changes nothing.
func checkTransition(t *testing.T, s *store.Store, tr transition, from model.MessageStatus) {
	t.Helper()
	before := inStatus(t, s, from)
	err := tr.run(s, before.ID)
	if from != tr.from {
		if !errors.Is(err, tr.err) {
			t.Errorf("err = %v, want %v", err, tr.err)
		}
		assertMessage(t, getMessage(t, s, before.ID), before)
		return
	}
	if err != nil {
		t.Fatalf("%s: %v", tr.name, err)
	}
	if got := getMessage(t, s, before.ID); got.Status != tr.to {
		t.Errorf("status %s, want %s", got.Status, tr.to)
	}
}

func TestUndoRacesRelease(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 2)
	ctx := context.Background()
	wins := map[string]int{}
	for round := range 200 {
		m := createMessage(t, hs[0], held(fmt.Sprintf("round %d", round), "s1"))
		errs := make([]error, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for k, op := range []func(context.Context, string) (model.Message, error){hs[0].UndoMessage, hs[1].ReleaseMessage} {
			wg.Go(func() {
				<-start
				errs[k] = retryBusy(func() error { _, err := op(ctx, m.ID); return err }, busyAtBegin)
			})
		}
		close(start)
		wg.Wait()
		got := getMessage(t, hs[0], m.ID)
		switch {
		case errs[0] == nil && errors.Is(errs[1], store.ErrNotHeld) && got.Status == model.MsgUndone:
			wins["undo"]++
		case errs[1] == nil && errors.Is(errs[0], store.ErrNotHeld) && got.Status == model.MsgQueued:
			wins["release"]++
		default:
			t.Fatalf("round %d: undo %v, release %v, stored %s; want exactly one winner", round, errs[0], errs[1], got.Status)
		}
	}
	t.Logf("winners: %v", wins)
}

func TestSweepCancelsOrphanedHeld(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	at := func(ago time.Duration) model.Message {
		m := held(fmt.Sprintf("held until %v ago", ago), "s1")
		m.HoldUntil = now.Add(-ago)
		return createMessage(t, s, m)
	}
	old, older := at(31*time.Second), at(time.Hour)
	edge := at(30 * time.Second) // hold_until + 30 s == now
	recent := at(29 * time.Second)
	queued := queuedMessage(t, s, "queued", "s1")
	n, err := s.SweepHeld(ctx, now)
	if err != nil || n != 3 {
		t.Fatalf("SweepHeld = %d, %v; want 3", n, err)
	}
	for _, m := range []model.Message{old, older, edge} {
		assertMessageStatus(t, s, m.ID, model.MsgCanceled, "")
	}
	assertMessageStatus(t, s, recent.ID, model.MsgHeld, "")
	assertMessageStatus(t, s, queued.ID, model.MsgQueued, "")
	if n, err := s.SweepHeld(ctx, now); err != nil || n != 0 {
		t.Errorf("second sweep = %d, %v; want 0", n, err)
	}
}

// typingCase is one row of TestClaimMessageTypingRefusals.
type typingCase struct {
	name      string
	state     model.State // "" for no session row
	pane      string
	subagents int
	peer      model.State // a second live row in the same pane; "" for none
	msg       func(model.Message) model.Message
	why       store.TypingRefusal
}

func TestClaimMessageTypingRefusals(t *testing.T) {
	cases := []typingCase{
		{name: "idle", state: model.StateIdle, pane: "%3", why: store.TypingClaimed},
		{name: "needs_decision", state: model.StateNeedsDecision, pane: "%3", why: store.TypingClaimed},
		{name: "working", state: model.StateWorking, pane: "%3", why: store.TypingWorking},
		{name: "waiting", state: model.StateWaiting, pane: "%3", why: store.TypingWaiting},
		{name: "ended", state: model.StateEnded, pane: "%3", why: store.TypingEnded},
		{name: "no pane", state: model.StateIdle, why: store.TypingNoPane},
		{name: "stale", state: model.StateStale, pane: "%3", why: store.TypingNoPane},
		{name: "no session row", why: store.TypingNoPane},
		{name: "subagents", state: model.StateIdle, pane: "%3", subagents: 1, why: store.TypingSubagents},
		{name: "shared pane", state: model.StateIdle, pane: "%3", peer: model.StateWaiting, why: store.TypingSharedPane},
		{
			name: "not queued", state: model.StateIdle, pane: "%3", why: store.TypingNotQueued,
			msg: func(m model.Message) model.Message {
				m.Status, m.HoldUntil = model.MsgHeld, t0.Add(5*time.Second)
				return m
			},
		},
		{
			name: "kind rule", state: model.StateIdle, pane: "%3", why: store.TypingNotQueued,
			msg: func(m model.Message) model.Message { m.Kind = model.KindRule; return m },
		},
		{
			name: "no target session", state: model.StateIdle, pane: "%3", why: store.TypingNotQueued,
			msg: func(m model.Message) model.Message { m.TargetSession = ""; return m },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkClaimMessageTyping(t, c) })
	}
	s := open(t, dbPath(t))
	if c := claimMessageTyping(t, s, "M-9", tClaim); c.Refusal != store.TypingNotQueued {
		t.Errorf("missing message = %v, want TypingNotQueued", c.Refusal)
	}
}

// checkClaimMessageTyping sets up c's session and message, claims the
// message for typing and checks the refusal, the rows returned and stored.
func checkClaimMessageTyping(t *testing.T, c typingCase) {
	t.Helper()
	s := open(t, dbPath(t))
	var sess model.Session
	if c.state != "" {
		sess = model.Session{ID: "s1", Agent: "claude", Pane: c.pane, State: c.state, SubagentsActive: c.subagents, UpdatedAt: t0}
		upsert(t, s, sess)
	}
	if c.peer != "" {
		liveSession(t, s, "s2", c.pane, c.peer)
	}
	m := instruction("use pnpm, not npm, in this repo", "s1")
	if c.msg != nil {
		m = c.msg(m)
	}
	m = createMessage(t, s, m)
	got := claimMessageTyping(t, s, m.ID, tClaim)
	if got.Refusal != c.why {
		t.Fatalf("refusal = %v, want %v", got.Refusal, c.why)
	}
	if m.TargetSession != "" && !reflect.DeepEqual(got.Session, sess) {
		t.Errorf("session:\n got %+v\nwant %+v", got.Session, sess)
	}
	if c.why != store.TypingClaimed {
		assertMessage(t, getMessage(t, s, m.ID), m)
		assertMessage(t, got.Message, m)
		return
	}
	stored := assertMessageStatus(t, s, m.ID, model.MsgDelivered, model.ViaSendKeys)
	if !stored.DeliveredAt.Equal(tClaim) || !stored.ConfirmedAt.IsZero() {
		t.Errorf("typed at %v confirmed %v; want at %v, unconfirmed", stored.DeliveredAt, stored.ConfirmedAt, tClaim)
	}
	assertMessage(t, got.Message, stored)
	if again := claimMessageTyping(t, s, m.ID, tClaim.Add(time.Second)); again.Refusal != store.TypingNotQueued {
		t.Errorf("second claim = %v, want TypingNotQueued", again.Refusal)
	}
}

func TestRequeueMessageOnlyUndoesTyping(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateIdle)
	liveSession(t, s, "s2", "%4", model.StateIdle)
	typed := queuedMessage(t, s, "typed", "s1")
	claimMessageTyping(t, s, typed.ID, tClaim)
	if err := s.RequeueMessage(ctx, typed.ID); err != nil {
		t.Fatal(err)
	}
	if got := assertMessageStatus(t, s, typed.ID, model.MsgQueued, ""); !got.DeliveredAt.IsZero() {
		t.Errorf("requeued message delivered at %v, want zero", got.DeliveredAt)
	}
	// A requeued typing blocks nothing.
	if c := claimMessageTyping(t, s, typed.ID, tClaim); c.Refusal != store.TypingClaimed {
		t.Fatalf("claim after requeue = %v", c.Refusal)
	}
	// Read by check_messages since the claim: left delivered.
	if _, err := s.CheckMessages(ctx, "s1", tClaim.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	stopped := queuedMessage(t, s, "stopped", "s2")
	stopClaim(t, s, "s2", tClaim, 4000)
	waiting := queuedMessage(t, s, "waiting", "s1")
	for _, id := range []string{typed.ID, stopped.ID, waiting.ID, "M-99"} {
		if err := s.RequeueMessage(ctx, id); err != nil {
			t.Errorf("RequeueMessage(%s): %v", id, err)
		}
	}
	assertMessageStatus(t, s, typed.ID, model.MsgDelivered, model.ViaSendKeys)
	assertMessageStatus(t, s, stopped.ID, model.MsgDelivered, model.ViaStopHook)
	assertMessageStatus(t, s, waiting.ID, model.MsgQueued, "")
}

func TestCheckMessagesClaimsAndConfirms(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateIdle)
	earlier := queuedMessage(t, s, "typed and already confirmed", "s1")
	claimMessageTyping(t, s, earlier.ID, tClaim)
	confirmMessages(t, s, "s1", message.TypedLine(getMessage(t, s, earlier.ID)), tClaim)
	typed := queuedMessage(t, s, "typed, not confirmed", "s1")
	claimMessageTyping(t, s, typed.ID, tClaim)
	first := queuedMessage(t, s, "queued first", "s1")
	other := queuedMessage(t, s, "for s2", "s2")
	heldOne := createMessage(t, s, held("still held", "s1"))
	ruleMsg := instruction("a rule", "s1") // a queued rule: no delivery path takes it
	ruleMsg.Kind = model.KindRule
	rule := createMessage(t, s, ruleMsg)
	second := queuedMessage(t, s, "queued second", "s1")
	stopped := queuedMessage(t, s, "stopped", "s3")
	stopClaim(t, s, "s3", tClaim, 4000)

	for _, caller := range []string{"", "s4"} {
		if got, err := s.CheckMessages(ctx, caller, tClaim); err != nil || len(got) != 0 {
			t.Errorf("CheckMessages(%q) = %v, %v; want none", caller, messageIDs(got), err)
		}
	}
	at := tClaim.Add(time.Minute)
	got, err := s.CheckMessages(ctx, "s1", at)
	if err != nil {
		t.Fatal(err)
	}
	// Ruling 105: a typed message, confirmed or not, is returned again, as
	// stored, so its full wording stays readable to its recipient.
	if ids, want := messageIDs(got), []string{earlier.ID, typed.ID, first.ID, second.ID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("CheckMessages(s1) = %v, want %v", ids, want)
	}
	for _, m := range got {
		assertMessage(t, m, getMessage(t, s, m.ID))
	}
	assertTimes(t, assertMessageStatus(t, s, first.ID, model.MsgDelivered, model.ViaCheck), at, at)
	assertTimes(t, assertMessageStatus(t, s, second.ID, model.MsgDelivered, model.ViaCheck), at, at)
	assertTimes(t, assertMessageStatus(t, s, typed.ID, model.MsgDelivered, model.ViaSendKeys), tClaim, at)
	assertTimes(t, getMessage(t, s, earlier.ID), tClaim, tClaim) // its confirmation stays
	assertMessageStatus(t, s, other.ID, model.MsgQueued, "")
	assertMessageStatus(t, s, heldOne.ID, model.MsgHeld, "")
	assertMessageStatus(t, s, rule.ID, model.MsgQueued, "")
	assertMessageStatus(t, s, stopped.ID, model.MsgDelivered, model.ViaStopHook)

	// Asking again claims nothing new and still returns the typed ones.
	again, err := s.CheckMessages(ctx, "s1", at.Add(time.Minute))
	if ids, want := messageIDs(again), []string{earlier.ID, typed.ID}; err != nil || !reflect.DeepEqual(ids, want) {
		t.Errorf("second CheckMessages = %v, %v; want %v", ids, err, want)
	}
	if c := claimMessageTyping(t, s, queuedMessage(t, s, "next", "s1").ID, at); c.Refusal != store.TypingClaimed {
		t.Errorf("claim after check_messages confirmed = %v, want claimed", c.Refusal)
	}
}

// assertTimes fails unless m was delivered at delivered and confirmed at
// confirmed.
func assertTimes(t *testing.T, m model.Message, delivered, confirmed time.Time) {
	t.Helper()
	if !m.DeliveredAt.Equal(delivered) || !m.ConfirmedAt.Equal(confirmed) {
		t.Errorf("%s: delivered %v confirmed %v, want %v and %v", m.ID, m.DeliveredAt, m.ConfirmedAt, delivered, confirmed)
	}
}

// msgRacers are the four outcomes TestMessageClaimedExactlyOnce races.
var msgRacers = []string{"send_keys", "stop_hook", "check", "cancel"}

// msgRace runs ClaimMessageTyping, a Stop claim inside UpdateSessionTx,
// CheckMessages and CancelMessage for message id of session s1 at once, from
// hs[1] to hs[4], led by racer lead as claimRace leads (the Stop claim
// releases the others from inside its transaction), and returns the
// outcomes that took the message.
func msgRace(t *testing.T, hs []*store.Store, id string, lead int) []string {
	t.Helper()
	ctx := context.Background()
	took := make([]bool, len(msgRacers))
	errs := make([]error, len(msgRacers))
	ahead := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ahead) }) }
	race := []func() error{
		func() error {
			c, err := hs[1].ClaimMessageTyping(ctx, id, tClaim)
			took[0] = c.Refusal == store.TypingClaimed
			if err == nil && c.Refusal != store.TypingClaimed && c.Refusal != store.TypingNotQueued {
				return fmt.Errorf("typing refused: %v", c.Refusal)
			}
			return err
		},
		func() error {
			got, err := stopClaimLocked(hs[2], "s1", tClaim, 4000, func() {
				if lead == 1 {
					release()
				}
			})
			took[1] = slices.ContainsFunc(got, func(m model.Message) bool { return m.ID == id })
			return err
		},
		func() error {
			got, err := hs[3].CheckMessages(ctx, "s1", tClaim)
			took[2] = slices.ContainsFunc(got, func(m model.Message) bool { return m.ID == id && m.DeliveredVia == model.ViaCheck })
			return err
		},
		func() error {
			_, err := hs[4].CancelMessage(ctx, id)
			took[3] = err == nil
			if errors.Is(err, store.ErrNotQueued) {
				return nil
			}
			return err
		},
	}
	runLed(race, errs, lead, lead != 1, ahead, release)
	var won []string
	for k, name := range msgRacers {
		if errs[k] != nil {
			t.Errorf("%s: %s: %v", id, name, errs[k])
		}
		if took[k] {
			won = append(won, name)
		}
	}
	return won
}

// msgRound queues one message, races for it with lead leading and returns
// the one outcome that took it, failing the test unless exactly one did and
// the stored row agrees. A typed line is then confirmed, so the next round
// may type.
func msgRound(t *testing.T, hs []*store.Store, round, lead int) string {
	t.Helper()
	m := queuedMessage(t, hs[0], fmt.Sprintf("delivered in round %d", round), "s1")
	won := msgRace(t, hs, m.ID, lead)
	got := getMessage(t, hs[0], m.ID)
	if len(won) != 1 {
		t.Fatalf("%s: taken by %v, want exactly one (stored %s via %q)", m.ID, won, got.Status, got.DeliveredVia)
	}
	want, via := model.MsgDelivered, won[0]
	if via == "cancel" {
		want, via = model.MsgCanceled, ""
	}
	if got.Status != want || string(got.DeliveredVia) != via {
		t.Fatalf("%s: stored %s via %q, want the winner %s", m.ID, got.Status, got.DeliveredVia, won[0])
	}
	if got.DeliveredVia == model.ViaSendKeys && got.ConfirmedAt.IsZero() {
		confirmMessages(t, hs[0], "s1", message.TypedLine(got), tClaim)
	}
	return won[0]
}

// Delivery at most once: the three claims and a cancel race for one queued
// message, and exactly one of them takes it, every round.
func TestMessageClaimedExactlyOnce(t *testing.T) {
	const minRounds, maxRounds = 200, 2000
	path := dbPath(t)
	hs := handles(t, path, 5)
	liveSession(t, hs[0], "s1", "%3", model.StateIdle)
	wins := map[string]int{}
	missing := func() int {
		for k, name := range msgRacers {
			if wins[name] == 0 {
				return k
			}
		}
		return -1
	}
	round := 0
	for ; round < maxRounds && (round < minRounds || missing() >= 0); round++ {
		lead := round % len(msgRacers)
		if round >= minRounds {
			lead = missing()
		}
		wins[msgRound(t, hs, round, lead)]++
	}
	t.Logf("winners over %d rounds: %v", round, wins)
	for _, name := range msgRacers {
		if wins[name] == 0 {
			t.Errorf("%s never won in %d rounds", name, round)
		}
	}
}

func TestRouteLabelsNewestFirst(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	add := func(by model.RoutedBy, conf float64, has bool) {
		m := instruction(fmt.Sprintf("routed %s at %v", by, conf), "s1")
		m.RoutedBy, m.Confidence, m.HasConfidence = by, conf, has
		createMessage(t, s, m)
	}
	add(model.RoutedConfirmed, 0.9, true)
	add(model.RoutedOverridden, 0.95, true)
	add(model.RoutedConfirmed, 0.5, true) // under the threshold
	add(model.RoutedAddress, 0.99, true)
	add(model.RoutedAuto, 0.99, true)
	add(model.RoutedManual, 0.99, true)
	add(model.RoutedSend, 0.99, true)
	add(model.RoutedConfirmed, 0, false) // no model was asked
	add(model.RoutedConfirmed, 0.8, true)
	add(model.RoutedOverridden, 0.7, true)
	got, err := s.RouteLabels(ctx, 0.7, 10)
	want := []model.RouteLabel{
		{Confidence: 0.7}, {Confidence: 0.8, Accepted: true}, {Confidence: 0.95}, {Confidence: 0.9, Accepted: true},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("RouteLabels = %v, %v; want %v", got, err, want)
	}
	if got, err := s.RouteLabels(ctx, 0.7, 2); err != nil || !reflect.DeepEqual(got, want[:2]) {
		t.Errorf("RouteLabels limit 2 = %v, %v; want %v", got, err, want[:2])
	}
	if got, err := s.RouteLabels(ctx, 0.7, 0); err != nil || len(got) != 0 {
		t.Errorf("RouteLabels limit 0 = %v, %v; want none", got, err)
	}
	if n, err := s.AutoEligibleCount(ctx); err != nil || n != 0 {
		t.Errorf("AutoEligibleCount = %d, %v; want 0", n, err)
	}
	for range 3 {
		m := instruction("eligible", "s1")
		m.AutoEligible = true
		createMessage(t, s, m)
	}
	if n, err := s.AutoEligibleCount(ctx); err != nil || n != 3 {
		t.Errorf("AutoEligibleCount = %d, %v; want 3", n, err)
	}
}

func TestRoutingStateSetOnceAndReset(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	state := func() (time.Time, time.Time) {
		t.Helper()
		e, tr, err := s.RoutingState(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return e, tr
	}
	if e, tr := state(); !e.IsZero() || !tr.IsZero() {
		t.Fatalf("fresh state = %v, %v; want zero", e, tr)
	}
	earned, tripped := t0.Add(time.Nanosecond), t0.Add(time.Hour+time.Nanosecond)
	for _, set := range []struct {
		f  func(context.Context, time.Time) error
		at time.Time
	}{
		{s.SetRoutingEarned, earned},
		{s.SetRoutingEarned, earned.Add(time.Hour)},
		{s.SetRoutingTripped, tripped},
		{s.SetRoutingTripped, tripped.Add(time.Hour)},
	} {
		if err := set.f(ctx, set.at); err != nil {
			t.Fatal(err)
		}
	}
	if e, tr := state(); !e.Equal(earned) || !tr.Equal(tripped) {
		t.Errorf("state = %v, %v; want the first of each, %v, %v", e, tr, earned, tripped)
	}
	assertAutoOff(t, s, true)
	if err := s.ResetRouting(ctx); err != nil {
		t.Fatal(err)
	}
	if e, tr := state(); !e.IsZero() || !tr.IsZero() {
		t.Errorf("after reset = %v, %v; want zero", e, tr)
	}
	assertAutoOff(t, s, false)
	if err := s.SetRoutingEarned(ctx, tripped); err != nil {
		t.Fatal(err)
	}
	if e, _ := state(); !e.Equal(tripped) {
		t.Errorf("earned after reset = %v, want %v", e, tripped)
	}
}

// assertAutoOff fails unless BadgeExtras reports auto-send off as want.
func assertAutoOff(t *testing.T, s *store.Store, want bool) {
	t.Helper()
	if _, off, err := s.BadgeExtras(context.Background()); err != nil || off != want {
		t.Errorf("BadgeExtras auto off = %v, %v; want %v", off, err, want)
	}
}

func TestBadgeExtras(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for id, state := range map[string]model.State{"live": model.StateIdle, "ended": model.StateEnded, "stale": model.StateStale, "typed": model.StateIdle} {
		liveSession(t, s, id, "%"+id, state)
		queuedMessage(t, s, "for "+id, id)
	}
	queuedMessage(t, s, "for a session with no row", "gone")
	for _, k := range []model.MessageKind{model.KindRule, model.KindTask} {
		m := instruction("a "+string(k), "live")
		m.Kind = k
		createMessage(t, s, m)
	}
	createMessage(t, s, held("held", "live"))
	// typed's message is typed, unconfirmed: still counted.
	typed, err := s.ListMessages(ctx, store.MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range typed {
		if m.TargetSession == "typed" {
			claimMessageTyping(t, s, m.ID, tClaim)
		}
	}
	if n, off, err := s.BadgeExtras(ctx); err != nil || n != 2 || off {
		t.Errorf("BadgeExtras = %d, %v, %v; want 2 (live and typed), auto on", n, off, err)
	}
	stopClaim(t, s, "live", tClaim, 4000)
	if _, err := s.CheckMessages(ctx, "typed", tClaim); err != nil {
		t.Fatal(err)
	}
	if n, _, err := s.BadgeExtras(ctx); err != nil || n != 0 {
		t.Errorf("BadgeExtras after delivery = %d, %v; want 0", n, err)
	}
}

func TestListMessagesFilters(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	a := queuedMessage(t, s, "a", "s1")
	b := createMessage(t, s, held("b", "s1"))
	c := queuedMessage(t, s, "c", "s1")
	for _, tc := range []struct {
		f    store.MessageFilter
		want []string
	}{
		{store.MessageFilter{}, []string{a.ID, b.ID, c.ID}},
		{store.MessageFilter{Statuses: []model.MessageStatus{model.MsgQueued}}, []string{a.ID, c.ID}},
		{store.MessageFilter{Statuses: []model.MessageStatus{model.MsgHeld, model.MsgQueued}, Limit: 2}, []string{c.ID, b.ID}},
		{store.MessageFilter{Statuses: []model.MessageStatus{model.MsgCanceled}}, []string{}},
	} {
		got, err := s.ListMessages(ctx, tc.f)
		if ids := messageIDs(got); err != nil || !reflect.DeepEqual(ids, tc.want) {
			t.Errorf("ListMessages(%+v) = %v, %v; want %v", tc.f, ids, err, tc.want)
		}
	}
	if _, ok, err := s.GetMessage(ctx, "M-9"); err != nil || ok {
		t.Errorf("GetMessage(missing) = %v, %v; want false, nil", ok, err)
	}
}

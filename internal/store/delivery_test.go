package store_test

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// tAnswer is when the delivery tests answer their tickets, and tClaim when
// they claim the answers.
var (
	tAnswer = t0.Add(time.Hour)
	tClaim  = t0.Add(2 * time.Hour)
)

// queued stores a blocking ticket with question from session, answers it
// with option A and returns the row, whose answer is queued for delivery.
func queued(t *testing.T, s *store.Store, question, session string) model.Decision {
	t.Helper()
	d := decide(t, s, ticket(t, question, model.ModeBlocking, session, t0), tAnswer)
	if d.Delivery != model.DeliveryQueued {
		t.Fatalf("setup: %s delivery %s, want queued", d.ID, d.Delivery)
	}
	return d
}

// liveSession stores session id in state in pane and returns it.
func liveSession(t *testing.T, s *store.Store, id, pane string, state model.State) model.Session {
	t.Helper()
	sess := model.Session{ID: id, Agent: "claude", Pane: pane, State: state, UpdatedAt: t0}
	upsert(t, s, sess)
	return sess
}

// assertQueued fails unless decision id is still queued and unclaimed.
func assertQueued(t *testing.T, s *store.Store, id string) {
	t.Helper()
	got := getDecision(t, s, id)
	if got.Delivery != model.DeliveryQueued || got.DeliveredVia != "" || !got.DeliveredAt.IsZero() {
		t.Errorf("%s: delivery %q via %q at %v, want queued and unclaimed", id, got.Delivery, got.DeliveredVia, got.DeliveredAt)
	}
}

// assertDelivered fails unless decision id was delivered via at at.
func assertDelivered(t *testing.T, s *store.Store, id string, via model.DeliveryVia, at time.Time) {
	t.Helper()
	got := getDecision(t, s, id)
	if got.Delivery != model.DeliveryDelivered || got.DeliveredVia != via || !got.DeliveredAt.Equal(at) {
		t.Errorf("%s: delivery %q via %q at %v, want delivered via %s at %v",
			id, got.Delivery, got.DeliveredVia, got.DeliveredAt, via, at)
	}
}

// claimStop claims session's queued answers the way curia hook Stop does,
// inside UpdateSessionTx, and returns them.
func claimStop(t *testing.T, s *store.Store, session string, now time.Time) []model.Decision {
	t.Helper()
	got, err := claimStopErr(s, session, now)
	if err != nil {
		t.Fatalf("claim for %s: %v", session, err)
	}
	return got
}

func claimStopErr(s *store.Store, session string, now time.Time) ([]model.Decision, error) {
	return claimStopLocked(s, session, now, func() {})
}

// claimStopLocked is claimStopErr calling locked first inside fn, where
// the transaction holds the write lock.
func claimStopLocked(s *store.Store, session string, now time.Time, locked func()) ([]model.Decision, error) {
	var claimed []model.Decision
	err := s.UpdateSessionTx(context.Background(), session, func(tx *store.Tx, sess model.Session, found bool) (model.Session, error) {
		locked()
		var err error
		claimed, err = tx.ClaimDeliveries(context.Background(), session, now)
		if !found {
			sess = model.Session{ID: session, Agent: "claude", State: model.StateIdle}
		}
		return sess, err
	})
	return claimed, err
}

func TestClaimTypingRequiresIdleOrNeedsDecision(t *testing.T) {
	cases := []struct {
		name  string
		state model.State // "" for no session row
		pane  string
		ok    bool
	}{
		{"idle", model.StateIdle, "%3", true},
		{"needs_decision", model.StateNeedsDecision, "%3", true},
		{"working", model.StateWorking, "%3", false},
		{"waiting", model.StateWaiting, "%3", false},
		{"stale", model.StateStale, "%3", false},
		{"ended", model.StateEnded, "%3", false},
		{"no pane", model.StateIdle, "", false},
		{"no session row", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkClaimTyping(t, c.state, c.pane, c.ok) })
	}
}

// checkClaimTyping claims a queued answer of session s1, stored in state
// (no row when state is "") with pane, and checks the outcome is wantOK.
func checkClaimTyping(t *testing.T, state model.State, pane string, wantOK bool) {
	t.Helper()
	s := open(t, dbPath(t))
	var sess model.Session
	if state != "" {
		sess = liveSession(t, s, "s1", pane, state)
	}
	d := queued(t, s, "Type the answer?", "s1")
	if state == model.StateNeedsDecision {
		// AnswerDecision cleared needs_decision; put it back.
		sess = liveSession(t, s, "s1", pane, state)
	}
	got, gotSess, ok, err := s.ClaimTyping(context.Background(), d.ID, tClaim)
	if err != nil {
		t.Fatal(err)
	}
	if ok != wantOK {
		t.Fatalf("ok = %v, want %v", ok, wantOK)
	}
	if !reflect.DeepEqual(gotSess, sess) {
		t.Errorf("session:\n got %+v\nwant %+v", gotSess, sess)
	}
	if !wantOK {
		assertQueued(t, s, d.ID)
		return
	}
	assertDelivered(t, s, d.ID, model.ViaSendKeys, tClaim)
	assertDecision(t, got, getDecision(t, s, d.ID))
	if _, _, again, err := s.ClaimTyping(context.Background(), d.ID, tClaim.Add(time.Second)); err != nil || again {
		t.Errorf("second claim = %v, %v; want false, nil", again, err)
	}
	assertDelivered(t, s, d.ID, model.ViaSendKeys, tClaim)
}

func TestClaimTypingNothingToClaim(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	sess := liveSession(t, s, "s1", "%3", model.StateIdle)
	if _, gotSess, ok, err := s.ClaimTyping(ctx, "D-99", tClaim); err != nil || ok || !reflect.DeepEqual(gotSess, model.Session{}) {
		t.Errorf("missing decision = %+v, %v, %v; want zero session, false, nil", gotSess, ok, err)
	}
	d := create(t, s, ticket(t, "Still open?", model.ModeBlocking, "s1", t0))
	got, gotSess, ok, err := s.ClaimTyping(ctx, d.ID, tClaim)
	if err != nil || ok {
		t.Fatalf("open ticket = %v, %v; want false, nil", ok, err)
	}
	if !reflect.DeepEqual(gotSess, sess) || got.Delivery != model.DeliveryNone {
		t.Errorf("open ticket: session %+v delivery %s; want the stored session and none", gotSess, got.Delivery)
	}
}

// Ruling 73: the claim succeeds only when the asking session is the one
// session row bound to its pane that is neither ended nor stale.
func TestClaimTypingRefusesSharedPane(t *testing.T) {
	cases := []struct {
		name  string
		other model.State // state of a second row in pane %3; "" for none
		pane  string      // the second row's pane
		ok    bool
	}{
		{"alone", "", "", true},
		{"working peer", model.StateWorking, "%3", false},
		{"waiting peer", model.StateWaiting, "%3", false},
		{"idle peer", model.StateIdle, "%3", false},
		{"needs_decision peer", model.StateNeedsDecision, "%3", false},
		{"ended peer", model.StateEnded, "%3", true},
		{"stale peer", model.StateStale, "%3", true},
		{"peer in another pane", model.StateWaiting, "%4", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := open(t, dbPath(t))
			sess := liveSession(t, s, "s1", "%3", model.StateIdle)
			if c.other != "" {
				liveSession(t, s, "s2", c.pane, c.other)
			}
			d := queued(t, s, "Type the answer?", "s1")
			_, gotSess, ok, err := s.ClaimTyping(context.Background(), d.ID, tClaim)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !reflect.DeepEqual(gotSess, sess) {
				t.Errorf("session:\n got %+v\nwant %+v", gotSess, sess)
			}
			if c.ok {
				assertDelivered(t, s, d.ID, model.ViaSendKeys, tClaim)
			} else {
				assertQueued(t, s, d.ID)
			}
		})
	}
}

func TestRequeueOnlyUndoesTyping(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateIdle)
	liveSession(t, s, "s2", "%4", model.StateIdle)

	typed := queued(t, s, "Typed?", "s1")
	if _, _, ok, err := s.ClaimTyping(ctx, typed.ID, tClaim); err != nil || !ok {
		t.Fatalf("ClaimTyping = %v, %v", ok, err)
	}
	if err := s.RequeueDelivery(ctx, typed.ID); err != nil {
		t.Fatal(err)
	}
	assertQueued(t, s, typed.ID)

	stopped := queued(t, s, "Stopped?", "s2")
	claimStop(t, s, "s2", tClaim)
	checked := queued(t, s, "Checked?", "s1")
	if _, _, err := s.CheckDecision(ctx, checked.ID, "s1", tClaim); err != nil {
		t.Fatal(err)
	}
	waiting := queued(t, s, "Waiting?", "s1")
	for _, id := range []string{stopped.ID, checked.ID, waiting.ID, "D-99"} {
		if err := s.RequeueDelivery(ctx, id); err != nil {
			t.Errorf("RequeueDelivery(%s): %v", id, err)
		}
	}
	assertDelivered(t, s, stopped.ID, model.ViaStopHook, tClaim)
	assertDelivered(t, s, checked.ID, model.ViaCheck, tClaim)
	assertQueued(t, s, waiting.ID)
}

func TestCheckDecisionClaimsForOwnerOnly(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	d := queued(t, s, "Checked by its owner?", "s1")
	for _, other := range []string{"s2", ""} {
		got, found, err := s.CheckDecision(ctx, d.ID, other, tClaim)
		if err != nil || !found {
			t.Fatalf("CheckDecision(%s, %q) = %v, %v", d.ID, other, found, err)
		}
		assertDecision(t, got, d)
		assertQueued(t, s, d.ID)
	}
	got, found, err := s.CheckDecision(ctx, d.ID, "s1", tClaim)
	if err != nil || !found {
		t.Fatalf("CheckDecision by owner = %v, %v", found, err)
	}
	assertDelivered(t, s, d.ID, model.ViaCheck, tClaim)
	assertDecision(t, got, getDecision(t, s, d.ID))
	// Asking again returns the answer already delivered, unchanged.
	again, _, err := s.CheckDecision(ctx, d.ID, "s1", tClaim.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, again, got)

	openRow := create(t, s, ticket(t, "Not answered yet?", model.ModeBlocking, "s1", t0))
	got, found, err = s.CheckDecision(ctx, openRow.ID, "s1", tClaim)
	if err != nil || !found {
		t.Fatalf("CheckDecision(open) = %v, %v", found, err)
	}
	assertDecision(t, got, openRow)

	if _, found, err := s.CheckDecision(ctx, "D-99", "s1", tClaim); err != nil || found {
		t.Errorf("CheckDecision(missing) = %v, %v; want false, nil", found, err)
	}
}

func TestTxClaimDeliveriesReturnsAndMarks(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	a := queued(t, s, "First for s1?", "s1")
	other := queued(t, s, "For s2?", "s2")
	b := queued(t, s, "Second for s1?", "s1")
	got := claimStop(t, s, "s1", tClaim)
	if ids, want := decisionIDs(got), []string{a.ID, b.ID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("claimed %v, want %v", ids, want)
	}
	for _, d := range got {
		assertDecision(t, d, getDecision(t, s, d.ID))
		assertDelivered(t, s, d.ID, model.ViaStopHook, tClaim)
	}
	assertQueued(t, s, other.ID)
	if again := claimStop(t, s, "s1", tClaim); len(again) != 0 {
		t.Errorf("second claim took %v, want nothing", decisionIDs(again))
	}

	// An empty session id claims nothing, even a queued row with no session.
	rawExec(t, path, `UPDATE decisions SET session_id = '' WHERE id = ?`, other.ID)
	if got := claimStop(t, s, "", tClaim); len(got) != 0 {
		t.Errorf("claim for \"\" took %v, want nothing", decisionIDs(got))
	}
	assertQueued(t, s, other.ID)
}

func TestTxClaimDeliveriesMixedStatusesInIDOrder(t *testing.T) {
	s := open(t, dbPath(t))
	// The overridden row comes first by id but sorts after answered by
	// status, the order the (session_id, status) index reads them in.
	over := answer(t, s, create(t, s, ticket(t, "Assumed wrong?", model.ModeAssume, "s1", t0)).ID,
		model.Answer{Key: "B", Via: model.AnsweredCLI}, tAnswer)
	ans := queued(t, s, "Asked plainly?", "s1")
	over2 := answer(t, s, create(t, s, ticket(t, "Assumed wrong again?", model.ModeAssume, "s1", t0)).ID,
		model.Answer{Text: "Use the existing queue.", Via: model.AnsweredCLI}, tAnswer)
	if over.Status != model.StatusOverridden || over2.Status != model.StatusOverridden {
		t.Fatalf("setup: statuses %s, %s, want overridden", over.Status, over2.Status)
	}
	got := claimStop(t, s, "s1", tClaim)
	if ids, want := decisionIDs(got), []string{over.ID, ans.ID, over2.ID}; !reflect.DeepEqual(ids, want) {
		t.Errorf("claimed %v, want id order %v", ids, want)
	}
}

func TestTxOpenBlockingCounts(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	now := t0.Add(time.Hour)
	create(t, s, ticket(t, "Blocking one?", model.ModeBlocking, "s1", t0))
	create(t, s, ticket(t, "Blocking two?", model.ModeBlocking, "s1", t0))
	past := create(t, s, ticket(t, "Blocking past its deadline?", model.ModeBlocking, "s1", t0))
	rawExec(t, path, `UPDATE decisions SET expires_at = ? WHERE id = ?`, "2026-11-12T09:30:00.000000000Z", past.ID)
	create(t, s, ticket(t, "Assume one?", model.ModeAssume, "s1", t0))
	queued(t, s, "Blocking answered?", "s1")
	create(t, s, ticket(t, "Blocking for s2?", model.ModeBlocking, "s2", t0))

	counts := map[string]int{}
	err := s.UpdateSessionTx(context.Background(), "s1", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
		for _, id := range []string{"s1", "s2", "s3", ""} {
			n, err := tx.OpenBlocking(context.Background(), id, now)
			if err != nil {
				return sess, err
			}
			counts[id] = n
		}
		sess.ID, sess.Agent, sess.State = "s1", "claude", model.StateIdle
		return sess, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"s1": 2, "s2": 1, "s3": 0, "": 0}; !reflect.DeepEqual(counts, want) {
		t.Errorf("counts %v, want %v", counts, want)
	}
}

// claimRacers are the three delivery paths in the order claimRace runs them.
var claimRacers = []model.DeliveryVia{model.ViaSendKeys, model.ViaStopHook, model.ViaCheck}

// claimRace runs ClaimTyping, a Stop claim inside UpdateSessionTx and
// CheckDecision for decision id of session s1 at once, each from its own
// handle at its own time, and returns the paths that claimed it. Racer
// lead (an index into claimRacers) is released first and the other two
// once it is under way: the Stop claim releases them from inside its fn,
// when it holds the write lock, so it wins by construction; typing and
// check release them just before they call the store. The other two then
// race the leader and each other for real.
func claimRace(t *testing.T, hs []*store.Store, id string, lead int) []model.DeliveryVia {
	t.Helper()
	ctx := context.Background()
	at := func(k int) time.Time { return tClaim.Add(time.Duration(k+1) * time.Nanosecond) }
	claimed := make([]bool, len(claimRacers))
	errs := make([]error, len(claimRacers))
	ahead := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(ahead) }) }
	race := []func() error{
		func() error {
			_, _, ok, err := hs[0].ClaimTyping(ctx, id, at(0))
			claimed[0] = ok
			return err
		},
		func() error {
			got, err := claimStopLocked(hs[1], "s1", at(1), func() {
				if lead == 1 {
					release()
				}
			})
			claimed[1] = len(got) == 1 && got[0].ID == id
			if len(got) > 1 {
				return fmt.Errorf("stop claimed %v", decisionIDs(got))
			}
			return err
		},
		func() error {
			got, _, err := hs[2].CheckDecision(ctx, id, "s1", at(2))
			claimed[2] = got.DeliveredVia == model.ViaCheck && got.DeliveredAt.Equal(at(2))
			return err
		},
	}
	runLed(race, errs, lead, lead != 1, ahead, release)
	var won []model.DeliveryVia
	for k, via := range claimRacers {
		if errs[k] != nil {
			t.Errorf("%s: %s: %v", id, via, errs[k])
		}
		if claimed[k] {
			won = append(won, via)
		}
	}
	return won
}

// runLed runs each of race in its own goroutine and stores its error in
// errs. Racer lead runs first, calling release just before it starts when
// releaseFirst is set (else it must call release itself) and in any case
// when it ends; the others wait for ahead, which release closes.
func runLed(race []func() error, errs []error, lead int, releaseFirst bool, ahead <-chan struct{}, release func()) {
	var wg sync.WaitGroup
	for k, run := range race {
		wg.Go(func() {
			if k == lead {
				defer release() // also when the leader fails before releasing
				if releaseFirst {
					release()
				}
			} else {
				<-ahead
			}
			// BUSY at BEGIN means nothing ran, so the attempt is retried.
			errs[k] = retryBusy(run, busyBeforeAnything)
		})
	}
	wg.Wait()
}

// busyBeforeAnything reports whether err is SQLITE_BUSY from starting a
// write transaction, which ran nothing: "begin" for the decision
// operations, "update session" (UpdateSession's wording) for the Stop
// claim, whose fn sets no error of its own when BEGIN fails.
func busyBeforeAnything(err error) bool {
	return busyAtBegin(err) || (isBusy(err) && strings.HasPrefix(err.Error(), "store: update session "))
}

func TestDeliveryClaimedExactlyOnce(t *testing.T) {
	// 300 rounds, each led by the next path in turn, then, only if some path
	// has not yet won, rounds led by that path up to a cap. A Stop leader
	// holds the write lock when the others start, so it always wins; a
	// typing or check leader has a head start, which wins every round it
	// leads on one CPU and still some rounds on a loaded multi-core run.
	// Every round must have exactly one winner.
	const minRounds, maxRounds = 300, 3000
	path := dbPath(t)
	hs := handles(t, path, 3)
	liveSession(t, hs[0], "s1", "%3", model.StateIdle)
	wins := map[model.DeliveryVia]int{}
	missing := func() int {
		for k, via := range claimRacers {
			if wins[via] == 0 {
				return k
			}
		}
		return -1
	}
	round := 0
	for ; round < maxRounds && (round < minRounds || missing() >= 0); round++ {
		lead := round % len(claimRacers)
		if round >= minRounds {
			lead = missing()
		}
		d := queued(t, hs[0], fmt.Sprintf("Delivered in round %d?", round), "s1")
		won := claimRace(t, hs, d.ID, lead)
		got := getDecision(t, hs[0], d.ID)
		if len(won) != 1 {
			t.Fatalf("%s: claimed by %v, want exactly one (stored via %q)", d.ID, won, got.DeliveredVia)
		}
		wins[won[0]]++
		if got.Delivery != model.DeliveryDelivered || got.DeliveredVia != won[0] {
			t.Fatalf("%s: stored %s via %q, want delivered via the winner %s", d.ID, got.Delivery, got.DeliveredVia, won[0])
		}
	}
	t.Logf("winners over %d rounds: %v", round, wins)
	for _, via := range claimRacers {
		if wins[via] == 0 {
			t.Errorf("%s never won in %d rounds", via, round)
		}
	}
}

// cloneQueued adds count copies of the queued row src, each with the next
// id, in one transaction on a raw connection (faster than answering each).
func cloneQueued(t *testing.T, path string, src model.Decision, count int) {
	t.Helper()
	db := rawDB(t, path)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		_, err := tx.Exec(`
INSERT INTO decisions (id, session_id, task_id, repo, question, context, options, recommendation, rationale,
  reversibility, mode, assumption, scope, tags, status, answer_key, answer, answer_reasoning, answered_via,
  args_hash, delivery, created_at, answered_at)
SELECT 'D-' || (SELECT MAX(n) + 1 FROM decisions), session_id, task_id, repo, question, context, options,
  recommendation, rationale, reversibility, mode, assumption, scope, tags, status, answer_key, answer,
  answer_reasoning, answered_via, args_hash || ?, delivery, created_at, answered_at
FROM decisions WHERE id = ?`, fmt.Sprintf("-%d", i), src.ID)
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimDeliveriesCappedAcrossStops(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	total := 2*store.ClaimCap + 5
	cloneQueued(t, path, queued(t, s, "One of many?", "s1"), total-1)
	var sizes []int
	var all []model.Decision
	for range 4 {
		got := claimStop(t, s, "s1", tClaim)
		sizes = append(sizes, len(got))
		all = append(all, got...)
	}
	if want := []int{store.ClaimCap, store.ClaimCap, 5, 0}; !reflect.DeepEqual(sizes, want) {
		t.Errorf("batch sizes %v, want %v", sizes, want)
	}
	assertEachOnceInOrder(t, s, all, total)
}

func TestClaimDeliveriesConcurrentStopsDeliverEachOnce(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 3)
	total := 2*store.ClaimCap + 5
	cloneQueued(t, path, queued(t, hs[0], "One of many at once?", "s1"), total-1)
	batches := make([][]model.Decision, len(hs))
	errs := make([]error, len(hs))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for k, h := range hs {
		wg.Go(func() {
			<-start
			batches[k], errs[k] = drainStops(h, total)
		})
	}
	close(start)
	wg.Wait()
	var all []model.Decision
	for k := range hs {
		if errs[k] != nil {
			t.Errorf("claimer %d: %v", k, errs[k])
		}
		all = append(all, batches[k]...)
	}
	slices.SortFunc(all, func(a, b model.Decision) int { return cmp.Compare(a.N, b.N) })
	assertEachOnceInOrder(t, hs[0], all, total)
}

// drainStops claims session s1's answers through h until a claim finds
// none, and returns all it claimed. It fails a claim over the cap, and
// stops once it holds more than total, which only a double claim can do.
func drainStops(h *store.Store, total int) ([]model.Decision, error) {
	var all []model.Decision
	for {
		var got []model.Decision
		err := retryBusy(func() error {
			var err error
			got, err = claimStopErr(h, "s1", tClaim)
			return err
		}, busyBeforeAnything)
		switch {
		case err != nil:
			return all, err
		case len(got) > store.ClaimCap:
			return all, fmt.Errorf("one claim took %d, over the cap %d", len(got), store.ClaimCap)
		case len(got) == 0:
			return all, nil
		}
		if all = append(all, got...); len(all) > total {
			return all, fmt.Errorf("claimed %d of %d answers: some more than once", len(all), total)
		}
	}
}

// assertEachOnceInOrder fails unless claimed holds each of the total
// decisions exactly once, in id order, and every one is stored as
// delivered by the Stop hook.
func assertEachOnceInOrder(t *testing.T, s *store.Store, claimed []model.Decision, total int) {
	t.Helper()
	if len(claimed) != total {
		t.Errorf("claimed %d answers, want %d", len(claimed), total)
	}
	for i, d := range claimed {
		if d.N != int64(i+1) {
			t.Fatalf("claim %d is %s, want D-%d: lost, repeated or out of order", i, d.ID, i+1)
		}
	}
	stored, err := s.ListDecisions(context.Background(), store.DecisionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range stored {
		if d.Delivery != model.DeliveryDelivered || d.DeliveredVia != model.ViaStopHook {
			t.Errorf("%s: delivery %s via %q, want delivered via stop_hook", d.ID, d.Delivery, d.DeliveredVia)
		}
	}
}

// longest builds k decided rows whose stop lines are as long as the
// limits allow and whose ids are as long as an id can be (D- and 19
// digits), the worst case for decision.StopReason.
func longest(k int) []model.Decision {
	ds := make([]model.Decision, k)
	for i := range ds {
		ds[i] = model.Decision{
			N:               9223372036854775807 - int64(i),
			Status:          model.StatusOverridden,
			Options:         []model.Option{{Key: "A", Label: strings.Repeat("l", decision.MaxLabel)}},
			AnswerKey:       "A",
			AnswerReasoning: strings.Repeat("r", decision.MaxReasoning),
		}
	}
	return ds
}

func TestClaimCapIsWhatOneStopReasonNames(t *testing.T) {
	named := func(ds []model.Decision) int {
		reason := decision.StopReason(ds)
		if n := utf8.RuneCountInString(reason); n > 4000 {
			t.Fatalf("reason is %d runes, over 4000", n)
		}
		n := 0
		for _, d := range ds {
			if strings.Contains(reason, fmt.Sprintf("D-%d", d.N)) {
				n++
			}
		}
		return n
	}
	if n := named(longest(store.ClaimCap)); n != store.ClaimCap {
		t.Errorf("a reason for %d worst-case answers names %d of them", store.ClaimCap, n)
	}
	// The cap is the most it can name: one more and one goes unnamed.
	if n := named(longest(store.ClaimCap + 1)); n != store.ClaimCap {
		t.Errorf("a reason for %d worst-case answers names %d, want exactly the cap %d", store.ClaimCap+1, n, store.ClaimCap)
	}
}

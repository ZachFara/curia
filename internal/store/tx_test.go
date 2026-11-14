package store_test

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func TestUpdateSessionTxRollsBackOnError(t *testing.T) {
	s := open(t, dbPath(t))
	orig := liveSession(t, s, "s1", "%3", model.StateWorking)
	d := queued(t, s, "Rolled back?", "s1")
	errBoom := errors.New("boom")
	var claimed []model.Decision
	err := s.UpdateSessionTx(context.Background(), "s1", func(tx *store.Tx, sess model.Session, found bool) (model.Session, error) {
		if !found || !reflect.DeepEqual(sess, orig) {
			t.Errorf("fn got (%+v, %v), want the stored session", sess, found)
		}
		var err error
		if claimed, err = tx.ClaimDeliveries(context.Background(), "s1", tClaim); err != nil {
			return sess, err
		}
		sess.State = model.StateIdle
		return sess, errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("UpdateSessionTx = %v, want fn's error", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %v inside fn, want %s", decisionIDs(claimed), d.ID)
	}
	if got := get(t, s, "s1"); !reflect.DeepEqual(got, orig) {
		t.Errorf("session changed:\n got %+v\nwant %+v", got, orig)
	}
	assertQueued(t, s, d.ID)
}

func TestUpdateSessionTxChangedIDRollsBack(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateWorking)
	d := queued(t, s, "Rolled back on a changed id?", "s1")
	err := s.UpdateSessionTx(ctx, "s1", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
		_, err := tx.ClaimDeliveries(ctx, "s1", tClaim)
		sess.ID = "s2"
		return sess, err
	})
	if err == nil {
		t.Fatal("UpdateSessionTx with a changed ID returned nil, want an error")
	}
	assertQueued(t, s, d.ID)
	if _, ok, err := s.GetSession(ctx, "s2"); err != nil || ok {
		t.Errorf("GetSession(s2) = ok %v, err %v; want nothing written", ok, err)
	}
}

func TestTxClaimDeliveriesErrorClaimsNothing(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateWorking)
	good := queued(t, s, "Readable?", "s1")
	bad := queued(t, s, "Unreadable?", "s1")
	// The bad row fails to scan after the UPDATE has marked both.
	rawExec(t, path, `UPDATE decisions SET options = 'not json' WHERE id = ?`, bad.ID)
	var claimErr error
	var claimed []model.Decision
	err := s.UpdateSessionTx(ctx, "s1", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
		claimed, claimErr = tx.ClaimDeliveries(ctx, "s1", tClaim)
		sess.State = model.StateIdle
		return sess, nil // fn ignores the error: the savepoint alone must undo the claim
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimErr == nil || claimed != nil {
		t.Fatalf("ClaimDeliveries = %v, %v; want nil and an error", decisionIDs(claimed), claimErr)
	}
	if got := get(t, s, "s1").State; got != model.StateIdle {
		t.Errorf("session state %s, want idle (the transaction itself committed)", got)
	}
	assertQueued(t, s, good.ID)
	var delivery string
	if err := rawDB(t, path).QueryRow(`SELECT delivery FROM decisions WHERE id = ?`, bad.ID).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if delivery != string(model.DeliveryQueued) {
		t.Errorf("%s delivery %s, want queued", bad.ID, delivery)
	}
}

func TestUpdateSessionStillLosesNoUpdates(t *testing.T) {
	// Plan 1's lost-update load, with half the goroutines going through
	// UpdateSession (which now delegates) and half through UpdateSessionTx
	// running a statement on its Tx.
	path := dbPath(t)
	hs := handles(t, path, 5)
	ctx := context.Background()
	const goroutines, calls = 10, 10
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			s := hs[g%len(hs)]
			for i := range calls {
				var err error
				if g%2 == 0 {
					err = incrementShared(ctx, s)
				} else {
					err = incrementSharedTx(ctx, s)
				}
				if err != nil {
					t.Errorf("goroutine %d call %d: %v", g, i, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := get(t, hs[0], "shared").SubagentsActive; got != goroutines*calls {
		t.Errorf("SubagentsActive = %d, want %d (updates lost)", got, goroutines*calls)
	}
}

// incrementSharedTx is incrementShared through UpdateSessionTx.
func incrementSharedTx(ctx context.Context, s *store.Store) error {
	const maxAttempts = 20
	var err error
	for range maxAttempts {
		ran := false
		err = s.UpdateSessionTx(ctx, "shared", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
			ran = true
			if _, txErr := tx.OpenBlocking(ctx, "shared", t0); txErr != nil {
				return sess, txErr
			}
			sess.ID, sess.Agent, sess.State = "shared", "claude", model.StateWorking
			sess.SubagentsActive++
			return sess, nil
		})
		var se *sqlite.Error
		if err == nil || ran || !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
			return err
		}
	}
	return fmt.Errorf("still busy after %d attempts: %w", maxAttempts, err)
}

func TestReturnedColumnsAreDecisionColumns(t *testing.T) {
	// RETURNING takes no alias, so tx.go repeats the column list by hand.
	got := strings.Fields(store.ReturnedDecision)
	want := strings.Fields(strings.ReplaceAll(store.DecisionColumns, "d.", ""))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("returnedDecision drifted from decisionColumns:\n got %v\nwant %v", got, want)
	}
}

// stopCheck runs StopCheck for session id at now in its own transaction on
// session s1.
func stopCheck(t *testing.T, s *store.Store, id string, now time.Time) (int, bool) {
	t.Helper()
	var n int
	var q bool
	err := s.UpdateSessionTx(context.Background(), "s1", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
		var err error
		n, q, err = tx.StopCheck(context.Background(), id, now)
		sess.ID, sess.Agent = "s1", "claude"
		return sess, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, q
}

func TestTxStopCheck(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	now := t0.Add(time.Hour)
	create(t, s, ticket(t, "Blocking one?", model.ModeBlocking, "s1", t0))
	create(t, s, ticket(t, "Blocking two?", model.ModeBlocking, "s1", t0))
	past := create(t, s, ticket(t, "Blocking past its deadline?", model.ModeBlocking, "s1", t0))
	rawExec(t, path, `UPDATE decisions SET expires_at = ? WHERE id = ?`, "2026-11-12T09:30:00.000000000Z", past.ID)
	create(t, s, ticket(t, "Assume one?", model.ModeAssume, "s1", t0))
	create(t, s, ticket(t, "Blocking for s2?", model.ModeBlocking, "s2", t0))
	delivered := queued(t, s, "Blocking answered and checked?", "s3")
	if _, _, err := s.CheckDecision(context.Background(), delivered.ID, "s3", tClaim); err != nil {
		t.Fatal(err)
	}
	queued(t, s, "Blocking answered?", "s1")

	type facts struct {
		open   int
		queued bool
	}
	got := map[string]facts{}
	for _, id := range []string{"s1", "s2", "s3", "s4", ""} {
		n, q := stopCheck(t, s, id, now)
		got[id] = facts{n, q}
	}
	want := map[string]facts{
		"s1": {2, true},  // the expired, assume and answered tickets are not open
		"s2": {1, false}, // open, nothing answered
		"s3": {0, false}, // answered and already delivered
		"s4": {0, false},
		"":   {0, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StopCheck = %+v, want %+v", got, want)
	}
}

// StopCheck agrees with OpenBlocking, and a claim in the same transaction
// leaves the count as it was and clears queued.
func TestTxStopCheckAroundAClaim(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateWorking)
	create(t, s, ticket(t, "Still open?", model.ModeBlocking, "s1", t0))
	d := queued(t, s, "Answered?", "s1")
	err := s.UpdateSessionTx(ctx, "s1", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
		before, q, err := tx.StopCheck(ctx, "s1", tClaim)
		if err != nil {
			return sess, err
		}
		n, err := tx.OpenBlocking(ctx, "s1", tClaim)
		if err != nil {
			return sess, err
		}
		if before != 1 || !q || n != before {
			t.Errorf("before the claim: StopCheck = %d, %v; OpenBlocking = %d; want 1, true, 1", before, q, n)
		}
		claimed, err := tx.ClaimDeliveries(ctx, "s1", tClaim)
		if err != nil || len(claimed) != 1 || claimed[0].ID != d.ID {
			t.Errorf("ClaimDeliveries = %v, %v; want %s", decisionIDs(claimed), err, d.ID)
		}
		after, q, err := tx.StopCheck(ctx, "s1", tClaim)
		if after != before || q || err != nil {
			t.Errorf("after the claim: StopCheck = %d, %v, %v; want %d, false, nil", after, q, err, before)
		}
		return sess, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// stopClaim claims session's queued messages within budget the way curia
// hook Stop does, inside UpdateSessionTx, and returns them.
func stopClaim(t *testing.T, s *store.Store, session string, now time.Time, budget int) []model.Message {
	t.Helper()
	got, err := stopClaimLocked(s, session, now, budget, func() {})
	if err != nil {
		t.Fatalf("claim messages for %s: %v", session, err)
	}
	return got
}

// stopClaimLocked is stopClaim calling locked first inside fn, where the
// transaction holds the write lock, and returning the error.
func stopClaimLocked(s *store.Store, session string, now time.Time, budget int, locked func()) ([]model.Message, error) {
	var claimed []model.Message
	err := s.UpdateSessionTx(context.Background(), session, func(tx *store.Tx, sess model.Session, found bool) (model.Session, error) {
		locked()
		var err error
		claimed, err = tx.ClaimMessages(context.Background(), session, now, budget)
		if !found {
			sess = model.Session{ID: session, Agent: "claude", State: model.StateIdle}
		}
		return sess, err
	})
	return claimed, err
}

// confirmMessages runs Tx.ConfirmTypedMessages for session as curia hook
// UserPromptSubmit does, inside UpdateSessionTx, and returns the count.
func confirmMessages(t *testing.T, s *store.Store, session, prompt string, at time.Time) int64 {
	t.Helper()
	var n int64
	err := s.UpdateSessionTx(context.Background(), "s1", func(tx *store.Tx, sess model.Session, found bool) (model.Session, error) {
		var err error
		n, err = tx.ConfirmTypedMessages(context.Background(), session, prompt, at)
		if !found {
			sess = model.Session{ID: "s1", Agent: "claude", State: model.StateIdle}
		}
		return sess, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// stopState runs StopState for session id at now in its own transaction.
func stopState(t *testing.T, s *store.Store, id string, now time.Time) store.StopState {
	t.Helper()
	var st store.StopState
	err := s.UpdateSessionTx(context.Background(), "s1", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
		var err error
		st, err = tx.StopState(context.Background(), id, now)
		sess.ID, sess.Agent = "s1", "claude"
		return sess, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStopStateOneStatement(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	// sb: an open blocking ticket only; sa: a queued answer only; sm: a
	// queued instruction only; all: all three; none: a held, a canceled
	// and a queued rule message, which do not count.
	create(t, s, ticket(t, "Blocking for sb?", model.ModeBlocking, "sb", t0))
	queued(t, s, "Answered for sa?", "sa")
	queuedMessage(t, s, "for sm", "sm")
	create(t, s, ticket(t, "Blocking for all?", model.ModeBlocking, "all", t0))
	queued(t, s, "Answered for all?", "all")
	queuedMessage(t, s, "for all", "all")
	createMessage(t, s, held("held for none", "none"))
	canceled := queuedMessage(t, s, "canceled for none", "none")
	if _, err := s.CancelMessage(ctx, canceled.ID); err != nil {
		t.Fatal(err)
	}
	rule := instruction("a rule for none", "none")
	rule.Kind = model.KindRule
	createMessage(t, s, rule)
	got := map[string]store.StopState{}
	for _, id := range []string{"sb", "sa", "sm", "all", "none", ""} {
		got[id] = stopState(t, s, id, now)
	}
	want := map[string]store.StopState{
		"sb":   {OpenBlocking: 1},
		"sa":   {Answers: true},
		"sm":   {Messages: true},
		"all":  {OpenBlocking: 1, Answers: true, Messages: true},
		"none": {},
		"":     {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StopState = %+v\nwant %+v", got, want)
	}
	// A typed, unconfirmed message is not queued: the Stop hook does not
	// deliver it again (Ruling 86).
	liveSession(t, s, "sm", "%5", model.StateIdle)
	claimMessageTyping(t, s, "M-1", tClaim)
	if st := stopState(t, s, "sm", now); st.Messages {
		t.Errorf("StopState after typing = %+v, want no message queued", st)
	}
}

// The hook path's reads use the partial indexes, so a Stop or a status
// refresh reads only the rows not yet delivered.
func TestStopStateAndBadgeUseTheIndexes(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	for _, q := range []struct {
		name, sql string
		args      []any
	}{
		{"StopState", store.StopStateQuery, []any{"s1", "x", "s1", "s1"}},
		{"BadgeExtras", store.BadgeExtrasQuery, nil},
	} {
		if plan := queryPlan(t, db, q.sql, q.args...); !strings.Contains(plan, "messages_undelivered") {
			t.Errorf("%s does not use messages_undelivered:\n%s", q.name, plan)
		}
	}
}

// queryPlan is EXPLAIN QUERY PLAN of query, its details one per line.
func queryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

func TestTxClaimMessagesFitsBudget(t *testing.T) {
	s := open(t, dbPath(t))
	var ms []model.Message
	for i := range 5 {
		ms = append(ms, queuedMessage(t, s, fmt.Sprintf("instruction number %d for s1", i), "s1"))
	}
	other := queuedMessage(t, s, "for s2", "s2")
	heldOne := createMessage(t, s, held("held for s1", "s1"))
	budget := utf8.RuneCountInString(message.StopBlock(ms[:2]))
	got := stopClaim(t, s, "s1", tClaim, budget)
	if ids, want := messageIDs(got), messageIDs(ms[:2]); !reflect.DeepEqual(ids, want) {
		t.Fatalf("claimed %v, want the oldest two %v", ids, want)
	}
	for _, m := range got {
		stored := assertMessageStatus(t, s, m.ID, model.MsgDelivered, model.ViaStopHook)
		if !stored.DeliveredAt.Equal(tClaim) || !stored.ConfirmedAt.Equal(tClaim) {
			t.Errorf("%s delivered %v confirmed %v, want both %v", m.ID, stored.DeliveredAt, stored.ConfirmedAt, tClaim)
		}
		assertMessage(t, m, stored)
	}
	for _, m := range append(ms[2:], other) {
		assertMessageStatus(t, s, m.ID, model.MsgQueued, "")
	}
	assertMessageStatus(t, s, heldOne.ID, model.MsgHeld, "")
	if got := stopClaim(t, s, "s1", tClaim, 0); len(got) != 0 {
		t.Errorf("budget 0 claimed %v, want none", messageIDs(got))
	}
	if got := stopClaim(t, s, "", tClaim, 4000); len(got) != 0 {
		t.Errorf("empty session claimed %v, want none", messageIDs(got))
	}
	// At most message.MaxPerStop at one Stop, the rest at the next ones.
	for i := range 10 {
		queuedMessage(t, s, fmt.Sprintf("more %d", i), "s1")
	}
	var sizes []int
	for range 4 {
		sizes = append(sizes, len(stopClaim(t, s, "s1", tClaim, 4000)))
	}
	if want := []int{message.MaxPerStop, 13 - message.MaxPerStop, 0, 0}; !reflect.DeepEqual(sizes, want) {
		t.Errorf("batch sizes %v, want %v", sizes, want)
	}
}

func TestTxClaimMessagesRollsBackOnError(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	liveSession(t, s, "s1", "%3", model.StateWorking)
	first := queuedMessage(t, s, "first", "s1")
	second := queuedMessage(t, s, "second", "s1")
	// The claim's UPDATE fails on the second row, after the first is marked.
	rawExec(t, path, `CREATE TRIGGER fail_second BEFORE UPDATE OF status ON messages
WHEN NEW.id = '`+second.ID+`' BEGIN SELECT RAISE(FAIL, 'boom'); END`)
	var claimErr error
	var claimed []model.Message
	err := s.UpdateSessionTx(ctx, "s1", func(tx *store.Tx, sess model.Session, _ bool) (model.Session, error) {
		claimed, claimErr = tx.ClaimMessages(ctx, "s1", tClaim, 4000)
		sess.State = model.StateIdle
		return sess, nil // fn ignores the error: the savepoint alone must undo the claim
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimErr == nil || claimed != nil {
		t.Fatalf("ClaimMessages = %v, %v; want nil and an error", messageIDs(claimed), claimErr)
	}
	if got := get(t, s, "s1").State; got != model.StateIdle {
		t.Errorf("session state %s, want idle (the transaction itself committed)", got)
	}
	assertMessage(t, getMessage(t, s, first.ID), first)
	assertMessage(t, getMessage(t, s, second.ID), second)
}

func TestConfirmTypedMessages(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	liveSession(t, s, "s1", "%3", model.StateIdle)
	queuedMessage(t, s, "first", "s1")
	m2 := queuedMessage(t, s, "x", "s1")
	claimMessageTyping(t, s, m2.ID, tClaim)
	// A second typed, unconfirmed line in the same session cannot be made
	// through the claims; M-12 is set up by hand to show M-2 is not M-12.
	for range 9 {
		queuedMessage(t, s, "filler", "s9")
	}
	m12 := queuedMessage(t, s, "twelve", "s1")
	if m12.ID != "M-12" {
		t.Fatalf("setup: %s, want M-12", m12.ID)
	}
	rawExec(t, path, `UPDATE messages SET status = 'delivered', delivered_via = 'send_keys', delivered_at = ? WHERE id = ?`,
		tClaim.UTC().Format("2006-01-02T15:04:05.000000000Z"), m12.ID)
	at := tClaim.Add(time.Second)
	for _, prompt := range []string{
		"please go on",
		"Message M-1 from the user: first.", // not typed
		"Message M-2from the user",          // not the wording
		"<task-notification> Message M-2 from the user: x.",
		"  <agent-message from=x> Message M-2 from the user: x.",
	} {
		if n := confirmMessages(t, s, "s1", prompt, at); n != 0 {
			t.Errorf("ConfirmTypedMessages(%q) = %d, want 0", prompt, n)
		}
	}
	// A prompt submitted before the typing is not its submission.
	if n := confirmMessages(t, s, "s1", "Message M-2 from the user: x", tClaim.Add(-time.Second)); n != 0 {
		t.Errorf("before the typing = %d, want 0", n)
	}
	if n := confirmMessages(t, s, "", "Message M-2 from the user: x", at); n != 0 {
		t.Errorf("empty session = %d, want 0", n)
	}
	if n := confirmMessages(t, s, "s1", "Message M-2 from the user: x", at); n != 1 {
		t.Fatalf("ConfirmTypedMessages = %d, want 1", n)
	}
	if got := getMessage(t, s, m2.ID); !got.ConfirmedAt.Equal(at) {
		t.Errorf("M-2 confirmed at %v, want %v", got.ConfirmedAt, at)
	}
	if got := getMessage(t, s, m12.ID); !got.ConfirmedAt.IsZero() {
		t.Errorf("%s confirmed at %v, want unconfirmed", m12.ID, got.ConfirmedAt)
	}
	if n := confirmMessages(t, s, "s1", "Message M-2 from the user: x", at); n != 0 {
		t.Errorf("confirming again = %d, want 0", n)
	}
}

// drainMessageStops claims session s1's messages through h until a claim
// finds none, and returns all it claimed. It fails a claim over
// message.MaxPerStop.
func drainMessageStops(h *store.Store) ([]model.Message, error) {
	var all []model.Message
	for {
		var got []model.Message
		err := retryBusy(func() error {
			var err error
			got, err = stopClaimLocked(h, "s1", tClaim, 4000, func() {})
			return err
		}, busyBeforeAnything)
		switch {
		case err != nil:
			return all, err
		case len(got) > message.MaxPerStop:
			return all, fmt.Errorf("one claim took %d, over %d", len(got), message.MaxPerStop)
		case len(got) == 0:
			return all, nil
		}
		all = append(all, got...)
	}
}

// Two Stop hooks of one session at once deliver each queued message once.
func TestClaimMessagesConcurrentStopsDeliverEachOnce(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 3)
	const total = 5*message.MaxPerStop + 3
	for i := range total {
		queuedMessage(t, hs[0], fmt.Sprintf("one of many, %d", i), "s1")
	}
	batches := make([][]model.Message, len(hs))
	errs := make([]error, len(hs))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for k, h := range hs {
		wg.Go(func() {
			<-start
			batches[k], errs[k] = drainMessageStops(h)
		})
	}
	close(start)
	wg.Wait()
	var all []model.Message
	for k := range hs {
		if errs[k] != nil {
			t.Errorf("claimer %d: %v", k, errs[k])
		}
		all = append(all, batches[k]...)
	}
	slices.SortFunc(all, func(a, b model.Message) int { return cmp.Compare(a.N, b.N) })
	if len(all) != total {
		t.Fatalf("claimed %d messages, want %d: %v", len(all), total, messageIDs(all))
	}
	for i, m := range all {
		if m.N != int64(i+1) {
			t.Fatalf("claim %d is %s, want M-%d: lost or repeated", i, m.ID, i+1)
		}
	}
}

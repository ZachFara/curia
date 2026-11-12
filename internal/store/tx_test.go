package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

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

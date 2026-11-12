package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

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

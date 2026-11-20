package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// holdWriter makes s hold the write lock until the returned func is called,
// and returns once the lock is held.
func holdWriter(t *testing.T, s *store.Store) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = s.UpdateSessionTx(context.Background(), "holder", func(_ *store.Tx, cur model.Session, _ bool) (model.Session, error) {
			close(held)
			<-done
			cur.ID, cur.Agent, cur.State = "holder", "claude", model.StateWorking
			return cur, nil
		})
	}()
	<-held
	return func() { close(done); <-finished }
}

// within fails the test when f takes 250 ms or more while a writer holds
// the lock for as long as f runs (a read must not queue behind writers).
func within(t *testing.T, name string, f func() error) {
	t.Helper()
	start := time.Now()
	if err := f(); err != nil {
		t.Errorf("%s: %v", name, err)
	}
	if d := time.Since(start); d >= 250*time.Millisecond {
		t.Errorf("%s took %v while a writer held the lock, want under 250ms", name, d)
	}
}

func TestOpenDoesNotWaitForWriters(t *testing.T) {
	path := dbPath(t)
	w := open(t, path)
	upsert(t, w, model.Session{ID: "s1", Agent: "claude", State: model.StateWorking})
	release := holdWriter(t, w)
	defer release()
	within(t, "Open of an existing store", func() error {
		s, err := store.Open(path)
		if err == nil {
			err = s.Close()
		}
		return err
	})
}

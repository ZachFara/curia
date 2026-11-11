package fleet

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// TestMarkStaleRechecksTheStoredRow changes the row between Reconcile's
// list pass and its update, as a hook running at the same time would, and
// checks the update re-decides from the stored row.
func TestMarkStaleRechecksTheStoredRow(t *testing.T) {
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	serverStart := t0.Add(-30 * time.Minute)
	listed := model.Session{
		ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1",
		StartedAt: t0.Add(-2 * time.Hour), UpdatedAt: t0.Add(-time.Hour),
	}
	tests := []struct {
		name      string
		live      []string
		change    func(model.Session) model.Session
		wantStale bool
	}{
		{"unchanged pre-restart session", []string{"%1"}, func(s model.Session) model.Session { return s }, true},
		{"unchanged session in a dead pane", nil, func(s model.Session) model.Session { return s }, true},
		{"restarted after the server", []string{"%1"}, func(s model.Session) model.Session {
			s.StartedAt, s.State = t0.Add(-time.Minute), model.StateWorking
			return s
		}, false},
		{"moved to a live pane", nil, func(s model.Session) model.Session {
			s.Pane, s.StartedAt = "%2", t0.Add(-time.Minute)
			return s
		}, false},
		{"ended", nil, func(s model.Session) model.Session {
			s.State = model.StateEnded
			return s
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stored := tc.change(listed)
			want := stored
			if tc.wantStale {
				want.State, want.UpdatedAt = model.StateStale, t0
			}
			if got := markStaleAfter(t, listed, stored, tc.live, serverStart, t0); got != want {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

// markStaleAfter stores stored, runs markStale for listed against a server
// started at serverStart with the live panes plus %2, and returns the row.
func markStaleAfter(t *testing.T, listed, stored model.Session, live []string, serverStart, now time.Time) model.Session {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "curia.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err = st.UpsertSession(ctx, stored); err != nil {
		t.Fatal(err)
	}
	var panes []tmux.Pane
	for _, id := range append(live, "%2") {
		panes = append(panes, tmux.Pane{ID: id, ServerStart: serverStart})
	}
	if _, err = markStale(ctx, st, listed, newServer(panes), now); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.GetSession(ctx, listed.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

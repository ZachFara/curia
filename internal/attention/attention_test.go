package attention

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

var t0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func TestMarkerByState(t *testing.T) {
	tests := []struct {
		name string
		s    model.Session
		want string
	}{
		{"waiting unseen", model.Session{State: model.StateWaiting, UpdatedAt: t0}, "red"},
		{"needs decision unseen", model.Session{State: model.StateNeedsDecision, UpdatedAt: t0, SeenAt: t0.Add(-time.Second)}, "red"},
		{"waiting seen", model.Session{State: model.StateWaiting, UpdatedAt: t0, SeenAt: t0}, ""},
		{"waiting seen later", model.Session{State: model.StateWaiting, UpdatedAt: t0, SeenAt: t0.Add(time.Second)}, ""},
		{"working", model.Session{State: model.StateWorking, UpdatedAt: t0}, ""},
		{"idle", model.Session{State: model.StateIdle, UpdatedAt: t0}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Marker(tt.s); got != tt.want {
				t.Errorf("Marker = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWindowMarkerStrongestWins(t *testing.T) {
	working := model.Session{State: model.StateWorking, UpdatedAt: t0}
	waiting := model.Session{State: model.StateWaiting, UpdatedAt: t0}
	if got := WindowMarker([]model.Session{working, waiting}); got != "red" {
		t.Errorf("WindowMarker = %q, want red", got)
	}
	if got := WindowMarker([]model.Session{working}); got != "" {
		t.Errorf("WindowMarker(working) = %q, want empty", got)
	}
	if got := WindowMarker(nil); got != "" {
		t.Errorf("WindowMarker(nil) = %q, want empty", got)
	}
}

func openStore(t *testing.T, sessions ...model.Session) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "curia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, s := range sessions {
		if err := st.UpsertSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestApplySetsWindowOption(t *testing.T) {
	st := openStore(t,
		model.Session{ID: "a", Pane: "%1", TmuxWindow: "@7", State: model.StateWaiting, UpdatedAt: t0},
		model.Session{ID: "b", Pane: "%2", TmuxWindow: "@7", State: model.StateWorking, UpdatedAt: t0},
	)
	fake := &tmux.Fake{}
	if err := Apply(context.Background(), fake, st, "@7"); err != nil {
		t.Fatal(err)
	}
	want := []string{"SetWindowOption @7 @curia_marker red"}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %q, want %q", fake.Calls, want)
	}
}

func TestApplyIgnoresOtherWindowsEndedAndStale(t *testing.T) {
	st := openStore(t,
		model.Session{ID: "a", TmuxWindow: "@8", State: model.StateWaiting, UpdatedAt: t0},
		model.Session{ID: "b", TmuxWindow: "@7", State: model.StateEnded, UpdatedAt: t0},
		model.Session{ID: "c", TmuxWindow: "@7", State: model.StateStale, UpdatedAt: t0},
	)
	fake := &tmux.Fake{}
	if err := Apply(context.Background(), fake, st, "@7"); err != nil {
		t.Fatal(err)
	}
	want := []string{"SetWindowOption @7 @curia_marker "}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %q, want %q", fake.Calls, want)
	}
}

func TestSeenClearsMarker(t *testing.T) {
	st := openStore(t, model.Session{ID: "a", Pane: "%1", TmuxWindow: "@7", State: model.StateWaiting, UpdatedAt: t0})
	ctx := context.Background()
	fake := &tmux.Fake{}
	if err := st.MarkSeen(ctx, "%1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, fake, st, "@7"); err != nil {
		t.Fatal(err)
	}
	want := []string{"SetWindowOption @7 @curia_marker "}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %q, want %q", fake.Calls, want)
	}
}

func TestApplyEmptyWindowDoesNothing(t *testing.T) {
	st := openStore(t, model.Session{ID: "a", State: model.StateWaiting, UpdatedAt: t0})
	fake := &tmux.Fake{}
	if err := Apply(context.Background(), fake, st, ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %q, want none", fake.Calls)
	}
}

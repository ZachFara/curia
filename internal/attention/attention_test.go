package attention

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
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

func TestMarkerBlueForUnseenReviewPending(t *testing.T) {
	review := func(state model.State, seen time.Time) model.Session {
		return model.Session{State: state, TaskID: "T-1", ReviewPending: true, UpdatedAt: t0, SeenAt: seen}
	}
	tests := []struct {
		name string
		s    model.Session
		want string
	}{
		{"idle unseen", review(model.StateIdle, t0.Add(-time.Second)), "blue"},
		{"idle never seen", review(model.StateIdle, time.Time{}), "blue"},
		{"working unseen", review(model.StateWorking, t0.Add(-time.Second)), "blue"},
		{"idle seen", review(model.StateIdle, t0), ""},
		{"idle seen later", review(model.StateIdle, t0.Add(time.Second)), ""},
		{"waiting unseen", review(model.StateWaiting, t0.Add(-time.Second)), "red"},
		{"needs decision unseen", review(model.StateNeedsDecision, time.Time{}), "red"},
		{"waiting seen", review(model.StateWaiting, t0), ""},
		{"ended", review(model.StateEnded, time.Time{}), ""},
		{"stale", review(model.StateStale, time.Time{}), ""},
		{"no review", model.Session{State: model.StateIdle, TaskID: "T-1", UpdatedAt: t0}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Marker(tt.s); got != tt.want {
				t.Errorf("Marker = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWindowMarkerRedBeatsBlue(t *testing.T) {
	blue := model.Session{State: model.StateIdle, ReviewPending: true, UpdatedAt: t0}
	red := model.Session{State: model.StateWaiting, UpdatedAt: t0}
	working := model.Session{State: model.StateWorking, UpdatedAt: t0}
	for _, tc := range []struct {
		name     string
		sessions []model.Session
		want     string
	}{
		{"blue then red", []model.Session{blue, red}, "red"},
		{"red then blue", []model.Session{red, blue}, "red"},
		{"blue alone", []model.Session{working, blue}, "blue"},
		{"neither", []model.Session{working}, ""},
	} {
		if got := WindowMarker(tc.sessions); got != tc.want {
			t.Errorf("%s: WindowMarker = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// applyCalls runs Apply on window @7 with a fresh fake and returns its calls.
func applyCalls(t *testing.T, st *store.Store) []string {
	t.Helper()
	fake := &tmux.Fake{}
	if err := Apply(context.Background(), fake, st, "@7"); err != nil {
		t.Fatal(err)
	}
	return fake.Calls
}

func TestApplySetsBlueFromPendingReview(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	task, err := st.CreateTask(ctx, model.Task{
		Title: "x", Repo: "/src/curia", Status: model.TaskReady, Source: model.SourceLocal, CreatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := model.Session{ID: "a", Pane: "%1", TmuxWindow: "@7", State: model.StateIdle, TaskID: task.ID, UpdatedAt: t0}
	other := model.Session{ID: "b", Pane: "%2", TmuxWindow: "@7", State: model.StateWorking, UpdatedAt: t0}
	for _, s := range []model.Session{agent, other} {
		if err = st.UpsertSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	unmarked := []string{"SetWindowOption @7 @curia_marker "}
	if got := applyCalls(t, st); !slices.Equal(got, unmarked) {
		t.Errorf("before the review: calls = %q, want %q", got, unmarked)
	}
	review, _, err := st.CompleteTask(ctx, model.Review{
		TaskID: task.ID, SessionID: "a", Summary: "done", Tests: model.TestsPassed, CreatedAt: t0,
	})
	if err != nil {
		t.Fatal(err)
	}
	blue := []string{"SetWindowOption @7 @curia_marker blue"}
	if got := applyCalls(t, st); !slices.Equal(got, blue) {
		t.Errorf("pending review: calls = %q, want %q", got, blue)
	}
	if err = st.MarkSeen(ctx, "%1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := applyCalls(t, st); !slices.Equal(got, unmarked) {
		t.Errorf("after MarkSeen: calls = %q, want %q", got, unmarked)
	}
	// A later event makes it unseen again, then the review closes.
	agent.SeenAt, agent.UpdatedAt = t0.Add(time.Minute), t0.Add(2*time.Minute)
	if err = st.UpsertSession(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if got := applyCalls(t, st); !slices.Equal(got, blue) {
		t.Errorf("unseen again: calls = %q, want %q", got, blue)
	}
	if _, _, err = st.CloseReview(ctx, review.ID, store.ReviewClose{
		Status: model.ReviewChanges, Notes: "more", Via: model.AnsweredCLI,
	}, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := applyCalls(t, st); !slices.Equal(got, unmarked) {
		t.Errorf("after CloseReview: calls = %q, want %q", got, unmarked)
	}
}

func TestApplyWithoutReviewsTableStillAppliesRed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curia.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err = st.UpsertSession(ctx, model.Session{
		ID: "a", Pane: "%1", TmuxWindow: "@7", State: model.StateWaiting, TaskID: "T-1", UpdatedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err = raw.Exec("DROP TABLE reviews"); err != nil {
		t.Fatal(err)
	}
	fake := &tmux.Fake{}
	err = Apply(ctx, fake, st, "@7")
	if err == nil || !strings.Contains(err.Error(), "pending reviews") {
		t.Errorf("Apply with no reviews table = %v, want the pending reviews read's error", err)
	}
	if want := []string{"SetWindowOption @7 @curia_marker red"}; !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %q, want %q", fake.Calls, want)
	}
}

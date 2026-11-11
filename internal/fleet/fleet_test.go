package fleet_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

var t0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// openStore opens a fresh store under t.TempDir() seeded with sessions.
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

func get(t *testing.T, st *store.Store, id string) model.Session {
	t.Helper()
	s, found, err := st.GetSession(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s) = found %v, err %v", id, found, err)
	}
	return s
}

func sess(id string, state model.State, pane string, updated time.Time) model.Session {
	return model.Session{ID: id, Agent: "claude", State: state, Pane: pane, UpdatedAt: updated}
}

func ids(s []model.Session) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.ID
	}
	return out
}

func TestReconcileMarksMissingPanesStale(t *testing.T) {
	old := t0.Add(-time.Hour)
	st := openStore(t,
		sess("gone", model.StateWorking, "%1", old),
		sess("live", model.StateWaiting, "%2", old),
	)
	now := t0
	if err := fleet.Reconcile(context.Background(), st, []tmux.Pane{{ID: "%2"}}, now); err != nil {
		t.Fatal(err)
	}
	if g := get(t, st, "gone"); g.State != model.StateStale || !g.UpdatedAt.Equal(now) {
		t.Errorf("gone = %s at %v, want stale at %v", g.State, g.UpdatedAt, now)
	}
	if l := get(t, st, "live"); l.State != model.StateWaiting || !l.UpdatedAt.Equal(old) {
		t.Errorf("live = %s at %v, want unchanged waiting at %v", l.State, l.UpdatedAt, old)
	}
}

// A restarted tmux server numbers panes from %0 again, so a pane id alone
// cannot tell a pre-restart session from the new pane that took its id.
func TestReconcileMarksSessionsFromBeforeServerRestartStale(t *testing.T) {
	serverStart := t0.Add(-30 * time.Minute)
	ghost := sess("ghost", model.StateWaiting, "%1", t0.Add(-time.Hour))
	ghost.StartedAt = t0.Add(-2 * time.Hour)
	fresh := sess("fresh", model.StateWaiting, "%2", t0.Add(-time.Minute))
	fresh.StartedAt = t0.Add(-10 * time.Minute)
	// Started in the server's first second: start_time is whole seconds.
	sameSecond := sess("same-second", model.StateWorking, "%3", t0.Add(-time.Minute))
	sameSecond.StartedAt = serverStart.Add(900 * time.Millisecond)
	unknown := sess("no-start", model.StateWorking, "%4", t0.Add(-time.Minute)) // StartedAt zero
	st := openStore(t, ghost, fresh, sameSecond, unknown)
	var live []tmux.Pane
	for _, id := range []string{"%1", "%2", "%3", "%4"} {
		live = append(live, tmux.Pane{ID: id, ServerStart: serverStart})
	}
	if err := fleet.Reconcile(context.Background(), st, live, t0); err != nil {
		t.Fatal(err)
	}
	if g := get(t, st, "ghost"); g.State != model.StateStale || !g.UpdatedAt.Equal(t0) {
		t.Errorf("ghost = %s at %v, want stale at %v", g.State, g.UpdatedAt, t0)
	}
	for _, id := range []string{"fresh", "same-second", "no-start"} {
		if s := get(t, st, id); s.State == model.StateStale {
			t.Errorf("%s marked stale, want it left alone", id)
		}
	}
}

func TestReconcileIgnoresSessionsWithoutPane(t *testing.T) {
	old := t0.Add(-time.Hour)
	st := openStore(t, sess("nopane", model.StateWorking, "", old))
	if err := fleet.Reconcile(context.Background(), st, nil, t0); err != nil {
		t.Fatal(err)
	}
	if s := get(t, st, "nopane"); s.State != model.StateWorking || !s.UpdatedAt.Equal(old) {
		t.Errorf("nopane = %s at %v, want unchanged working", s.State, s.UpdatedAt)
	}
}

// An already stale session keeps its UpdatedAt, or it would never be hidden.
func TestReconcileLeavesStaleAndEndedAlone(t *testing.T) {
	old := t0.Add(-time.Hour)
	st := openStore(t,
		sess("stale", model.StateStale, "%1", old),
		sess("ended", model.StateEnded, "%2", old),
	)
	if err := fleet.Reconcile(context.Background(), st, nil, t0); err != nil {
		t.Fatal(err)
	}
	if s := get(t, st, "stale"); s.State != model.StateStale || !s.UpdatedAt.Equal(old) {
		t.Errorf("stale = %s at %v, want stale at %v", s.State, s.UpdatedAt, old)
	}
	if s := get(t, st, "ended"); s.State != model.StateEnded || !s.UpdatedAt.Equal(old) {
		t.Errorf("ended = %s at %v, want ended at %v", s.State, s.UpdatedAt, old)
	}
}

func TestReconcileKeepsOtherFields(t *testing.T) {
	in := model.Session{
		ID: "a", Agent: "claude", Pane: "%1", Repo: "curia", Branch: "main", State: model.StateIdle,
		ContextUsed: 10, TranscriptOffset: 99, SeenAt: t0.Add(-2 * time.Hour), UpdatedAt: t0.Add(-time.Hour),
	}
	st := openStore(t, in)
	if err := fleet.Reconcile(context.Background(), st, nil, t0); err != nil {
		t.Fatal(err)
	}
	want := in
	want.State, want.UpdatedAt = model.StateStale, t0
	if got := get(t, st, "a"); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestRankOrder(t *testing.T) {
	at := t0.Add(-time.Hour)
	idleUnseen := sess("3-idle-unseen", model.StateIdle, "%3", at)
	idleUnseen.SeenAt = at.Add(-time.Minute)
	idleSeen := sess("5-idle-seen", model.StateIdle, "%5", at)
	idleSeen.SeenAt = at.Add(time.Minute)
	in := []model.Session{
		sess("6-stale", model.StateStale, "%6", at),
		idleSeen,
		sess("7-unknown", model.State("bogus"), "%7", at),
		sess("4-working", model.StateWorking, "%4", at),
		idleUnseen,
		sess("2-waiting", model.StateWaiting, "%2", at),
		sess("1-needs-decision", model.StateNeedsDecision, "%1", at),
	}
	orig := ids(in)
	want := []string{"1-needs-decision", "2-waiting", "3-idle-unseen", "4-working", "5-idle-seen", "6-stale", "7-unknown"}
	if got := ids(fleet.Rank(in)); !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(ids(in), orig) {
		t.Errorf("Rank reordered its input: %v", ids(in))
	}
}

func TestRankOldestFirstWithinGroup(t *testing.T) {
	in := []model.Session{
		sess("new", model.StateWaiting, "%1", t0),
		sess("old", model.StateWaiting, "%2", t0.Add(-2*time.Hour)),
		sess("mid", model.StateWaiting, "%3", t0.Add(-time.Hour)),
	}
	want := []string{"old", "mid", "new"}
	if got := ids(fleet.Rank(in)); !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v, want %v", got, want)
	}
}

// A zero SeenAt is "before" UpdatedAt: a session never looked at is unseen.
func TestRankIdleNeverSeenIsUnseen(t *testing.T) {
	seen := sess("seen", model.StateIdle, "%1", t0.Add(-time.Hour))
	seen.SeenAt = t0
	working := sess("working", model.StateWorking, "%2", t0.Add(-time.Hour))
	never := sess("never", model.StateIdle, "%3", t0)
	want := []string{"never", "working", "seen"}
	if got := ids(fleet.Rank([]model.Session{seen, working, never})); !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v, want %v", got, want)
	}
}

func TestCount(t *testing.T) {
	seen := sess("e", model.StateNeedsDecision, "%5", t0)
	seen.SeenAt = t0.Add(time.Minute) // counted regardless of seen
	in := []model.Session{
		sess("a", model.StateNeedsDecision, "%1", t0),
		sess("b", model.StateWaiting, "", t0),
		sess("c", model.StateWorking, "%3", t0),
		sess("d", model.StateIdle, "%4", t0),
		seen,
		sess("f", model.StateStale, "%6", t0),
	}
	if got, want := fleet.Count(in), (fleet.Counts{Decisions: 2, Waiting: 1}); got != want {
		t.Errorf("Count = %+v, want %+v", got, want)
	}
}

func TestBadge(t *testing.T) {
	for _, tc := range []struct {
		in   fleet.Counts
		want string
	}{
		{fleet.Counts{Decisions: 2, Waiting: 1, Reviews: 1}, "D2 W1 R1"},
		{fleet.Counts{Waiting: 1}, "W1"},
		{fleet.Counts{}, ""},
		{fleet.Counts{Decisions: 3, Reviews: 12}, "D3 R12"},
	} {
		if got := fleet.Badge(tc.in); got != tc.want {
			t.Errorf("Badge(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNextSkipsSessionsWithoutPane(t *testing.T) {
	in := []model.Session{
		sess("nopane", model.StateNeedsDecision, "", t0.Add(-time.Hour)),
		sess("waiting", model.StateWaiting, "%2", t0),
	}
	got, ok := fleet.Next(in)
	if !ok || got.ID != "waiting" {
		t.Errorf("Next = %q, %v, want waiting, true", got.ID, ok)
	}
}

func TestNextPicksIdleUnseen(t *testing.T) {
	seen := sess("seen", model.StateIdle, "%1", t0.Add(-time.Hour))
	seen.SeenAt = t0
	unseen := sess("unseen", model.StateIdle, "%2", t0)
	got, ok := fleet.Next([]model.Session{seen, unseen})
	if !ok || got.ID != "unseen" {
		t.Errorf("Next = %q, %v, want unseen, true", got.ID, ok)
	}
}

func TestNextNoneWhenAllWorking(t *testing.T) {
	seen := sess("seen", model.StateIdle, "%3", t0.Add(-time.Hour))
	seen.SeenAt = t0
	in := []model.Session{
		sess("a", model.StateWorking, "%1", t0),
		sess("b", model.StateWorking, "%2", t0),
		seen,
		sess("c", model.StateStale, "%4", t0),
	}
	if got, ok := fleet.Next(in); ok {
		t.Errorf("Next = %q, true, want none", got.ID)
	}
	if _, ok := fleet.Next(nil); ok {
		t.Error("Next(nil) found a session")
	}
}

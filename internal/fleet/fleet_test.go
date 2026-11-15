package fleet_test

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
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
	if _, err := fleet.Reconcile(context.Background(), st, []tmux.Pane{{ID: "%2"}}, now); err != nil {
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
	if _, err := fleet.Reconcile(context.Background(), st, live, t0); err != nil {
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

// Reconcile reports the windows of the sessions it marked stale, so the
// caller can re-apply their markers.
func TestReconcileReportsWindowsMarkedStale(t *testing.T) {
	old := t0.Add(-time.Hour)
	inWindow := func(id string, state model.State, pane, window string) model.Session {
		s := sess(id, state, pane, old)
		s.TmuxWindow = window
		return s
	}
	st := openStore(t,
		inWindow("gone-a", model.StateWaiting, "%1", "@3"),
		inWindow("gone-b", model.StateWorking, "%2", "@3"),
		inWindow("gone-c", model.StateIdle, "%5", "@4"),
		inWindow("no-window", model.StateWaiting, "%6", ""),
		inWindow("live", model.StateWaiting, "%7", "@5"),
		inWindow("already-stale", model.StateStale, "%8", "@6"),
	)
	windows, err := fleet.Reconcile(context.Background(), st, []tmux.Pane{{ID: "%7"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"@3", "@4"}; !reflect.DeepEqual(windows, want) {
		t.Errorf("windows = %q, want %q", windows, want)
	}
}

func TestReconcileIgnoresSessionsWithoutPane(t *testing.T) {
	old := t0.Add(-time.Hour)
	st := openStore(t, sess("nopane", model.StateWorking, "", old))
	if _, err := fleet.Reconcile(context.Background(), st, nil, t0); err != nil {
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
	if _, err := fleet.Reconcile(context.Background(), st, nil, t0); err != nil {
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
	if _, err := fleet.Reconcile(context.Background(), st, nil, t0); err != nil {
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
	seen := sess("e", model.StateWaiting, "%5", t0)
	seen.SeenAt = t0.Add(time.Minute) // counted regardless of seen
	in := []model.Session{
		sess("a", model.StateNeedsDecision, "%1", t0),
		sess("b", model.StateWaiting, "", t0),
		sess("c", model.StateWorking, "%3", t0),
		sess("d", model.StateIdle, "%4", t0),
		seen,
		sess("f", model.StateStale, "%6", t0),
	}
	if got, want := fleet.Count(in, 0, 0), (fleet.Counts{Waiting: 2}); got != want {
		t.Errorf("Count = %+v, want %+v", got, want)
	}
}

// Section 4.10: D counts open blocking tickets, not needs_decision sessions.
func TestCountUsesOpenBlocking(t *testing.T) {
	in := []model.Session{
		sess("a", model.StateNeedsDecision, "%1", t0),
		sess("b", model.StateWaiting, "%2", t0),
		sess("c", model.StateWaiting, "%3", t0),
	}
	if got, want := fleet.Count(in, 3, 1), (fleet.Counts{Decisions: 3, Undelivered: 1, Waiting: 2}); got != want {
		t.Errorf("Count = %+v, want %+v", got, want)
	}
}

func TestBadge(t *testing.T) {
	for _, tc := range []struct {
		in   fleet.Counts
		want string
	}{
		{fleet.Counts{Decisions: 2, Waiting: 1, Reviews: 1}, "D2 W1 R1"},
		{fleet.Counts{Decisions: 1, Undelivered: 2, Waiting: 1}, "D1 Q2 W1"}, // Ruling 89
		{fleet.Counts{Undelivered: 1}, "Q1"},
		{fleet.Counts{Waiting: 1}, "W1"},
		{fleet.Counts{}, ""},
		{fleet.Counts{Decisions: 3, Reviews: 12}, "D3 R12"},
	} {
		if got := fleet.Badge(tc.in); got != tc.want {
			t.Errorf("Badge(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBadgeAutoOffSuffix(t *testing.T) {
	for _, tc := range []struct {
		in   fleet.Counts
		want string
	}{
		{fleet.Counts{Decisions: 1, AutoOff: true}, "D1 auto-off"},
		{fleet.Counts{Decisions: 2, Undelivered: 1, Waiting: 3, Reviews: 4, AutoOff: true}, "D2 Q1 W3 R4 auto-off"},
		{fleet.Counts{AutoOff: true}, "auto-off"},
		{fleet.Counts{Decisions: 1}, "D1"},
		{fleet.Counts{}, ""},
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

// withTask is sess with TaskID task.
func withTask(id string, state model.State, pane, task string, updated time.Time) model.Session {
	s := sess(id, state, pane, updated)
	s.TaskID = task
	return s
}

func TestWithReviewsMarksLiveSessionsOfPendingTasks(t *testing.T) {
	in := []model.Session{
		withTask("idle", model.StateIdle, "%1", "T-1", t0),
		withTask("working", model.StateWorking, "%2", "T-1", t0),
		withTask("waiting", model.StateWaiting, "%3", "T-1", t0),
		withTask("needs", model.StateNeedsDecision, "%4", "T-1", t0),
		withTask("ended", model.StateEnded, "%5", "T-1", t0),
		withTask("stale", model.StateStale, "%6", "T-1", t0),
		withTask("other-task", model.StateIdle, "%7", "T-2", t0),
		withTask("no-task", model.StateIdle, "%8", "", t0),
	}
	got := fleet.WithReviews(in, map[string]string{"T-1": "R-1", "T-3": "R-2"})
	want := map[string]bool{"idle": true, "working": true, "waiting": true, "needs": true}
	if len(got) != len(in) {
		t.Fatalf("WithReviews returned %d sessions, want %d", len(got), len(in))
	}
	for i, s := range got {
		if s.ID != in[i].ID {
			t.Errorf("session %d = %s, want %s (order kept)", i, s.ID, in[i].ID)
		}
		if s.ReviewPending != want[s.ID] {
			t.Errorf("%s ReviewPending = %v, want %v", s.ID, s.ReviewPending, want[s.ID])
		}
		s.ReviewPending = in[i].ReviewPending
		if !reflect.DeepEqual(s, in[i]) {
			t.Errorf("%s changed beyond ReviewPending:\n got %+v\nwant %+v", s.ID, s, in[i])
		}
	}
	// A session already marked by an earlier call whose review has since closed is unmarked.
	if again := fleet.WithReviews(got, map[string]string{}); again[0].ReviewPending {
		t.Error("WithReviews with no pending reviews kept ReviewPending set")
	}
	if out := fleet.WithReviews(nil, map[string]string{"T-1": "R-1"}); len(out) != 0 {
		t.Errorf("WithReviews(nil) = %v, want empty", out)
	}
}

func TestWithReviewsDoesNotMutateInput(t *testing.T) {
	in := []model.Session{
		withTask("a", model.StateIdle, "%1", "T-1", t0),
		withTask("b", model.StateWorking, "%2", "T-2", t0),
	}
	orig := slices.Clone(in)
	got := fleet.WithReviews(in, map[string]string{"T-1": "R-1", "T-2": "R-2"})
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("WithReviews changed its input: %+v", in)
	}
	if !got[0].ReviewPending || !got[1].ReviewPending {
		t.Fatalf("WithReviews = %+v, want both marked", got)
	}
	got[0].ID = "changed"
	if in[0].ID != "a" {
		t.Error("WithReviews returned a slice sharing the input's array")
	}
}

func TestRankPutsReviewReadyAfterWaiting(t *testing.T) {
	at := t0.Add(-time.Hour)
	reviewSeen := withTask("3-review-ready-seen", model.StateIdle, "%3", "T-1", at)
	reviewSeen.SeenAt, reviewSeen.ReviewPending = at.Add(time.Minute), true
	reviewUnseen := withTask("3-review-ready-unseen", model.StateIdle, "%9", "T-2", at.Add(time.Second))
	reviewUnseen.ReviewPending = true
	idleUnseen := sess("4-idle-unseen", model.StateIdle, "%4", at.Add(-time.Hour)) // older than the review-ready ones
	idleSeen := sess("6-idle-seen", model.StateIdle, "%6", at)
	idleSeen.SeenAt = at.Add(time.Minute)
	in := []model.Session{
		idleSeen,
		sess("5-working", model.StateWorking, "%5", at.Add(-2*time.Hour)),
		idleUnseen,
		reviewUnseen,
		reviewSeen,
		sess("2-waiting", model.StateWaiting, "%2", at),
		sess("1-needs-decision", model.StateNeedsDecision, "%1", at),
	}
	want := []string{
		"1-needs-decision", "2-waiting", "3-review-ready-seen", "3-review-ready-unseen",
		"4-idle-unseen", "5-working", "6-idle-seen",
	}
	if got := ids(fleet.Rank(in)); !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v\nwant   %v", got, want)
	}
}

// The agent was sent back to work: its review is still pending, but it is
// working, not ready for review.
func TestReviewPendingWorkingSessionRanksAsWorking(t *testing.T) {
	at := t0.Add(-time.Hour)
	back := withTask("back-at-work", model.StateWorking, "%1", "T-1", at.Add(-time.Hour))
	back.ReviewPending = true
	idleUnseen := sess("idle-unseen", model.StateIdle, "%2", at)
	working := sess("working", model.StateWorking, "%3", at)
	idleSeen := sess("idle-seen", model.StateIdle, "%4", at.Add(-2*time.Hour))
	idleSeen.SeenAt = at
	want := []string{"idle-unseen", "back-at-work", "working", "idle-seen"}
	if got := ids(fleet.Rank([]model.Session{idleSeen, working, back, idleUnseen})); !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v, want %v", got, want)
	}
	if got, ok := fleet.Next([]model.Session{back, working}); ok {
		t.Errorf("Next = %q, true; want none (a working review-pending session is not review-ready)", got.ID)
	}
}

func TestNextJumpsToReviewReady(t *testing.T) {
	ready := withTask("ready", model.StateIdle, "%2", "T-1", t0)
	ready.SeenAt, ready.ReviewPending = t0.Add(time.Minute), true // seen: still review-ready
	working := sess("working", model.StateWorking, "%1", t0.Add(-time.Hour))
	got, ok := fleet.Next([]model.Session{working, ready})
	if !ok || got.ID != "ready" {
		t.Errorf("Next = %q, %v, want ready, true", got.ID, ok)
	}
	// Without its review the same session is idle-seen, which Next skips.
	ready.ReviewPending = false
	if got, ok := fleet.Next([]model.Session{working, ready}); ok {
		t.Errorf("Next without the review = %q, true; want none", got.ID)
	}
}

// onServer is a pane on the server whose socket is socket.
func onServer(socket string, start time.Time, ids ...string) []tmux.Pane {
	var panes []tmux.Pane
	for _, id := range ids {
		panes = append(panes, tmux.Pane{ID: id, ServerStart: start, Socket: socket})
	}
	return panes
}

func TestReconcileLeavesOtherServerAlone(t *testing.T) {
	old := t0.Add(-time.Hour)
	elsewhere := sess("elsewhere", model.StateIdle, "%1", old)
	elsewhere.Socket, elsewhere.TmuxWindow = "/tmp/a", "@1"
	// Pane ids are reused across servers: %2 on /tmp/b is not this session's
	// pane, and /tmp/b started after the session did.
	recycled := sess("recycled", model.StateWaiting, "%2", old)
	recycled.Socket, recycled.TmuxWindow, recycled.StartedAt = "/tmp/a", "@2", t0.Add(-2*time.Hour)
	st := openStore(t, elsewhere, recycled)
	windows, err := fleet.Reconcile(context.Background(), st, onServer("/tmp/b", t0.Add(-30*time.Minute), "%2", "%3"), t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 0 {
		t.Errorf("windows = %q, want none", windows)
	}
	if s := get(t, st, "elsewhere"); s.State != model.StateIdle || !s.UpdatedAt.Equal(old) {
		t.Errorf("elsewhere = %s at %v, want unchanged idle", s.State, s.UpdatedAt)
	}
	if s := get(t, st, "recycled"); s.State != model.StateWaiting || !s.UpdatedAt.Equal(old) {
		t.Errorf("recycled = %s at %v, want unchanged waiting", s.State, s.UpdatedAt)
	}
}

func TestReconcileSameServerStillMarksStale(t *testing.T) {
	old := t0.Add(-time.Hour)
	gone := sess("gone", model.StateIdle, "%1", old)
	gone.Socket, gone.TmuxWindow = "/tmp/b", "@1"
	// Same server, restarted since the session started: %2 is another pane now.
	ghost := sess("ghost", model.StateWaiting, "%2", old)
	ghost.Socket, ghost.TmuxWindow, ghost.StartedAt = "/tmp/b", "@2", t0.Add(-2*time.Hour)
	live := sess("live", model.StateWaiting, "%3", old)
	live.Socket, live.StartedAt = "/tmp/b", t0.Add(-10*time.Minute)
	st := openStore(t, gone, ghost, live)
	// The first pane reports no socket: the server's socket is the first non-empty one.
	panes := append([]tmux.Pane{{ID: "%9", ServerStart: t0.Add(-30 * time.Minute)}},
		onServer("/tmp/b", t0.Add(-30*time.Minute), "%2", "%3")...)
	windows, err := fleet.Reconcile(context.Background(), st, panes, t0)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(windows)
	if want := []string{"@1", "@2"}; !reflect.DeepEqual(windows, want) {
		t.Errorf("windows = %q, want %q (in any order)", windows, want)
	}
	for _, id := range []string{"gone", "ghost"} {
		if s := get(t, st, id); s.State != model.StateStale || !s.UpdatedAt.Equal(t0) {
			t.Errorf("%s = %s at %v, want stale at %v", id, s.State, s.UpdatedAt, t0)
		}
	}
	if s := get(t, st, "live"); s.State != model.StateWaiting {
		t.Errorf("live = %s, want waiting", s.State)
	}
}

// A session with no recorded server (a row from before Plan 4, or written by
// an older hook), or a server whose panes report no socket, is reconciled as
// in Plan 2: by pane id and server start alone.
func TestReconcileUnknownSocketBehavesAsBefore(t *testing.T) {
	serverStart := t0.Add(-30 * time.Minute)
	old := t0.Add(-time.Hour)
	for _, tc := range []struct {
		name, sessionSocket, serverSocket string
	}{
		{"session socket unknown", "", "/tmp/b"},
		{"server socket unknown", "/tmp/a", ""},
		{"both unknown", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gone := sess("gone", model.StateIdle, "%1", old)
			ghost := sess("ghost", model.StateWaiting, "%2", old)
			ghost.StartedAt = t0.Add(-2 * time.Hour)
			live := sess("live", model.StateWaiting, "%3", old)
			live.StartedAt = t0.Add(-10 * time.Minute)
			for _, s := range []*model.Session{&gone, &ghost, &live} {
				s.Socket = tc.sessionSocket
			}
			st := openStore(t, gone, ghost, live)
			if _, err := fleet.Reconcile(context.Background(), st, onServer(tc.serverSocket, serverStart, "%2", "%3"), t0); err != nil {
				t.Fatal(err)
			}
			for id, want := range map[string]model.State{"gone": model.StateStale, "ghost": model.StateStale, "live": model.StateWaiting} {
				if s := get(t, st, id); s.State != want {
					t.Errorf("%s = %s, want %s", id, s.State, want)
				}
			}
		})
	}
}

// OtherServer is Reconcile's server rule for callers that must not type
// into, or count, a pane whose id the session's own server also uses.
func TestOtherServer(t *testing.T) {
	start := t0.Add(-30 * time.Minute)
	noSocket := []tmux.Pane{{ID: "%9", ServerStart: start}}
	for _, tc := range []struct {
		name, socket string
		live         []tmux.Pane
		want         bool
	}{
		{"another server", "/tmp/a", onServer("/tmp/b", start, "%1"), true},
		{"the same server", "/tmp/b", onServer("/tmp/b", start, "%1"), false},
		{"first known socket decides", "/tmp/a", append(noSocket, onServer("/tmp/b", start, "%1")...), true},
		{"first known socket is the same", "/tmp/b", append(noSocket, onServer("/tmp/b", start, "%1")...), false},
		{"session socket unknown", "", onServer("/tmp/b", start, "%1"), false},
		{"server socket unknown", "/tmp/a", onServer("", start, "%1"), false},
		{"no panes", "/tmp/a", nil, false},
	} {
		s := sess("s", model.StateIdle, "%1", t0)
		s.Socket = tc.socket
		if got := fleet.OtherServer(tc.live, s); got != tc.want {
			t.Errorf("%s: OtherServer = %v, want %v", tc.name, got, tc.want)
		}
	}
}

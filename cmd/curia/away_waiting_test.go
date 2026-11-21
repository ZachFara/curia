package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func awayStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// The "Waiting for you" block of curia back with a distinct non-zero number on every line, and
// rows around it that must not be counted: wrong status, before the window, other kinds.
// If the numbers' source changes (fix/plan6-fixwave-c2 moves the per-agent counts to COUNT
// queries), only the "What each agent did" lines would need updating; this block is Store.Waiting.
func TestBackSummaryWaitingBlockCountsEachKind(t *testing.T) {
	st, now := awayEnv(t, awaySess("s1abcdef", "idle", ""))
	_ = st
	if _, err := runRoot(t, "away", "2h"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	db, in, before, after := os.Getenv("CURIA_DB"), awayStamp(awayT0.Add(30*time.Minute)), awayStamp(awayT0.Add(-time.Hour)), awayStamp(awayT0.Add(2*time.Hour))
	n := 100
	ticket := func(mode, status, coerced, at string) {
		n++
		statusExec(t, db, `INSERT INTO decisions (id, session_id, question, options, recommendation, rationale, reversibility, mode, scope, status, args_hash, created_at, coerced_from)
VALUES (?, 's1abcdef', 'q', '[]', 'A', 'r', 'cheap', ?, 'repo', ?, ?, ?, NULLIF(?, ''))`, fmt.Sprintf("D-%d", n), mode, status, fmt.Sprint("h", n), at, coerced)
	}
	for range 3 {
		ticket("blocking", "open", "", in)
	}
	ticket("blocking", "answered", "", in) // not open
	ticket("blocking", "open", "", before) // before the window
	for range 3 {
		ticket("assume", "open", "", in)
	}
	for range 2 {
		ticket("assume", "open", "blocking", in) // coerced: 5 assumptions, 2 of them coerced
	}
	ticket("assume", "confirmed", "blocking", in) // coerced but no longer open
	ticket("assume", "open", "", before)
	ticket("assume", "open", "blocking", after)
	for i := range 4 {
		statusExec(t, db, `INSERT INTO reviews (id, task_id, summary, tests_status, status, created_at, updated_at) VALUES (?, ?, 's', 'passed', 'pending', ?, ?)`, fmt.Sprintf("R-%d", i+1), fmt.Sprintf("T-%d", i+1), in, in)
	}
	statusExec(t, db, `INSERT INTO reviews (id, task_id, summary, tests_status, status, created_at, updated_at) VALUES ('R-9', 'T-9', 's', 'passed', 'merged', ?, ?)`, in, in)
	for i, status := range []string{"queued", "queued", "queued", "queued", "queued", "held", "held", "delivered", "canceled"} {
		statusExec(t, db, `INSERT INTO messages (id, text, kind, routed_by, status, created_at) VALUES (?, 'x', 'instruction', 'send', ?, ?)`, fmt.Sprintf("M-%d", i+1), status, in)
	}
	cost := func(session string, at time.Time, usd float64) {
		statusExec(t, db, `INSERT INTO cost_samples (at, session_id, cost_usd) VALUES (?, ?, ?)`, awayStamp(at), session, usd)
	}
	cost("c1", awayT0.Add(-time.Hour), 1)
	cost("c1", awayT0.Add(20*time.Minute), 3.5)
	cost("c1", awayT0.Add(40*time.Minute), 4.5) // +3.50
	cost("c2", awayT0.Add(10*time.Minute), 0.25)
	cost("c2", awayT0.Add(50*time.Minute), 2.25) // +2.25
	cost("c3", awayT0.Add(2*time.Hour), 90)      // after the window
	cost("c4", awayT0.Add(-time.Hour), 5)
	cost("c4", awayT0.Add(30*time.Minute), 5.4) // +0.40
	cost("c5", awayT0.Add(-time.Hour), 7)
	cost("c5", awayT0.Add(30*time.Minute), 7) // no growth
	for _, u := range []struct {
		win string
		at  time.Time
		pct float64
	}{
		{"five_hour", awayT0.Add(10 * time.Minute), 10},
		{"five_hour", awayT0.Add(50 * time.Minute), 42},
		{"five_hour", awayT0.Add(2 * time.Hour), 99},
		{"seven_day", awayT0.Add(50 * time.Minute), 17},
	} {
		statusExec(t, db, `INSERT INTO usage_samples (at, window, used_pct) VALUES (?, ?, ?)`, awayStamp(u.at), u.win, u.pct)
	}
	out, err := runRoot(t, "back")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  3 blocking ticket(s) raised while away; 5 assumption(s) to confirm (2 of them were blocking and became assumptions): curia decisions ls\n",
		"  4 review(s) waiting; 7 message(s) queued\n",
		"Spend: cost +$6.15; usage windows at 42% (5 hours), 17% (7 days)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

// awayTyped stores a message typed into the pane (delivered by send-keys) for session sid.
func awayTyped(t *testing.T, id string, deliveredAt, confirmedAt time.Time) {
	t.Helper()
	conf := any(nil)
	if !confirmedAt.IsZero() {
		conf = awayStamp(confirmedAt)
	}
	statusExec(t, os.Getenv("CURIA_DB"), `INSERT INTO messages (id, text, kind, routed_by, status, delivered_via, created_at, delivered_at, confirmed_at, target_session)
VALUES (?, 'x', 'instruction', 'send', 'delivered', 'send_keys', ?, ?, ?, ?)`, "M-"+id, awayStamp(awayT0), awayStamp(deliveredAt), conf, id)
}

// A typed leaving message makes an agent ready only once the agent confirmed it, not when it was typed.
func TestAwayTypedMessageCountsOnConfirmationNotDelivery(t *testing.T) {
	st, _ := awayEnv(t, awaySess("typed1", "idle", ""))
	ctx := context.Background()
	s, _, _ := st.GetSession(ctx, "typed1")
	s.UpdatedAt = awayT0.Add(2 * time.Minute) // its latest turn ended after the message was typed
	if err := st.UpsertSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	notReady := func() string {
		nr, err := awayNotReady(ctx, st, map[string]string{"typed1": "M-typed1"})
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(nr, "|")
	}
	awayTyped(t, "typed1", awayT0.Add(time.Minute), time.Time{})
	if got := notReady(); got != "typed1 (has not received the message)" {
		t.Errorf("typed, not confirmed: %q, want not ready (has not received the message)", got)
	}
	statusExec(t, os.Getenv("CURIA_DB"), `UPDATE messages SET confirmed_at = ? WHERE id = 'M-typed1'`, awayStamp(awayT0.Add(90*time.Second)))
	if got := notReady(); got != "" {
		t.Errorf("confirmed before the turn ended: %q, want ready", got)
	}
	statusExec(t, os.Getenv("CURIA_DB"), `UPDATE messages SET confirmed_at = ? WHERE id = 'M-typed1'`, awayStamp(awayT0.Add(3*time.Minute)))
	if got := notReady(); got != "typed1 (has not ended its turn)" {
		t.Errorf("confirmed after the last turn end: %q, want not ready (has not ended its turn)", got)
	}
}

// A session that ended (or went stale) after being asked is not named as not ready.
func TestAwayEndedOrStaleSessionCountsAsReady(t *testing.T) {
	st, _ := awayEnv(t, awaySess("gone1", "ended", ""), awaySess("old1", "stale", ""))
	ids := map[string]string{}
	for _, sid := range []string{"gone1", "old1"} { // typed and confirmed, and the session has not updated since
		awayTyped(t, sid, awayT0.Add(time.Minute), awayT0.Add(time.Minute))
		ids[sid] = "M-" + sid
	}
	nr, err := awayNotReady(context.Background(), st, ids)
	if err != nil || len(nr) != 0 {
		t.Errorf("not ready = %q, %v; want none for an ended and a stale session", nr, err)
	}
}

// The readiness wait ends at its deadline on the injected clock; a broken deadline fails here by name, not at the package timeout.
func TestAwayWaitStopsAtItsDeadline(t *testing.T) {
	st, now := awayEnv(t, awaySess("stuck2", "waiting", ""))
	awayTyped(t, "stuck2", awayT0, awayT0)
	sleeps := 0
	awaySleep = func(_ context.Context, d time.Duration) {
		if sleeps++; sleeps > 100 {
			t.Fatalf("awayWait still waiting after %d polls (%v on the clock): its deadline does not end it", sleeps, now.Sub(awayT0))
		}
		*now = now.Add(d)
	}
	got, err := awayWait(context.Background(), st, map[string]string{"stuck2": "M-stuck2"}, time.Minute)
	if err != nil || len(got) != 1 || !strings.HasPrefix(got[0], "stuck2") {
		t.Errorf("awayWait = %q, %v; want stuck2 named as not ready", got, err)
	}
	if want := int(time.Minute / awayPoll); sleeps != want {
		t.Errorf("polls = %d, want %d (a minute at %v)", sleeps, want, awayPoll)
	}
}

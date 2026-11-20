package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	latgate "github.com/ZachFara/curia/internal/latency"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func TestStatusPrintsBadge(t *testing.T) {
	s := func(id string, state model.State, pane string) model.Session {
		return model.Session{ID: id, Agent: "claude", State: state, Pane: pane, UpdatedAt: at(11, 0)}
	}
	fleetHome(t, nil,
		s("a", model.StateNeedsDecision, "%1"),
		s("b", model.StateNeedsDecision, ""), // outside tmux still counts
		s("c", model.StateWaiting, "%3"),
		s("d", model.StateWorking, "%4"),
		s("e", model.StateIdle, "%5"),
		s("f", model.StateEnded, "%6"),
	)
	// needs_decision sessions are not counted: D counts tickets (section 4.10).
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Errorf("status = %q, %v, want \"W1\" with no newline", out, err)
	}
}

func TestStatusPrintsNothingWhenQuiet(t *testing.T) {
	fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1"})
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status = %q, %v, want empty output", out, err)
	}
}

func TestStatusStillSilentOnError(t *testing.T) {
	fleetHome(t, nil)
	t.Setenv("CURIA_DB", t.TempDir()) // a directory is not a database
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status with a broken store = %q, %v, want empty output and exit 0", out, err)
	}
	if out, err := runRoot(t, "status", "extra", "--bogus"); err != nil || out != "" {
		t.Errorf("status with bad arguments = %q, %v, want empty output and exit 0", out, err)
	}

	// The sessions read works but the ticket count fails.
	db, _ := fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("DROP TABLE decisions"); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status with a broken decisions table = %q, %v, want empty output and exit 0", out, err)
	}
}

// statusSession is session s1 in state, in live pane %1.
func statusSession(state model.State) model.Session {
	s := decideSessionS1()
	s.State = state
	return s
}

func TestBadgeCountsTicketsNotSessions(t *testing.T) {
	const z = ".000000000Z"
	db := decisionsHome(t,
		decisionsSpec{"first", model.ModeBlocking, "2026-11-11T08:00:00" + z, "", decisionsEnd{}},
		decisionsSpec{"second", model.ModeBlocking, "2026-11-11T09:00:00" + z, "", decisionsEnd{}},
	)
	seedSessions(t, db, statusSession(model.StateWorking))
	if out, err := runRoot(t, "status"); err != nil || out != "D2" {
		t.Errorf("status = %q, %v, want \"D2\"", out, err)
	}
}

func TestBadgeIgnoresAssumeAndPastDeadline(t *testing.T) {
	const z = ".000000000Z"
	for name, specs := range map[string][]decisionsSpec{
		"assume only": {
			{"live", model.ModeAssume, "2026-11-11T08:00:00" + z, "2026-11-12T08:00:00" + z, decisionsEnd{}},
			{"past deadline", model.ModeAssume, "2026-11-11T09:00:00" + z, "2026-11-11T11:00:00" + z, decisionsEnd{}},
		},
		// D-4's answer is queued, which Q counts (Ruling 89).
		"none open": decisionsSpecs()[2:],
	} {
		t.Run(name, func(t *testing.T) {
			db := decisionsHome(t, specs...)
			seedSessions(t, db, statusSession(model.StateNeedsDecision))
			want := map[string]string{"assume only": "", "none open": "Q1"}[name]
			if out, err := runRoot(t, "status"); err != nil || out != want {
				t.Errorf("status = %q, %v, want %q: no D part", out, err, want)
			}
			if name != "assume only" {
				return
			}
			// The badge is a pure read: it never sweeps expiry.
			if d := decisionsGet(t, db, "D-2"); d.Status != model.StatusOpen {
				t.Errorf("D-2 = %s after status, want still open", d.Status)
			}
		})
	}
}

// Ruling 77: a denied prompt fires no hook, so status settles the session
// from its transcript and the W it held goes away.
func TestStatusDropsWAfterDenial(t *testing.T) {
	path := settleTranscript(t, settleToolUse)
	db, fake := fleetHome(t, nil, model.Session{
		ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1", TmuxWindow: "@1",
		TranscriptPath: path, UpdatedAt: at(11, 0),
	})
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Fatalf("status before the denial = %q, %v, want \"W1\"", out, err)
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(settleDenial); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status after the denial = %q, %v, want empty output", out, err)
	}
	s := getSession(t, db, "a")
	if s.State != model.StateIdle || !s.SeenAt.Equal(fleetNow) || !s.UpdatedAt.Equal(fleetNow) || s.LastUpdate != "interrupted" {
		t.Errorf("a = %s seen %v updated %v %q, want idle, seen and updated at %v, \"interrupted\"",
			s.State, s.SeenAt, s.UpdatedAt, s.LastUpdate, fleetNow)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("status called tmux: %q", fake.Calls)
	}
}

// Settling is a correction: a transcript status cannot read, or a path
// that is not a regular file (one that never ends, a FIFO that would block
// an open), leaves that session as it is, and the badge is still printed.
func TestStatusIgnoresUnreadableTranscript(t *testing.T) {
	waiting := func(id, path string) model.Session {
		return model.Session{ID: id, Agent: "claude", State: model.StateWaiting, Pane: "%1", TranscriptPath: path, UpdatedAt: at(11, 0)}
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fleetHome(t, nil,
		waiting("a", settleUnreadable(t)), waiting("c", "/dev/zero"), waiting("d", t.TempDir()), waiting("e", fifo),
		settleDenied(t, "b", model.StateWaiting, "%2"),
	)
	if out, err := runRoot(t, "status"); err != nil || out != "W4" {
		t.Errorf("status = %q, %v, want \"W4\"", out, err)
	}
	if log := readLog(t); log != nil {
		t.Errorf("status logged %q, want nothing", log)
	}
}

// Ruling 79: a marker from before the session's last hook event does not
// settle it, so the W stays.
func TestStatusKeepsWForOlderMarker(t *testing.T) {
	s := settleDenied(t, "a", model.StateWaiting, "%1")
	s.UpdatedAt = at(11, 45) // the Notification came after the 11:30 marker
	db, _ := fleetHome(t, nil, s)
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Errorf("status = %q, %v, want \"W1\"", out, err)
	}
	if got := getSession(t, db, "a"); got.State != model.StateWaiting || !got.UpdatedAt.Equal(at(11, 45)) {
		t.Errorf("a = %s at %v, want waiting at 11:45", got.State, got.UpdatedAt)
	}
}

// statusMessage stores one message for target in the database at db.
func statusMessage(t *testing.T, db, target string, kind model.MessageKind, status model.MessageStatus) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	m := model.Message{
		Text: "use pnpm", Kind: kind, Sender: model.SenderUser, TargetSession: target,
		RoutedBy: model.RoutedSend, Status: status, CreatedAt: at(11, 0),
	}
	if status == model.MsgHeld {
		m.HoldUntil = at(11, 1)
	}
	if _, err := st.CreateMessage(context.Background(), m); err != nil {
		t.Fatal(err)
	}
}

func TestStatusCountsUndeliveredMessages(t *testing.T) {
	// D-4 holds a queued answer for s1; two queued instructions join it.
	db := decisionsHome(t, decisionsSpecs()[3])
	seedSessions(t, db, statusSession(model.StateIdle))
	statusMessage(t, db, "s1", model.KindInstruction, model.MsgQueued)
	statusMessage(t, db, "s1", model.KindInstruction, model.MsgQueued)
	if out, err := runRoot(t, "status"); err != nil || out != "Q3" {
		t.Errorf("status = %q, %v, want \"Q3\"", out, err)
	}
}

func TestStatusIgnoresMessagesNobodyCanCollect(t *testing.T) {
	db := decisionsHome(t)
	ended, stale := statusSession(model.StateEnded), statusSession(model.StateStale)
	ended.ID, ended.Pane, stale.ID, stale.Pane = "gone", "%2", "old", "%3"
	seedSessions(t, db, statusSession(model.StateIdle), ended, stale)
	statusMessage(t, db, "gone", model.KindInstruction, model.MsgQueued)
	statusMessage(t, db, "old", model.KindInstruction, model.MsgQueued)
	statusMessage(t, db, "nobody", model.KindInstruction, model.MsgQueued)
	statusMessage(t, db, "s1", model.KindRule, model.MsgQueued)
	statusMessage(t, db, "s1", model.KindInstruction, model.MsgHeld)
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status = %q, %v, want empty output", out, err)
	}
}

func TestBadgeSaysAutoOff(t *testing.T) {
	db := decisionsHome(t, decisionsSpec{"first", model.ModeBlocking, "2026-11-11T08:00:00.000000000Z", "", decisionsEnd{}})
	seedSessions(t, db, statusSession(model.StateWorking))
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if out, err := runRoot(t, "status"); err != nil || out != "D1" {
		t.Fatalf("status before the trip = %q, %v, want \"D1\"", out, err)
	}
	if err := st.SetRoutingTripped(ctx, at(11, 0)); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, "status"); err != nil || out != "D1 auto-off" {
		t.Errorf("status after the trip = %q, %v, want \"D1 auto-off\"", out, err)
	}
	if err := st.ResetRouting(ctx); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, "status"); err != nil || out != "D1" {
		t.Errorf("status after the reset = %q, %v, want \"D1\"", out, err)
	}
}

func TestStatusStillSilentOnMessageQueryError(t *testing.T) {
	db, _ := fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("DROP TABLE messages"); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status with a broken messages table = %q, %v, want empty output and exit 0", out, err)
	}
}

// statusReviewTask creates a task and opens a pending review for it, and
// returns the task id.
func statusReviewTask(t *testing.T, db, title string) string {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	task, err := st.CreateTask(ctx, model.Task{
		Title: title, Repo: "/src/curia", Status: model.TaskReady, Source: model.SourceLocal, CreatedAt: at(9, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CompleteTask(ctx, model.Review{
		TaskID: task.ID, SessionID: "s-" + task.ID, Summary: "done " + title, Tests: model.TestsPassed, CreatedAt: at(10, 0),
	}); err != nil {
		t.Fatal(err)
	}
	return task.ID
}

// statusExec runs one statement on the database behind db.
func statusExec(t *testing.T, db, stmt string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(stmt, args...); err != nil {
		t.Fatal(err)
	}
}

func TestStatusCountsPendingReviews(t *testing.T) {
	db := decisionsHome(t)
	statusReviewTask(t, db, "one")
	statusReviewTask(t, db, "two")
	if out, err := runRoot(t, "status"); err != nil || out != "R2" {
		t.Errorf("status = %q, %v, want \"R2\"", out, err)
	}
}

func TestStatusCountsReviewsAfterDecisions(t *testing.T) {
	db := decisionsHome(t, decisionsSpec{"first", model.ModeBlocking, "2026-11-11T08:00:00.000000000Z", "", decisionsEnd{}})
	seedSessions(t, db, statusSession(model.StateWorking))
	statusReviewTask(t, db, "one")
	statusReviewTask(t, db, "two")
	if out, err := runRoot(t, "status"); err != nil || out != "D1 R2" {
		t.Errorf("status = %q, %v, want \"D1 R2\"", out, err)
	}
}

func TestStatusIgnoresClosedAndDroppedReviews(t *testing.T) {
	db := decisionsHome(t)
	closed := statusReviewTask(t, db, "closed")
	dropped := statusReviewTask(t, db, "dropped")
	statusReviewTask(t, db, "open")
	statusExec(t, db, "UPDATE reviews SET status = 'merged', closed_at = ? WHERE task_id = ?", "2026-11-11T10:30:00.000000000Z", closed)
	statusExec(t, db, "UPDATE tasks SET status = 'dropped' WHERE id = ?", dropped)
	if out, err := runRoot(t, "status"); err != nil || out != "R1" {
		t.Errorf("status = %q, %v, want \"R1\"", out, err)
	}
}

func TestStatusStillSilentOnReviewQueryError(t *testing.T) {
	db, _ := fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	statusExec(t, db, "DROP TABLE reviews")
	if out, err := runRoot(t, "status"); err != nil || out != "" {
		t.Errorf("status with a broken reviews table = %q, %v, want empty output and exit 0", out, err)
	}
}

func TestStatusDoesNotCallTmuxForReviews(t *testing.T) {
	db, fake := fleetHome(t, []string{"%1"})
	statusReviewTask(t, db, "one")
	if out, err := runRoot(t, "status"); err != nil || out != "R1" {
		t.Errorf("status = %q, %v, want \"R1\"", out, err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", fake.Calls)
	}
}

const statusForecastBadge = "5h 62% → limit ~43m (resets 1h50m)"

func TestStatusShowsForecastWarning(t *testing.T) {
	db, _ := fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	usageSeries(t, db, fleetNow.Add(110*time.Minute), 50, 56, 62)
	if out, err := runRoot(t, "status"); err != nil || out != "W1 "+statusForecastBadge {
		t.Errorf("status = %q, %v, want %q", out, err, "W1 "+statusForecastBadge)
	}
}

func TestStatusNoForecastWithoutWarning(t *testing.T) {
	db, _ := fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	usageSeries(t, db, fleetNow.Add(110*time.Minute), 40, 40, 40) // steady
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Errorf("steady: status = %q, %v, want \"W1\"", out, err)
	}
	db, _ = fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	usageSeries(t, db, fleetNow.Add(20*time.Minute), 50, 56, 62) // resets first
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Errorf("resets first: status = %q, %v, want \"W1\"", out, err)
	}
}

func TestStatusForecastAloneWhenNoCounts(t *testing.T) {
	db, _ := fleetHome(t, nil)
	usageSeries(t, db, fleetNow.Add(110*time.Minute), 50, 56, 62)
	if out, err := runRoot(t, "status"); err != nil || out != statusForecastBadge {
		t.Errorf("status = %q, %v, want %q", out, err, statusForecastBadge)
	}
}

func TestStatusStillSilentOnUsageError(t *testing.T) {
	db, _ := fleetHome(t, nil, model.Session{ID: "a", Agent: "claude", State: model.StateWaiting, Pane: "%1"})
	usageSeries(t, db, fleetNow.Add(110*time.Minute), 50, 56, 62)
	statusExec(t, db, "DROP TABLE usage_samples")
	if out, err := runRoot(t, "status"); err != nil || out != "W1" {
		t.Errorf("status = %q, %v, want the counts alone and no error text", out, err)
	}
}

// TestStatusLatencyCostOfTheExtraRead reports what the forecast read adds to
// curia status on a store with 3000 tickets and 20000 samples, and gates the
// read's own p95. (The brief's name is TestStatusCostOfTheExtraRead;
// "Latency" is what the test step selects latency gates by.)
func TestStatusLatencyCostOfTheExtraRead(t *testing.T) {
	db := decisionsHome(t)
	clock = func() time.Time { return fleetNow }
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := raw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3000 { // one open blocking ticket each, as CreateDecision stores them
		if _, err = tx.ExecContext(context.Background(), `
INSERT INTO decisions (id, session_id, question, options, recommendation, rationale, reversibility, mode, scope, status, args_hash, created_at)
VALUES (?, 's1', ?, '[{"key":"A","label":"a","tradeoffs":""}]', 'A', 'r', 'costly', 'blocking', 'repo', 'open', ?, ?)`,
			fmt.Sprintf("D-%d", i+1), fmt.Sprintf("question %d", i), fmt.Sprintf("h%d", i), "2026-11-11T11:00:00.000000000Z"); err != nil {
			t.Fatal(err)
		}
	}
	const layout = "2006-01-02T15:04:05.000000000Z"
	for i := range 20000 {
		at := fleetNow.Add(-24*time.Hour + time.Duration(i)*4320*time.Millisecond).Format(layout)
		w := "five_hour"
		if i%2 == 1 {
			w = "seven_day"
		}
		if _, err = tx.ExecContext(context.Background(), `INSERT INTO usage_samples (at, window, used_pct, resets_at, session_id) VALUES (?, ?, ?, ?, 'feed')`,
			at, w, float64(i%100), fleetNow.Add(time.Hour).Format(layout)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	const runs = 200
	timed := func(f func()) (p50, p95 time.Duration) {
		d := make([]time.Duration, 0, runs)
		for range runs {
			start := time.Now()
			f()
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		return d[runs/2], d[runs*95/100]
	}
	if badge, berr := statusBadge(context.Background(), nil); berr != nil || !strings.HasPrefix(badge, "D3000") {
		t.Fatalf("badge = %q, %v, want the 3000 tickets counted", badge, berr)
	}
	b50, b95 := timed(func() {
		if _, berr := statusBadge(context.Background(), nil); berr != nil {
			t.Fatal(berr)
		}
	})
	rd, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rd.Close() }()
	latgate.Gate(t, "the forecast read", 20*time.Millisecond, func() time.Duration {
		p50, p95 := timed(func() { _ = statusForecast(context.Background(), rd, fleetNow) })
		t.Logf("the forecast read alone (statusForecast), %d runs: p50 %v, p95 %v", runs, p50, p95)
		return p95
	})
	t.Logf("statusBadge with 3000 tickets and 20000 samples, %d runs (not gated: the ticket counts it already did cost most of it under -race): p50 %v, p95 %v", runs, b50, b95)
}

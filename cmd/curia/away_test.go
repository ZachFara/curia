package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// bidi is a right-to-left override, built so the source holds no bidirectional control character.
var bidi = string(rune(0x202e))

var awayT0 = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

func awayReset(t *testing.T) {
	t.Helper()
	awayFlags.forDur, awayFlags.repo = 0, ""
	modeReset(t)
	t.Cleanup(func() { awayFlags.forDur, awayFlags.repo = 0, "" })
}

// awayEnv is a terminal user, a clock the test moves, a store with sessions and no sleeping.
func awayEnv(t *testing.T, sessions ...model.Session) (*store.Store, *time.Time) {
	t.Helper()
	awayReset(t)
	prevLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prevLocal })
	db, _ := fleetHome(t, []string{"%1", "%2", "%3"}, sessions...)
	userTerminalAs(t, true)
	now := awayT0
	clock = func() time.Time { return now }
	prevSleep := awaySleep
	awaySleep = func(_ context.Context, d time.Duration) { now = now.Add(d) }
	t.Cleanup(func() { awaySleep = prevSleep })
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, s := range sessions { // the upsert never writes a mode: SetModes does, one repository each
		if s.Mode != "" {
			if _, err = st.SetModes(context.Background(), store.ModeScope{Repo: s.Repo}, s.Mode, time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return st, &now
}

func awaySess(id, state string, mode string) model.Session {
	return model.Session{
		ID: id, Agent: "claude", Repo: "/r/" + id, Cwd: "/r", State: model.State(state), Mode: mode,
		StartedAt: awayT0, UpdatedAt: awayT0, SeenAt: awayT0,
	}
}

func modeIs(t *testing.T, st *store.Store, id, want string) {
	t.Helper()
	if got, _ := modeOf(t, st, id); got != want {
		t.Errorf("session %s mode = %q, want %q", id, got, want)
	}
}

func TestAwayThenBackRestoresEachSessionsPreviousMode(t *testing.T) {
	st, _ := awayEnv(t, awaySess("edit1", "idle", "edits"), awaySess("plain2", "idle", ""),
		awaySess("byp3", "idle", "bypass"), awaySess("gone4", "idle", "edits"))
	out, err := runRoot(t, "away", "6h")
	if err != nil || !strings.Contains(out, "Away until 2026-11-20 15:00") || !strings.Contains(out, "MAY") || !strings.Contains(out, "may NOT") {
		t.Fatalf("away: %q, %v", out, err)
	}
	for _, id := range []string{"edit1", "plain2", "byp3", "gone4"} {
		modeIs(t, st, id, "away")
	}
	// One ends meanwhile; one starts meanwhile and joins; a second away is refused.
	if err = st.UpsertSession(context.Background(), awaySess("gone4", "ended", "")); err != nil {
		t.Fatal(err)
	}
	if err = st.UpsertSession(context.Background(), awaySess("new5", "idle", "")); err != nil {
		t.Fatal(err)
	}
	if err = st.InheritAway(context.Background(), "new5"); err != nil {
		t.Fatal(err)
	}
	modeIs(t, st, "new5", "away")
	if _, err = runRoot(t, "away", "1h"); err == nil || !strings.Contains(err.Error(), "already away") {
		t.Errorf("second away: %v", err)
	}
	out, err = runRoot(t, "back")
	if err != nil || !strings.Contains(out, "Claude Code's own permission mode is NOT restored") {
		t.Fatalf("back: %q, %v", out, err)
	}
	modeIs(t, st, "edit1", "edits")
	modeIs(t, st, "plain2", "")
	modeIs(t, st, "byp3", "") // a saved bypass is never restored
	modeIs(t, st, "new5", "") // started during the period: the default
	if out, err = runRoot(t, "back"); err != nil || !strings.Contains(out, "not away") {
		t.Errorf("second back: %q, %v", out, err)
	}
}

func TestBackKeepsAModeTheUserSetMeanwhile(t *testing.T) {
	st, _ := awayEnv(t, awaySess("a1", "idle", ""))
	if _, err := runRoot(t, "away", "2h"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetModes(context.Background(), store.ModeScope{All: true}, "edits", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "back"); err != nil {
		t.Fatal(err)
	}
	modeIs(t, st, "a1", "edits")
}

func TestAwayDeadlineEndsItOnceWithTwoMonitors(t *testing.T) {
	st, now := awayEnv(t, awaySess("a1", "idle", "edits"))
	if _, err := runRoot(t, "away", "1h"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var notices int
	prevRun, prevOS := mcpRun, mcpGOOS
	mcpRun = func(context.Context, string, ...string) error { mu.Lock(); notices++; mu.Unlock(); return nil }
	mcpGOOS = "darwin"
	t.Cleanup(func() { mcpRun, mcpGOOS = prevRun, prevOS })
	cfg, _ := loadConfig()
	cfg.NotifyMacOS = true
	mk := func() *monitorEnv { return &monitorEnv{st: st, cfg: cfg, now: func() time.Time { return *now }} }
	// Not yet.
	if err := mk().expireAway(context.Background()); err != nil {
		t.Fatal(err)
	}
	modeIs(t, st, "a1", "away")
	*now = now.Add(61 * time.Minute)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _ = mk().expireAway(context.Background()) })
	}
	wg.Wait()
	modeIs(t, st, "a1", "edits")
	if notices != 1 {
		t.Errorf("notices = %d, want 1 (one monitor ends it)", notices)
	}
	if r, ok, _ := st.UnshownAway(context.Background()); !ok {
		t.Fatal("no ended period left for back")
	} else if _, won, _ := st.EndAway(context.Background(), r.ID, *now); won {
		t.Error("a third caller also ended the period: the claim is not once-only")
	}
	out, err := runRoot(t, "back")
	if err != nil || !strings.Contains(out, "ended by itself at its deadline") {
		t.Errorf("back after the deadline: %q, %v", out, err)
	}
}

func TestBackSummaryGroupsDenialsAndNeverShowsRawInput(t *testing.T) {
	st, now := awayEnv(t, awaySess("s1", "idle", ""), awaySess("s2", "idle", ""))
	if _, err := runRoot(t, "away", "2h"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(30 * time.Minute)
	ctx := context.Background()
	evil := "rm \x1b[31m-rf\x1b[0m " + bidi + "evil " + strings.Repeat("A", 1<<20)
	for range 3 {
		_ = st.AddPermissionEvent(ctx, store.PermissionEvent{SessionID: "s1", Mode: "away", Tool: "Bash", Summary: evil, Verdict: "deny", At: *now})
	}
	_ = st.AddPermissionEvent(ctx, store.PermissionEvent{SessionID: "s2", Mode: "away", Tool: "Write", Summary: "/etc/passwd", Verdict: "deny", At: *now})
	_ = st.AddPermissionEvent(ctx, store.PermissionEvent{SessionID: "s2", Mode: "away", Tool: "Bash", Summary: "git status", Verdict: "allow", At: *now})
	out, err := runRoot(t, "back")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Refused (4 call(s)") || !strings.Contains(out, "3x s1") || !strings.Contains(out, "1x s2 Write: /etc/passwd") {
		t.Errorf("denials not grouped once each with their count:\n%s", out)
	}
	if strings.ContainsAny(out, "\x1b"+bidi) || strings.Contains(out, "git status") || len(out) > 8000 {
		t.Errorf("summary shows raw or unbounded input (%d bytes):\n%.600s", len(out), out)
	}
}

func TestBackSummaryGolden(t *testing.T) {
	st, now := awayEnv(t, awaySess("s1abcdef", "idle", ""))
	if _, err := runRoot(t, "away", "2h"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	_ = st.AddPermissionEvent(context.Background(), store.PermissionEvent{SessionID: "s1abcdef", Mode: "away", Tool: "Bash", Summary: "git push origin main", Verdict: "deny", At: *now})
	out, err := runRoot(t, "back")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("UPDATE_GOLDEN") != "" {
		_ = os.WriteFile("testdata/back-summary.golden.txt", []byte(out), 0o600)
	}
	want, err := os.ReadFile("testdata/back-summary.golden.txt")
	if err != nil || string(want) != out {
		t.Errorf("summary differs from testdata/back-summary.golden.txt (UPDATE_GOLDEN=1 rewrites it):\n%s", out)
	}
}

func TestAwayAndBackRefuseAnAgent(t *testing.T) {
	st, _ := awayEnv(t, awaySess("a1", "idle", ""))
	t.Setenv("CLAUDECODE", "1")
	for _, args := range [][]string{{"away", "2h"}, {"back"}} {
		if _, err := runRoot(t, args...); err == nil || !strings.Contains(err.Error(), "only the user may") {
			t.Errorf("curia %v as an agent: %v, want a refusal", args, err)
		}
	}
	modeIs(t, st, "a1", "")
	if _, ok, _ := st.OpenAway(context.Background()); ok {
		t.Error("an agent started an away period")
	}
	t.Setenv("CLAUDECODE", "")
	userTerminalAs(t, false)
	if _, err := runRoot(t, "away", "2h"); err == nil {
		t.Error("away without a terminal did not refuse")
	}
}

func TestAwayWaitsForReadinessThenGoesOnNamingWhoWasNot(t *testing.T) {
	st, now := awayEnv(t, awaySess("stuck1", "waiting", ""))
	s, _, _ := st.GetSession(context.Background(), "stuck1")
	s.Pane = "%1"
	if err := st.UpsertSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "curia"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile(home), []byte("[away]\nprepare_wait = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "away", "3h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Not ready when you left: stuck1 (sitting at a permission prompt)") {
		t.Errorf("not named:\n%s", out)
	}
	if got := now.Sub(awayT0); got < time.Minute || got > 2*time.Minute {
		t.Errorf("waited %v on the injected clock, want about the 1m prepare_wait", got)
	}
	modeIs(t, st, "stuck1", "away")
}

func TestAwayNotReadyNeedsTheMessageThenATurnEnd(t *testing.T) {
	st, _ := awayEnv(t, awaySess("done", "idle", ""), awaySess("busy", "idle", ""))
	ctx := context.Background()
	ids := map[string]string{}
	for _, sid := range []string{"done", "busy"} {
		m, err := st.CreateMessage(ctx, model.Message{Text: "x", Kind: model.KindInstruction, Sender: model.SenderUser, TargetSession: sid, RoutedBy: model.RoutedSend, Status: model.MsgQueued, CreatedAt: awayT0})
		if err != nil {
			t.Fatal(err)
		}
		ids[sid] = m.ID
	}
	notReady := func() string {
		nr, err := awayNotReady(ctx, st, ids)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(nr, "|")
	}
	if got := notReady(); !strings.Contains(got, "done (has not received the message)") || !strings.Contains(got, "busy") {
		t.Errorf("undelivered: %q, want both not ready", got)
	}
	for _, sid := range []string{"done", "busy"} { // handed over at the next stop: delivered via check
		if _, err := st.CheckMessages(ctx, sid, awayT0.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if got := notReady(); !strings.Contains(got, "done (has not ended its turn)") {
		t.Errorf("delivered but no turn ended since: %q", got)
	}
	later := awayT0.Add(2 * time.Minute)
	for sid, state := range map[string]model.State{"done": model.StateIdle, "busy": model.StateWaiting} {
		s, _, _ := st.GetSession(ctx, sid)
		s.State, s.UpdatedAt = state, later
		if err := st.UpsertSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if got := notReady(); got != "busy (sitting at a permission prompt)" {
		t.Errorf("after the turns: %q, want only busy, at a permission prompt", got)
	}
}

// A session in away that ends during the period and is resumed after `back`
// (same id) comes back in its previous mode, never away.
func TestSessionThatEndedDuringAwayIsNotAwayWhenResumedAfterBack(t *testing.T) {
	st, _ := awayEnv(t, awaySess("e1", "idle", "edits"), awaySess("e2", "idle", ""))
	ctx := context.Background()
	if _, err := runRoot(t, "away", "2h"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"e1", "e2"} {
		if err := st.UpsertSession(ctx, awaySess(id, "ended", "")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runRoot(t, "back"); err != nil {
		t.Fatal(err)
	}
	modeIs(t, st, "e1", "edits")
	modeIs(t, st, "e2", "")
}

// A session resumed during the period keeps nothing it carried: away now,
// its old mode back at `back`; bypass is never left in force and a mode that
// lapsed meanwhile restores to the default.
func TestSessionResumedDuringAwayIsAwayAndRestoredAtBack(t *testing.T) {
	st, now := awayEnv(t, awaySess("r1", "idle", ""), awaySess("r2", "idle", ""), awaySess("r3", "idle", ""))
	ctx := context.Background()
	for id, m := range map[string]string{"r1": "edits", "r2": "bypass", "r3": "bypass"} {
		until := awayT0.Add(10 * time.Hour)
		if id == "r3" {
			until = awayT0.Add(90 * time.Minute) // lapses during the period
		}
		if _, err := st.SetModes(ctx, store.ModeScope{Repo: "/r/" + id}, m, until); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runRoot(t, "away", "2h"); err != nil {
		t.Fatal(err)
	}
	// They were all put away at the start; a resume re-reads the row as its older mode (Claude Code resumes the same id).
	for id, m := range map[string]string{"r1": "edits", "r2": "bypass", "r3": "bypass"} {
		if _, err := st.SetModes(ctx, store.ModeScope{Repo: "/r/" + id}, m, awayT0.Add(10*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := st.InheritAway(ctx, id); err != nil {
			t.Fatal(err)
		}
		modeIs(t, st, id, "away")
	}
	// r3's saved bypass lapses: the saved entry says until 10h, so it is dropped as bypass anyway.
	*now = now.Add(time.Hour)
	if _, err := runRoot(t, "back"); err != nil {
		t.Fatal(err)
	}
	modeIs(t, st, "r1", "edits")
	modeIs(t, st, "r2", "")
	modeIs(t, st, "r3", "")
}

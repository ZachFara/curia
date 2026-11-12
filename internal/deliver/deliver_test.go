package deliver_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// now is the fixed clock of every test.
var now = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// Delivery notes of plan section 4.9 and Rulings 54 and 58. Every note for
// an answer an idle agent would never collect ends with what the user can do.
const (
	noteWorking  = "agent is working; the Stop hook will deliver it"
	noteWaiting  = "agent is at a prompt of its own; queued until it next stops"
	notePaneGone = "pane gone; tell the agent to call check_decision D-1"
	noteNoTmux   = "tmux unreachable; queued for the Stop hook, or tell the agent to call check_decision D-1"
	noteFailed   = "typing failed; queued for the Stop hook, or tell the agent to call check_decision D-1"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	return openStoreAt(t, filepath.Join(t.TempDir(), "curia.db"))
}

func openStoreAt(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// lockStore takes the database's write lock on a connection of its own,
// as a busy writer in another process would, and returns its release.
func lockStore(t *testing.T, path string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err == nil {
		_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	}
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
			_ = conn.Close()
			_ = db.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// asker is the asking session: needs_decision in live pane %5 of window @7,
// never looked at (so its window is red).
func asker() model.Session {
	return model.Session{
		ID: "s1", Agent: "claude", State: model.StateNeedsDecision, Pane: "%5", TmuxSession: "work",
		TmuxWindow: "@7", StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-30 * time.Minute),
	}
}

// idleScreen is a capture of Claude Code's empty input prompt (live check
// 2), the only screen typed into (Ruling 85).
var idleScreen = mustRead(filepath.Join("testdata", "prompt", "live-idle.txt"))

// mustRead reads a fixture file at package initialisation.
func mustRead(path string) string {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		panic(err)
	}
	return string(b)
}

// livePane is pane %5 on a server started before the asking session.
var livePane = tmux.Pane{ID: "%5", Session: "work", WindowID: "@7", ServerStart: now.Add(-2 * time.Hour)}

func seedSession(t *testing.T, st *store.Store, s model.Session) {
	t.Helper()
	if err := st.UpsertSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// seedTicket stores an open ticket of session s1 (options A "Postgres
// table" and B, recommendation A) whose option A has label, and returns it.
func seedTicket(t *testing.T, st *store.Store, mode model.Mode, label string) model.Decision {
	t.Helper()
	return seedTicketFor(t, st, "s1", mode, label)
}

func seedTicketFor(t *testing.T, st *store.Store, session string, mode model.Mode, label string) model.Decision {
	t.Helper()
	d := model.Decision{
		SessionID: session, Repo: "/src/curia", Question: "Which store for events?",
		Options: []model.Option{
			{Key: "A", Label: label, Tradeoffs: "Durable."},
			{Key: "B", Label: "Redis", Tradeoffs: "Fast."},
		},
		Recommendation: "A", Rationale: "Replays need history.", Reversibility: model.ReversCostly,
		Mode: mode, Scope: model.ScopeRepo, Status: model.StatusOpen, Delivery: model.DeliveryNone,
		ArgsHash: fmt.Sprintf("hash-%s-%s-%s", session, mode, label), CreatedAt: now.Add(-time.Minute),
	}
	if mode == model.ModeAssume {
		d.Assumption = "Postgres table"
		d.ExpiresAt = now.Add(time.Hour)
	}
	stored, _, err := st.CreateDecision(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func testLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// answerA is the blocking answer of most tests.
var answerA = model.Answer{Key: "A", Reasoning: "we already run it", Via: model.AnsweredCLI}

func getDecision(t *testing.T, st *store.Store, id string) model.Decision {
	t.Helper()
	d, found, err := st.GetDecision(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetDecision(%s) = found %v, err %v", id, found, err)
	}
	return d
}

func getSession(t *testing.T, st *store.Store, id string) model.Session {
	t.Helper()
	s, found, err := st.GetSession(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s) = found %v, err %v", id, found, err)
	}
	return s
}

// sendKeysCalls are the SendKeys calls among calls.
func sendKeysCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "SendKeys") {
			out = append(out, c)
		}
	}
	return out
}

// wantQueued checks that out and the stored row say the answer to id is
// still queued, with note.
func wantQueued(t *testing.T, st *store.Store, out model.Outcome, id, note string) {
	t.Helper()
	if out.Note != note {
		t.Errorf("note = %q, want %q", out.Note, note)
	}
	if out.Decision.Delivery != model.DeliveryQueued || out.Decision.DeliveredVia != "" {
		t.Errorf("outcome row delivery = %s via %q, want queued", out.Decision.Delivery, out.Decision.DeliveredVia)
	}
	if d := getDecision(t, st, id); d.Delivery != model.DeliveryQueued || d.DeliveredVia != "" || !d.DeliveredAt.IsZero() {
		t.Errorf("stored delivery = %s via %q at %v, want queued", d.Delivery, d.DeliveredVia, d.DeliveredAt)
	}
}

// wantWarnNoError checks the log has a WARN line and no ERROR line.
func wantWarnNoError(t *testing.T, log *bytes.Buffer) {
	t.Helper()
	if !strings.Contains(log.String(), "level=WARN") {
		t.Errorf("log has no WARN line:\n%s", log)
	}
	if strings.Contains(log.String(), "level=ERROR") {
		t.Errorf("log has an ERROR line:\n%s", log)
	}
}

func TestIdleSessionGetsTypedLine(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	want := "SendKeys %5 Decision D-1 answered: option A, Postgres table. Reasoning: we already run it. Continue."
	if got := sendKeysCalls(fake.Calls); !slices.Equal(got, []string{want}) {
		t.Errorf("SendKeys calls = %q, want %q", got, want)
	}
	if out.Note != "" {
		t.Errorf("note = %q, want none", out.Note)
	}
	stored := getDecision(t, st, d.ID)
	if stored.Delivery != model.DeliveryDelivered || stored.DeliveredVia != model.ViaSendKeys || !stored.DeliveredAt.Equal(now) {
		t.Errorf("stored delivery = %s via %s at %v, want delivered via send_keys at now", stored.Delivery, stored.DeliveredVia, stored.DeliveredAt)
	}
	if out.Decision.DeliveredVia != model.ViaSendKeys || out.Decision.Status != model.StatusAnswered {
		t.Errorf("outcome row = %s via %s, want answered via send_keys", out.Decision.Status, out.Decision.DeliveredVia)
	}
	if s := getSession(t, st, "s1"); s.State != model.StateIdle {
		t.Errorf("session = %s, want idle", s.State)
	}
}

func TestNeverTypesIntoWaitingOrWorking(t *testing.T) {
	for _, c := range []struct {
		state model.State
		note  string
	}{{model.StateWaiting, noteWaiting}, {model.StateWorking, noteWorking}} {
		t.Run(string(c.state), func(t *testing.T) {
			st := openStore(t)
			s := asker()
			s.State = c.state
			seedSession(t, st, s)
			d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
			fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
			log, _ := testLog()
			out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := sendKeysCalls(fake.Calls); len(got) != 0 {
				t.Errorf("typed into a %s session: %q", c.state, got)
			}
			wantQueued(t, st, out, d.ID, c.note)
		})
	}
}

// racingTmux lists the live panes, then moves session s1 to state in the
// store, as a hook would between the listing and the claim.
type racingTmux struct {
	*tmux.Fake
	st    *store.Store
	state model.State
}

func (r racingTmux) ListPanes(ctx context.Context) ([]tmux.Pane, error) {
	panes, err := r.Fake.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	err = r.st.UpdateSession(ctx, "s1", func(s model.Session, _ bool) model.Session {
		s.State = r.state
		return s
	})
	return panes, err
}

func TestStateChangeBeforeClaimIsNotTyped(t *testing.T) {
	for _, c := range []struct {
		state model.State
		note  string
	}{{model.StateWaiting, noteWaiting}, {model.StateWorking, noteWorking}} {
		t.Run(string(c.state), func(t *testing.T) {
			st := openStore(t)
			s := asker()
			s.State = model.StateIdle
			seedSession(t, st, s)
			d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
			fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
			log, _ := testLog()
			out, err := deliver.Answer(context.Background(), st, racingTmux{fake, st, c.state}, log, d.ID, answerA, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := sendKeysCalls(fake.Calls); len(got) != 0 {
				t.Errorf("typed into a session that became %s: %q", c.state, got)
			}
			wantQueued(t, st, out, d.ID, c.note)
		})
	}
}

// claimingTmux lists the live panes, then lets the asking agent's
// check_decision deliver the answer before curia decide claims it.
type claimingTmux struct {
	*tmux.Fake
	st *store.Store
	id string
}

func (c claimingTmux) ListPanes(ctx context.Context) ([]tmux.Pane, error) {
	panes, err := c.Fake.ListPanes(ctx)
	if err != nil {
		return nil, err
	}
	_, _, err = c.st.CheckDecision(ctx, c.id, "s1", now)
	return panes, err
}

// An answer another path delivered first is not typed, and the outcome
// says it was delivered, by that path.
func TestAnswerDeliveredElsewhereIsNotTyped(t *testing.T) {
	st := openStore(t)
	s := asker()
	s.State = model.StateIdle
	seedSession(t, st, s)
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, claimingTmux{fake, st, d.ID}, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed an answer check_decision delivered: %q", got)
	}
	if out.Note != "" || out.Decision.DeliveredVia != model.ViaCheck {
		t.Errorf("outcome = note %q, via %q, want delivered via check", out.Note, out.Decision.DeliveredVia)
	}
}

func TestPaneGoneLeavesQueued(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Panes: []tmux.Pane{{ID: "%6", WindowID: "@8", ServerStart: livePane.ServerStart}}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed into a gone pane: %q", got)
	}
	if s := getSession(t, st, "s1"); s.State != model.StateStale {
		t.Errorf("session = %s, want stale", s.State)
	}
	wantQueued(t, st, out, d.ID, notePaneGone)
}

func TestRecycledPaneIsNotTyped(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	recycled := livePane
	recycled.ServerStart = now.Add(-10 * time.Minute) // after the session started
	fake := &tmux.Fake{Panes: []tmux.Pane{recycled}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed into a recycled pane id: %q", got)
	}
	if s := getSession(t, st, "s1"); s.State != model.StateStale {
		t.Errorf("session = %s, want stale", s.State)
	}
	wantQueued(t, st, out, d.ID, notePaneGone)
}

func TestSessionOutsideTmuxLeavesQueued(t *testing.T) {
	st := openStore(t)
	s := asker()
	s.State, s.Pane, s.TmuxSession, s.TmuxWindow = model.StateIdle, "", "", ""
	seedSession(t, st, s)
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed for a session with no pane: %q", got)
	}
	wantQueued(t, st, out, d.ID, notePaneGone)
}

// Ended and stale sessions are never typed into, even with a live pane id.
func TestEndedOrStaleSessionLeavesQueued(t *testing.T) {
	for _, state := range []model.State{model.StateEnded, model.StateStale} {
		t.Run(string(state), func(t *testing.T) {
			st := openStore(t)
			s := asker()
			s.State = state
			seedSession(t, st, s)
			d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
			fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
			log, _ := testLog()
			out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := sendKeysCalls(fake.Calls); len(got) != 0 {
				t.Errorf("typed into a %s session: %q", state, got)
			}
			wantQueued(t, st, out, d.ID, notePaneGone)
		})
	}
}

// A ticket whose session has no row is never typed for.
func TestUnknownSessionLeavesQueued(t *testing.T) {
	st := openStore(t)
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed for an unknown session: %q", got)
	}
	wantQueued(t, st, out, d.ID, notePaneGone)
}

// downTmux is a tmux client whose server is unreachable.
type downTmux struct{ *tmux.Fake }

func (d downTmux) ListPanes(context.Context) ([]tmux.Pane, error) {
	d.Calls = append(d.Calls, "ListPanes")
	return nil, errors.New("no server running on /tmp/tmux-501/default")
}

func TestTmuxUnreachableLeavesQueued(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, buf := testLog()
	out, err := deliver.Answer(context.Background(), st, downTmux{fake}, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed with tmux unreachable: %q", got)
	}
	wantQueued(t, st, out, d.ID, noteNoTmux)
	wantWarnNoError(t, buf)
}

// A SendKeys error that is not ErrPartialSend means nothing was typed: the
// claim is undone and the answer waits for the Stop hook.
func TestSendKeysFailureRequeues(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}, SendKeysErr: errors.New("tmux send-keys: can't find pane: %5")}
	log, buf := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatalf("Answer returned %v; a delivery failure is not an answer failure", err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 1 {
		t.Errorf("SendKeys calls = %q, want exactly one", got)
	}
	wantQueued(t, st, out, d.ID, noteFailed)
	if out.Decision.Status != model.StatusAnswered {
		t.Errorf("status = %s, want answered", out.Decision.Status)
	}
	wantWarnNoError(t, buf)
}

// Rulings 54 and 58: after ErrPartialSend the line may be in the prompt, so
// the claim is never undone (no requeue, so no second typing by any path)
// and the note says the agent can read the answer with check_decision.
func TestPartialSendIsNotRequeued(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	partial := fmt.Errorf("%w: tmux send-keys: exit status 1", tmux.ErrPartialSend)
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}, SendKeysErr: partial}
	log, buf := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatalf("Answer returned %v; a delivery failure is not an answer failure", err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 1 {
		t.Errorf("SendKeys calls = %q, want exactly one, never a retry", got)
	}
	want := "typing may not have finished: check the agent's pane (tmux session work, window @7); " +
		"press Enter if the line is there, otherwise tell it to call check_decision D-1"
	if out.Note != want {
		t.Errorf("note = %q, want %q", out.Note, want)
	}
	stored := getDecision(t, st, d.ID)
	if stored.Delivery != model.DeliveryDelivered || stored.DeliveredVia != model.ViaSendKeys {
		t.Errorf("stored delivery = %s via %q, want still claimed by send_keys (no requeue)", stored.Delivery, stored.DeliveredVia)
	}
	if out.Decision.DeliveredVia != model.ViaSendKeys {
		t.Errorf("outcome via = %q, want send_keys", out.Decision.DeliveredVia)
	}
	// The Stop hook finds nothing to deliver again.
	err = st.UpdateSessionTx(context.Background(), "s1", func(tx *store.Tx, s model.Session, _ bool) (model.Session, error) {
		ds, cerr := tx.ClaimDeliveries(context.Background(), "s1", now)
		if len(ds) != 0 {
			t.Errorf("Stop hook claimed %d answers after a partial send, want 0", len(ds))
		}
		return s, cerr
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWarnNoError(t, buf)
}

func TestConfirmDeliversNothing(t *testing.T) {
	st := openStore(t)
	s := asker()
	s.State = model.StateIdle
	seedSession(t, st, s)
	d := seedTicket(t, st, model.ModeAssume, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, now)
	if err != nil {
		t.Fatal(err)
	}
	if out.Decision.Status != model.StatusConfirmed || out.Decision.Delivery != model.DeliveryNone {
		t.Errorf("outcome row = %s, %s, want confirmed, none", out.Decision.Status, out.Decision.Delivery)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed a confirmation: %q", got)
	}
	if slices.Contains(fake.Calls, "ListPanes") {
		t.Errorf("listed panes with nothing to deliver: %q", fake.Calls)
	}
	if out.Note != "nothing to deliver" {
		t.Errorf("note = %q, want %q", out.Note, "nothing to deliver")
	}
}

func TestNoSessionDeliversNothing(t *testing.T) {
	st := openStore(t)
	d := seedTicketFor(t, st, "", model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", fake.Calls)
	}
	if out.Note != "no session to deliver to" || out.Decision.Delivery != model.DeliveryNone {
		t.Errorf("outcome = note %q, delivery %s, want no session, none", out.Note, out.Decision.Delivery)
	}
}

func TestOverrideTypesRevertInstruction(t *testing.T) {
	st := openStore(t)
	s := asker()
	s.State = model.StateIdle
	seedSession(t, st, s)
	d := seedTicket(t, st, model.ModeAssume, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, model.Answer{Key: "B", Via: model.AnsweredCLI}, now)
	if err != nil {
		t.Fatal(err)
	}
	got := sendKeysCalls(fake.Calls)
	want := "SendKeys %5 Decision D-1 overridden: use option B, Redis, not your assumption. Revert work based on the old assumption, then continue."
	if !slices.Equal(got, []string{want}) {
		t.Errorf("SendKeys calls = %q, want %q", got, want)
	}
	if out.Decision.Status != model.StatusOverridden || out.Decision.DeliveredVia != model.ViaSendKeys {
		t.Errorf("outcome row = %s via %s, want overridden via send_keys", out.Decision.Status, out.Decision.DeliveredVia)
	}
}

func TestMarkerReappliedAfterAnswer(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker()) // needs_decision, never seen: window @7 is red
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	if _, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fake.Calls, "SetWindowOption @7 @curia_marker ") {
		t.Errorf("tmux calls = %q, want the marker of @7 cleared", fake.Calls)
	}
}

// When reconcile marks another session stale, its window's marker is
// re-applied as well as the asking session's.
func TestMarkerReappliedForReconciledWindows(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	seedSession(t, st, model.Session{
		ID: "s2", Agent: "claude", State: model.StateWaiting, Pane: "%9", TmuxWindow: "@3",
		StartedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute),
	})
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	if _, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SetWindowOption @7 @curia_marker ", "SetWindowOption @3 @curia_marker "} {
		if !slices.Contains(fake.Calls, want) {
			t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
		}
	}
}

// markerFailingTmux fails every marker write.
type markerFailingTmux struct{ *tmux.Fake }

func (markerFailingTmux) SetWindowOption(context.Context, string, string, string) error {
	return errors.New("tmux set-option: server exited unexpectedly")
}

func TestMarkerFailureIsWarnedNotReturned(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, buf := testLog()
	out, err := deliver.Answer(context.Background(), st, markerFailingTmux{fake}, log, d.ID, answerA, now)
	if err != nil {
		t.Fatalf("Answer returned %v for a marker failure", err)
	}
	if out.Note != "" || out.Decision.DeliveredVia != model.ViaSendKeys {
		t.Errorf("outcome = note %q, via %q, want delivered", out.Note, out.Decision.DeliveredVia)
	}
	wantWarnNoError(t, buf)
}

func TestAnswerErrorDeliversNothing(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	_, err := deliver.Answer(context.Background(), st, fake, log, "D-99", answerA, now)
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", fake.Calls)
	}
}

// A second answer to the same ticket is refused by the store and types
// nothing more.
func TestSecondAnswerIsNotDelivered(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	if _, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now); err != nil {
		t.Fatal(err)
	}
	fake.Calls = nil
	if _, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now); !errors.Is(err, store.ErrNotOpen) {
		t.Errorf("second answer err = %v, want store.ErrNotOpen", err)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("second answer tmux calls = %q, want none", fake.Calls)
	}
}

// A partial send in a session with no tmux session or window known still
// tells the user what to do.
func TestPartialSendNoteWithoutWindow(t *testing.T) {
	st := openStore(t)
	s := asker()
	s.TmuxSession, s.TmuxWindow = "", ""
	seedSession(t, st, s)
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}, SendKeysErr: tmux.ErrPartialSend}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	want := "typing may not have finished: check the agent's pane; " +
		"press Enter if the line is there, otherwise tell it to call check_decision D-1"
	if out.Note != want {
		t.Errorf("note = %q, want %q", out.Note, want)
	}
}

// Ruling 73: a pane another live session is bound to (a nested session
// that died without SessionEnd, say) is never typed into.
func TestSharedPaneIsNotTyped(t *testing.T) {
	for _, other := range []model.State{model.StateWorking, model.StateWaiting, model.StateIdle} {
		t.Run(string(other), func(t *testing.T) {
			st := openStore(t)
			seedSession(t, st, asker())
			outer := asker()
			outer.ID, outer.State = "outer", other
			seedSession(t, st, outer)
			d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
			fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
			log, _ := testLog()
			out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := sendKeysCalls(fake.Calls); len(got) != 0 {
				t.Errorf("typed into a pane shared with a %s session: %q", other, got)
			}
			wantQueued(t, st, out, d.ID, "pane is shared by another session; queued for the Stop hook, or tell the agent to call check_decision D-1")
		})
	}
}

// Ruling 84: a session with subagents running is never typed into, since a
// subagent's permission dialog may be on screen; the note says why and what
// the user can do.
func TestSubagentsRunningIsNotTyped(t *testing.T) {
	st := openStore(t)
	s := asker()
	s.SubagentsActive = 1
	seedSession(t, st, s)
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed into a session with a subagent running: %q", got)
	}
	wantQueued(t, st, out, d.ID,
		"agent has subagents running; it gets the answer when they finish, or tell the agent to call check_decision D-1")
}

// Rulings 81 and 86: after a partial send the line may sit in the prompt,
// so the next answer for the session is not typed (it would be glued to the
// line) until a submitted prompt names the first; then it is.
func TestNoTypingOverAnUnsubmittedLine(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	first := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	second := seedTicket(t, st, model.ModeBlocking, "Postgres view")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}, SendKeysErr: tmux.ErrPartialSend}
	log, _ := testLog()
	if _, err := deliver.Answer(context.Background(), st, fake, log, first.ID, answerA, now); err != nil {
		t.Fatal(err)
	}
	fake.SendKeysErr, fake.Calls = nil, nil
	// The partial send left the session idle (AnswerDecision cleared
	// needs_decision), with no hook event since.
	out, err := deliver.Answer(context.Background(), st, fake, log, second.ID, answerA, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 0 {
		t.Errorf("typed over an unsubmitted line: %q", got)
	}
	wantQueued(t, st, out, second.ID, "an answer typed earlier is not confirmed yet (it may sit unsubmitted in the agent's prompt); "+
		"queued for the Stop hook, or tell the agent to call check_decision D-2")

	// The user pressed Enter: the UserPromptSubmit named D-1 (Ruling 86).
	if _, err = st.ConfirmTyped(context.Background(), "s1", decision.TypedMessage(getDecision(t, st, first.ID)), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	third := seedTicket(t, st, model.ModeBlocking, "Postgres log")
	out, err = deliver.Answer(context.Background(), st, fake, log, third.ID, answerA, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 1 || out.Decision.DeliveredVia != model.ViaSendKeys {
		t.Errorf("after a hook event: SendKeys calls %q, via %q; want D-3 typed", got, out.Decision.DeliveredVia)
	}
}

// deadlineTmux records the deadline of the context SendKeys is given.
type deadlineTmux struct {
	*tmux.Fake
	left *time.Duration
}

func (d deadlineTmux) SendKeys(ctx context.Context, pane, text string) error {
	if dl, ok := ctx.Deadline(); ok {
		*d.left = time.Until(dl)
	}
	return d.Fake.SendKeys(ctx, pane, text)
}

// Ruling 81: typing is not squeezed into one second for all three tmux
// steps; SendKeys bounds each step itself and gets the caller's context.
func TestTypingKeepsTheCallersDeadline(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var left time.Duration
	if _, err := deliver.Answer(ctx, st, deadlineTmux{fake, &left}, log, d.ID, answerA, now); err != nil {
		t.Fatal(err)
	}
	if left < 30*time.Second {
		t.Errorf("SendKeys got a deadline %v away, want the caller's minute", left)
	}
}

// goneWindowTmux fails every marker write as tmux does for a closed window.
type goneWindowTmux struct{ *tmux.Fake }

func (goneWindowTmux) SetWindowOption(context.Context, string, string, string) error {
	return fmt.Errorf("%w: tmux set-option: no such window: @7", tmux.ErrNoWindow)
}

// A window that closed is not worth a warning (as for applyMarkers).
func TestMarkerOnGoneWindowIsNotWarned(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, buf := testLog()
	if _, err := deliver.Answer(context.Background(), st, goneWindowTmux{fake}, log, d.ID, answerA, now); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("log warns about a gone window:\n%s", buf)
	}
}

// Ruling 85, defects 1, 2 and 3: the pane is captured after the claim and
// just before typing, and an answer is typed only when the capture shows
// the empty input prompt. A draft, a dialog, a menu, a running turn, a
// screen not recognized or a failed capture types nothing; the answer is
// queued again and the note says why.
func TestDefects123PaneCheckedBeforeTyping(t *testing.T) {
	for _, c := range []struct {
		fixture string
		capErr  error
		why     string
	}{
		{fixture: "built-draft.txt", why: "prompt has a draft; nothing typed"},
		{fixture: "live-dialog-subagent.txt", why: "a dialog or menu is open in the agent's pane; nothing typed"},
		{fixture: "built-rewind.txt", why: "a dialog or menu is open in the agent's pane; nothing typed"},
		{fixture: "live-busy.txt", why: "agent's pane shows a running turn; nothing typed"},
		{fixture: "built-waiting-background.txt", why: "agent is waiting for background subagents; nothing typed"},
		{fixture: "built-shell.txt", why: "prompt not recognized (Claude Code's screen may have changed); nothing typed"},
		{fixture: "live-idle.txt", capErr: errors.New("tmux capture-pane: can't find pane: %5"), why: "could not read the agent's pane; nothing typed"},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			st := openStore(t)
			seedSession(t, st, asker())
			d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
			fake := &tmux.Fake{Screen: mustRead(filepath.Join("testdata", "prompt", c.fixture)), CaptureErr: c.capErr, Panes: []tmux.Pane{livePane}}
			log, _ := testLog()
			out, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := sendKeysCalls(fake.Calls); len(got) != 0 {
				t.Errorf("typed into %s: %q", c.fixture, got)
			}
			if !slices.Contains(fake.Calls, "CapturePane %5") {
				t.Errorf("tmux calls = %q, want the pane captured", fake.Calls)
			}
			wantQueued(t, st, out, d.ID, c.why+"; queued for the Stop hook, or tell the agent to call check_decision D-1")
		})
	}
}

// The capture is the last step before typing: after the claim, right
// before SendKeys.
func TestPaneCapturedRightBeforeTyping(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, _ := testLog()
	if _, err := deliver.Answer(context.Background(), st, fake, log, d.ID, answerA, now); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(fake.Calls, "CapturePane %5")
	if i < 0 || i+1 >= len(fake.Calls) || !strings.HasPrefix(fake.Calls[i+1], "SendKeys %5 ") || fake.Calls[0] != "ListPanes" {
		t.Errorf("tmux calls = %q, want ListPanes, then CapturePane %%5 directly before SendKeys", fake.Calls)
	}
}

// lateTmux's SendKeys fails as a killed copy-mode step does once the
// caller's deadline has passed: a plain error, nothing typed.
type lateTmux struct{ *tmux.Fake }

func (l lateTmux) SendKeys(ctx context.Context, pane, text string) error {
	_ = l.Fake.SendKeys(ctx, pane, text)
	<-ctx.Done()
	return errors.New("tmux copy-mode: signal: killed")
}

// The caller's deadline passing while typing does not stop the requeue,
// nor the marker update after it.
func TestRequeueSurvivesCallerDeadline(t *testing.T) {
	st := openStore(t)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	log, buf := testLog()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out, err := deliver.Answer(ctx, st, lateTmux{fake}, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("setup: the caller's deadline did not pass while typing")
	}
	wantQueued(t, st, out, d.ID, noteFailed)
	if !slices.Contains(fake.Calls, "SetWindowOption @7 @curia_marker ") {
		t.Errorf("tmux calls = %q, want the marker of @7 cleared after the deadline", fake.Calls)
	}
	if strings.Contains(buf.String(), "deadline exceeded") {
		t.Errorf("a step after typing ran on the dead context:\n%s", buf)
	}
}

// closingTmux lists the live panes, then closes the store Answer was given,
// so the next store step fails.
type closingTmux struct {
	*tmux.Fake
	st *store.Store
}

func (c closingTmux) ListPanes(ctx context.Context) ([]tmux.Pane, error) {
	panes, err := c.Fake.ListPanes(ctx)
	if err == nil {
		err = c.st.Close()
	}
	return panes, err
}

// lockingTmux lists the live panes, then has another writer take the
// store's write lock, so the claim's transaction cannot start.
type lockingTmux struct {
	*tmux.Fake
	lock func()
}

func (l lockingTmux) ListPanes(ctx context.Context) ([]tmux.Pane, error) {
	panes, err := l.Fake.ListPanes(ctx)
	l.lock()
	return panes, err
}

// A store error in Reconcile or ClaimTyping types nothing and leaves the
// answer queued.
func TestStoreErrorBeforeClaimTypesNothing(t *testing.T) {
	const noteNoCheck = "could not check the session; queued for the Stop hook, or tell the agent to call check_decision D-1"
	t.Run("reconcile", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "curia.db")
		st := openStoreAt(t, path)
		seedSession(t, st, asker())
		d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
		doomed := openStoreAt(t, path)
		fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
		log, buf := testLog()
		out, err := deliver.Answer(context.Background(), doomed, closingTmux{fake, doomed}, log, d.ID, answerA, now)
		if err != nil {
			t.Fatal(err)
		}
		if got := sendKeysCalls(fake.Calls); len(got) != 0 {
			t.Errorf("typed after a store error: %q", got)
		}
		wantQueued(t, st, out, d.ID, noteNoCheck)
		wantWarnNoError(t, buf)
	})
	t.Run("claim", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "curia.db")
		st := openStoreAt(t, path)
		seedSession(t, st, asker())
		d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
		fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
		var release func()
		lock := func() { release = lockStore(t, path) }
		log, buf := testLog()
		out, err := deliver.Answer(context.Background(), st, lockingTmux{fake, lock}, log, d.ID, answerA, now)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if got := sendKeysCalls(fake.Calls); len(got) != 0 {
			t.Errorf("typed after a failed claim: %q", got)
		}
		wantQueued(t, st, out, d.ID, noteNoCheck)
		if !strings.Contains(buf.String(), "typing claim failed") {
			t.Errorf("log does not name the failed claim:\n%s", buf)
		}
		wantWarnNoError(t, buf)
	})
}

// lockedSendTmux fails SendKeys with a plain error (nothing typed) while
// another writer holds the store's write lock, so the requeue cannot run.
type lockedSendTmux struct {
	*tmux.Fake
	lock func()
}

func (l lockedSendTmux) SendKeys(ctx context.Context, pane, text string) error {
	_ = l.Fake.SendKeys(ctx, pane, text)
	l.lock()
	return errors.New("tmux send-keys: can't find pane: %5")
}

func TestFailedRequeueSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "curia.db")
	st := openStoreAt(t, path)
	seedSession(t, st, asker())
	d := seedTicket(t, st, model.ModeBlocking, "Postgres table")
	fake := &tmux.Fake{Screen: idleScreen, Panes: []tmux.Pane{livePane}}
	var release func()
	lock := func() { release = lockStore(t, path) }
	log, buf := testLog()
	out, err := deliver.Answer(context.Background(), st, lockedSendTmux{fake, lock}, log, d.ID, answerA, now)
	if err != nil {
		t.Fatal(err)
	}
	release()
	want := "typing failed and the answer could not be queued again; tell the agent to call check_decision D-1"
	if out.Note != want {
		t.Errorf("note = %q, want %q", out.Note, want)
	}
	if got := sendKeysCalls(fake.Calls); len(got) != 1 {
		t.Errorf("SendKeys calls = %q, want exactly one", got)
	}
	if strings.Contains(buf.String(), "answer queued again") {
		t.Errorf("log says the answer was queued again although the requeue failed:\n%s", buf)
	}
	wantWarnNoError(t, buf)
}

// realSocket is per run, so concurrent test runs never share a server.
var realSocket = fmt.Sprintf("curia-test-%d", os.Getpid())

// realServer starts a throwaway tmux server with no configuration on its
// own socket (never the default server), whose one pane runs argv in dir,
// and returns a Client on it, a helper running raw tmux commands against
// it, and the pane.
func realServer(t *testing.T, dir string, argv ...string) (tmux.Client, func(args ...string) (string, error), tmux.Pane) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	raw := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-L", realSocket, "-f", os.DevNull}, args...)...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", realSocket, "kill-server").Run() })
	_, _ = raw("kill-server") // clear a leftover server from an aborted run
	args := append([]string{"new-session", "-d", "-s", "t", "-x", "200", "-y", "50", "-c", dir, "--"}, argv...)
	if out, err := raw(args...); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	client := tmux.NewExec(realSocket)
	panes, err := client.ListPanes(context.Background())
	if err != nil || len(panes) != 1 {
		t.Fatalf("ListPanes = %v, %v, want one pane", panes, err)
	}
	return client, raw, panes[0]
}

// realAsker is session s1, idle in pane p, started after p's server.
func realAsker(p tmux.Pane) model.Session {
	return model.Session{
		ID: "s1", Agent: "claude", State: model.StateIdle, Pane: p.ID, TmuxSession: p.Session,
		TmuxWindow: p.WindowID, StartedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

// waitFor polls cond every 20 ms for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForPrompt waits until pane shows the drawn empty prompt.
func waitForPrompt(t *testing.T, raw func(args ...string) (string, error), pane tmux.Pane) {
	t.Helper()
	waitFor(t, "the drawn prompt", func() bool {
		screen, err := raw("capture-pane", "-p", "-t", pane.ID)
		return err == nil && strings.Contains(screen, "auto mode on")
	})
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestTypedLineReachesRealPane(t *testing.T) {
	dir, err := os.MkdirTemp("", "cd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	file := filepath.Join(dir, "typed")
	// The pane draws Claude Code's empty prompt (Ruling 85), then records
	// what is typed.
	client, raw, pane := realServer(t, dir, "/bin/sh", "-c", `printf '%s' "$2"; exec cat > "$1"`, "sh", file, idleScreen)
	waitForPrompt(t, raw, pane)
	st := openStore(t)
	seedSession(t, st, realAsker(pane))
	label := "B $(touch " + filepath.Join(dir, "pwned") + "); echo"
	d := seedTicket(t, st, model.ModeBlocking, label)
	log, buf := testLog()
	out, err := deliver.Answer(context.Background(), st, client, log, d.ID, answerA, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out.Note != "" || out.Decision.DeliveredVia != model.ViaSendKeys {
		t.Fatalf("outcome = note %q, via %q, want delivered by send_keys; log:\n%s", out.Note, out.Decision.DeliveredVia, buf)
	}
	var got string
	waitFor(t, "the typed line", func() bool {
		b, err := os.ReadFile(filepath.Clean(file))
		got = string(b)
		return err == nil && strings.HasSuffix(got, "\n")
	})
	if want := decision.TypedMessage(out.Decision) + "\n"; got != want {
		t.Errorf("pane received %q, want %q", got, want)
	}
	if exists(filepath.Join(dir, "pwned")) {
		t.Error("the hostile label ran a command")
	}
}

// The agent has exited and its pane is a shell, while the store still
// records it idle: the typed line reaches the shell and runs nothing.
func TestHostileTextIsInertInARealShell(t *testing.T) {
	dir, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := func(name string) string { return filepath.Join(dir, name) }
	// The shell is started under a drawn empty Claude Code prompt, with no
	// prompt of its own, so the pane check (Ruling 85) lets the line through.
	client, raw, pane := realServer(t, dir, "/bin/sh", "-c",
		`printf '%s' "$1"; exec /usr/bin/env -i PATH=/usr/bin:/bin HOME="$2" PS1= /bin/sh`, "sh", idleScreen, dir)
	waitForPrompt(t, raw, pane)
	st := openStore(t)
	seedSession(t, st, realAsker(pane))
	d := seedTicket(t, st, model.ModeBlocking, "$(touch "+p("pwned")+")")
	a := model.Answer{
		Key: "A", Via: model.AnsweredCLI,
		Reasoning: "`touch " + p("pwned2") + "` ; touch " + p("pwned3"),
	}
	log, buf := testLog()
	out, err := deliver.Answer(context.Background(), st, client, log, d.ID, a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out.Note != "" || out.Decision.DeliveredVia != model.ViaSendKeys {
		t.Fatalf("outcome = note %q, via %q, want delivered by send_keys; log:\n%s", out.Note, out.Decision.DeliveredVia, buf)
	}
	if o, rerr := raw("send-keys", "-t", pane.ID, "-l", "--", "touch "+p("done")); rerr != nil {
		t.Fatalf("send-keys: %v: %s", rerr, o)
	}
	if o, rerr := raw("send-keys", "-t", pane.ID, "Enter"); rerr != nil {
		t.Fatalf("send-keys Enter: %v: %s", rerr, o)
	}
	waitFor(t, "the shell to run the next command", func() bool { return exists(p("done")) })
	for _, name := range []string{"pwned", "pwned2", "pwned3"} {
		if exists(p(name)) {
			t.Errorf("%s exists: the typed line ran a command", name)
		}
	}
	// The shell ran the typed line as one command named "Decision" and
	// nothing else: exactly one line of the screen says a command was not
	// found, and it is the shell saying so of "Decision".
	screen, err := raw("capture-pane", "-p", "-J", "-t", pane.ID)
	if err != nil {
		t.Fatalf("capture-pane: %v: %s", err, screen)
	}
	var notFound []string
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, "not found") {
			notFound = append(notFound, line)
		}
	}
	if len(notFound) != 1 || !shellNotFound.MatchString(notFound[0]) {
		t.Errorf("pane shows %q, want exactly one line saying the command Decision was not found", screen)
	}
}

// shellNotFound matches a POSIX shell's one line for an unknown command
// named Decision: "sh: Decision: command not found" from bash as sh (macOS)
// and "/bin/sh: 1: Decision: not found" from dash (Debian, Ubuntu).
var shellNotFound = regexp.MustCompile(`^[^ ]+: (?:(?:line )?[0-9]+: )?Decision: (?:command )?not found$`)

// TestShellNotFoundPattern pins what shellNotFound accepts, so the shell
// test proves the same thing on every platform.
func TestShellNotFoundPattern(t *testing.T) {
	for line, want := range map[string]bool{
		"sh: Decision: command not found":           true,
		"/bin/sh: 1: Decision: not found":           true,
		"bash: line 1: Decision: command not found": true,
		"sh: touch: command not found":              false,
		"sh: Decisions: not found":                  false,
		"$ Decision: not found":                     false,
		"sh: Decision: not found; touch":            false,
		"Decision: not found":                       false,
	} {
		if got := shellNotFound.MatchString(line); got != want {
			t.Errorf("shellNotFound(%q) = %v, want %v", line, got, want)
		}
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// fleetNow is the fixed clock of the fleet command tests.
var fleetNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// at returns fleetNow's date at hh:mm UTC.
func at(hh, mm int) time.Time {
	return time.Date(2026, 11, 11, hh, mm, 0, 0, time.UTC)
}

// fleetHome isolates HOME and CURIA_DB (see hookHome), fixes the clock at
// fleetNow, makes the commands use a tmux.Fake (see useFakeTmux) whose
// server has the given panes, and seeds the store. It returns the database
// path and the fake.
func fleetHome(t *testing.T, panes []string, sessions ...model.Session) (string, *tmux.Fake) {
	t.Helper()
	db := hookHome(t)
	fake := useFakeTmux(t)
	for _, id := range panes {
		fake.Panes = append(fake.Panes, tmux.Pane{ID: id, Session: "work", WindowID: "@1"})
	}
	prevClock := clock
	clock = func() time.Time { return fleetNow }
	t.Cleanup(func() { clock, lsJSON = prevClock, false })
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, s := range sessions {
		if err := st.UpsertSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	return db, fake
}

// useDownTmux makes the commands use a tmux client whose server is
// unreachable; tmux.Fake cannot fail, so this replaces it.
func useDownTmux(t *testing.T) {
	t.Helper()
	prev := newTmuxClient
	newTmuxClient = func() tmux.Client { return &downTmux{} }
	t.Cleanup(func() { newTmuxClient = prev })
}

// downTmux is a tmux client whose server is unreachable.
type downTmux struct{ tmux.Fake }

var errNoServer = errors.New("no server running on /tmp/tmux-501/default")

func (*downTmux) ListPanes(context.Context) ([]tmux.Pane, error) { return nil, errNoServer }

func (*downTmux) SwitchTo(context.Context, string) error { return errNoServer }

func (*downTmux) DisplayMessage(context.Context, string) error { return errNoServer }

func getSession(t *testing.T, db, id string) model.Session {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s, found, err := st.GetSession(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s) = found %v, err %v", id, found, err)
	}
	return s
}

// goldenSessions covers every rank group, a session outside tmux, one whose
// pane is gone (marked stale by ls), one stale long enough to be hidden, and
// an ended one.
func goldenSessions() []model.Session {
	s := func(id string, state model.State, pane string, updated time.Time) model.Session {
		return model.Session{
			ID: id, Agent: "claude", Pane: pane, State: state,
			StartedAt: at(9, 0), UpdatedAt: updated,
		}
	}
	full := model.Session{
		ID: "a-decision", Agent: "claude", Pane: "%1", TmuxSession: "work", TmuxWindow: "@1",
		Cwd: "/src/curia", Repo: "curia", Branch: "main", State: model.StateNeedsDecision,
		Model: "claude-opus-5-5", ContextUsed: 120000, ContextMax: 200000, SubagentsActive: 1,
		LastUpdate: `Allow Bash: rm -rf "build"?`, TranscriptPath: "/t/a.jsonl", TranscriptOffset: 4096,
		StartedAt: at(9, 0).Add(123456789 * time.Nanosecond), UpdatedAt: at(11, 45),
	}
	waiting := s("b-waiting", model.StateWaiting, "%2", at(11, 50))
	waiting.SeenAt = at(11, 55)
	idleUnseen := s("c-idle-unseen", model.StateIdle, "%3", at(11, 30))
	idleUnseen.SeenAt = at(11, 0)
	idleSeen := s("e-idle-seen", model.StateIdle, "%5", at(11, 40))
	idleSeen.SeenAt = at(11, 59)
	return []model.Session{
		s("h-ended", model.StateEnded, "%6", at(11, 0)),
		s("g-stale-old", model.StateStale, "%8", at(11, 0)),
		s("g-stale-recent", model.StateStale, "%7", at(11, 55)),
		s("f-pane-gone", model.StateWorking, "%9", at(11, 10)),
		idleSeen,
		s("i-no-pane", model.StateWorking, "", at(11, 58)),
		s("d-working", model.StateWorking, "%4", at(11, 20)),
		idleUnseen,
		waiting,
		full,
	}
}

func TestLsJSONGolden(t *testing.T) {
	db, _ := fleetHome(t, []string{"%1", "%2", "%3", "%4", "%5"}, goldenSessions()...)
	out, err := runRoot(t, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v\n%s", err, out)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "ls.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("ls --json output:\n%s\nwant (testdata/ls.golden.json):\n%s", out, want)
	}
	if s := getSession(t, db, "f-pane-gone"); s.State != model.StateStale || !s.UpdatedAt.Equal(fleetNow) {
		t.Errorf("f-pane-gone in store = %s at %v, want stale at %v", s.State, s.UpdatedAt, fleetNow)
	}
}

func TestLsJSONEmpty(t *testing.T) {
	fleetHome(t, nil)
	out, err := runRoot(t, "ls", "--json")
	if err != nil || out != "[]\n" {
		t.Errorf("ls --json on an empty store = %q, %v, want \"[]\\n\"", out, err)
	}
}

func TestLsTable(t *testing.T) {
	fleetHome(t, []string{"%1", "%2", "%3", "%4", "%5"}, goldenSessions()...)
	out, err := runRoot(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	wantStates := []string{
		"STATE", "needs_decision", "waiting", "idle", "working", "working", "idle", "stale", "stale",
	}
	if len(lines) != len(wantStates) {
		t.Fatalf("ls printed %d lines, want %d:\n%s", len(lines), len(wantStates), out)
	}
	for i, w := range wantStates {
		if f := strings.Fields(lines[i]); len(f) == 0 || f[0] != w {
			t.Errorf("line %d = %q, want state %s", i, lines[i], w)
		}
	}
	for _, want := range []string{"%1", "curia", "main", "15m", `Allow Bash: rm -rf "build"?`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("first row %q is missing %q", lines[1], want)
		}
	}
}

// Ruling 22: the table shows the repo's basename; --json keeps the full path.
func TestLsTableShowsRepoBasename(t *testing.T) {
	fleetHome(t, []string{"%1"}, model.Session{
		ID: "x", Agent: "claude", Pane: "%1", State: model.StateWaiting, UpdatedAt: at(11, 0),
		Repo: "/Users/me/src/curia", Branch: "main",
	})
	out, err := runRoot(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	if f := strings.Fields(strings.Split(out, "\n")[1]); len(f) < 3 || f[2] != "curia" {
		t.Errorf("ls row = %q, want repo column curia", out)
	}
	if out, _ = runRoot(t, "ls", "--json"); !strings.Contains(out, `"repo": "/Users/me/src/curia"`) {
		t.Errorf("ls --json lost the full repo path:\n%s", out)
	}
}

// ls --json prints no raw C1, bidi or other unprintable rune and does not
// HTML-escape, yet stays valid JSON that decodes to the stored strings.
func TestLsJSONEscapesUnprintableRunes(t *testing.T) {
	odd := model.Session{
		ID: "x", Agent: "claude", Pane: "%1", State: model.StateWaiting, UpdatedAt: at(11, 0),
		Repo:       "repo\x1b]0;owned\x07",
		Branch:     "br\u202eanch\u009b2J",
		LastUpdate: "make && ./run <x> \U000E0001 é 世 \u2028",
	}
	fleetHome(t, []string{"%1"}, odd)
	out, err := runRoot(t, "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if r != '\n' && !unicode.IsPrint(r) {
			t.Errorf("ls --json printed the raw rune %U:\n%s", r, out)
		}
	}
	for _, want := range []string{"make && ./run <x>", `\u202e`, `\u009b`, `\udb40\udc01`, "é 世", `\u001b`} {
		if !strings.Contains(out, want) {
			t.Errorf("ls --json lacks %q:\n%s", want, out)
		}
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("ls --json is not valid JSON: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0]["repo"] != odd.Repo || rows[0]["branch"] != odd.Branch || rows[0]["last_update"] != odd.LastUpdate {
		t.Errorf("decoded %q, want the stored strings back", rows)
	}
}

// ls hides sessions stale for longer than stale_hide_after; its help says so.
func TestLsHelpMentionsHiddenStaleSessions(t *testing.T) {
	t.Cleanup(func() { // cobra keeps --help set on the command
		if ls, _, err := rootCmd.Find([]string{"ls"}); err == nil {
			_ = ls.Flags().Set("help", "false")
		}
	})
	out, err := runRoot(t, "ls", "--help")
	if err != nil || !strings.Contains(out, "stale_hide_after") {
		t.Errorf("ls --help = %q, %v, want it to say old stale sessions are hidden", out, err)
	}
}

func TestLsTableStripsControlCharacters(t *testing.T) {
	evil := model.Session{
		ID: "x", Agent: "claude", Pane: "%1", State: model.StateWaiting, UpdatedAt: at(11, 0),
		Repo:       "repo\x1b]0;owned\x07",
		Branch:     "br\u202eanch\u009b2J\x9b", // U+009B is the C1 CSI; \x9b a raw, invalid byte
		LastUpdate: "line one\nline two\r\x1b[2Jcleared\ttab",
	}
	fleetHome(t, []string{"%1"}, evil)
	out, err := runRoot(t, "ls")
	if err != nil {
		t.Fatalf("ls: %v\n%s", err, out)
	}
	if strings.ContainsAny(out, "\x1b\x07\r\t\u202e\u009b") || strings.Contains(out, "\x9b") {
		t.Errorf("ls passed a control character through: %q", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("ls printed %d lines, want a header and one row: %q", n, out)
	}
	if !strings.Contains(out, "line one?line two??[2Jcleared?tab") {
		t.Errorf("last update not kept with control characters replaced: %q", out)
	}
}

func TestLsSkipsReconcileWhenTmuxUnreachable(t *testing.T) {
	db, _ := fleetHome(t, nil, model.Session{
		ID: "a", Agent: "claude", Pane: "%1", State: model.StateWorking, UpdatedAt: at(11, 0),
	})
	useDownTmux(t)
	out, err := runRoot(t, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"state": "working"`) {
		t.Errorf("ls --json = %s, want the session listed as working", out)
	}
	if s := getSession(t, db, "a"); s.State != model.StateWorking {
		t.Errorf("session became %s without a tmux server", s.State)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "no server running") {
		t.Errorf("log = %q, want one line saying reconcile was skipped", log)
	}
}

func TestLsReportsStoreError(t *testing.T) {
	fleetHome(t, nil)
	t.Setenv("CURIA_DB", t.TempDir()) // a directory is not a database
	if _, err := runRoot(t, "ls"); err == nil {
		t.Error("ls returned nil error when the store cannot open")
	}
}

func TestLsShowsInterruptedSessionIdle(t *testing.T) {
	_, fake := fleetHome(t, []string{"%1"}, settleDenied(t, "a", model.StateWorking, "%1"))
	out, err := runRoot(t, "ls")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "idle") || !strings.HasSuffix(lines[1], "interrupted") {
		t.Errorf("ls =\n%s\nwant one idle row with LAST UPDATE interrupted", out)
	}
	if want := []string{"ListPanes", "SetWindowOption @1 @curia_marker "}; !slices.Equal(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
}

// A transcript ls cannot read is logged; the session is listed unchanged.
func TestLsLogsSettleFailure(t *testing.T) {
	fleetHome(t, []string{"%1"}, model.Session{
		ID: "a", Agent: "claude", State: model.StateWorking, Pane: "%1", TranscriptPath: settleUnreadable(t), UpdatedAt: at(11, 0),
	})
	out, err := runRoot(t, "ls", "--json")
	if err != nil || !strings.Contains(out, `"state": "working"`) {
		t.Errorf("ls --json = %s, %v, want the session listed as working", out, err)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "settle") || !strings.Contains(log[0], "session a") {
		t.Errorf("log = %q, want one line naming the session that failed to settle", log)
	}
}

func TestLsJSONHasTaskAndTeamKeys(t *testing.T) {
	sessions := []model.Session{
		{
			ID: "with", Agent: "claude", State: model.StateIdle,
			TaskID: "T-7", Worktree: "/r/.curia-worktrees/t-7", TeamID: "team-a", TeamRole: "lead", TeamMember: "alice",
		},
		{ID: "without", Agent: "claude", State: model.StateIdle},
	}
	var buf bytes.Buffer
	if err := printJSON(&buf, sessions); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	keys := []string{"task_id", "worktree", "team_id", "team_role", "team_member"}
	wantWith := []string{"T-7", "/r/.curia-worktrees/t-7", "team-a", "lead", "alice"}
	for i, k := range keys {
		if got := rows[0][k]; got != wantWith[i] {
			t.Errorf("with: %s = %v, want %q", k, got, wantWith[i])
		}
		if got, ok := rows[1][k]; !ok || got != "" {
			t.Errorf("without: %s = %v (present %v), want empty string", k, got, ok)
		}
	}
}

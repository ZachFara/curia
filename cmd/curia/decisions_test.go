package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// decisionsSpec is one seeded ticket: the open ticket CreateDecision stores,
// then the raw UPDATE that moves it to its state with exact values.
type decisionsSpec struct {
	question string
	mode     model.Mode
	created  string       // store timestamp
	expires  string       // "" for none
	end      decisionsEnd // applied after creation; zero for an open ticket
}

// decisionsEnd is the state a seeded ticket is moved to by a raw UPDATE, so
// every value is exact. An empty time is NULL.
type decisionsEnd struct {
	status, key, answer, reasoning, via, precedent, withdraw, by, delivery, dvia string
	answered, closed, supersededAt, delivered                                    string
}

const decisionsEndSQL = `UPDATE decisions SET status = ?, answer_key = ?, answer = ?, answer_reasoning = ?,
  answered_via = ?, precedent_of = ?, withdraw_reason = ?, superseded_by = ?, delivery = ?, delivered_via = ?,
  answered_at = ?, closed_at = ?, superseded_at = ?, delivered_at = ? WHERE id = ?`

// decisionsNull is s as a SQL value: NULL when empty.
func decisionsNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const decisionsNow = "2026-11-11T12:00:00Z"

// decisionsSpecs seeds seven tickets. Their created_at values are not in id
// order on purpose: ls lists in allocation (id) order, not by age.
func decisionsSpecs() []decisionsSpec {
	const z = ".000000000Z"
	return []decisionsSpec{
		{"Which store for events?", model.ModeBlocking, "2026-11-11T08:00:00.123456789Z", "", decisionsEnd{}},
		{"Run a && b?", model.ModeAssume, "2026-11-11T09:00:00" + z, "2026-11-12T09:00:00" + z, decisionsEnd{}},
		{"Which index?", model.ModeBlocking, "2026-11-11T07:00:00" + z, "", decisionsEnd{
			status: "answered", key: "A", answer: "A: Postgres", reasoning: "fits replays", via: "cli",
			answered: "2026-11-11T07:30:00" + z, delivery: "delivered", dvia: "stop_hook", delivered: "2026-11-11T07:45:00" + z,
		}},
		{"Which cache?", model.ModeAssume, "2026-11-11T06:00:00" + z, "2026-11-12T06:00:00" + z, decisionsEnd{
			status: "overridden", key: "B", answer: "B: Redis", via: "popup",
			answered: "2026-11-11T06:30:00" + z, delivery: "queued",
		}},
		{"Which format?", model.ModeBlocking, "2026-11-11T05:00:00" + z, "", decisionsEnd{
			status: "withdrawn", precedent: "D-3", withdraw: "covered by D-3", closed: "2026-11-11T05:10:00" + z,
		}},
		{"Which timeout?", model.ModeAssume, "2026-11-11T04:00:00" + z, "2026-11-11T04:30:00" + z, decisionsEnd{
			status: "expired", closed: "2026-11-11T04:30:00" + z,
		}},
		{"Which queue?", model.ModeAssume, "2026-11-11T03:00:00" + z, "2026-11-12T03:00:00" + z, decisionsEnd{
			status: "confirmed", key: "A", answer: "A: Postgres", via: "ssh", answered: "2026-11-11T03:20:00" + z,
			delivery: "delivered", dvia: "check", delivered: "2026-11-11T03:25:00" + z,
			by: "D-3", supersededAt: "2026-11-11T11:00:00" + z,
		}},
	}
}

func decisionsParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// decisionsHome isolates HOME and CURIA_DB, fixes the clock at decisionsNow
// and seeds the specs. It returns the database path.
func decisionsHome(t *testing.T, specs ...decisionsSpec) string {
	t.Helper()
	userTerminalAs(t, true) // the user, at a terminal
	db := hookHome(t)
	prevClock := clock
	clock = func() time.Time { return decisionsParse(t, decisionsNow) }
	t.Cleanup(func() {
		clock = prevClock
		decisionsStatus, decisionsJSON, decisionsScope = "open", false, ""
		decisionsSupersede, decisionsSupersededBy = false, ""
	})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	for i, sp := range specs {
		d := model.Decision{
			SessionID: "s1", Repo: "/src/curia", Question: sp.question,
			Options: []model.Option{
				{Key: "A", Label: "Postgres", Tradeoffs: "Durable."},
				{Key: "B", Label: "Redis", Tradeoffs: "Fast."},
			},
			Recommendation: "A", Rationale: "Replays need history.", Reversibility: model.ReversCostly,
			Mode: sp.mode, Scope: model.ScopeRepo, Tags: []string{"storage"}, Status: model.StatusOpen,
			ArgsHash: "hash-" + sp.question, Delivery: model.DeliveryNone,
			CreatedAt: decisionsParse(t, sp.created),
		}
		if sp.mode == model.ModeAssume {
			d.Assumption = "Postgres"
		}
		if sp.expires != "" {
			d.ExpiresAt = decisionsParse(t, sp.expires)
		}
		stored, _, err := st.CreateDecision(context.Background(), d)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if e := sp.end; e.status != "" {
			if e.delivery == "" {
				e.delivery = "none"
			}
			if _, err := raw.Exec(decisionsEndSQL, e.status, e.key, e.answer, e.reasoning, e.via, e.precedent,
				e.withdraw, e.by, e.delivery, e.dvia, decisionsNull(e.answered), decisionsNull(e.closed),
				decisionsNull(e.supersededAt), decisionsNull(e.delivered), stored.ID); err != nil {
				t.Fatalf("seed %s: %v", stored.ID, err)
			}
		}
	}
	return db
}

func decisionsGet(t *testing.T, db, id string) model.Decision {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	d, found, err := st.GetDecision(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetDecision(%s) = %v, found %v", id, err, found)
	}
	return d
}

// decisionsIDs returns the ids in `curia decisions ls --json` output.
func decisionsIDs(t *testing.T, args ...string) []string {
	t.Helper()
	out, err := runRoot(t, append([]string{"decisions", "ls", "--json"}, args...)...)
	if err != nil {
		t.Fatalf("decisions ls: %v\n%s", err, out)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestDecisionsLsJSONGolden(t *testing.T) {
	decisionsHome(t, decisionsSpecs()...)
	out, err := runRoot(t, "decisions", "ls", "--json", "--status", "all")
	if err != nil {
		t.Fatalf("decisions ls: %v\n%s", err, out)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "decisions.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("decisions ls --json output:\n%s\nwant (testdata/decisions.golden.json):\n%s", out, want)
	}
}

func TestDecisionsLsStatusFilter(t *testing.T) {
	decisionsHome(t, decisionsSpecs()...)
	for _, c := range []struct {
		args []string
		want string
	}{
		// D-4's answer is still queued: listed with the open ones (M3).
		{nil, "D-1 D-2 D-4"},
		{[]string{"--status", "open"}, "D-1 D-2 D-4"},
		{[]string{"--status", "decided"}, "D-3 D-4 D-7"},
		{[]string{"--status", "closed"}, "D-5 D-6"},
		{[]string{"--status", "all"}, "D-1 D-2 D-3 D-4 D-5 D-6 D-7"},
	} {
		if got := strings.Join(decisionsIDs(t, c.args...), " "); got != c.want {
			t.Errorf("ls %v = %s, want %s", c.args, got, c.want)
		}
		decisionsStatus = "open"
	}
	if out, err := runRoot(t, "decisions", "ls", "--status", "x"); err == nil {
		t.Errorf("--status x succeeded: %s", out)
	}
}

func TestDecisionsLsEmptyIsEmptyArray(t *testing.T) {
	decisionsHome(t)
	out, err := runRoot(t, "decisions", "ls", "--json")
	if err != nil || out != "[]\n" {
		t.Errorf("ls --json on an empty store = %q, %v, want \"[]\\n\"", out, err)
	}
}

func TestDecisionsLsSweepsExpiry(t *testing.T) {
	decisionsHome(t, decisionsSpec{"Due?", model.ModeAssume, "2026-11-10T09:00:00.000000000Z", "2026-11-11T09:00:00.000000000Z", decisionsEnd{}})
	if got := decisionsIDs(t); len(got) != 0 {
		t.Errorf("open = %v, want none", got)
	}
	decisionsStatus = "open"
	if got := decisionsIDs(t, "--status", "closed"); strings.Join(got, " ") != "D-1" {
		t.Errorf("closed = %v, want D-1", got)
	}
}

// decisionsHostile are texts a hostile agent could store: an escape sequence,
// a line break, bidi controls and isolates, invisible marks, DEL, and
// invalid UTF-8.
const decisionsHostile = "x\x1b[31mred\nnext\u202e\u2066\u2069\u200e\u200f\u061c\x7f\xff\xfey"

// decisionsHostileHome seeds one open ticket whose question, repo, answer and
// answer reasoning are all decisionsHostile.
func decisionsHostileHome(t *testing.T) {
	t.Helper()
	db := decisionsHome(t, decisionsSpec{decisionsHostile, model.ModeBlocking, "2026-11-11T08:00:00.000000000Z", "", decisionsEnd{}})
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE decisions SET repo = ?, answer = ?, answer_reasoning = ? WHERE id = 'D-1'`,
		"/src/"+decisionsHostile, decisionsHostile, decisionsHostile); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionsLsTableIsSanitised(t *testing.T) {
	decisionsHostileHome(t)
	out, err := runRoot(t, "decisions", "ls")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header and one row, got %d lines:\n%q", len(lines), out)
	}
	if !utf8.ValidString(out) {
		t.Errorf("table is not valid UTF-8: %q", out)
	}
	for _, r := range out {
		if r != '\n' && !unicode.IsPrint(r) {
			t.Errorf("table contains unprintable %U: %q", r, out)
		}
	}
	for _, residue := range []string{"[31m", "\x1b"} {
		if strings.Contains(out, residue) {
			t.Errorf("table contains escape residue %q: %q", residue, out)
		}
	}
	for _, h := range []string{"ID", "STATUS", "MODE", "AGE", "REPO", "QUESTION", "ANSWER", "DELIVERY"} {
		if !strings.Contains(lines[0], h) {
			t.Errorf("header %q lacks %s", lines[0], h)
		}
	}
	for _, w := range []string{"D-1", "open", "blocking", "4h", "next", "none"} {
		if !strings.Contains(lines[1], w) {
			t.Errorf("row %q lacks %q", lines[1], w)
		}
	}
}

func TestDecisionsLsJSONEscapesUnprintable(t *testing.T) {
	decisionsHostileHome(t)
	out, err := runRoot(t, "decisions", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		if r != '\n' && !unicode.IsPrint(r) {
			t.Fatalf("raw unprintable %U in %q", r, out)
		}
	}
	var rows []struct{ Question, Repo, Answer, AnswerReasoning string }
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	// The escapes decode to the same text: the bidi isolate is still there,
	// as an escape and not as a raw character.
	if !strings.Contains(rows[0].Question, "\u2066") || !strings.Contains(out, `\u2066`) {
		t.Errorf("U+2066 not preserved as an escape: %q", out)
	}
}

func TestEditRescope(t *testing.T) {
	db := decisionsHome(t, decisionsSpecs()...)
	out, err := runRoot(t, "decisions", "edit", "D-3", "--scope", "global")
	if err != nil || out != "D-3 scope repo -> global\n" {
		t.Fatalf("edit = %q, %v", out, err)
	}
	d := decisionsGet(t, db, "D-3")
	if d.Scope != model.ScopeGlobal || d.Status != model.StatusAnswered || !d.SupersededAt.IsZero() {
		t.Errorf("D-3 = scope %s status %s superseded_at %v", d.Scope, d.Status, d.SupersededAt)
	}
}

func TestEditSupersedeSetsSupersededAt(t *testing.T) {
	db := decisionsHome(t, decisionsSpecs()...)
	seedSessions(t, db, decideSessionS1()) // a live session, which can collect a retraction
	useFakeTmux(t).Panes = []tmux.Pane{{ID: "%1", Session: "work", WindowID: "@1", ServerStart: at(8, 0)}}
	out, err := runRoot(t, "decisions", "edit", "D-4", "--supersede")
	if err != nil || out != "D-4 superseded\nD-4 retraction queued for the agent's Stop hook, or tell the agent to call check_decision D-4\n" {
		t.Fatalf("edit = %q, %v", out, err)
	}
	if d := decisionsGet(t, db, "D-4"); !d.SupersededAt.Equal(decisionsParse(t, decisionsNow)) {
		t.Errorf("D-4 superseded_at = %v", d.SupersededAt)
	}
	decisionsSupersede = false
	if got := strings.Join(decisionsIDs(t, "--status", "decided"), " "); got != "D-3 D-4 D-7" {
		t.Errorf("decided = %s, want D-3 D-4 D-7", got)
	}
	decisionsStatus = "open"
	// --superseded-by implies --supersede and prints both parts with --scope.
	out, err = runRoot(t, "decisions", "edit", "D-3", "--scope", "task", "--superseded-by", "D-4")
	if err != nil || out != "D-3 scope repo -> task\nD-3 superseded by D-4\n"+
		"D-3 retraction queued for the agent's Stop hook, or tell the agent to call check_decision D-3\n" {
		t.Fatalf("edit = %q, %v", out, err)
	}
	// D-3 had been delivered: its retraction is queued (Ruling 82).
	if d := decisionsGet(t, db, "D-3"); d.SupersededBy != "D-4" || d.SupersededAt.IsZero() || d.Delivery != model.DeliveryQueued {
		t.Errorf("D-3 = by %q at %v, delivery %s", d.SupersededBy, d.SupersededAt, d.Delivery)
	}
	// D-7 was superseded before: superseding again queues nothing.
	decisionsScope, decisionsSupersede, decisionsSupersededBy = "", false, ""
	out, err = runRoot(t, "decisions", "edit", "D-7", "--supersede")
	if err != nil || out != "D-7 superseded\n" {
		t.Fatalf("edit D-7 = %q, %v", out, err)
	}
}

func TestEditErrors(t *testing.T) {
	db := decisionsHome(t, decisionsSpecs()...)
	for _, c := range []struct {
		name string
		args []string
		id   string // row that must stay unchanged
		is   error  // errors.Is target, or nil
		msg  string // substring of the message, or ""
	}{
		{"no flag", []string{"D-3"}, "D-3", nil, "nothing to change"},
		{"bad id", []string{"d3", "--supersede"}, "", nil, `invalid decision id "d3"`},
		{"bad id zero", []string{"D-0", "--supersede"}, "", nil, `invalid decision id "D-0"`},
		{"unknown id", []string{"D-99", "--supersede"}, "", store.ErrNotFound, "D-99"},
		{"open ticket", []string{"D-1", "--scope", "global"}, "D-1", store.ErrNotDecided, "D-1"},
		{"closed ticket", []string{"D-5", "--supersede"}, "D-5", store.ErrNotDecided, "D-5"},
		{"bad scope", []string{"D-3", "--scope", "planet"}, "D-3", nil, `invalid --scope "planet"`},
		{"superseded-by itself", []string{"D-3", "--superseded-by", "D-3"}, "D-3", store.ErrBadPrecedent, "D-3"},
		{"superseded-by open", []string{"D-3", "--superseded-by", "D-1"}, "D-3", store.ErrBadPrecedent, "D-3"},
		{"superseded-by unknown", []string{"D-3", "--superseded-by", "D-99"}, "D-3", store.ErrBadPrecedent, "D-3"},
		{"superseded-by malformed", []string{"D-3", "--superseded-by", "x"}, "D-3", nil, `invalid --superseded-by "x"`},
	} {
		var before model.Decision
		if c.id != "" {
			before = decisionsGet(t, db, c.id)
		}
		out, err := runRoot(t, append([]string{"decisions", "edit"}, c.args...)...)
		decisionsScope, decisionsSupersede, decisionsSupersededBy = "", false, ""
		if err == nil {
			t.Errorf("%s: succeeded: %s", c.name, out)
			continue
		}
		if c.is != nil && !errors.Is(err, c.is) {
			t.Errorf("%s: error %q is not %v", c.name, err, c.is)
		}
		if !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: error %q lacks %q", c.name, err, c.msg)
		}
		if strings.Contains(out, "D-3 superseded") || strings.Contains(out, "scope repo ->") {
			t.Errorf("%s: printed success text: %s", c.name, out)
		}
		if c.id != "" {
			if after := decisionsGet(t, db, c.id); after.Scope != before.Scope || after.SupersededBy != before.SupersededBy ||
				!after.SupersededAt.Equal(before.SupersededAt) || after.Status != before.Status {
				t.Errorf("%s: row changed: %+v -> %+v", c.name, before, after)
			}
		}
	}
}

// decisionsResetFlags restores the flag variables after a test that runs
// commands without decisionsHome.
func decisionsResetFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		decisionsStatus, decisionsJSON, decisionsScope = "open", false, ""
		decisionsSupersede, decisionsSupersededBy = false, ""
	})
}

// Every path that reaches runDecisionsEdit refuses first, before any store
// step: no database file is created.
func TestEditRefusesInsideAgentBeforeTheStore(t *testing.T) {
	for _, args := range [][]string{
		{"D-3", "--scope", "global"},
		{"D-99", "--supersede"},
		{"--scope", "global", "D-3"},
		{"--supersede", "D-3"},
		{"d3", "--supersede"},
		{"D-3"},
		{"D-3", "--scope", "planet"},
		{"D-3", "--supersede"},
		{"D-3", "--superseded-by", "D-4"},
		{"D-3", "--superseded-by", "x"},
		{"D-3", "--scope", "task", "--supersede", "--superseded-by", "D-4"},
	} {
		db := hookHome(t)
		decisionsResetFlags(t)
		t.Setenv("CLAUDECODE", "1")
		out, err := runRoot(t, append([]string{"decisions", "edit"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "cannot run inside an agent session") {
			t.Errorf("edit %v = %v, %q, want the refusal", args, err, out)
		}
		if out != "" && strings.Contains(out, "->") {
			t.Errorf("edit %v printed %q", args, out)
		}
		if _, statErr := os.Stat(db); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("edit %v touched the database: %v", args, statErr)
		}
	}
}

func TestEditRefusesInsideAgent(t *testing.T) {
	db := decisionsHome(t, decisionsSpecs()...)
	t.Setenv("CLAUDECODE", "1")
	out, err := runRoot(t, "decisions", "edit", "D-3", "--scope", "global")
	if err == nil || !strings.Contains(err.Error(), "agent session") {
		t.Errorf("edit inside agent = %v, %q, want refusal", err, out)
	}
	if d := decisionsGet(t, db, "D-3"); d.Scope != model.ScopeRepo {
		t.Errorf("scope = %s, want repo", d.Scope)
	}
	decisionsScope = ""
	if got := decisionsIDs(t); strings.Join(got, " ") != "D-1 D-2 D-4" {
		t.Errorf("ls inside agent = %v", got)
	}
}

// Ruling 69: help is exempt from the guard. It prints usage, reaches no
// store step and writes nothing.
func TestEditHelpInsideAgentIsExempt(t *testing.T) {
	db := hookHome(t)
	decisionsResetFlags(t)
	t.Setenv("CLAUDECODE", "1")
	t.Cleanup(func() {
		if c, _, err := rootCmd.Find([]string{"decisions", "edit"}); err == nil {
			_ = c.Flags().Set("help", "false")
		}
	})
	out, err := runRoot(t, "decisions", "edit", "--help")
	if err != nil || !strings.Contains(out, "Usage:") {
		t.Errorf("edit --help = %q, %v, want usage", out, err)
	}
	if _, statErr := os.Stat(db); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("help touched the database: %v", statErr)
	}
}

// userTerminalUnset is userTerminalOpen under test: a test that reaches the
// terminal check without saying whether there is a terminal fails.
func userTerminalUnset() error {
	panic("userTerminalOpen called in a test that did not set it (userTerminalAs)")
}

// userProcessUnset is userProcess under test: a test that reaches the
// ancestry walk without a process table of its own fails (the real one
// would read the test's own parents, which may be an agent's).
func userProcessUnset() (map[int]userProc, error) {
	panic("userProcesses called in a test that did not set a process table (userTerminalAs or userProcessesAs)")
}

func init() {
	userTerminalOpen = userTerminalUnset
	userProcesses = userProcessUnset
}

// userTerminalAs makes the controlling terminal openable (at) or not for
// the test; at a terminal the caller's parents are a login shell of a
// terminal application, as the user's are.
func userTerminalAs(t *testing.T, at bool) {
	t.Helper()
	userTerminalOpen = func() error {
		if at {
			return nil
		}
		return errors.New("open /dev/tty: device not configured")
	}
	userProcessesAs(t, userShellTable())
	t.Cleanup(func() { userTerminalOpen = userTerminalUnset })
}

// userShellTable is the user's own terminal: curia's parent is a login
// shell under a terminal application under launchd.
func userShellTable() map[int]userProc {
	return map[int]userProc{os.Getppid(): {9001, "-zsh", ""}, 9001: {9002, "login", ""}, 9002: {1, "/System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal", ""}}
}

// userProcessesAs makes table the process table for the test: a pid not
// in it is not running (an ancestry through it cannot be read).
func userProcessesAs(t *testing.T, table map[int]userProc) {
	t.Helper()
	userProcesses = func() (map[int]userProc, error) { return maps.Clone(table), nil }
	t.Cleanup(func() { userProcesses = userProcessUnset })
}

// TestUserOnlyRefusesBySessionEnv: the agent guard also refuses with
// CLAUDE_CODE_SESSION_ID set, and in the pane of a live agent session
// (Ruling 112), but not in a pane whose session is gone.
func TestUserOnlyRefusesBySessionEnv(t *testing.T) {
	commands := [][]string{
		{"say", "@api", "hello"},
		{"decide", "D-1", "--option", "A"},
		{"decisions", "edit", "D-1", "--supersede"},
		{"messages", "cancel", "M-1"},
		{"routing", "reset"},
		{"setup", "approve", "T-1"},
		{"pause", "T-1"},
		{"resume", "T-1"},
		{"clean", "T-1"},
	}
	refusedAll := func(t *testing.T, why string) {
		t.Helper()
		for _, args := range commands {
			_, err := runRoot(t, args...)
			if err == nil || !strings.Contains(err.Error(), "inside an agent session") || !strings.Contains(err.Error(), why) {
				t.Errorf("%v = %v, want the agent refusal naming %q", args, err, why)
			}
		}
	}
	t.Run("session id", func(t *testing.T) {
		hookHome(t)
		useFakeTmux(t)
		t.Setenv("CLAUDE_CODE_SESSION_ID", "1a2b3c4d-0000-4000-8000-000000000000")
		refusedAll(t, "CLAUDE_CODE_SESSION_ID is set")
	})
	t.Run("agent pane", func(t *testing.T) {
		db, fake := readHome(t, []string{"%1", "%2"}, readSession("agent-1", model.StateIdle, "%1"))
		seedSessions(t, db, readSession("old", model.StateEnded, "%2"))
		t.Setenv("TMUX_PANE", "%1")
		refusedAll(t, "pane %1 of agent session agent-1")
		// The pane of an ended session is not an agent's.
		t.Setenv("TMUX_PANE", "%2")
		if sign := agentSign(); sign != "" {
			t.Errorf("in an ended session's pane: agentSign = %q, want none", sign)
		}
		// The agent's pane is gone (tmux restarted and %1 is someone else's):
		// the pane is the user's, and the guard writes nothing.
		t.Setenv("TMUX_PANE", "%1")
		fake.Panes = []tmux.Pane{{ID: "%1", Session: "work", WindowID: "@1", ServerStart: at(10, 0)}}
		if sign := agentSign(); sign != "" {
			t.Errorf("in a recycled pane: agentSign = %q, want none", sign)
		}
		if s := getSession(t, db, "agent-1"); s.State != model.StateIdle {
			t.Errorf("agent-1 = %s, want idle: the guard is read-only", s.State)
		}
	})
	t.Run("no store yet", func(t *testing.T) {
		db := hookHome(t)
		useFakeTmux(t)
		t.Setenv("TMUX_PANE", "%1")
		if sign := agentSign(); sign != "" {
			t.Errorf("agentSign = %q, want none", sign)
		}
		if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the check created the store: %v", err)
		}
	})
}

func TestUserOnlyGuard(t *testing.T) {
	userTerminalAs(t, true)
	if err := userOnly("x", "do x"); err != nil {
		t.Errorf("at a terminal, no agent variable: %v", err)
	}
	t.Setenv("CLAUDECODE", "")
	if err := userOnly("x", "do x"); err != nil {
		t.Errorf("CLAUDECODE empty: %v", err)
	}
	t.Setenv("CLAUDECODE", "1")
	if err := userOnly("x", "do x"); err == nil || !strings.Contains(err.Error(), "curia x cannot run inside an agent session (CLAUDECODE is set; if you are not an agent, tmux may hold it: run tmux set-environment -gu CLAUDECODE and open a new shell): only the user may do x") {
		t.Errorf("CLAUDECODE=1: %v", err)
	}
	t.Setenv("CLAUDECODE", "")
	userTerminalAs(t, false)
	if err := userOnly("x", "do x"); err == nil || !strings.Contains(err.Error(), "curia x cannot run without a terminal") ||
		!strings.Contains(err.Error(), "only the user may do x") {
		t.Errorf("no terminal: %v", err)
	}
}

// M3, Ruling 89: the table says where each ticket was answered (VIA) and
// how far its delivery got, marks a superseded one, and the default
// listing holds the answers the agent has not got yet.
func TestDecisionsLsShowsViaDeliveryAndSupersession(t *testing.T) {
	const z = ".000000000Z"
	specs := append(decisionsSpecs(), decisionsSpec{"Typed, not confirmed?", model.ModeBlocking, "2026-11-11T10:00:00" + z, "", decisionsEnd{
		status: "answered", key: "A", answer: "A: Postgres", via: "popup", answered: "2026-11-11T10:30:00" + z,
		delivery: "delivered", dvia: "send_keys", delivered: "2026-11-11T10:30:01" + z,
	}})
	decisionsHome(t, specs...)
	out, err := runRoot(t, "decisions", "ls", "--status", "all")
	if err != nil {
		t.Fatal(err)
	}
	fields := func(id string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, id+" ") {
				return strings.Join(strings.Fields(l), " ")
			}
		}
		t.Fatalf("no row %s in:\n%s", id, out)
		return ""
	}
	if h := strings.Fields(strings.SplitN(out, "\n", 2)[0]); !slices.Equal(h, []string{"ID", "STATUS", "MODE", "AGE", "REPO", "QUESTION", "ANSWER", "VIA", "DELIVERY"}) {
		t.Errorf("header = %q", h)
	}
	for id, want := range map[string]string{
		"D-1": "open blocking",
		"D-3": "cli delivered (stop_hook)",
		"D-4": "popup queued",
		"D-7": "confirmed (superseded)",
		"D-8": "popup typed, unconfirmed",
	} {
		if got := fields(id); !strings.Contains(got, want) {
			t.Errorf("%s row = %q, want it to hold %q", id, got, want)
		}
	}
	if got := fields("D-7"); !strings.HasSuffix(got, "ssh delivered (check)") {
		t.Errorf("D-7 row = %q, want via ssh, delivered by check", got)
	}
	decisionsStatus = "open"
	if got := strings.Join(decisionsIDs(t), " "); got != "D-1 D-2 D-4 D-8" {
		t.Errorf("default listing = %s, want the open tickets and the undelivered answers D-4 and D-8", got)
	}
}

// Re-review N3: superseding an answer delivered to a session that has
// ended, or has no row, queues no retraction nobody could collect, says so,
// and leaves the badge without a Q.
func TestEditSupersedeForGoneSessionRetractsNothing(t *testing.T) {
	for name, sessions := range map[string][]model.Session{
		"ended":  {statusSession(model.StateEnded)},
		"no row": nil,
	} {
		db := decisionsHome(t, decisionsSpecs()...)
		seedSessions(t, db, sessions...)
		out, err := runRoot(t, "decisions", "edit", "D-3", "--supersede")
		decisionsSupersede = false
		if want := "D-3 superseded\nD-3: its session has ended; nothing to retract\n"; err != nil || out != want {
			t.Errorf("%s: edit = %q, %v; want %q", name, out, err, want)
		}
		if d := decisionsGet(t, db, "D-3"); d.Delivery != model.DeliveryDelivered || d.SupersededAt.IsZero() {
			t.Errorf("%s: D-3 = delivery %s, superseded at %v; want still delivered, superseded", name, d.Delivery, d.SupersededAt)
		}
		// D-1 and D-2 are open. D-4's answer is queued in the seed, but for
		// a session that has ended or has no row, so it is not on the badge
		// either; D-3 adds nothing.
		if out, err := runRoot(t, "status"); err != nil || out != "D1" {
			t.Errorf("%s: status = %q, %v; want %q", name, out, err, "D1")
		}
	}
}

// Live check 3, defect 7a: an answer not delivered yet whose session has
// ended, gone stale or has no row is still listed, marked so, and not
// counted on the badge.
func TestDecisionsLsMarksAnswersNobodyCanCollect(t *testing.T) {
	for state, mark := range map[model.State]string{
		model.StateEnded: "queued (session ended)",
		model.StateStale: "queued (session stale)",
		"":               "queued (no session row)",
		model.StateIdle:  "queued",
	} {
		db := decisionsHome(t, decisionsSpecs()...)
		if state != "" {
			seedSessions(t, db, statusSession(state))
		}
		out, err := runRoot(t, "decisions", "ls")
		if err != nil {
			t.Fatal(err)
		}
		var row string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "D-4 ") {
				row = strings.Join(strings.Fields(l), " ")
			}
		}
		if !strings.HasSuffix(row, "popup "+mark) {
			t.Errorf("session %q: D-4 row = %q, want it to end %q", state, row, "popup "+mark)
		}
		want := map[bool]string{true: "D1 Q1", false: "D1"}[state == model.StateIdle]
		if got, err := runRoot(t, "status"); err != nil || got != want {
			t.Errorf("session %q: status = %q, %v; want %q", state, got, err, want)
		}
	}
}

// The ancestry walk (Ruling 119): a caller under Claude Code, however many
// processes between, or under the first process of a live agent's pane, is
// not the user; an unreadable or endless ancestry fails closed.
func TestUserAncestry(t *testing.T) {
	me := os.Getppid()
	claude := "/Users/user/.local/bin/claude"
	for _, c := range []struct {
		name  string
		table map[int]userProc
		want  string // "" is the user
	}{
		{"direct child of claude", map[int]userProc{me: {1, claude, ""}}, "runs under Claude Code"},
		{"claude by base name", map[int]userProc{me: {1, "claude", ""}}, "runs under Claude Code"},
		{"through sh, env and script", map[int]userProc{
			me: {10, "env", ""}, 10: {11, "script", ""}, 11: {12, "/bin/zsh", ""}, 12: {13, "claude", ""}, 13: {1, "/sbin/launchd", ""},
		}, "runs under Claude Code (process 12)"},
		{"the user's terminal", userShellTable(), ""},
		{"a tmux popup or run-shell job", map[int]userProc{me: {40, "sh", ""}, 40: {1, "tmux", ""}}, ""},
		{"the Claude desktop app is not Claude Code", map[int]userProc{me: {50, "-zsh", ""}, 50: {1, "/Applications/Claude.app/Contents/MacOS/Claude", ""}}, ""},
		{"a name that only holds claude", map[int]userProc{me: {60, "claude-wrapper", ""}, 60: {1, "launchd", ""}}, ""},
		// Claude Code from npm runs as node or bun with its script.
		{"node through npm's link", map[int]userProc{me: {1, "node", "node /usr/local/bin/claude --continue"}}, "runs under Claude Code"},
		{"node on the cli script", map[int]userProc{me: {
			1, "/opt/homebrew/bin/node",
			"node --no-warnings /opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js",
		}}, "runs under Claude Code"},
		{"bun on the cli script", map[int]userProc{me: {1, "bun", "bun /x/node_modules/@anthropic-ai/claude-code/cli.mjs"}}, "runs under Claude Code"},
		{"node running something else", map[int]userProc{me: {1, "node", "node /srv/claude-notes/server.js --name claude"}}, ""},
		{"node with another cli.js", map[int]userProc{me: {1, "node", "node /x/tools/cli.js claude"}}, ""},
		{"unreadable", map[int]userProc{me: {70, "sh", ""}}, "cannot be read"},
		{"a loop", map[int]userProc{me: {80, "sh", ""}, 80: {me, "sh", ""}}, "more than 64 deep"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hookHome(t)
			useFakeTmux(t)
			userProcessesAs(t, c.table)
			if got := agentAncestor(); (c.want == "" && got != "") || !strings.Contains(got, c.want) {
				t.Errorf("agentAncestor = %q, want %q", got, c.want)
			}
		})
	}
	t.Run("the pane of a live agent", func(t *testing.T) {
		_, fake := readHome(t, []string{"%1", "%2"}, readSession("agent-1", model.StateIdle, "%1"))
		fake.Panes[0].PID = 4242 // %1's shell, in which the agent runs
		userProcessesAs(t, map[int]userProc{me: {4242, "bash", ""}, 4242: {1, "tmux", ""}})
		if got := agentAncestor(); !strings.Contains(got, "pane of agent session agent-1 (process 4242)") {
			t.Errorf("agentAncestor = %q, want the agent's pane", got)
		}
		// The same chain through a pane with no live session is the user's.
		fake.Panes[0].PID, fake.Panes[1].PID = 1111, 4242
		if got := agentAncestor(); got != "" {
			t.Errorf("agentAncestor = %q, want none for another pane", got)
		}
	})
}

// The pane check is skipped, not failed, when the pane list cannot be read
// (Ruling 123): the user at a terminal is not locked out while tmux is down
// or restarted with sessions still recorded live, and a Claude Code
// ancestor, read from the process table alone, still refuses; a store that
// cannot be read refuses.
func TestUserAncestryWithoutPaneList(t *testing.T) {
	me := os.Getppid()
	shell := map[int]userProc{me: {1, "-zsh", ""}}
	underClaude := map[int]userProc{me: {600, "/bin/zsh", ""}, 600: {601, "claude", ""}, 601: {1, "tmux", ""}} // 600 is claude
	unreachable := func(t *testing.T) {
		t.Helper()
		db := hookHome(t) // the guard's tmux: unreachable
		seedSessions(t, db, readSession("agent-1", model.StateIdle, "%1"))
	}
	otherServer := func(t *testing.T) {
		t.Helper()
		_, fake := readHome(t, []string{"%1"}, readSession("agent-1", model.StateIdle, "%1"))
		fake.Panes = []tmux.Pane{{ID: "%7", PID: 700, ServerStart: at(8, 0)}} // not the agent's pane
	}
	for _, c := range []struct {
		name  string
		world func(*testing.T)
		table map[int]userProc
		want  string // "" is the user
	}{
		{"tmux unreachable, the user's terminal", unreachable, shell, ""},
		{"tmux unreachable, under Claude Code", unreachable, underClaude, "runs under Claude Code (process 600)"},
		{"another server, the user's terminal", otherServer, shell, ""},
		{"another server, under Claude Code", otherServer, underClaude, "runs under Claude Code (process 600)"},
		{"process table unreadable", unreachable, map[int]userProc{me: {610, "sh", ""}}, "cannot be read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.world(t)
			userProcessesAs(t, c.table)
			if got := agentAncestor(); (c.want == "" && got != "") || !strings.Contains(got, c.want) {
				t.Errorf("agentAncestor = %q, want %q", got, c.want)
			}
		})
	}
	t.Run("the skip is logged", func(t *testing.T) {
		unreachable(t)
		userProcessesAs(t, shell)
		if got := agentAncestor(); got != "" {
			t.Fatalf("agentAncestor = %q, want none", got)
		}
		if log := strings.Join(readLog(t), "\n"); !strings.Contains(log, "level=INFO") || !strings.Contains(log, "agent pane check skipped") {
			t.Errorf("log = %q, want one INFO line saying the pane check was skipped", log)
		}
	})
	t.Run("store unreadable", func(t *testing.T) {
		db := hookHome(t)
		if err := os.MkdirAll(db, 0o700); err != nil { // a directory where the database file should be
			t.Fatal(err)
		}
		userProcessesAs(t, shell)
		if got := agentAncestor(); !strings.Contains(got, "store cannot be read") {
			t.Errorf("agentAncestor = %q, want the refusal", got)
		}
	})
	t.Run("no live agent in a pane", func(t *testing.T) {
		db := hookHome(t)
		seedSessions(t, db, readSession("old", model.StateEnded, "%1"), readSession("outside", model.StateIdle, ""))
		userProcessesAs(t, shell)
		if got := agentAncestor(); got != "" {
			t.Errorf("agentAncestor = %q, want none", got)
		}
	})
}

// The user works in pane %1 of a second tmux server (TMUX points at it)
// while the agents' sessions are recorded in %1 and %2 of theirs: the pane
// ids collide (re-review A). The guard reads, and writes no session row.
//   - The second server started after the sessions: by fleet.Gone none of
//     them is in it, so the user is let through.
//   - It started before: %1 looks like agent-1's pane and the user is
//     refused. Telling the servers apart needs a server identity on the
//     session row (Plan 4); refusing is the safe side.
func TestSecondServerPaneIDCollision(t *testing.T) {
	me := os.Getppid()
	shell := map[int]userProc{me: {4242, "-zsh", ""}, 4242: {1, "tmux", ""}}
	unchanged := func(t *testing.T, db string) {
		t.Helper()
		for _, id := range []string{"agent-1", "agent-2"} {
			if s := getSession(t, db, id); s.State != model.StateIdle {
				t.Errorf("%s = %s, want idle: the guard writes nothing", id, s.State)
			}
		}
	}
	world := func(t *testing.T, serverStart time.Time) string {
		t.Helper()
		db, fake := readHome(t, []string{"%1"}, readSession("agent-1", model.StateIdle, "%1"), readSession("agent-2", model.StateIdle, "%2"))
		fake.Panes[0].ServerStart = serverStart
		fake.Panes[0].PID = 4242 // the user's shell in the second server's %1
		userProcessesAs(t, shell)
		t.Setenv("TMUX_PANE", "%1")
		return db
	}
	t.Run("second server started after the sessions", func(t *testing.T) {
		db := world(t, at(10, 0))
		if err := userOnly("decide", "answer a decision"); err != nil {
			t.Errorf("userOnly = %v, want the user let through", err)
		}
		unchanged(t, db)
	})
	t.Run("second server started before the sessions", func(t *testing.T) {
		db := world(t, at(8, 0))
		if err := userOnly("decide", "answer a decision"); err == nil || !strings.Contains(err.Error(), "agent session agent-1") {
			t.Errorf("userOnly = %v, want the refusal naming agent-1 (needs a server identity to tell apart)", err)
		}
		unchanged(t, db)
	})
}

// The real process table, read by ps: it holds this test process with its
// parent. Whether a Claude Code ancestor is in it depends on where the
// tests run (none on CI), so only the table itself is checked.
func TestRealProcessTable(t *testing.T) {
	table, err := psTable()
	if err != nil {
		t.Fatalf("psTable: %v", err)
	}
	p, ok := table[os.Getpid()]
	if !ok {
		t.Fatalf("psTable has no row for this process (%d) among %d", os.Getpid(), len(table))
	}
	if p.PPID != os.Getppid() {
		t.Errorf("this process's parent = %d, want %d", p.PPID, os.Getppid())
	}
	if p.Comm == "" {
		t.Error("this process's command name is empty")
	}
}

// The user at a terminal decides while tmux is down and the asking session
// is still recorded live (Ruling 123: no lockout).
func TestDecideWhileTmuxIsDown(t *testing.T) {
	db, _ := decideHome(t, decideOpenSpecs()...)
	useDownTmux(t)
	if _, _, err := decideRun(t, strings.NewReader(""), "D-1", "--option", "A", "--reason", "r"); err != nil {
		t.Fatalf("decide with tmux down = %v, want the answer stored", err)
	}
	if d := decisionsGet(t, db, "D-1"); d.Status != model.StatusAnswered {
		t.Errorf("D-1 = %s, want answered", d.Status)
	}
}

// The review's reproduction: an agent wraps its send in a pseudo-terminal
// of its own, "script -q /dev/null env -u CLAUDECODE -u
// CLAUDE_CODE_SESSION_ID -u TMUX_PANE curia send %1 …". It now has a
// controlling terminal and no agent variable, and is still under Claude
// Code: the message is an agent's, and user-only commands refuse.
func TestScriptWrappedSendIsNeverFromUser(t *testing.T) {
	db, fake := sendHome(t)
	userTerminalAs(t, true) // script's pty: /dev/tty opens
	me := os.Getppid()      // curia's parent, env
	userProcessesAs(t, map[int]userProc{
		me: {500, "env", ""}, 500: {501, "script", ""}, 501: {502, "/bin/zsh", ""}, 502: {503, "claude", ""}, 503: {1, "launchd", ""},
	})
	if _, err := readRun(t, "", "send", "%1", "reply with only LABEL-TWO"); err != nil {
		t.Fatal(err)
	}
	m := sendOne(t, db)
	if m.Sender != model.SenderAgent || m.SenderSession != "" {
		t.Errorf("message = %+v, want an unknown agent sender", m)
	}
	if typed := sendTyped(fake); len(typed) != 0 {
		t.Errorf("typed %q, want nothing", typed)
	}
	for _, argv := range [][]string{{"say", "T-1:", "hello"}, {"pause", "T-1"}, {"clean", "T-1"}, {"decide", "D-1", "--option", "A"}} {
		_, err := runRoot(t, argv...)
		if err == nil || !strings.Contains(err.Error(), "runs under Claude Code (process 502)") {
			t.Errorf("%v = %v, want the ancestry refusal", argv, err)
		}
	}
}

// userOnlyCommands are the commands only the user may run, each with
// arguments it accepts (Ruling 112). A command marked user-only that is not
// listed here fails TestUserOnlyCommandsGoThroughTheGuard: add it.
var userOnlyCommands = map[string][]string{
	"curia say":             {"T-1:", "hello"},
	"curia decide":          {"D-1", "--option", "A"},
	"curia decisions edit":  {"D-1", "--supersede"},
	"curia messages cancel": {"M-1"},
	"curia routing reset":   {},
	"curia setup approve":   {"T-1"},
	"curia pause":           {"T-1"},
	"curia resume":          {"T-1"},
	"curia clean":           {"T-1"},
	"curia task add":        {"a title"},
	"curia task edit":       {"T-1", "--title", "x"},
	"curia task ready":      {"T-1"},
	"curia task import":     {"--backlog-md"},
	"curia install":         {},
	"curia uninstall":       {},
}

// userNotOnlyCommands are the leaf commands anyone, an agent included, may
// run, each with why (re-review A, I1). Every leaf command is in this table
// or in userOnlyCommands; TestEveryCommandIsClassified fails on one in
// neither, so a new command cannot be added without that decision.
var userNotOnlyCommands = map[string]string{
	"curia decisions ls":          "reads the tickets only",
	"curia hook":                  "run by Claude Code's hooks; records the calling session's own event",
	"curia launch":                "runs the agent in the caller's own process; spawn decides what is started",
	"curia ls":                    "reads the sessions only",
	"curia mcp":                   "run by Claude Code; its tools cannot decide, send, spawn or pause",
	"curia messages ls":           "reads the messages only",
	"curia next":                  "moves the tmux client to a session; changes nothing",
	"curia read":                  "reads a pane's last lines, as tmux capture-pane does",
	"curia routing stats":         "reads the routing counts only",
	"curia seen":                  "attention bookkeeping, run by tmux on pane focus",
	"curia send":                  "agents may send; labeled, limited and never from the user (Rulings 112, 113)",
	"curia sidebar":               "a view; its user actions go through the guard (uiUserActions)",
	"curia spawn":                 "agents may spawn ready tasks only (Ruling 113)",
	"curia status":                "prints the badge",
	"curia task ls":               "reads the tasks only",
	"curia ui":                    "a view; its user actions go through the guard (uiUserActions)",
	"curia version":               "prints the version",
	"curia help":                  "cobra's help",
	"curia completion":            "cobra's shell completion scripts",
	"curia completion bash":       "cobra's shell completion script",
	"curia completion fish":       "cobra's shell completion script",
	"curia completion powershell": "cobra's shell completion script",
	"curia completion zsh":        "cobra's shell completion script",
}

// unclassifiedCommands are the leaf commands under root in neither table,
// and the ones in both.
func unclassifiedCommands(root *cobra.Command) []string {
	var out []string
	userOnlyWalk(root, func(c *cobra.Command) {
		if c.HasSubCommands() {
			return
		}
		path := c.CommandPath()
		_, only := userOnlyCommands[path]
		_, not := userNotOnlyCommands[path]
		if only == not {
			out = append(out, path)
		}
	})
	return out
}

func TestEveryCommandIsClassified(t *testing.T) {
	for _, path := range unclassifiedCommands(rootCmd) {
		t.Errorf("%s is in neither userOnlyCommands nor userNotOnlyCommands (or in both): decide whether only the user may run it", path)
	}
	for path := range userNotOnlyCommands {
		if marked := userOnlyMarked(t); marked[path] {
			t.Errorf("%s is marked user-only but listed as not", path)
		}
	}
	// The test of the test: a command added the way a new one would be, with
	// no mark and no guard, is reported.
	dummy := &cobra.Command{Use: "approve-everything", RunE: func(*cobra.Command, []string) error { return nil }}
	rootCmd.AddCommand(dummy)
	defer rootCmd.RemoveCommand(dummy)
	if got := unclassifiedCommands(rootCmd); !slices.Contains(got, "curia approve-everything") {
		t.Errorf("an unclassified command is not reported: %v", got)
	}
}

// userOnlyWalk calls f on every command under c.
func userOnlyWalk(c *cobra.Command, f func(*cobra.Command)) {
	f(c)
	for _, sub := range c.Commands() {
		userOnlyWalk(sub, f)
	}
}

// userOnlyMarked returns the paths of the commands marked user-only,
// failing t for a command whose help says "the user only" but is not marked.
func userOnlyMarked(t *testing.T) map[string]bool {
	t.Helper()
	marked := map[string]bool{}
	userOnlyWalk(rootCmd, func(c *cobra.Command) {
		_, isMarked := c.Annotations[userOnlyAnnotation]
		if strings.Contains(c.Short, "the user only") && !isMarked {
			t.Errorf("%s says it is for the user only but is not marked user-only", c.CommandPath())
		}
		if isMarked {
			marked[c.CommandPath()] = true
		}
	})
	return marked
}

// userOnlyRefused runs argv with no controlling terminal (and env set, when
// not empty) and checks userOnly refused it with want before it opened the
// store or called tmux.
func userOnlyRefused(t *testing.T, argv []string, env, want string) {
	t.Helper()
	db := hookHome(t)
	fake := useFakeTmux(t)
	userTerminalAs(t, false)
	if env != "" {
		t.Setenv(env, "1")
	}
	defer taskResetFlags()
	defer func() { pauseForce, pauseNoVerify, cleanForce = false, false, false }()
	out, err := runRoot(t, argv...)
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "only the user may") {
		t.Errorf("%v = %v (output %q), want %q", argv, err, out, want)
	}
	if _, serr := os.Stat(db); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("%v opened the store before refusing: %v", argv, serr)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("%v called tmux before refusing: %q", argv, fake.Calls)
	}
}

// TestUserOnlyCommandsGoThroughTheGuard enumerates the cobra commands: every
// command marked user-only (or whose help says "the user only") is listed
// above, and run with no controlling terminal, and again with CLAUDECODE set,
// each is refused by userOnly before it opens the store or calls tmux.
func TestUserOnlyCommandsGoThroughTheGuard(t *testing.T) {
	marked := userOnlyMarked(t)
	for path := range userOnlyCommands {
		if !marked[path] {
			t.Errorf("%s is not marked user-only (Annotations: userOnlyMark())", path)
		}
	}
	for path := range marked {
		args, ok := userOnlyCommands[path]
		if !ok {
			t.Errorf("%s is marked user-only but not listed in userOnlyCommands", path)
			continue
		}
		name := strings.TrimPrefix(path, "curia ")
		argv := append(strings.Fields(name), args...)
		t.Run(path+"/no terminal", func(t *testing.T) {
			userOnlyRefused(t, argv, "", "curia "+name+" cannot run without a terminal")
		})
		t.Run(path+"/agent", func(t *testing.T) {
			userOnlyRefused(t, argv, "CLAUDECODE", "curia "+name+" cannot run inside an agent session")
		})
	}
}

// userEvasions are the three environments live check B used to pass an
// agent off as the user (defect 1): each clears the agent's variables, and
// none has a controlling terminal, as an agent's tool shell has none.
var userEvasions = map[string][]string{
	"env -u CLAUDECODE -u CLAUDE_CODE_SESSION_ID -u TMUX_PANE": {"CURIA_TASK_ID=T-1"},
	"env -i PATH=$PATH": {},
	"env -u CLAUDECODE": {"CLAUDE_CODE_SESSION_ID=1a2b3c4d-0000-4000-8000-0000000000a1", "TMUX_PANE=%2", "CURIA_TASK_ID=T-1"},
}

// userEvade sets env, as NAME=value pairs, for the test.
func userEvade(t *testing.T, env []string) {
	t.Helper()
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
}

// userEvadedSend sends from env with no controlling terminal and checks
// the message is an agent's, labeled so, and never typed when its sender is
// unknown.
func userEvadedSend(t *testing.T, env []string) {
	t.Helper()
	db, fake := sendHome(t) // t1 in %1, the sending agent a2 in %2, a shell in %9
	userTerminalAs(t, false)
	userEvade(t, env)
	if _, err := readRun(t, "", "send", "%1", "reply with only LABEL-TWO"); err != nil {
		t.Fatal(err)
	}
	m := sendOne(t, db)
	if m.Sender != model.SenderAgent {
		t.Errorf("sender = %s, want agent", m.Sender)
	}
	line := message.StopBlock([]model.Message{m})
	if strings.Contains(line, "M-1 from the user") || !strings.Contains(line, "not from the user") {
		t.Errorf("Stop block %q, want it labeled as not from the user", line)
	}
	if m.SenderSession == "" && len(sendTyped(fake)) != 0 {
		t.Errorf("an unknown sender's message was typed: %q", sendTyped(fake))
	}
}

// userEvadedCommand runs argv from env with no controlling terminal and
// checks it is refused as user-only, storing nothing.
func userEvadedCommand(t *testing.T, env, argv []string) {
	t.Helper()
	db, _ := sendHome(t)
	userTerminalAs(t, false)
	userEvade(t, env)
	defer func() { pauseForce, pauseNoVerify, cleanForce = false, false, false }()
	_, err := runRoot(t, argv...)
	if err == nil || !strings.Contains(err.Error(), "only the user may") {
		t.Errorf("%v = %v, want the user-only refusal", argv, err)
	}
	if ms := sendMessages(t, db); len(ms) != 0 {
		t.Errorf("%v stored %+v", argv, ms)
	}
}

func TestEvasionsAreNeverTheUser(t *testing.T) {
	for name, env := range userEvasions {
		t.Run("send/"+name, func(t *testing.T) { userEvadedSend(t, env) })
		for _, argv := range [][]string{
			{"say", "T-2:", "reply with only SAY-SPOOF"}, {"pause", "T-1"}, {"clean", "T-1"}, {"decide", "D-1", "--option", "A"},
		} {
			t.Run(argv[0]+"/"+name, func(t *testing.T) { userEvadedCommand(t, env, argv) })
		}
	}
}

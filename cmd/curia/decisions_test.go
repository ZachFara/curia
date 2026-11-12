package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
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
		{nil, "D-1 D-2"},
		{[]string{"--status", "open"}, "D-1 D-2"},
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
	out, err := runRoot(t, "decisions", "edit", "D-4", "--supersede")
	if err != nil || out != "D-4 superseded\n" {
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
	if err != nil || out != "D-3 scope repo -> task\nD-3 superseded by D-4\n" {
		t.Fatalf("edit = %q, %v", out, err)
	}
	if d := decisionsGet(t, db, "D-3"); d.SupersededBy != "D-4" || d.SupersededAt.IsZero() {
		t.Errorf("D-3 = by %q at %v", d.SupersededBy, d.SupersededAt)
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
		if err == nil || !strings.Contains(err.Error(), "refusing to run inside an agent session") {
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
	if got := decisionsIDs(t); strings.Join(got, " ") != "D-1 D-2" {
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

func TestRefuseInsideAgent(t *testing.T) {
	if err := refuseInsideAgent(); err != nil {
		t.Errorf("CLAUDECODE unset: %v", err)
	}
	t.Setenv("CLAUDECODE", "")
	if err := refuseInsideAgent(); err != nil {
		t.Errorf("CLAUDECODE empty: %v", err)
	}
	t.Setenv("CLAUDECODE", "1")
	if err := refuseInsideAgent(); err == nil {
		t.Error("CLAUDECODE=1: no error")
	}
}

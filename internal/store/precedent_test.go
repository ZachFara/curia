package store_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/precedent"
	"github.com/ZachFara/curia/internal/store"
)

// testRepo is the repo ticket() gives every test ticket.
const testRepo = "/src/curia"

// decide stores the open ticket d and answers it with option A at at.
func decide(t *testing.T, s *store.Store, d model.Decision, at time.Time) model.Decision {
	t.Helper()
	return answer(t, s, create(t, s, d).ID, keyA, at)
}

// decided stores a blocking ticket with question from session in repo with
// scope, answers it with option A an hour after t0 and returns the row.
func decided(t *testing.T, s *store.Store, question, session, repo string, scope model.Scope) model.Decision {
	t.Helper()
	d := ticket(t, question, model.ModeBlocking, session, t0)
	d.Repo, d.Scope = repo, scope
	return decide(t, s, d, t0.Add(time.Hour))
}

// search runs q and returns the ids found, in order.
func search(t *testing.T, s *store.Store, q store.PrecedentQuery) []string {
	t.Helper()
	got, err := s.SearchDecisions(context.Background(), q)
	if err != nil {
		t.Fatalf("SearchDecisions(%.60q): %v", q.Text, err)
	}
	return decisionIDs(got)
}

// caller is a search from session s1 in testRepo.
func caller(text string) store.PrecedentQuery {
	return store.PrecedentQuery{Text: text, Repo: testRepo, SessionID: "s1"}
}

func TestSearchFindsDecidedOnly(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	now := t0.Add(time.Hour)
	q := "Use the kumquat cache?"
	mk := func(session string, mode model.Mode) model.Decision {
		d := ticket(t, q, mode, session, t0)
		d.Scope = model.ScopeGlobal
		return create(t, s, d)
	}
	mk("open", model.ModeBlocking) // left open
	withdrawn := mk("withdrawn", model.ModeBlocking)
	if _, err := s.WithdrawDecision(ctx, withdrawn.ID, "withdrawn", "Not needed.", "", now); err != nil {
		t.Fatal(err)
	}
	expired := mk("expired", model.ModeAssume)
	if _, err := s.ExpireDecisions(ctx, expired.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	superseded := answer(t, s, mk("superseded", model.ModeBlocking).ID, keyA, now)
	if _, err := s.EditDecision(ctx, superseded.ID, model.DecisionEdit{Supersede: true}, now); err != nil {
		t.Fatal(err)
	}
	answered := answer(t, s, mk("answered", model.ModeBlocking).ID, keyA, now)
	confirmed := answer(t, s, mk("confirmed", model.ModeAssume).ID, keyA, now)
	overridden := answer(t, s, mk("overridden", model.ModeAssume).ID, model.Answer{Key: "B", Via: model.AnsweredCLI}, now)

	if got := getDecision(t, s, expired.ID).Status; got != model.StatusExpired {
		t.Fatalf("setup: %s is %s, want expired", expired.ID, got)
	}
	got := search(t, s, store.PrecedentQuery{Text: "kumquat", Limit: 10})
	slices.Sort(got)
	want := []string{answered.ID, confirmed.ID, overridden.ID}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("found %v, want only the decided, unsuperseded rows %v", got, want)
	}
}

func TestSearchScopeTable(t *testing.T) {
	const word = "quince"
	cases := []struct {
		name                string
		scope               model.Scope
		repo, session       string
		taskID              string // set on the row by raw SQL
		callerRepo          string
		callerTask, callerS string
		want                bool
	}{
		{"global from another repo", model.ScopeGlobal, "/other", "s9", "", testRepo, "", "s1", true},
		{"global with an empty caller", model.ScopeGlobal, "/other", "s9", "", "", "", "", true},
		{"repo, same repo", model.ScopeRepo, testRepo, "s9", "", testRepo, "", "s1", true},
		{"repo, different repo", model.ScopeRepo, "/other", "s1", "", testRepo, "", "s1", false},
		{"repo, both repos empty", model.ScopeRepo, "", "s1", "", "", "", "s1", false},
		{"task, same session, no task", model.ScopeTask, testRepo, "s1", "", testRepo, "", "s1", true},
		{"task, another session", model.ScopeTask, testRepo, "s9", "", testRepo, "", "s1", false},
		{"task, both sessions empty", model.ScopeTask, testRepo, "", "", testRepo, "", "", false},
		{"task, equal task id from another session", model.ScopeTask, "/other", "s9", "T1", testRepo, "T1", "s1", true},
		{"task, other task id, same session", model.ScopeTask, testRepo, "s1", "T1", testRepo, "T2", "s1", false},
		{"task with a task id, caller without one, same session", model.ScopeTask, testRepo, "s1", "T1", testRepo, "", "s1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := dbPath(t)
			s := open(t, path)
			d := decided(t, s, "Adopt the "+word+" layout?", c.session, c.repo, c.scope)
			if c.taskID != "" {
				rawExec(t, path, `UPDATE decisions SET task_id = ? WHERE id = ?`, c.taskID, d.ID)
			}
			got := search(t, s, store.PrecedentQuery{Text: word, Repo: c.callerRepo, TaskID: c.callerTask, SessionID: c.callerS})
			if found := len(got) == 1 && got[0] == d.ID; found != c.want || len(got) > 1 {
				t.Errorf("found %v, want match %v", got, c.want)
			}
		})
	}
}

func TestSearchRanksQuestionAboveContext(t *testing.T) {
	s := open(t, dbPath(t))
	// The question row is answered first, so only the ranking puts it first.
	inQuestion := ticket(t, "Ripen the persimmon queue?", model.ModeBlocking, "s1", t0)
	inQuestion = decide(t, s, inQuestion, t0.Add(time.Hour))
	inContext := ticket(t, "Ripen the other queue?", model.ModeBlocking, "s1", t0)
	inContext.Context = "The persimmon worker reads it."
	inContext = decide(t, s, inContext, t0.Add(2*time.Hour))
	got := search(t, s, caller("persimmon"))
	if want := []string{inQuestion.ID, inContext.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("order %v, want the question match first %v", got, want)
	}
}

func TestSearchNewestFirstOnEqualRank(t *testing.T) {
	s := open(t, dbPath(t))
	older := decide(t, s, ticket(t, "Prune the walnut log?", model.ModeBlocking, "s1", t0), t0.Add(time.Hour))
	newer := decide(t, s, ticket(t, "Prune the walnut log?", model.ModeBlocking, "s2", t0), t0.Add(2*time.Hour))
	if got, want := search(t, s, caller("walnut")), []string{newer.ID, older.ID}; !reflect.DeepEqual(got, want) {
		t.Errorf("order %v, want the newer answer first %v", got, want)
	}
}

func TestSearchStemming(t *testing.T) {
	s := open(t, dbPath(t))
	d := decided(t, s, "Squash the old migrations?", "s1", testRepo, model.ScopeRepo)
	if got := search(t, s, caller("migration")); !reflect.DeepEqual(got, []string{d.ID}) {
		t.Errorf("found %v, want %s", got, d.ID)
	}
}

func TestSearchTagsAnyOf(t *testing.T) {
	s := open(t, dbPath(t))
	mk := func(session string, tags ...string) string {
		d := ticket(t, "Index the mango table?", model.ModeBlocking, session, t0)
		d.Tags = tags
		return decide(t, s, d, t0.Add(time.Hour)).ID
	}
	db := mk("s1", "db")
	dbPerf := mk("s2", "db", "perf")
	ui := mk("s3", "ui")
	none := mk("s4")
	cases := []struct {
		tags []string
		want []string
	}{
		{[]string{"db"}, []string{db, dbPerf}},
		{[]string{"d"}, nil},
		{[]string{"b"}, nil},
		{[]string{"db perf"}, nil},
		{[]string{"%"}, nil},
		{[]string{"DB "}, []string{db, dbPerf}},
		{[]string{" Ui", "  "}, []string{ui}},
		{[]string{""}, []string{db, dbPerf, ui, none}},
		{[]string{" ", "\t"}, []string{db, dbPerf, ui, none}},
		{[]string{"ui", "perf"}, []string{dbPerf, ui}},
		{nil, []string{db, dbPerf, ui, none}},
	}
	for _, c := range cases {
		got := search(t, s, store.PrecedentQuery{Text: "mango", Repo: testRepo, Tags: c.tags, Limit: 10})
		slices.Sort(got)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("tags %q: found %v, want %v", c.tags, got, want)
		}
	}
}

func TestSearchExcludeAndLimit(t *testing.T) {
	s := open(t, dbPath(t))
	var all []string
	for i := range 12 {
		d := ticket(t, "Peel the lychee cache?", model.ModeBlocking, fmt.Sprintf("s%d", i), t0)
		all = append(all, decide(t, s, d, t0.Add(time.Duration(i+1)*time.Minute)).ID)
	}
	q := caller("lychee")
	for _, c := range []struct{ limit, want int }{{0, 5}, {-1, 5}, {3, 3}, {10, 10}, {50, 10}} {
		q.Limit = c.limit
		if got := search(t, s, q); len(got) != c.want {
			t.Errorf("Limit %d: %d rows, want %d", c.limit, len(got), c.want)
		}
	}
	q.Limit = 10
	newest := all[len(all)-1]
	if got := search(t, s, q); len(got) == 0 || got[0] != newest {
		t.Fatalf("top row %v, want %s", got, newest)
	}
	q.ExcludeID = newest
	got := search(t, s, q)
	if slices.Contains(got, newest) || len(got) != 10 {
		t.Errorf("ExcludeID %s: found %v, want 10 rows without it", newest, got)
	}
}

func TestSearchHostileQuery(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	d := decided(t, s, "Paint the zebra crossing?", "s1", testRepo, model.ScopeGlobal)
	rowTokens := precedent.Tokens(strings.Join([]string{
		d.Question, d.Context, d.Rationale, d.Answer,
		d.AnswerReasoning, strings.Join(d.Tags, " "),
	}, " "))
	inputs := []string{
		`NEAR(`, `"unterminated`, `col:x`, `* OR *`, strings.Repeat("(", 500<<10), "",
		"!!!", strings.Repeat("x", 1<<20), "\x00", "zebra\x00crossing",
		`"; DROP TABLE decisions; --`, `question:zebra`, `NEAR(zebra "paint" *)`,
		`zebra*`, `^zebra`, `{question context}: zebra`, `"zebra" AND NOT "paint"`,
		"\xff\xfe zebra", strings.Repeat("zebra OR ", 100000),
	}
	for _, in := range inputs {
		got, err := s.SearchDecisions(context.Background(), caller(in))
		if err != nil {
			t.Errorf("SearchDecisions(%.40q): %v", in, err)
			continue
		}
		// Rows come back only when a real token of the input is in the row.
		wantRow := slices.ContainsFunc(precedent.Tokens(in), func(tok string) bool { return slices.Contains(rowTokens, tok) })
		if found := len(got) == 1 && got[0].ID == d.ID; found != wantRow || len(got) > 1 {
			t.Errorf("SearchDecisions(%.40q) = %v, want a row %v", in, decisionIDs(got), wantRow)
		}
	}
	if n := countDecisions(t, path); n != 1 {
		t.Errorf("%d decisions after the hostile queries, want 1", n)
	}
}

func TestSearchSeesAnswerText(t *testing.T) {
	s := open(t, dbPath(t))
	d := create(t, s, ticket(t, "Ship the release today?", model.ModeBlocking, "s1", t0))
	q := caller("tangerine")
	if got := search(t, s, q); len(got) != 0 {
		t.Errorf("before the answer: found %v, want none", got)
	}
	answer(t, s, d.ID, model.Answer{Key: "A", Reasoning: "The tangerine branch is frozen.", Via: model.AnsweredCLI}, t0.Add(time.Hour))
	if got := search(t, s, q); !reflect.DeepEqual(got, []string{d.ID}) {
		t.Errorf("after the answer: found %v, want %s", got, d.ID)
	}
}

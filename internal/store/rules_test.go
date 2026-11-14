package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// ruleText is a standing rule only its own tests' words find.
const ruleText = "Never vendor the kumquat dependency; use Go modules instead."

// repoRule is a rule for testRepo given from the command line.
func repoRule(text string) store.Rule {
	return store.Rule{Text: text, Scope: model.ScopeRepo, Repo: testRepo, Via: model.AnsweredCLI}
}

// globalRule is a global rule given from the command line.
func globalRule(text string) store.Rule {
	return store.Rule{Text: text, Scope: model.ScopeGlobal, Via: model.AnsweredCLI}
}

// createRule stores r at now and requires a new row.
func createRule(t *testing.T, s *store.Store, r store.Rule, now time.Time) model.Decision {
	t.Helper()
	d, dup, err := s.CreateRule(context.Background(), r, now)
	if err != nil {
		t.Fatalf("CreateRule(%.40q): %v", r.Text, err)
	}
	if dup {
		t.Fatalf("CreateRule(%.40q) duplicate = true (%s), want a new row", r.Text, d.ID)
	}
	return d
}

// ruleHash is the args hash of section 4.11.
func ruleHash(scope model.Scope, repo, text string) string {
	sum := sha256.Sum256([]byte("rule\n" + string(scope) + "\n" + repo + "\n" + text))
	return hex.EncodeToString(sum[:])
}

func TestCreateRuleIsDecidedPrecedent(t *testing.T) {
	s := open(t, dbPath(t))
	got := createRule(t, s, repoRule(ruleText), t0)
	want := model.Decision{
		N: 1, ID: "D-1", Repo: testRepo, Question: ruleText, Options: []model.Option{},
		Reversibility: model.ReversCheap, Mode: model.ModeBlocking, Scope: model.ScopeRepo, Tags: []string{},
		Status: model.StatusAnswered, Answer: ruleText, AnsweredVia: model.AnsweredCLI,
		ArgsHash: ruleHash(model.ScopeRepo, testRepo, ruleText), Delivery: model.DeliveryNone,
		CreatedAt: t0, AnsweredAt: t0,
	}
	assertDecision(t, got, want)
	assertDecision(t, getDecision(t, s, "D-1"), want)
	if len(got.Options) != 0 {
		t.Errorf("options = %v, want none (that is what marks a rule)", got.Options)
	}
	if ids := search(t, s, caller("kumquat dependency")); !slices.Equal(ids, []string{"D-1"}) {
		t.Errorf("search from the repo = %v, want [D-1]", ids)
	}

	// The question is the rule cut to 300 runes on one line; the answer is
	// the full text.
	long := "Prefer table driven tests.\n" + strings.Repeat("Each case names its input. ", 20) + "Finally zebracorn."
	d := createRule(t, s, globalRule(long), t0)
	if strings.ContainsAny(d.Question, "\n\r") || utf8.RuneCountInString(d.Question) != 300 {
		t.Errorf("question = %q (%d runes), want one line of 300 runes", d.Question, utf8.RuneCountInString(d.Question))
	}
	if !strings.HasPrefix(d.Question, "Prefer table driven tests. Each case") {
		t.Errorf("question = %.60q, want the rule's start on one line", d.Question)
	}
	if d.Answer != long {
		t.Errorf("answer = %.60q, want the full text", d.Answer)
	}
	if ids := search(t, s, caller("zebracorn")); !slices.Equal(ids, []string{d.ID}) {
		t.Errorf("search for a word past the cut = %v, want [%s]", ids, d.ID)
	}

	// No via: cli.
	r := globalRule("Run gofumpt before committing.")
	r.Via = ""
	if d := createRule(t, s, r, t0); d.AnsweredVia != model.AnsweredCLI {
		t.Errorf("answered_via with no via = %q, want cli", d.AnsweredVia)
	}
	r = globalRule("Squash merge every pull request.")
	r.Via = model.AnsweredPopup
	if d := createRule(t, s, r, t0); d.AnsweredVia != model.AnsweredPopup {
		t.Errorf("answered_via = %q, want popup", d.AnsweredVia)
	}
}

func TestRuleScopeTable(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	repo := createRule(t, s, repoRule("Kumquat logging goes to stderr."), t0)
	g := globalRule("Kumquat errors are wrapped with context.")
	g.Repo = testRepo // ignored for a global rule
	global := createRule(t, s, g, t0)
	if global.Repo != "" || global.ArgsHash != ruleHash(model.ScopeGlobal, "", g.Text) {
		t.Errorf("global rule repo %q hash %s, want no repo in either", global.Repo, global.ArgsHash)
	}
	for _, c := range []struct {
		name string
		repo string
		want []string
	}{
		{"its repo", testRepo, []string{repo.ID, global.ID}},
		{"another repo", "/src/other", []string{global.ID}},
		{"no repo", "", []string{global.ID}},
	} {
		got := search(t, s, store.PrecedentQuery{Text: "kumquat", Repo: c.repo, SessionID: "s9"})
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: search = %v, want %v", c.name, got, c.want)
		}
	}

	for _, r := range []store.Rule{
		{Text: "Task scoped rule.", Scope: model.ScopeTask, Repo: testRepo},
		{Text: "Repo rule without a repo.", Scope: model.ScopeRepo},
		{Text: "No scope at all.", Repo: testRepo},
		{Text: "Unknown scope.", Scope: "galaxy", Repo: testRepo},
		{Text: "   ", Scope: model.ScopeGlobal},
	} {
		if _, _, err := s.CreateRule(context.Background(), r, t0); !errors.Is(err, store.ErrBadRule) {
			t.Errorf("CreateRule(%q, scope %q, repo %q) = %v, want ErrBadRule", r.Text, r.Scope, r.Repo, err)
		}
	}
	if n := countDecisions(t, path); n != 2 {
		t.Errorf("%d decisions after the bad rules, want 2", n)
	}
}

func TestIdenticalRuleCollapses(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	first := createRule(t, s, repoRule(ruleText), t0)
	again, dup, err := s.CreateRule(ctx, repoRule(ruleText), t0.Add(time.Minute))
	if err != nil || !dup || again.ID != first.ID {
		t.Fatalf("second CreateRule = %s, %v, %v; want %s, true, nil", again.ID, dup, err, first.ID)
	}
	if !again.CreatedAt.Equal(t0) {
		t.Errorf("duplicate created_at = %v, want the first row's %v", again.CreatedAt, t0)
	}
	if n := countDecisions(t, path); n != 1 {
		t.Fatalf("%d rows after the same rule twice, want 1", n)
	}

	// The same text in another scope or repo is another rule.
	other := repoRule(ruleText)
	other.Repo = "/src/other"
	if d := createRule(t, s, other, t0); d.ID != "D-2" {
		t.Errorf("same text in another repo = %s, want D-2", d.ID)
	}
	if d := createRule(t, s, globalRule(ruleText), t0); d.ID != "D-3" {
		t.Errorf("same text, global = %s, want D-3", d.ID)
	}

	// After supersession the rule can be given again.
	supersede(t, s, first.ID, t0.Add(time.Hour))
	if d := createRule(t, s, repoRule(ruleText), t0.Add(2*time.Hour)); d.ID != "D-4" {
		t.Errorf("after supersession = %s, want D-4", d.ID)
	}

	// Only a row with no session is a rule's twin.
	rawExec(t, path, `UPDATE decisions SET session_id = 's1' WHERE id = 'D-3'`)
	if d := createRule(t, s, globalRule(ruleText), t0); d.ID != "D-5" {
		t.Errorf("twin with a session = %s, want a new row D-5", d.ID)
	}
}

func TestRuleCanBeRescopedAndSuperseded(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	r := createRule(t, s, repoRule(ruleText), t0)
	elsewhere := store.PrecedentQuery{Text: "kumquat", Repo: "/src/other"}
	if got := search(t, s, elsewhere); len(got) != 0 {
		t.Fatalf("repo rule found from another repo: %v", got)
	}
	d, err := s.EditDecision(ctx, r.ID, model.DecisionEdit{Scope: model.ScopeGlobal}, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("EditDecision rescope: %v", err)
	}
	if d.Scope != model.ScopeGlobal {
		t.Errorf("scope = %s, want global", d.Scope)
	}
	if got := search(t, s, elsewhere); !slices.Equal(got, []string{r.ID}) {
		t.Errorf("rescoped rule from another repo = %v, want [%s]", got, r.ID)
	}
	d = supersede(t, s, r.ID, t0.Add(2*time.Hour))
	if d.Delivery != model.DeliveryNone || d.Status != model.StatusAnswered {
		t.Errorf("superseded rule delivery %s status %s, want none answered", d.Delivery, d.Status)
	}
	if got := search(t, s, elsewhere); len(got) != 0 {
		t.Errorf("superseded rule still found: %v", got)
	}
	if got := search(t, s, caller("kumquat")); len(got) != 0 {
		t.Errorf("superseded rule still found from its repo: %v", got)
	}
}

func TestRuleNeverQueuesDelivery(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	liveSession(t, s, "s1", "%1", model.StateIdle)
	queued(t, s, "Which queue backs the kumquat jobs?", "s1")
	now := t0.Add(time.Hour)
	beforeOpen, beforeQ, err := s.BadgeCounts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	createRule(t, s, repoRule(ruleText), now)
	createRule(t, s, globalRule("Kumquat jobs are idempotent."), now)
	afterOpen, afterQ, err := s.BadgeCounts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if afterOpen != beforeOpen || afterQ != beforeQ {
		t.Errorf("BadgeCounts after rules = %d %d, want %d %d", afterOpen, afterQ, beforeOpen, beforeQ)
	}
	counts, err := s.OpenCounts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := counts[""]; ok {
		t.Errorf("OpenCounts has rules under no session: %+v", c)
	}
}

func TestFTSStaysInSyncWithRules(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	now := t0.Add(time.Hour)
	create(t, s, ticket(t, "Indexed ticket question?", model.ModeBlocking, "s1", t0))
	createRule(t, s, repoRule(ruleText), t0)
	createRule(t, s, globalRule(strings.Repeat("Long standing rule text. ", 20)+"Ends with quokka."), t0)
	answer(t, s, "D-1", model.Answer{Text: "Use a bloom filter.", Reasoning: "Memory is tight."}, now)
	if _, err := s.EditDecision(ctx, "D-2", model.DecisionEdit{Scope: model.ScopeGlobal}, now); err != nil {
		t.Fatal(err)
	}
	supersede(t, s, "D-3", now)
	createRule(t, s, globalRule(strings.Repeat("Long standing rule text. ", 20)+"Ends with quokka."), now)
	db := rawDB(t, path)
	if err := ftsIntegrity(db); err != nil {
		t.Errorf("integrity-check: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM decisions_fts WHERE decisions_fts MATCH 'quokka'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("MATCH quokka = %d rows, %v; want 2 (the answer holds the full text)", n, err)
	}
}

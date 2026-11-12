package decision

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// goodTicket is the skill's good-ticket example (Task 5).
const goodTicket = `{
  "question": "Store webhook events in Postgres or in the existing Redis?",
  "context": "internal/events/store.go writes about 200 events a minute. Replays need 30 days of history.",
  "options": [
    {"key": "A", "label": "Postgres table", "tradeoffs": "Durable and queryable. Needs a migration."},
    {"key": "B", "label": "Redis stream", "tradeoffs": "No migration. History is capped by memory."}
  ],
  "recommendation": "A",
  "rationale": "Replays need 30 days of history, which Redis cannot hold at this volume. We already run Postgres.",
  "reversibility": "costly",
  "mode": "blocking",
  "scope": "repo",
  "tags": ["storage", "events"]
}`

func goodRequest(t *testing.T) Request {
	t.Helper()
	r, ps := Parse(json.RawMessage(goodTicket))
	if ps != nil {
		t.Fatalf("Parse(good) problems = %v", ps)
	}
	return Normalize(r)
}

func TestValidateAcceptsGoodTicket(t *testing.T) {
	if ps := Validate(goodRequest(t)); len(ps) != 0 {
		t.Errorf("Validate(good) = %v, want none", ps)
	}
}

func words(n int) string {
	return strings.TrimSpace(strings.Repeat("w ", n))
}

func TestValidateReportsEveryProblem(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(r *Request)
		field string
	}{
		{"missing question", func(r *Request) { r.Question = "" }, "question"},
		{"301-rune question", func(r *Request) { r.Question = strings.Repeat("q", 301) }, "question"},
		{"2001-rune context", func(r *Request) { r.Context = strings.Repeat("c", 2001) }, "context"},
		{"1 option", func(r *Request) { r.Options = r.Options[:1] }, "options"},
		{"5 options", func(r *Request) {
			for _, k := range []string{"C", "D", "E"} {
				r.Options = append(r.Options, model.Option{Key: k, Label: "label " + k})
			}
		}, "options"},
		{"key a b", func(r *Request) { r.Options[1].Key = "a b" }, "options[1].key"},
		{"9-rune key", func(r *Request) { r.Options[1].Key = "B12345678" }, "options[1].key"},
		{"non-ASCII key", func(r *Request) { r.Options[1].Key = "é" }, "options[1].key"},
		{"duplicate keys A and a", func(r *Request) { r.Options[1].Key = "a" }, "options[1].key"},
		{"empty label", func(r *Request) { r.Options[0].Label = "" }, "options[0].label"},
		{"81-rune label", func(r *Request) { r.Options[0].Label = strings.Repeat("l", 81) }, "options[0].label"},
		{"301-rune tradeoffs", func(r *Request) { r.Options[1].Tradeoffs = strings.Repeat("t", 301) }, "options[1].tradeoffs"},
		{"recommendation Z", func(r *Request) { r.Recommendation = "Z" }, "recommendation"},
		{"recommendation wrong case", func(r *Request) { r.Recommendation = "a" }, "recommendation"},
		{"missing recommendation", func(r *Request) { r.Recommendation = "" }, "recommendation"},
		{"missing rationale", func(r *Request) { r.Rationale = "" }, "rationale"},
		{"151-word rationale", func(r *Request) { r.Rationale = words(151) }, "rationale"},
		{"1201-rune rationale", func(r *Request) { r.Rationale = strings.Repeat("r", 1201) }, "rationale"},
		{"reversibility maybe", func(r *Request) { r.Reversibility = "maybe" }, "reversibility"},
		{"mode ask", func(r *Request) { r.Mode = "ask" }, "mode"},
		{"irreversible + assume", func(r *Request) {
			r.Reversibility, r.Mode, r.Assumption = "irreversible", "assume", "we use Postgres"
		}, "mode"},
		{"assume without assumption", func(r *Request) { r.Mode = "assume" }, "assumption"},
		{"301-rune assumption", func(r *Request) {
			r.Mode, r.Assumption = "assume", strings.Repeat("a", 301)
		}, "assumption"},
		{"blocking with assumption", func(r *Request) { r.Assumption = "we use Postgres" }, "assumption"},
		{"scope team", func(r *Request) { r.Scope = "team" }, "scope"},
		{"9 tags", func(r *Request) {
			r.Tags = []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9"}
		}, "tags"},
		{"tag Has Space", func(r *Request) { r.Tags = []string{"Has Space"} }, "tags"},
		{"33-rune tag", func(r *Request) { r.Tags = []string{strings.Repeat("t", 33)} }, "tags"},
		{"tag starting with -", func(r *Request) { r.Tags = []string{"-x"} }, "tags"},
		{"differs_from 117", func(r *Request) { r.DiffersFrom, r.DiffersWhy = "117", "new facts" }, "differs_from"},
		{"differs_from without differs_why", func(r *Request) { r.DiffersFrom = "D-12" }, "differs_why"},
		{"301-rune differs_why", func(r *Request) {
			r.DiffersFrom, r.DiffersWhy = "D-12", strings.Repeat("w", 301)
		}, "differs_why"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := goodRequest(t)
			tt.edit(&r)
			ps := Validate(Normalize(r))
			if len(ps) != 1 || ps[0].Field != tt.field || ps[0].Problem == "" {
				t.Errorf("problems = %v, want exactly one on %q", ps, tt.field)
			}
		})
	}

	t.Run("three rules broken", func(t *testing.T) {
		r := goodRequest(t)
		r.Scope, r.Question, r.Mode = "team", "", "ask"
		ps := Validate(Normalize(r))
		var got []string
		for _, p := range ps {
			got = append(got, p.Field)
		}
		want := []string{"question", "mode", "scope"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("fields = %v, want %v", got, want)
		}
	})
}

func TestValidateRefusesLineBreakInOneLineFields(t *testing.T) {
	r := goodRequest(t)
	r.Question = "a\nb"
	r.Options[0].Label = "x\ry"
	ps := Validate(r) // not normalized: Normalize would remove the breaks
	if len(ps) != 2 || ps[0].Field != "question" || ps[1].Field != "options[0].label" {
		t.Errorf("problems = %v, want question and options[0].label", ps)
	}
}

func TestValidateAcceptsLimits(t *testing.T) {
	r := goodRequest(t)
	r.Question = strings.Repeat("q", MaxQuestion)
	r.Context = strings.Repeat("c", MaxContext)
	r.Options = []model.Option{
		{Key: "A", Label: strings.Repeat("l", MaxLabel), Tradeoffs: strings.Repeat("t", MaxTradeoffs)},
		{Key: "B1234567", Label: "b"},
		{Key: "c", Label: "c"},
		{Key: "D", Label: "d"},
	}
	r.Rationale = words(MaxRationaleWords)
	r.Mode, r.Assumption = "assume", strings.Repeat("a", MaxAssumption)
	r.Scope = "global"
	r.Tags = []string{"a", "b", "c", "d", "e", "f", "g", strings.Repeat("h", MaxTag)}
	r.DiffersFrom, r.DiffersWhy = "D-999999999", strings.Repeat("w", MaxDiffersWhy)
	if ps := Validate(Normalize(r)); len(ps) != 0 {
		t.Errorf("Validate at the limits = %v, want none", ps)
	}
}

func TestNormalizeCleansAndSortsTags(t *testing.T) {
	r := Normalize(Request{
		Question: " a\x1b[31m b \n",
		Context:  "  first paragraph\x07\n\n\n\nsecond paragraph \n",
		Options:  []model.Option{{Key: " A ", Label: " Postgres\ttable ", Tradeoffs: " x \n y "}},
		Tags:     []string{"DB", "api", "db"},
	})
	if r.Question != "a b" {
		t.Errorf("Question = %q, want %q", r.Question, "a b")
	}
	if want := "first paragraph\n\nsecond paragraph"; r.Context != want {
		t.Errorf("Context = %q, want %q", r.Context, want)
	}
	if want := (model.Option{Key: "A", Label: "Postgres table", Tradeoffs: "x\ny"}); r.Options[0] != want {
		t.Errorf("Options[0] = %+v, want %+v", r.Options[0], want)
	}
	if want := []string{"api", "db"}; !reflect.DeepEqual(r.Tags, want) {
		t.Errorf("Tags = %q, want %q", r.Tags, want)
	}
}

func TestNormalizeBoundsWork(t *testing.T) {
	r := Normalize(Request{Question: strings.Repeat("q", 10000), Rationale: strings.Repeat("r ", 10000)})
	if n := len([]rune(r.Question)); n != MaxQuestion+1 {
		t.Errorf("question runes = %d, want %d", n, MaxQuestion+1)
	}
	if n := len([]rune(r.Rationale)); n != MaxRationale+1 {
		t.Errorf("rationale runes = %d, want %d", n, MaxRationale+1)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	messy := Request{
		Question:       "  why\x1b]0;title\x07 now?\t" + strings.Repeat("x", 400),
		Context:        "\n\n para one \r\n\r\n\r\n para two\x00 \n" + strings.Repeat("c", 3000),
		Options:        []model.Option{{Key: " A\n", Label: strings.Repeat("l", 100), Tradeoffs: " t \n\n\n u "}},
		Recommendation: " A ",
		Rationale:      strings.Repeat("word ", 400),
		Reversibility:  " cheap ",
		Mode:           "\tassume",
		Assumption:     " we\n\n\nassume ",
		Tags:           []string{"Zed", " zed ", "B", "", "a b"},
		DiffersFrom:    " D-1 ",
		DiffersWhy:     " because \n",
	}
	once := Normalize(messy)
	if twice := Normalize(once); !reflect.DeepEqual(once, twice) {
		t.Errorf("Normalize is not idempotent:\nonce  %#v\ntwice %#v", once, twice)
	}
	if ArgsHash(messy) != ArgsHash(once) {
		t.Error("ArgsHash(messy) != ArgsHash(Normalize(messy))")
	}
}

func TestDefaultScopeIsRepo(t *testing.T) {
	if got := Normalize(Request{}).Scope; got != "repo" {
		t.Errorf("Normalize scope = %q, want repo", got)
	}
	r := goodRequest(t)
	withScope := ArgsHash(r)
	r.Scope = ""
	if ArgsHash(r) != withScope {
		t.Error("an empty scope does not hash as repo")
	}
	if d := New(r, Caller{}, time.Unix(0, 0), time.Hour); d.Scope != model.ScopeRepo {
		t.Errorf("New scope = %q, want repo", d.Scope)
	}
}

func TestParseTypeErrorIsOneProblem(t *testing.T) {
	_, ps := Parse(json.RawMessage(`{"options":"x"}`))
	if len(ps) != 1 || ps[0].Field != "options" {
		t.Errorf(`Parse({"options":"x"}) = %v, want one problem on options`, ps)
	}

	extra := `{"extra":1,` + strings.TrimPrefix(strings.TrimSpace(goodTicket), "{")
	r, ps := Parse(json.RawMessage(extra))
	if ps != nil {
		t.Fatalf("Parse(extra field) = %v, want none", ps)
	}
	if !reflect.DeepEqual(Normalize(r), goodRequest(t)) {
		t.Errorf("Parse(extra field) = %+v", r)
	}

	for _, bad := range []string{``, `{`, `[]`, `"x"`, `{"question":1,"mode":true}`} {
		if _, ps := Parse(json.RawMessage(bad)); len(ps) != 1 {
			t.Errorf("Parse(%q) = %v, want one problem", bad, ps)
		}
	}
}

// hashVector is the request of TestArgsHashVector. Its canonical JSON is
// (one line, no trailing newline; "<", ">" and "&" are not escaped):
//
//	{"question":"Use <b> & \"x\"?","context":"line one\nline two","options":[{"key":"A","label":"Yes ≈ fine","tradeoffs":""},{"key":"B","label":"No","tradeoffs":"costs more"}],"recommendation":"A","rationale":"Because.","reversibility":"cheap","mode":"assume","assumption":"we go with A","scope":"repo","tags":["api","db"]}
//
// and its hash comes from: printf '%s' '<that JSON>' | shasum -a 256
var hashVector = Request{
	Question: `Use <b> & "x"?`,
	Context:  "line one\nline two",
	Options: []model.Option{
		{Key: "A", Label: "Yes ≈ fine"},
		{Key: "B", Label: "No", Tradeoffs: "costs more"},
	},
	Recommendation: "A",
	Rationale:      "Because.",
	Reversibility:  "cheap",
	Mode:           "assume",
	Assumption:     "we go with A",
	Tags:           []string{"db", "api"},
	DiffersFrom:    "D-3",
	DiffersWhy:     "not hashed",
}

func TestArgsHashVector(t *testing.T) {
	const want = "a18090463ab540678f9cbee42c90ed7f8677f9dc113d7151e2aca245ae8dc2d5"
	if got := ArgsHash(hashVector); got != want {
		t.Errorf("ArgsHash = %s, want %s", got, want)
	}
}

func TestArgsHashIgnoresDiffersAndTagOrder(t *testing.T) {
	base := ArgsHash(goodRequest(t))
	r := goodRequest(t)
	r.DiffersFrom, r.DiffersWhy = "D-7", "the facts changed"
	r.Tags = []string{"events", "storage", "Events"}
	r.Question = "  " + r.Question + "  "
	r.Options[0].Label = " " + r.Options[0].Label + "\t"
	r.Rationale += " \n"
	if got := ArgsHash(r); got != base {
		t.Errorf("ArgsHash changed: %s != %s", got, base)
	}
}

func TestArgsHashChangesWithMeaning(t *testing.T) {
	base := ArgsHash(goodRequest(t))
	edits := map[string]func(r *Request){
		"label":        func(r *Request) { r.Options[0].Label = "Postgres tables" },
		"option order": func(r *Request) { r.Options[0], r.Options[1] = r.Options[1], r.Options[0] },
		"mode":         func(r *Request) { r.Mode, r.Assumption = "assume", "Postgres" },
		"scope":        func(r *Request) { r.Scope = "global" },
		"tags":         func(r *Request) { r.Tags = nil },
	}
	for name, edit := range edits {
		r := goodRequest(t)
		edit(&r)
		if ArgsHash(r) == base {
			t.Errorf("a different %s gives the same hash", name)
		}
	}
}

func TestNewSetsExpiryForAssumeOnly(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c := Caller{SessionID: "s1", TaskID: "t1", Repo: "/repo"}
	r := goodRequest(t)
	r.DiffersFrom, r.DiffersWhy = "D-2", "new facts"
	want := model.Decision{
		SessionID: "s1", TaskID: "t1", Repo: "/repo",
		Question: r.Question, Context: r.Context, Options: r.Options,
		Recommendation: "A", Rationale: r.Rationale,
		Reversibility: model.ReversCostly, Mode: model.ModeBlocking,
		Scope: model.ScopeRepo, Tags: []string{"events", "storage"},
		Status: model.StatusOpen, DiffersFrom: "D-2", DiffersWhy: "new facts",
		ArgsHash: ArgsHash(r), Delivery: model.DeliveryNone, CreatedAt: now,
	}
	if d := New(r, c, now, 24*time.Hour); !reflect.DeepEqual(d, want) {
		t.Errorf("New(blocking) =\n%+v\nwant\n%+v", d, want)
	}

	r.Mode, r.Assumption = "assume", "Postgres"
	want.Mode, want.Assumption, want.ArgsHash = model.ModeAssume, "Postgres", ArgsHash(r)
	want.ExpiresAt = now.Add(24 * time.Hour)
	if d := New(r, c, now, 24*time.Hour); !reflect.DeepEqual(d, want) {
		t.Errorf("New(assume) =\n%+v\nwant\n%+v", d, want)
	}
}

func TestNewNormalizes(t *testing.T) {
	r := goodRequest(t)
	r.Question = " " + r.Question + "\x1b[1m"
	if d := New(r, Caller{}, time.Unix(0, 0), time.Hour); d.Question != goodRequest(t).Question {
		t.Errorf("New question = %q", d.Question)
	}
}

func TestValidID(t *testing.T) {
	for _, id := range []string{"D-1", "D-117", "D-999999999"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false", id)
		}
	}
	for _, id := range []string{"D-0", "D-01", "d-1", "D-1 ", "D-1;x", "", "D-", "D-1000000000", " D-1", "D-١"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}

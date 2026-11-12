package precedent

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

func TestTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Should the events table use Postgres or SQLite?", []string{"events", "table", "use", "postgres", "sqlite"}},
		{"a I É x1 日本語 don't", []string{"x1", "日本語", "don"}},
		{"Postgres postgres POSTGRES events Events", []string{"postgres", "events"}},
		{"", nil},
		{"room² ⅳ", []string{"room²"}}, // ⅳ is one rune
		{"हिन्दी भाषा", []string{"हिन्दी", "भाषा"}},
		{"cafe\u0301 au", []string{"cafe\u0301", "au"}},
		{strings.Repeat("x", 64), []string{strings.Repeat("x", 64)}},
		{strings.Repeat("x", 65), nil},
		{strings.Repeat("x", 1<<20), nil},
	}
	for _, c := range cases {
		if got := Tokens(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func words(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

func TestMatchExprQuotesAndCaps(t *testing.T) {
	if got, want := MatchExpr("postgres events"), `"postgres" OR "events"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got := MatchExpr(strings.Join(words("word", 40), " "))
	if n := len(strings.Split(got, " OR ")); n != MaxTokens {
		t.Errorf("got %d terms, want %d: %s", n, MaxTokens, got)
	}
	if !strings.HasPrefix(got, `"word0" OR "word1"`) || !strings.HasSuffix(got, `"word15"`) {
		t.Errorf("wrong terms kept: %s", got)
	}
}

func TestMatchExprHostile(t *testing.T) {
	safe := regexp.MustCompile(`^("[\p{L}\p{N}]+"( OR "[\p{L}\p{N}]+")*)?$`)
	inputs := []string{
		`NEAR("x" *) : ^ - + AND OR NOT {col}`,
		`"; DROP TABLE decisions; --`,
		`"unbalanced quote`,
		`col:foo* ^bar (baz OR qux) NOT quux`,
		"nul\x00byte \xff\xfe invalid utf8 events",
		strings.Repeat("alpha ", 100000),
		strings.Repeat(`"(`, 50000),
		strings.Repeat("x", 1<<20),
	}
	for _, in := range inputs {
		got := MatchExpr(in)
		if !safe.MatchString(got) {
			n := len(got)
			if n > 200 {
				got = got[:200]
			}
			t.Errorf("unsafe output (len %d) for input %.40q: %q", n, in, got)
		}
	}
	// Operators survive only as plain quoted words, never as syntax.
	if got, want := MatchExpr("NEAR AND foo"), `"near" OR "foo"`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMatchExprEmpty(t *testing.T) {
	for _, in := range []string{"", "the of a", "!!!", `"" * ()`} {
		if got := MatchExpr(in); got != "" {
			t.Errorf("MatchExpr(%q) = %q, want empty", in, got)
		}
	}
}

func TestSimilarity(t *testing.T) {
	j, shared := Similarity("Should the events table use Postgres or SQLite?", "Use Postgres or SQLite for the events store?")
	if shared != 4 || j != 4.0/6.0 {
		t.Errorf("got %v, %d; want 4/6, 4", j, shared)
	}
	if j, s := Similarity("alpha beta", "Beta ALPHA"); j != 1 || s != 2 {
		t.Errorf("identical: got %v, %d", j, s)
	}
	if j, s := Similarity("alpha beta", "gamma delta"); j != 0 || s != 0 {
		t.Errorf("disjoint: got %v, %d", j, s)
	}
	if j, s := Similarity("", "the of"); j != 0 || s != 0 {
		t.Errorf("empty: got %v, %d", j, s)
	}
}

func dec(id, q0 string) model.Decision {
	return model.Decision{ID: id, Question: q0}
}

func TestStrongThreshold(t *testing.T) {
	q := "aa bb cc dd ee"
	cands := []model.Decision{
		dec("1", "aa bb cc dd ff"), // 4 shared, union 6: 0.67
		dec("2", "aa bb cc ff gg"), // 3 shared, union 7: 0.43, below
		dec("3", "aa bb cc"),       // 3 shared, union 5: 0.6
		dec("4", "aa bb"),          // 1.0 of itself but only 2 shared
	}
	got := Strong(q, cands)
	if ids := idsOf(got); !reflect.DeepEqual(ids, []string{"1", "3"}) {
		t.Errorf("ids = %v, want [1 3]", ids)
	}

	// Exactly 0.6 passes, just under 0.6 fails.
	q2 := strings.Join(words("k", 10), " ")
	atEdge := dec("5", strings.Join(words("k", 6), " "))                                      // 6/10
	under := dec("6", strings.Join(words("k", 10), " ")+" "+strings.Join(words("n", 7), " ")) // 10/17 = 0.588
	got = Strong(q2, []model.Decision{under, atEdge})
	if ids := idsOf(got); !reflect.DeepEqual(ids, []string{"5"}) {
		t.Errorf("edge ids = %v, want [5]", ids)
	}
}

func TestStrongOrderBestFirstStable(t *testing.T) {
	q := "aa bb cc dd"
	cands := []model.Decision{
		dec("1", "aa bb cc dd ee"), // 0.8
		dec("2", "aa bb cc dd"),    // 1.0
		dec("3", "aa bb cc dd ff"), // 0.8, after 1
	}
	if ids := idsOf(Strong(q, cands)); !reflect.DeepEqual(ids, []string{"2", "1", "3"}) {
		t.Errorf("ids = %v, want [2 1 3]", ids)
	}
	if got := Strong("", cands); len(got) != 0 {
		t.Errorf("empty question matched %v", got)
	}
}

func TestStrongCapsAtThree(t *testing.T) {
	var cands []model.Decision
	for i := 1; i <= 5; i++ {
		cands = append(cands, dec(fmt.Sprint(i), "aa bb cc dd"))
	}
	got := Strong("aa bb cc dd", cands)
	if ids := idsOf(got); !reflect.DeepEqual(ids, []string{"1", "2", "3"}) {
		t.Errorf("ids = %v, want [1 2 3]", ids)
	}
}

func idsOf(ds []model.Decision) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func TestStrongRepeatedWordsDoNotInflate(t *testing.T) {
	// Distinct tokens: {aa bb} vs {aa bb cc}: only 2 shared however often repeated.
	got := Strong("aa aa aa bb bb", []model.Decision{dec("1", "aa bb cc aa bb")})
	if len(got) != 0 {
		t.Errorf("repeats inflated the shared count: %v", idsOf(got))
	}
}

func TestMatchExprHugeWordIsEmpty(t *testing.T) {
	if got := MatchExpr(strings.Repeat("x", 1<<20)); got != "" {
		t.Errorf("got %d bytes, want empty", len(got))
	}
}

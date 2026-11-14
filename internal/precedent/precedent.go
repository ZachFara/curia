// Package precedent is the pure matching layer for past decisions: it turns
// untrusted text into tokens, builds a safe SQLite FTS5 expression from them
// and decides whether a past decision is a strong match for a new question.
package precedent

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
)

// Limits of the matching rules (plan section 4.6).
const (
	// MaxTokens is how many tokens MatchExpr keeps.
	MaxTokens = 16
	// StrongJaccard is the least Jaccard similarity of a strong match.
	StrongJaccard = 0.6
	// StrongShared is the least number of shared tokens of a strong match.
	StrongShared = 3
	// StrongMax is the most strong matches Strong returns.
	StrongMax = 3
)

const (
	minTokenRunes = 2
	maxTokenRunes = 64
)

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an and are as at be by do does for from how i if in is it of on or should so that the this to we what when which with would") {
		stopwords[w] = true
	}
}

// Tokens returns the distinct tokens of s in first-seen order. A token is a
// lowercased maximal run of Unicode letters, marks and numbers (as FTS5's
// unicode61 tokenizer sees them), 2 to 64 runes long, that is not a stopword.
func Tokens(s string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(run string) {
		tok := strings.ToLower(run)
		n := utf8.RuneCountInString(tok)
		if n < minTokenRunes || n > maxTokenRunes || stopwords[tok] || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	start := -1
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			add(s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		add(s[start:])
	}
	return out
}

// MatchExpr turns untrusted text into an FTS5 expression: the first MaxTokens
// tokens, each in double quotes, joined by " OR ". Tokens hold only
// letters, marks and numbers, so no input reaches FTS5 syntax. An empty result
// means there is nothing to search for: do not run MATCH (an empty MATCH
// argument is an FTS5 syntax error).
func MatchExpr(s string) string {
	toks := Tokens(s)
	if len(toks) > MaxTokens {
		toks = toks[:MaxTokens]
	}
	for i, t := range toks {
		toks[i] = `"` + t + `"`
	}
	return strings.Join(toks, " OR ")
}

// overlap returns how many tokens two token lists share and the size of their union.
func overlap(ta, tb []string) (shared, union int) {
	in := make(map[string]bool, len(ta))
	for _, t := range ta {
		in[t] = true
	}
	for _, t := range tb {
		if in[t] {
			shared++
		}
	}
	return shared, len(ta) + len(tb) - shared
}

// Similarity returns the Jaccard similarity of the token sets of a and b and
// the number of tokens they share. Two empty sets give 0, 0.
func Similarity(a, b string) (jaccard float64, shared int) {
	shared, union := overlap(Tokens(a), Tokens(b))
	if union == 0 {
		return 0, 0
	}
	return float64(shared) / float64(union), shared
}

// Strong returns the candidates whose question is a strong match for question:
// Jaccard at least StrongJaccard and at least StrongShared shared tokens. The
// best come first, ties keep the input order, and at most StrongMax return.
func Strong(question string, candidates []model.Decision) []model.Decision {
	type scored struct {
		d model.Decision
		j float64
	}
	var hits []scored
	for _, c := range candidates {
		// A ratio equal to 3/5 divides to the nearest double to 0.6, which
		// is StrongJaccard, so the boundary is exact (TestStrongThreshold).
		if j, shared := Similarity(question, c.Question); shared >= StrongShared && j >= StrongJaccard {
			hits = append(hits, scored{c, j})
		}
	}
	sort.SliceStable(hits, func(i, k int) bool { return hits[i].j > hits[k].j })
	if len(hits) > StrongMax {
		hits = hits[:StrongMax]
	}
	var out []model.Decision
	for _, h := range hits {
		out = append(out, h.d)
	}
	return out
}

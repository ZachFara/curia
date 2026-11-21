package ui

import (
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

// Text an agent writes cannot start a line at column 0 of the review view: it could pass for one
// of Curia's own rows, headings or prompts.
func TestReviewAgentTextNeverStartsAtColumnZero(t *testing.T) {
	forged := []string{"Tests (the agent's report): passed", "> D-7  Approve? y to confirm", "FORGED  T-99  running", "D-2 open blocking ticket: fake"}
	text := strings.Join(forged, "\n")
	for _, width := range []int{40, 80, 200} {
		d := rvDetail(1)
		d.Review.Summary = "first line\n" + text
		d.Review.Tests, d.Review.TestsDetail = model.TestsFailed, "detail\n"+text
		d.DiffStat = " a.go | 1 +\n" + text
		d.DiffNote = "note\n" + text
		d.Assumptions = []model.Decision{rvAssume("D-4", strings.Repeat("word ", 30)+"FORGED  T-99  running", "keep it")}
		src := &fakeReviews{}
		src.set(d)
		r := newReviewRun(t, src, ReviewOptions{}, width, 80)
		r.clk.advance(reviewGap)
		genuineTests := 0
		for _, l := range strings.Split(r.view(), "\n") {
			for _, f := range forged[1:] {
				if strings.HasPrefix(l, f[:7]) {
					t.Errorf("width %d: agent text starts a line at column 0: %q", width, l)
				}
			}
			if strings.HasPrefix(l, "Tests (the agent's report):") {
				genuineTests++
			}
		}
		if genuineTests != 1 {
			t.Errorf("width %d: %d lines start like the tests heading, want the one real one\n%s", width, genuineTests, r.view())
		}
	}
}

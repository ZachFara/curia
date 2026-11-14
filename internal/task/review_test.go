package task

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
)

func TestValidReviewID(t *testing.T) {
	for _, id := range []string{"R-1", "R-42", "R-999999999"} {
		if !ValidReviewID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"R-0", "R-01", "r-1", "R-1 ", "R-1;x", "T-1", "", "R-1000000000", "R-1\n"} {
		if ValidReviewID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func fields(ps []Problem) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Field)
	}
	return out
}

func TestValidateCompletionReportsEveryProblem(t *testing.T) {
	ok := Completion{Summary: "done", Tests: model.TestsPassed, TestsDetail: "fine"}
	if ps := ValidateCompletion(ok); len(ps) != 0 {
		t.Fatalf("valid completion: %v", ps)
	}
	cases := []struct {
		name string
		c    Completion
		want []string
	}{
		{"empty summary", Completion{Tests: model.TestsPassed}, []string{"summary"}},
		{"long summary", Completion{Summary: strings.Repeat("a", 2001), Tests: model.TestsFailed}, []string{"summary"}},
		{"max summary", Completion{Summary: strings.Repeat("a", 2000), Tests: model.TestsNotRun}, nil},
		{"bad tests", Completion{Summary: "x", Tests: "ok"}, []string{"tests_status"}},
		{"empty tests", Completion{Summary: "x"}, []string{"tests_status"}},
		{"long detail", Completion{Summary: "x", Tests: model.TestsPassed, TestsDetail: strings.Repeat("d", 201)}, []string{"tests_detail"}},
		{"all wrong", Completion{Tests: "", TestsDetail: strings.Repeat("d", 201)}, []string{"summary", "tests_status", "tests_detail"}},
	}
	for _, tc := range cases {
		got := fields(ValidateCompletion(tc.c))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: fields %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeCompletionCleans(t *testing.T) {
	c := NormalizeCompletion(Completion{
		Summary:     "a\x1b[2Jb\u202ec\n\n\n\n\nd",
		Tests:       model.TestsPassed,
		TestsDetail: "line one\nline two",
	})
	if c.Summary != "abc\n\nd" {
		t.Errorf("summary %q", c.Summary)
	}
	if c.TestsDetail != "line one line two" {
		t.Errorf("detail %q", c.TestsDetail)
	}
	if c.Tests != model.TestsPassed {
		t.Errorf("tests %q", c.Tests)
	}
	long := NormalizeCompletion(Completion{Summary: strings.Repeat("a", 2001)})
	if utf8.RuneCountInString(long.Summary) != 2001 {
		t.Errorf("normalize must not cut: %d", utf8.RuneCountInString(long.Summary))
	}
}

func TestNormalizeNote(t *testing.T) {
	got, err := NormalizeNote("  fix \x1b[31mthis\n\n\n now ")
	if err != nil || got != "fix this\n\nnow" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "  \n ", strings.Repeat("x", 601), "\x1b[2J"} {
		if _, err := NormalizeNote(bad); !errors.Is(err, ErrBadNote) {
			t.Errorf("%q: err %v", bad, err)
		}
	}
	if _, err := NormalizeNote(strings.Repeat("x", 600)); err != nil {
		t.Errorf("600 runes: %v", err)
	}
}

func TestNoteMessage(t *testing.T) {
	if got := NoteMessage("R-3", "T-42", "handle nil"); got != "Review R-3 of T-42: changes requested. handle nil" {
		t.Errorf("got %q", got)
	}
	got := NoteMessage("R-999999999", "T-999999999", strings.Repeat("é", 600))
	if n := utf8.RuneCountInString(got); n > 800 {
		t.Errorf("%d runes", n)
	}
}

func TestPRTitleHasNoMention(t *testing.T) {
	if got := PRTitle(model.Task{ID: "T-42", Title: "Fix @alice's bug"}); got != "T-42: Fix alice's bug" {
		t.Errorf("got %q", got)
	}
	if got := PRTitle(model.Task{ID: "T-42", Title: strings.Repeat("a", 200)}); utf8.RuneCountInString(got) != 120 {
		t.Errorf("%d runes", utf8.RuneCountInString(got))
	}
	got := PRTitle(model.Task{ID: "T-42", Title: "two\nlines\r\nhere"})
	if got != "T-42: two lines here" {
		t.Errorf("got %q", got)
	}
}

func prFixture(detail string) (model.Task, model.Review) {
	return model.Task{ID: "T-42", Title: "Fix"}, model.Review{
		ID: "R-3", TaskID: "T-42", Summary: "Fixed it.\nSecond line.",
		Tests: model.TestsPassed, TestsDetail: detail,
	}
}

func TestPRBodyLayout(t *testing.T) {
	d := []model.Decision{{ID: "D-4"}, {ID: "D-9"}}
	tk, r := prFixture("make check: 412 passed")
	want := "Curia task T-42, review R-3.\n\n" +
		"Tests (the agent's report): passed. make check: 412 passed\n\n" +
		"Decisions on this task: D-4, D-9\n\n" +
		"Summary by the agent:\n\n~~~~text\nFixed it.\nSecond line.\n~~~~\n"
	if got := PRBody(tk, r, d); got != want {
		t.Errorf("with decisions and detail:\n%q\nwant\n%q", got, want)
	}
	want = "Curia task T-42, review R-3.\n\n" +
		"Tests (the agent's report): passed.\n\n" +
		"Summary by the agent:\n\n~~~~text\nFixed it.\nSecond line.\n~~~~\n"
	tk, r = prFixture("")
	if got := PRBody(tk, r, nil); got != want {
		t.Errorf("bare:\n%q\nwant\n%q", got, want)
	}
}

func TestPRBodyFencesSummary(t *testing.T) {
	tk, r := prFixture("")
	r.Summary = "ok\n~~~~\n@everyone ![x](http://evil)\n~~~~~~~~"
	body := PRBody(tk, r, nil)
	open := strings.Index(body, "~~~~text\n")
	if open < 0 || !strings.HasSuffix(body, "\n~~~~\n") {
		t.Fatalf("fence missing:\n%s", body)
	}
	inside := body[open+len("~~~~text\n") : len(body)-len("~~~~\n")]
	for _, l := range strings.Split(inside, "\n") {
		if strings.HasPrefix(l, "~~~") {
			t.Errorf("line %q could close the fence", l)
		}
	}
	if strings.Contains(body[:open], "@") {
		t.Errorf("@ outside the fence:\n%s", body[:open])
	}
	if !strings.Contains(inside, "@everyone") {
		t.Errorf("summary lost: %q", inside)
	}
}

func TestPRBodyCapsAtEightKiB(t *testing.T) {
	tk, r := prFixture(strings.Repeat("d", 200))
	r.Summary = strings.Repeat("😀", 2000)
	body := PRBody(tk, r, []model.Decision{{ID: "D-1"}})
	if len(body) > MaxPRBody {
		t.Errorf("len %d", len(body))
	}
	if !utf8.ValidString(body) {
		t.Error("invalid UTF-8")
	}
	if !strings.HasSuffix(body, "\n~~~~\n") {
		t.Error("closing fence missing")
	}
	if len(body) < MaxPRBody-8 {
		t.Errorf("cut too early: %d", len(body))
	}
}

func TestPRBodyDetailCannotMention(t *testing.T) {
	tk, r := prFixture("@bob `x`\nnext")
	body := PRBody(tk, r, nil)
	line := strings.Split(body, "\n")[2]
	if !strings.HasPrefix(line, "Tests (the agent's report): passed.") || strings.ContainsAny(line, "@`") {
		t.Errorf("tests line %q", line)
	}
}

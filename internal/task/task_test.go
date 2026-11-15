package task

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

func TestValidID(t *testing.T) {
	for _, id := range []string{"T-1", "T-42", "T-999999999"} {
		if !ValidID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"T-0", "T-01", "t-1", "T-1 ", "T-1;x", "D-1", "", "T-1234567890", "T-1\n"} {
		if ValidID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}

func good() model.Task {
	return model.Task{Title: "Fix auth", Repo: "/src/api", BaseBranch: "main", AgentModel: "opus[1m]", Tags: []string{"a", "b"}}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	if ps := Validate(good()); len(ps) != 0 {
		t.Fatalf("good task: %v", ps)
	}
	nine := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	rows := []struct {
		name  string
		mod   func(*model.Task)
		field string
	}{
		{"empty title", func(x *model.Task) { x.Title = "" }, "title"},
		{"121 runes", func(x *model.Task) { x.Title = strings.Repeat("é", 121) }, "title"},
		{"two lines", func(x *model.Task) { x.Title = "a\nb" }, "title"},
		{"49 KiB body", func(x *model.Task) { x.Body = strings.Repeat("a", 49<<10) }, "body"},
		{"relative repo", func(x *model.Task) { x.Repo = "src/api" }, "repo"},
		{"uncleaned repo", func(x *model.Task) { x.Repo = "/src/../api" }, "repo"},
		{"empty repo", func(x *model.Task) { x.Repo = "" }, "repo"},
		{"base -x", func(x *model.Task) { x.BaseBranch = "-x" }, "base_branch"},
		{"base a..b", func(x *model.Task) { x.BaseBranch = "a..b" }, "base_branch"},
		{"base /a", func(x *model.Task) { x.BaseBranch = "/a" }, "base_branch"},
		{"base 201", func(x *model.Task) { x.BaseBranch = strings.Repeat("a", 201) }, "base_branch"},
		{"9 tags", func(x *model.Task) { x.Tags = nine }, "tags"},
		{"bad tag", func(x *model.Task) { x.Tags = []string{"Has Space"} }, "tags"},
		{"bad model", func(x *model.Task) { x.AgentModel = "opus; rm" }, "agent_model"},
	}
	for _, r := range rows {
		x := good()
		r.mod(&x)
		ps := Validate(x)
		if len(ps) != 1 || ps[0].Field != r.field {
			t.Errorf("%s: got %v, want one problem on %s", r.name, ps, r.field)
		}
	}
	x := good()
	x.Title, x.Repo, x.AgentModel = "", "rel", "bad one"
	ps := Validate(x)
	var fields []string
	for _, p := range ps {
		fields = append(fields, p.Field)
	}
	if !reflect.DeepEqual(fields, []string{"title", "repo", "agent_model"}) {
		t.Errorf("fields = %v", fields)
	}
}

func TestNormalizeKeepsBodyLayout(t *testing.T) {
	body := "Intro\n\n```go\nfunc x() {\n\treturn\n}\n```\n\n\n  indented"
	got := Normalize(model.Task{Title: "t", Body: body})
	if got.Body != body {
		t.Errorf("body changed: %q", got.Body)
	}
	got = Normalize(model.Task{Title: "a\x1b[2Jb", Body: "x\x1b[2Jy", Tags: []string{"Zed", "alpha", "ALPHA", "mid"}})
	if got.Body != "xy" || got.Title != "ab" {
		t.Errorf("escape kept: %q %q", got.Title, got.Body)
	}
	if !reflect.DeepEqual(got.Tags, []string{"alpha", "mid", "zed"}) {
		t.Errorf("tags = %v", got.Tags)
	}
	long := Normalize(model.Task{Title: strings.Repeat("a", 500)})
	if ps := Validate(model.Task{Title: long.Title, Repo: "/r"}); len(ps) != 1 || ps[0].Field != "title" {
		t.Errorf("overlong title not reported after Normalize: %v", ps)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Fix the auth refresh!": "fix-the-auth-refresh",
		"  Über  émile ":        "ber-mile",
		"!!!":                   "task",
		"":                      "task",
		strings.Repeat("a", 80): strings.Repeat("a", 40),
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	s := Slug(strings.Repeat("a", 39) + " bbb")
	if len(s) > MaxSlug || strings.HasSuffix(s, "-") {
		t.Errorf("slug %q", s)
	}
}

func TestBranchAndPaths(t *testing.T) {
	if got := Branch("T-42", "Fix auth"); got != "curia/T-42-fix-auth" {
		t.Error(got)
	}
	if got := WorktreePath("/src/api-server", "T-42"); got != "/src/.curia-worktrees/api-server-T-42" {
		t.Error(got)
	}
}

func TestSessionName(t *testing.T) {
	cases := map[string]string{"/src/api-server": "api-server", "/src/my.repo:x": "my_repo_x", "/": "curia"}
	for in, want := range cases {
		if got := SessionName(in); got != want {
			t.Errorf("SessionName(%q) = %q, want %q", in, got, want)
		}
	}
	re := regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	for _, in := range []string{"/", "", "/a/" + strings.Repeat("x.", 50), "/a/;", "/a/é"} {
		if got := SessionName(in); !re.MatchString(got) {
			t.Errorf("SessionName(%q) = %q", in, got)
		}
	}
}

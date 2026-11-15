package task

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

const rlo = "\xe2\x80\xae" // U+202E, written as bytes so no tool rewrites it

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "backlogmd", name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseCapturedFiles(t *testing.T) {
	tests := []struct {
		file string
		want Imported
	}{
		{"task-1 - Add login page.md", Imported{Ref: "task-1", Title: "Add login page", Tags: []string{"auth", "web"}}},
		{"task-2 - Fix 'auth' refresh.md", Imported{Ref: "task-2", Title: "Fix: the 'auth' refresh", Tags: []string{"bug", "needs-review"}}},
		{"task-3 - Old chore.md", Imported{Ref: "task-3", Title: "Old chore", Done: true}},
	}
	for _, tc := range tests {
		got, err := ParseBacklogMD(tc.file, readFixture(t, tc.file))
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got.Body == "" {
			t.Errorf("%s: empty body", tc.file)
		}
		got.Body = ""
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v want %+v", tc.file, got, tc.want)
		}
	}
	if _, err := ParseBacklogMD("plain.md", readFixture(t, "plain.md")); !errors.Is(err, ErrNotATask) {
		t.Errorf("plain.md: %v", err)
	}
}

// The captured files are real Backlog.md output (probe P8). Original names:
// "task-1 - Fix-the-auth-refresh.md", "task-2 - Write-the-README-intro.md",
// "task-3 - Ship-the-v1-release.md".
func TestParseRealCapturedFiles(t *testing.T) {
	tests := []struct {
		file, name string
		want       Imported
	}{
		{"captured-1.md", "task-1 - Fix-the-auth-refresh.md", Imported{Ref: "TASK-1", Title: "Fix: the 'auth' refresh", Tags: []string{"backend", "auth", "needs-review"}}},
		{"captured-2.md", "task-2 - Write-the-README-intro.md", Imported{Ref: "TASK-2", Title: "Write the README intro"}},
		{"captured-3.md", "task-3 - Ship-the-v1-release.md", Imported{Ref: "TASK-3", Title: "Ship the v1 release", Tags: []string{"release"}, Done: true}},
	}
	for _, tc := range tests {
		got, err := ParseBacklogMD(tc.name, readFixture(t, tc.file))
		if err != nil || got.Body == "" || !strings.Contains(got.Body, "## Description") {
			t.Fatalf("%s: %v body %q", tc.file, err, got.Body)
		}
		got.Body = ""
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v want %+v", tc.file, got, tc.want)
		}
		// Same file without its title key: the dashed slug becomes the title.
		noTitle := strings.Join(slicesDropTitle(string(readFixture(t, tc.file))), "\n")
		fb, err := ParseBacklogMD(tc.name, []byte(noTitle))
		if err != nil || fb.Title != strings.ReplaceAll(strings.TrimSuffix(tc.name[len("task-1 - "):], ".md"), "-", " ") {
			t.Errorf("%s fallback title %q %v", tc.file, fb.Title, err)
		}
	}
}

func slicesDropTitle(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "title:") {
			out = append(out, l)
		}
	}
	return out
}

func TestParseInlineAndBlockLabels(t *testing.T) {
	inline, _ := ParseBacklogMD("a.md", []byte("---\ntitle: T\nlabels: [One, 'two words', \"c_3\"]\n---\n"))
	block, _ := ParseBacklogMD("a.md", []byte("---\ntitle: T\nlabels:\n  - One\n  - 'two words'\n  - c_3\nstatus: Done\n---\n"))
	want := []string{"one", "two-words", "c_3"}
	if !reflect.DeepEqual(inline.Tags, want) || !reflect.DeepEqual(block.Tags, want) {
		t.Errorf("inline %v block %v want %v", inline.Tags, block.Tags, want)
	}
	if !block.Done {
		t.Error("status after labels not read")
	}
	many := "---\ntitle: T\nlabels: [a,b,c,d,e,f,g,h,i,j]\n---\n"
	got, _ := ParseBacklogMD("a.md", []byte(many))
	if len(got.Tags) != 8 {
		t.Errorf("tags %v, want 8", got.Tags)
	}
}

func TestParseQuotedTitle(t *testing.T) {
	got, err := ParseBacklogMD("a.md", []byte("---\ntitle: \"Fix: the 'auth' refresh\"\n---\n"))
	if err != nil || got.Title != "Fix: the 'auth' refresh" {
		t.Errorf("%+v %v", got, err)
	}
	got, _ = ParseBacklogMD("a.md", []byte("---\ntitle: 'It''s'\n---\n"))
	if got.Title != "It's" {
		t.Errorf("single-quoted title %q", got.Title)
	}
}

func TestParseTitleFallsBackToFileName(t *testing.T) {
	got, err := ParseBacklogMD("task-12 - Fix login.md", []byte("---\nstatus: To Do\n---\nbody\n"))
	if err != nil || got.Title != "Fix login" || got.Ref != "task-12 - Fix login.md" {
		t.Errorf("%+v %v", got, err)
	}
}

func TestParseNotATask(t *testing.T) {
	cases := map[string][]byte{
		"no front matter": []byte("# hi\n"),
		"unclosed":        []byte("---\ntitle: x\nbody\n"),
		"empty":           nil,
		"too big":         []byte("---\ntitle: x\n---\n" + strings.Repeat("a\n", 150<<10)),
	}
	for name, data := range cases {
		if _, err := ParseBacklogMD("a.md", data); !errors.Is(err, ErrNotATask) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseHostile(t *testing.T) {
	src := "---\n" +
		"id: \"t\x1b[31m-1" + rlo + "\"\n" +
		"title: Evil \x1b]0;pwn\x07 " + rlo + "title\x00\n" +
		"status: \x1b[1mDone\n" +
		"labels: [\"\x1b[2JX" + rlo + "\", \"\x00\"]\n" +
		"anchor: &a value\n" +
		"alias: *a\n" +
		"multi: |\n  line one\n  title: injected\n" +
		"title2: >\n  folded\n" +
		"???\n" +
		"---\nbody \x1b[31mred" + rlo + " text\x07\n"
	got, err := ParseBacklogMD("x\x1b.md", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	all := got.Ref + got.Title + got.Body + strings.Join(got.Tags, "")
	for _, r := range all {
		if r == '\n' {
			continue
		}
		if unicode.IsControl(r) || strings.ContainsRune(rlo, r) {
			t.Fatalf("control rune %q in %+v", r, got)
		}
	}
	if got.Title != "Evil title" {
		t.Errorf("title %q", got.Title)
	}
	if strings.Contains(got.Title, "injected") {
		t.Error("multi-line scalar leaked")
	}
	if !got.Done {
		t.Error("done not read through escapes")
	}
}

func TestParseBounds(t *testing.T) {
	long := strings.Repeat("é", 500)
	got, err := ParseBacklogMD("a.md", []byte("---\ntitle: "+long+"\n---\n"+strings.Repeat("x", 100<<10)))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got.Title)); n > 120 {
		t.Errorf("title %d runes", n)
	}
	if n := len(got.Body); n > 48<<10 {
		t.Errorf("body %d bytes", n)
	}
}

func TestParseCRLF(t *testing.T) {
	got, err := ParseBacklogMD("a.md", []byte("---\r\ntitle: T\r\nstatus: Done\r\n---\r\nb\r\n"))
	if err != nil || got.Title != "T" || !got.Done {
		t.Errorf("%+v %v", got, err)
	}
}

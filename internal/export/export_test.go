package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
)

var answeredAt = time.Date(2026, 11, 13, 23, 59, 0, 0, time.UTC)

func blocking() model.Decision {
	return model.Decision{
		ID: "D-117", SessionID: "sess-1234", TaskID: "T-42", Repo: "/Users/x/src/api-server",
		Question: "Postgres or Redis for events?", Context: "We store 10k events a second.",
		Options: []model.Option{
			{Key: "A", Label: "Postgres", Tradeoffs: "durable, slower"},
			{Key: "B", Label: "Redis", Tradeoffs: "fast, volatile"},
		},
		Recommendation: "B", Mode: model.ModeBlocking, Scope: model.ScopeRepo,
		Tags: []string{"db", "events"}, Status: model.StatusAnswered,
		AnswerKey: "B", AnswerReasoning: "Speed matters more.", AnsweredAt: answeredAt,
	}
}

func global() model.Decision {
	d := blocking()
	d.Scope = model.ScopeGlobal
	return d
}

func TestNoteTitleIsAPlainName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Postgres or Redis for events?", "Postgres or Redis for events"},
		{"../../etc/passwd", "etc passwd"},
		{"a/b\\c:d*e", "a b c d e"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"...", ""},
		{"#tag [[link]] `x`", "tag link x"},
		{"it's (fine), ok-ish_", "it's (fine), ok-ish_"},
		{"a\x00b\nc", "ab c"},
	}
	for _, c := range cases {
		if got := NoteTitle(c.in); got != c.want {
			t.Errorf("NoteTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, in := range []string{strings.Repeat("x", 300), strings.Repeat("日本語 ", 100), strings.Repeat("𠀀", 300)} {
		got := NoteTitle(in)
		if n := utf8.RuneCountInString(got); n > MaxTitle || n == 0 {
			t.Errorf("NoteTitle of %d runes has %d runes", utf8.RuneCountInString(in), n)
		}
		if len(got) > 200 {
			t.Errorf("title is %d bytes", len(got))
		}
	}
	hostile := "a/\\.\x00\x01\x1f" + string(rune(0x202e)) + "\u2028b\u0085"
	got := NoteTitle(hostile)
	if strings.ContainsAny(got, "/\\.\x00") {
		t.Errorf("title %q holds a path character", got)
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Errorf("title %q holds control rune %U", got, r)
		}
	}
}

func TestNoteName(t *testing.T) {
	d := blocking()
	if got, want := NoteName(d), "D-117 Postgres or Redis for events.md"; got != want {
		t.Errorf("NoteName = %q, want %q", got, want)
	}
	d.Question = "?!..."
	if got := NoteName(d); got != "D-117.md" {
		t.Errorf("NoteName = %q", got)
	}
	// The id is never raw text: a hostile id cannot add a path.
	d.ID = "../../.ssh/x"
	got := NoteName(d)
	if strings.ContainsAny(got, "/\\") || strings.HasPrefix(got, ".") || !strings.HasSuffix(got, ".md") {
		t.Errorf("NoteName with hostile id = %q", got)
	}
	d.ID = ""
	if got := NoteName(d); strings.HasPrefix(got, ".") || got == "" {
		t.Errorf("NoteName with empty id = %q", got)
	}
	d = blocking()
	d.Question = strings.Repeat("𠀀", 300)
	if n := len(NoteName(d)); n > 255 {
		t.Errorf("name is %d bytes", n)
	}
}

func TestIsNoteOf(t *testing.T) {
	yes := []string{"D-1.md", "D-1 x.md", "D-1 .md"}
	no := []string{"D-11 x.md", "D-1x.md", "D-1 x.txt", "x D-1 .md", "D-1", "D-1 md", ".md", "sub/D-1.md"}
	for _, n := range yes {
		if !IsNoteOf(n, "D-1") {
			t.Errorf("IsNoteOf(%q) = false", n)
		}
	}
	for _, n := range no {
		if IsNoteOf(n, "D-1") {
			t.Errorf("IsNoteOf(%q) = true", n)
		}
	}
	if IsNoteOf(".md", "") || IsNoteOf(" x.md", "") {
		t.Error("an empty id owns nothing")
	}
}

func TestOwnsNote(t *testing.T) {
	note := Note(blocking())
	if !OwnsNote(note, "D-117") {
		t.Error("own note not owned")
	}
	if OwnsNote(note, "D-11") || OwnsNote(note, "D-1177") || OwnsNote(note, "D-9") {
		t.Error("another id owned")
	}
	late := strings.Repeat("title: x\n", 24) + "curia_id: D-1\n"
	if OwnsNote([]byte("---\n"+late+"---\n"), "D-1") {
		t.Error("marker on line 25 counted")
	}
	if !OwnsNote([]byte("---\ntitle: x\ncuria_id: D-1\n---\nbody"), "D-1") {
		t.Error("marker in front matter not found")
	}
	if !OwnsNote([]byte("---\r\ncuria_id: D-1\r\n---\r\n"), "D-1") {
		t.Error("CRLF marker not found")
	}
	for name, head := range map[string]string{
		"after front matter": "---\ntitle: x\n---\ncuria_id: D-1\n",
		"no front matter":    "curia_id: D-1\n",
		"in a fence":         "# x\n```\ncuria_id: D-1\n```\n",
		"not first line":     "\n---\ncuria_id: D-1\n---\n",
		"indented":           "---\n curia_id: D-1\n---\n",
		"empty":              "",
	} {
		if OwnsNote([]byte(head), "D-1") {
			t.Errorf("%s: owned", name)
		}
	}
}

func TestFenceCannotBeClosed(t *testing.T) {
	got := Fence("a ``` b\n`````\nc")
	if !strings.HasPrefix(got, "``````\n") || !strings.HasSuffix(got, "\n``````") {
		t.Errorf("Fence = %q", got)
	}
	if got := Fence("plain"); got != "```\nplain\n```" {
		t.Errorf("Fence = %q", got)
	}
	// Every run length from 1 to 10, alone and mixed with tildes and indentation.
	for n := 1; n <= 10; n++ {
		for _, text := range []string{
			strings.Repeat("`", n),
			"x\n" + strings.Repeat("`", n) + "\ny",
			"   " + strings.Repeat("`", n) + "  \n" + strings.Repeat("~", n+3),
			strings.Repeat("`", n) + "info",
		} {
			f := Fence(text)
			lines := strings.Split(f, "\n")
			open, closing := lines[0], lines[len(lines)-1]
			if open != closing || strings.Trim(open, "`") != "" || len(open) < 3 || len(open) <= n {
				t.Errorf("Fence(%q) = %q", text, f)
			}
			for _, l := range strings.Split(strings.Join(lines[1:len(lines)-1], "\n"), "\n") {
				if strings.HasPrefix(strings.TrimLeft(l, " "), open) {
					t.Errorf("line %q of %q could close %q", l, text, open)
				}
			}
		}
	}
	// A carriage return is a line end to Markdown: it never survives inside a fence.
	if f := Fence("a\r```\rb"); strings.Contains(f, "\r") {
		t.Errorf("Fence kept a CR: %q", f)
	}
}

const goldenBlocking = "---\n" +
	"curia_id: D-117\n" +
	"status: answered\n" +
	"scope: repo\n" +
	"mode: blocking\n" +
	"repo: \"api-server\"\n" +
	"task: \"T-42\"\n" +
	"date: 2026-11-13\n" +
	"tags: [\"db\", \"events\"]\n" +
	"---\n" +
	"# D-117 Postgres or Redis for events\n" +
	"## Question\n" +
	"```\nPostgres or Redis for events?\n```\n" +
	"## Context\n" +
	"```\nWe store 10k events a second.\n```\n" +
	"## Options\n" +
	"- A\n" +
	"```\nPostgres: durable, slower\n```\n" +
	"- B (recommended) (chosen)\n" +
	"```\nRedis: fast, volatile\n```\n" +
	"## Answer\n" +
	"```\nB: Redis\n```\n" +
	"## Reasoning\n" +
	"```\nSpeed matters more.\n```\n"

func TestNoteGolden(t *testing.T) {
	if got := string(Note(blocking())); got != goldenBlocking {
		t.Errorf("Note =\n%s\nwant\n%s", got, goldenBlocking)
	}
	// Overridden, global, nothing but a recommendation: the other chosen option and the
	// rest of the layout.
	d := model.Decision{
		ID: "D-3", Question: "Tabs or spaces?", Mode: model.ModeAssume, Scope: model.ScopeGlobal,
		Status: model.StatusOverridden, Options: []model.Option{{Key: "A", Label: "Tabs"}, {Key: "B", Label: "Spaces", Tradeoffs: "wide"}},
		Recommendation: "A", AnswerKey: "B", AnsweredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	want := "---\ncuria_id: D-3\nstatus: overridden\nscope: global\nmode: assume\ndate: 2026-01-02\ntags: []\n---\n" +
		"# D-3 Tabs or spaces\n## Question\n```\nTabs or spaces?\n```\n" +
		"## Options\n- A (recommended)\n```\nTabs\n```\n- B (chosen)\n```\nSpaces: wide\n```\n" +
		"## Answer\n```\nB: Spaces\n```\n"
	if got := string(Note(d)); got != want {
		t.Errorf("Note =\n%s\nwant\n%s", got, want)
	}
}

func TestNoteFreeTextAndRule(t *testing.T) {
	d := blocking()
	d.AnswerKey, d.Answer = "", "Use SQLite."
	got := string(Note(d))
	if strings.Contains(got, "(chosen)") {
		t.Error("free-text answer marks an option")
	}
	if !strings.Contains(got, "## Answer\n```\nUse SQLite.\n```\n") {
		t.Errorf("free text missing:\n%s", got)
	}
	rule := model.Decision{
		ID: "D-5", Question: "Always use gofumpt.", Scope: model.ScopeGlobal, Mode: model.ModeAssume,
		Status: model.StatusConfirmed, Answer: "Yes.", AnsweredAt: answeredAt,
	}
	got = string(Note(rule))
	if strings.Contains(got, "## Options") || strings.Contains(got, "## Context") || strings.Contains(got, "## Reasoning") {
		t.Errorf("empty sections kept:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("note must end with exactly one newline: %q", got[len(got)-4:])
	}
}

func TestNoteSuperseded(t *testing.T) {
	d := blocking()
	d.SupersededAt = answeredAt.Add(time.Hour)
	got := string(Note(d))
	if !strings.Contains(got, "\nstatus: superseded\n") || strings.Contains(got, "status: answered") {
		t.Errorf("status:\n%s", got)
	}
}

// frontMatter returns the lines between the first two "---" lines.
func frontMatter(t *testing.T, note string) []string {
	t.Helper()
	lines := strings.Split(note, "\n")
	if lines[0] != "---" {
		t.Fatalf("note starts with %q", lines[0])
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			return lines[1:i]
		}
	}
	t.Fatal("front matter never closes")
	return nil
}

// outsideFences returns the note with every fenced block removed.
func outsideFences(note string) string {
	var out []string
	fence := ""
	for _, l := range strings.Split(note, "\n") {
		switch {
		case fence == "" && strings.HasPrefix(l, "```") && strings.Trim(l, "`") == "":
			fence = l
		case fence != "" && l == fence:
			fence = ""
		case fence == "":
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestNoteHostileText(t *testing.T) {
	hostile := []string{
		"---\ncuria_id: D-9\n---",
		"![[secret]]",
		"```dataviewjs\ndv.pages()\n```",
		"```\n---\ncuria_id: D-9\n```",
		"<% tp.system.prompt() %>",
		"<script>alert(1)</script>",
		"<%* await tp.file.delete() *>",
		"{{query}} ~~~ ````` `````````` ",
		"\r```\r## Answer\r",
		"\x1b]0;x\x07 \x1b[31m ## Injected",
		" ``` ## Injected",
		"# heading\n- [ ] task\n> quote\n| a | b |",
	}
	for i, h := range hostile {
		d := blocking()
		d.Question, d.Context, d.AnswerReasoning, d.Answer, d.AnswerKey = h, h, h, h, ""
		d.Options = []model.Option{{Key: "A", Label: h, Tradeoffs: h}, {Key: h, Label: h}}
		d.Recommendation = h
		d.Tags = []string{h, `a"b`, "x\\y", "a\nb", "- c\n---"}
		d.Repo, d.TaskID = "/x/"+h, h
		d.ID = "D-1" // ids come from the store
		checkHostileNote(t, i, string(Note(d)))
	}
}

// checkHostileNote checks one note built from hostile text.
func checkHostileNote(t *testing.T, i int, note string) {
	t.Helper()
	fm := frontMatter(t, note)
	keys := map[string]int{}
	for _, l := range fm {
		k, _, ok := strings.Cut(l, ": ")
		if !ok {
			t.Fatalf("case %d: front matter line %q is not key: value", i, l)
		}
		keys[k]++
		if l == "---" || strings.HasPrefix(l, "- ") {
			t.Errorf("case %d: front matter line %q", i, l)
		}
	}
	for _, k := range []string{"curia_id", "date", "mode", "repo", "scope", "status", "tags", "task"} {
		if keys[k] != 1 {
			t.Errorf("case %d: key %q appears %d times", i, k, keys[k])
		}
	}
	if len(keys) != 8 {
		t.Errorf("case %d: keys %v", i, keys)
	}
	// The front matter holds only JSON-quoted values (checked above and in
	// TestNoteFrontMatterValuesAreQuoted); the body outside fences is ours.
	_, body, _ := strings.Cut(strings.TrimPrefix(note, "---\n"), "\n---\n")
	outside := outsideFences(body)
	for _, bad := range []string{"![[", "```dataview", "<%", "<script", "{{", "curia_id: D-9", "## Injected", "\r", "\u2028", "\u2029", "\x1b", "- [ ]", "| a |"} {
		if strings.Contains(outside, bad) {
			t.Errorf("case %d: %q outside a fence in:\n%s", i, bad, note)
		}
	}
	if !strings.Contains(outside, "## Question\n") || !strings.Contains(outside, "## Answer\n") {
		t.Errorf("case %d: headings changed:\n%s", i, outside)
	}
}

func TestNoteFrontMatterValuesAreQuoted(t *testing.T) {
	specials := []string{
		`a: b`, `"quoted"`, `'single'`, `# comment`, `- item`, `[x]`, `{y: z}`, `&anchor`, `*alias`, `!tag`,
		`|`, `>`, `%dir`, `@at`, "`tick`", `null`, `true`, `~`, `yes`, `: x`, `a\b`, `x # y`, `: `, `... `, `<%x%>`,
	}
	for _, s := range specials {
		d := blocking()
		d.Repo, d.TaskID = "/r/"+s, s
		d.Tags = []string{s}
		note := string(Note(d))
		fm := frontMatter(t, note)
		for _, l := range fm {
			k, v, _ := strings.Cut(l, ": ")
			switch k {
			case "repo", "task":
				var out string
				if err := json.Unmarshal([]byte(v), &out); err != nil {
					t.Errorf("%s value %q is not a quoted string: %v", k, v, err)
				}
			case "tags":
				var out []string
				if err := json.Unmarshal([]byte(v), &out); err != nil {
					t.Errorf("tags value %q does not parse: %v", v, err)
				}
				if len(out) != 1 {
					t.Errorf("tags %q: %v", v, out)
				}
			}
		}
	}
	// A hostile id, status, scope or mode cannot add a key either.
	d := blocking()
	d.ID = "D-1\nstatus: x"
	d.Status, d.Scope, d.Mode = "answered\ninjected: 1", "repo\n---", "x: y"
	fm := frontMatter(t, string(Note(d)))
	if len(fm) != 8 {
		t.Errorf("front matter has %d lines: %q", len(fm), fm)
	}
}

func TestNoteLongInputsAreBounded(t *testing.T) {
	big := strings.Repeat("z", 1<<20)
	d := blocking()
	d.Context = big
	got := Note(d)
	if len(got) > MaxNote {
		t.Fatalf("note is %d bytes", len(got))
	}
	if !bytes.HasSuffix(got, []byte("\n```\n")) {
		t.Errorf("note does not end with a closed fence: %q", got[len(got)-20:])
	}
	// Every field long at once, many options, many tags.
	d = blocking()
	d.Question, d.Context, d.Answer, d.AnswerKey, d.AnswerReasoning = big, big, big, "", big
	d.Recommendation = big
	d.Tags = nil
	d.Options = nil
	for i := 0; i < 500; i++ {
		d.Options = append(d.Options, model.Option{Key: strings.Repeat("k", 5000), Label: big, Tradeoffs: big})
		d.Tags = append(d.Tags, big[:5000])
	}
	d.Repo, d.TaskID = big, big
	got = Note(d)
	if len(got) > MaxNote {
		t.Fatalf("note is %d bytes", len(got))
	}
	if !utf8.Valid(got) || !bytes.HasSuffix(got, []byte("\n")) {
		t.Error("note is not valid text")
	}
	_, body, _ := strings.Cut(strings.TrimPrefix(string(got), "---\n"), "\n---\n")
	// Only the title in the heading (at most MaxTitle runes) is outside a fence.
	if strings.Count(outsideFences(body), "z") > MaxTitle {
		t.Error("long text escaped the fences")
	}
	// The context section is cut to MaxContext.
	d = blocking()
	d.Context = strings.Repeat("y", MaxContext*2)
	if n := strings.Count(string(Note(d)), "y"); n > MaxContext {
		t.Errorf("context is %d bytes", n)
	}
	// Multi-byte text is not cut inside a rune.
	d.Context = strings.Repeat("日", 1<<18)
	if got := Note(d); !utf8.Valid(got) || len(got) > MaxNote {
		t.Error("multi-byte note invalid or too long")
	}
}

func TestNoteIsDeterministic(t *testing.T) {
	d := blocking()
	if !bytes.Equal(Note(d), Note(d)) {
		t.Error("Note differs between calls")
	}
	if d.Tags[0] != "db" {
		t.Error("Note modified its argument")
	}
}

const wantCause = "We store 10k events a second.\n\nOptions considered: A: Postgres; B: Redis"

func TestCommonsEntry(t *testing.T) {
	got, err := CommonsEntry(global())
	if err != nil {
		t.Fatal(err)
	}
	want := Entry{
		Symptom:     "Decision: Postgres or Redis for events?",
		Environment: map[string]string{"source": "curia", "kind": "decision"},
		Cause:       wantCause,
		Fix:         "Chosen: B: Redis. Reasoning: Speed matters more.",
		Domains:     []string{"db", "events"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entry = %+v\nwant %+v", got, want)
	}
	raw, _ := json.Marshal(got)
	const wantJSON = `{"symptom":"Decision: Postgres or Redis for events?","environment":{"kind":"decision","source":"curia"},` +
		`"cause":"We store 10k events a second.\n\nOptions considered: A: Postgres; B: Redis",` +
		`"fix":"Chosen: B: Redis. Reasoning: Speed matters more.","domains":["db","events"]}`
	if string(raw) != wantJSON {
		t.Errorf("json = %s", raw)
	}

	// Free text, a standing rule, no tags: Domains is empty, never nil.
	d := global()
	d.AnswerKey, d.Answer, d.AnswerReasoning, d.Options, d.Context, d.Tags = "", "Use SQLite.", "", nil, "", nil
	got, err = CommonsEntry(d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fix != "Chosen: Use SQLite." || got.Cause != "" || got.Domains == nil || len(got.Domains) != 0 {
		t.Errorf("entry = %+v", got)
	}
	raw, _ = json.Marshal(got)
	if !strings.Contains(string(raw), `"domains":[]`) {
		t.Errorf("json = %s", raw)
	}
	// Confirmed and overridden are decided too.
	for _, s := range []model.DecisionStatus{model.StatusConfirmed, model.StatusOverridden} {
		d := global()
		d.Status = s
		if _, err := CommonsEntry(d); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestCommonsEntryRefusals(t *testing.T) {
	mut := map[string]func(d *model.Decision){
		"open":       func(d *model.Decision) { d.Status = model.StatusOpen },
		"withdrawn":  func(d *model.Decision) { d.Status = model.StatusWithdrawn },
		"expired":    func(d *model.Decision) { d.Status = model.StatusExpired },
		"repo":       func(d *model.Decision) { d.Scope = model.ScopeRepo },
		"task":       func(d *model.Decision) { d.Scope = model.ScopeTask },
		"no scope":   func(d *model.Decision) { d.Scope = "" },
		"superseded": func(d *model.Decision) { d.SupersededAt = answeredAt },
	}
	for name, f := range mut {
		d := global()
		f(&d)
		if _, err := CommonsEntry(d); !errors.Is(err, ErrNotPublishable) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCommonsEntryHoldsNoPaths(t *testing.T) {
	d := global()
	d.Repo, d.TaskID, d.SessionID = "/Users/x/src/api", "T-4", "sess-secret-77"
	d.ID = "D-117"
	got, err := CommonsEntry(d)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	for _, s := range []string{"/Users/x", "src/api", "api", "T-4", "sess-secret-77", "D-117"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("entry holds %q: %s", s, raw)
		}
	}
}

func TestCommonsEntryIsBoundedValidJSON(t *testing.T) {
	big := strings.Repeat("é", 1<<20)
	d := global()
	d.Question, d.Context, d.AnswerReasoning, d.AnswerKey, d.Answer = big, big, big, "", big
	d.Options = nil
	for i := 0; i < 300; i++ {
		d.Options = append(d.Options, model.Option{Key: "K", Label: big, Tradeoffs: big})
	}
	d.Tags = nil
	for i := 0; i < 300; i++ {
		d.Tags = append(d.Tags, big)
	}
	got, err := CommonsEntry(d)
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(got.Symptom); n > len("Decision: ")+500 {
		t.Errorf("symptom %d runes", n)
	}
	if n := utf8.RuneCountInString(got.Fix); n > 2000 {
		t.Errorf("fix %d runes", n)
	}
	raw, err := json.Marshal(got)
	if err != nil || len(raw) > 64<<10 {
		t.Fatalf("json: %v, %d bytes", err, len(raw))
	}
	var back Entry
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, got) {
		t.Errorf("round trip failed: %v", err)
	}
}

func TestCommonsEntryRoundTripsHostileText(t *testing.T) {
	for _, h := range []string{
		"\"quote\" \\ back \x00 nul \xff invalid   sep </script> \x1b[31m",
		"```\n---\n<% x %>", "", "\n\n\n",
	} {
		d := global()
		d.Question, d.Context, d.AnswerReasoning, d.Answer, d.AnswerKey = h, h, h, h, ""
		d.Options = []model.Option{{Key: "A", Label: h, Tradeoffs: h}}
		d.Tags = []string{h, "ok"}
		got, err := CommonsEntry(d)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var back Entry
		if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, got) {
			t.Errorf("round trip of %q failed: %v", h, err)
		}
		if !utf8.ValidString(got.Symptom + got.Cause + got.Fix) {
			t.Error("invalid UTF-8")
		}
		if strings.ContainsAny(got.Symptom+got.Cause+got.Fix, "\x00\x1b\r") {
			t.Error("control character kept")
		}
	}
}

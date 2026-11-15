package sanitize

import (
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func TestLineStripsEscapeSequences(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"csi", "\x1b[31mred\x1b[0m", "red"},
		{"osc bel", "\x1b]0;title\x07x", "x"},
		{"osc st", "\x1b]8;;http://e\x1b\\link", "link"},
		{"dcs", "a\x1bPdcs\x1b\\b", "ab"},
		{"sos", "a\x1bXsos\x1b\\b", "ab"},
		{"pm", "a\x1b^pm\x1b\\b", "ab"},
		{"apc", "a\x1b_apc\x1b\\b", "ab"},
		{"c1 csi", "a\u009b31mb", "ab"},
		{"c1 osc", "a\u009d0;t\x07b", "ab"},
		{"c1 dcs", "a\u0090dcs\x1b\\b", "ab"},
		{"c1 sos", "a\u0098s\x1b\\b", "ab"},
		{"c1 pm", "a\u009em\x1b\\b", "ab"},
		{"c1 apc", "a\u009fp\x1b\\b", "ab"},
		{"two byte", "a\x1bcb\x1b7c", "abc"},
		{"unterminated csi", "a\x1b[31", "a"},
		{"unterminated osc", "a\x1b]0;title", "a"},
		{"unterminated dcs", "a\x1bPdata\x1b", "a"},
		{"lone esc", "a\x1b", "a"},
		{"esc esc", "a\x1b\x1b[31mb", "ab"},
		{"csi aborted by esc", "a\x1b[31\x1b[0mb", "ab"},
		// T1: an escape with intermediate bytes takes its final letter too.
		{"charset designation", "a\x1b(Bb", "ab"},
		{"charset designation G1", "a\x1b)0b", "ab"},
		{"two intermediates", "a\x1b$(Bb", "ab"},
		{"line size", "a\x1b#8b", "ab"},
		{"unterminated nF", "a\x1b(", "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Line(c.in, 0); got != c.want {
				t.Errorf("Line(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestLineOneLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\r\nb\tc", "a b c"},
		{"  a   b ", "a b"},
		{"a\nb\rc", "a b c"},
		{"\n\t ", ""},
	}
	for _, c := range cases {
		if got := Line(c.in, 0); got != c.want {
			t.Errorf("Line(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLineDropsInvisible(t *testing.T) {
	for _, r := range []rune{0x202E, 0x2066, 0x200B, 0xFEFF, 0x0000, 0x007F} {
		in := "a" + string(r) + "b"
		if got := Line(in, 0); got != "ab" {
			t.Errorf("Line(%q) = %q, want %q", in, got, "ab")
		}
	}
	for _, in := range []string{"é", "\u65e5\u672c", "\u2248"} {
		if got := Line(in, 0); got != in {
			t.Errorf("Line(%q) = %q, want it kept", in, got)
		}
	}
}

func TestLineDropsInvalidUTF8(t *testing.T) {
	if got := Line("a\xff\xfeb", 0); got != "ab" {
		t.Errorf("got %q", got)
	}
}

func TestLineLimit(t *testing.T) {
	if got := Line("abcdefghij", 5); got != "abcd\u2026" {
		t.Errorf("got %q, want %q", got, "abcd\u2026")
	}
	if got := Line("\u65e5\u672c\u8a9e\u65e5\u672c\u8a9e\u65e5\u672c\u8a9e\u65e5", 5); got != "\u65e5\u672c\u8a9e\u65e5\u2026" {
		t.Errorf("multibyte: got %q", got)
	}
	if got := Line("abcde", 5); got != "abcde" {
		t.Errorf("exact fit: got %q", got)
	}
	if got := Line("abcdefghij", 0); got != "abcdefghij" {
		t.Errorf("limit 0: got %q", got)
	}
	if got := Line("abcdefghij", -3); got != "abcdefghij" {
		t.Errorf("negative limit: got %q", got)
	}
	if got := Line("abcdefghij", 1); got != "\u2026" {
		t.Errorf("limit 1: got %q", got)
	}
}

func TestTextKeepsParagraphs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\r\n\r\n\r\nb", "a\n\nb"},
		{"\x1b[2Jx\ny", "x\ny"},
		{"a\rb", "a\nb"},
		{"  a \t b  \n  c  ", "a b\nc"},
		{"a\n \t \n\n\nb", "a\n\nb"},
	}
	for _, c := range cases {
		if got := Text(c.in, 0); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTextLimit(t *testing.T) {
	if got := Text("ab\ncdefgh", 5); got != "ab\nc\u2026" {
		t.Errorf("got %q", got)
	}
}

func TestKeysAlphabet(t *testing.T) {
	hostile := "use $(rm -rf ~); `id` | sh > /dev/null & \"q\" 'q' !! * ? ~ # % = \\ ( ) { } [ ] < >"
	out, lossy := Keys(hostile, 0)
	if !lossy {
		t.Error("hostile text: lossy = false")
	}
	for _, r := range out {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' && !strings.ContainsRune(".,:-_/", r) {
			t.Errorf("rune %q in %q", r, out)
		}
	}
	if strings.Contains(out, "  ") || out != strings.TrimSpace(out) {
		t.Errorf("spaces not collapsed in %q", out)
	}

	plain := "Postgres, version 16.2: use it"
	out, lossy = Keys(plain, 200)
	if out != plain || lossy {
		t.Errorf("Keys(%q) = %q, %v", plain, out, lossy)
	}
}

func TestKeysLimitIsLossy(t *testing.T) {
	out, lossy := Keys(strings.Repeat("a", 300), 200)
	if n := utf8.RuneCountInString(out); n != 200 || !lossy {
		t.Errorf("got %d runes, lossy %v", n, lossy)
	}
	out, lossy = Keys(strings.Repeat("a", 200), 200)
	if len(out) != 200 || lossy {
		t.Errorf("exact fit: got %d runes, lossy %v", len(out), lossy)
	}
}

func TestKeysTrimsAfterCut(t *testing.T) {
	out, lossy := Keys("abc def", 4)
	if out != "abc" || !lossy {
		t.Errorf("got %q, %v", out, lossy)
	}
}

func TestKeysCleaningIsNotLossy(t *testing.T) {
	out, lossy := Keys("\x1b[31ma\u200bb\x1b[0m\tc\n", 0)
	if out != "ab c" || lossy {
		t.Errorf("got %q, %v", out, lossy)
	}
}

func TestKeysKeepsNonASCIILetters(t *testing.T) {
	out, lossy := Keys("café \u65e5\u672c \u0663", 0)
	if out != "café \u65e5\u672c \u0663" || lossy {
		t.Errorf("got %q, %v", out, lossy)
	}
}

var hostile = []string{
	"\x1b]0;pwned\x07title",
	"\x1b]2;title\x1b\\x",
	"\x1b[2J\x1b[Hscreen",
	"\x1b[10;20Hmove",
	"\x1b[A\x1b[B\x1b[C\x1b[D",
	"\x1b[201~injected",
	"\x1b[200~paste",
	"evil\u202etxt.exe",
	"nul\x00byte",
	"lone \x1b esc",
	"\xff\xfe",
	"\x1b[31",
	"\x1b]52;c;SGVsbG8=\x07",
	"\x1bPq#0;2;0;0;0\x1b\\",
	"\u009b2Jc1",
	"\u009d0;t\u009cx",
	"bell\x07 and \x08backspace",
	"a\u2066b\u2069c\u200bd\ufeffe",
	"\x7f\x80\x9f del and c1",
	"line1\r\nline2\rline3\n\n\n\nline4\t\ttab",
	"nb\u00a0sp\u2003em\u3000ideo\u202fnarrow",
	"a\x1b\nb\x1b\r\nc\x1b\rd",
	"tail\n\n\n \n",
	"\n\n\nhead",
	strings.Repeat("\x1b[", 1<<19),
}

func TestIdempotent(t *testing.T) {
	inputs := append([]string{"plain text", "", "a\n\n\nb", "  spaced   out  ", "abc def ghi"}, hostile...)
	for _, limit := range []int{0, 1, 5, 200} {
		for _, in := range inputs {
			if a, b := Line(in, limit), Line(Line(in, limit), limit); a != b {
				t.Errorf("Line not idempotent at limit %d for %.40q: %q then %q", limit, in, a, b)
			}
			if a, b := Text(in, limit), Text(Text(in, limit), limit); a != b {
				t.Errorf("Text not idempotent at limit %d for %.40q: %q then %q", limit, in, a, b)
			}
			a, _ := Keys(in, limit)
			if b, _ := Keys(a, limit); a != b {
				t.Errorf("Keys not idempotent at limit %d for %.40q: %q then %q", limit, in, a, b)
			}
		}
	}
}

func TestHostileCorpus(t *testing.T) {
	check := func(name, out string, newlineOK bool) {
		if !utf8.ValidString(out) {
			t.Errorf("%s: invalid UTF-8 in %.40q", name, out)
		}
		for _, r := range out {
			switch {
			case r == '\n' && newlineOK:
			case r < 0x20, r >= 0x7f && r <= 0x9f, r >= 0x202a && r <= 0x202e,
				r >= 0x2066 && r <= 0x2069, r == 0x200b, r == 0xfeff:
				t.Errorf("%s: forbidden rune %U in %.40q", name, r, out)
			}
		}
	}
	start := time.Now()
	for _, in := range hostile {
		check("Line", Line(in, 0), false)
		check("Text", Text(in, 0), true)
		k, _ := Keys(in, 0)
		check("Keys", k, false)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("corpus took %v, want under 2s", d)
	}
}

func TestHostileExpectations(t *testing.T) {
	cases := []struct{ in, line string }{
		{"\x1b]0;pwned\x07title", "title"},
		{"\x1b[201~injected", "injected"},
		{"evil\u202etxt.exe", "eviltxt.exe"},
		{"nul\x00byte", "nulbyte"},
		{"\xff\xfe", ""},
		{strings.Repeat("\x1b[", 1<<19), ""},
	}
	for _, c := range cases {
		if got := Line(c.in, 0); got != c.line {
			t.Errorf("Line(%.30q) = %q, want %q", c.in, got, c.line)
		}
	}
}

func TestSpaceSeparatorsBecomeSpaces(t *testing.T) {
	in := "a\u00a0b\u2003c\u3000d\u202fe\u00a0\u00a0 f"
	want := "a b c d e f"
	if got := Line(in, 0); got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	if got := Text(in+"\n\u3000g\u00a0", 0); got != want+"\ng" {
		t.Errorf("Text = %q, want %q", got, want+"\ng")
	}
	out, lossy := Keys(in, 0)
	if out != want || lossy {
		t.Errorf("Keys = %q, %v, want %q, false", out, lossy, want)
	}
}

func TestEscBeforeLineBreakKeepsBreak(t *testing.T) {
	for _, in := range []string{"a\x1b\nb", "a\x1b\rb", "a\x1b\r\nb"} {
		if got := Text(in, 0); got != "a\nb" {
			t.Errorf("Text(%q) = %q, want %q", in, got, "a\nb")
		}
		if got := Line(in, 0); got != "a b" {
			t.Errorf("Line(%q) = %q, want %q", in, got, "a b")
		}
	}
}

func TestTextTrailingAndLeadingBlankLines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\n\n\n", "a\n"},
		{"a\n \n\t\n", "a\n"},
		{"\n\n\na", "\na"},
		{"\n\n", ""},
	}
	for _, c := range cases {
		if got := Text(c.in, 0); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestC1StringWithoutTerminatorSwallowsRest(t *testing.T) {
	for _, intro := range []string{"\u0090", "\u0098", "\u009d", "\u009e", "\u009f"} {
		in := "keep" + intro + "payload\nmore text"
		if got := Line(in, 0); got != "keep" {
			t.Errorf("Line(%q) = %q, want %q", in, got, "keep")
		}
		if got := Text(in, 0); got != "keep" {
			t.Errorf("Text(%q) = %q, want %q", in, got, "keep")
		}
	}
}

func TestLargeInputIsLinear(t *testing.T) {
	inputs := []string{
		strings.Repeat("\x1b[", 1<<20),
		strings.Repeat("\x1b]", 1<<20),
		strings.Repeat("\x1b[31", 1<<18),
		strings.Repeat("a\u00a0\n", 1<<18),
	}
	start := time.Now()
	for _, in := range inputs {
		Line(in, 200)
		Text(in, 200)
		Keys(in, 200)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("1 MiB inputs took %v, want under 2s", d)
	}
}

// T1: NEL, U+2028 and U+2029 separate lines: Line and Keys make them
// spaces, so words either side are not glued, and Text line breaks.
func TestLineSeparatorsSeparate(t *testing.T) {
	in := "a\u2028b\u2029c\u0085d"
	if got := Line(in, 0); got != "a b c d" {
		t.Errorf("Line(%q) = %q, want %q", in, got, "a b c d")
	}
	if got, lossy := Keys(in, 0); got != "a b c d" || lossy {
		t.Errorf("Keys(%q) = %q, %v, want %q, not lossy", in, got, lossy, "a b c d")
	}
	if got := Text(in, 0); got != "a\nb\nc\nd" {
		t.Errorf("Text(%q) = %q, want %q", in, got, "a\nb\nc\nd")
	}
}

func TestBlockKeepsLayout(t *testing.T) {
	in := "a\n\n\n    indented\n\tb  c  \r\n"
	want := "a\n\n\n    indented\n\tb  c"
	if got := Block(in, 0); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBlockStripsEscapes(t *testing.T) {
	if got := Block("\x1b[31mred\x1b[0m\n\x1b]0;t\x07x", 0); got != "red\nx" {
		t.Errorf("got %q", got)
	}
	if got := Block("a\u202eb\x00c\x7fd\u0085e", 0); got != "abcde" {
		t.Errorf("got %q", got)
	}
}

func TestBlockLineEndings(t *testing.T) {
	if got := Block("a\rb\r\nc\rd", 0); got != "a\nb\nc\nd" {
		t.Errorf("got %q", got)
	}
}

func TestBlockTrimsTrailing(t *testing.T) {
	cases := map[string]string{
		"a \t\nb\t \n\n \n\t\n": "a\nb",
		"\n\n  x":               "\n\n  x",
		" \t\n \n":              "",
		"":                      "",
	}
	for in, want := range cases {
		if got := Block(in, 0); got != want {
			t.Errorf("Block(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBlockLimitIsBytes(t *testing.T) {
	if got := Block(strings.Repeat("é", 10), 5); got != "éé" {
		t.Errorf("got %q", got)
	}
	if got := Block("abcdef", 3); got != "abc" {
		t.Errorf("got %q", got)
	}
	// A cut that leaves trailing blanks is trimmed again.
	if got := Block("ab  \n\n cd", 5); got != "ab" {
		t.Errorf("got %q", got)
	}
}

func TestBlockIdempotent(t *testing.T) {
	inputs := append([]string{"plain", "", "a\n\n\nb", "  x  \n\n", "é é é é é é"}, hostile...)
	for _, limit := range []int{0, 1, 3, 5, 200} {
		for _, in := range inputs {
			a := Block(in, limit)
			if b := Block(a, limit); a != b {
				t.Errorf("Block not idempotent at limit %d for %.40q: %q then %q", limit, in, a, b)
			}
		}
	}
}

func TestBlockInHostileCorpus(t *testing.T) {
	for _, in := range hostile {
		out := Block(in, 0)
		if !utf8.ValidString(out) {
			t.Errorf("invalid UTF-8 in %q", out)
		}
		for _, r := range out {
			switch {
			case r == '\n' || r == '\t':
			case r < 0x20, r >= 0x7f && r <= 0x9f, r >= 0x202a && r <= 0x202e:
				t.Errorf("forbidden rune %U in %.40q", r, out)
			}
		}
	}
}

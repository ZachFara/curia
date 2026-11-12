// Package sanitize is the one place untrusted text is cleaned before it is
// stored, drawn or typed into a terminal. Every function first removes whole
// escape sequences and invalid UTF-8, and runs in linear time (no regular
// expressions), so a 1 MiB input of nothing but "ESC [" costs one pass.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	esc      = '\x1b'
	bel      = '\a'
	ellipsis = "…"

	// keysPunct is the punctuation Keys keeps besides letters, digits and
	// the space.
	keysPunct = ".,:-_/"
)

// Line makes s one line of printable text: escape sequences are removed,
// line breaks and tabs become spaces, runes that are not printable are
// dropped, runs of spaces collapse and the ends are trimmed. A positive
// limit cuts the result to that many runes, the last one an ellipsis;
// callers pass a positive limit for untrusted text, since a limit that is
// not positive leaves the length unbounded.
func Line(s string, limit int) string {
	out, _ := cut(oneLine(scan(s)), limit, ellipsis)
	return out
}

// Text is Line for several lines: "\r\n" and "\r" become "\n", each line is
// cleaned as Line cleans one, and at most one blank line is kept in a row.
// A positive limit cuts the result to that many runes, the last one an
// ellipsis; callers pass a positive limit for untrusted text, since a limit
// that is not positive leaves the length unbounded.
func Text(s string, limit int) string {
	s = strings.ReplaceAll(scan(s), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blanks := 0
	for _, l := range lines {
		l = oneLine(l)
		if l == "" {
			blanks++
			if blanks > 1 {
				continue
			}
		} else {
			blanks = 0
		}
		out = append(out, l)
	}
	res, _ := cut(strings.Join(out, "\n"), limit, ellipsis)
	return res
}

// Keys is the strictest cleaner, for text that is typed into a terminal. It
// cleans as Line does, then keeps only letters, digits, spaces and the
// punctuation ". , : - _ /", and cuts the result to limit runes (no cut when
// limit is not positive) with no ellipsis. lossy reports that the alphabet
// filter dropped a rune or the limit cut the text.
func Keys(s string, limit int) (out string, lossy bool) {
	line := oneLine(scan(s))
	kept := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune(keysPunct, r) {
			return r
		}
		return -1
	}, line)
	lossy = len(kept) != len(line)
	out, cutDone := cut(squeeze(kept), limit, "")
	return squeeze(out), lossy || cutDone
}

// squeeze collapses runs of spaces to one and trims the ends.
func squeeze(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// oneLine turns line breaks, tabs and Unicode space separators (category Zs)
// into spaces, drops every rune that is not printable, then squeezes the spaces. The input has been through scan.
func oneLine(s string) string {
	return squeeze(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.Is(unicode.Zs, r):
			return ' '
		case unicode.IsPrint(r): // includes the ASCII space
			return r
		default:
			return -1
		}
	}, s))
}

// cut shortens s to limit runes, the last of them end when end is not empty.
// It reports whether it cut. A limit that is not positive never cuts.
func cut(s string, limit int, end string) (string, bool) {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	keep := limit - utf8.RuneCountInString(end)
	n := 0
	for i := range s {
		if n == keep {
			return s[:i] + end, true
		}
		n++
	}
	return s, false // unreachable: s has more than limit runes
}

// scan returns s without invalid UTF-8 and without escape sequences: CSI,
// OSC, DCS, SOS, PM and APC, any other two-byte ESC sequence, and the C1
// forms of the introducers. An unterminated sequence runs to the end.
func scan(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == utf8.RuneError && n == 1:
		case r == esc:
			i += escLen(s[i:])
		case r == 0x9b:
			i += csiLen(s[i:])
		case r == 0x9d:
			i += stringLen(s[i:], true)
		case r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f:
			i += stringLen(s[i:], false)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escLen is the length in bytes of what follows an ESC that belongs to the
// sequence. An ESC followed by another ESC is dropped alone, so the second
// one introduces its own sequence; an ESC followed by a line break is
// dropped alone too, so the break survives.
func escLen(rest string) int {
	r, n := utf8.DecodeRuneInString(rest)
	switch r {
	case utf8.RuneError:
		return n // end of input (0) or one invalid byte (1)
	case esc, '\n', '\r':
		return 0
	case '[':
		return n + csiLen(rest[n:])
	case ']':
		return n + stringLen(rest[n:], true)
	case 'P', 'X', '^', '_':
		return n + stringLen(rest[n:], false)
	default:
		return n
	}
}

// csiLen is the length of a CSI sequence after its introducer: up to and
// including the final byte 0x40 to 0x7E. An ESC ends it early, unconsumed,
// because a terminal starts a new sequence there.
func csiLen(rest string) int {
	for i, r := range rest {
		switch {
		case r == esc:
			return i
		case r >= 0x40 && r <= 0x7e:
			return i + 1
		}
	}
	return len(rest)
}

// stringLen is the length of an OSC, DCS, SOS, PM or APC body after its
// introducer: up to and including ESC \ (or BEL too when bel is set).
func stringLen(rest string, belEnds bool) int {
	for i, r := range rest {
		switch {
		case belEnds && r == bel:
			return i + 1
		case r == esc && strings.HasPrefix(rest[i+1:], "\\"):
			return i + 2
		}
	}
	return len(rest)
}

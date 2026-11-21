// Package export turns a stored decision into text for other tools: a
// Markdown note for an Obsidian vault and an Agent Commons entry. It is pure:
// it writes no file, opens no connection and starts no process.
//
// Everything an agent or the user wrote is untrusted and ends up in an
// application that executes some constructs (embeds, plugin code blocks,
// template tags, HTML, front matter). So all such text sits inside a plain
// code fence that the text cannot close, and the front matter holds only
// fixed keys whose values are filtered to a safe alphabet or JSON-quoted.
package export

import (
	"encoding/json"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Limits of a note.
const (
	MaxNote    = 64 << 10 // bytes of one note
	MaxTitle   = 60       // runes of a note's title
	MaxContext = 8 << 10  // bytes of the context section
)

const (
	maxTitleBytes = 180 // keeps "<id> <title>.md" well under a file system's 255 bytes
	maxIDRunes    = 40
	maxOptions    = 20
	maxTags       = 20

	maxQuestion  = 4 << 10
	maxOptionTxt = 2 << 10
	maxAnswer    = 8 << 10
	maxReasoning = 16 << 10
)

// NoteTitle is the question as a plain file-name-safe title; "" when nothing is left.
func NoteTitle(question string) string {
	ln := line(question, MaxTitle)
	var b strings.Builder
	space := true // squeezes runs and trims the start
	for _, r := range ln {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_(),'", r) {
			b.WriteRune(r)
			space = false
		} else if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	title := strings.TrimRight(b.String(), " ")
	for len(title) > maxTitleBytes {
		_, n := utf8.DecodeLastRuneInString(title)
		title = strings.TrimRight(title[:len(title)-n], " ")
	}
	return title
}

// safeToken keeps only ASCII letters, digits, "-" and "_", at most max runes.
func safeToken(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			break
		}
		if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fileID is the id as it appears in a file name and the front matter.
func fileID(id string) string {
	if t := safeToken(id, maxIDRunes); t != "" {
		return t
	}
	return "decision"
}

// NoteName is the file name for d: "<id> <title>.md", or "<id>.md".
func NoteName(d model.Decision) string {
	id := fileID(d.ID)
	if title := NoteTitle(d.Question); title != "" {
		return id + " " + title + ".md"
	}
	return id + ".md"
}

// IsNoteOf reports whether a file name is a note of decision id: "<id>.md", or "<id> ….md".
func IsNoteOf(name, id string) bool {
	if id == "" {
		return false
	}
	if name == id+".md" {
		return true
	}
	return len(name) >= len(id)+1+len(".md") && strings.HasPrefix(name, id+" ") && strings.HasSuffix(name, ".md")
}

// OwnsNote reports whether head, the first bytes of a file, holds the front-matter line "curia_id: <id>" within its first 20 lines.
func OwnsNote(head []byte, id string) bool {
	lines := strings.SplitN(string(head), "\n", 21)
	if len(lines) > 20 {
		lines = lines[:20]
	}
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return false
	}
	for _, l := range lines[1:] {
		l = strings.TrimSuffix(l, "\r")
		if l == "---" {
			return false
		}
		if l == "curia_id: "+id {
			return true
		}
	}
	return false
}

// Fence wraps text in a code fence that text cannot close: a run of backticks one longer than the longest in text, at least three, no info string.
// The text is cleaned first (escape sequences, control characters and every
// kind of line break), because a carriage return would end a line for the
// reader and let "```" after it close the fence.
func Fence(text string) string {
	text = sanitize.Block(text, 0)
	longest, run := 0, 0
	for i := 0; i < len(text); i++ {
		if text[i] == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + "\n" + text + "\n" + f
}

// block cleans text and cuts it to limit bytes; a limit that is not positive leaves nothing.
func block(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	// Cleaning only removes bytes, so a few times the limit is enough input
	// and a huge text costs no more than a small one.
	if len(s) > 4*limit {
		s = s[:4*limit]
	}
	return sanitize.Block(s, limit)
}

// quote is s as a JSON string, which YAML reads as a double-quoted string.
// The default JSON escaping also hides "<", ">" and "&".
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil { // a string always marshals; the fallback keeps the front matter valid
		return `""`
	}
	return string(b)
}

func baseName(repo string) string {
	repo = line(repo, 4096)
	if repo == "" {
		return ""
	}
	b := path.Base(strings.ReplaceAll(repo, "\\", "/"))
	if b == "." || b == "/" {
		return ""
	}
	return line(b, 100)
}

func tagList(tags []string, runes int) []string {
	out := []string{}
	for _, t := range tags {
		if len(out) == maxTags {
			break
		}
		if t = line(t, runes); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func tokenOr(s, fallback string) string {
	if t := safeToken(s, 20); t != "" {
		return t
	}
	return fallback
}

// answerText is the chosen option's key and label, or the free text.
func answerText(d model.Decision) string {
	if d.AnswerKey == "" {
		return d.Answer
	}
	for _, o := range d.Options {
		if o.Key == d.AnswerKey {
			return d.AnswerKey + ": " + o.Label
		}
	}
	return d.AnswerKey
}

// Note is the whole note for a decided decision.
func Note(d model.Decision) []byte {
	for scale := 1; ; scale *= 2 {
		n := buildNote(d, scale)
		if len(n) <= MaxNote || scale >= 1<<20 {
			return n
		}
	}
}

// header is the fixed front matter, with its closing line.
func header(d model.Decision) string {
	status := tokenOr(string(d.Status), "unknown")
	if !d.SupersededAt.IsZero() {
		status = "superseded"
	}
	var b strings.Builder
	b.WriteString("---\ncuria_id: " + fileID(d.ID) + "\n")
	b.WriteString("status: " + status + "\n")
	b.WriteString("scope: " + tokenOr(string(d.Scope), "unknown") + "\n")
	b.WriteString("mode: " + tokenOr(string(d.Mode), "unknown") + "\n")
	if r := baseName(d.Repo); r != "" {
		b.WriteString("repo: " + quote(r) + "\n")
	}
	if t := line(d.TaskID, 60); t != "" {
		b.WriteString("task: " + quote(t) + "\n")
	}
	if !d.AnsweredAt.IsZero() {
		b.WriteString("date: " + d.AnsweredAt.UTC().Format(time.DateOnly) + "\n")
	}
	tags := tagList(d.Tags, 40)
	for i, t := range tags {
		tags[i] = quote(t)
	}
	b.WriteString("tags: [" + strings.Join(tags, ", ") + "]\n---\n")
	return b.String()
}

// optionsSection lists the options, each with its fenced label and tradeoffs.
func optionsSection(d model.Decision, scale int) string {
	var b strings.Builder
	b.WriteString("## Options\n")
	for i, o := range d.Options {
		if i == maxOptions {
			break
		}
		key := safeToken(o.Key, 16)
		if key == "" {
			key = "?"
		}
		b.WriteString("- " + key)
		if d.Recommendation != "" && o.Key == d.Recommendation {
			b.WriteString(" (recommended)")
		}
		if d.AnswerKey != "" && o.Key == d.AnswerKey {
			b.WriteString(" (chosen)")
		}
		b.WriteString("\n")
		text := o.Label
		if o.Tradeoffs != "" {
			text += ": " + o.Tradeoffs
		}
		if text = block(text, maxOptionTxt/scale); text != "" {
			b.WriteString(Fence(text) + "\n")
		}
	}
	return b.String()
}

// buildNote builds the note with every free-text cap divided by scale.
func buildNote(d model.Decision, scale int) []byte {
	var b strings.Builder
	b.WriteString(header(d))
	heading := "# " + fileID(d.ID)
	if title := NoteTitle(d.Question); title != "" {
		heading += " " + title
	}
	b.WriteString(heading + "\n")

	section := func(name, text string, limit int) {
		if text = block(text, limit/scale); text != "" {
			b.WriteString("## " + name + "\n" + Fence(text) + "\n")
		}
	}
	section("Question", d.Question, maxQuestion)
	section("Context", d.Context, MaxContext)
	if len(d.Options) > 0 {
		b.WriteString(optionsSection(d, scale))
	}
	section("Answer", answerText(d), maxAnswer)
	section("Reasoning", d.AnswerReasoning, maxReasoning)
	return []byte(b.String())
}

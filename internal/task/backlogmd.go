// Package task holds Curia's task model. This file reads Backlog.md task
// files: untrusted input, bounded and cleaned before anything is stored.
package task

import (
	"bufio"
	"bytes"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ZachFara/curia/internal/sanitize"
)

// MaxBacklogFile is the largest Backlog.md file ParseBacklogMD reads.
const MaxBacklogFile = 256 << 10

const (
	maxTitleRunes = 120
	maxBodyBytes  = 48 << 10
	maxRefRunes   = 64
	maxTags       = 8
	maxTagRunes   = 32
	fence         = "---"
	doneStatus    = "done"
)

// The front-matter key names the reader looks at. Every other key is ignored.
const (
	keyID     = "id"
	keyTitle  = "title"
	keyStatus = "status"
	keyLabels = "labels"
)

// ErrNotATask reports a file that is not a Backlog.md task file.
var ErrNotATask = errors.New("task: not a Backlog.md task file")

// Imported is one Backlog.md task file, mapped to Curia's fields.
type Imported struct {
	Ref, Title, Body string   // Ref is the file's id field, else its base name
	Tags             []string // from labels
	Done             bool     // the file's status is "Done", ignoring case
}

var (
	tagPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	tagBad        = regexp.MustCompile(`[^a-z0-9_-]+`)
	fileNameTitle = regexp.MustCompile(`^task-[0-9]+ - `)
)

// ParseBacklogMD maps one Backlog.md task file to Imported. The front matter
// is read as flat "key: value" lines only; no YAML library, and anything not
// understood is ignored. name is the file's name, used as a fallback.
func ParseBacklogMD(name string, data []byte) (Imported, error) {
	if len(data) > MaxBacklogFile {
		return Imported{}, ErrNotATask
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), MaxBacklogFile+1)
	if !sc.Scan() || strings.TrimRight(sc.Text(), " \t\r") != fence {
		return Imported{}, ErrNotATask
	}
	var front []string
	closed := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimRight(line, " \t") == fence {
			closed = true
			break
		}
		front = append(front, line)
	}
	if !closed {
		return Imported{}, ErrNotATask
	}
	var body strings.Builder
	for sc.Scan() {
		body.WriteString(sc.Text())
		body.WriteByte('\n')
	}
	if sc.Err() != nil {
		return Imported{}, ErrNotATask
	}

	fields := readFront(front)
	base := filepath.Base(name)
	imp := Imported{
		Ref:   sanitize.Line(fields.scalars[keyID], maxRefRunes),
		Title: sanitize.Line(fields.scalars[keyTitle], maxTitleRunes),
		Body:  sanitize.Block(body.String(), maxBodyBytes),
		Tags:  cleanTags(fields.labels),
		Done:  strings.EqualFold(sanitize.Line(fields.scalars[keyStatus], maxRefRunes), doneStatus),
	}
	if imp.Ref == "" {
		imp.Ref = sanitize.Line(base, maxRefRunes)
	}
	if imp.Title == "" {
		stem := strings.TrimSuffix(base, ".md")
		stem = fileNameTitle.ReplaceAllString(stem, "")
		if !strings.Contains(stem, " ") { // Backlog.md's dashed slug
			stem = strings.ReplaceAll(stem, "-", " ")
		}
		imp.Title = sanitize.Line(stem, maxTitleRunes)
	}
	if imp.Title == "" {
		return Imported{}, ErrNotATask
	}
	return imp, nil
}

type front struct {
	scalars map[string]string
	labels  []string
}

// readFront reads flat top-level "key: value" lines. Indented lines belong to
// the previous key and are used only as "- item" entries of labels.
func readFront(lines []string) front {
	f := front{scalars: map[string]string{}}
	inLabels := false
	for _, line := range lines {
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' || line[0] == '-' {
			if item, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok && inLabels {
				f.labels = append(f.labels, unquote(item))
			}
			continue
		}
		inLabels = false
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch {
		case key == keyLabels && val == "":
			inLabels = true
		case key == keyLabels:
			f.labels = append(f.labels, inlineList(val)...)
		case !understood(val):
			continue
		default:
			f.scalars[key] = unquote(val)
		}
	}
	return f
}

// understood is false for values the flat reader cannot take: anchors,
// aliases, tags, and block scalar or flow collection openers.
func understood(val string) bool {
	if val == "" {
		return true
	}
	return !strings.ContainsRune("&*!|>", rune(val[0]))
}

func inlineList(val string) []string {
	inner, ok := strings.CutPrefix(val, "[")
	if !ok {
		return nil
	}
	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return nil
	}
	var out []string
	for _, p := range strings.Split(inner, ",") {
		out = append(out, unquote(strings.TrimSpace(p)))
	}
	return out
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		inner := s[1 : len(s)-1]
		if s[0] == '\'' { // YAML single quotes double an embedded quote
			inner = strings.ReplaceAll(inner, "''", "'")
		}
		return inner
	}
	return s
}

func cleanTags(labels []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range labels {
		t := strings.Trim(tagBad.ReplaceAllString(strings.ToLower(sanitize.Line(l, maxTagRunes*2)), "-"), "-")
		if len(t) > maxTagRunes || !tagPattern.MatchString(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == maxTags {
			break
		}
	}
	return out
}

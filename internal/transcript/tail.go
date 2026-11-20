package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZachFara/curia/internal/sanitize"
)

const (
	// tailBytes bounds what Tail reads: the last 256 KiB, never the whole file.
	tailBytes = 256 << 10
	// tailLineRunes cuts each displayed line.
	tailLineRunes = 300
)

// Tail returns up to n display lines from the end of the transcript at path:
// an assistant's text (one display line per text line), "tool: <name>" for
// each tool call, and the first line of each tool result, indented. Every
// line is cleaned by sanitize.Line. It reads only the last tailBytes: a
// line that starts before them (or one longer than them) is skipped, and
// lines that are not complete JSON (still being written) or not
// conversation are ignored. A missing file or one that is not a regular file
// (a FIFO is never opened) yields nil and no error.
func Tail(path string, n int) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	path = filepath.Clean(path)
	if pre, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) || (err == nil && !pre.Mode().IsRegular()) {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, wrap("stat transcript", err)
	}
	start := max(info.Size()-tailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return nil, wrap("read transcript", err)
	}
	if start > 0 { // the first line is cut: drop it
		_, data, _ = bytes.Cut(data, []byte("\n"))
	}
	var out []string
	for _, l := range bytes.Split(data, []byte("\n")) {
		out = append(out, tailLines(l)...)
	}
	return out[max(len(out)-n, 0):], nil
}

// tailLines is the display lines of one transcript line.
func tailLines(data []byte) []string {
	var l line
	if len(data) == 0 || json.Unmarshal(data, &l) != nil || (l.Type != "assistant" && l.Type != "user") {
		return nil
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil
	}
	var out []string
	add := func(prefix, s string) {
		if s = sanitize.Line(s, tailLineRunes); s != "" {
			out = append(out, prefix+s)
		}
	}
	for _, b := range blocks {
		switch {
		case b.Type == "text" && l.Type == "assistant":
			for _, tl := range strings.Split(sanitize.Text(b.Text, 0), "\n") {
				add("", tl)
			}
		case b.Type == "tool_use" && l.Type == "assistant":
			add("tool: ", b.Name)
		case b.Type == "tool_result":
			add("  ", FirstLine(resultText(b.Content)))
		}
	}
	return out
}

// resultText is a tool result's text: a string, or the text blocks of an array.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct{ Text string }
	_ = json.Unmarshal(raw, &blocks)
	for _, b := range blocks {
		if b.Text != "" {
			return b.Text
		}
	}
	return ""
}

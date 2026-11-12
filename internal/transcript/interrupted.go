package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// interruptPrefix starts the text of the user line Claude Code writes when
// the user denies a permission prompt or interrupts a turn:
// "[Request interrupted by user]" or "[Request interrupted by user for tool use]".
const interruptPrefix = "[Request interrupted by user"

// Interrupted reports whether the last conversational (user or assistant)
// line after offset is a user-interrupt marker: a user line, not a
// subagent's or a meta line, whose content is a string, or an array of
// exactly one text block, starting with "[Request interrupted by user".
// Lines of other types are skipped. It reads at most the tail window Read
// does.
//
// It errs towards false, because a session it reports may be typed into: a
// missing file or empty path, an offset the file cannot have (negative or
// past its end), no conversational line in the window, and a line it cannot
// classify after the marker (invalid JSON, too long, or still being
// written) all yield false.
func Interrupted(path string, offset int64) (bool, error) {
	if path == "" {
		return false, nil
	}
	f, size, err := openTranscript(path)
	if f == nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	if offset < 0 || offset > size {
		return false, nil
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var buf []byte
	if _, ok, err := position(f, r, size, offset, &buf); err != nil || !ok {
		return false, err
	}
	marker := false
	for {
		data, n, complete, err := readLine(r, &buf)
		if err != nil {
			return false, wrap("read transcript", err)
		}
		if !complete {
			// A line still being written may be the next turn.
			return marker && n == 0, nil
		}
		switch classify(data) {
		case lineOther:
		case lineMarker:
			marker = true
		default:
			marker = false
		}
	}
}

type lineKind int

const (
	lineOther          lineKind = iota // not user or assistant, or blank: skipped
	lineMarker                         // a user-interrupt marker
	lineConversational                 // any other user or assistant line, or one not understood
)

// entry is what classify reads of a line. A field of the wrong JSON type
// fails the whole line, which then counts as not understood.
type entry struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Message     *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// classify sorts one complete line. A nil line (longer than maxLineBytes)
// and a line that is not a JSON object are not understood, so they count
// as conversational.
func classify(data []byte) lineKind {
	if data != nil && len(bytes.TrimSpace(data)) == 0 {
		return lineOther
	}
	var e entry
	if data == nil || json.Unmarshal(data, &e) != nil {
		return lineConversational
	}
	switch {
	case e.Type != "user" && e.Type != "assistant":
		return lineOther
	case e.Type == "user" && !e.IsSidechain && !e.IsMeta && e.Message != nil && isMarker(e.Message.Content):
		return lineMarker
	default:
		return lineConversational
	}
}

// isMarker reports whether a user line's content is a string, or an array
// of exactly one text block, that starts with interruptPrefix.
func isMarker(content json.RawMessage) bool {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return strings.HasPrefix(s, interruptPrefix)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil || len(blocks) != 1 {
		return false
	}
	return blocks[0].Type == "text" && strings.HasPrefix(blocks[0].Text, interruptPrefix)
}

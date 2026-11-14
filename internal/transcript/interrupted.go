package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The texts of the user line Claude Code writes when the user interrupts a
// turn while Claude writes text, and when the user denies a permission
// prompt or interrupts a tool.
const (
	interruptMarker        = "[Request interrupted by user]"
	interruptToolUseMarker = "[Request interrupted by user for tool use]"
)

// Interrupted reports whether the last conversational (user or assistant)
// line after offset is a user-interrupt marker, and if so the marker's own
// timestamp, the time Claude Code created the entry (Ruling 79: the caller
// compares it with what it knows of the session). A marker is a user line,
// not a subagent's or a meta line, whose content is an array of exactly
// one text block whose text is exactly "[Request interrupted by user]" or
// "[Request interrupted by user for tool use]" (Ruling 80: string content
// is how a typed prompt is stored, so it is never a marker), and whose
// "timestamp" is an RFC 3339 time. Lines of other types are skipped. It
// reads at most the tail window Read does, and only the bytes the file had
// when it was opened.
//
// It errs towards not found, because a session it reports may be typed
// into: a missing file or empty path, a path that is not a regular file
// (a directory, device or FIFO, which is never opened), an offset the file
// cannot have (negative or past its end), no conversational line in the
// window, a marker without a valid timestamp, and a line it cannot classify
// after the marker (invalid JSON, too long, or still being written) all
// yield found false with no error.
func Interrupted(path string, offset int64) (at time.Time, found bool, err error) {
	if path == "" {
		return time.Time{}, false, nil
	}
	// Stat before opening: opening a FIFO blocks until it has a writer.
	if pre, serr := os.Stat(filepath.Clean(path)); errors.Is(serr, fs.ErrNotExist) || (serr == nil && !pre.Mode().IsRegular()) {
		return time.Time{}, false, nil
	}
	f, info, err := openTranscript(path)
	if f == nil {
		return time.Time{}, false, err
	}
	defer func() { _ = f.Close() }()
	size := info.Size()
	// Checked again on what was opened, in case the path was replaced.
	if !info.Mode().IsRegular() || offset < 0 || offset > size {
		return time.Time{}, false, nil
	}
	src := bounded(f, size)
	r := bufio.NewReaderSize(src, 64<<10)
	var buf []byte
	if _, ok, err := position(src, r, size, offset, &buf); err != nil || !ok {
		return time.Time{}, false, err
	}
	return lastMarker(r, &buf)
}

// lastMarker reads the remaining lines of r and returns the timestamp of
// the marker that is the last conversational line, if there is one.
func lastMarker(r *bufio.Reader, buf *[]byte) (time.Time, bool, error) {
	var at time.Time
	marker := false
	for {
		data, n, complete, err := readLine(r, buf)
		if err != nil {
			return time.Time{}, false, wrap("read transcript", err)
		}
		if !complete {
			// A line still being written may be the next turn.
			if !marker || n != 0 {
				return time.Time{}, false, nil
			}
			return at, true, nil
		}
		switch kind, t := classify(data); kind {
		case lineOther:
		case lineMarker:
			at, marker = t, true
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
	Timestamp   string `json:"timestamp"`
	Message     *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// classify sorts one complete line and returns a marker's timestamp. A nil
// line (longer than maxLineBytes), a line that is not a JSON object, and a
// marker whose timestamp is missing or not RFC 3339 are not understood, so
// they count as conversational.
func classify(data []byte) (lineKind, time.Time) {
	if data != nil && len(bytes.TrimSpace(data)) == 0 {
		return lineOther, time.Time{}
	}
	var e entry
	if data == nil || json.Unmarshal(data, &e) != nil {
		return lineConversational, time.Time{}
	}
	switch {
	case e.Type != "user" && e.Type != "assistant":
		return lineOther, time.Time{}
	case e.Type == "user" && !e.IsSidechain && !e.IsMeta && e.Message != nil && isMarker(e.Message.Content):
		at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
		if err != nil {
			return lineConversational, time.Time{}
		}
		return lineMarker, at
	default:
		return lineConversational, time.Time{}
	}
}

// isMarker reports whether a user line's content is an array of exactly
// one text block whose text is exactly one of the two interrupt markers.
// Any other content, a string included, is not a marker.
func isMarker(content json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil || len(blocks) != 1 || blocks[0].Type != "text" {
		return false
	}
	return blocks[0].Text == interruptMarker || blocks[0].Text == interruptToolUseMarker
}

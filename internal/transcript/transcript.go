// Package transcript incrementally reads Claude Code JSONL transcripts.
//
// Transcript content is untrusted: lines may be huge, malformed or hostile.
// Read never panics on bad input; it skips what it cannot use.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxLineBytes caps how much of one line is held in memory. Longer lines
	// are consumed but skipped.
	maxLineBytes = 8 << 20
	// maxTokens clamps each usage counter so hostile values cannot overflow
	// the sum or go negative.
	maxTokens = 1 << 40
	// syntheticModel marks placeholder assistant lines Claude Code writes
	// locally; they carry no real usage.
	syntheticModel = "<synthetic>"
)

// Update is what the new bytes of a transcript say about the session.
type Update struct {
	Model             string
	ContextUsed       int
	LastAssistantText string
	Found             bool
}

type line struct {
	Type    string `json:"type"`
	Message struct {
		Model   string          `json:"model"`
		Usage   *usage          `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type usage struct {
	Input         int64 `json:"input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
}

// Read scans the transcript at path from offset and returns the update from
// the last usable assistant line plus the offset just past the last complete
// line read. A missing file yields an empty Update and the same offset. If the
// file is shorter than offset (or offset is negative) it is read from 0.
func Read(path string, offset int64) (Update, int64, error) {
	f, err := os.Open(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return Update{}, offset, nil
	}
	if err != nil {
		return Update{}, offset, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return Update{}, offset, fmt.Errorf("stat transcript: %w", err)
	}
	if offset < 0 || offset > info.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return Update{}, offset, fmt.Errorf("seek transcript: %w", err)
	}

	var upd Update
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		data, n, complete, err := readLine(r)
		if err != nil {
			return upd, offset, fmt.Errorf("read transcript: %w", err)
		}
		if !complete {
			return upd, offset, nil
		}
		offset += n
		if data != nil {
			if u, ok := parseLine(data); ok {
				upd = u
			}
		}
	}
}

// readLine reads one line. It returns the line bytes without the newline (nil
// if the line exceeded maxLineBytes), the number of bytes consumed, and whether
// the line was newline-terminated. At EOF it returns complete == false.
func readLine(r *bufio.Reader) (data []byte, n int64, complete bool, err error) {
	var buf []byte
	tooLong := false
	for {
		chunk, rerr := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooLong {
			if len(buf)+len(chunk) > maxLineBytes {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case rerr == nil:
			if tooLong {
				return nil, n, true, nil
			}
			return buf[:len(buf)-1], n, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			return nil, n, false, nil
		default:
			return nil, n, false, rerr
		}
	}
}

func parseLine(data []byte) (Update, bool) {
	if !bytes.Contains(data, []byte(`"assistant"`)) {
		return Update{}, false
	}
	var l line
	if json.Unmarshal(data, &l) != nil {
		return Update{}, false
	}
	if l.Type != "assistant" || l.Message.Usage == nil || l.Message.Model == syntheticModel {
		return Update{}, false
	}
	u := l.Message.Usage
	return Update{
		Model:             l.Message.Model,
		ContextUsed:       int(clamp(u.Input) + clamp(u.CacheRead) + clamp(u.CacheCreation)),
		LastAssistantText: lastTextFirstLine(l.Message.Content),
		Found:             true,
	}, true
}

func clamp(v int64) int64 {
	return max(0, min(v, maxTokens))
}

// lastTextFirstLine returns the first line of the last text block in a content
// array. Content that is not an array of objects yields "".
func lastTextFirstLine(content json.RawMessage) string {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	text := ""
	for _, raw := range blocks {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &b) == nil && b.Type == "text" {
			text = b.Text
		}
	}
	first, _, _ := strings.Cut(text, "\n")
	return strings.TrimRight(first, "\r")
}

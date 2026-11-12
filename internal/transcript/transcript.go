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
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	// maxLineBytes caps how much of one line is held in memory. Longer lines
	// are consumed but skipped.
	maxLineBytes = 8 << 20
	// maxTokens clamps each usage counter and the total so hostile values
	// cannot overflow (even a 32-bit int) or go negative.
	maxTokens = math.MaxInt32
	// tailWindow bounds how much of a large unread span is parsed: reading
	// starts at the first complete line within the last tailWindow bytes.
	tailWindow = 1 << 20
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
// file is shorter than offset (or offset is negative) it is read from 0. If
// more than tailWindow bytes are unread, reading starts at the first complete
// line inside the last tailWindow bytes, so only the latest state is parsed.
//
// Model and ContextUsed come from the last counted assistant line;
// LastAssistantText is the first line of the last non-empty text block among
// all counted assistant lines read (Claude Code writes one line per content
// block, so the final line may hold only a tool_use).
func Read(path string, offset int64) (Update, int64, error) {
	f, size, err := openTranscript(path)
	if f == nil {
		return Update{}, offset, err
	}
	defer func() { _ = f.Close() }()
	if offset < 0 || offset > size {
		offset = 0
	}
	r := bufio.NewReaderSize(f, 64<<10)
	var buf []byte
	start, ok, err := position(f, r, size, offset, &buf)
	if err != nil || !ok {
		return Update{}, offset, err
	}
	return scan(r, &buf, start)
}

// openTranscript opens the transcript at path and returns it with its size.
// A missing file yields a nil file and no error; on any error the file is
// nil too.
func openTranscript(path string) (*os.File, int64, error) {
	f, err := os.Open(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open transcript: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat transcript: %w", err)
	}
	return f, info.Size(), nil
}

// position seeks f to the first line to parse. Normally that is offset; if
// more than tailWindow bytes are unread it is the first complete line inside
// the last tailWindow bytes. ok is false if that window holds no complete line.
func position(f *os.File, r *bufio.Reader, size, offset int64, buf *[]byte) (start int64, ok bool, err error) {
	if size-offset <= tailWindow {
		_, err = f.Seek(offset, io.SeekStart)
		return offset, err == nil, wrap("seek transcript", err)
	}
	// Seek one byte early so a start that is already on a line boundary is
	// detected: the skipped "line" is then just the previous newline.
	from := size - tailWindow - 1
	if _, err = f.Seek(from, io.SeekStart); err != nil {
		return offset, false, wrap("seek transcript", err)
	}
	_, n, complete, err := readLine(r, buf)
	if err != nil {
		return offset, false, wrap("read transcript", err)
	}
	return from + n, complete, nil
}

// scan reads whole lines from r, starting at offset, and builds the Update.
func scan(r *bufio.Reader, buf *[]byte, offset int64) (Update, int64, error) {
	var upd Update
	text := ""
	for {
		data, n, complete, err := readLine(r, buf)
		if err != nil {
			return upd, offset, fmt.Errorf("read transcript: %w", err)
		}
		if !complete {
			upd.LastAssistantText = text
			return upd, offset, nil
		}
		offset += n
		if data == nil {
			continue
		}
		if u, t, ok := parseLine(data); ok {
			upd = u
			if t != nil {
				text = *t
			}
		}
	}
}

// readLine reads one line into *buf (reused across calls). It returns the line
// without its newline (nil if it exceeded maxLineBytes; valid until the next
// call), the number of bytes consumed, and whether the line was
// newline-terminated. At EOF it returns complete == false.
func readLine(r *bufio.Reader, buf *[]byte) (data []byte, n int64, complete bool, err error) {
	*buf = (*buf)[:0]
	tooLong := false
	for {
		chunk, rerr := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !tooLong {
			if len(*buf)+len(chunk) > maxLineBytes {
				tooLong = true
			} else {
				*buf = append(*buf, chunk...)
			}
		}
		switch {
		case rerr == nil:
			if tooLong {
				return nil, n, true, nil
			}
			return (*buf)[:len(*buf)-1], n, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			continue
		case errors.Is(rerr, io.EOF):
			return nil, n, false, nil
		default:
			return nil, n, false, rerr
		}
	}
}

// parseLine returns the counted update for one line and, when the line holds a
// non-empty text block, the first line of the last such block.
func parseLine(data []byte) (Update, *string, bool) {
	if !bytes.Contains(data, []byte(`"assistant"`)) {
		return Update{}, nil, false
	}
	var l line
	if json.Unmarshal(data, &l) != nil {
		return Update{}, nil, false
	}
	if l.Type != "assistant" || l.Message.Usage == nil || l.Message.Model == syntheticModel {
		return Update{}, nil, false
	}
	u := l.Message.Usage
	total := clamp(clamp(u.Input) + clamp(u.CacheRead) + clamp(u.CacheCreation))
	return Update{
		Model:       l.Message.Model,
		ContextUsed: int(total),
		Found:       true,
	}, lastTextFirstLine(l.Message.Content), true
}

func clamp(v int64) int64 {
	return max(0, min(v, maxTokens))
}

// lastTextFirstLine returns the first line of the last non-empty text block in
// a content array, or nil if there is none. Content that is not an array of
// objects yields nil.
func lastTextFirstLine(content json.RawMessage) *string {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var text *string
	for _, raw := range blocks {
		var b struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &b) == nil && b.Type == "text" && b.Text != "" {
			text = &b.Text
		}
	}
	if text == nil {
		return nil
	}
	first := FirstLine(*text)
	return &first
}

// FirstLine returns text up to its first line break, without a trailing
// carriage return.
func FirstLine(text string) string {
	first, _, _ := strings.Cut(text, "\n")
	return strings.TrimRight(first, "\r")
}

func wrap(msg string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", msg, err)
}

package transcript_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/transcript"
)

// Lines in the shapes Claude Code writes (see the interrupt_* fixtures).
const (
	markerLine     = `{"type":"user","isSidechain":false,"timestamp":"2026-11-12T03:21:48.329Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}` + "\n"
	attachmentLine = `{"type":"attachment","attachment":{"type":"x"}}` + "\n"
	systemLine     = `{"type":"system","subtype":"turn_duration","durationMs":5}` + "\n"
	assistantText  = `{"type":"assistant","message":{"model":"m","role":"assistant","content":[{"type":"text","text":"Sure."}],"usage":{"input_tokens":1}}}` + "\n"
	promptLine     = `{"type":"user","message":{"role":"user","content":"Try again, but write it to b.txt."}}` + "\n"
	toolResultLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}` + "\n"
)

// mustInterrupted returns whether Interrupted found a marker.
func mustInterrupted(t *testing.T, path string, offset int64) bool {
	t.Helper()
	_, found := mustInterruptedAt(t, path, offset)
	return found
}

// mustInterruptedAt returns the marker time and whether there is one; the
// time is zero when there is none.
func mustInterruptedAt(t *testing.T, path string, offset int64) (time.Time, bool) {
	t.Helper()
	at, found, err := transcript.Interrupted(path, offset)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	if !found && !at.IsZero() {
		t.Errorf("not found, but at = %v", at)
	}
	return at, found
}

func TestInterruptedAfterDenial(t *testing.T) {
	content := fixture(t, "interrupt_denial.jsonl")
	p := tempFile(t, content)
	if !mustInterrupted(t, p, 0) {
		t.Error("denial: Interrupted = false, want true")
	}
	// The offset the hook stored before the denial (just past the tool_use).
	if off := int64(bytes.Index(content, []byte(`{"parentUuid":"as1"`))); !mustInterrupted(t, p, off) {
		t.Errorf("denial after offset %d: Interrupted = false, want true", off)
	}
	// A marker before the stored offset was already read by a hook: not news.
	if off := int64(bytes.Index(content, []byte(`{"parentUuid":"u3"`))); mustInterrupted(t, p, off) {
		t.Errorf("marker before offset %d: Interrupted = true, want false", off)
	}
	if mustInterrupted(t, p, int64(len(content))) {
		t.Error("nothing after the offset: Interrupted = true, want false")
	}
}

func TestInterruptedDuringTool(t *testing.T) {
	if !mustInterrupted(t, tempFile(t, fixture(t, "interrupt_during_tool.jsonl")), 0) {
		t.Error("Interrupted = false, want true")
	}
}

func TestInterruptedMidStream(t *testing.T) {
	if !mustInterrupted(t, tempFile(t, fixture(t, "interrupt_mid_stream.jsonl")), 0) {
		t.Error("Interrupted = false, want true")
	}
}

func TestNotInterruptedWhenTurnContinued(t *testing.T) {
	for name, later := range map[string]string{
		"later assistant line":      assistantText,
		"later real user prompt":    promptLine,
		"later tool result":         toolResultLine,
		"prompt then attachment":    promptLine + attachmentLine + systemLine,
		"user line without message": `{"type":"user"}` + "\n",
		"assistant line, odd shape": `{"type":"assistant","message":"x"}` + "\n",
	} {
		for _, fix := range []string{"interrupt_denial.jsonl", "interrupt_during_tool.jsonl", "interrupt_mid_stream.jsonl"} {
			t.Run(name+"/"+fix, func(t *testing.T) {
				p := tempFile(t, fixture(t, fix))
				appendTo(t, p, []byte(later))
				if mustInterrupted(t, p, 0) {
					t.Error("Interrupted = true, want false")
				}
			})
		}
	}
	// No transcript ending in an interrupt, and no conversational line at all.
	for name, content := range map[string]string{
		"normal turn end":  promptLine + assistantText + systemLine,
		"only other lines": attachmentLine + systemLine,
		"empty":            "",
	} {
		t.Run(name, func(t *testing.T) {
			if mustInterrupted(t, tempFile(t, []byte(content)), 0) {
				t.Error("Interrupted = true, want false")
			}
		})
	}
}

func TestInterruptedIgnoresTrailingNonConversationalLines(t *testing.T) {
	blob := `{"type":"api-request-blob","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]\n"}]}}` + "\n"
	p := tempFile(t, []byte(promptLine+assistantText+markerLine+attachmentLine+systemLine+blob+
		`{"type":"last-prompt"}`+"\n"+`{"type":"cost-state"}`+"\n"+`{"no_type":true}`+"\n"+"\n"))
	if !mustInterrupted(t, p, 0) {
		t.Error("marker then other lines: Interrupted = false, want true")
	}
	// A non-conversational line that quotes the marker is not one.
	p = tempFile(t, []byte(promptLine+assistantText+blob+attachmentLine))
	if mustInterrupted(t, p, 0) {
		t.Error("only a blob quoting the marker: Interrupted = true, want false")
	}
}

func TestInterruptedMissingFileAndPartialLine(t *testing.T) {
	dir := t.TempDir()
	for name, path := range map[string]string{
		"missing file": filepath.Join(dir, "nope.jsonl"),
		"empty path":   "",
	} {
		at, found, err := transcript.Interrupted(path, 0)
		if found || !at.IsZero() || err != nil {
			t.Errorf("%s: Interrupted = %v, %v, %v, want zero, false, nil", name, at, found, err)
		}
	}
	// The marker still being written is not there yet.
	p := tempFile(t, []byte(assistantText+strings.TrimSuffix(markerLine, "\n")))
	if mustInterrupted(t, p, 0) {
		t.Error("partial marker line: Interrupted = true, want false")
	}
	appendTo(t, p, []byte("\n"))
	if !mustInterrupted(t, p, 0) {
		t.Error("completed marker line: Interrupted = false, want true")
	}
	// A line being written after the marker may be the next turn.
	appendTo(t, p, []byte(`{"type":"assistant","message":{"content":[{"type":"te`))
	if mustInterrupted(t, p, 0) {
		t.Error("partial line after the marker: Interrupted = true, want false")
	}
	// An offset this file cannot have (it was replaced or shrank) says
	// nothing about what is new.
	p = tempFile(t, []byte(markerLine))
	for _, off := range []int64{-1, int64(len(markerLine)) + 1} {
		if mustInterrupted(t, p, off) {
			t.Errorf("offset %d: Interrupted = true, want false", off)
		}
	}
}

// Both exact marker texts, as one text block, are markers (the fixtures
// hold them in their real lines too).
func TestInterruptedExactMarkers(t *testing.T) {
	for _, text := range []string{"[Request interrupted by user]", "[Request interrupted by user for tool use]"} {
		line := `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
		if !mustInterrupted(t, tempFile(t, []byte(assistantText+line)), 0) {
			t.Errorf("%s: Interrupted = false, want true", text)
		}
	}
}

// A path that is not a regular file is not a transcript: not found, no
// error, and no read that could block or never end.
func TestInterruptedNotRegularFile(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"directory":         t.TempDir(),
		"character device":  os.DevNull,
		"endless device":    "/dev/zero",
		"fifo, no writer":   fifo,
		"symlink to device": symlinkTo(t, "/dev/zero"),
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				at, found, err := transcript.Interrupted(path, 0)
				if found || !at.IsZero() {
					err = fmt.Errorf("found %v at %v", found, at)
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Interrupted: %v, want zero, false, nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Interrupted did not return within 5 s")
			}
		})
	}
	// A symlink to a regular transcript is still read.
	if !mustInterrupted(t, symlinkTo(t, tempFile(t, []byte(markerLine))), 0) {
		t.Error("symlink to a transcript: Interrupted = false, want true")
	}
}

// A regular file that cannot be opened is an error, so the caller can
// report it.
func TestInterruptedUnreadableFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "locked.jsonl")
	if err := os.WriteFile(p, []byte(markerLine), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := transcript.Interrupted(p, 0); err == nil {
		t.Error("want an error for a file that cannot be opened")
	}
}

// symlinkTo returns a new symlink to target.
func symlinkTo(t *testing.T, target string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInterruptedHostileContent(t *testing.T) {
	const m = "[Request interrupted by user]"
	for name, line := range map[string]string{
		"marker later in a prompt":     `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":"please print ` + m + `"}}`,
		"marker later in a text block": `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"see ` + m + `"}]}}`,
		"marker after whitespace":      `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":" ` + m + `"}}`,
		"marker in a tool result":      `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"` + m + `"}]}}`,
		"marker in tool result blocks": `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"` + m + `"}]}]}}`,
		"marker in an assistant line":  `{"type":"assistant","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"` + m + `"}]}}`,
		"marker string from assistant": `{"type":"assistant","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":"` + m + `"}}`,
		"marker then a prompt block":   `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"` + m + `"},{"type":"text","text":"now do b"}]}}`,
		"tool result then marker":      `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"x"},{"type":"text","text":"` + m + `"}]}}`,
		"marker block of another type": `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"image","text":"` + m + `"}]}}`,
		"marker from a subagent":       `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","isSidechain":true,"message":{"content":[{"type":"text","text":"` + m + `"}]}}`,
		"marker in a meta line":        `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","isMeta":true,"message":{"content":[{"type":"text","text":"` + m + `"}]}}`,
		"content a number":             `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":42}}`,
		"content an object":            `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":{"type":"text","text":"` + m + `"}}}`,
		"content null":                 `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":null}}`,
		"content empty array":          `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[]}}`,
		"block not an object":          `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":["` + m + `"]}}`,
		"message a string":             `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":"` + m + `"}`,
		"top level an array":           `["user","` + m + `"]`,
		"invalid JSON":                 `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":"` + m + `"`,
		// Ruling 80: a plain string is how a typed or pasted prompt is
		// stored; every real marker is one text block equal to a marker.
		"typed prompt equal to the marker":  `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":"` + m + `"}}`,
		"typed prompt starting with it":     `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":"` + m + ` and then what?"}}`,
		"typed tool-use marker":             `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":"[Request interrupted by user for tool use]"}}`,
		"text block continuing the marker":  `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"` + m + ` and more"}]}}`,
		"text block, tool-use marker cont.": `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use] x"}]}}`,
		"text block, marker prefix only":    `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"[Request interrupted by user"}]}}`,
		"text block, marker then a space":   `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"` + m + ` "}]}}`,
		"text block, marker then a newline": `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"` + m + `\n"}]}}`,
		"text block, space then the marker": `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":" ` + m + `"}]}}`,
		"text block, other case":            `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"[request interrupted by user]"}]}}`,
		"two blocks, the first the marker":  `{"type":"user","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"` + m + `"},{"type":"text","text":"` + m + `"}]}}`,
		"invalid UTF-8 prefix":              "{\"type\":\"user\",\"timestamp\":\"2026-11-12T03:21:48.329Z\",\"message\":{\"content\":\"\xff" + m + "\"}}",
	} {
		t.Run(name, func(t *testing.T) {
			// Alone after an assistant line it is not a marker...
			if mustInterrupted(t, tempFile(t, []byte(assistantText+line+"\n")), 0) {
				t.Error("as the last line: Interrupted = true, want false")
			}
			// ...and after a real marker it is a line the marker is not last before.
			if mustInterrupted(t, tempFile(t, []byte(markerLine+line+"\n")), 0) {
				t.Error("after a marker: Interrupted = true, want false")
			}
		})
	}
	big := `{"type":"assistant","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":[{"type":"text","text":"` + strings.Repeat("a", 1<<20) + `"}]}}` + "\n"
	other := `{"type":"attachment","attachment":{"content":"` + strings.Repeat("b", 4000) + `"}}` + "\n"
	for _, tt := range []struct {
		name    string
		content string
		want    bool
	}{
		// Not a user line, so not a marker (and, like any other type, skipped).
		{"type in another case", assistantText + `{"type":"User","timestamp":"2026-11-12T03:21:48.329Z","message":{"content":"` + m + `"}}` + "\n", false},
		{"invalid JSON before the marker", "{not json\n" + markerLine, true},
		{"1 MB assistant line after the marker", markerLine + big, false},
		{"1 MB line before the marker", big + markerLine, true},
		// The tail window holds no conversational line, so nothing is known.
		{"marker followed by over 1 MB of other lines", assistantText + markerLine + strings.Repeat(other, 300), false},
		{"marker at the end of 3 MB unread", strings.Repeat(assistantText, 3<<20/len(assistantText)) + markerLine, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := mustInterrupted(t, tempFile(t, []byte(tt.content)), 0); got != tt.want {
				t.Errorf("Interrupted = %v, want %v", got, tt.want)
			}
		})
	}
}

// Ruling 79: the marker's own timestamp is returned, parsed as a time, and a
// marker without a valid one is not a marker.
func TestInterruptedReturnsMarkerTime(t *testing.T) {
	ms := func(s string) time.Time {
		t.Helper()
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	for fix, want := range map[string]string{
		"interrupt_denial.jsonl":      "2026-11-12T03:21:48.329Z",
		"interrupt_during_tool.jsonl": "2026-11-12T03:21:59.887Z",
		"interrupt_mid_stream.jsonl":  "2026-11-12T03:22:06.059Z",
	} {
		if at, found := mustInterruptedAt(t, tempFile(t, fixture(t, fix)), 0); !found || !at.Equal(ms(want)) {
			t.Errorf("%s: Interrupted = %v, %v, want %s", fix, at, found, want)
		}
	}
	marker := func(ts string) string {
		return `{"type":"user",` + ts + `"message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}` + "\n"
	}
	// The last of two markers wins; another zone is the same instant.
	p := tempFile(t, []byte(marker(`"timestamp":"2026-11-12T03:00:00.000Z",`)+marker(`"timestamp":"2026-11-12T04:21:48.5+01:00",`)))
	if at, found := mustInterruptedAt(t, p, 0); !found || !at.Equal(ms("2026-11-12T03:21:48.500Z")) {
		t.Errorf("two markers: Interrupted = %v, %v, want 03:21:48.5Z", at, found)
	}
	for name, ts := range map[string]string{
		"missing":       ``,
		"empty":         `"timestamp":"",`,
		"not a time":    `"timestamp":"yesterday",`,
		"no zone":       `"timestamp":"2026-11-12T03:21:48.329",`,
		"space form":    `"timestamp":"2026-11-12 03:21:48.329Z",`,
		"a number":      `"timestamp":1794453708329,`,
		"null":          `"timestamp":null,`,
		"out of range":  `"timestamp":"2026-13-40T03:21:48.329Z",`,
		"trailing text": `"timestamp":"2026-11-12T03:21:48.329Z now",`,
	} {
		t.Run(name, func(t *testing.T) {
			if mustInterrupted(t, tempFile(t, []byte(assistantText+marker(ts))), 0) {
				t.Error("as the last line: Interrupted found a marker")
			}
			// It is a line not understood, so an earlier good marker is not last.
			if mustInterrupted(t, tempFile(t, []byte(markerLine+marker(ts))), 0) {
				t.Error("after a good marker: Interrupted found a marker")
			}
		})
	}
}

package transcript_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/transcript"
)

// Lines in the shapes Claude Code writes (see the interrupt_* fixtures).
const (
	markerLine     = `{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}` + "\n"
	attachmentLine = `{"type":"attachment","attachment":{"type":"x"}}` + "\n"
	systemLine     = `{"type":"system","subtype":"turn_duration","durationMs":5}` + "\n"
	assistantText  = `{"type":"assistant","message":{"model":"m","role":"assistant","content":[{"type":"text","text":"Sure."}],"usage":{"input_tokens":1}}}` + "\n"
	promptLine     = `{"type":"user","message":{"role":"user","content":"Try again, but write it to b.txt."}}` + "\n"
	toolResultLine = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}` + "\n"
)

func mustInterrupted(t *testing.T, path string, offset int64) bool {
	t.Helper()
	got, err := transcript.Interrupted(path, offset)
	if err != nil {
		t.Fatalf("Interrupted: %v", err)
	}
	return got
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
		got, err := transcript.Interrupted(path, 0)
		if got || err != nil {
			t.Errorf("%s: Interrupted = %v, %v, want false, nil", name, got, err)
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
	if _, err := transcript.Interrupted(t.TempDir(), 0); err == nil {
		t.Error("directory path: want an error")
	}
}

func TestInterruptedHostileContent(t *testing.T) {
	const m = "[Request interrupted by user]"
	for name, line := range map[string]string{
		"marker later in a prompt":     `{"type":"user","message":{"content":"please print ` + m + `"}}`,
		"marker later in a text block": `{"type":"user","message":{"content":[{"type":"text","text":"see ` + m + `"}]}}`,
		"marker after whitespace":      `{"type":"user","message":{"content":" ` + m + `"}}`,
		"marker in a tool result":      `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"` + m + `"}]}}`,
		"marker in tool result blocks": `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"` + m + `"}]}]}}`,
		"marker in an assistant line":  `{"type":"assistant","message":{"content":[{"type":"text","text":"` + m + `"}]}}`,
		"marker string from assistant": `{"type":"assistant","message":{"content":"` + m + `"}}`,
		"marker then a prompt block":   `{"type":"user","message":{"content":[{"type":"text","text":"` + m + `"},{"type":"text","text":"now do b"}]}}`,
		"tool result then marker":      `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"x"},{"type":"text","text":"` + m + `"}]}}`,
		"marker block of another type": `{"type":"user","message":{"content":[{"type":"image","text":"` + m + `"}]}}`,
		"marker from a subagent":       `{"type":"user","isSidechain":true,"message":{"content":[{"type":"text","text":"` + m + `"}]}}`,
		"marker in a meta line":        `{"type":"user","isMeta":true,"message":{"content":[{"type":"text","text":"` + m + `"}]}}`,
		"content a number":             `{"type":"user","message":{"content":42}}`,
		"content an object":            `{"type":"user","message":{"content":{"type":"text","text":"` + m + `"}}}`,
		"content null":                 `{"type":"user","message":{"content":null}}`,
		"content empty array":          `{"type":"user","message":{"content":[]}}`,
		"block not an object":          `{"type":"user","message":{"content":["` + m + `"]}}`,
		"message a string":             `{"type":"user","message":"` + m + `"}`,
		"top level an array":           `["user","` + m + `"]`,
		"invalid JSON":                 `{"type":"user","message":{"content":"` + m + `"`,
		"invalid UTF-8 prefix":         "{\"type\":\"user\",\"message\":{\"content\":\"\xff" + m + "\"}}",
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
	big := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("a", 1<<20) + `"}]}}` + "\n"
	other := `{"type":"attachment","attachment":{"content":"` + strings.Repeat("b", 4000) + `"}}` + "\n"
	for _, tt := range []struct {
		name    string
		content string
		want    bool
	}{
		// Not a user line, so not a marker (and, like any other type, skipped).
		{"type in another case", assistantText + `{"type":"User","message":{"content":"` + m + `"}}` + "\n", false},
		{"string content marker counts", assistantText + `{"type":"user","message":{"content":"` + m + `"}}` + "\n", true},
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

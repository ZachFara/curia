package transcript_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/transcript"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tempFile(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendTo(t *testing.T, path string, content []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string, offset int64) (transcript.Update, int64) {
	t.Helper()
	u, off, err := transcript.Read(path, offset)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return u, off
}

func TestReadsLatestUsage(t *testing.T) {
	content := fixture(t, "two_assistant.jsonl")
	u, off := mustRead(t, tempFile(t, content), 0)
	if !u.Found {
		t.Fatal("Found = false")
	}
	if u.Model != "claude-sonnet-5-5" {
		t.Errorf("Model = %q", u.Model)
	}
	if u.ContextUsed != 12+140000+1988 {
		t.Errorf("ContextUsed = %d", u.ContextUsed)
	}
	if off != int64(len(content)) {
		t.Errorf("offset = %d, want %d", off, len(content))
	}
}

func TestIncrementalOffset(t *testing.T) {
	p := tempFile(t, fixture(t, "two_assistant.jsonl"))
	_, off := mustRead(t, p, 0)

	appendTo(t, p, fixture(t, "append_line.jsonl"))
	u, off2 := mustRead(t, p, off)
	if !u.Found || u.Model != "claude-opus-5-5" || u.ContextUsed != 6 || u.LastAssistantText != "Appended." {
		t.Errorf("update = %+v", u)
	}
	if off2 <= off {
		t.Errorf("offset did not advance: %d -> %d", off, off2)
	}

	u, off3 := mustRead(t, p, off2)
	if u.Found || off3 != off2 {
		t.Errorf("idle read = %+v, offset %d, want empty and %d", u, off3, off2)
	}
}

func TestPartialTrailingLine(t *testing.T) {
	p := tempFile(t, fixture(t, "two_assistant.jsonl"))
	_, off := mustRead(t, p, 0)

	line := strings.TrimSuffix(string(fixture(t, "append_line.jsonl")), "\n")
	appendTo(t, p, []byte(line))
	u, off2 := mustRead(t, p, off)
	if u.Found || off2 != off {
		t.Fatalf("partial line consumed: %+v, offset %d, want %d", u, off2, off)
	}

	appendTo(t, p, []byte("\n"))
	u, off3 := mustRead(t, p, off2)
	if !u.Found || u.LastAssistantText != "Appended." {
		t.Errorf("completed line not read: %+v", u)
	}
	if off3 != off+int64(len(line))+1 {
		t.Errorf("offset = %d", off3)
	}
}

func TestMissingFile(t *testing.T) {
	u, off, err := transcript.Read(filepath.Join(t.TempDir(), "nope.jsonl"), 42)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if u != (transcript.Update{}) || off != 42 {
		t.Errorf("got %+v, %d", u, off)
	}
}

func TestShrunkFileRereadsFromZero(t *testing.T) {
	line := fixture(t, "append_line.jsonl")
	if len(line) >= 10000 {
		t.Fatal("fixture too large")
	}
	u, off := mustRead(t, tempFile(t, line), 10000)
	if !u.Found || u.LastAssistantText != "Appended." {
		t.Errorf("update = %+v", u)
	}
	if off != int64(len(line)) {
		t.Errorf("offset = %d", off)
	}
}

func TestNegativeOffsetReadsFromZero(t *testing.T) {
	u, _ := mustRead(t, tempFile(t, fixture(t, "append_line.jsonl")), -5)
	if !u.Found {
		t.Error("Found = false")
	}
}

func TestSkipsSyntheticAndMalformed(t *testing.T) {
	content := fixture(t, "synthetic_malformed.jsonl")
	u, off := mustRead(t, tempFile(t, content), 0)
	if !u.Found || u.Model != "claude-opus-5-5" || u.ContextUsed != 777 || u.LastAssistantText != "Real." {
		t.Errorf("update = %+v", u)
	}
	if off != int64(len(content)) {
		t.Errorf("offset = %d, want %d", off, len(content))
	}
}

func TestLastAssistantTextFirstLine(t *testing.T) {
	u, _ := mustRead(t, tempFile(t, fixture(t, "two_assistant.jsonl")), 0)
	if u.LastAssistantText != "Done." {
		t.Errorf("LastAssistantText = %q", u.LastAssistantText)
	}
}

func TestOversizedLineIsSkippedNotFatal(t *testing.T) {
	huge := `{"type":"assistant","message":{"model":"evil","usage":{"input_tokens":1},"content":[{"type":"text","text":"` +
		strings.Repeat("x", 20<<20) + `"}]}}` + "\n"
	good := string(fixture(t, "append_line.jsonl"))
	content := good + huge + strings.Replace(good, "Appended.", "After.", 1)
	u, off := mustRead(t, tempFile(t, []byte(content)), 0)
	if !u.Found || u.LastAssistantText != "After." || u.Model != "claude-opus-5-5" {
		t.Errorf("update = %+v", u)
	}
	if off != int64(len(content)) {
		t.Errorf("offset = %d, want %d", off, len(content))
	}
}

func TestHostileValues(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"model":"m","usage":{"input_tokens":-5,"cache_read_input_tokens":9223372036854775807,"cache_creation_input_tokens":9223372036854775807},"content":"just a string"}}`,
		`{"type":"assistant","message":{"model":"m2","usage":{"input_tokens":1,"cache_read_input_tokens":1,"cache_creation_input_tokens":1},"content":[{"type":"text","text":""},{"type":"text","text":"\nsecond line"},null,5]}}`,
	}
	u, _ := mustRead(t, tempFile(t, []byte(lines[0]+"\n")), 0)
	if !u.Found || u.ContextUsed != math.MaxInt32 {
		t.Errorf("update = %+v, want ContextUsed %d", u, math.MaxInt32)
	}
	u, _ = mustRead(t, tempFile(t, []byte(lines[1]+"\n")), 0)
	if !u.Found || u.Model != "m2" || u.ContextUsed != 3 || u.LastAssistantText != "" {
		t.Errorf("update = %+v", u)
	}
}

func TestDirectoryPathReturnsError(t *testing.T) {
	if _, _, err := transcript.Read(t.TempDir(), 0); err == nil {
		t.Error("expected error reading a directory")
	}
}

func assistantLine(model, text string, in int) string {
	block := `{"type":"tool_use","id":"t","name":"Bash","input":{}}`
	if text != "" {
		block = `{"type":"text","text":"` + text + `"}`
	}
	return fmt.Sprintf(`{"type":"assistant","message":{"model":%q,"usage":{"input_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},"content":[%s]}}`+"\n", model, in, block)
}

func TestTextFromEarlierLineUsageFromLast(t *testing.T) {
	content := assistantLine("m1", "All done.\\nMore", 100) + assistantLine("m2", "", 250)
	u, off := mustRead(t, tempFile(t, []byte(content)), 0)
	if !u.Found || u.LastAssistantText != "All done." || u.Model != "m2" || u.ContextUsed != 250 {
		t.Errorf("update = %+v", u)
	}
	if off != int64(len(content)) {
		t.Errorf("offset = %d", off)
	}
}

func TestLargeUnreadSpanStartsInTailWindow(t *testing.T) {
	// A usable line, then ~1.5 MiB of short lines, then the last usable line.
	filler := strings.Repeat(`{"type":"user","message":{"content":"`+strings.Repeat("y", 90)+`"}}`+"\n", 16000)
	content := assistantLine("before", "Early.", 1) + filler + assistantLine("last", "Late.", 2)
	u, off := mustRead(t, tempFile(t, []byte(content)), 0)
	if !u.Found || u.Model != "last" || u.ContextUsed != 2 || u.LastAssistantText != "Late." {
		t.Errorf("update = %+v", u)
	}
	if off != int64(len(content)) {
		t.Errorf("offset = %d, want %d", off, len(content))
	}

	// With only an early usable line outside the window, nothing is reported.
	content = assistantLine("before", "Early.", 1) + filler
	u, off = mustRead(t, tempFile(t, []byte(content)), 0)
	if u.Found || off != int64(len(content)) {
		t.Errorf("update = %+v, offset %d, want empty and %d", u, off, len(content))
	}
}

func TestTailWindowStartsMidLine(t *testing.T) {
	// One 1.5 MiB line spans the window start; its tail must not be parsed as
	// a line, and the following line is read normally.
	big := `{"type":"user","message":{"content":"` + strings.Repeat("z", 3<<19) + `"}}` + "\n"
	content := assistantLine("before", "Early.", 1) + big + assistantLine("last", "Late.", 2)
	u, off := mustRead(t, tempFile(t, []byte(content)), 0)
	if !u.Found || u.Model != "last" || u.LastAssistantText != "Late." {
		t.Errorf("update = %+v", u)
	}
	if off != int64(len(content)) {
		t.Errorf("offset = %d, want %d", off, len(content))
	}
}

func TestTailWindowWithoutCompleteLine(t *testing.T) {
	// No newline anywhere in the window: nothing consumed, offset unchanged.
	content := assistantLine("before", "Early.", 1) + strings.Repeat("q", 2<<20)
	start := int64(len(assistantLine("before", "Early.", 1)))
	u, off := mustRead(t, tempFile(t, []byte(content)), start)
	if u.Found || off != start {
		t.Errorf("update = %+v, offset %d, want empty and %d", u, off, start)
	}
}

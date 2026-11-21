package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTail(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const tailAssistant = `{"type":"assistant","message":{"content":[{"type":"text","text":"hello\nsecond \u001b[31mred\u001b[0m"},{"type":"tool_use","name":"Bash"}]}}` + "\n"

const tailResult = `{"type":"user","message":{"content":[{"type":"tool_result","content":"out one\nout two"}]}}` + "\n"

func TestTailLinesAreCleanedAndToolNamesShown(t *testing.T) {
	got, err := Tail(writeTail(t, tailAssistant+tailResult+"not json\n"), 30)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hello", "second red", "tool: Bash", "  out one"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTailKeepsOnlyTheLastLines(t *testing.T) {
	var b strings.Builder
	for range 100 {
		b.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}]}}` + "\n")
	}
	got, _ := Tail(writeTail(t, b.String()), 30)
	if len(got) != 30 {
		t.Fatalf("got %d lines", len(got))
	}
}

func TestTailOneHugeLineIsBoundedAndSkipped(t *testing.T) {
	huge := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("a", 5<<20) + `"}]}}` + "\n"
	got, err := Tail(writeTail(t, tailAssistant+huge), 30)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d lines, err %v", len(got), err)
	}
}

func TestTailMissingAndNonRegularFiles(t *testing.T) {
	if got, err := Tail(filepath.Join(t.TempDir(), "gone"), 30); err != nil || got != nil {
		t.Fatalf("missing: %v %v", got, err)
	}
	if got, err := Tail(t.TempDir(), 30); err != nil || got != nil {
		t.Fatalf("directory: %v %v", got, err)
	}
}

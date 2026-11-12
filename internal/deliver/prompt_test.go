package deliver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// promptFixture reads testdata/prompt/name. Files named live-* are pane
// captures of Claude Code 2.1.323 from live check 2 (cut to the bottom of
// the screen); built-* are made from live-idle.txt by changing the input box
// or footer as the live check report describes (a draft, a two-line draft,
// "Esc again to clear", the Rewind menu, waiting for a background agent), or
// are other screens altogether.
func promptFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "prompt", name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Ruling 85, defects 1, 2 and 3: only an empty Claude Code input prompt,
// with nothing open over it and only a known idle footer under it, may be
// typed into. Everything else, recognized or not, is refused.
func TestDefects123ReadPrompt(t *testing.T) {
	for name, want := range map[string]promptState{
		"live-idle.txt":                promptEmpty,
		"built-idle-manual.txt":        promptEmpty,
		"built-idle-no-footer.txt":     promptEmpty,
		"live-busy.txt":                promptBusy,
		"live-dialog-permission.txt":   promptDialog, // defect 1: Enter would approve
		"live-dialog-subagent.txt":     promptDialog, // defect 1, run L3
		"built-draft.txt":              promptDraft,  // defect 2
		"built-draft-multiline.txt":    promptDraft,
		"built-esc-again.txt":          promptDraft,
		"built-rewind.txt":             promptDialog, // defect 3: the line would be lost
		"built-waiting-background.txt": promptSubagents,
		"built-shell.txt":              promptUnknown,
		"built-empty.txt":              promptUnknown,
		"built-other-footer.txt":       promptUnknown, // a footer these rules do not know
		"built-below-box.txt":          promptUnknown, // a list under the input box
	} {
		if got := readPrompt(promptFixture(t, name)); got != want {
			t.Errorf("%s: readPrompt = %d, want %d", name, got, want)
		}
	}
}

// A Claude Code UI change fails safe: each small change to the captured
// idle screen is no longer read as an empty prompt.
func TestReadPromptFailsSafeOnChanges(t *testing.T) {
	idle := promptFixture(t, "live-idle.txt")
	rule := strings.Repeat("─", 40)
	for name, screen := range map[string]string{
		"no prompt mark":       strings.Replace(idle, "❯", ">", 1),
		"rules drawn in ASCII": strings.ReplaceAll(idle, "─", "-"),
		"no bottom rule":       strings.Replace(idle, "─\n  ⏵⏵", "x\n  ⏵⏵", 1),
		"a third footer line":  strings.TrimRight(idle, "\n") + "\n  one\n  two\n",
		"empty box":            "x\n" + rule + "\n" + rule + "\n",
		"text after the box":   strings.TrimRight(idle, "\n") + "\nDecision D-1 answered\n",
		"short rule":           "x\n" + "─────\n❯ \n" + rule + "\n",
		"running footer":       strings.Replace(idle, "(shift+tab to cycle)", "(shift+tab to cycle) · esc to interrupt", 1),
	} {
		if got := readPrompt(screen); got == promptEmpty {
			t.Errorf("%s: read as an empty prompt", name)
		}
	}
}

package deliver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// promptFixture reads testdata/prompt/name. Files named lc2-* and lc3-*
// are raw `capture-pane -p` captures of Claude Code 2.1.323 from live checks
// 2 and 3 (home paths, user and host names replaced). Files named built-*
// were not captured: they are a captured screen changed as a report
// describes (built-esc-again: the "Esc again to clear" footer, never shown
// in live check 3; built-waiting-background: the waiting line without the
// agent list), or other screens altogether (an empty pane, a footer no
// capture shows, no footer at all).
func promptFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "prompt", name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// promptCases is every fixture with what readPrompt must read in it.
var promptCases = map[string]promptState{
	// The empty prompt in every permission mode and width captured.
	"lc2-idle.txt":                        promptEmpty,
	"lc3-idle-auto-200col.txt":            promptEmpty,
	"lc3-idle-manual-200col.txt":          promptEmpty, // live check 3, defect 1
	"lc3-idle-acceptedits-200col.txt":     promptEmpty,
	"lc3-idle-plan-200col.txt":            promptEmpty,
	"lc3-idle-auto-60col.txt":             promptEmpty,
	"lc3-idle-auto-320col.txt":            promptEmpty,
	"lc3-idle-auto-after-subagent.txt":    promptEmpty,
	"lc3-idle-auto-after-clear.txt":       promptEmpty,
	"lc3-idle-auto-after-argv-prompt.txt": promptEmpty,
	// Refused, and safe: a footer cut short at 34 columns.
	"lc3-idle-auto-34col.txt": promptUnknown,
	// A fresh session's dim placeholder reads as a draft: a plain capture
	// cannot tell them apart (live check 3, defect 3).
	"lc3-idle-fresh-acceptedits-placeholder.txt": promptDraft,
	// Agents offered or listed under the box.
	"lc3-idle-fresh-agents-hint.txt":        promptAgents,
	"lc3-idle-fresh-manual-placeholder.txt": promptAgents,
	"lc3-agents-listed-main-stopped.txt":    promptAgents,
	// Drafts.
	"lc3-draft-oneline.txt":             promptDraft,
	"lc3-draft-multiline-backslash.txt": promptDraft,
	"lc3-draft-multiline-pasted.txt":    promptDraft,
	"lc3-draft-after-one-escape.txt":    promptDraft,
	"lc3-menu-slash.txt":                promptDraft, // the box holds "❯ /"
	"built-esc-again.txt":               promptDraft,
	// Dialogs, menus and full-screen views.
	"lc2-dialog-permission.txt":            promptDialog,
	"lc2-dialog-subagent.txt":              promptDialog,
	"lc3-dialog-permission-early.txt":      promptDialog,
	"lc3-dialog-permission-waiting.txt":    promptDialog,
	"lc3-dialog-subagent-main-writing.txt": promptDialog,
	"lc3-dialog-subagent-main-stopped.txt": promptDialog,
	"lc3-dialog-subagent2-first.txt":       promptDialog,
	"lc3-dialog-trust-folder.txt":          promptDialog,
	"lc3-menu-rewind.txt":                  promptDialog,
	"lc3-screen-help.txt":                  promptDialog,
	"lc3-screen-config.txt":                promptDialog, // live check 3, defect 5
	"lc3-screen-config-nofilter.txt":       promptDialog,
	// A running turn, and waiting for background agents.
	"lc2-busy.txt":                 promptBusy,
	"lc3-busy-running-turn.txt":    promptBusy,
	"built-waiting-background.txt": promptSubagents,
	// No Claude Code at its prompt.
	"lc3-shell-after-kill.txt": promptShell,
	"lc3-shell-after-exit.txt": promptShell,
	"built-empty.txt":          promptUnknown,
	"built-other-footer.txt":   promptUnknown,
	"built-idle-no-footer.txt": promptUnknown, // no capture shows an idle prompt with no footer
}

// Ruling 85, defects 1, 2 and 3, and live check 3: only an empty Claude Code
// input prompt with exactly one captured idle footer may be typed into.
// Every fixture is read as its table entry says, and every refusal has a
// note of its own.
func TestDefects123ReadPrompt(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "prompt", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, ok := promptCases[filepath.Base(f)]; !ok {
			t.Errorf("fixture %s has no entry in promptCases", filepath.Base(f))
		}
	}
	for name, want := range promptCases {
		got := readPrompt(promptFixture(t, name))
		if got != want {
			t.Errorf("%s: readPrompt = %d, want %d", name, got, want)
		}
		if note, ok := promptNotes[got]; got != promptEmpty && (!ok || note == "") {
			t.Errorf("%s: no note for state %d", name, got)
		}
	}
	for state, want := range map[promptState]string{
		promptDialog: "a dialog or menu is open in the agent's pane; nothing typed",
		promptAgents: "the agent's prompt lists background agents (or offers them); nothing typed",
		promptShell:  "the pane shows a shell: the session's Claude Code is not at its prompt; nothing typed",
	} {
		if promptNotes[state] != want {
			t.Errorf("note for %d = %q, want %q", state, promptNotes[state], want)
		}
	}
}

// A Claude Code UI change fails safe: each small change to the captured
// idle screen is no longer read as an empty prompt.
func TestReadPromptFailsSafeOnChanges(t *testing.T) {
	idle := promptFixture(t, "lc3-idle-auto-200col.txt")
	rule := strings.Repeat("─", 40)
	for name, screen := range map[string]string{
		"no prompt mark":       strings.Replace(idle, "❯\u00a0\n", ">\u00a0\n", 1),
		"rules drawn in ASCII": strings.ReplaceAll(idle, "─", "-"),
		"a second footer line": strings.TrimRight(idle, "\n") + "\n  one\n",
		"footer with a hint":   strings.Replace(idle, "(shift+tab to cycle)", "(shift+tab to cycle) · ← for agents", 1),
		"empty box":            "x\n" + rule + "\n" + rule + "\n",
		"text after the box":   strings.TrimRight(idle, "\n") + "\nDecision D-1 answered\n",
		"short rule":           "x\n" + "─────\n❯\u00a0\n" + rule + "\n",
		"running footer":       strings.Replace(idle, "(shift+tab to cycle)", "(shift+tab to cycle) · esc to interrupt", 1),
		"mode not captured":    strings.Replace(idle, "auto mode on", "bypass permissions on", 1),
	} {
		if got := readPrompt(screen); got == promptEmpty {
			t.Errorf("%s: read as an empty prompt", name)
		}
	}
}

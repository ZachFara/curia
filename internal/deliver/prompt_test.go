package deliver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/tmux"
)

// readPrompt is readScreen of a plain capture, with no escape sequences to
// tell a dim suggestion by.
func readPrompt(screen string) promptState { return readScreen(screen, nil) }

// promptFixture reads testdata/prompt/name. Files named lc2-* and lc3-*
// are raw `capture-pane -p` captures of Claude Code 2.1.323 from live checks
// 2 and 3, lc4-* of Claude Code 2.1.325 from Plan 3's live probe and live
// check A, and lc5-* of 2.1.325 from live check B, the *.esc ones taken
// with `capture-pane -e -p` (home paths, user and host names and temp
// directories replaced). Files named built-*
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
	// Ruling 101 and LA3: for about 30 s after a subagent ran, 2.1.325 adds
	// "/tasks to see subagents" to the footer in every mode.
	"lc4-idle-auto-tasks-hint.txt":        promptEmpty,
	"lc4-idle-manual-tasks-hint.txt":      promptEmpty,
	"lc4-idle-acceptedits-tasks-hint.txt": promptEmpty,
	"lc4-idle-plan-tasks-hint.txt":        promptEmpty,
	"lc5-idle-plan.txt":                   promptEmpty,
	// LA4: after a Stop-hook delivery the footer gains a second line for a
	// few seconds; idle, it is an empty prompt, and a running turn is busy.
	"lc4-idle-stophook-second-line.txt": promptEmpty,
	"lc5-idle-stophook-second-line.txt": promptEmpty,
	"lc5-busy-stophook-second-line.txt": promptBusy,
	// Claude Code's feedback survey above the box: a digit would answer it.
	"lc5-survey-idle.txt": promptSurvey,
	// A team lead in a narrow pane: the footer is cut and the hint with it,
	// but the agent list under the box is there (live check B, defect 3).
	"lc5-lead-narrow-agents-listed.txt": promptManage,
	// Without escape sequences a dim suggestion cannot be told from a draft
	// (readCapture reads the *.esc captures).
	"lc5-suggestion-push-it-plain.txt": promptDraft,
	"lc5-draft-equals-suggestion.txt":  promptDraft,
	"lc5-draft-multi-line.txt":         promptUnknown, // its footer adds "ctrl+g to edit in VS Code": refused

	"lc5-draft-one-word.txt": promptDraft,
	// A team lead after its team was shut down: the plain footer again.
	"lc4-idle-lead-after-team-shutdown.txt": promptEmpty,
	// Refused: tmux's focus-events hint, on the footer line or under it.
	"lc4-idle-focus-hint-same-line.txt":    promptUnknown,
	"lc4-fresh-focus-hint-second-line.txt": promptUnknown,
	// Refused, and safe: a footer cut short at 34 columns.
	"lc3-idle-auto-34col.txt": promptUnknown,
	// A fresh session's dim placeholder reads as a draft: a plain capture
	// cannot tell them apart (live check 3, defect 3).
	"lc3-idle-fresh-acceptedits-placeholder.txt": promptDraft,
	// Agents offered under the box.
	"lc3-idle-fresh-agents-hint.txt":        promptAgents,
	"lc3-idle-fresh-manual-placeholder.txt": promptAgents,
	"lc4-resume-agents-hint.txt":            promptAgents,
	// Background agents or a live team's teammates listed under the box.
	"lc3-agents-listed-main-stopped.txt": promptManage,
	"lc4-team-lead-agents-listed.txt":    promptManage,
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
		promptManage: "background agents or teammates are listed under the agent's prompt " +
			"(nothing is typed to a team lead while its teammates are alive); nothing typed",
		promptShell: "the pane shows a shell: the session's Claude Code is not at its prompt; nothing typed",
		promptSurvey: "Claude Code's feedback survey is on screen, and a typed line holds digits that would answer it; " +
			"nothing typed",
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
		// Ruling 101 accepts the /tasks hint only as captured: after the auto
		// mode footer, alone.
		"tasks hint twice":       strings.Replace(idle, "(shift+tab to cycle)", "(shift+tab to cycle) · /tasks to see subagents · /tasks to see subagents", 1),
		"tasks hint and manage":  strings.Replace(idle, "(shift+tab to cycle)", "(shift+tab to cycle) · /tasks to see subagents · ↓ to manage", 1),
		"tasks hint and running": strings.Replace(idle, "(shift+tab to cycle)", "(shift+tab to cycle) · /tasks to see subagents · esc to interrupt", 1),
		"tasks hint before mode": strings.Replace(idle, "⏵⏵ auto mode on (shift+tab to cycle)", "/tasks to see subagents · ⏵⏵ auto mode on (shift+tab to cycle)", 1),
		"tasks hint, other text": strings.Replace(idle, "(shift+tab to cycle)", "(shift+tab to cycle) · /tasks to see agents", 1),
	} {
		if got := readPrompt(screen); got == promptEmpty {
			t.Errorf("%s: read as an empty prompt", name)
		}
	}
}

// Ruling 101 and LA3: the "/tasks to see subagents" hint is accepted after
// each mode's footer, as captured, and only as captured.
func TestTasksHintInEveryMode(t *testing.T) {
	for _, name := range []string{
		"lc4-idle-auto-tasks-hint.txt", "lc4-idle-manual-tasks-hint.txt",
		"lc4-idle-acceptedits-tasks-hint.txt", "lc4-idle-plan-tasks-hint.txt",
	} {
		idle := promptFixture(t, name)
		if got := readPrompt(idle); got != promptEmpty {
			t.Errorf("%s: readPrompt = %d, want empty", name, got)
		}
		// The hint glued to anything else is not an idle footer.
		changed := strings.Replace(idle, " · /tasks to see subagents", " · /tasks to see subagent", 1)
		if got := readPrompt(changed); got != promptUnknown {
			t.Errorf("%s with the hint changed: readPrompt = %d, want unknown", name, got)
		}
	}
}

// promptCursorAtInput is capt with the cursor where an empty input's is:
// on the input line (the last line starting with "❯"), after "❯ ".
func promptCursorAtInput(t *testing.T, styled string) tmux.Capture {
	t.Helper()
	capt := tmux.Capture{Styled: styled, Screen: tmux.StripEscapes(styled)}
	lines := strings.Split(capt.Screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], promptMark) {
			capt.CursorX, capt.CursorY, capt.CursorKnown = placeholderColumn, i, true
			return capt
		}
	}
	t.Fatalf("no input line in %q", styled)
	return capt
}

// Ruling 110: with escape sequences, a suggestion Claude Code draws dim in
// an empty prompt reads as empty; a draft never does, not even one equal to
// a suggestion, one with a dim suggestion glued to it, or one on two lines.
func TestReadCaptureSuggestion(t *testing.T) {
	cases := map[string]promptState{
		"lc5-t4-after-brief.esc":           promptEmpty, // ❯ push it, auto mode
		"lc5-t5-suggestion-mode1.esc":      promptEmpty, // manual
		"lc5-t5-suggestion-mode2.esc":      promptEmpty, // accept edits
		"lc5-t5-suggestion-mode3.esc":      promptEmpty, // plan
		"lc5-t5-suggestion-mode4-back.esc": promptEmpty,
		"lc5-t7-after-turn2.esc":           promptEmpty, // ❯ commit it
		"lc5-t7-commit-it-manual.esc":      promptEmpty,
		"lc5-t7-commit-it-plan.esc":        promptEmpty,
		"lc5-empty-prompt-auto.esc":        promptEmpty,
		"lc5-empty-prompt-plan.esc":        promptEmpty,
		"lc5-stophook-footer-2.esc":        promptEmpty,
		"lc5-stophook-after-turn.esc":      promptEmpty,
		"lc5-draft-one-word.esc":           promptDraft,
		"lc5-draft-equals-suggestion.esc":  promptDraft,   // "push it" typed: no SGR 2
		"lc5-draft-multi-line.esc":         promptUnknown, // refused: see the plain capture
		"lc5-stophook-footer-1.esc":        promptBusy,
		"lc5-f-lead-narrow-suggestion.esc": promptManage, // a suggestion, but teammates are listed
		"lc5-survey-idle-t1.esc":           promptSurvey,
	}
	files, err := filepath.Glob(filepath.Join("testdata", "prompt", "*.esc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, ok := cases[filepath.Base(f)]; !ok {
			t.Errorf("fixture %s has no entry", filepath.Base(f))
		}
	}
	for name, want := range cases {
		capt := promptCursorAtInput(t, promptFixture(t, name))
		if got := readCapture(capt); got != want {
			t.Errorf("%s: readCapture = %d, want %d", name, got, want)
		}
		// Without the escape sequences a suggestion is a draft.
		if want == promptEmpty && strings.Contains(capt.Styled, "\x1b[2m") && readPrompt(capt.Screen) != promptDraft {
			t.Errorf("%s: readPrompt = %d without escapes, want draft", name, readPrompt(capt.Screen))
		}
	}
	// The suggestion read as empty only with the cursor at the input's start.
	push := promptFixture(t, "lc5-t4-after-brief.esc")
	for _, move := range []func(*tmux.Capture){
		func(c *tmux.Capture) { c.CursorX = 9 },         // after the text, as after typing it
		func(c *tmux.Capture) { c.CursorX = 1 },         // on the space
		func(c *tmux.Capture) { c.CursorY-- },           // another row
		func(c *tmux.Capture) { c.CursorKnown = false }, // moved during the capture
		func(c *tmux.Capture) { c.Styled = c.Screen },   // no attributes at all
		func(c *tmux.Capture) { c.CursorY = len(push) }, // off the screen
	} {
		capt := promptCursorAtInput(t, push)
		move(&capt)
		if got := readCapture(capt); got != promptDraft {
			t.Errorf("suggestion with cursor %d,%d known %v: readCapture = %d, want draft", capt.CursorX, capt.CursorY, capt.CursorKnown, got)
		}
	}
	// A draft with a dim completion glued to it, built from the captured line.
	glued := strings.Replace(push, "\x1b[39m❯\u00a0\x1b[2mpush it\x1b[0m", "\x1b[39m❯\u00a0pu\x1b[2msh it\x1b[0m", 1)
	if glued == push {
		t.Fatal("the captured suggestion line was not found")
	}
	if got := readCapture(promptCursorAtInput(t, glued)); got != promptDraft {
		t.Errorf("draft with a dim completion: readCapture = %d, want draft", got)
	}
}

// promptPlaceholder is a state machine over the input line's SGR sequences:
// only dim, known cells after "❯", with the cursor after "❯ ".
func TestPromptPlaceholder(t *testing.T) {
	const at = placeholderColumn
	for _, c := range []struct {
		name   string
		line   string
		cursor int
		want   bool
	}{
		{"captured suggestion", "\x1b[39m❯\u00a0\x1b[2mpush it\x1b[0m", at, true},
		{"dim and color", "❯ \x1b[2;38;5;246mcommit it\x1b[0m", at, true},
		{"truecolor after dim", "❯ \x1b[2m\x1b[38;2;10;20;30mx\x1b[0m", at, true},
		{"empty", "\x1b[39m❯\u00a0", at, false},
		{"typed draft", "\x1b[39m❯\u00a0push it", at, false},
		{"draft then dim", "❯ pu\x1b[2msh it\x1b[0m", at, false},
		{"dim then plain", "❯ \x1b[2mpush\x1b[0m it", at, false},
		{"dim ended by 22", "❯ \x1b[2mpush\x1b[22m it", at, false},
		{"38;2 carries a 2 that is no dim", "❯ \x1b[38;2;2;2;2mpush it\x1b[0m", at, false},
		{"38;5;2 is a color", "❯ \x1b[38;5;2mpush it", at, false},
		{"48;2 too", "❯ \x1b[48;2;0;0;2mpush it", at, false},
		{"bold is unknown", "❯ \x1b[2;1mpush it", at, false},
		{"inverse is unknown", "❯ \x1b[2m\x1b[7mp\x1b[27mush", at, false},
		{"sub-parameters", "❯ \x1b[38:5:2mx", at, false},
		{"another sequence", "❯ \x1b[2m\x1b[Kpush it", at, false},
		{"hyperlink", "❯ \x1b[2m\x1b]8;;x\x1b\\push\x1b]8;;\x1b\\", at, false},
		{"cut color", "❯ \x1b[38;5m\x1b[2mpush", at, false},
		{"color over 255", "❯ \x1b[2;38;5;300mpush", at, false},
		{"no prompt mark", "\x1b[2mpush it", at, false},
		{"text before the mark", "x❯ \x1b[2mpush it", at, false},
		{"cursor after the text", "❯ \x1b[2mpush it\x1b[0m", 9, false},
		{"cursor on the mark", "❯ \x1b[2mpush it\x1b[0m", 0, false},
		{"control character", "❯ \x1b[2mpush\x07it", at, false},
		// A paste's or an image's placeholder is the user's draft, dim or
		// not, with the cursor at the input's start or after it.
		{"dim pasted text", "❯ \x1b[2m[Pasted text #1 +12 lines]\x1b[0m", at, false},
		{"dim pasted text, cursor at the end", "❯ \x1b[2m[Pasted text #1 +12 lines]\x1b[0m", 28, false},
		{"plain pasted text", "❯ [Pasted text #2 +3 lines]", at, false},
		{"plain pasted text, cursor at the end", "❯ [Pasted text #2 +3 lines]", 27, false},
		{"dim image", "❯ \x1b[2m[Image #1]\x1b[0m", at, false},
		{"dim image, cursor at the end", "❯ \x1b[2m[Image #1]\x1b[0m", 12, false},
		{"plain image", "❯ [Image #3]", at, false},
		{"dim truncated text", "❯ \x1b[2m[...Truncated text #1 +400 lines...]\x1b[0m", at, false},
		{"dim placeholder after a suggestion", "❯ \x1b[2mlook at [Image #1]\x1b[0m", at, false},
		{"a suggestion that names an image", "❯ \x1b[2mopen the image\x1b[0m", at, true},
	} {
		if got := promptPlaceholder(c.line, c.cursor); got != c.want {
			t.Errorf("%s: promptPlaceholder(%q, %d) = %v, want %v", c.name, c.line, c.cursor, got, c.want)
		}
	}
}

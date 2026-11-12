package deliver

import (
	"regexp"
	"strings"
	"unicode"
)

// promptState is what a capture of an agent's pane shows about typing into
// it (Ruling 85). Only promptEmpty allows typing; every other state, and
// anything the rules below do not recognize, means do not type.
type promptState int

const (
	// promptUnknown: the screen is not Claude Code's input prompt as these
	// rules know it (another program, a Claude Code UI change, a list below
	// the input box). Never typed into.
	promptUnknown promptState = iota
	// promptEmpty: Claude Code's input prompt, empty, with no dialog, menu
	// or running turn on screen.
	promptEmpty
	// promptDraft: the input prompt holds text the user typed (or Claude
	// Code put back after an interrupt), which a typed line would be glued
	// to and submitted with.
	promptDraft
	// promptDialog: a permission dialog or a menu (such as Rewind) is open,
	// where a typed digit or Enter would choose an option.
	promptDialog
	// promptBusy: a turn is running.
	promptBusy
	// promptSubagents: the main agent waits for background subagents, one
	// of which may open a permission dialog at any moment.
	promptSubagents
)

// The parts of Claude Code's screen these rules read, as captured from
// Claude Code 2.1.323 (live check 2; fixtures in testdata/prompt). The
// input box is a rule line, one line starting "❯" (an empty prompt is "❯"
// and a no-break space), another rule line, then at most two footer lines.
const (
	promptMark   = "❯"
	ruleRune     = '─'
	minRuleRunes = 10
	maxFooter    = 2
	// dialogScan is how many lines from the bottom are searched for a
	// dialog's or menu's hints.
	dialogScan = 20
	// aboveScan is how many non-blank lines above the box are read for a
	// status line.
	aboveScan = 3
)

// dialogHints are texts only a dialog or menu shows: the permission
// dialog's "Esc to cancel · Tab to amend", Rewind's "Enter to continue · Esc
// to cancel".
var dialogHints = []string{"Esc to cancel", "Enter to continue", "Enter to confirm", "Enter to select"}

// numberedChoice matches a dialog's highlighted numbered option, "❯ 1. Yes".
var numberedChoice = regexp.MustCompile(`^\s*❯\s*[0-9]+\.\s`)

// idleFooter matches the footer lines of an idle prompt: the permission
// mode line ("⏵⏵ auto mode on (shift+tab to cycle)", the one captured) and
// the shortcuts hint. A running turn appends " · esc to interrupt", which
// this does not match.
var idleFooter = regexp.MustCompile(`^(?:(?:⏵⏵|⏸) [a-z]+(?: [a-z]+)* on \(shift\+tab to cycle\)|\? for shortcuts)$`)

// readPrompt says what screen, a capture of an agent's pane, shows about
// typing into it. It is strict: the input box must be the last thing on
// screen, empty, with only a known idle footer under it.
func readPrompt(screen string) promptState {
	lines := screenLines(screen)
	if hasDialog(lines) {
		return promptDialog
	}
	bottom := lastRule(lines, len(lines))
	if bottom < 0 || len(lines)-1-bottom > maxFooter {
		return promptUnknown
	}
	top := lastRule(lines, bottom)
	if top < 0 || top == bottom-1 {
		return promptUnknown
	}
	if s := footerState(lines[bottom+1:]); s != promptEmpty {
		return s
	}
	if s := inputState(lines[top+1 : bottom]); s != promptEmpty {
		return s
	}
	if waitingForSubagents(lines[:top]) {
		return promptSubagents
	}
	return promptEmpty
}

// screenLines splits screen into lines with trailing white space (the
// no-break space after "❯" included) removed, dropping the blank lines at
// the bottom.
func screenLines(screen string) []string {
	lines := strings.Split(screen, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// hasDialog reports whether the bottom of the screen shows a dialog's or a
// menu's hints or numbered choices.
func hasDialog(lines []string) bool {
	for _, l := range lines[max(0, len(lines)-dialogScan):] {
		if numberedChoice.MatchString(l) {
			return true
		}
		for _, h := range dialogHints {
			if strings.Contains(l, h) {
				return true
			}
		}
	}
	return false
}

// lastRule is the index of the last rule line before index end, or -1.
func lastRule(lines []string, end int) int {
	for i := end - 1; i >= 0; i-- {
		if isRule(lines[i]) {
			return i
		}
	}
	return -1
}

// isRule reports whether l is a horizontal rule of the input box.
func isRule(l string) bool {
	n := 0
	for _, r := range strings.TrimSpace(l) {
		if r != ruleRune {
			return false
		}
		n++
	}
	return n >= minRuleRunes
}

// footerState reads the lines under the input box: promptEmpty when each is
// a known idle footer.
func footerState(footer []string) promptState {
	for _, l := range footer {
		l = strings.TrimSpace(l)
		switch {
		case strings.Contains(l, "esc to interrupt"):
			return promptBusy
		case strings.Contains(l, "Esc again to clear"):
			return promptDraft // shown only while the prompt holds a draft
		case l != "" && !idleFooter.MatchString(l):
			return promptUnknown
		}
	}
	return promptEmpty
}

// inputState reads the lines inside the input box: promptEmpty for the one
// line "❯" and nothing else.
func inputState(input []string) promptState {
	first, ok := strings.CutPrefix(input[0], promptMark)
	switch {
	case !ok:
		return promptUnknown // another input mode, or not Claude Code
	case len(input) > 1 || strings.TrimSpace(first) != "":
		return promptDraft
	}
	return promptEmpty
}

// waitingForSubagents reports whether a status line just above the input
// box says the main agent waits for background subagents.
func waitingForSubagents(above []string) bool {
	seen := 0
	for i := len(above) - 1; i >= 0 && seen < aboveScan; i-- {
		if above[i] == "" {
			continue
		}
		seen++
		if strings.Contains(above[i], "Waiting for") && strings.Contains(above[i], "background agent") {
			return true
		}
	}
	return false
}

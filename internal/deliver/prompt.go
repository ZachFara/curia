package deliver

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/tmux"
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
	// promptAgents: the footer offers background agents ("← for agents").
	promptAgents
	// promptManage: background agents, or a live team's teammates, are
	// listed under the box ("↓ to manage"). Nothing is typed to a team lead
	// while its teammates are alive.
	promptManage
	// promptShell: the pane shows a shell prompt: the session's Claude Code
	// has exited or died.
	promptShell
	// promptSurvey: Claude Code's feedback survey ("1: Bad  2: Fine ...")
	// is above the input box, where a typed digit would answer it.
	promptSurvey
)

// The parts of Claude Code's screen these rules read, as captured from
// Claude Code 2.1.323 (live checks 2 and 3) and 2.1.325 (Plan 3's live
// probe; fixtures in testdata/prompt).
// The input box is a rule line, one line starting "❯" (an empty prompt is
// "❯" and a no-break space), another rule line, then the footer.
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

// dialogHints are texts only a dialog, menu or full-screen view shows, as
// captured: the permission dialog's and /help's "Esc to cancel", Rewind's
// "Enter to continue", the trust dialog's "Enter to confirm", /config's
// "Esc to clear" (filtering) and "Esc to close".
var dialogHints = []string{
	"Esc to cancel", "Enter to continue", "Enter to confirm", "Enter to select", "Esc to clear", "Esc to close",
}

// offerHints and manageHints are what the footer shows when it offers or
// lists background agents: "← for agents" on a fresh or resumed session,
// "↓ to manage" with background agents or a team's teammates listed under
// the input box (live checks 3 and 4). They are read under the box only
// (underBox).
var (
	offerHints  = []string{"← for agents"}
	manageHints = []string{"↓ to manage"}
)

// shellPrompt matches the last line of a pane that shows a shell prompt,
// such as "host%", "sh-3.2$" or "$": Claude Code has exited or died there.
var shellPrompt = regexp.MustCompile(`^[\w.@:~/-]*[%$#]$`)

// numberedChoice matches a dialog's highlighted numbered option, "❯ 1. Yes".
var numberedChoice = regexp.MustCompile(`^\s*❯\s*[0-9]+\.\s`)

// idleFooters are the footer lines of an idle prompt, one per permission
// mode, exactly as captured at 60 to 320 columns (live check 3), and each
// mode's with the " · /tasks to see subagents" that Claude Code 2.1.325
// shows for about 30 s after a subagent ran (Ruling 101; live check A
// captured all four, LA3: in manual mode it replaces "? for shortcuts").
// Nothing else is accepted: a running turn appends " · esc to interrupt", a
// fresh session " · ← for agents", running agents or a live team " · ↓ to
// manage", tmux a hint about focus events, and a narrow pane cuts the line
// short.
var idleFooters = []string{
	"⏵⏵ auto mode on (shift+tab to cycle)",
	"⏵⏵ accept edits on (shift+tab to cycle)",
	"⏸ plan mode on (shift+tab to cycle)",
	"⏸ manual mode on · ? for shortcuts",
	"⏵⏵ auto mode on (shift+tab to cycle) · /tasks to see subagents",
	"⏵⏵ accept edits on (shift+tab to cycle) · /tasks to see subagents",
	"⏸ plan mode on (shift+tab to cycle) · /tasks to see subagents",
	"⏸ manual mode on · /tasks to see subagents",
}

// stopHookFooter is the second footer line Claude Code shows for a few
// seconds after a Stop hook blocked the stop (Curia's delivery), right
// aligned under an idle footer (live checks A and B, LA4). It changes
// nothing about the prompt.
const stopHookFooter = "Stop hook error occurred · ctrl+o to see"

// surveyChoice matches one choice of Claude Code's feedback survey, "1: Bad",
// as captured by live check B ("  1: Bad    2: Fine   3: Good   0: Dismiss").
var surveyChoice = regexp.MustCompile(`(^|\s)[0-9]: \S`)

// agentListed matches a line of the agent list Claude Code draws under the
// input box for background agents or a team's teammates ("⏺ main",
// "◯ alpha"), which a narrow pane shows without its "↓ to manage" hint
// (live check B, defect 3).
var agentListed = regexp.MustCompile(`^\s*(⏺ main$|◯ \S)`)

// readCapture says what capt, a capture of an agent's pane, shows about
// typing into it (readScreen), and also reads a dim suggestion in
// the input as empty (Ruling 110): the input line holds only the dim cells
// of a suggestion (promptPlaceholder) and the cursor sits on that line just
// after "❯ ". Anything else in the input is a draft, never typed over.
func readCapture(capt tmux.Capture) promptState {
	styled := strings.Split(capt.Styled, "\n")
	return readScreen(capt.Screen, func(row int) bool {
		return capt.CursorKnown && capt.CursorY == row && row < len(styled) && promptPlaceholder(styled[row], capt.CursorX)
	})
}

// readScreen says what screen, a plain capture of an agent's pane, shows
// about typing into it. It is strict: the input box must be the last thing
// on screen, empty, with only a known idle footer under it. placeholder,
// when not nil, says whether the one-line input at row (counted from 0) is
// only a dim suggestion, which then reads as empty.
func readScreen(screen string, placeholder func(row int) bool) promptState {
	lines := screenLines(screen)
	if s, ok := screenWide(lines); ok {
		return s
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
	s := inputState(lines[top+1 : bottom])
	if s == promptDraft && bottom == top+2 && placeholder != nil && placeholder(top+1) {
		s = promptEmpty
	}
	if s != promptEmpty {
		return s
	}
	switch {
	case waitingForSubagents(lines[:top]):
		return promptSubagents
	case surveyAbove(lines[:top]):
		return promptSurvey
	}
	return promptEmpty
}

// screenWide is what the whole screen says before the input box is read:
// a shell prompt on the last line, a dialog's or menu's hint or numbered
// choice, or agents listed or offered under the box.
func screenWide(lines []string) (promptState, bool) {
	switch {
	case len(lines) > 0 && shellPrompt.MatchString(strings.TrimSpace(lines[len(lines)-1])):
		return promptShell, true
	case hasHint(lines, dialogHints) || hasNumberedChoice(lines):
		return promptDialog, true
	case hasHint(underBox(lines), manageHints) || slices.ContainsFunc(underBox(lines), agentListed.MatchString):
		return promptManage, true
	case hasHint(underBox(lines), offerHints):
		return promptAgents, true
	}
	return promptEmpty, false
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

// hasHint reports whether the bottom of the screen holds one of hints.
func hasHint(lines, hints []string) bool {
	for _, l := range lines[max(0, len(lines)-dialogScan):] {
		for _, h := range hints {
			if strings.Contains(l, h) {
				return true
			}
		}
	}
	return false
}

// underBox is the lines after the last rule line: the footer, and whatever
// Claude Code lists under it. The transcript above the box can quote any
// hint, so only these are read for the agents' hints.
func underBox(lines []string) []string {
	if bottom := lastRule(lines, len(lines)); bottom >= 0 {
		return lines[bottom+1:]
	}
	return nil
}

// hasNumberedChoice reports whether the bottom of the screen shows a
// dialog's highlighted numbered option.
func hasNumberedChoice(lines []string) bool {
	return slices.ContainsFunc(lines[max(0, len(lines)-dialogScan):], numberedChoice.MatchString)
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

// footerState reads the lines under the input box: promptEmpty when they
// are exactly one known idle footer line.
func footerState(footer []string) promptState {
	idle := 0
	for _, l := range footer {
		l = strings.TrimSpace(l)
		switch {
		case strings.Contains(l, "esc to interrupt"):
			return promptBusy
		case strings.Contains(l, "Esc again to clear"):
			return promptDraft // shown only while the prompt holds a draft
		case slices.Contains(idleFooters, l):
			idle++
		case l == stopHookFooter:
		case l != "":
			return promptUnknown
		}
	}
	if idle != 1 { // every captured idle prompt has exactly one footer line
		return promptUnknown
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

// surveyAbove reports whether a line just above the input box offers
// Claude Code's feedback survey: two or more digit choices.
func surveyAbove(above []string) bool {
	seen := 0
	for i := len(above) - 1; i >= 0 && seen < aboveScan; i-- {
		if above[i] == "" {
			continue
		}
		seen++
		if len(surveyChoice.FindAllStringIndex(above[i], -1)) >= 2 {
			return true
		}
	}
	return false
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

// placeholderColumn is the cursor column of an empty input: after "❯" and
// the no-break space that follows it, where typed text starts.
const placeholderColumn = 2

// promptPlaceholder reports whether line, the input line of a capture with
// escape sequences (capture-pane -e), shows only a suggestion Claude Code
// draws dim in an empty prompt, with the cursor at cursorX (Ruling 110).
// It is the only reading that lets text in the input be typed over, so it
// is strict, and false means a draft: the line must start with "❯" (after
// SGR sequences only); every non-blank cell after it must be dim (SGR 2,
// set on this line: dim carried over from an earlier line does not count)
// and there must be one; the cursor must be at placeholderColumn; and every
// escape sequence must be an SGR this function knows, read as a state
// machine (the numbers after 38;5, 38;2, 48;5 and 48;2 are colors, not
// attributes). Anything else, an unknown attribute or sequence included,
// is false.
func promptPlaceholder(line string, cursorX int) bool {
	if cursorX != placeholderColumn {
		return false
	}
	rest, ok := skipSGR(line)
	if !ok {
		return false
	}
	rest, ok = strings.CutPrefix(rest, promptMark)
	if !ok {
		return false
	}
	dim, cells := false, 0
	for rest != "" {
		if rest[0] == '\x1b' {
			n, d, known := sgr(rest, dim)
			if !known {
				return false
			}
			rest, dim = rest[n:], d
			continue
		}
		r, n := utf8.DecodeRuneInString(rest)
		rest = rest[n:]
		if unicode.IsSpace(r) {
			continue
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) || !dim {
			return false
		}
		cells++
	}
	return cells > 0
}

// skipSGR skips the known SGR sequences at the start of s; false when one
// is not known.
func skipSGR(s string) (string, bool) {
	for strings.HasPrefix(s, "\x1b") {
		n, _, known := sgr(s, false)
		if !known {
			return "", false
		}
		s = s[n:]
	}
	return s, true
}

// sgr reads the SGR sequence at the start of s ("\x1b[" params "m") with
// dim the state before it, and returns its length, the dim state after it
// and whether every parameter is known: 0 (and an empty list) resets, 2 sets
// dim, 22 ends it, 39 and 49 are the default colors, 30 to 37, 40 to 47, 90
// to 97 and 100 to 107 are colors, and 38 and 48 take 5;n or 2;r;g;b.
func sgr(s string, dim bool) (int, bool, bool) {
	end := strings.IndexByte(s, 'm')
	if !strings.HasPrefix(s, "\x1b[") || end < 0 {
		return 0, dim, false
	}
	body := s[2:end]
	if strings.Trim(body, "0123456789;") != "" {
		return 0, dim, false // ':' sub-parameters, private markers, another final byte
	}
	params := strings.Split(body, ";")
	for i := 0; i < len(params); i++ {
		switch p := params[i]; {
		case p == "" || p == "0":
			dim = false
		case p == "2":
			dim = true
		case p == "22":
			dim = false
		case p == "38" || p == "48":
			n := colorArgs(params[i+1:])
			if n < 0 {
				return 0, dim, false
			}
			i += n
		case !plainColor(p):
			return 0, dim, false
		}
	}
	return end + 1, dim, true
}

// colorArgs is how many parameters after 38 or 48 a color takes (5;n or
// 2;r;g;b, each a number up to 255), or -1.
func colorArgs(rest []string) int {
	want := 0
	switch {
	case len(rest) >= 2 && rest[0] == "5":
		want = 2
	case len(rest) >= 4 && rest[0] == "2":
		want = 4
	default:
		return -1
	}
	for _, v := range rest[1:want] {
		n, err := strconv.Atoi(v)
		if err != nil || n > 255 {
			return -1
		}
	}
	return want
}

// plainColor reports whether p is a basic color or a default color.
func plainColor(p string) bool {
	n, err := strconv.Atoi(p)
	if err != nil {
		return false
	}
	return n == 39 || n == 49 || (n >= 30 && n <= 37) || (n >= 40 && n <= 47) || (n >= 90 && n <= 97) || (n >= 100 && n <= 107)
}

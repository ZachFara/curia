package install

import (
	"fmt"
	"path/filepath"
	"time"
)

// StatusLineState is what StatusLine found or did.
type StatusLineState int

const (
	// StatusLineAdded means Curia's status line was written.
	StatusLineAdded StatusLineState = iota
	// StatusLineOurs means it was already there.
	StatusLineOurs
	// StatusLineForeign means the user has their own; nothing was written.
	StatusLineForeign
	// StatusLineUpdated means Curia's entry named another path for this
	// binary (isOurStatusLine) and now names bin.
	StatusLineUpdated
)

// statusLineKey is the Claude Code settings key of the status line.
const statusLineKey = "statusLine"

// statusLineArgs is what follows the program in Curia's status-line command.
const statusLineArgs = " statusline"

// StatusLineCommand is the command Curia registers for bin.
func StatusLineCommand(bin string) string {
	return shellQuote(bin) + statusLineArgs
}

// StatusLineChain is the command a user with a status line of their own can
// set instead of it, with "<your command>" standing for theirs: Curia's
// ingest and their command each read the whole input. It needs only a POSIX
// sh (no process substitution, which /bin/sh on macOS and dash refuse).
func StatusLineChain(bin string) string {
	return `input=$(cat); printf '%s' "$input" | ` + shellQuote(bin) +
		` ingest --statusline; printf '%s' "$input" | <your command>`
}

// StatusLine registers Curia's status-line command in settingsPath unless a
// statusLine is already set. A missing file is created with the one key. A
// statusLine that is Curia's for bin is left as it is (StatusLineOurs), one
// that is Curia's for another path of this binary gets bin's command
// (StatusLineUpdated), and anything else, a value of another type included,
// is the user's and left untouched (StatusLineForeign). Only Added and
// Updated write, with the usual backup; on an error nothing was registered.
func StatusLine(settingsPath, bin string, now time.Time) (StatusLineState, error) {
	settingsPath = filepath.Clean(settingsPath)
	raw, settings, err := loadSettings(settingsPath)
	if err != nil {
		return StatusLineForeign, err
	}
	v, present := settings[statusLineKey]
	var state StatusLineState
	switch entry, isObject := v.(map[string]any); {
	case !present:
		settings[statusLineKey] = map[string]any{"type": "command", "command": StatusLineCommand(bin)}
		state = StatusLineAdded
	case isCurrentStatusLine(v, bin):
		return StatusLineOurs, nil
	case isObject && isOurStatusLine(v, bin):
		entry["command"] = StatusLineCommand(bin)
		state = StatusLineUpdated
	default:
		return StatusLineForeign, nil
	}
	if err := saveSettings(settingsPath, raw, settings, now); err != nil {
		return StatusLineForeign, fmt.Errorf("%s: %w", settingsPath, err)
	}
	return state, nil
}

// RemoveStatusLine deletes the statusLine key when its command is Curia's
// for bin (isOurStatusLine), and reports whether it did. A missing file, or
// a statusLine of any other kind, is left alone.
func RemoveStatusLine(settingsPath, bin string) (bool, error) {
	settingsPath = filepath.Clean(settingsPath)
	raw, settings, err := loadSettings(settingsPath)
	if err != nil {
		return false, err
	}
	if raw == nil || !isOurStatusLine(settings[statusLineKey], bin) {
		return false, nil
	}
	delete(settings, statusLineKey)
	if err := saveSettings(settingsPath, raw, settings, time.Now()); err != nil {
		return false, fmt.Errorf("%s: %w", settingsPath, err)
	}
	return true, nil
}

// statusLineCommandOf returns v's command when v is an object whose type is
// "command" and whose command is a string.
func statusLineCommandOf(v any) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || m["type"] != "command" {
		return "", false
	}
	cmd, ok := m["command"].(string)
	return cmd, ok
}

// isCurrentStatusLine reports whether v is exactly what StatusLine writes for
// bin, other keys of the object aside.
func isCurrentStatusLine(v any, bin string) bool {
	cmd, ok := statusLineCommandOf(v)
	return ok && cmd == StatusLineCommand(bin)
}

// isOurStatusLine reports whether v is Curia's status line for this binary,
// by the rule isOurs applies to hook commands: bin's command, or exactly
// "<program> statusline" where ourBinary accepts program.
func isOurStatusLine(v any, bin string) bool {
	if isCurrentStatusLine(v, bin) {
		return true
	}
	cmd, ok := statusLineCommandOf(v)
	if !ok {
		return false
	}
	word, rest, ok := splitCommandWord(cmd)
	return ok && rest == statusLineArgs && ourBinary(word, bin)
}

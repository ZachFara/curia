// Package message holds the pure rules of a message: its text, the address
// at its start, its id, and the fixed wording it is delivered in (plan
// sections 4.2, 4.9 and 4.10).
package message

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Limits and fixed words of a message (plan section 4.9).
const (
	MaxText    = 800 // runes of a message
	MaxTyped   = 600 // runes of a typed line
	typedText  = 400 // runes of the text inside a typed line
	StopHeader = "Curia: messages for you."
	MaxPerStop = 8
)

// ErrBadText is returned by Normalize for text that is empty or too long
// once cleaned.
var ErrBadText = errors.New("message: text must be 1 to 800 characters")

// maxName is the longest name an "@name" address part may hold.
const maxName = 64

// ValidID reports whether id is a message id, ^M-[1-9][0-9]{0,8}$.
func ValidID(id string) bool {
	return validID(id, "M-")
}

// validID reports whether id is prefix followed by 1 to 9 digits, the first
// not 0 (plan section 4.2).
func validID(id, prefix string) bool {
	n, ok := strings.CutPrefix(id, prefix)
	if !ok || n == "" || len(n) > 9 || n[0] == '0' {
		return false
	}
	for i := range len(n) {
		if n[i] < '0' || n[i] > '9' {
			return false
		}
	}
	return true
}

// Normalize cleans a message's text with sanitize.Text, drops blank lines
// around it, and returns ErrBadText unless 1 to MaxText runes are left.
func Normalize(text string) (string, error) {
	// One rune over the limit is enough to tell that the text is too long.
	s := strings.Trim(sanitize.Text(text, MaxText+1), "\n")
	if n := utf8.RuneCountInString(s); n < 1 || n > MaxText {
		return "", ErrBadText
	}
	return s, nil
}

// Address is what the start of a message says about where it goes.
type Address struct {
	TaskID  string            // from "T-42:"
	Name    string            // from "@name"
	Kind    model.MessageKind // from "rule:" or "task:"
	HasKind bool
}

// ParseAddress reads the address parts off the start of text and returns the
// rest, trimmed. A part is one whole word, ended by white space or the end
// of the text: "T-42:" (a valid task id and a colon), "@name" with name of 1
// to 64 of [A-Za-z0-9._/-] and an optional colon, or "rule:" or "task:" in
// any case. Each part is read at most once, in any order; the first word
// that is no part, or repeats one already read, starts the rest.
func ParseAddress(text string) (Address, string) {
	var a Address
	rest := strings.TrimSpace(text)
	for rest != "" {
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			end = len(rest)
		}
		if !a.take(rest[:end]) {
			break
		}
		rest = strings.TrimSpace(rest[end:])
	}
	return a, rest
}

// take records word in a when it is an address part a does not hold yet,
// and reports whether it did.
func (a *Address) take(word string) bool {
	if id, ok := strings.CutSuffix(word, ":"); ok && a.TaskID == "" && validID(id, "T-") {
		a.TaskID = id
		return true
	}
	if name, ok := strings.CutPrefix(word, "@"); ok && a.Name == "" {
		name = strings.TrimSuffix(name, ":")
		if !validName(name) {
			return false
		}
		a.Name = name
		return true
	}
	if a.HasKind {
		return false
	}
	switch strings.ToLower(word) {
	case "rule:":
		a.Kind, a.HasKind = model.KindRule, true
	case "task:":
		a.Kind, a.HasKind = model.KindTask, true
	default:
		return false
	}
	return true
}

// validName reports whether name matches [A-Za-z0-9._/-]{1,64}.
func validName(name string) bool {
	if name == "" || len(name) > maxName {
		return false
	}
	for i := range len(name) {
		c := name[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '/' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// agentPrompts are the starts of prompts that Claude Code builds itself,
// from a background task or another agent (Ruling 102).
var agentPrompts = []string{"<task-notification", "<agent-message"}

// AgentBuilt reports whether prompt is one Claude Code built itself: it
// starts, after white space, with "<task-notification" (a background task's
// notification) or "<agent-message" (a subagent's report). Such a prompt
// confirms no typed line, neither a message nor an answer, whatever it
// quotes (Ruling 102); this is the one list of those starts.
func AgentBuilt(prompt string) bool {
	start := strings.TrimLeftFunc(prompt, unicode.IsSpace)
	for _, p := range agentPrompts {
		if strings.HasPrefix(start, p) {
			return true
		}
	}
	return false
}

// Names reports whether prompt holds the typed line of message id: "Message
// <id> " (the id, then a space), so M-1 is not taken for M-12. An id that is
// not valid, or a prompt Claude Code built itself (AgentBuilt), names
// nothing.
func Names(prompt, id string) bool {
	if !ValidID(id) || AgentBuilt(prompt) {
		return false
	}
	return strings.Contains(prompt, "Message "+id+" ")
}

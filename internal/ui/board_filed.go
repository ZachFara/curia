package ui

import (
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

// Bounds, in cells, of the untrusted filer and reason on a card.
const (
	maxFilerCells = 24
	maxWhyCells   = 60
)

// cardFiledTag is "[agent <session>] " for a task an agent filed, else "".
func cardFiledTag(c task.Card) string {
	if c.Task.FiledBy == "" {
		return ""
	}
	return "[agent " + sanitize.Line(c.Task.FiledBy, maxFilerCells) + "] "
}

// cardFiledWhy is "  (why: <reason>)" for a task an agent filed, else "".
func cardFiledWhy(c task.Card) string {
	if c.Task.FiledBy == "" {
		return ""
	}
	return "  (why: " + sanitize.Line(c.Task.FiledReason, maxWhyCells) + ")"
}

// Package model defines the types shared by every Curia package.
package model

import "time"

// State is the status of one agent session.
type State string

// Session states.
const (
	StateWorking       State = "working"
	StateWaiting       State = "waiting"
	StateNeedsDecision State = "needs_decision"
	StateIdle          State = "idle"
	StateEnded         State = "ended"
	StateStale         State = "stale"
)

// EventKind is the name of a Claude Code hook event.
type EventKind string

// Hook event kinds, one per Claude Code hook name.
const (
	SessionStart     EventKind = "SessionStart"
	UserPromptSubmit EventKind = "UserPromptSubmit"
	PreToolUse       EventKind = "PreToolUse"
	PostToolUse      EventKind = "PostToolUse"
	Notification     EventKind = "Notification"
	Stop             EventKind = "Stop"
	SubagentStop     EventKind = "SubagentStop"
	SessionEnd       EventKind = "SessionEnd"
)

// Session is one row of the store: an agent session and its latest state.
type Session struct {
	ID, Agent, Pane, TmuxSession, TmuxWindow, Cwd, Repo, Branch, Model, LastUpdate, TranscriptPath string
	State                                                                                          State
	ContextUsed, ContextMax, SubagentsActive                                                       int
	TranscriptOffset                                                                               int64
	StartedAt, UpdatedAt, SeenAt                                                                   time.Time
}

// Event is a parsed hook payload.
type Event struct {
	Kind                                                                EventKind
	SessionID, Pane, Cwd, TranscriptPath, ToolName, ToolTarget, Message string
	At                                                                  time.Time
}

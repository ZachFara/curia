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
	// TaskID is the task whose agent this is; Worktree its worktree. TeamID,
	// TeamRole and TeamMember place the session in an agent team.
	TaskID, Worktree, TeamID, TeamRole, TeamMember string
}

// Team roles.
const (
	RoleLead     = "lead"
	RoleTeammate = "teammate"
)

// Event is a parsed hook payload. NotificationType is a Notification's
// notification_type (for example "permission_prompt" or "idle_prompt").
// LastAssistantMessage is a Stop's last_assistant_message: Claude's final
// answer, which the transcript may not hold yet when Stop fires. Model is a
// SessionStart's model, empty when the payload has none. Source is a
// SessionStart's source ("startup", "resume", "clear" or "compact").
// StopBlocked is set by the hook on a Stop it is about to block.
// HelperAgent marks a SubagentStop of one of Claude Code's own helper agents
// (agent_type present and empty), which no Agent or Task tool call started
// (Ruling 87). On a Stop, RunningSubagents counts the subagents its
// background_tasks lists as running, when BackgroundKnown says the payload
// has that list. Prompt is a UserPromptSubmit's prompt.
type Event struct {
	Kind                                                                                  EventKind
	SessionID, Pane, Cwd, TranscriptPath, ToolName, ToolTarget, Message, NotificationType string
	LastAssistantMessage, Model, Source, Prompt                                           string
	StopBlocked, HelperAgent, BackgroundKnown                                             bool
	RunningSubagents                                                                      int
	At                                                                                    time.Time
	// TaskID is the task named by the hook's environment (CURIA_TASK_ID).
	TaskID string
}

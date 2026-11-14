package model

import "time"

// TaskStatus is the status of a task.
type TaskStatus string

// Task statuses. Only backlog, ready and dropped are stored as such; the
// others are derived.
const (
	TaskBacklog       TaskStatus = "backlog"
	TaskReady         TaskStatus = "ready"
	TaskRunning       TaskStatus = "running"
	TaskNeedsDecision TaskStatus = "needs_decision"
	TaskReview        TaskStatus = "review"
	TaskDone          TaskStatus = "done"
	TaskDropped       TaskStatus = "dropped"
)

// TaskSource says where a task came from.
type TaskSource string

// Task sources.
const (
	SourceLocal     TaskSource = "local"
	SourceBacklogMD TaskSource = "backlog_md"
)

// Task is one row of the tasks table.
type Task struct {
	N                                                     int64
	ID, Title, Body, Repo, BaseBranch                     string
	Status                                                TaskStatus // as stored: backlog, ready or dropped
	Source                                                TaskSource
	SourceRef                                             string
	Tags                                                  []string
	Branch, Worktree, AgentModel, Pane, ResumeSession     string
	Team                                                  bool
	CreatedAt, UpdatedAt, LaunchedAt, PausedAt, CleanedAt time.Time
}

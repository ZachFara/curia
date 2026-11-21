package model

import "time"

// UsageWindow names an account-wide usage window.
type UsageWindow string

// Usage windows.
const (
	WindowFiveHour UsageWindow = "five_hour"
	WindowSevenDay UsageWindow = "seven_day"
)

// UsageSample is one reading of an account-wide usage window.
type UsageSample struct {
	N         int64
	At        time.Time
	Window    UsageWindow
	UsedPct   float64
	ResetsAt  time.Time // zero: the reading had none
	SessionID string
}

// CostSample is one reading of a session's running cost.
type CostSample struct {
	N         int64
	At        time.Time
	SessionID string
	CostUSD   float64
}

// Task priorities.
const (
	PriorityHigh   = 1 // P1
	PriorityNormal = 2 // P2, the default
	PriorityLow    = 3 // P3
)

// Agent names.
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

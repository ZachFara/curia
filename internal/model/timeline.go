package model

import "time"

// TimelineEvent is one line of the F25 timeline: something that happened,
// read from an existing row. Line is the stored text, bounded but not yet
// cleaned: a view cleans it before it shows it.
type TimelineEvent struct {
	At        time.Time
	Ref       string // the source row's id as text; with At and Kind it is a stable position
	SessionID string // "" when the event has no session
	TaskID    string // "" when the event has no task
	Kind      string // session_start, turn_end, answered, review_opened, ...
	Line      string
}

// DecisionLogRow is one decision of the F25 decision log with its reuse.
type DecisionLogRow struct {
	ID, Question, Answer, Scope, Repo string
	Status                            DecisionStatus
	CreatedAt, AnsweredAt             time.Time
	Superseded                        bool
	Reuse                             int       // decisions whose precedent_of is this one
	LastReuse                         time.Time // zero when never reused
	AskedAgain                        int       // tickets raised although this was their near_match
}

// DecisionLogSummary is the log's one summary line: how many of the
// answered decisions (superseded ones excluded) were reused at least once.
type DecisionLogSummary struct{ Answered, Reused int }

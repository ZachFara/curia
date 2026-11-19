package model

import "time"

// ReviewStatus is where a task's review is in its life.
type ReviewStatus string

// Review statuses.
const (
	ReviewPending  ReviewStatus = "pending"
	ReviewApproved ReviewStatus = "approved"
	ReviewChanges  ReviewStatus = "changes_requested"
	ReviewMerged   ReviewStatus = "merged"
	ReviewDropped  ReviewStatus = "dropped"
)

// TestsStatus is what the agent reported about its tests.
type TestsStatus string

// Tests statuses.
const (
	TestsPassed TestsStatus = "passed"
	TestsFailed TestsStatus = "failed"
	TestsNotRun TestsStatus = "not_run"
)

// ApproveHow is how an approved review reaches the base branch.
type ApproveHow string

// Approval paths.
const (
	ApproveMerge ApproveHow = "merge"
	ApprovePR    ApproveHow = "pr"
)

// MaxReviewNote bounds a change request, in runes.
const MaxReviewNote = 600

// Review is one row of the reviews table.
type Review struct {
	N                                         int64
	ID, TaskID, SessionID                     string
	Summary                                   string
	Tests                                     TestsStatus
	TestsDetail                               string
	Status                                    ReviewStatus
	ApprovedHow                               ApproveHow
	DiffStat, Head, PRURL, Notes, NoteMessage string
	ClosedVia                                 AnsweredVia
	CreatedAt, UpdatedAt, ClosedAt            time.Time
	// IntentAt is, on a pending review, when an approval recorded its intent
	// (ApprovedHow, Head and DiffStat) before running git; zero when none.
	// It is stored in closed_at, which a pending review does not otherwise use.
	IntentAt time.Time
}

// PushKind says what a push notification is about.
type PushKind string

// Push kinds.
const (
	PushDecision PushKind = "decision"
	PushReview   PushKind = "review"
)

// PushStatus is where a push is in its life.
type PushStatus string

// Push statuses.
const (
	PushPending PushStatus = "pending"
	PushSending PushStatus = "sending"
	PushSent    PushStatus = "sent"
	PushFailed  PushStatus = "failed"
	PushDropped PushStatus = "dropped"
)

// Push is one row of the pushes table.
type Push struct {
	N                                    int64
	Kind                                 PushKind
	Ref, Device                          string
	Status                               PushStatus
	NotBefore                            time.Time
	Attempts                             int
	LastError, TokenHash                 string
	CreatedAt, ClaimedAt, SentAt, UsedAt time.Time
}

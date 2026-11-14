package store

// Unexported values the external tests check.
const (
	ClaimCap         = claimCap
	DecisionColumns  = decisionColumns
	ReturnedDecision = returnedDecision
)

// The hook path's and the badge's statements, for their query plans.
const (
	StopStateQuery   = stopState
	BadgeExtrasQuery = badgeExtras
)

// The reviews' and the task views' statements, for their query plans.
const (
	PendingReviewTasksQuery    = pendingReviewTasks
	PendingReviewsQuery        = pendingReviewsCount
	LatestReviewsQuery         = latestReviews
	TaskDecisionsQuery         = taskDecisions
	TaskRunQuery               = taskRun
	ExpireTaskAssumptionsQuery = expireTaskAssumptions
	PendingReviewOfTaskQuery   = updatePendingReview
)

// ListReviewsQuery is ListReviews' statement and arguments for f.
var ListReviewsQuery = listReviewsQuery

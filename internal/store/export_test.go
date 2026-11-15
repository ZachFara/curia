package store

import (
	"context"

	"github.com/ZachFara/curia/internal/model"
)

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

// The outbox's and the restore's statements, for their query plans.
const (
	PushesDueQuery         = pushesDue
	ClaimPushPickQuery     = claimPushPick
	SettlePushesQuery      = settlePushes
	SpendablePushesQuery   = spendablePushes
	RestoreCandidatesQuery = restoreCandidates
)

// PushForTest reads push n as the store stores it.
func (s *Store) PushForTest(ctx context.Context, n int64) (model.Push, error) {
	return scanPush(s.db.QueryRowContext(ctx, selectPushes+` WHERE n = ?`, n))
}

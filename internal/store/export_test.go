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
	SendQueuedQuery  = sendQueued
	SendRecentQuery  = sendRecent
	SendSenderLive   = sendSenderLive
	UnconfirmedQuery = unconfirmedQuery
	CheckClaim       = checkClaimQuery
	CheckConfirm     = checkConfirmQuery
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

// The ingest path's and the usage readers' statements, for their query plans.
const (
	IngestSessionQuery       = ingestSession
	IngestSessionExistsQuery = ingestSessionExists
	NewestCostQuery          = newestCost
	NewestUsageQuery         = newestUsage
	PruneUsageQuery          = pruneUsage
	PruneCostQuery           = pruneCost
	PruneWalkQuery           = pruneWalkIDs
	UsageSinceQuery          = usageSince
	CostSinceQuery           = costSince
)

// PruneWalk is how many sessions' cost samples one ingest prunes.
const PruneWalk = pruneWalk

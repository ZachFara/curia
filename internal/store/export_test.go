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
	SendQueuedQuery  = sendQueued
	SendRecentQuery  = sendRecent
	SendSenderLive   = sendSenderLive
	UnconfirmedQuery = unconfirmedQuery
	CheckClaim       = checkClaimQuery
	CheckConfirm     = checkConfirmQuery
)

package main

import (
	"cmp"
	"context"
	"slices"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// inboxSource is the dashboard inbox's ui.InboxSource: a query over the
// rows the status badge counts, with no write and no tmux call (no
// reconcile and no expiry sweep, as the badge does), so its counts are the
// badge's D, W and R.
type inboxSource struct{ st *store.Store }

// Items returns the blocking tickets in decision.Triage order, the sessions
// waiting on a prompt, the pending reviews and the open assume tickets, each
// group oldest first.
func (s inboxSource) Items(ctx context.Context) ([]ui.InboxItem, error) {
	now := clock()
	tickets, err := s.st.OpenTickets(ctx, now)
	if err != nil {
		return nil, err
	}
	sessions, _, err := listSettled(ctx, s.st, store.ListFilter{}, "", now)
	if err != nil {
		return nil, err
	}
	pending, err := s.st.PendingReviewTasks(ctx)
	if err != nil {
		return nil, err
	}
	var decisions, assumes, prompts, reviews []ui.InboxItem
	for _, t := range decision.Triage(tickets, now) {
		it := ui.InboxItem{
			Key: t.ID, SessionID: t.SessionID, TaskID: t.TaskID, At: t.CreatedAt,
			Label: cmp.Or(t.TaskID, t.SessionID), Summary: t.Question,
		}
		if t.Mode == model.ModeAssume {
			it.Kind, it.Summary = ui.InboxAssume, cmp.Or(t.Assumption, t.Question)
			assumes = append(assumes, it)
		} else {
			decisions = append(decisions, it)
		}
	}
	for _, x := range sessions {
		if x.State == model.StateWaiting {
			prompts = append(prompts, ui.InboxItem{
				Kind: ui.InboxPrompt, Key: x.ID, SessionID: x.ID, TaskID: x.TaskID,
				Session: x, At: x.UpdatedAt, Label: cmp.Or(x.TaskID, x.ID), Summary: x.LastUpdate,
			})
		}
	}
	slices.SortStableFunc(prompts, func(a, b ui.InboxItem) int { return a.At.Compare(b.At) })
	for _, id := range pending {
		r, found, err := s.st.GetReview(ctx, id)
		if err != nil {
			return nil, err
		}
		if found && r.Status == model.ReviewPending { // closed between the two reads
			reviews = append(reviews, ui.InboxItem{
				Kind: ui.InboxReview, Key: r.ID, SessionID: r.SessionID, TaskID: r.TaskID,
				At: r.CreatedAt, Label: r.TaskID, Summary: r.Summary,
			})
		}
	}
	slices.SortStableFunc(reviews, func(a, b ui.InboxItem) int { return a.At.Compare(b.At) })
	return slices.Concat(decisions, prompts, reviews, assumes), nil
}

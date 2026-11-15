package task

import (
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

func sess(id string, st model.State) model.Session {
	return model.Session{ID: id, State: st, TaskID: "T-1", Pane: "%" + id}
}

func TestDeriveTable(t *testing.T) {
	tk := func(st model.TaskStatus) model.Task { return model.Task{ID: "T-1", Status: st} }
	rows := []struct {
		name string
		task model.Task
		ss   []model.Session
		open int
		want model.TaskStatus
	}{
		{"dropped with live", tk(model.TaskDropped), []model.Session{sess("a", model.StateWorking)}, 1, model.TaskDropped},
		{"ready idle", tk(model.TaskReady), []model.Session{sess("a", model.StateIdle)}, 0, model.TaskRunning},
		{"backlog needs_decision session", tk(model.TaskBacklog), []model.Session{sess("a", model.StateNeedsDecision)}, 0, model.TaskNeedsDecision},
		{"ready working + blocking", tk(model.TaskReady), []model.Session{sess("a", model.StateWorking)}, 1, model.TaskNeedsDecision},
		{"only ended and stale", tk(model.TaskReady), []model.Session{sess("a", model.StateEnded), sess("b", model.StateStale)}, 0, model.TaskReady},
		{"blocking, session gone", tk(model.TaskBacklog), nil, 1, model.TaskBacklog},
		{"ended needs_decision ignored", tk(model.TaskReady), []model.Session{sess("a", model.StateEnded), sess("b", model.StateWorking)}, 0, model.TaskRunning},
	}
	for _, r := range rows {
		if got := Derive(r.task, r.ss, r.open, ""); got != r.want {
			t.Errorf("%s: got %s, want %s", r.name, got, r.want)
		}
	}
}

func TestCardsOrderAndLeadFirst(t *testing.T) {
	tasks := []model.Task{
		{N: 3, ID: "T-3", Status: model.TaskReady},
		{N: 1, ID: "T-1", Status: model.TaskReady},
		{N: 2, ID: "T-2", Status: model.TaskBacklog},
		{N: 4, ID: "T-4", Status: model.TaskDropped},
	}
	ss := []model.Session{
		{ID: "z", TaskID: "T-1", State: model.StateWorking, TeamRole: model.RoleTeammate, Pane: "%9", UpdatedAt: at(5)},
		{ID: "m", TaskID: "T-1", State: model.StateWorking, TeamRole: model.RoleLead, Pane: "%1", UpdatedAt: at(1)},
		{ID: "a", TaskID: "T-1", State: model.StateWorking, TeamRole: model.RoleTeammate, Pane: "%2"},
		{ID: "e", TaskID: "T-1", State: model.StateEnded, Pane: "%0"},
		{ID: "x", TaskID: "", State: model.StateWorking},
	}
	cards := Cards(tasks, ss, nil, nil)
	var ids []string
	for _, c := range cards {
		ids = append(ids, c.Task.ID)
	}
	want := []string{"T-2", "T-3", "T-1", "T-4"} // backlog, ready, running, dropped
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
	c := cards[2]
	if c.Pane != "%1" || len(c.Sessions) != 3 || c.Sessions[0].ID != "m" || c.Sessions[1].ID != "a" || c.Sessions[2].ID != "z" {
		t.Errorf("card = %+v", c)
	}
	if cards[0].Pane != "" {
		t.Error("pane for task without sessions")
	}
}

func at(n int) time.Time { return time.Unix(int64(n), 0) }

func TestDeriveWithReviews(t *testing.T) {
	tk := func(st model.TaskStatus) model.Task { return model.Task{ID: "T-1", Status: st} }
	work := []model.Session{sess("a", model.StateWorking)}
	idle := []model.Session{sess("a", model.StateIdle)}
	need := []model.Session{sess("a", model.StateNeedsDecision)}
	rows := []struct {
		name   string
		task   model.Task
		ss     []model.Session
		open   int
		latest model.ReviewStatus
		want   model.TaskStatus
	}{
		{"pending, working", tk(model.TaskReady), work, 0, model.ReviewPending, model.TaskReview},
		{"pending, no session", tk(model.TaskReady), nil, 0, model.ReviewPending, model.TaskReview},
		{"pending beats needs_decision", tk(model.TaskReady), need, 2, model.ReviewPending, model.TaskReview},
		{"approved", tk(model.TaskReady), nil, 0, model.ReviewApproved, model.TaskDone},
		{"merged", tk(model.TaskReady), nil, 0, model.ReviewMerged, model.TaskDone},
		{"merged with live session", tk(model.TaskReady), work, 1, model.ReviewMerged, model.TaskDone},
		{"changes requested, idle", tk(model.TaskReady), idle, 0, model.ReviewChanges, model.TaskRunning},
		{"changes requested, needs_decision", tk(model.TaskReady), need, 0, model.ReviewChanges, model.TaskNeedsDecision},
		{"changes requested, no session", tk(model.TaskBacklog), nil, 0, model.ReviewChanges, model.TaskBacklog},
		{"dropped review, session", tk(model.TaskReady), idle, 0, model.ReviewDropped, model.TaskRunning},
		{"dropped review, no session", tk(model.TaskDropped), nil, 0, model.ReviewDropped, model.TaskDropped},
		{"stored dropped, pending", tk(model.TaskDropped), work, 0, model.ReviewPending, model.TaskDropped},
		{"stored dropped, approved", tk(model.TaskDropped), nil, 0, model.ReviewApproved, model.TaskDropped},
		{"no review, session", tk(model.TaskReady), work, 0, "", model.TaskRunning},
		{"no review, nothing", tk(model.TaskBacklog), nil, 0, "", model.TaskBacklog},
	}
	for _, r := range rows {
		if got := Derive(r.task, r.ss, r.open, r.latest); got != r.want {
			t.Errorf("%s: got %s, want %s", r.name, got, r.want)
		}
	}
}

func TestCardsCarryNewestReviewAndOrder(t *testing.T) {
	tasks := []model.Task{
		{N: 1, ID: "T-1", Status: model.TaskReady},
		{N: 2, ID: "T-2", Status: model.TaskReady},
		{N: 3, ID: "T-3", Status: model.TaskReady},
		{N: 4, ID: "T-4", Status: model.TaskReady},
		{N: 5, ID: "T-5", Status: model.TaskReady},
		{N: 6, ID: "T-6", Status: model.TaskReady},
	}
	ss := []model.Session{
		{ID: "a", TaskID: "T-1", State: model.StateNeedsDecision, Pane: "%1"},
		{ID: "b", TaskID: "T-2", State: model.StateWorking, Pane: "%2"},
	}
	latest := map[string]model.Review{
		"T-2": {ID: "R-2", TaskID: "T-2", Status: model.ReviewPending},
		"T-3": {ID: "R-3", TaskID: "T-3", Status: model.ReviewMerged},
		"T-4": {ID: "R-4", TaskID: "T-4", Status: model.ReviewChanges},
	}
	cards := Cards(tasks, ss, nil, latest)
	var ids []string
	for _, c := range cards {
		ids = append(ids, c.Task.ID)
	}
	// ready (T-5, T-6, T-4 after changes_requested with no session), needs_decision, review, done
	want := []string{"T-4", "T-5", "T-6", "T-1", "T-2", "T-3"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
	for _, c := range cards {
		if c.Review != latest[c.Task.ID] {
			t.Errorf("%s: Review = %+v, want %+v", c.Task.ID, c.Review, latest[c.Task.ID])
		}
	}
	if cards[4].Status != model.TaskReview || cards[4].Pane != "%2" || cards[5].Status != model.TaskDone {
		t.Errorf("cards = %+v", cards)
	}
}

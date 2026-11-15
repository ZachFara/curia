package task

import (
	"sort"

	"github.com/ZachFara/curia/internal/model"
)

func live(s model.Session) bool {
	return s.State != model.StateEnded && s.State != model.StateStale
}

// Derive is the status shown for t. sessions are the ones linked to t;
// openBlocking counts t's open blocking tickets; latest is the status of t's
// newest review, "" when it has none. Order: stored dropped, a pending newest
// review (review), an approved or merged one (done), then the sessions, then
// the stored status. A newest review that is changes_requested or dropped
// changes nothing.
func Derive(t model.Task, sessions []model.Session, openBlocking int, latest model.ReviewStatus) model.TaskStatus {
	if t.Status == model.TaskDropped {
		return model.TaskDropped
	}
	switch latest {
	case model.ReviewPending:
		return model.TaskReview
	case model.ReviewApproved, model.ReviewMerged:
		return model.TaskDone
	}
	hasLive := false
	needs := openBlocking > 0
	for _, s := range sessions {
		if !live(s) {
			continue
		}
		hasLive = true
		if s.State == model.StateNeedsDecision {
			needs = true
		}
	}
	switch {
	case hasLive && needs:
		return model.TaskNeedsDecision
	case hasLive:
		return model.TaskRunning
	}
	return t.Status
}

// Card is a task as the board and task ls show it.
type Card struct {
	Task     model.Task
	Status   model.TaskStatus
	Sessions []model.Session // live linked sessions; the lead or solo session first, then by id
	Pane     string          // the first session's pane, "" when none
	Review   model.Review    // the newest review; zero when none
}

// columns is the order of the board's columns.
var columns = map[model.TaskStatus]int{
	model.TaskBacklog: 0, model.TaskReady: 1, model.TaskRunning: 2,
	model.TaskNeedsDecision: 3, model.TaskReview: 4, model.TaskDone: 5,
	model.TaskDropped: 6,
}

// Cards derives every task's card. Order: by status as the board's columns
// run (backlog, ready, running, needs_decision, review, done, dropped), then
// by n. latest holds each task's newest review, by task id.
func Cards(tasks []model.Task, sessions []model.Session, openBlocking map[string]int, latest map[string]model.Review) []Card {
	by := map[string][]model.Session{}
	for _, s := range sessions {
		if s.TaskID != "" && live(s) {
			by[s.TaskID] = append(by[s.TaskID], s)
		}
	}
	cards := make([]Card, 0, len(tasks))
	for _, t := range tasks {
		ss := by[t.ID]
		sort.SliceStable(ss, func(i, j int) bool {
			ti, tj := ss[i].TeamRole == model.RoleTeammate, ss[j].TeamRole == model.RoleTeammate
			if ti != tj {
				return !ti
			}
			return ss[i].ID < ss[j].ID
		})
		rv := latest[t.ID]
		c := Card{Task: t, Status: Derive(t, ss, openBlocking[t.ID], rv.Status), Sessions: ss, Review: rv}
		if len(ss) > 0 {
			c.Pane = ss[0].Pane
		}
		cards = append(cards, c)
	}
	sort.SliceStable(cards, func(i, j int) bool {
		ci, cj := columns[cards[i].Status], columns[cards[j].Status]
		if ci != cj {
			return ci < cj
		}
		return cards[i].Task.N < cards[j].Task.N
	})
	return cards
}

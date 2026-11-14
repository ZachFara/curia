package task

import (
	"sort"

	"github.com/ZachFara/curia/internal/model"
)

func live(s model.Session) bool {
	return s.State != model.StateEnded && s.State != model.StateStale
}

// Derive is the status shown for t (section 4.4). sessions are the ones
// linked to t; openBlocking counts t's open blocking tickets.
func Derive(t model.Task, sessions []model.Session, openBlocking int) model.TaskStatus {
	if t.Status == model.TaskDropped {
		return model.TaskDropped
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
}

// columns is the order of the board's columns.
var columns = map[model.TaskStatus]int{
	model.TaskBacklog: 0, model.TaskReady: 1, model.TaskRunning: 2,
	model.TaskNeedsDecision: 3, model.TaskReview: 4, model.TaskDone: 5,
	model.TaskDropped: 6,
}

// Cards derives every task's card. Order: by status as the board's columns
// run (backlog, ready, running, needs_decision, review, done, dropped), then
// by n.
func Cards(tasks []model.Task, sessions []model.Session, openBlocking map[string]int) []Card {
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
		c := Card{Task: t, Status: Derive(t, ss, openBlocking[t.ID]), Sessions: ss}
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

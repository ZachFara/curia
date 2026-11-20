package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// `curia usage` lists a session's task with that task's budget (F16).
func TestUsageShowsTaskBudget(t *testing.T) {
	db, _ := fleetHome(t, nil)
	t.Cleanup(func() { usageJSON = false })
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := st.CreateTask(context.Background(), model.Task{Title: "t", Repo: "/r", Status: model.TaskReady, BudgetUSD: 20, CreatedAt: fleetNow})
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	seedSessions(t, db, model.Session{ID: "sa", State: model.StateWorking, TaskID: tk.ID, UpdatedAt: fleetNow})
	usageCost(t, db, "sa", 16)
	seedSessions(t, db, model.Session{ID: "sb", State: model.StateWorking, UpdatedAt: fleetNow})
	usageCost(t, db, "sb", 14)

	out, err := runRoot(t, "usage")
	if err != nil || !strings.Contains(out, "BUDGET") || !strings.Contains(out, "$16.00/$20 80% !") {
		t.Errorf("usage lacks the budget (%v):\n%s", err, out)
	}
	if strings.Count(out, "/$20") != 1 {
		t.Errorf("a session of no task shows a budget:\n%s", out)
	}
	out, err = runRoot(t, "usage", "--json")
	var got struct {
		Burn []struct {
			SessionID string   `json:"session_id"`
			Budget    *float64 `json:"budget_usd"`
			Pct       *int     `json:"budget_pct"`
		}
	}
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || len(got.Burn) != 2 {
		t.Fatalf("usage --json = %v:\n%s", err, out)
	}
	for _, b := range got.Burn {
		if (b.SessionID == "sa") != (b.Budget != nil && *b.Budget == 20 && b.Pct != nil && *b.Pct == 80) {
			t.Errorf("burn row %+v: budget shown for the wrong session", b)
		}
	}
}

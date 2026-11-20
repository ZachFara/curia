package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func TestTaskBudgetRoundTripAndSpend(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	tk := newTask("budgeted")
	tk.BudgetUSD = 5
	tk = createTask(t, s, tk)
	if got := getTask(t, s, tk.ID); got.BudgetUSD != 5 {
		t.Fatalf("BudgetUSD = %v, want 5", got.BudgetUSD)
	}
	plain := createTask(t, s, newTask("plain"))
	if plain.BudgetUSD != 0 {
		t.Errorf("a task with no budget reads back %v, want 0", plain.BudgetUSD)
	}
	for _, id := range []string{"a", "b", "c"} {
		upsert(t, s, model.Session{ID: id, State: model.StateIdle, TaskID: tk.ID, Agent: "claude", StartedAt: taskT0})
	}
	rawExec(t, path, `UPDATE sessions SET state = 'ended' WHERE id = 'a'`)
	rawExec(t, path, `UPDATE sessions SET cost_usd = 1.5 WHERE id = 'a'`) // an ended session still counts
	rawExec(t, path, `UPDATE sessions SET cost_usd = 2.5 WHERE id = 'b'`) // c has no cost: unknown, not zero

	b, err := s.TaskBudget(ctx, tk.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := (model.Budget{Limit: 5, Spent: 4, Known: true}); b != want {
		t.Errorf("TaskBudget = %+v, want %+v", b, want)
	}
	if over, _ := b.Exhausted(); over {
		t.Error("exhausted at 80%")
	}
	rawExec(t, path, `UPDATE sessions SET cost_usd = 3.5 WHERE id = 'b'`)
	if b, _ = s.TaskBudget(ctx, tk.ID, 0); !func() bool { o, _ := b.Exhausted(); return o }() {
		t.Errorf("TaskBudget = %+v, want exhausted at exactly 5", b)
	}

	// The default applies to a task with none of its own; nothing for no task.
	if b, _ = s.TaskBudget(ctx, plain.ID, 7); b.Limit != 7 || b.Known {
		t.Errorf("default budget = %+v, want limit 7, spend unknown", b)
	}
	if b, _ = s.TaskBudget(ctx, plain.ID, 0); b.Limit != 0 {
		t.Errorf("no budget and no default = %+v, want limit 0", b)
	}
	if b, err = s.TaskBudget(ctx, "", 7); err != nil || b.Limit != 0 {
		t.Errorf("TaskBudget of no task = %+v, %v; want none", b, err)
	}
	if b, err = s.TaskBudget(ctx, "T-99", 7); err != nil || b.Limit != 0 {
		t.Errorf("TaskBudget of unknown task = %+v, %v; want none", b, err)
	}
}

// The hook of part 2 calls TaskBudget on every tool call: it must be one
// indexed read. The plan is asserted; the time is only logged (no latency
// percentile is asserted here).
func TestTaskBudgetIsOneIndexedRead(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	tk := newTask("b")
	tk.BudgetUSD = 5
	tk = createTask(t, s, tk)
	for i := range 200 {
		upsert(t, s, model.Session{ID: fmt.Sprintf("s%d", i), State: model.StateIdle, TaskID: tk.ID, Agent: "claude", StartedAt: taskT0})
	}
	db := rawDB(t, path)
	rows, err := db.Query("EXPLAIN QUERY PLAN "+store.TaskBudgetQuery, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan string
	for rows.Next() {
		var a, b, c int
		var d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		plan += d + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan, "SCAN sessions") || !strings.Contains(plan, "sessions_task") {
		t.Errorf("TaskBudget does not use the sessions_task index:\n%s", plan)
	}
	start := time.Now()
	const n = 1000
	for range n {
		if _, err := s.TaskBudget(ctx, tk.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("TaskBudget with 200 sessions: %v per call", time.Since(start)/n)
}

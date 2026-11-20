package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ZachFara/curia/internal/model"
)

// taskBudget reads a task's own budget and the summed cost of its sessions,
// of any state, in one statement over the primary key and the sessions_task
// partial index (whose condition the join repeats): cheap enough for the hook.
const taskBudget = `
SELECT t.budget_usd, COALESCE(SUM(s.cost_usd), 0), count(s.cost_usd)
FROM tasks t LEFT JOIN sessions s ON s.task_id = t.id AND s.task_id IS NOT NULL AND s.task_id != ''
WHERE t.id = ? GROUP BY t.id`

// TaskBudget is task id's budget and spend: its own budget_usd, else
// defaultUSD (cost.task_budget_default; 0 = none), against the cost of every
// session linked to it, the same sum as TaskRun. Spend is unknown when no
// session has a cost. No task, or an empty id, has no budget.
func (s *Store) TaskBudget(ctx context.Context, id string, defaultUSD float64) (model.Budget, error) {
	if id == "" {
		return model.Budget{}, nil
	}
	var own sql.NullFloat64
	var b model.Budget
	var known int
	err := s.db.QueryRowContext(ctx, taskBudget, id).Scan(&own, &b.Spent, &known)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Budget{}, nil
	}
	if err != nil {
		return model.Budget{}, fail("read the budget of task", id, err)
	}
	b.Known = known > 0
	b.Limit = defaultUSD
	if own.Valid {
		b.Limit = own.Float64
	}
	return b, nil
}

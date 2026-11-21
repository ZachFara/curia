package main

import (
	"context"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// taskBudgets fills the Budget of each card whose task has one (its own or
// the cost.task_budget_default).
func taskBudgets(ctx context.Context, st *store.Store, cfg config.Config, cards []task.Card) error {
	for i, c := range cards {
		if c.Task.BudgetUSD == 0 && cfg.Cost.TaskBudgetDefault == 0 {
			continue
		}
		b, err := st.TaskBudget(ctx, c.Task.ID, cfg.Cost.TaskBudgetDefault)
		if err != nil {
			return err
		}
		cards[i].Budget = b
	}
	return nil
}

// The JSON forms of a budget: nulls for no budget and for an unknown spend.
func budgetLimit(b model.Budget) *float64 {
	if b.Level() == model.BudgetNone {
		return nil
	}
	return &b.Limit
}

func budgetSpent(b model.Budget) *float64 {
	if l := b.Level(); l == model.BudgetNone || l == model.BudgetUnknown {
		return nil
	}
	return &b.Spent
}

func budgetPct(b model.Budget) *int {
	if p, ok := b.Pct(); ok {
		return &p
	}
	return nil
}

// usageBudgets fills the budget of each burn row's task.
func usageBudgets(ctx context.Context, st *store.Store, cfg config.Config, rows []usageBurnRow) error {
	for i, r := range rows {
		b, err := st.TaskBudget(ctx, r.TaskID, cfg.Cost.TaskBudgetDefault)
		if err != nil {
			return err
		}
		rows[i].Budget = b
	}
	return nil
}

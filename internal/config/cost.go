package config

import (
	"fmt"
	"math"

	"github.com/ZachFara/curia/internal/model"
)

// Cost configures F16's per-task budget.
type Cost struct {
	// TaskBudgetDefault is the budget in dollars of a task with none of its
	// own; 0 means none. A repository file may only lower a nonzero value.
	TaskBudgetDefault float64 `toml:"task_budget_default"`
	// BudgetHardStop makes the PreToolUse hook deny a task's tool calls, but
	// Curia's own MCP tools, once its budget is spent (hook.preToolUse).
	BudgetHardStop bool `toml:"budget_hard_stop"`
}

func (c Cost) validate() error {
	if v := c.TaskBudgetDefault; math.IsNaN(v) || v < 0 || v > model.MaxBudgetUSD {
		return fmt.Errorf("cost.task_budget_default must be between 0 and %d, got %v", model.MaxBudgetUSD, v)
	}
	return nil
}

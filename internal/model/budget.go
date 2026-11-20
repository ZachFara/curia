package model

import (
	"fmt"
	"math"
)

// BudgetLevel is how far a task is through its budget (spec F16).
type BudgetLevel int

// The levels. Warn starts at 80%, Over at 100%.
const (
	BudgetNone    BudgetLevel = iota // no budget
	BudgetUnknown                    // a budget, but no session has a known cost
	BudgetOK
	BudgetWarn
	BudgetOver
)

// Budget is a task's limit in dollars (0: none) and what its sessions have
// spent, as store.TaskRun sums it. Known is false when no session has a
// dollar cost: the spend is then unknown, never zero.
type Budget struct {
	Limit, Spent float64
	Known        bool
}

// Level is the budget's level. An unknown spend is never Warn or Over.
func (b Budget) Level() BudgetLevel {
	switch {
	case !(b.Limit > 0):
		return BudgetNone
	case !b.Known || math.IsNaN(b.Spent):
		return BudgetUnknown
	case b.Spent >= b.Limit:
		return BudgetOver
	case b.Spent >= 0.8*b.Limit:
		return BudgetWarn
	}
	return BudgetOK
}

// Pct is the share spent, rounded down so 99.9% never shows as 100; ok is
// false when there is no budget or the spend is unknown.
func (b Budget) Pct() (pct int, ok bool) {
	if l := b.Level(); l == BudgetNone || l == BudgetUnknown {
		return 0, false
	}
	return int(math.Floor(b.Spent / b.Limit * 100)), true
}

// Exhausted is what the PreToolUse stop of part 2 asks: the spend has
// reached the budget. False for no budget and for an unknown spend (a
// session with no cost data cannot be stopped); reason is for the deny.
func (b Budget) Exhausted() (bool, string) {
	if b.Level() != BudgetOver {
		return false, ""
	}
	return true, fmt.Sprintf("task budget %s spent (%s): stop and report", fmt.Sprintf("$%g", b.Limit), fmt.Sprintf("$%.2f", b.Spent))
}

func (l BudgetLevel) String() string {
	return [...]string{"none", "unknown", "ok", "warn", "over"}[l]
}

// Short is the budget for a crowded line: "" with no budget, "?%" when the
// spend is unknown, else "82%", with "!" from 80% and "!!" from 100%.
func (b Budget) Short() string {
	pct, ok := b.Pct()
	if !ok {
		if b.Level() == BudgetUnknown {
			return "?%"
		}
		return ""
	}
	return fmt.Sprintf("%d%%%s", pct, [...]string{BudgetOK: "", BudgetWarn: "!", BudgetOver: "!!"}[b.Level()])
}

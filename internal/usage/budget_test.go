package usage

import (
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

func TestBudgetCell(t *testing.T) {
	for _, c := range []struct {
		name  string
		b     model.Budget
		cell  string
		level model.BudgetLevel
		over  bool
	}{
		{"none", model.Budget{Spent: 3, Known: true}, "", model.BudgetNone, false},
		{"zero spend", model.Budget{Limit: 5, Known: true}, "$0.00/$5 0%", model.BudgetOK, false},
		{"below warn", model.Budget{Limit: 5, Spent: 3.99, Known: true}, "$3.99/$5 79%", model.BudgetOK, false},
		{"exactly 80", model.Budget{Limit: 5, Spent: 4, Known: true}, "$4.00/$5 80% !", model.BudgetWarn, false},
		{"just under 100", model.Budget{Limit: 5, Spent: 4.999, Known: true}, "$5.00/$5 99% !", model.BudgetWarn, false},
		{"exactly 100", model.Budget{Limit: 5, Spent: 5, Known: true}, "$5.00/$5 100% !!", model.BudgetOver, true},
		{"over", model.Budget{Limit: 5.5, Spent: 11, Known: true}, "$11.00/$5.50 200% !!", model.BudgetOver, true},
		{"unknown", model.Budget{Limit: 5}, "budget ?", model.BudgetUnknown, false},
		{"unknown with stray spend", model.Budget{Limit: 5, Spent: 9}, "budget ?", model.BudgetUnknown, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := BudgetCell(c.b); got != c.cell {
				t.Errorf("Cell = %q, want %q", got, c.cell)
			}
			if got := c.b.Level(); got != c.level {
				t.Errorf("Level = %v, want %v", got, c.level)
			}
			if over, reason := c.b.Exhausted(); over != c.over || (over && reason == "") {
				t.Errorf("Exhausted = %v, %q; want %v with a reason when true", over, reason, c.over)
			}
		})
	}
}

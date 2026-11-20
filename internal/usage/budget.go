package usage

import (
	"fmt"
	"math"
	"strconv"

	"github.com/ZachFara/curia/internal/model"
)

// BudgetCell is the budget for a cell: "" with no budget, "budget ?" when the
// spend is unknown, else "$4.10/$5 82%" with "!" from 80% and "!!" from
// 100%.
func BudgetCell(b model.Budget) string {
	l := b.Level()
	switch l {
	case model.BudgetNone:
		return ""
	case model.BudgetUnknown:
		return "budget ?"
	}
	pct, _ := b.Pct()
	s := fmt.Sprintf("%s/%s %d%%", CostCell(b.Spent, true), dollars(b.Limit), pct)
	switch l {
	case model.BudgetWarn:
		s += " !"
	case model.BudgetOver:
		s += " !!"
	}
	return s
}

// dollars is a limit as typed: "$5", "$5.50".
func dollars(v float64) string {
	if v == math.Trunc(v) && v < 1e9 {
		return "$" + strconv.FormatInt(int64(v), 10)
	}
	return fmt.Sprintf("$%.2f", v)
}

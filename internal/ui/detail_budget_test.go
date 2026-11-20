package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

func TestDetailCostShowsBudget(t *testing.T) {
	for _, c := range []struct {
		b    model.Budget
		want string
	}{
		{model.Budget{Limit: 5, Spent: 4, Known: true}, "budget $4.00/$5 80% !"},
		{model.Budget{Limit: 5, Spent: 5, Known: true}, "budget $5.00/$5 100% !!"},
		{model.Budget{Limit: 5}, "budget ?"},
	} {
		d := DetailData{Session: model.Session{ID: "s1", State: model.StateIdle}, Budget: c.b}
		if got := RenderDetail(d, DetailDiff{}, func() time.Time { return testNow }); !strings.Contains(got, c.want) {
			t.Errorf("detail for %+v lacks %q:\n%s", c.b, c.want, got)
		}
	}
	d := DetailData{Session: model.Session{ID: "s1", State: model.StateIdle}}
	if got := RenderDetail(d, DetailDiff{}, func() time.Time { return testNow }); strings.Contains(got, "budget") {
		t.Errorf("detail without a budget mentions one:\n%s", got)
	}
}

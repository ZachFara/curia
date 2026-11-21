package usage

import (
	"strings"
	"testing"
)

func TestBranchCostBuilt(t *testing.T) {
	usd, ok := BranchCost(fixture(t, "summary-built.json"), "curia/T-1-fix")
	assert.True(t, ok)
	assert.InDelta(t, 5.03, usd, 1e-9)
	_, ok = BranchCost(fixture(t, "summary-built.json"), "curia/T-1")
	assert.False(t, ok, "the branch name is compared exactly")
}

func TestBranchCostCaptured(t *testing.T) {
	usd, ok := BranchCost(fixture(t, "summary-captured.json"), "curia/T-2-add-hello")
	assert.True(t, ok)
	assert.InDelta(t, 8.000000000000001e-07+0.0003015+0.0022766+0.00146124, usd, 1e-12)
}

func TestBranchCostRefusals(t *testing.T) {
	part := func(in, out, cw, cr string) string {
		return `{"currentProject":{"branches":[{"branch":"b","spend":{"cost":{"input":` + in + `,"output":` + out + `,"cacheWrite":` + cw + `,"cacheRead":` + cr + `}}}]}}`
	}
	cases := map[string]string{
		"absent branch":   `{"currentProject":{"branches":[{"branch":"x","spend":{"cost":{"input":1,"output":1,"cacheWrite":1,"cacheRead":1}}}]}}`,
		"null project":    `{"currentProject":null}`,
		"no project":      `{}`,
		"html":            `<html>`,
		"empty":           ``,
		"null":            `null`,
		"array":           `[]`,
		"string part":     part(`"1"`, "1", "1", "1"),
		"negative part":   part("1", "-0.5", "1", "1"),
		"huge part":       part("1", "1", "1e12", "1"),
		"overflow part":   part("1", "1", "1", "1e999"),
		"missing part":    `{"currentProject":{"branches":[{"branch":"b","spend":{"cost":{"input":1,"output":1,"cacheWrite":1}}}]}}`,
		"null part":       part("1", "1", "null", "1"),
		"branches object": `{"currentProject":{"branches":{"branch":"b"}}}`,
		"branches null":   `{"currentProject":{"branches":null}}`,
		"project array":   `{"currentProject":[]}`,
		"no spend":        `{"currentProject":{"branches":[{"branch":"b"}]}}`,
		"cost not object": `{"currentProject":{"branches":[{"branch":"b","spend":{"cost":5}}]}}`,
		"too large":       `{"x":"` + strings.Repeat("a", MaxSummary) + `"}`,
		"other project":   `{"projects":[{"branches":[{"branch":"b","spend":{"cost":{"input":1,"output":1,"cacheWrite":1,"cacheRead":1}}}]}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			usd, ok := BranchCost([]byte(in), "b")
			assert.False(t, ok)
			assert.Zero(t, usd)
		})
	}
	usd, ok := BranchCost([]byte(part("0", "0", "0", "1000000")), "b")
	assert.True(t, ok, "the upper bound is inclusive")
	assert.InDelta(t, 1000000, usd, 0)
}

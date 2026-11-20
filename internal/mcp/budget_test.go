package mcp

import (
	"strings"
	"testing"
)

// An agent can never set or raise a budget (F16): no tool names or takes one.
func TestNoToolOffersABudget(t *testing.T) {
	for _, tool := range Tools(Deps{}) {
		if all := strings.ToLower(tool.Name + tool.Description + string(tool.InputSchema)); strings.Contains(all, "budget") {
			t.Errorf("tool %s mentions a budget: an agent must have no way to set one", tool.Name)
		}
	}
}

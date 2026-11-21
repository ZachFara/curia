package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// budgetRow is the budget part of a `curia task ls --json` row.
type budgetRow struct {
	ID          string   `json:"id"`
	BudgetUSD   *float64 `json:"budget_usd"`
	SpentUSD    *float64 `json:"spent_usd"`
	BudgetPct   *int     `json:"budget_pct"`
	BudgetLevel string   `json:"budget_level"`
}

func budgetLs(t *testing.T) map[string]budgetRow {
	t.Helper()
	out := taskMust(t, "task", "ls", "--json", "--status", "all")
	var rows []budgetRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	m := map[string]budgetRow{}
	for _, r := range rows {
		m[r.ID] = r
	}
	return m
}

func budgetCost(t *testing.T, db, session string, usd float64) {
	t.Helper()
	if _, err := taskOpen(t, db).Ingest(context.Background(), store.Reading{SessionID: session, CostUSD: &usd}, decisionsParse(t, taskNow)); err != nil {
		t.Fatal(err)
	}
}

func TestTaskBudgetAddEditAndRefusals(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Plain", "--repo", repo)
	taskMust(t, "task", "add", "Capped", "--repo", repo, "--budget", "5.00")
	if got := taskGet(t, db, "T-1").BudgetUSD; got != 0 {
		t.Errorf("no --budget stored %v, want none", got)
	}
	if got := taskGet(t, db, "T-2").BudgetUSD; got != 5 {
		t.Errorf("--budget 5.00 stored %v", got)
	}
	taskMust(t, "task", "edit", "T-2", "--budget", "8")
	if got := taskGet(t, db, "T-2").BudgetUSD; got != 8 {
		t.Errorf("edit --budget 8 stored %v", got)
	}
	taskMust(t, "task", "edit", "T-2", "--title", "Renamed")
	if got := taskGet(t, db, "T-2").BudgetUSD; got != 8 {
		t.Errorf("an edit of the title changed the budget to %v", got)
	}
	taskMust(t, "task", "edit", "T-2", "--budget", "0")
	if got := taskGet(t, db, "T-2").BudgetUSD; got != 0 {
		t.Errorf("edit --budget 0 left %v, want none", got)
	}
	taskMust(t, "task", "edit", "T-2", "--budget", "3")
	for _, bad := range []string{"-1", "abc", "NaN", "Inf", "2000000"} {
		out, err := taskRun(t, nil, "task", "edit", "T-2", "--budget="+bad)
		if err == nil {
			t.Errorf("--budget=%s accepted:\n%s", bad, out)
		}
		if _, err = taskRun(t, nil, "task", "add", "x", "--repo", repo, "--budget="+bad); err == nil {
			t.Errorf("add --budget=%s accepted", bad)
		}
	}
	if got := taskGet(t, db, "T-2").BudgetUSD; got != 3 || taskCount(t, db) != 2 {
		t.Errorf("a refused budget changed the store: budget %v, %d tasks", got, taskCount(t, db))
	}
}

// Only the user sets a budget: inside an agent both commands refuse (the guard).
func TestTaskBudgetInsideAgent(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	taskMust(t, "task", "add", "Capped", "--repo", repo, "--budget", "5")
	t.Setenv("CLAUDECODE", "1")
	if _, err := taskRun(t, nil, "task", "edit", "T-1", "--budget", "500"); err == nil {
		t.Error("an agent raised a budget")
	}
	if _, err := taskRun(t, nil, "task", "add", "x", "--repo", repo, "--budget", "500"); err == nil {
		t.Error("an agent added a task with a budget")
	}
	if got := taskGet(t, db, "T-1").BudgetUSD; got != 5 || taskCount(t, db) != 1 {
		t.Errorf("budget %v, %d tasks after the agent's attempts, want 5 and 1", got, taskCount(t, db))
	}
}

func TestTaskLsShowsBudget(t *testing.T) {
	db := taskHome(t)
	repo := taskRepo(t, "main")
	budgetSeed(t, db, repo)
	rows := budgetLs(t)
	budgetCheckRows(t, rows)
	if r := rows["T-7"]; r.BudgetUSD == nil || r.SpentUSD != nil || r.BudgetPct != nil || r.BudgetLevel != "unknown" {
		t.Errorf("T-7 (cost unknown) = %+v, want a limit and nulls, level unknown: never an invented figure", r)
	}
	if r := rows["T-1"]; r.BudgetUSD != nil || r.BudgetLevel != "none" {
		t.Errorf("T-1 (no budget) = %+v", r)
	}

	table := taskMust(t, "task", "ls", "--status", "all")
	for _, want := range []string{"BUDGET", "$4.00/$5 80% !", "$5.00/$5 100% !!", "$7.50/$5 150% !!", "$0.00/$5 0%", "budget ?"} {
		if !strings.Contains(table, want) {
			t.Errorf("table lacks %q:\n%s", want, table)
		}
	}

	// The default applies to a task with none of its own, and not to one whose own wins.
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Dir(configFile(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configFile(home), []byte("[cost]\ntask_budget_default = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows = budgetLs(t)
	if r := rows["T-1"]; r.BudgetUSD == nil || *r.BudgetUSD != 2 {
		t.Errorf("T-1 with default 2 = %+v", r)
	}
	if r := rows["T-3"]; r.BudgetUSD == nil || *r.BudgetUSD != 5 {
		t.Errorf("T-3 with its own budget = %+v, want 5", r)
	}
}

func budgetCheckRows(t *testing.T, rows map[string]budgetRow) {
	t.Helper()
	for _, c := range []struct {
		id    string
		limit float64
		spent float64
		pct   int
		level string
	}{
		{"T-2", 5, 0, 0, "ok"},
		{"T-3", 5, 4, 80, "warn"},
		{"T-4", 5, 5, 100, "over"},
		{"T-5", 5, 7.5, 150, "over"},
		{"T-6", 5, 7, 140, "over"},
	} {
		r := rows[c.id]
		if r.BudgetUSD == nil || *r.BudgetUSD != c.limit || r.SpentUSD == nil || *r.SpentUSD != c.spent || r.BudgetPct == nil || *r.BudgetPct != c.pct || r.BudgetLevel != c.level {
			t.Errorf("%s = %+v, want limit %v spent %v pct %d %s", c.id, r, c.limit, c.spent, c.pct, c.level)
		}
	}
}

// budgetSeed makes T-1 to T-8 with the budgets and sessions the checks read.
func budgetSeed(t *testing.T, db, repo string) {
	t.Helper()
	for _, title := range []string{"none", "zero", "eighty", "exact", "over", "ended", "unknown", "dflt"} {
		args := []string{"task", "add", title, "--repo", repo}
		if title != "none" && title != "dflt" {
			args = append(args, "--budget", "5")
		}
		taskMust(t, args...)
	}
	// T-2 zero spend with a session, T-3 80%, T-4 exactly 100%, T-5 over,
	// T-6 an ended session still counts, T-7 a session with no cost.
	for i, c := range []struct {
		id   string
		cost float64
	}{{"T-2", 0}, {"T-3", 4}, {"T-4", 5}, {"T-5", 7.5}, {"T-6", 6}} {
		taskPutSession(t, db, "s"+c.id, c.id, "%"+string(rune('1'+i)), model.StateWorking)
		budgetCost(t, db, "s"+c.id, c.cost)
	}
	taskPutSession(t, db, "sT-6b", "T-6", "%9", model.StateEnded)
	budgetCost(t, db, "sT-6b", 1)
	taskPutSession(t, db, "sT-7", "T-7", "%8", model.StateWorking)
}

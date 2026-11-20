package usage

import (
	"strings"
	"testing"
	"time"
)

func TestParsePSSkipsMalformedLinesAndBoundsRows(t *testing.T) {
	out := "  1     0  0.0   1000\n 10     1 12.5  20480\nx y z w\n 11    10  1.0\n -5 1 1 1\n 12 10 NaN 5\n 13 10 1e999 5\n 14 10 2.0 -3\n"
	rows := ParsePS(out)
	if len(rows) != 2 || rows[1] != (ProcRow{PID: 10, PPID: 1, CPU: 12.5, RSSKB: 20480}) {
		t.Fatalf("rows = %+v, want pids 1 and 10 only", rows)
	}
	big := strings.Repeat("2 1 0.0 1\n", maxPSRows+50)
	if got := len(ParsePS(big)); got != maxPSRows {
		t.Errorf("rows = %d, want the cap %d", got, maxPSRows)
	}
}

func TestSumTreeSumsDescendantsOnly(t *testing.T) {
	rows := []ProcRow{
		{PID: 5, PPID: 1, CPU: 1, RSSKB: 100},   // the pane's shell
		{PID: 6, PPID: 5, CPU: 2, RSSKB: 200},   // child
		{PID: 7, PPID: 6, CPU: 3.5, RSSKB: 300}, // grandchild
		{PID: 8, PPID: 1, CPU: 50, RSSKB: 9999}, // another tree
	}
	cpu, kb, ok := SumTree(rows, 5)
	if !ok || cpu != 6.5 || kb != 600 {
		t.Errorf("SumTree = %v %v %v, want 6.5 600 true", cpu, kb, ok)
	}
	if _, _, ok = SumTree(rows, 99); ok {
		t.Error("a pane pid that exited before the snapshot must give no sample")
	}
}

func TestSumTreeToleratesCyclesDuplicatesAndMissingParents(t *testing.T) {
	rows := []ProcRow{
		{PID: 5, PPID: 7, CPU: 1, RSSKB: 10}, // cycle 5 -> 6 -> 7 -> 5
		{PID: 6, PPID: 5, CPU: 1, RSSKB: 10},
		{PID: 7, PPID: 6, CPU: 1, RSSKB: 10},
		{PID: 9, PPID: 4000, CPU: 1, RSSKB: 10}, // parent not in the table
		{PID: 6, PPID: 5, CPU: 1, RSSKB: 10},    // duplicate row
	}
	cpu, kb, ok := SumTree(rows, 5)
	if !ok || cpu != 3 || kb != 30 {
		t.Errorf("SumTree = %v %v %v, want each process once: 3 30 true", cpu, kb, ok)
	}
}

func TestResourceCellIsNeverAStaleNumber(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	iv := 10 * time.Second
	for _, c := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"fresh", now.Add(-5 * time.Second), "13% 340M"},
		{"edge", now.Add(-ResourceStale(iv)), "13% 340M"},
		{"stale", now.Add(-ResourceStale(iv) - time.Second), "?"},
		{"none", time.Time{}, "?"},
		{"future", now.Add(time.Hour), "?"},
	} {
		if got := ResourceCell(12.6, 340*1024, c.at, now, iv); got != c.want {
			t.Errorf("%s: cell = %q, want %q", c.name, got, c.want)
		}
	}
	if got := ResourceCell(0.04, 1500*1024*1024/1024, now, now, iv); got != "0% 1.5G" {
		t.Errorf("cell = %q, want 0%% 1.5G", got)
	}
}

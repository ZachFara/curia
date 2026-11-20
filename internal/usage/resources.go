package usage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// F19 per-agent resources: the pure parts of the monitor's sampler. The ps
// call and the store live elsewhere.

// maxPSRows bounds how many rows of a process table are read: output from
// outside is data, never trusted to be short.
const maxPSRows = 200000

// ProcRow is one line of `ps -A -o pid=,ppid=,%cpu=,rss=`.
type ProcRow struct {
	PID, PPID int
	CPU       float64 // percent of one core, as ps reports it
	RSSKB     int64   // resident memory in KiB
}

// ParsePS reads that output. A line that is not four numbers, with a pid
// below 1, a negative or non-finite value, is skipped (a process can exit
// mid-listing), and at most maxPSRows rows are read.
func ParsePS(out string) []ProcRow {
	var rows []ProcRow
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		cpu, e3 := strconv.ParseFloat(f[2], 64)
		rss, e4 := strconv.ParseInt(f[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || pid < 1 || ppid < 0 || rss < 0 || cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) {
			continue
		}
		if rows = append(rows, ProcRow{PID: pid, PPID: ppid, CPU: cpu, RSSKB: rss}); len(rows) == maxPSRows {
			break
		}
	}
	return rows
}

// SumTree sums CPU and resident memory over root and all its descendants in
// one snapshot. ok is false when root is not in it (the process exited
// before the snapshot). Each process counts once, so a cycle, a duplicate
// row or a parent missing from the table cannot loop or double count: the
// walk visits each pid at most once.
func SumTree(rows []ProcRow, root int) (cpu float64, rssKB int64, ok bool) {
	byPID := map[int]ProcRow{}
	kids := map[int][]int{}
	for _, r := range rows {
		if _, dup := byPID[r.PID]; !dup {
			byPID[r.PID] = r
			kids[r.PPID] = append(kids[r.PPID], r.PID)
		}
	}
	if _, found := byPID[root]; !found {
		return 0, 0, false
	}
	seen := map[int]bool{}
	for todo := []int{root}; len(todo) > 0; {
		pid := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		cpu += byPID[pid].CPU
		rssKB += byPID[pid].RSSKB
		todo = append(todo, kids[pid]...)
	}
	return cpu, rssKB, true
}

// ResourceStale is how old a sample may be and still count as current: three
// intervals, so one slow or missed tick does not blank the numbers and a
// stopped monitor does within seconds.
func ResourceStale(interval time.Duration) time.Duration { return 3 * interval }

// ResourceFresh reports whether a sample taken at at is current at now: not
// absent, not older than ResourceStale and not in the future (a clock that
// moved). A number that is not fresh is never shown.
func ResourceFresh(at, now time.Time, interval time.Duration) bool {
	return !at.IsZero() && !at.After(now.Add(interval)) && now.Sub(at) <= ResourceStale(interval)
}

// ResourceMem is resident memory in KiB as "340M" or "1.5G".
func ResourceMem(kb int64) string {
	mb := kb / 1024
	if mb >= 1024 {
		return fmt.Sprintf("%.1fG", float64(mb)/1024)
	}
	return fmt.Sprintf("%dM", mb)
}

// ResourceCell is the dashboard's and the detail view's text for one
// session: "13% 340M", or "?" when the sample is not fresh.
func ResourceCell(cpu float64, rssKB int64, at, now time.Time, interval time.Duration) string {
	if !ResourceFresh(at, now, interval) {
		return "?"
	}
	return fmt.Sprintf("%.0f%% %s", cpu, ResourceMem(rssKB))
}

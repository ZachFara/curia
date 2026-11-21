// Command verdict is the last step of `make test-latency`: it reads the
// `go test -v` output of the three spaced passes on stdin (docs/decisions/0012),
// prints the table and exits 1 unless every gate passes. When nothing failed
// but a gate ran on a machine too slow to measure it, the last line is
// CouldNotMeasureMarker (CI reads it); a real failure anywhere wins.
package main

import (
	"fmt"
	"os"

	"github.com/ZachFara/curia/internal/latency"
)

func main() {
	rs, err := latency.ReadResults(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verdict:", err)
		os.Exit(2)
	}
	table, outcome := latency.Judge(rs)
	fmt.Print(table)
	if outcome == latency.CouldNotMeasure {
		fmt.Println(latency.CouldNotMeasureMarker)
	}
	if outcome != latency.Pass {
		os.Exit(1)
	}
}

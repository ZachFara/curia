// Command verdict is the last step of `make test-latency`: it reads the
// `go test -v` output of the three spaced passes on stdin (docs/decisions/0012),
// prints the table and exits 1 unless every gate passes.
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
	table, ok := latency.Judge(rs)
	fmt.Print(table)
	if !ok {
		os.Exit(1)
	}
}

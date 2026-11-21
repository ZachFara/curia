package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/usage"
)

// statuslineLimit bounds a status-line run's reading and storing. Claude
// Code runs the command about 300 ms after each API response and SIGTERMs
// a run still going when the next starts (probe S1), so a run gives up on
// a busy store well before then and prints what it parsed.
var statuslineLimit = 200 * time.Millisecond

// statuslineCells is the widest line printed.
const statuslineCells = 80

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "statusline",
		Short: "Claude Code's status line: store the session's cost and usage, print one line",
		Long: "Run by Claude Code as its statusLine command (curia install registers it).\n" +
			"Reads the status-line JSON on stdin, stores it as curia ingest --statusline\n" +
			"does, and prints one line: model, context, cost and the 5-hour window.\n" +
			"Always exits 0 and prints the line, empty when there is nothing to show.",
		Args: cobra.ArbitraryArgs,
		// Every argument reaches runStatusline, so a bad one is logged, not printed.
		DisableFlagParsing: true,
		Run:                runStatusline,
	})
}

// runStatusline stores the reading under statuslineLimit and prints its
// line on every path, panics included; nothing goes to stderr. A non-zero
// exit would blank the user's status line (probe S1).
func runStatusline(cmd *cobra.Command, args []string) {
	signal.Ignore(syscall.SIGPIPE) // as the hook: a closed pipe must not kill the run
	printed := false
	printLine := func(line string) {
		if !printed {
			printed = true
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), line)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			printLine("")
			ingestLog("statusline", usage.Doc{}, fmt.Errorf("panic: %s", sanitize.Line(fmt.Sprint(r), 200)), nil)
		}
	}()
	ctx, cancel := context.WithTimeout(cmd.Context(), statuslineLimit)
	defer cancel()
	doc, err := ingestRun(ctx, cmd.InOrStdin(), true)
	printLine(statuslineText(doc)) // first: the log may be slow
	ingestLog("statusline", doc, err, args)
}

// statuslineText is the line for doc: usage.StatusText (numbers, fixed
// words and the cleaned model name), cleaned once more as one line of at
// most statuslineCells.
func statuslineText(doc usage.Doc) string {
	return sanitize.Line(usage.StatusText(doc), statuslineCells)
}

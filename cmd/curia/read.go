package main

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/sanitize"
)

// Limits of `curia read` (plan section 4.8).
const (
	readDefaultLines = 50
	readMaxLines     = 2000
	readLineRunes    = 500 // runes of a printed line
)

// readLines is --lines; tests reset it.
var readLines = readDefaultLines

func init() {
	cmd := &cobra.Command{
		Use:   "read <pane>",
		Short: "Print the last lines of an agent session's pane",
		Long: "Prints the last --lines lines (1 to 2000, default 50) of <pane> (a pane id such\n" +
			"as %12), cleaned of escape sequences and control characters, each cut to 500\n" +
			"characters. Only the pane of a live agent session is read: a pane with no\n" +
			"session, or only an ended or stale one, is refused.",
		Args: cobra.ExactArgs(1),
		RunE: runRead,
	}
	cmd.Flags().IntVar(&readLines, "lines", readDefaultLines, "how many lines to print (1 to 2000)")
	rootCmd.AddCommand(cmd)
}

func runRead(cmd *cobra.Command, args []string) error {
	pane := args[0]
	if err := sendCheckPane(pane); err != nil {
		return err
	}
	if readLines < 1 || readLines > readMaxLines {
		return fmt.Errorf("--lines must be 1 to %d, not %d", readMaxLines, readLines)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ctx, tm := cmd.Context(), newTmuxClient()
	if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
		return err
	}
	// A shared pane is read: reading types nothing.
	if _, err = sendPaneSession(ctx, st, pane); err != nil && !sendShared(err) {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	tail, err := tm.CaptureTail(tctx, pane, readLines)
	if err != nil {
		return err
	}
	lines := readClean(tail, readLines)
	if len(lines) == 0 {
		return nil
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n"))
	return err
}

// readClean is the last n lines of tail, each cleaned by sanitize.Block and
// cut to readLineRunes runes. Each line is cleaned on its own, so the bound
// on one never drops the lines after it.
func readClean(tail string, n int) []string {
	if tail == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(strings.TrimSuffix(tail, "\n"), "\n") {
		clean := sanitize.Block(raw, readLineRunes*utf8.UTFMax)
		for _, l := range strings.Split(clean, "\n") {
			out = append(out, readCut(l))
		}
	}
	return out[max(0, len(out)-n):]
}

// readCut is s cut to at most readLineRunes runes.
func readCut(s string) string {
	i, k := 0, 0
	for i < len(s) && k < readLineRunes {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		k++
	}
	return s[:i]
}

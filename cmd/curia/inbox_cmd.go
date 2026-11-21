package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/ui"
)

// inboxJSON is the --json flag of `curia inbox`; tests reset it.
var inboxJSON bool

// inboxLineLimit bounds the summary of one printed item.
const inboxLineLimit = 120

// inboxKinds names an item's kind in the output, indexed by ui.InboxKind,
// and the key of its count.
var (
	inboxKinds  = [...]string{"decision", "prompt", "review", "assume"}
	inboxCounts = [...]string{"decisions", "prompts", "reviews", "assumptions"}
)

func init() {
	c := &cobra.Command{
		Use:   "inbox",
		Short: "Everything that needs you, in one list",
		Long: "Prints what the dashboard's inbox shows, from the same source: blocking\n" +
			"decisions in triage order, sessions waiting at a permission prompt, reviews\n" +
			"pending and assumptions to confirm, oldest first within a group. --json\n" +
			"prints {\"counts\": {decisions, prompts, reviews, assumptions}, \"items\": [{kind,\n" +
			"id, session, task, since, summary}]}; counts equal the status badge's D, W and R.",
		Args: cobra.NoArgs,
		RunE: runInbox,
	}
	c.Flags().BoolVar(&inboxJSON, "json", false, "print JSON instead of a table")
	rootCmd.AddCommand(c)
}

func runInbox(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	items, err := inboxSource{st: st}.Items(cmd.Context())
	if err != nil {
		return err
	}
	if inboxJSON {
		return inboxPrintJSON(cmd.OutOrStdout(), items)
	}
	return inboxPrintTable(cmd.OutOrStdout(), items, clock())
}

func inboxPrintTable(w io.Writer, items []ui.InboxItem, now time.Time) error {
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, "nothing needs you")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KIND\tAGE\tWHO\tSUMMARY")
	for _, it := range items {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", inboxKinds[it.Kind], ui.Age(it.At, now),
			ui.Cell(sanitize.Line(it.Label, 24)), ui.Cell(sanitize.Line(it.Summary, inboxLineLimit)))
	}
	return tw.Flush()
}

func inboxPrintJSON(w io.Writer, items []ui.InboxItem) error {
	type row struct {
		Kind    string    `json:"kind"`
		ID      string    `json:"id"`
		Session string    `json:"session"`
		Task    string    `json:"task"`
		Since   time.Time `json:"since"`
		Summary string    `json:"summary"`
	}
	out := struct {
		Counts map[string]int `json:"counts"`
		Items  []row          `json:"items"`
	}{map[string]int{}, make([]row, 0, len(items))} // never nil: nothing is [], not null
	for _, n := range inboxCounts {
		out.Counts[n] = 0
	}
	for _, it := range items {
		out.Counts[inboxCounts[it.Kind]]++
		out.Items = append(out.Items, row{
			inboxKinds[it.Kind], sanitize.Line(it.Key, 80), sanitize.Line(it.SessionID, 80),
			sanitize.Line(it.TaskID, 80), it.At.UTC(), sanitize.Line(it.Summary, inboxLineLimit),
		})
	}
	return writeJSON(w, out, "  ")
}

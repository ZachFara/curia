package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia reviews ls`; tests reset them.
var (
	reviewsStatus string
	reviewsJSON   bool
)

// Cell limits of the reviews table, in runes: with ids below R-1000 and
// T-1000 a row fits 80 columns (live check defect 10), as curia ls does by
// keeping its cells short. The whole text is in --json and curia review.
const (
	reviewsTitleLimit   = 12
	reviewsSummaryLimit = 20
	reviewsShortLimit   = 40
)

// reviewsStatusNames are the --status values besides "all", in the order
// the usage error names them.
var reviewsStatusNames = []model.ReviewStatus{
	model.ReviewPending, model.ReviewApproved, model.ReviewChanges, model.ReviewMerged, model.ReviewDropped,
}

func init() {
	parent := &cobra.Command{
		Use:   "reviews",
		Short: "List reviews",
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List reviews (read-only)",
		Long: "Lists reviews by --status: pending (the default), approved, changes_requested,\n" +
			"merged, dropped or all, in allocation (id) order. --json prints a JSON array.\n" +
			"The summary and the test result are what the agent reported, not something\n" +
			"Curia checked. It only reads, so it is allowed inside an agent session.",
		Args: cobra.NoArgs,
		RunE: runReviewsLs,
	}
	ls.Flags().StringVar(&reviewsStatus, "status", "pending", "pending, approved, changes_requested, merged, dropped or all")
	ls.Flags().BoolVar(&reviewsJSON, "json", false, "print a JSON array instead of a table")
	parent.AddCommand(ls)
	rootCmd.AddCommand(parent)
}

// reviewsStatuses maps a --status value to the statuses it lists; nil means all.
func reviewsStatuses(name string) ([]model.ReviewStatus, error) {
	if name == "all" {
		return nil, nil
	}
	for _, s := range reviewsStatusNames {
		if string(s) == name {
			return []model.ReviewStatus{s}, nil
		}
	}
	valid := make([]string, 0, len(reviewsStatusNames)+1)
	for _, s := range reviewsStatusNames {
		valid = append(valid, string(s))
	}
	return nil, fmt.Errorf("invalid --status %q: want %s or all", sanitize.Line(name, reviewsShortLimit), strings.Join(valid, ", "))
}

func runReviewsLs(cmd *cobra.Command, _ []string) error {
	statuses, err := reviewsStatuses(reviewsStatus)
	if err != nil {
		return err
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
	rs, err := st.ListReviews(cmd.Context(), store.ReviewFilter{Statuses: statuses})
	if err != nil {
		return err
	}
	if reviewsJSON {
		return reviewsPrintJSON(cmd.OutOrStdout(), rs)
	}
	titles, err := reviewsTitles(cmd, st, rs)
	if err != nil {
		return err
	}
	return reviewsPrintTable(cmd.OutOrStdout(), rs, titles, clock())
}

// reviewsTitles returns the title of each task rs mention, one primary-key
// lookup per task (ListTasks would scan the whole table).
func reviewsTitles(cmd *cobra.Command, st *store.Store, rs []model.Review) (map[string]string, error) {
	titles := make(map[string]string)
	for _, r := range rs {
		if _, seen := titles[r.TaskID]; seen {
			continue
		}
		t, found, err := st.GetTask(cmd.Context(), r.TaskID)
		if err != nil {
			return nil, err
		}
		if found {
			titles[r.TaskID] = t.Title
		} else {
			titles[r.TaskID] = ""
		}
	}
	return titles, nil
}

// reviewsRow is one review in `curia reviews ls --json`: the columns of the
// reviews table except n; a zero time is null.
type reviewsRow struct {
	ID          string             `json:"id"`
	TaskID      string             `json:"task_id"`
	SessionID   string             `json:"session_id"`
	Summary     string             `json:"summary"`
	TestsStatus model.TestsStatus  `json:"tests_status"`
	TestsDetail string             `json:"tests_detail"`
	Status      model.ReviewStatus `json:"status"`
	ApprovedHow model.ApproveHow   `json:"approved_how"`
	DiffStat    string             `json:"diff_stat"`
	Head        string             `json:"head"`
	PRURL       string             `json:"pr_url"`
	MyNotes     string             `json:"my_notes"`
	NoteMessage string             `json:"note_message"`
	ClosedVia   model.AnsweredVia  `json:"closed_via"`
	CreatedAt   *time.Time         `json:"created_at"`
	UpdatedAt   *time.Time         `json:"updated_at"`
	ClosedAt    *time.Time         `json:"closed_at"`
}

func reviewsToRow(r model.Review) reviewsRow {
	return reviewsRow{
		ID: r.ID, TaskID: r.TaskID, SessionID: r.SessionID, Summary: r.Summary, TestsStatus: r.Tests,
		TestsDetail: r.TestsDetail, Status: r.Status, ApprovedHow: r.ApprovedHow, DiffStat: r.DiffStat,
		Head: r.Head, PRURL: r.PRURL, MyNotes: r.Notes, NoteMessage: r.NoteMessage, ClosedVia: r.ClosedVia,
		CreatedAt: jsonTime(r.CreatedAt), UpdatedAt: jsonTime(r.UpdatedAt), ClosedAt: jsonTime(r.ClosedAt),
	}
}

func reviewsPrintJSON(w io.Writer, rs []model.Review) error {
	rows := make([]reviewsRow, 0, len(rs)) // never nil, so no reviews is []
	for _, r := range rs {
		rows = append(rows, reviewsToRow(r))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		return err
	}
	_, err := io.WriteString(w, escapeUnprintable(buf.String()))
	return err
}

// reviewsCell makes agent or stored text, which is untrusted, safe as one table cell.
func reviewsCell(s string, limit int) string { return ui.Cell(sanitize.Line(s, limit)) }

// reviewsStatusText is the status as the table words it.
func reviewsStatusText(r model.Review) string {
	switch r.Status {
	case model.ReviewChanges:
		return "changes asked"
	case model.ReviewApproved:
		if r.ApprovedHow != "" {
			return "approved (" + string(r.ApprovedHow) + ")"
		}
	}
	return string(r.Status)
}

// reviewsTestsText is the test result, under the header AGENT TESTS, which
// marks it as the agent's own claim.
func reviewsTestsText(r model.Review) string {
	return strings.ReplaceAll(string(r.Tests), "_", " ")
}

// reviewsFirstLine is the first line of s, whatever the line break.
func reviewsFirstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n\u0085  "); i >= 0 {
		return s[:i]
	}
	return s
}

// reviewsTaskText is the task's id and, when known, its title.
func reviewsTaskText(id, title string) string {
	id = sanitize.Line(id, reviewsShortLimit)
	if title = sanitize.Line(title, reviewsTitleLimit); title != "" {
		return id + " " + title
	}
	return id
}

// reviewsPrintTable prints one row per review, in allocation (id) order;
// titles holds each task's title by task id.
func reviewsPrintTable(w io.Writer, rs []model.Review, titles map[string]string, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tAGE\tTASK\tSTATUS\tAGENT TESTS\tSUMMARY")
	for _, r := range rs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			reviewsCell(r.ID, reviewsShortLimit), reviewsCell(ui.Age(r.CreatedAt, now), reviewsShortLimit),
			ui.Cell(reviewsTaskText(r.TaskID, titles[r.TaskID])), reviewsCell(reviewsStatusText(r), reviewsShortLimit),
			reviewsCell(reviewsTestsText(r), reviewsShortLimit),
			reviewsCell(reviewsFirstLine(r.Summary), reviewsSummaryLimit))
	}
	return tw.Flush()
}

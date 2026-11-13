package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia decisions`; tests reset them.
var (
	decisionsStatus       string
	decisionsJSON         bool
	decisionsScope        string
	decisionsSupersede    bool
	decisionsSupersededBy string
)

// Cell limits of the decisions table.
const (
	decisionsQuestionLimit = 70
	decisionsAnswerLimit   = 40
)

func init() {
	parent := &cobra.Command{
		Use:   "decisions",
		Short: "List and edit decision tickets",
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List decision tickets",
		Long: "Expires assume tickets that are due, then lists tickets by --status: open\n" +
			"(the default; with it every answer the agent has not got yet), decided\n" +
			"(answered, confirmed, overridden), closed (withdrawn, expired) or all, in\n" +
			"allocation (id) order. --json prints a JSON array.",
		Args: cobra.NoArgs,
		RunE: runDecisionsLs,
	}
	ls.Flags().StringVar(&decisionsStatus, "status", "open", "open (and answers not yet delivered), decided, closed or all")
	ls.Flags().BoolVar(&decisionsJSON, "json", false, "print a JSON array instead of a table")
	edit := &cobra.Command{
		Use:   "edit <id>",
		Short: "Rescope or supersede a decided ticket (the user only)",
		Args:  cobra.ExactArgs(1),
		RunE:  runDecisionsEdit,
	}
	edit.Flags().StringVar(&decisionsScope, "scope", "", "new scope: task, repo or global")
	edit.Flags().BoolVar(&decisionsSupersede, "supersede", false, "mark the ticket superseded")
	edit.Flags().StringVar(&decisionsSupersededBy, "superseded-by", "", "id of the decided ticket that replaces it (implies --supersede)")
	parent.AddCommand(ls, edit)
	rootCmd.AddCommand(parent)
}

// refuseInsideAgent returns an error when the command runs inside an agent
// session: Claude Code sets CLAUDECODE in every command it runs. Commands
// only the user may run call it first.
func refuseInsideAgent() error {
	if os.Getenv("CLAUDECODE") != "" {
		return errors.New("refusing to run inside an agent session (CLAUDECODE is set): only the user may do this")
	}
	return nil
}

// decisionsStatuses maps a --status value to the statuses it lists; nil
// means all.
func decisionsStatuses(name string) ([]model.DecisionStatus, error) {
	switch name {
	case "open":
		return []model.DecisionStatus{model.StatusOpen}, nil
	case "decided":
		return []model.DecisionStatus{model.StatusAnswered, model.StatusConfirmed, model.StatusOverridden}, nil
	case "closed":
		return []model.DecisionStatus{model.StatusWithdrawn, model.StatusExpired}, nil
	case "all":
		return nil, nil
	}
	return nil, fmt.Errorf("invalid --status %q: want open, decided, closed or all", sanitize.Line(name, 40))
}

func runDecisionsLs(cmd *cobra.Command, _ []string) error {
	statuses, err := decisionsStatuses(decisionsStatus)
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
	now := clock()
	if _, err = st.ExpireDecisions(cmd.Context(), now); err != nil {
		return err
	}
	// The default listing also shows the answers the agent has not got yet:
	// an agent may be waiting on one with nothing else showing it (M3).
	ds, err := st.ListDecisions(cmd.Context(), store.DecisionFilter{Statuses: statuses, Undelivered: decisionsStatus == "open"})
	if err != nil {
		return err
	}
	if decisionsJSON {
		return decisionsPrintJSON(cmd.OutOrStdout(), ds)
	}
	return decisionsPrintTable(cmd.OutOrStdout(), ds, now)
}

// decisionsRow is one ticket in `curia decisions ls --json`. Its keys are
// the columns of the decisions table except n; a zero time is null.
type decisionsRow struct {
	ID             string               `json:"id"`
	SessionID      string               `json:"session_id"`
	TaskID         string               `json:"task_id"`
	Repo           string               `json:"repo"`
	Question       string               `json:"question"`
	Context        string               `json:"context"`
	Options        []model.Option       `json:"options"`
	Recommendation string               `json:"recommendation"`
	Rationale      string               `json:"rationale"`
	Reversibility  model.Reversibility  `json:"reversibility"`
	Mode           model.Mode           `json:"mode"`
	Assumption     string               `json:"assumption"`
	Scope          model.Scope          `json:"scope"`
	Tags           []string             `json:"tags"`
	Status         model.DecisionStatus `json:"status"`
	AnswerKey      string               `json:"answer_key"`
	Answer         string               `json:"answer"`
	AnswerReason   string               `json:"answer_reasoning"`
	AnsweredVia    model.AnsweredVia    `json:"answered_via"`
	PrecedentOf    string               `json:"precedent_of"`
	DiffersFrom    string               `json:"differs_from"`
	DiffersWhy     string               `json:"differs_why"`
	WithdrawReason string               `json:"withdraw_reason"`
	SupersededBy   string               `json:"superseded_by"`
	ArgsHash       string               `json:"args_hash"`
	Delivery       model.Delivery       `json:"delivery"`
	DeliveredVia   model.DeliveryVia    `json:"delivered_via"`
	CreatedAt      *time.Time           `json:"created_at"`
	ExpiresAt      *time.Time           `json:"expires_at"`
	AnsweredAt     *time.Time           `json:"answered_at"`
	ClosedAt       *time.Time           `json:"closed_at"`
	SupersededAt   *time.Time           `json:"superseded_at"`
	DeliveredAt    *time.Time           `json:"delivered_at"`
	ConfirmedAt    *time.Time           `json:"confirmed_at"`
}

func decisionsToRow(d model.Decision) decisionsRow {
	opts, tags := d.Options, d.Tags
	if opts == nil {
		opts = []model.Option{}
	}
	if tags == nil {
		tags = []string{}
	}
	return decisionsRow{
		ID: d.ID, SessionID: d.SessionID, TaskID: d.TaskID, Repo: d.Repo, Question: d.Question,
		Context: d.Context, Options: opts, Recommendation: d.Recommendation, Rationale: d.Rationale,
		Reversibility: d.Reversibility, Mode: d.Mode, Assumption: d.Assumption, Scope: d.Scope, Tags: tags,
		Status: d.Status, AnswerKey: d.AnswerKey, Answer: d.Answer, AnswerReason: d.AnswerReasoning,
		AnsweredVia: d.AnsweredVia, PrecedentOf: d.PrecedentOf, DiffersFrom: d.DiffersFrom,
		DiffersWhy: d.DiffersWhy, WithdrawReason: d.WithdrawReason, SupersededBy: d.SupersededBy,
		ArgsHash: d.ArgsHash, Delivery: d.Delivery, DeliveredVia: d.DeliveredVia,
		CreatedAt: jsonTime(d.CreatedAt), ExpiresAt: jsonTime(d.ExpiresAt), AnsweredAt: jsonTime(d.AnsweredAt),
		ClosedAt: jsonTime(d.ClosedAt), SupersededAt: jsonTime(d.SupersededAt), DeliveredAt: jsonTime(d.DeliveredAt),
		ConfirmedAt: jsonTime(d.ConfirmedAt),
	}
}

func decisionsPrintJSON(w io.Writer, ds []model.Decision) error {
	rows := make([]decisionsRow, 0, len(ds)) // never nil, so no tickets is []
	for _, d := range ds {
		rows = append(rows, decisionsToRow(d))
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

// decisionsCell makes ticket text, which is untrusted, safe as one table cell.
func decisionsCell(s string, limit int) string { return ui.Cell(sanitize.Line(s, limit)) }

// decisionsPrintTable prints one row per ticket, in allocation (id) order,
// with where it was answered (VIA) and how far its delivery got (M3).
func decisionsPrintTable(w io.Writer, ds []model.Decision, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tMODE\tAGE\tREPO\tQUESTION\tANSWER\tVIA\tDELIVERY")
	for _, d := range ds {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			decisionsCell(d.ID, decisionsQuestionLimit), decisionsCell(decisionsStatusText(d), decisionsQuestionLimit),
			decisionsCell(string(d.Mode), decisionsQuestionLimit), decisionsCell(ui.Age(d.CreatedAt, now), decisionsQuestionLimit), decisionsCell(ui.RepoName(d.Repo), decisionsQuestionLimit),
			decisionsCell(d.Question, decisionsQuestionLimit),
			decisionsCell(d.Answer, decisionsAnswerLimit), decisionsCell(decisionsOr(string(d.AnsweredVia), "-"), decisionsQuestionLimit),
			decisionsCell(decisionsDeliveryText(d), decisionsQuestionLimit))
	}
	return tw.Flush()
}

// decisionsOr is s, or alt when s is empty.
func decisionsOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// decisionsStatusText is d's status, marked when the user superseded it.
func decisionsStatusText(d model.Decision) string {
	if !d.SupersededAt.IsZero() {
		return string(d.Status) + " (superseded)"
	}
	return string(d.Status)
}

// decisionsDeliveryText is how far d's delivery got: "delivered
// (stop_hook)", "typed, unconfirmed" for a typed answer no submitted prompt
// or check_decision has confirmed yet (Ruling 86), "queued" or "none".
func decisionsDeliveryText(d model.Decision) string {
	switch {
	case d.Delivery == model.DeliveryDelivered && d.DeliveredVia == model.ViaSendKeys && d.ConfirmedAt.IsZero():
		return "typed, unconfirmed"
	case d.Delivery == model.DeliveryDelivered && d.DeliveredVia != "":
		return string(d.Delivery) + " (" + string(d.DeliveredVia) + ")"
	}
	return string(d.Delivery)
}

func runDecisionsEdit(cmd *cobra.Command, args []string) error {
	if err := refuseInsideAgent(); err != nil {
		return err
	}
	id := args[0]
	if !decision.ValidID(id) {
		return fmt.Errorf("invalid decision id %q: want D-<number>", sanitize.Line(id, 40))
	}
	e, err := decisionsEditFromFlags()
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
	// The previous scope is only for the message; EditDecision decides.
	before, _, err := st.GetDecision(cmd.Context(), id)
	if err != nil {
		return err
	}
	after, err := st.EditDecision(cmd.Context(), id, e, clock())
	if err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	out := cmd.OutOrStdout()
	if e.Scope != "" {
		_, _ = fmt.Fprintf(out, "%s scope %s -> %s\n", id, before.Scope, after.Scope)
	}
	switch {
	case e.SupersededBy != "":
		_, _ = fmt.Fprintf(out, "%s superseded by %s\n", id, after.SupersededBy)
	case e.Supersede:
		_, _ = fmt.Fprintf(out, "%s superseded\n", id)
	}
	if e.Supersede && after.SessionID != "" && after.Delivery == model.DeliveryQueued {
		// The retraction goes by the Stop hook or check_decision (Ruling 82).
		_, _ = fmt.Fprintf(out, "%s retraction queued for the agent's Stop hook, or tell the agent to call check_decision %s\n", id, id)
	}
	return nil
}

// decisionsEditFromFlags builds the edit the flags ask for.
func decisionsEditFromFlags() (model.DecisionEdit, error) {
	e := model.DecisionEdit{
		Scope:        model.Scope(decisionsScope),
		Supersede:    decisionsSupersede || decisionsSupersededBy != "",
		SupersededBy: decisionsSupersededBy,
	}
	switch e.Scope {
	case "", model.ScopeTask, model.ScopeRepo, model.ScopeGlobal:
	default:
		return e, fmt.Errorf("invalid --scope %q: want task, repo or global", sanitize.Line(decisionsScope, 40))
	}
	if e.SupersededBy != "" && !decision.ValidID(e.SupersededBy) {
		return e, fmt.Errorf("invalid --superseded-by %q: want D-<number>", sanitize.Line(e.SupersededBy, 40))
	}
	if e.Scope == "" && !e.Supersede {
		return e, errors.New("nothing to change: give --scope, --supersede or --superseded-by")
	}
	return e, nil
}

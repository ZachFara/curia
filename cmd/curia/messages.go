package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia messages ls`; tests reset them.
var (
	messagesStatus string
	messagesJSON   bool
)

// messagesTextLimit is the width of the TEXT and TO cells.
const (
	messagesTextLimit = 70
	messagesToLimit   = 30
)

func init() {
	parent := &cobra.Command{
		Use:   "messages",
		Short: "List and cancel messages",
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List messages",
		Long: "Cancels held messages whose say process died, then lists messages by --status:\n" +
			"open (the default: held, queued, and typed into a pane but not confirmed),\n" +
			"all, or one of held, queued, delivered, undone, canceled. --json prints a JSON array.",
		Args: cobra.NoArgs,
		RunE: runMessagesLs,
	}
	ls.Flags().StringVar(&messagesStatus, "status", "open", "open, all, or one status")
	ls.Flags().BoolVar(&messagesJSON, "json", false, "print a JSON array instead of a table")
	cancel := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a queued message (the user only)",
		Args:  cobra.ExactArgs(1),
		RunE:  runMessagesCancel,
	}
	parent.AddCommand(ls, cancel)
	rootCmd.AddCommand(parent)
}

// messagesStatuses maps a --status value to the statuses to read; nil is all.
func messagesStatuses(name string) ([]model.MessageStatus, error) {
	switch name {
	case "open":
		return []model.MessageStatus{model.MsgHeld, model.MsgQueued, model.MsgDelivered}, nil
	case "all":
		return nil, nil
	case "held", "queued", "delivered", "undone", "canceled":
		return []model.MessageStatus{model.MessageStatus(name)}, nil
	}
	return nil, fmt.Errorf("invalid --status %q: want open, all, held, queued, delivered, undone or canceled", sanitize.Line(name, 40))
}

// messagesTyped reports whether m was typed into a pane and not confirmed.
func messagesTyped(m model.Message) bool {
	return m.Status == model.MsgDelivered && m.DeliveredVia == model.ViaSendKeys && m.ConfirmedAt.IsZero()
}

func runMessagesLs(cmd *cobra.Command, _ []string) error {
	statuses, err := messagesStatuses(messagesStatus)
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
	if _, err = st.SweepHeld(cmd.Context(), now); err != nil {
		return err
	}
	ms, err := st.ListMessages(cmd.Context(), store.MessageFilter{Statuses: statuses})
	if err != nil {
		return err
	}
	if messagesStatus == "open" {
		ms = slices.DeleteFunc(ms, func(m model.Message) bool { return m.Status == model.MsgDelivered && !messagesTyped(m) })
	}
	if messagesJSON {
		return messagesPrintJSON(cmd.OutOrStdout(), ms)
	}
	sessions, err := st.ListSessions(cmd.Context(), store.ListFilter{IncludeEnded: true})
	if err != nil {
		return err
	}
	states := make(map[string]model.State, len(sessions))
	for _, s := range sessions {
		states[s.ID] = s.State
	}
	return messagesPrintTable(cmd.OutOrStdout(), ms, states, now)
}

// messagesRow is one message in `curia messages ls --json`; its keys are
// the columns of the messages table except n, and a zero time is null.
type messagesRow struct {
	ID              string              `json:"id"`
	Text            string              `json:"text"`
	Kind            model.MessageKind   `json:"kind"`
	Sender          model.Sender        `json:"sender"`
	SenderSession   string              `json:"sender_session"`
	TargetSession   string              `json:"target_session"`
	TargetTask      string              `json:"target_task"`
	TargetRepo      string              `json:"target_repo"`
	SuggestedTarget string              `json:"suggested_target"`
	SuggestedKind   model.MessageKind   `json:"suggested_kind"`
	RunnerUp        string              `json:"runner_up"`
	Confidence      *float64            `json:"confidence"`
	RoutedBy        model.RoutedBy      `json:"routed_by"`
	AutoEligible    bool                `json:"auto_eligible"`
	Status          model.MessageStatus `json:"status"`
	DeliveredVia    model.DeliveryVia   `json:"delivered_via"`
	ResultRef       string              `json:"result_ref"`
	CreatedAt       *time.Time          `json:"created_at"`
	HoldUntil       *time.Time          `json:"hold_until"`
	DeliveredAt     *time.Time          `json:"delivered_at"`
	ConfirmedAt     *time.Time          `json:"confirmed_at"`
}

func messagesToRow(m model.Message) messagesRow {
	var conf *float64
	if m.HasConfidence {
		conf = &m.Confidence
	}
	return messagesRow{
		ID: m.ID, Text: m.Text, Kind: m.Kind, Sender: m.Sender, SenderSession: m.SenderSession,
		TargetSession: m.TargetSession, TargetTask: m.TargetTask, TargetRepo: m.TargetRepo,
		SuggestedTarget: m.SuggestedTarget, SuggestedKind: m.SuggestedKind, RunnerUp: m.RunnerUp,
		Confidence: conf, RoutedBy: m.RoutedBy, AutoEligible: m.AutoEligible, Status: m.Status,
		DeliveredVia: m.DeliveredVia, ResultRef: m.ResultRef, CreatedAt: jsonTime(m.CreatedAt),
		HoldUntil: jsonTime(m.HoldUntil), DeliveredAt: jsonTime(m.DeliveredAt), ConfirmedAt: jsonTime(m.ConfirmedAt),
	}
}

// messagesPrintJSON prints ms as a JSON array, escaped as decisions ls is.
func messagesPrintJSON(w io.Writer, ms []model.Message) error {
	rows := make([]messagesRow, 0, len(ms)) // never nil, so no messages is []
	for _, m := range ms {
		rows = append(rows, messagesToRow(m))
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

// messagesCell makes untrusted text safe as one table cell.
func messagesCell(s string, limit int) string { return ui.Cell(sanitize.Line(s, limit)) }

// messagesTo is where m is addressed: its session, task or repository.
func messagesTo(m model.Message) string {
	switch {
	case m.TargetSession != "":
		return m.TargetSession
	case m.TargetTask != "":
		return m.TargetTask
	case m.TargetRepo != "":
		return ui.RepoName(m.TargetRepo)
	}
	return ""
}

// messagesStatusText is how far m got: "typed, unconfirmed", "delivered
// (stop_hook)", "applied D-40", or its status; a message an agent has not
// received yet is marked when its target session can no longer collect it.
func messagesStatusText(m model.Message, states map[string]model.State) string {
	text := string(m.Status)
	switch {
	case messagesTyped(m):
		text = "typed, unconfirmed"
	case m.Status == model.MsgDelivered && m.DeliveredVia == model.ViaApplied:
		return "applied " + m.ResultRef
	case m.Status == model.MsgDelivered && m.DeliveredVia != "":
		return text + " (" + string(m.DeliveredVia) + ")"
	case m.Status != model.MsgQueued && m.Status != model.MsgHeld:
		return text
	}
	if m.Kind != model.KindInstruction || m.TargetSession == "" {
		return text
	}
	switch state, known := states[m.TargetSession]; {
	case !known:
		return text + " (no session row)"
	case state == model.StateEnded || state == model.StateStale:
		return text + " (session " + string(state) + ")"
	}
	return text
}

func messagesPrintTable(w io.Writer, ms []model.Message, states map[string]model.State, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tAGE\tKIND\tTO\tSTATUS\tTEXT")
	for _, m := range ms {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			messagesCell(m.ID, messagesToLimit), messagesCell(ui.Age(m.CreatedAt, now), messagesToLimit),
			messagesCell(string(m.Kind), messagesToLimit), messagesCell(messagesTo(m), messagesToLimit),
			messagesCell(messagesStatusText(m, states), messagesTextLimit), messagesCell(m.Text, messagesTextLimit))
	}
	return tw.Flush()
}

func runMessagesCancel(cmd *cobra.Command, args []string) error {
	if err := refuseInsideAgent(); err != nil {
		return err
	}
	id := args[0]
	if !message.ValidID(id) {
		return fmt.Errorf("invalid message id %q: want M-<number>", sanitize.Line(id, 40))
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
	if _, err = st.CancelMessage(cmd.Context(), id); err == nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s canceled\n", id)
		return nil
	}
	switch {
	case errors.Is(err, store.ErrNoMessage):
		return fmt.Errorf("%s: no such message", id)
	case errors.Is(err, store.ErrNotQueued):
		// The store decided atomically; say what the message is now.
		cur, found, gerr := st.GetMessage(cmd.Context(), id)
		if gerr != nil || !found {
			return fmt.Errorf("%s is not queued, so it was not canceled", id)
		}
		return fmt.Errorf("%s is %s, so it was not canceled", id, messagesStatusText(cur, nil))
	}
	return err
}

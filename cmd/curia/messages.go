package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
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
		Use:         "cancel <id>",
		Short:       "Cancel a queued message (the user only)",
		Annotations: userOnlyMark(),
		Args:        cobra.ExactArgs(1),
		RunE:        runMessagesCancel,
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
	BroadcastID     string              `json:"broadcast_id"` // "" unless the message is one row of a broadcast
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
		BroadcastID: m.BroadcastID,
	}
}

// messagesPrintJSON prints ms as a JSON array, escaped as decisions ls is.
func messagesPrintJSON(w io.Writer, ms []model.Message) error {
	rows := make([]messagesRow, 0, len(ms)) // never nil, so no messages is []
	for _, m := range ms {
		rows = append(rows, messagesToRow(m))
	}
	return writeJSON(w, rows, "  ")
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

// messagesTargetsShown bounds the per-target lines under a broadcast.
const messagesTargetsShown = 5

func messagesPrintTable(w io.Writer, ms []model.Message, states map[string]model.State, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tAGE\tKIND\tTO\tSTATUS\tTEXT")
	row := func(id string, m model.Message, to, status string) {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			messagesCell(id, messagesToLimit), messagesCell(ui.Age(m.CreatedAt, now), messagesToLimit),
			messagesCell(string(m.Kind), messagesToLimit), messagesCell(to, messagesToLimit),
			messagesCell(status, messagesTextLimit), messagesCell(m.Text, messagesTextLimit))
	}
	done := map[string]bool{}
	for _, m := range ms {
		if m.BroadcastID == "" {
			row(m.ID, m, messagesTo(m), messagesStatusText(m, states))
			continue
		}
		if done[m.BroadcastID] {
			continue
		}
		done[m.BroadcastID] = true
		var group []model.Message // a broadcast is one entry, its targets under it
		for _, x := range ms {
			if x.BroadcastID == m.BroadcastID {
				group = append(group, x)
			}
		}
		row(m.BroadcastID, m, fmt.Sprintf("%d sessions", len(group)), messagesTally(group, states))
		for i, x := range group {
			if i == messagesTargetsShown {
				_, _ = fmt.Fprintf(tw, "\t\t\t\t%s\t\n", messagesCell(fmt.Sprintf("… and %d more", len(group)-i), messagesToLimit))
				break
			}
			_, _ = fmt.Fprintf(tw, "  %s\t\t\t%s\t%s\t\n", messagesCell(x.ID, messagesToLimit),
				messagesCell(messagesTo(x), messagesToLimit), messagesCell(messagesStatusText(x, states), messagesTextLimit))
		}
	}
	return tw.Flush()
}

// messagesTally counts a broadcast's rows by their status text, in the
// order each first appears: "2 delivered (send_keys), 1 queued".
func messagesTally(group []model.Message, states map[string]model.State) string {
	var order []string
	n := map[string]int{}
	for _, m := range group {
		t := messagesStatusText(m, states)
		if n[t] == 0 {
			order = append(order, t)
		}
		n[t]++
	}
	parts := make([]string, 0, len(order))
	for _, t := range order {
		parts = append(parts, fmt.Sprintf("%d %s", n[t], t))
	}
	return strings.Join(parts, ", ")
}

func runMessagesCancel(cmd *cobra.Command, args []string) error {
	if err := userOnly("messages cancel", "cancel a message"); err != nil {
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

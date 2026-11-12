package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia decide`; tests reset them.
var (
	decideOption, decideText, decideReason, decideScope string
	decideAccept, decideJSON                            bool
)

// decideMaxStdin is the most `--reason -` reads from stdin.
const decideMaxStdin = 64 << 10

// decideAnswerLimit bounds a printed answer: a key, ": " and a label, or
// free text.
const decideAnswerLimit = decision.MaxAnswerText

// decideNoteLimit bounds a printed delivery note.
const decideNoteLimit = 300

// errDecideInsideAgent refuses curia decide inside an agent session: no
// agent may answer a decision (spec section 7).
var errDecideInsideAgent = errors.New("curia decide cannot run inside an agent session (CLAUDECODE is set): only the user may answer a decision")

func init() {
	cmd := &cobra.Command{
		Use:   "decide [<id>]",
		Short: "Answer decision tickets (the user only)",
		Long: "With no id, opens the decision queue (run it in a tmux popup) to answer\n" +
			"tickets one by one. With an id, answers that ticket with exactly one of\n" +
			"--option, --accept (the recommendation) or --text, then delivers the\n" +
			"answer: typed into the agent's pane when it is idle, otherwise queued for\n" +
			"its Stop hook or its own check_decision. --reason - reads the reason from\n" +
			"stdin. Refuses to run inside an agent session.",
		Args: cobra.MaximumNArgs(1),
		RunE: runDecide,
	}
	f := cmd.Flags()
	f.StringVar(&decideOption, "option", "", "answer with this option key")
	f.BoolVar(&decideAccept, "accept", false, "answer with the ticket's recommendation")
	f.StringVar(&decideText, "text", "", "answer with this free text")
	f.StringVar(&decideReason, "reason", "", "the reason for the answer (- reads it from stdin)")
	f.StringVar(&decideScope, "scope", "", "how far the answer applies: task, repo or global")
	f.BoolVar(&decideJSON, "json", false, "print the outcome as one JSON object")
	rootCmd.AddCommand(cmd)
}

func runDecide(cmd *cobra.Command, args []string) error {
	if refuseInsideAgent() != nil {
		return errDecideInsideAgent
	}
	if len(args) == 0 {
		if decideOption != "" || decideAccept || decideText != "" || decideReason != "" || decideScope != "" || decideJSON {
			return errors.New("--option, --accept, --text, --reason, --scope and --json need a decision id")
		}
		return runDecideView()
	}
	id := args[0]
	if !decision.ValidID(id) {
		return fmt.Errorf("invalid decision id %q: want D-<number>", sanitize.Line(id, 40))
	}
	a, err := decideAnswerFromFlags(cmd.InOrStdin())
	if err != nil {
		return err
	}
	return runDecideAnswer(cmd, id, a)
}

// decideAnswerFromFlags builds the answer the flags give, without its key
// for --accept, which comes from the ticket.
func decideAnswerFromFlags(stdin io.Reader) (model.Answer, error) {
	n := 0
	for _, set := range []bool{decideOption != "", decideAccept, decideText != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return model.Answer{}, errors.New("give exactly one of --option <key>, --accept or --text <text>")
	}
	a := model.Answer{Key: decideOption, Text: decideText, Reasoning: decideReason, Scope: model.Scope(decideScope), Via: model.AnsweredCLI}
	switch a.Scope {
	case "", model.ScopeTask, model.ScopeRepo, model.ScopeGlobal:
	default:
		return model.Answer{}, fmt.Errorf("invalid --scope %q: want task, repo or global", sanitize.Line(decideScope, 40))
	}
	if decideReason == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, decideMaxStdin+1))
		if err != nil {
			return model.Answer{}, fmt.Errorf("read the reason from stdin: %w", err)
		}
		if len(b) > decideMaxStdin {
			return model.Answer{}, errors.New("the reason on stdin is over 64 KiB")
		}
		a.Reasoning = string(b)
	}
	return a, nil
}

// runDecideAnswer answers ticket id with a, delivers it and prints the
// outcome.
func runDecideAnswer(cmd *cobra.Command, id string, a model.Answer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	ctx := cmd.Context()
	if decideAccept {
		d, found, gerr := st.GetDecision(ctx, id)
		switch {
		case gerr != nil:
			return gerr
		case !found:
			return decideErr{fmt.Sprintf("no decision %s", id), store.ErrNotFound}
		}
		a.Key = d.Recommendation
	}
	out, err := newDecideSource(st, newTmuxClient(), logger).Answer(ctx, id, a)
	if err != nil {
		return err
	}
	if decideJSON {
		return decidePrintJSON(cmd.OutOrStdout(), out)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), decideLine(out))
	return err
}

// runDecideView runs the decide view in the alternate screen until it
// quits. Answers given over SSH are recorded as such.
func runDecideView() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	via := model.AnsweredPopup
	if os.Getenv("SSH_CONNECTION") != "" {
		via = model.AnsweredSSH
	}
	src := newDecideSource(st, newTmuxClient(), logger)
	return runProgram(ui.NewDecide(src, clock, ui.DecideOptions{Via: via}), tea.WithAltScreen())
}

// decideSource is the decide view's ui.DecideSource over the store.
type decideSource struct {
	st  *store.Store
	tm  tmux.Client
	log *slog.Logger
}

// newDecideSource returns the ui.DecideSource of curia decide: it reads
// the open tickets in queue order and answers through deliver.Answer.
func newDecideSource(st *store.Store, tm tmux.Client, logger *slog.Logger) ui.DecideSource {
	return decideSource{st: st, tm: tm, log: logger}
}

// Tickets expires the assume tickets that are due, then returns the open
// tickets in queue order (decision.Triage).
func (s decideSource) Tickets(ctx context.Context) ([]model.Ticket, error) {
	now := clock()
	if _, err := s.st.ExpireDecisions(ctx, now); err != nil {
		return nil, err
	}
	t, err := s.st.OpenTickets(ctx, now)
	if err != nil {
		return nil, err
	}
	return decision.Triage(t, now), nil
}

// Answer answers ticket id with a and delivers it (deliver.Answer). A
// ticket already answered is refused by the store and nothing is
// delivered again; the error says so in the user's words.
func (s decideSource) Answer(ctx context.Context, id string, a model.Answer) (model.Outcome, error) {
	out, err := deliver.Answer(ctx, s.st, s.tm, s.log, id, a, clock())
	if err != nil {
		return model.Outcome{}, decideExplain(ctx, s.st, id, a, err)
	}
	return out, nil
}

// decideErr is an answer error told in the user's words; errors.Is still
// matches the error it explains.
type decideErr struct {
	msg string
	err error
}

func (e decideErr) Error() string { return e.msg }
func (e decideErr) Unwrap() error { return e.err }

// decideExplain tells err, from answering ticket id with a, in the user's
// words.
func decideExplain(ctx context.Context, st *store.Store, id string, a model.Answer, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return decideErr{"no decision " + id, err}
	case errors.Is(err, store.ErrNotOpen):
		d, _, gerr := st.GetDecision(ctx, id)
		switch {
		case gerr != nil:
			return decideErr{id + " is not open", err}
		case d.Status == model.StatusWithdrawn:
			return decideErr{id + " was withdrawn", err}
		case d.Status == model.StatusExpired || d.Status == model.StatusOpen: // open: past its deadline
			return decideErr{id + " has expired", err}
		}
		return decideErr{id + " is already answered", err}
	case errors.Is(err, decision.ErrBadAnswer):
		return decideBadAnswer(ctx, st, id, a, err)
	}
	return err
}

// decideBadAnswer explains a refused answer, naming the ticket's valid keys.
func decideBadAnswer(ctx context.Context, st *store.Store, id string, a model.Answer, err error) error {
	d, _, gerr := st.GetDecision(ctx, id)
	if gerr != nil {
		return err
	}
	keys := make([]string, 0, len(d.Options))
	known := false
	for _, o := range d.Options {
		keys = append(keys, sanitize.Line(o.Key, decision.MaxKey))
		known = known || o.Key == a.Key
	}
	valid := "valid keys are " + strings.Join(keys, ", ")
	if a.Key != "" && !known {
		return decideErr{fmt.Sprintf("%s has no option %q; %s", id, sanitize.Line(a.Key, decision.MaxKey), valid), err}
	}
	return decideErr{fmt.Sprintf("%s: answer refused: free text is 1 to %d characters and the reason at most %d; %s",
		id, decision.MaxAnswerText, decision.MaxReasoning, valid), err}
}

// decideDelivery says what happened to out's delivery: "delivered
// (send_keys)", "queued: <note>" or the note.
func decideDelivery(out model.Outcome) string {
	d := out.Decision
	switch {
	case out.Note == "" && d.Delivery == model.DeliveryDelivered:
		return "delivered (" + sanitize.Line(string(d.DeliveredVia), 20) + ")"
	case d.Delivery == model.DeliveryQueued:
		return "queued: " + sanitize.Line(out.Note, decideNoteLimit)
	}
	return sanitize.Line(out.Note, decideNoteLimit)
}

// decideLine is the one line printed for out, such as "D-117 answered: A:
// Postgres table · delivered (send_keys)" or "D-117 confirmed · nothing to
// deliver".
func decideLine(out model.Outcome) string {
	d := out.Decision
	s := sanitize.Line(d.ID, 20) + " " + sanitize.Line(string(d.Status), 20)
	if d.Status != model.StatusConfirmed {
		s += ": " + sanitize.Line(d.Answer, decideAnswerLimit)
	}
	return s + " · " + decideDelivery(out)
}

// decideResult is the outcome as `curia decide --json` prints it.
type decideResult struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	AnswerKey    string `json:"answer_key"`
	Answer       string `json:"answer"`
	Delivery     string `json:"delivery"`
	DeliveredVia string `json:"delivered_via"`
	Note         string `json:"note"`
}

func decidePrintJSON(w io.Writer, out model.Outcome) error {
	d := out.Decision
	r := decideResult{
		ID: sanitize.Line(d.ID, 20), Status: sanitize.Line(string(d.Status), 20),
		AnswerKey: sanitize.Line(d.AnswerKey, decision.MaxKey), Answer: sanitize.Line(d.Answer, decideAnswerLimit),
		Delivery: sanitize.Line(string(d.Delivery), 20), DeliveredVia: sanitize.Line(string(d.DeliveredVia), 20),
		Note: sanitize.Line(out.Note, decideNoteLimit),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return err
	}
	_, err := io.WriteString(w, escapeUnprintable(buf.String()))
	return err
}

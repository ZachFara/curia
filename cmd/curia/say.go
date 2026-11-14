package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/route"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// sayMaxStdin is the most `curia say -` reads from stdin.
const sayMaxStdin = 64 << 10

// sayNoteLimit bounds a printed delivery note.
const sayNoteLimit = 300

// sayTrippedNotice is added to the output when a label trips auto-send off.
const sayTrippedNotice = "auto-send switched itself off: accuracy fell below 95% (curia routing stats)"

// sayAskMessages is how the user hands a queued instruction over at once.
const sayAskMessages = "tell the agent to call check_messages"

// sayIsTerminal reports whether stdin and stdout are terminals. Tests
// replace it.
var sayIsTerminal = func() bool { return taskIsTerminal(os.Stdin) && taskIsTerminal(os.Stdout) }

// saySleep waits d, or until ctx ends (an interrupt), which it returns.
// Tests replace it.
var saySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "say [<text>... | -]",
		Short: "Send a message to an agent, or make it a rule or a task (the user only)",
		Long: "Routes one message. An address at its start picks the target with no model:\n" +
			"T-42: (a task), @<name> (an alias, task id, repository or branch), and rule: or\n" +
			"task: for the kind. Without an address one bounded `claude -p` call suggests the\n" +
			"target, which you confirm (Enter), change (Tab, k) or cancel (Esc). With no text\n" +
			"on a terminal it opens the say view (prefix + m). Without a terminal only an\n" +
			"addressed message, or one auto-send allows, is sent. - reads the text from\n" +
			"stdin. Refuses to run inside an agent session.",
		DisableFlagParsing: true, // the message is text: "--force" in it is no flag
		RunE:               runSay,
	})
}

func runSay(cmd *cobra.Command, args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		return cmd.Help()
	}
	// Only the user instructs as the user (spec section 7).
	if refuseInsideAgent() != nil {
		return errors.New("curia say cannot run inside an agent session (CLAUDECODE is set): only the user may send a message")
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
	ctx := cmd.Context()
	if _, err = st.SweepHeld(ctx, clock()); err != nil {
		return err
	}
	text, err := sayReadText(cmd.InOrStdin(), args)
	if err != nil {
		return err
	}
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	tm := newTmuxClient()
	if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil { // also refreshes teams
		return err
	}
	if !sayIsTerminal() {
		if text == "" {
			return errors.New("give the message: curia say <text>, or curia say - to read it from stdin")
		}
		return sayHeadless(ctx, cmd.OutOrStdout(), sayNewSource(st, tm, logger, cfg, model.AnsweredCLI), text)
	}
	return sayView(cmd.OutOrStdout(), sayNewSource(st, tm, logger, cfg, decideVia()), text)
}

// sayReadText is the message: the arguments joined by one space, or stdin
// up to sayMaxStdin with "-", normalised; "" when there are no arguments.
func sayReadText(stdin io.Reader, args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	raw := strings.Join(args, " ")
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, sayMaxStdin+1))
		if err != nil {
			return "", fmt.Errorf("read the message from stdin: %w", err)
		}
		if len(b) > sayMaxStdin {
			return "", errors.New("the message on stdin is over 64 KiB")
		}
		raw = string(b)
	}
	text, err := message.Normalize(raw)
	if err != nil {
		return "", sayTextError()
	}
	return text, nil
}

// sayView runs the say view until it quits and prints its outcome line.
func sayView(w io.Writer, src *saySource, text string) error {
	last := ui.NewSay(src, clock, text)
	if err := runProgram(sayCapture{m: last, last: &last}, tea.WithAltScreen()); err != nil {
		return err
	}
	if line, _ := last.Result(); line != "" {
		_, err := fmt.Fprintln(w, sanitize.Line(line, 4*sayNoteLimit))
		return err
	}
	return nil
}

// sayCapture runs the say view and keeps its latest state in last, so its
// Result can be read once the program has quit.
type sayCapture struct {
	m    ui.SayModel
	last *ui.SayModel
}

func (c sayCapture) Init() tea.Cmd { return c.m.Init() }

func (c sayCapture) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := c.m.Update(msg)
	if m, ok := next.(ui.SayModel); ok {
		c.m, *c.last = m, m
	}
	return c, cmd
}

func (c sayCapture) View() string { return c.m.View() }

// sayHeadless is curia say without a terminal: it proposes once, then sends
// an address that names one target, holds and releases an auto proposal,
// and otherwise records the message canceled and says what to do.
func sayHeadless(ctx context.Context, w io.Writer, src *saySource, text string) error {
	plan, err := src.propose(ctx, text)
	if err != nil {
		return err
	}
	p := plan.p
	switch {
	case p.Auto:
		return sayHeadlessAuto(ctx, w, src, plan)
	case p.Routed == model.RoutedAddress && len(p.Candidates) == 1:
		return sayHeadlessSend(ctx, w, src, plan)
	}
	id, cerr := src.cancelPlan(ctx, plan, clock())
	if cerr != nil {
		return cerr
	}
	if p.Routed == model.RoutedAddress {
		return fmt.Errorf("%s not sent: %s; address one of them:\n%s", id, sanitize.Line(p.Note, sayNoteLimit), sayList(p.Candidates))
	}
	why := plan.why
	if !plan.asked {
		why = p.Note
	}
	return fmt.Errorf("%s not sent: without a terminal an unaddressed message is sent only by auto-send (%s); address it, for example %s",
		id, sanitize.Line(why, sayNoteLimit), sayExample(plan))
}

// sayList is one indented line per candidate label.
func sayList(cands []route.Candidate) string {
	lines := make([]string, 0, len(cands))
	for _, c := range cands {
		lines = append(lines, "  "+sanitize.Line(c.Label, 200))
	}
	return strings.Join(lines, "\n")
}

// sayExample is an address the user could have written: @ and the base name
// of the suggested target's repository, or of the first candidate with one.
func sayExample(plan sayPlan) string {
	cands := plan.all
	if plan.asked {
		cands = append([]route.Candidate{plan.sugg.Target}, cands...)
	}
	for _, c := range cands {
		if c.Repo != "" {
			return "@" + sanitize.Line(filepath.Base(c.Repo), 64)
		}
	}
	return "@<repository>"
}

// sayHeadlessSend sends an address that named one target; a refusal
// records the message canceled.
func sayHeadlessSend(ctx context.Context, w io.Writer, src *saySource, plan sayPlan) error {
	c := ui.Choice{Proposal: plan.p, Target: plan.p.Candidates[0], Kind: plan.p.Kind}
	out, notice, err := src.send(ctx, c)
	if err != nil {
		id, cerr := src.cancelPlan(ctx, plan, clock())
		if cerr != nil {
			return errors.Join(err, cerr)
		}
		return fmt.Errorf("%s not sent: %w", id, err)
	}
	return sayPrint(w, out, notice)
}

// sayHeadlessAuto holds an auto proposal for ui.SayHold, then releases it;
// an interrupt during the hold undoes it.
func sayHeadlessAuto(ctx context.Context, w io.Writer, src *saySource, plan sayPlan) error {
	c := ui.Choice{Proposal: plan.p, Target: plan.p.Candidates[0], Kind: plan.p.Kind}
	held, err := src.Hold(ctx, c)
	if err != nil {
		return fmt.Errorf("not sent: %w", err)
	}
	_, _ = fmt.Fprintf(w, "%s → %s in %v (interrupt to undo)\n", sanitize.Line(held.ID, 20),
		sanitize.Line(c.Target.Label, 200), ui.SayHold)
	sctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if serr := saySleep(sctx, ui.SayHold); serr != nil {
		if uerr := src.Undo(context.WithoutCancel(ctx), held.ID); uerr != nil {
			return fmt.Errorf("%s: interrupted, but not undone: %w", held.ID, uerr)
		}
		return fmt.Errorf("%s undone: interrupted before it was sent", held.ID)
	}
	out, err := src.release(ctx, held.ID)
	if err != nil {
		return err
	}
	return sayPrint(w, out, "")
}

// sayPrint prints the outcome line and the notice, if any.
func sayPrint(w io.Writer, out model.MessageOutcome, notice string) error {
	if _, err := fmt.Fprintln(w, sayOutcomeLine(out)); err != nil {
		return err
	}
	if notice != "" {
		_, err := fmt.Fprintln(w, notice)
		return err
	}
	return nil
}

// sayOutcomeLine is "M-12 delivered (send_keys)", "M-12 queued: …", "M-13
// stored as rule D-40 (repo api-server)" or "M-14 filed as T-51 (backlog)".
func sayOutcomeLine(out model.MessageOutcome) string {
	return sanitize.Line(out.Message.ID, 20) + " " + sayWhat(out)
}

// sayWhat says what happened to the message of out.
func sayWhat(out model.MessageOutcome) string {
	m := out.Message
	note := sanitize.Line(out.Note, sayNoteLimit)
	ref := sanitize.Line(m.ResultRef, 20)
	switch {
	case m.Status == model.MsgDelivered && m.DeliveredVia == model.ViaApplied && m.Kind == model.KindTask:
		return "filed as " + ref + " (backlog)"
	case m.Status == model.MsgDelivered && m.DeliveredVia == model.ViaApplied:
		scope := "global"
		if m.TargetRepo != "" {
			scope = "repo " + sanitize.Line(filepath.Base(m.TargetRepo), 64)
		}
		return "stored as rule " + ref + " (" + scope + ")"
	case m.Status == model.MsgDelivered:
		s := "delivered (" + sanitize.Line(string(m.DeliveredVia), 20) + ")"
		if note != "" {
			s += ": " + note
		}
		return s
	case m.Status == model.MsgQueued && note == "":
		return "queued for the Stop hook, or " + sayAskMessages
	}
	return sanitize.Line(string(m.Status), 20) + ": " + note
}

// sayViewOutcome is out for the say view, whose line shows the note in
// place of "delivered": anything but a plain delivery, and any notice, is
// told in sayWhat's words.
func sayViewOutcome(out model.MessageOutcome, notice string) model.MessageOutcome {
	if out.Note != "" || out.Message.Status != model.MsgDelivered || notice != "" {
		out.Note = sayWhat(out)
	}
	if notice != "" {
		out.Note += "; " + notice
	}
	return out
}

// saySource is the say view's ui.SaySource over the store: it stores the
// message with its routing columns, then delivers an instruction
// (deliver.Message) or applies a rule or task. It keeps the plan of its
// last Propose, which says how the proposal was made.
type saySource struct {
	st  *store.Store
	tm  tmux.Client
	log *slog.Logger
	cfg config.Config
	via model.AnsweredVia

	mu      sync.Mutex
	plan    sayPlan
	planned bool
}

var _ ui.SaySource = (*saySource)(nil)

// sayNewSource returns the say source; via is how a rule is recorded.
func sayNewSource(st *store.Store, tm tmux.Client, logger *slog.Logger, cfg config.Config, via model.AnsweredVia) *saySource {
	return &saySource{st: st, tm: tm, log: logger, cfg: cfg, via: via}
}

// propose plans text and keeps the plan.
func (s *saySource) propose(ctx context.Context, text string) (sayPlan, error) {
	plan, err := sayPlanFor(ctx, s.st, s.cfg, text, clock())
	if err == nil {
		s.mu.Lock()
		s.plan, s.planned = plan, true
		s.mu.Unlock()
	}
	return plan, err
}

// lastPlan is the plan of the last Propose, and false before one.
func (s *saySource) lastPlan() (sayPlan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan, s.planned
}

// Propose builds the proposal for text (sayPropose).
func (s *saySource) Propose(ctx context.Context, text string) (ui.Proposal, error) {
	plan, err := s.propose(ctx, text)
	return plan.p, err
}

// Send stores c queued and delivers or applies it. It returns an error only
// when nothing was stored; a problem after that is the outcome's note.
func (s *saySource) Send(ctx context.Context, c ui.Choice) (model.MessageOutcome, error) {
	out, notice, err := s.send(ctx, c)
	if err != nil {
		return model.MessageOutcome{}, err
	}
	return sayViewOutcome(out, notice), nil
}

// send is Send with the outcome as stored and the gate's notice apart.
func (s *saySource) send(ctx context.Context, c ui.Choice) (model.MessageOutcome, string, error) {
	now := clock()
	plan, _ := s.lastPlan()
	m, err := sayRow(plan, c, model.MsgQueued, sayRoutedBy(c), now)
	if err != nil {
		return model.MessageOutcome{}, "", err
	}
	stored, err := s.st.CreateMessage(ctx, m)
	if err != nil {
		return model.MessageOutcome{}, "", err
	}
	out := s.apply(ctx, stored)
	return out, s.gate(ctx, stored, now), nil
}

// sayRoutedBy is how c's target was chosen: an address or a manual pick as
// proposed; a model suggestion confirmed, or overridden when the target or
// the kind was changed.
func sayRoutedBy(c ui.Choice) model.RoutedBy {
	if c.Proposal.Routed != model.RoutedConfirmed {
		return c.Proposal.Routed
	}
	if c.Changed {
		return model.RoutedOverridden
	}
	return model.RoutedConfirmed
}

// sayRow is the message c stores with status and routed, its target
// settled (route.Settle) over the plan's candidates, and every routing
// column filled from the plan. A refusal is an error and nothing is stored.
func sayRow(plan sayPlan, c ui.Choice, status model.MessageStatus, routed model.RoutedBy, now time.Time) (model.Message, error) {
	cands := plan.all
	if cands == nil {
		cands = c.Proposal.Candidates
	}
	t, err := route.Settle(c.Kind, c.Target, cands)
	if err != nil {
		return model.Message{}, saySettleError(err)
	}
	m := sayPlanRow(plan, c.Proposal.Text, c.Kind, status, routed, now)
	m.TargetSession, m.TargetTask, m.TargetRepo = t.Session, t.Task, t.Repo
	if c.Kind == model.KindInstruction {
		m.TargetRepo = c.Target.Repo
		if m.TargetTask == "" {
			m.TargetTask = c.Target.Session.TaskID
		}
	}
	return m, nil
}

// sayPlanRow is a message from the user with the plan's routing columns
// and no target.
func sayPlanRow(plan sayPlan, text string, kind model.MessageKind, status model.MessageStatus, routed model.RoutedBy, now time.Time) model.Message {
	m := model.Message{
		Text: text, Kind: kind, Sender: model.SenderUser, RoutedBy: routed,
		AutoEligible: plan.eligible, Status: status, CreatedAt: now,
	}
	if plan.asked {
		m.SuggestedTarget, m.SuggestedKind, m.RunnerUp = plan.sugg.Target.Ref, plan.sugg.Kind, plan.sugg.RunnerUp.Ref
		m.Confidence, m.HasConfidence = plan.sugg.Confidence, true
	}
	return m
}

// apply delivers stored instruction m (deliver.Message) or applies rule or
// task m. It never fails: what went wrong is the note.
func (s *saySource) apply(ctx context.Context, m model.Message) model.MessageOutcome {
	now := clock()
	switch m.Kind {
	case model.KindRule:
		return s.applyRule(ctx, m, now)
	case model.KindTask:
		return s.applyTask(ctx, m, now)
	}
	out, err := deliver.Message(ctx, s.st, s.tm, s.log, m.ID, now)
	if err != nil {
		s.log.Warn("say: delivery failed", "id", m.ID, "err", err.Error())
		return model.MessageOutcome{Message: m, Note: "not delivered (" + sanitize.Line(err.Error(), 200) + "); queued for the Stop hook, or " + sayAskMessages}
	}
	return out
}

// applyRule stores rule m as decided precedent and marks m applied with
// its D- id.
func (s *saySource) applyRule(ctx context.Context, m model.Message, now time.Time) model.MessageOutcome {
	scope := model.ScopeRepo
	if m.TargetRepo == "" {
		scope = model.ScopeGlobal
	}
	d, _, err := s.st.CreateRule(ctx, store.Rule{Text: m.Text, Scope: scope, Repo: m.TargetRepo, Via: s.via}, now)
	if err != nil {
		return s.unapplied(ctx, m, "the rule was not stored: "+err.Error())
	}
	return s.applied(ctx, m, d.ID, "rule", now)
}

// applyTask files task m in the backlog of its repository, on the branch
// checked out there, and marks m applied with its T- id.
func (s *saySource) applyTask(ctx context.Context, m model.Message, now time.Time) model.MessageOutcome {
	first, _, _ := strings.Cut(m.Text, "\n")
	_, branch, _ := gitInfo(ctx, m.TargetRepo, s.log)
	t, ps := taskCheck(model.Task{
		Title: sanitize.Line(first, task.MaxTitle), Body: m.Text, Repo: m.TargetRepo, BaseBranch: branch,
		Status: model.TaskBacklog, Source: model.SourceLocal, CreatedAt: now,
	}, nil)
	if len(ps) > 0 {
		return s.unapplied(ctx, m, "the task was not filed: "+ps[0].Field+" "+ps[0].Problem)
	}
	stored, err := s.st.CreateTask(ctx, t)
	if err != nil {
		return s.unapplied(ctx, m, "the task was not filed: "+err.Error())
	}
	return s.applied(ctx, m, stored.ID, "task", now)
}

// applied marks m applied as ref; if that fails, m stays queued and the
// note says so.
func (s *saySource) applied(ctx context.Context, m model.Message, ref, what string, now time.Time) model.MessageOutcome {
	done, err := s.st.ApplyMessage(ctx, m.ID, ref, now)
	if err != nil {
		s.log.Warn("say: apply failed", "id", m.ID, "ref", ref, "err", err.Error())
		return model.MessageOutcome{Message: m, Note: fmt.Sprintf("%s %s stored, but %s is still queued: %s", what, ref, m.ID, err.Error())}
	}
	return model.MessageOutcome{Message: done}
}

// unapplied cancels m, whose rule or task could not be stored, and says
// why.
func (s *saySource) unapplied(ctx context.Context, m model.Message, why string) model.MessageOutcome {
	s.log.Warn("say: not applied", "id", m.ID, "err", why)
	c, err := s.st.CancelMessage(ctx, m.ID)
	if err != nil {
		return model.MessageOutcome{Message: m, Note: why + "; " + m.ID + " could not be canceled: " + err.Error()}
	}
	return model.MessageOutcome{Message: c, Note: why}
}

// gate reads the accuracy gate after label m is stored: the first time it
// is earned it records that, and when the label trips auto-send off it
// records that and returns the notice. A failure is logged, never returned.
func (s *saySource) gate(ctx context.Context, m model.Message, now time.Time) string {
	if m.RoutedBy != model.RoutedConfirmed && m.RoutedBy != model.RoutedOverridden {
		return ""
	}
	earnedAt, trippedAt, err := s.st.RoutingState(ctx)
	var labels []model.RouteLabel
	if err == nil {
		labels, err = s.st.RouteLabels(ctx, s.cfg.Routing.AutoThreshold, route.Window)
	}
	g := route.Gate(labels)
	if err == nil && g.Earned && earnedAt.IsZero() {
		err = s.st.SetRoutingEarned(ctx, now)
	}
	tripped := err == nil && s.cfg.Routing.AutoSend && trippedAt.IsZero() && route.Tripped(!earnedAt.IsZero(), g)
	if tripped {
		err = s.st.SetRoutingTripped(ctx, now)
	}
	if err != nil {
		s.log.Warn("say: routing gate not updated", "err", err.Error())
		return ""
	}
	if tripped {
		return sayTrippedNotice
	}
	return ""
}

// Hold stores c held until ui.SayHold from now, routed auto. An instruction
// whose target session has ended or gone stale since the proposal is not
// held: it is recorded canceled and refused.
func (s *saySource) Hold(ctx context.Context, c ui.Choice) (model.Message, error) {
	now := clock()
	plan, _ := s.lastPlan()
	if !sayTargetAlive(ctx, s.st, c.Kind, c.Target, plan.all) {
		if _, err := s.cancelPlan(ctx, plan, now); err != nil {
			return model.Message{}, err
		}
		return model.Message{}, errors.New("the target session ended since the route was proposed; nothing sent")
	}
	m, err := sayRow(plan, c, model.MsgHeld, model.RoutedAuto, now)
	if err != nil {
		return model.Message{}, err
	}
	m.HoldUntil = now.Add(ui.SayHold)
	return s.st.CreateMessage(ctx, m)
}

// Release queues held message id and delivers or applies it.
func (s *saySource) Release(ctx context.Context, id string) (model.MessageOutcome, error) {
	out, err := s.release(ctx, id)
	if err != nil {
		return model.MessageOutcome{}, err
	}
	return sayViewOutcome(out, ""), nil
}

// release is Release with the outcome as stored.
func (s *saySource) release(ctx context.Context, id string) (model.MessageOutcome, error) {
	m, err := s.st.ReleaseMessage(ctx, id)
	if err != nil {
		return model.MessageOutcome{}, err
	}
	return s.apply(ctx, m), nil
}

// Undo undoes held message id.
func (s *saySource) Undo(ctx context.Context, id string) error {
	_, err := s.st.UndoMessage(ctx, id)
	return err
}

// Cancel records text as a canceled message, with the routing columns of
// the last proposal when there is one. Text that is no message records
// nothing.
func (s *saySource) Cancel(ctx context.Context, text string) error {
	plan, ok := s.lastPlan()
	if !ok {
		plan, ok = sayTextPlan(text)
	}
	if !ok {
		return nil
	}
	_, err := s.cancelPlan(ctx, plan, clock())
	return err
}

// sayTextPlan is the plan of a message canceled before it was proposed: its
// text after any address, a manual pick; false when text is no message.
func sayTextPlan(text string) (sayPlan, bool) {
	norm, err := message.Normalize(text)
	if err != nil {
		return sayPlan{}, false
	}
	addr, rest := message.ParseAddress(norm)
	if rest == "" {
		return sayPlan{}, false
	}
	p := ui.Proposal{Text: rest, Kind: model.KindInstruction, Routed: model.RoutedManual}
	if addr.HasKind {
		p.Kind = addr.Kind
	}
	return sayPlan{p: p}, true
}

// cancelPlan records the plan's message canceled and returns its id. A
// suggestion the user never confirmed is no label, so it is stored routed
// manual, with the suggestion in its columns.
func (s *saySource) cancelPlan(ctx context.Context, plan sayPlan, now time.Time) (string, error) {
	routed := plan.p.Routed
	if routed != model.RoutedAddress {
		routed = model.RoutedManual
	}
	m, err := s.st.CreateMessage(ctx, sayPlanRow(plan, plan.p.Text, plan.p.Kind, model.MsgCanceled, routed, now))
	return m.ID, err
}

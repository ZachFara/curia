package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// awayFlags are the flags of `curia away`; tests reset them.
var awayFlags struct {
	forDur time.Duration
	repo   string
}

// awaySleep waits between readiness checks; tests replace it.
var awaySleep = func(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

const (
	awayPoll   = 2 * time.Second
	awayMaxFor = 7 * 24 * time.Hour
	// awayLeaving is the message every live agent gets first (spec F20); %s is the duration.
	awayLeaving = "The owner is leaving for %s. Raise every decision you can foresee now with request_decision, then end your turn."
	// awaySessionsShown and awayNotReadyShown bound the lists of a summary.
	awaySessionsShown = 20
)

func init() {
	away := &cobra.Command{
		Use:         "away [duration]",
		Short:       "Leave: agents keep working under away mode until you are back (the user only)",
		Annotations: userOnlyMark(),
		Long: "curia away 6h asks every live agent to raise the decisions it can foresee and end its turn, waits up to\n" +
			"away.prepare_wait for that, then puts every live session in away mode until the time is up (--repo limits\n" +
			"it to one repository) and prints what agents may and may not do meanwhile. Sessions that start during\n" +
			"the period are away too. curia back, or the deadline, ends it. Only the user may run it.",
		Args: cobra.MaximumNArgs(1),
		RunE: runAway,
	}
	away.Flags().DurationVar(&awayFlags.forDur, "for", 0, "how long you are away (or give it as the argument)")
	away.Flags().StringVar(&awayFlags.repo, "repo", "", "only the sessions of a repository (default: everyone)")
	away.Flags().Lookup("repo").NoOptDefVal = "."
	rootCmd.AddCommand(away)
	rootCmd.AddCommand(&cobra.Command{
		Use:         "back",
		Short:       "End the away period, restore the modes and print what happened (the user only)",
		Annotations: userOnlyMark(),
		Args:        cobra.NoArgs,
		RunE:        runBack,
	})
}

func awayDuration(args []string) (time.Duration, error) {
	d := awayFlags.forDur
	if len(args) == 1 {
		if d != 0 {
			return 0, errors.New("give the duration as the argument or as --for, not both")
		}
		var err error
		if d, err = time.ParseDuration(args[0]); err != nil {
			return 0, fmt.Errorf("the duration %q is not like 6h or 90m", sanitize.Line(args[0], 40))
		}
	}
	if d <= 0 || d > awayMaxFor {
		return 0, fmt.Errorf("say how long you are away, between 1ns and %s (curia away 6h)", awayMaxFor)
	}
	return d, nil
}

func runAway(cmd *cobra.Command, args []string) error {
	if err := userOnly("away", "start an away period"); err != nil {
		return err
	}
	dur, err := awayDuration(args)
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
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	scope, err := awayCheck(ctx, st)
	if err != nil {
		return err
	}
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	tm := newTmuxClient()
	if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
		return err
	}
	sent, notReady, err := awayAsk(ctx, st, tm, logger, scope, dur)
	if err != nil {
		return err
	}
	if len(sent) > 0 {
		_, _ = fmt.Fprintf(out, "Asked %d agent(s) to raise what they foresee and end their turn; waiting up to %s.\n", len(sent), cfg.Away.PrepareWait)
	}
	waiting, err := awayWait(ctx, st, sent, cfg.Away.PrepareWait)
	if err != nil {
		return err
	}
	notReady = append(notReady, waiting...)
	now := clock()
	run, noRoot, err := awayStart(ctx, st, scope, now, now.Add(dur), notReady)
	if err != nil {
		return err
	}
	awayPrint(out, cfg, run, len(sent), notReady, noRoot)
	return nil
}

// awayStart starts the period and names who got no root.
func awayStart(ctx context.Context, st *store.Store, scope store.ModeScope, now, ends time.Time, notReady []string) (store.AwayRun, []string, error) {
	run, err := st.StartAway(ctx, scope, now, ends, notReady)
	if err != nil {
		return run, nil, err
	}
	noRoot, err := awayNoRoot(ctx, st, scope)
	return run, noRoot, err
}

// awayNoRoot names the live sessions in scope with no worktree and no repository: away froze no root for them, so they may edit nothing.
func awayNoRoot(ctx context.Context, st *store.Store, scope store.ModeScope) (names []string, err error) {
	live, err := modeLive(ctx, st, scope)
	for _, s := range live {
		if s.State != model.StateEnded && s.Repo == "" && s.Worktree == "" {
			names = append(names, spawnShortID(s.ID))
		}
	}
	return names, err
}

// awayCheck refuses a second period and resolves --repo (default: everyone).
func awayCheck(ctx context.Context, st *store.Store) (store.ModeScope, error) {
	open, ok, err := st.OpenAway(ctx)
	if err != nil {
		return store.ModeScope{}, err
	}
	if ok {
		return store.ModeScope{}, fmt.Errorf("already away until %s: curia back ends it", open.EndsAt.Local().Format("15:04"))
	}
	if awayFlags.repo == "" {
		return store.ModeScope{All: true}, nil
	}
	modeFlags.repo, modeFlags.all = awayFlags.repo, false
	scope, _, err := modeScope(ctx)
	return scope, err
}

// awayAsk sends the leaving message to each live agent it can reach (the F18
// delivery path: typed into an idle pane, or handed over at its next stop) and
// returns the message id per session and who could not be messaged.
func awayAsk(ctx context.Context, st *store.Store, tm tmux.Client, logger *slog.Logger, scope store.ModeScope, dur time.Duration) (sent map[string]string, notReady []string, err error) {
	live, err := modeLive(ctx, st, scope)
	if err != nil {
		return nil, nil, err
	}
	sent = map[string]string{}
	for _, s := range live {
		if s.State == model.StateEnded || s.State == model.StateStale {
			continue
		}
		if s.Pane == "" || s.Agent == model.AgentCodex {
			notReady = append(notReady, spawnShortID(s.ID)+" (cannot be messaged)")
			continue
		}
		m, err := st.CreateMessage(ctx, model.Message{
			Text: fmt.Sprintf(awayLeaving, dur), Kind: model.KindInstruction, Sender: model.SenderUser,
			TargetSession: s.ID, RoutedBy: model.RoutedSend, Status: model.MsgQueued, CreatedAt: clock(),
		})
		if err != nil {
			notReady = append(notReady, spawnShortID(s.ID)+" (not messaged: "+sanitize.Line(err.Error(), 80)+")")
			continue
		}
		if _, err = deliver.Message(ctx, st, tm, logger, m.ID, clock()); err != nil {
			logger.Warn("away: deliver", "session", s.ID, "err", err.Error())
		}
		sent[s.ID] = m.ID
	}
	return sent, notReady, nil
}

// awayWait polls awayNotReady until nobody is left or wait has passed, and
// returns who was not ready.
func awayWait(ctx context.Context, st *store.Store, sent map[string]string, wait time.Duration) ([]string, error) {
	deadline := clock().Add(wait)
	for {
		waiting, err := awayNotReady(ctx, st, sent)
		if err != nil || len(waiting) == 0 || !clock().Before(deadline) || ctx.Err() != nil {
			return waiting, err
		}
		awaySleep(ctx, awayPoll)
	}
}

// awayNotReady lists, for each session sent the leaving message, those that
// have not ended a turn since getting it (idle or needs_decision after the
// message was delivered, a typed one confirmed). A session sitting at a
// permission prompt is waiting, so it is not ready. A system turn (a task
// notification, an agent message) never confirms the message, so it cannot
// make a session ready. A session that has ended counts as ready.
func awayNotReady(ctx context.Context, st *store.Store, sent map[string]string) ([]string, error) {
	var out []string
	for sid, mid := range sent {
		s, found, err := st.GetSession(ctx, sid)
		if err != nil {
			return nil, err
		}
		if !found || s.State == model.StateEnded || s.State == model.StateStale {
			continue
		}
		m, found, err := st.GetMessage(ctx, mid)
		if err != nil {
			return nil, err
		}
		got := m.DeliveredAt
		if m.DeliveredVia == model.ViaSendKeys {
			got = m.ConfirmedAt
		}
		switch {
		case !found || got.IsZero() || !s.UpdatedAt.After(got):
			out = append(out, spawnShortID(sid)+" ("+awayWhy(s, got)+")")
		case s.State != model.StateIdle && s.State != model.StateNeedsDecision:
			out = append(out, spawnShortID(sid)+" ("+awayWhy(s, got)+")")
		}
	}
	slices.Sort(out)
	return out, nil
}

func awayWhy(s model.Session, got time.Time) string {
	switch {
	case s.State == model.StateWaiting:
		return "sitting at a permission prompt"
	case got.IsZero():
		return "has not received the message"
	default:
		return "has not ended its turn"
	}
}

// awayPrint says what agents may and may not do without asking, and when it ends.
func awayPrint(out io.Writer, cfg config.Config, run store.AwayRun, asked int, notReady, noRoot []string) {
	w := func(f string, a ...any) { _, _ = fmt.Fprintf(out, f+"\n", a...) }
	label := "everyone"
	if run.Scope != "" {
		label = sanitize.Line(run.Scope, 120)
	}
	w("Away until %s for %s. Agents of %s are in away mode.", run.EndsAt.Local().Format("2006-01-02 15:04"), label, label)
	if len(notReady) > 0 {
		w("Not ready when you left: %s.", sanitize.Line(strings.Join(notReady, ", "), 400))
	} else if asked > 0 {
		w("Every agent had ended its turn.")
	}
	if len(noRoot) > 0 {
		w("No folder to allow edits in: %s (no worktree or repository known), so away allows them no edits.", sanitize.Line(strings.Join(noRoot, ", "), 400))
	}
	w("Without asking, an agent MAY: edit and write files inside its own worktree, run Bash commands that start with one of [%s] and have plain arguments (mode.away_allow), raise decision tickets.",
		sanitize.Line(strings.Join(cfg.Mode.AwayAllow, ", "), 200))
	w("It may NOT do anything else: every other tool call is refused with a reason, recorded, and listed when you are back. A blocking ticket it raises is stored as an assumption that waits for you; Curia answers nothing for you.")
	w("Claude Code's own deny and ask rules still hold. If Curia's hook fails or times out, Claude Code's own prompt appears and that agent waits at it.")
	w("It ends by itself at %s (the monitor must be running; the hook stops applying away at that time regardless), or run curia back.", run.EndsAt.Local().Format("15:04"))
}

func runBack(cmd *cobra.Command, _ []string) error {
	if err := userOnly("back", "end an away period"); err != nil {
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
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	now := clock()
	run, ok, err := awayEnd(ctx, st, now)
	if err != nil {
		return err
	}
	if !ok {
		_, err = fmt.Fprintln(out, "You are not away, and there is no summary you have not seen.")
		return err
	}
	if run.Summary == "" { // ended elsewhere before its summary was stored
		if err = awayFinish(ctx, st, &run); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprint(out, run.Summary)
	return st.MarkAwayShown(ctx, run.ID, now)
}

// awayEnd ends the open period, or finds the one that ended since the user
// last looked (the deadline, or the monitor a moment ago); ok is false when
// there is neither.
func awayEnd(ctx context.Context, st *store.Store, now time.Time) (run store.AwayRun, ok bool, err error) {
	if run, ok, err = st.OpenAway(ctx); err != nil {
		return run, false, err
	}
	if ok {
		var won bool
		if run, won, err = st.EndAway(ctx, run.ID, now); err != nil || won {
			if err == nil {
				err = awayFinish(ctx, st, &run)
			}
			return run, err == nil, err
		}
	}
	return st.UnshownAway(ctx)
}

// awayFinish builds the summary of an ended run and stores it.
func awayFinish(ctx context.Context, st *store.Store, run *store.AwayRun) error {
	text, err := awaySummary(ctx, st, *run)
	if err != nil {
		return err
	}
	run.Summary = text
	return st.SetAwaySummary(ctx, run.ID, text)
}

// awaySummary is the text `curia back` prints, from rows that exist already:
// the timeline, the denied tool calls (grouped, cleaned, bounded: never raw
// tool input), the tickets, reviews and messages waiting, and the cost.
func awaySummary(ctx context.Context, st *store.Store, run store.AwayRun) (string, error) {
	var b strings.Builder
	w := func(f string, a ...any) { _, _ = fmt.Fprintf(&b, f+"\n", a...) }
	to := run.EndedAt
	how := "ended with curia back"
	if !to.Before(run.EndsAt) {
		to, how = run.EndsAt, "ended by itself at its deadline"
	}
	w("Away %s to %s (%s), %s.", run.StartedAt.Local().Format("2006-01-02 15:04"), to.Local().Format("15:04"), to.Sub(run.StartedAt).Round(time.Minute), how)
	did, err := awayDid(ctx, st, run.StartedAt, to)
	if err != nil {
		return "", err
	}
	b.WriteString(did)
	groups, total, notRecorded, err := st.Denials(ctx, run.StartedAt, to)
	if err != nil {
		return "", err
	}
	w("Refused (%d call(s); one line per session, tool and command line):", total)
	if total == 0 {
		w("  nothing")
	}
	for _, d := range groups {
		w("  %dx %s %s: %s", d.Count, spawnShortID(d.SessionID), sanitize.Line(d.Tool, 40), sanitize.Line(d.Summary, 100))
	}
	if notRecorded > 0 {
		w("  %d further refusal(s) not recorded (a session passed %d distinct ones)", notRecorded, store.PermissionDistinctCap)
	}
	wt, err := st.Waiting(ctx, run.StartedAt, to)
	if err != nil {
		return "", err
	}
	w("Waiting for you:")
	w("  %d blocking ticket(s) raised while away; %d assumption(s) to confirm (%d of them were blocking and became assumptions): curia decisions ls", wt.Blocking, wt.Assumed, wt.Coerced)
	w("  %d review(s) waiting; %d message(s) queued", wt.Reviews, wt.Messages)
	cost := fmt.Sprintf("cost +$%.2f", wt.CostUSD)
	if wt.HasUsage {
		cost += fmt.Sprintf("; usage windows at %.0f%% (5 hours), %.0f%% (7 days)", wt.FivePct, wt.SevenPct)
	}
	w("Spend: %s", cost)
	if len(run.NotReady) > 0 {
		w("Not ready when you left: %s", sanitize.Line(strings.Join(run.NotReady, ", "), 400))
	}
	w("Curia's own mode rows are restored (a saved bypass is dropped; type it again). Claude Code's own permission mode is NOT restored: a session resumed during away or bypass is in Claude Code's default mode now.")
	return b.String(), nil
}

// awayDid is "what each agent did" from the timeline of the window.
func awayDid(ctx context.Context, st *store.Store, from, to time.Time) (string, error) {
	events, err := st.TimelineBetween(ctx, from, to.Add(time.Nanosecond))
	if err != nil {
		return "", err
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return "", err
	}
	repo := map[string]string{}
	for _, s := range sessions {
		repo[s.ID] = filepath.Base(s.Repo)
	}
	counts := map[string]map[string]int{}
	for _, kind := range []string{"turn_end", "ticket_raised", "review_opened", "message_delivered"} {
		for _, e := range events[kind] {
			if counts[e.SessionID] == nil {
				counts[e.SessionID] = map[string]int{}
			}
			counts[e.SessionID][kind]++
		}
	}
	ids := make([]string, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var b strings.Builder
	b.WriteString("What each agent did:\n")
	if len(ids) == 0 {
		b.WriteString("  nothing recorded\n")
	}
	for i, id := range ids {
		if i == awaySessionsShown {
			fmt.Fprintf(&b, "  … and %d more\n", len(ids)-i)
			break
		}
		c := counts[id]
		fmt.Fprintf(&b, "  %s %s: %d turn(s), %d ticket(s) raised, %d review(s) opened, %d message(s) delivered\n",
			spawnShortID(id), sanitize.Line(repo[id], 40), c["turn_end"], c["ticket_raised"], c["review_opened"], c["message_delivered"])
	}
	return b.String(), nil
}

// expireAway is the monitor's job: when the open away period's deadline has
// passed it ends it as `curia back` does (the claim is one UPDATE, so two
// monitors end it once), stores the summary for the next `curia back` and
// shows one notice, under the same shared rate limit as a blocking ticket's.
func (e *monitorEnv) expireAway(ctx context.Context) error {
	run, ok, err := e.st.OpenAway(ctx)
	if err != nil || !ok || e.now().Before(run.EndsAt) {
		return err
	}
	run, won, err := e.st.EndAway(ctx, run.ID, e.now())
	if err != nil || !won {
		return err
	}
	if err = awayFinish(ctx, e.st, &run); err != nil {
		return err
	}
	slot, err := e.st.ClaimNotifySlot(ctx, e.now(), mcpNotifyEvery)
	if err != nil || !slot {
		return err
	}
	return notify.AwayEnded(ctx, mcpRun, mcpGOOS, e.cfg, "Away ended: run curia back for the summary")
}

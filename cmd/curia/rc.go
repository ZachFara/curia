package main

// F21 Remote Control (`curia rc`). Remote Control itself has NEVER been
// observed: the probe that would run /rc is reserved for the owner, and no
// test or code here starts Claude Code. Everything below is built on fakes
// and on what the probes did observe (a built-in slash command fires no
// UserPromptSubmit, so only the pane can confirm it; `claude
// --remote-control [name]` exists in 2.1.329's --help).
//
// UNVERIFIED UNTIL THE OWNER'S PROBE (the one place; each item names the
// constant that holds it):
//  1. rcLine: that "/rc <name>" is accepted as typed (rcCommand, rcName).
//  2. rcPatterns: the footer text of an active session ("/rc active") and of
//     a disconnected one ("Remote Control not started here"): placeholders.
//  3. That the prompt returns empty within rcWait after /rc (rcWait, rcStep),
//     and what the first /rc shows (a one-time dialog or panel would read as
//     "not confirmed" and block more typing until `curia rc --check`).
//  4. That `--remote-control <name>` takes the name as its own argument and
//     leaves the brief positional (launchWithRC, key rc.launch).
//  5. That a second /rc into a session Curia could not see as on is harmless.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

const (
	rcCommand = "/rc" // the documented alias of /remote-control
	rcWait    = 5 * time.Second
	rcStep    = 500 * time.Millisecond
	rcNameMax = 40 // runes of a task title in the name
	rcFooter  = 3  // non-blank bottom lines the indicator table reads
)

// rcNow and rcSleep are the clock of the bounded wait; tests replace them.
var (
	rcNow   = time.Now
	rcSleep = func(ctx context.Context, d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
)

// rcPatterns is the one table of what the pane shows about Remote Control,
// read from the last rcFooter non-blank lines only (an agent's own output
// above must not set the state).
// shortcut: placeholders from the documentation's wording, never seen on a
// real screen; replace them with the owner's probe captures. A session
// matching neither stays "unknown" (the default), and "on" needs a match.
var rcPatterns = []struct {
	state, how string
	re         *regexp.Regexp
}{
	{"on", "indicator", regexp.MustCompile(`/rc active`)},
	{"off", "disconnected", regexp.MustCompile(`Remote Control (not started here|disconnected|failed)`)},
}

// rcReadIndicator is the state the footer of screen shows ("on", "off") and
// how it knows, or "" when it shows neither.
func rcReadIndicator(screen string) (state, how string) {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	lines = lines[max(0, len(lines)-rcFooter):]
	for _, p := range rcPatterns {
		for _, l := range lines {
			if p.re.MatchString(l) {
				return p.state, p.how
			}
		}
	}
	return "", ""
}

var rcRune = regexp.MustCompile(`[^\p{L}\p{N} ._/:·-]`)

// rcName is the name typed after /rc: "<task id> <title> · <repo>" or
// "<branch> · <repo>", cleaned (escapes, breaks, anything but letters,
// digits, space and . _ / : · -), the title cut to rcNameMax runes and no
// leading dash or slash. It is "" when nothing is left.
func rcName(s model.Session, title string) string {
	clean := func(v string, limit int) string {
		v = strings.Join(strings.Fields(rcRune.ReplaceAllString(sanitize.Line(v, 200), "")), " ")
		if r := []rune(v); len(r) > limit {
			v = strings.TrimSpace(string(r[:limit]))
		}
		return strings.TrimLeft(v, "-/ ")
	}
	head := clean(s.Branch, 80)
	if s.TaskID != "" {
		head = strings.TrimSpace(clean(s.TaskID, 20) + " " + clean(title, rcNameMax))
	}
	repo := clean(s.Repo, 40)
	switch {
	case head != "" && repo != "":
		return head + " · " + repo
	case head != "":
		return head
	}
	return repo
}

// rcLine is the one line typed: fixed command, cleaned name.
func rcLine(s model.Session, title string) string {
	if n := rcName(s, title); n != "" {
		return rcCommand + " " + n
	}
	return rcCommand
}

const rcWarning = "Remote Control exposes the session outside this machine through Anthropic's service:\n" +
	"anyone signed in to your Claude account can read it and drive it from the Claude app."

var rcFlags struct{ all, check bool }

func init() {
	cmd := &cobra.Command{
		Use:   "rc [<session|T-id|%pane>] [--all] [--check]",
		Short: "Turn on Remote Control in agent sessions (the user only)",
		Long: "Types /rc <name> into the idle, empty prompt of a session (or of every live session with\n" +
			"--all), after showing a warning and asking you to confirm. The pane is then read for up to\n" +
			"5 seconds and each session is reported confirmed, not confirmed or unknown. --check types\n" +
			"nothing: it reads each pane and reports on (only with the indicator), off (only with the\n" +
			"disconnect notice) or unknown. A session known to be on is never typed into.",
		Args:        cobra.MaximumNArgs(1),
		Annotations: userOnlyMark(),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer func() { rcFlags.all, rcFlags.check = false, false }() // a second Execute in one process starts clean
			return userOnlyRunE("rc", "turn on Remote Control", runRC)(cmd, args)
		},
	}
	cmd.Flags().BoolVar(&rcFlags.all, "all", false, "every live session of this tmux server")
	cmd.Flags().BoolVar(&rcFlags.check, "check", false, "type nothing; report what each pane shows")
	rootCmd.AddCommand(cmd)
}

func runRC(cmd *cobra.Command, args []string) error {
	all, check := rcFlags.all, rcFlags.check
	switch {
	case len(args) == 0 && !all && !check:
		return errors.New("name a session, a task or a pane, or pass --all (or --check)")
	case len(args) == 1 && all:
		return errors.New("a target and --all together: pick one")
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
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	ctx, tm, w := cmd.Context(), newTmuxClient(), cmd.OutOrStdout()
	if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
		return err
	}
	targets, err := rcTargets(ctx, st, args, all || check && len(args) == 0)
	if err != nil {
		return err
	}
	if check {
		return rcCheck(ctx, st, tm, w, targets)
	}
	return rcRun(ctx, cmd, st, tm, logger, targets, all)
}

// rcRun asks for confirmation and types into each target.
func rcRun(ctx context.Context, cmd *cobra.Command, st *store.Store, tm tmux.Client, log *slog.Logger, targets []model.Session, all bool) error {
	w := cmd.OutOrStdout()
	if len(targets) == 0 {
		_, err := fmt.Fprintln(w, "no live session")
		return err
	}
	if ok, err := rcConfirm(cmd, targets, all); err != nil || !ok {
		return err
	}
	for _, s := range targets {
		_, _ = fmt.Fprintln(w, rcType(ctx, st, tm, log, s))
	}
	return nil
}

// rcTargets is the one session args names, or every live Claude session on
// the caller's tmux server (Ruling 133), by id.
func rcTargets(ctx context.Context, st *store.Store, args []string, all bool) ([]model.Session, error) {
	if !all {
		id, err := showResolve(ctx, st, args[0])
		if err != nil {
			return nil, err
		}
		s, _, err := st.GetSession(ctx, id)
		return []model.Session{s}, err
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	socket := seenSocket(os.Getenv("TMUX"))
	var out []model.Session
	for _, s := range sessions {
		if s.State != model.StateEnded && s.State != model.StateStale && s.Pane != "" &&
			(s.Agent == "" || s.Agent == model.AgentClaude) && !fleet.OtherSocket(socket, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// rcConfirm shows the warning and asks: --all lists the sessions and needs a
// y (as a broadcast does), one session needs Enter. No terminal: refused.
func rcConfirm(cmd *cobra.Command, targets []model.Session, all bool) (bool, error) {
	w := cmd.OutOrStdout()
	if !sayIsTerminal() {
		return false, errors.New("remote control always needs your confirmation, and there is no terminal to ask on")
	}
	_, _ = fmt.Fprintln(w, rcWarning)
	if all {
		for _, s := range targets[:min(len(targets), sendBroadcastShown)] {
			_, _ = fmt.Fprintf(w, "  %s  %s\n", sanitize.Line(s.ID, 40), sanitize.Line(s.Repo+" "+s.Branch, 60))
		}
		if more := len(targets) - sendBroadcastShown; more > 0 {
			_, _ = fmt.Fprintf(w, "  … and %d more\n", more)
		}
		_, _ = fmt.Fprintf(w, "type /rc into %d sessions? [y/N] ", len(targets))
	} else {
		_, _ = fmt.Fprintf(w, "type /rc into %s? Enter to continue, anything else cancels: ", sanitize.Line(targets[0].ID, 40))
	}
	line, _ := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 64)).ReadString('\n')
	line = strings.TrimSpace(line)
	if ok := line == "y" && all || line == "" && !all; !ok {
		_, err := fmt.Fprintln(w, "nothing typed")
		return false, err
	}
	return true, nil
}

// rcType types the line into s and reads the pane after, for at most rcWait,
// and returns the session's report line. Confirmed: the prompt is empty
// again with no dialog (the pane is the only confirmation: no hook fires).
// Not confirmed: the line or a panel is still there. Unknown: the screen is
// not one the recogniser knows, or cannot be read. A line that is not
// confirmed blocks further typing into the session until `curia rc --check`.
func rcType(ctx context.Context, st *store.Store, tm tmux.Client, log *slog.Logger, s model.Session) string {
	id := sanitize.Line(s.ID, 40)
	title := ""
	if s.TaskID != "" {
		if t, found, err := st.GetTask(ctx, s.TaskID); err == nil && found {
			title = t.Title
		}
	}
	typed, why := deliver.TypeRC(ctx, st, tm, log, s.ID, rcLine(s, title), rcNow())
	if !typed {
		return id + ": nothing typed: " + why
	}
	end, outcome, last := rcNow().Add(rcWait), "unknown", deliver.PaneRead{}
	for {
		pr, err := deliver.ReadPane(ctx, tm, s.Pane)
		if err != nil {
			outcome = "unknown"
		} else if last = pr; pr.Prompt == deliver.PaneEmpty {
			outcome = "confirmed"
			break
		} else if pr.Prompt == deliver.PaneUnknown {
			outcome = "unknown"
		} else {
			outcome = "not confirmed"
		}
		if !rcNow().Before(end) || ctx.Err() != nil {
			break
		}
		rcSleep(ctx, rcStep)
	}
	state, how, note := "unknown", "unconfirmed", ""
	if outcome == "confirmed" {
		state, how = "unknown", "typed"
		if ind, _ := rcReadIndicator(last.Screen); ind == "on" {
			state, how, note = "on", "indicator", ", Remote Control on (indicator seen)"
		} else {
			note = ", Remote Control state unknown (no indicator seen)"
		}
	} else {
		note = ", more typing is blocked until you look at the pane and run curia rc --check"
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := st.SetRC(sctx, s.ID, state, how, rcNow()); err != nil {
		log.Warn("rc: could not record the state", "session", s.ID, "err", err.Error())
	}
	return id + ": /rc typed, " + outcome + note
}

// rcCheck reads each target's pane and reports on, off or unknown, typing
// nothing.
func rcCheck(ctx context.Context, st *store.Store, tm tmux.Client, w io.Writer, targets []model.Session) error {
	for _, s := range targets {
		id := sanitize.Line(s.ID, 40)
		pr, err := deliver.ReadPane(ctx, tm, s.Pane)
		if err != nil {
			_, _ = fmt.Fprintln(w, id+": unknown (pane not readable)")
			continue
		}
		state, how := rcCheckState(s, pr)
		if state != "" {
			if err = st.SetRC(ctx, s.ID, state, how, rcNow()); err != nil {
				return err
			}
		}
		_, _ = fmt.Fprintf(w, "%s: %s\n", id, rcShown(s, state))
	}
	return nil
}

// rcCheckState is what to record for s from its pane: "on" and "off" need
// their pattern; otherwise a session recorded on is downgraded to unknown, a
// /rc block is lifted once the prompt is empty again, and "" records nothing.
func rcCheckState(s model.Session, pr deliver.PaneRead) (state, how string) {
	if state, how = rcReadIndicator(pr.Screen); state != "" {
		return state, how
	}
	if blocked := s.RCHow == "typing" || s.RCHow == "unconfirmed"; s.RC == "on" || blocked && pr.Prompt == deliver.PaneEmpty {
		return "unknown", "check"
	}
	return "", ""
}

// rcShown is the word reported: what was just recorded, else what is stored,
// else unknown. Never "on" without the indicator, which recording it needs.
func rcShown(s model.Session, recorded string) string {
	switch {
	case recorded != "":
		return recorded
	case s.RC != "" && s.RC != "on":
		return s.RC
	}
	return "unknown"
}

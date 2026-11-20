package main

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

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// sendStdinMax is how many bytes of text `curia send -` reads from stdin.
const sendStdinMax = 64 << 10

// sendPanePattern is the only form of pane `curia send` and `curia read`
// accept (plan section 4.8).
var sendPanePattern = regexp.MustCompile(`^%[0-9]+$`)

// sendPaneError is a refusal of sendPaneSession: the pane is not a live
// agent session's, or (shared) more than one live session is bound to it.
type sendPaneError struct {
	msg    string
	shared bool
}

func (e sendPaneError) Error() string { return e.msg }

// sendShared reports whether err is sendPaneSession's refusal of a shared
// pane.
func sendShared(err error) bool {
	var pe sendPaneError
	return errors.As(err, &pe) && pe.shared
}

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "send <pane>|@all|@<repo>/* <text...>|-",
		Short: "Send a message to the agent session in a pane",
		Long: "Stores a message for the one live agent session in <pane> (a pane id such as %12)\n" +
			"and delivers it: typed into the pane when that is safe, otherwise at the session's\n" +
			"next stop or by its check_messages. The text is the remaining arguments, or stdin\n" +
			"with -, at most 800 characters. Run from inside an agent session, the message is\n" +
			"delivered as coming from another agent session, never from the user, and a session\n" +
			"cannot send to itself or into its own pane. @all and @<repo>/* send to every live\n" +
			"session (of this tmux server) or of one repository: only the user at a terminal may,\n" +
			"after the count and the list and a typed y.",
		Args: cobra.MinimumNArgs(2),
		RunE: runSend,
	})
}

// sendCheckPane refuses a pane that is not a pane id.
func sendCheckPane(pane string) error {
	if !sendPanePattern.MatchString(pane) {
		return sendPaneError{msg: fmt.Sprintf("pane %q is not a pane id like %%12", sanitize.Line(pane, 20))}
	}
	return nil
}

// sendPaneSession is the one live session (neither ended nor stale) bound to
// pane on the caller's own tmux server (Ruling 133: a pane id is matched only
// with its server; share one helper with show.go's showPane when plan-6
// merges). It refuses with a sendPaneError when pane is not a pane id, when no
// live session is in it, and (shared, see sendShared) when more than one is.
func sendPaneSession(ctx context.Context, st *store.Store, pane string) (model.Session, error) {
	if err := sendCheckPane(pane); err != nil {
		return model.Session{}, err
	}
	socket := seenSocket(os.Getenv("TMUX"))
	if socket == "" {
		return model.Session{}, sendPaneError{msg: "a pane id names a pane on your tmux server, and TMUX is not set here: use a session or task target instead"}
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return model.Session{}, err
	}
	var live []model.Session
	for _, s := range sessions {
		if s.Pane == pane && !fleet.OtherSocket(socket, s) && s.State != model.StateEnded && s.State != model.StateStale {
			live = append(live, s)
		}
	}
	switch len(live) {
	case 0:
		return model.Session{}, sendPaneError{msg: "pane " + pane + " is not the pane of a live agent session"}
	case 1:
		return live[0], nil
	}
	return model.Session{}, sendPaneError{msg: "pane " + pane + " is shared by more than one live session", shared: true}
}

// sendText is the message text: the arguments after the pane joined by
// spaces, or stdin (at most sendStdinMax bytes) for a lone "-", normalised
// by message.Normalize.
func sendText(stdin io.Reader, args []string) (string, error) {
	raw := strings.Join(args, " ")
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, sendStdinMax+1))
		if err != nil {
			return "", fmt.Errorf("read the text from stdin: %w", err)
		}
		if len(b) > sendStdinMax {
			return "", errors.New("the text on stdin is over 64 KiB; a message holds at most 800 characters")
		}
		raw = string(b)
	}
	return message.Normalize(raw)
}

// sendSender is who sends: the user, or an agent with its verified session
// (id and row; both empty when unverified). It is the user only when no
// session is claimed (CURIA_SESSION_ID, or TMUX_PANE in a session's pane,
// or CLAUDE_CODE_SESSION_ID) and the caller is the user at a terminal
// (userAtTerminal, Rulings 112 and 119). An agent's session is read from
// its processes (ancestorSession): the nearest ancestor that is the first
// process of a live agent's pane names it. A claimed session must be that
// one; a claim of another session, a claim with no agent found, or an
// ancestry that cannot be read leave the sender unverified, which is
// stored as an unknown agent sender: never typed, and counted with the
// other unknown senders (Ruling 113). No flag or other variable makes it
// the user.
func sendSender(ctx context.Context, st *store.Store, claimed model.Session, claimedID string) (model.Sender, model.Session, string) {
	if claimedID == "" && userAtTerminal() {
		return model.SenderUser, model.Session{}, ""
	}
	found, ok := ancestorSession()
	switch {
	case !ok || found == "", claimedID != "" && claimedID != found:
		return model.SenderAgent, model.Session{}, ""
	case claimedID == found:
		return model.SenderAgent, claimed, found
	}
	sess, known, err := st.GetSession(ctx, found)
	if err != nil || !known {
		return model.SenderAgent, model.Session{}, ""
	}
	return model.SenderAgent, sess, found
}

// sendCheckSelf refuses a message into the caller's own session or pane:
// target is the caller's session, the caller's session row is bound to
// target's pane, or the caller runs in that pane (TMUX_PANE).
func sendCheckSelf(target, sender model.Session, senderID string) error {
	switch {
	case senderID != "" && senderID == target.ID,
		sender.Pane != "" && sender.Pane == target.Pane && !fleet.OtherSocket(target.Socket, sender),
		os.Getenv("TMUX_PANE") == target.Pane:
		return errors.New("refusing to send into your own session or pane")
	}
	return nil
}

func runSend(cmd *cobra.Command, args []string) error {
	addr, rest := message.ParseAddress(args[0])
	broadcast := addr.Broadcast && rest == "" // "@all" or "@<repo>/*", nothing else
	if !broadcast {
		if err := sendCheckPane(args[0]); err != nil {
			return err
		}
	}
	text, err := sendText(cmd.InOrStdin(), args[1:])
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
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	ctx, tm := cmd.Context(), newTmuxClient()
	if err = reconcile(ctx, st, tm, cfg.LogPath, clock()); err != nil {
		return err
	}
	if broadcast {
		return sendBroadcast(ctx, cmd, sayNewSource(st, tm, logger, cfg, model.AnsweredCLI), args[0], text)
	}
	return sendDeliver(ctx, cmd.OutOrStdout(), st, tm, logger, args[0], text)
}

// sendBroadcastShown bounds the targets listed before the question.
const sendBroadcastShown = 10

// sendBroadcast is `curia send @all <text>` and `curia send @<repo>/* <text>`:
// only the user at a terminal broadcasts (the sender is read from process
// ancestry as for any send, Ruling 113; an agent, or a sender Curia cannot
// identify, is refused), and always after seeing the count and a bounded
// list and typing y (Ruling 152).
func sendBroadcast(ctx context.Context, cmd *cobra.Command, src *saySource, addr, text string) error {
	claimed, claimedID := mcpSession(ctx, src.st, src.log)
	if sender, _, _ := sendSender(ctx, src.st, claimed, claimedID); sender != model.SenderUser {
		return errors.New("refused: an agent cannot broadcast; only the user at a terminal sends to several sessions")
	}
	plan, err := src.propose(ctx, addr+" "+text)
	if err != nil {
		return err
	}
	p, w := plan.p, cmd.OutOrStdout()
	if len(p.Targets) == 0 {
		return errors.New("not a broadcast")
	}
	if !sayIsTerminal() {
		return errors.New("a broadcast always needs your confirmation, and there is no terminal to ask on")
	}
	_, _ = fmt.Fprintln(w, p.Candidates[0].Label)
	shown := p.Targets[:min(len(p.Targets), sendBroadcastShown)]
	_, _ = fmt.Fprintln(w, sayList(shown))
	if more := len(p.Targets) - len(shown); more > 0 {
		_, _ = fmt.Fprintf(w, "  … and %d more\n", more)
	}
	_, _ = fmt.Fprintf(w, "send to %d sessions? [y/N] ", len(p.Targets))
	line, _ := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 64)).ReadString('\n')
	if strings.TrimSpace(line) != "y" {
		_, err = fmt.Fprintln(w, "nothing sent")
		return err
	}
	out, _, err := src.send(ctx, ui.Choice{Proposal: p, Target: p.Candidates[0], Kind: p.Kind})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, sanitize.Line(out.Message.ID, 20)+" "+out.Note)
	return err
}

// sendDeliver stores text as a queued instruction for the live session in
// pane, delivers it with deliver.Message and prints the outcome.
func sendDeliver(ctx context.Context, w io.Writer, st *store.Store, tm tmux.Client, logger *slog.Logger, pane, text string) error {
	target, err := sendPaneSession(ctx, st, pane)
	if err != nil {
		return err
	}
	claimed, claimedID := mcpSession(ctx, st, logger)
	sender, senderSess, senderID := sendSender(ctx, st, claimed, claimedID)
	// Into the caller's own session or pane, claimed or verified: refused.
	if err = errors.Join(sendCheckSelf(target, claimed, claimedID), sendCheckSelf(target, senderSess, senderID)); err != nil {
		return errors.New("refusing to send into your own session or pane")
	}
	m, err := st.CreateMessage(ctx, model.Message{
		Text: text, Kind: model.KindInstruction, Sender: sender, SenderSession: senderID,
		TargetSession: target.ID, RoutedBy: model.RoutedSend, Status: model.MsgQueued, CreatedAt: clock(),
	})
	if errors.Is(err, store.ErrTooMany) { // Ruling 113: plain words for the sending agent
		return errors.New("not sent: " + strings.TrimPrefix(err.Error(), store.ErrTooMany.Error()+": "))
	}
	if err != nil {
		return err
	}
	out, err := deliver.Message(ctx, st, tm, logger, m.ID, clock())
	if err != nil {
		return err
	}
	lines := sayOutcomeLine(out) // the one outcome line of say and send
	if sender == model.SenderAgent && senderSess.ID == "" {
		lines += "\n" + sanitize.Line(m.ID, 20) + ": Curia cannot identify the sending agent session, so this " +
			"message is never typed; the target gets it at its next stop or with check_messages."
	}
	_, err = fmt.Fprintln(w, lines)
	return err
}

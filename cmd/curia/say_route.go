package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/route"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// sayRouterLimit bounds the router model call (plan section 4.10): the
// child is killed when it runs out. A variable only so tests can shorten it.
var sayRouterLimit = 5 * time.Second

// sayRouterWaitDelay bounds how long the router call waits for the child's
// output pipe once the child has exited or been killed.
const sayRouterWaitDelay = 100 * time.Millisecond

// sayRouterArgs is the router call (Ruling 99: no --json-schema). No
// argument is ever the message: the prompt goes on stdin.
var sayRouterArgs = []string{"-p", "--model", "haiku", "--output-format", "json", "--safe-mode", "--no-session-persistence", "--tools", ""}

// sayRouterKeep are the only variables the router child gets: probe P6's
// environment, which is enough for the CLI to find its login. Nothing of
// tmux, Curia or Claude Code passes.
var sayRouterKeep = []string{"HOME", "USER", "LOGNAME", "PATH", "LANG"}

// sayRouterMaxTargets bounds the targets put into the prompt. Sessions,
// repositories and global are always offered; tasks fill what is left.
const sayRouterMaxTargets = 60

// errSayNoClaude is the router's error when claude is not on PATH.
var errSayNoClaude = errors.New("claude is not on PATH")

// sayRouter runs the router model: prompt on stdin, the envelope on stdout.
// Tests replace it.
var sayRouter = sayRunClaude

// sayRouterEnv is the router child's environment: this process's values of
// sayRouterKeep and nothing else, so no TMUX, TMUX_PANE, CURIA_* or CLAUDE*
// variable reaches it.
func sayRouterEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); slices.Contains(sayRouterKeep, name) {
			env = append(env, kv)
		}
	}
	return env
}

// sayCapped keeps the first max bytes written to it and notes whether more
// came; it never fails a write, so the child is not stopped by a broken
// pipe before it is killed.
type sayCapped struct {
	buf  []byte
	max  int
	over bool
}

func (w *sayCapped) Write(p []byte) (int, error) {
	room := w.max - len(w.buf)
	if len(p) > room {
		w.over = true
		w.buf = append(w.buf, p[:max(room, 0)]...)
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// sayRunClaude runs `claude` (found on PATH) with sayRouterArgs in dir, with
// sayRouterEnv, the prompt on stdin and stderr discarded, and returns its
// stdout. The child runs in its own process group, which is killed at
// sayRouterLimit or when ctx ends; stdout over route.MaxRouterOutput is an
// error. It is never retried.
func sayRunClaude(ctx context.Context, dir string, prompt []byte) ([]byte, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, errSayNoClaude
	}
	ctx, cancel := context.WithTimeout(ctx, sayRouterLimit)
	defer cancel()
	out := &sayCapped{max: route.MaxRouterOutput}
	cmd := exec.CommandContext(ctx, path, sayRouterArgs...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout = dir, sayRouterEnv(), bytes.NewReader(prompt), out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = sayRouterWaitDelay
	err = cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, err
	case out.over:
		return nil, fmt.Errorf("router output over %d bytes", route.MaxRouterOutput)
	}
	return out.buf, nil
}

// sayRouterDir is the router's working directory, <dir of the log
// file>/router, created 0700 and emptied of anything in it.
func sayRouterDir(logPath string) (string, error) {
	dir := filepath.Join(filepath.Dir(filepath.Clean(logPath)), "router")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the router directory: %w", err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("the router directory %s is not a directory", sanitize.Line(dir, 200))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read the router directory: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return "", fmt.Errorf("empty the router directory: %w", err)
		}
	}
	return dir, nil
}

// sayPlan is a proposal and what storing it needs: every candidate, the
// model's suggestion when one was accepted, and whether the message passed
// every auto-send guard but sampling.
type sayPlan struct {
	p        ui.Proposal
	all      []route.Candidate
	sugg     route.Suggestion
	asked    bool // sugg holds the model's accepted suggestion
	eligible bool
	why      string // why auto-send did not apply, for a suggestion
}

// sayPropose builds the proposal for text: an address that resolves, else
// the model, else a manual pick.
func sayPropose(ctx context.Context, st *store.Store, cfg config.Config, text string, now time.Time) (ui.Proposal, error) {
	plan, err := sayPlanFor(ctx, st, cfg, text, now)
	return plan.p, err
}

// sayPlanFor is sayPropose with what the say source stores beside the
// proposal.
func sayPlanFor(ctx context.Context, st *store.Store, cfg config.Config, text string, now time.Time) (sayPlan, error) {
	norm, err := message.Normalize(text)
	if err != nil {
		return sayPlan{}, sayTextError()
	}
	addr, rest := message.ParseAddress(norm)
	if rest == "" {
		return sayPlan{}, errors.New("the message is empty after its address")
	}
	cands, sessions, err := sayCandidates(ctx, st, now)
	if err != nil {
		return sayPlan{}, err
	}
	kind := model.KindInstruction
	if addr.HasKind {
		kind = addr.Kind
	}
	if addr.TaskID != "" || addr.Name != "" {
		return sayAddressed(addr, rest, kind, cands, sessions, cfg.Routing.Aliases)
	}
	return sayAsk(ctx, st, cfg, rest, kind, addr.HasKind, cands)
}

// sayTextError names the message's limit.
func sayTextError() error {
	return fmt.Errorf("the message must be 1 to %d characters once cleaned", message.MaxText)
}

// sayCandidates lists the targets (route.Candidates) over the sessions
// that are not ended and the task cards, and returns the sessions too.
func sayCandidates(ctx context.Context, st *store.Store, now time.Time) ([]route.Candidate, []model.Session, error) {
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, nil, err
	}
	cards, err := taskCards(ctx, st, now)
	if err != nil {
		return nil, nil, err
	}
	return route.Candidates(sessions, cards), sessions, nil
}

// sayAddressed proposes the candidates the address names (route.Resolve).
// A name that names only teammates names their leads: a team's messages go
// to its lead. Nothing named is an error.
func sayAddressed(addr message.Address, rest string, kind model.MessageKind, cands []route.Candidate,
	sessions []model.Session, aliases map[string]string,
) (sayPlan, error) {
	matches := route.Resolve(addr, cands, aliases)
	if len(matches) == 0 && addr.TaskID == "" {
		matches = sayTeamLeads(sayUnalias(addr.Name, aliases), sessions, cands)
	}
	matches = route.ForKind(kind, matches, cands)
	if len(matches) == 0 {
		if addr.TaskID != "" {
			return sayPlan{}, fmt.Errorf("%s is not a target: no such task, or it is in review, done or dropped", addr.TaskID)
		}
		return sayPlan{}, fmt.Errorf("nothing is named @%s: not an alias, task id, repository or branch", addr.Name)
	}
	p := ui.Proposal{Text: rest, Kind: kind, Candidates: matches, Routed: model.RoutedAddress}
	if len(matches) > 1 {
		p.Note = fmt.Sprintf("%s names %d targets", sayAddrName(addr), len(matches))
	}
	return sayPlan{p: p, all: cands}, nil
}

// sayAddrName is how the address is written: T-42 or @name.
func sayAddrName(addr message.Address) string {
	if addr.TaskID != "" {
		return addr.TaskID
	}
	return "@" + addr.Name
}

// sayUnalias replaces name by its alias value, once, as route.Resolve does.
func sayUnalias(name string, aliases map[string]string) string {
	if v, ok := aliases[name]; ok {
		return v
	}
	for k, v := range aliases {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return name
}

// sayTeamLeads is the lead candidates of the live teammates on branch name.
func sayTeamLeads(name string, sessions []model.Session, cands []route.Candidate) []route.Candidate {
	var teams []string
	for _, s := range sessions {
		if s.TeamRole == model.RoleTeammate && s.TeamID != "" && s.Branch == name &&
			s.State != model.StateEnded && s.State != model.StateStale {
			teams = append(teams, s.TeamID)
		}
	}
	var out []route.Candidate
	for _, c := range cands {
		if c.Type == route.TargetSession && c.Session.TeamRole == model.RoleLead && slices.Contains(teams, c.Session.TeamID) {
			out = append(out, c)
		}
	}
	return out
}

// sayAsk asks the router model once and proposes its suggestion, the pick
// first and the runner-up second; on any failure it proposes every
// candidate for a manual pick. A kind given in the text wins over the
// model's.
func sayAsk(ctx context.Context, st *store.Store, cfg config.Config, rest string, kind model.MessageKind,
	hasKind bool, cands []route.Candidate,
) (sayPlan, error) {
	sugg, note := sayAskModel(ctx, cfg.LogPath, rest, cands)
	if note != "" {
		return sayPlan{p: ui.Proposal{Text: rest, Kind: kind, Candidates: cands, Routed: model.RoutedManual, Note: note}, all: cands}, nil
	}
	plan := sayPlan{all: cands, sugg: sugg, asked: true}
	shown := sugg
	if hasKind {
		shown.Kind = kind
	}
	send, eligible, why, err := sayAuto(ctx, st, cfg, shown, cands)
	if err != nil {
		return sayPlan{}, err
	}
	plan.eligible, plan.why = eligible, why
	plan.p = ui.Proposal{
		Text: rest, Kind: shown.Kind, Candidates: sayOrder(cands, sugg), Confidence: sugg.Confidence,
		Routed: model.RoutedConfirmed, Auto: send,
	}
	if cfg.Routing.AutoSend && !send {
		plan.p.Note = "not sent automatically: " + why
	}
	return plan, nil
}

// sayAskModel makes the one router call for rest and returns the accepted
// suggestion, or a note saying why there is none. A failure is logged,
// without the prompt or the model's output.
func sayAskModel(ctx context.Context, logPath, rest string, cands []route.Candidate) (route.Suggestion, string) {
	dir, err := sayRouterDir(logPath)
	if err != nil {
		logWarn(logPath, "say: router not run", "err", err.Error())
		return route.Suggestion{}, "the router could not run; pick a target"
	}
	offered := sayOffered(cands)
	rctx, cancel := context.WithTimeout(ctx, sayRouterLimit)
	defer cancel()
	out, err := sayRouter(rctx, dir, []byte(route.Prompt(rest, offered)))
	var note string
	switch {
	case errors.Is(err, errSayNoClaude):
		note = "claude is not on PATH, so no route was suggested; pick a target"
	case rctx.Err() != nil:
		note = fmt.Sprintf("the router model did not answer in time (%v); pick a target", sayRouterLimit)
	case err != nil:
		note = "the router model failed; pick a target"
	}
	if note == "" {
		s, perr := route.ParseSuggestion(out, offered)
		if perr == nil {
			return s, ""
		}
		err, note = perr, "the router model gave no usable answer; pick a target"
	}
	if err == nil {
		err = rctx.Err()
	}
	logWarn(logPath, "say: no route suggested", "err", sanitize.Line(fmt.Sprint(err), 300))
	return route.Suggestion{}, note
}

// sayOffered bounds the targets put into the prompt at
// sayRouterMaxTargets: every candidate but tasks, and the first tasks that
// fit. Only offered keys are accepted from the model.
func sayOffered(cands []route.Candidate) []route.Candidate {
	if len(cands) <= sayRouterMaxTargets {
		return cands
	}
	room := sayRouterMaxTargets
	for _, c := range cands {
		if c.Type != route.TargetTask {
			room--
		}
	}
	out := make([]route.Candidate, 0, sayRouterMaxTargets)
	for _, c := range cands {
		if c.Type != route.TargetTask || room > 0 {
			if c.Type == route.TargetTask {
				room--
			}
			out = append(out, c)
		}
	}
	return out
}

// sayOrder is what Tab cycles through: the pick, the runner-up, then the
// other candidates in their order.
func sayOrder(cands []route.Candidate, s route.Suggestion) []route.Candidate {
	out := []route.Candidate{s.Target}
	if s.RunnerUp.Key != "" {
		out = append(out, s.RunnerUp)
	}
	for _, c := range cands {
		if c.Key != s.Target.Key && c.Key != s.RunnerUp.Key {
			out = append(out, c)
		}
	}
	return out
}

// sayAuto applies the auto-send guards (route.Auto) to suggestion s, after
// reading the gate, the trip flag and the eligible count; with auto-send
// not configured it reads nothing.
func sayAuto(ctx context.Context, st *store.Store, cfg config.Config, s route.Suggestion, cands []route.Candidate) (send, eligible bool, why string, err error) {
	if !cfg.Routing.AutoSend {
		return false, false, "auto-send is not configured", nil
	}
	_, tripped, err := st.RoutingState(ctx)
	if err != nil {
		return false, false, "", err
	}
	labels, err := st.RouteLabels(ctx, cfg.Routing.AutoThreshold, route.Window)
	if err != nil {
		return false, false, "", err
	}
	n, err := st.AutoEligibleCount(ctx)
	if err != nil {
		return false, false, "", err
	}
	send, eligible, why = route.Auto(route.AutoInput{
		Configured: true, Tripped: !tripped.IsZero(), Threshold: cfg.Routing.AutoThreshold,
		Gate: route.Gate(labels), Suggestion: s, TargetAlive: sayTargetAlive(ctx, st, s.Kind, s.Target, cands),
		EligibleSoFar: n,
	})
	return send, eligible, why, nil
}

// sayTargetAlive reports whether an instruction to c would reach a session
// that, read again from the store, is neither ended nor stale. Any other
// kind has no session to check.
func sayTargetAlive(ctx context.Context, st *store.Store, kind model.MessageKind, c route.Candidate, cands []route.Candidate) bool {
	if kind != model.KindInstruction {
		return true
	}
	t, err := route.Settle(kind, c, cands)
	if err != nil {
		return false
	}
	s, found, err := st.GetSession(ctx, t.Session)
	return err == nil && found && s.State != model.StateEnded && s.State != model.StateStale
}

// saySettleError is a route.Settle refusal in the user's words.
func saySettleError(err error) error {
	if errors.Is(err, route.ErrNoTarget) {
		return errors.New(sanitize.Line(strings.TrimPrefix(err.Error(), route.ErrNoTarget.Error()+": "), 300))
	}
	return err
}

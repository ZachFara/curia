package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/message"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/state"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/transcript"
)

// Env is what Run needs from outside the payload. Every func must be set.
// Git and PaneInfo may be called at the same time.
type Env struct {
	// Pane is the tmux pane id ($TMUX_PANE), empty outside tmux.
	Pane string
	// Now is the clock.
	Now func() time.Time
	// Git returns, for cwd, the main repository's top level, the current
	// branch and, in a linked worktree, the worktree's top level (else
	// empty), or empty strings when cwd is not in a repository.
	Git func(cwd string) (repo, branch, worktree string)
	// PaneInfo returns the name of the tmux session and the id of the window
	// (for example "@7") that hold pane.
	PaneInfo func(pane string) (tmuxSession, window string)
	// TaskID is the task named by the environment (CURIA_TASK_ID), already
	// validated, or empty.
	TaskID string
}

// startInfo is what a SessionStart learns from git and tmux before the
// session's write transaction begins.
type startInfo struct {
	repo, branch, worktree, tmuxSession, window string
}

// Result is what Run did: the session as stored, and what curia hook must
// write to stdout. Stdout is nil except on a Stop that claimed queued
// answers or messages: then it is one line, {"decision":"block","reason":...}
// and a newline, which blocks the stop and hands Claude the answers, then
// the messages, as the reason.
type Result struct {
	Session model.Session
	Stdout  []byte
	// Claimed holds the ids of the answers a Stop claimed, which Stdout
	// delivers; the caller logs them (M8).
	Claimed []string
	// ClaimedMessages holds the ids of the messages a Stop claimed, which
	// Stdout delivers after the answers; the caller logs them too.
	ClaimedMessages []string
}

// Run records one hook event and returns a Result: the session as stored,
// and on a Stop that claimed answers the line to print. In order it parses
// the payload, appends the raw event, and on SessionStart (but not a
// compaction's for a live session, Ruling 74), or for a session it has no
// row for yet (one first seen mid-run, Ruling 29), ends any other session
// in the pane and asks git and tmux about it. Then in one store
// transaction it loads the session, folds in the transcript's new lines
// (PostToolUse, Notification, Stop), applies the state machine and writes the
// result. Git and tmux run before that transaction, so no subprocess runs
// while the write lock is held, and only once per session besides
// SessionStart. A session with no task takes env.TaskID on any event; one
// that has a task keeps it.
//
// On Stop the same transaction also counts the session's open blocking
// tickets and claims its queued answers and instructions (plan sections 4.3
// and 4.9): a claim blocks the stop (Result.Stdout) and leaves the session
// working; otherwise an open blocking ticket makes it needs_decision. A
// UserPromptSubmit whose prompt names an answer or a message typed into the
// session confirms it (Ruling 86), unless Claude Code built the prompt
// itself (message.AgentBuilt, Ruling 102); no other event touches the
// decisions or the messages.
//
// A transcript that cannot be read does not stop the update: the session is
// written without it and the read error is returned with it. The same holds
// for a claim that fails (it is rolled back, so that kind claims nothing
// and is not printed, and the other kind is still delivered) and for the
// read before the claims that fails (it counts as no open ticket and
// nothing queued).
func Run(ctx context.Context, kind model.EventKind, payload []byte, env Env, st *store.Store, cfg config.Config) (Result, error) {
	ev, err := Parse(kind, payload, env.Pane, env.Now())
	if err != nil {
		return Result{}, err
	}
	ev.TaskID = env.TaskID
	if err = st.AppendEvent(ctx, ev, payload); err != nil {
		return Result{}, err
	}
	// A compaction's SessionStart (Rulings 74 and 74a) starts only a session
	// with no row yet or an ended one; a live one goes on as it was.
	starting := kind == model.SessionStart && !state.Compaction(ev)
	if !starting {
		var prev model.Session
		var found bool
		if prev, found, err = st.GetSession(ctx, ev.SessionID); err != nil {
			return Result{}, err
		}
		// First seen mid-run, or an ended session a compaction revives.
		starting = !found || (kind == model.SessionStart && !state.Continues(prev, ev))
	}
	var start *startInfo
	if starting {
		if start, err = sessionStart(ctx, ev, env, st); err != nil {
			return Result{}, err
		}
	}
	if kind == model.Stop {
		return runStop(ctx, ev, start, st, cfg)
	}
	if kind == model.UserPromptSubmit && confirms(ev.Prompt) {
		return runConfirming(ctx, ev, start, st, cfg)
	}
	var out model.Session
	var readErr error
	err = st.UpdateSession(ctx, ev.SessionID, func(s model.Session, _ bool) model.Session {
		out, readErr = apply(s, ev, start, cfg, nil)
		return out
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Session: out}, readErr
}

// The words a typed line starts with, and so what a prompt that may confirm
// one holds.
const (
	answerMarker  = "Decision D-"
	messageMarker = "Message M-"
)

// confirms reports whether prompt may confirm a typed line: it holds an
// answer's or a message's marker and Claude Code did not build it
// (message.AgentBuilt, Ruling 102). Any other prompt costs no statement.
func confirms(prompt string) bool {
	return (strings.Contains(prompt, answerMarker) || strings.Contains(prompt, messageMarker)) &&
		!message.AgentBuilt(prompt)
}

// runConfirming is Run's transaction for a UserPromptSubmit whose prompt
// may name a typed answer or message (Ruling 86): the session update and
// the confirmations are one write, so a busy database cannot let the update
// through and lose a confirmation (re-review N4). Each kind's statement
// runs only when the prompt holds its marker; a failed one rolls the whole
// update back.
func runConfirming(ctx context.Context, ev model.Event, start *startInfo, st *store.Store, cfg config.Config) (Result, error) {
	var out model.Session
	var readErr error
	err := st.UpdateSessionTx(ctx, ev.SessionID, func(tx *store.Tx, s model.Session, _ bool) (model.Session, error) {
		out, readErr = apply(s, ev, start, cfg, nil)
		var err error
		if strings.Contains(ev.Prompt, answerMarker) {
			_, err = tx.ConfirmTyped(ctx, ev.SessionID, ev.Prompt, ev.At)
		}
		if err == nil && strings.Contains(ev.Prompt, messageMarker) {
			_, err = tx.ConfirmTypedMessages(ctx, ev.SessionID, ev.Prompt, ev.At)
		}
		return out, err
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Session: out}, readErr
}

// maxReason is the most runes a whole Stop reason may hold (plan section
// 4.9, Plan 2's bound).
const maxReason = 4000

// stopClaims is what one Stop claimed, and the reason that hands it over.
type stopClaims struct {
	answers  []model.Decision
	messages []model.Message
	reason   string
}

// runStop is Run's transaction for a Stop: apply with the session's queued
// answers and instructions claimed and its open blocking tickets counted on
// the same transaction. The block line is written only once the transaction
// has committed, so nothing is announced unless its claim is stored.
func runStop(ctx context.Context, ev model.Event, start *startInfo, st *store.Store, cfg config.Config) (Result, error) {
	var out model.Session
	var claimed stopClaims
	var errs error
	err := st.UpdateSessionTx(ctx, ev.SessionID, func(tx *store.Tx, s model.Session, _ bool) (model.Session, error) {
		var readErr, decideErr error
		out, readErr = apply(s, ev, start, cfg, func(ev *model.Event) bool {
			var open bool
			claimed, open, decideErr = decide(ctx, tx, ev)
			return open
		})
		errs = errors.Join(readErr, decideErr)
		return out, nil
	})
	if err != nil {
		return Result{}, err
	}
	res := Result{Session: out}
	if claimed.reason == "" {
		return res, errs
	}
	res.Stdout, err = blockLine(claimed.reason)
	for _, d := range claimed.answers {
		res.Claimed = append(res.Claimed, d.ID)
	}
	for _, m := range claimed.messages {
		res.ClaimedMessages = append(res.ClaimedMessages, m.ID)
	}
	return res, errors.Join(errs, err)
}

// decide reads, on tx at ev's time, the open blocking tickets of ev's
// session and whether answers or instructions are queued for it, in one
// small statement (store.StopState); only for a kind that is queued does it
// run that kind's claim, so the common Stop prepares no claim. Answers come
// first: the messages get what the answers leave of maxReason, less the
// line break that joins the two, so the reason never passes maxReason; what
// does not fit stays queued for the next Stop. It sets ev.StopBlocked when
// it claimed anything. A failed claim has been rolled back and claims
// nothing, and the other kind is still claimed; a failed read counts as no
// ticket and nothing queued. Errors are returned.
func decide(ctx context.Context, tx *store.Tx, ev *model.Event) (stopClaims, bool, error) {
	state, readErr := tx.StopState(ctx, ev.SessionID, ev.At)
	var c stopClaims
	var answerErr, messageErr error
	if state.Answers {
		c.answers, answerErr = tx.ClaimDeliveries(ctx, ev.SessionID, ev.At)
	}
	answers := decision.StopReason(c.answers)
	if state.Messages {
		budget := maxReason
		if answers != "" {
			budget -= utf8.RuneCountInString(answers) + 1
		}
		c.messages, messageErr = tx.ClaimMessages(ctx, ev.SessionID, ev.At, budget)
	}
	c.reason = answers
	if block := message.StopBlock(c.messages); block != "" {
		if c.reason != "" {
			c.reason += "\n"
		}
		c.reason += block
	}
	ev.StopBlocked = c.reason != ""
	return c, state.OpenBlocking > 0, errors.Join(readErr, answerErr, messageErr)
}

// blockLine is the hook output that blocks a Stop with reason: one line of
// JSON, with HTML escaping off, and a newline. JSON escapes the reason's
// own line breaks, so it is one line.
func blockLine(reason string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	err := enc.Encode(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{"block", reason})
	if err != nil {
		return nil, fmt.Errorf("stop block line: %w", err)
	}
	return b.Bytes(), nil
}

// sessionStart ends the other sessions in ev's pane and asks git and tmux
// about the new one. Git and tmux run at the same time: each is a subprocess
// of several milliseconds, and together they would use most of the budget.
func sessionStart(ctx context.Context, ev model.Event, env Env, st *store.Store) (*startInfo, error) {
	start := &startInfo{}
	if ev.Pane == "" {
		start.repo, start.branch, start.worktree = env.Git(ev.Cwd)
		return start, nil
	}
	if err := st.EndOthersInPane(ctx, ev.Pane, ev.SessionID, ev.At); err != nil {
		return start, err
	}
	var wg sync.WaitGroup
	var panicked any
	wg.Go(func() {
		defer func() { panicked = recover() }()
		start.tmuxSession, start.window = env.PaneInfo(ev.Pane)
	})
	start.repo, start.branch, start.worktree = env.Git(ev.Cwd)
	wg.Wait()
	if panicked != nil {
		panic(panicked) // on the caller's goroutine, where it can be recovered
	}
	return start, nil
}

// apply is the update Run makes inside the store transaction. start is what
// git and tmux said, or nil when they were not asked. decisions, set on
// Stop only, runs after the transcript is folded in: it may set
// ev.StopBlocked and reports whether the session has an open blocking
// ticket. It is not called for an event the state machine ignores. apply
// itself must not call the store.
func apply(s model.Session, ev model.Event, start *startInfo, cfg config.Config, decisions func(*model.Event) bool) (model.Session, error) {
	if state.Ignores(s, ev) {
		return s, nil // the transcript is not read either
	}
	enrich(&s, ev, start)
	var err error
	switch ev.Kind {
	case model.PostToolUse, model.Notification, model.Stop:
		err = readTranscript(&s, &ev, cfg)
	}
	// Claude Code may run Stop before the final answer is in the transcript,
	// so the payload's copy wins (Ruling 36).
	if first := transcript.FirstLine(ev.LastAssistantMessage); ev.Kind == model.Stop && first != "" {
		ev.Message = first
	}
	open := false
	if decisions != nil {
		open = decisions(&ev)
	}
	return state.Apply(s, ev, open), err
}

// enrich sets what git, tmux and the environment said about the session.
// start is nil when git and tmux were not asked. A session keeps the task it
// has; an empty one takes ev's, on any event, so a missing CURIA_TASK_ID (a
// teammate's process) never clears a task.
func enrich(s *model.Session, ev model.Event, start *startInfo) {
	fillEmpty(&s.TaskID, ev.TaskID)
	switch {
	case start == nil: // git and tmux were not asked
	case ev.Kind == model.SessionStart && !state.Continues(*s, ev):
		s.Repo, s.Branch, s.Worktree = start.repo, start.branch, start.worktree
		s.TmuxSession, s.TmuxWindow = start.tmuxSession, start.window
	default:
		// First seen mid-run (a compaction's SessionStart included). A
		// SessionStart may have written the row since Run looked, so fill
		// only what is still empty.
		fillEmpty(&s.Repo, start.repo)
		fillEmpty(&s.Branch, start.branch)
		fillEmpty(&s.Worktree, start.worktree)
		fillEmpty(&s.TmuxSession, start.tmuxSession)
		fillEmpty(&s.TmuxWindow, start.window)
	}
}

// fillEmpty sets *field to v when *field is empty.
func fillEmpty(field *string, v string) {
	if *field == "" {
		*field = v
	}
}

// readTranscript reads the transcript's lines after s.TranscriptOffset into
// s and, on Stop, puts the last assistant text into ev.Message (which apply
// replaces with the payload's last_assistant_message when there is one).
func readTranscript(s *model.Session, ev *model.Event, cfg config.Config) error {
	path := s.TranscriptPath
	if path == "" { // first seen mid-run: Apply takes the path from ev
		path = ev.TranscriptPath
	}
	if path == "" {
		return nil
	}
	upd, offset, err := transcript.Read(path, s.TranscriptOffset)
	if err != nil {
		return fmt.Errorf("session %s: %w", ev.SessionID, err)
	}
	s.TranscriptOffset = offset
	if upd.Found {
		s.Model, s.ContextUsed = upd.Model, upd.ContextUsed
		s.ContextMax = contextMax(cfg, upd.Model)
	}
	if ev.Kind == model.Stop {
		ev.Message = upd.LastAssistantText
	}
	return nil
}

// contextMax is the configured context window for the model, or the default.
func contextMax(cfg config.Config, modelName string) int {
	if n, ok := cfg.ContextMax[modelName]; ok {
		return n
	}
	return cfg.DefaultContextMax
}

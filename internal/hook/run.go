package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/decision"
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
	// Git returns the repository top level and current branch for cwd, or
	// empty strings when cwd is not in a repository.
	Git func(cwd string) (repo, branch string)
	// PaneInfo returns the name of the tmux session and the id of the window
	// (for example "@7") that hold pane.
	PaneInfo func(pane string) (tmuxSession, window string)
}

// startInfo is what a SessionStart learns from git and tmux before the
// session's write transaction begins.
type startInfo struct {
	repo, branch, tmuxSession, window string
}

// Result is what Run did: the session as stored, and what curia hook must
// write to stdout. Stdout is nil except on a Stop that claimed queued
// answers: then it is one line, {"decision":"block","reason":...} and a
// newline, which blocks the stop and hands Claude the answers as the
// reason.
type Result struct {
	Session model.Session
	Stdout  []byte
}

// Run records one hook event and returns the session as stored. In order it
// parses the payload, appends the raw event, and on SessionStart, or for a
// session it has no row for yet (one first seen mid-run, Ruling 29), ends any
// other session in the pane and asks git and tmux about it. Then in one store
// transaction it loads the session, folds in the transcript's new lines
// (PostToolUse, Notification, Stop), applies the state machine and writes the
// result. Git and tmux run before that transaction, so no subprocess runs
// while the write lock is held, and only once per session besides
// SessionStart.
//
// On Stop the same transaction also claims the session's queued answers and
// counts its open blocking tickets (plan sections 4.3 and 4.9): claimed
// answers block the stop (Result.Stdout) and leave the session working;
// otherwise an open blocking ticket makes it needs_decision. No other event
// reads the decisions.
//
// A transcript that cannot be read does not stop the update: the session is
// written without it and the read error is returned with it. The same holds
// for a claim that fails (it is rolled back, so nothing is claimed or
// printed) and for a count that fails (it counts as none).
func Run(ctx context.Context, kind model.EventKind, payload []byte, env Env, st *store.Store, cfg config.Config) (Result, error) {
	ev, err := Parse(kind, payload, env.Pane, env.Now())
	if err != nil {
		return Result{}, err
	}
	if err = st.AppendEvent(ctx, ev, payload); err != nil {
		return Result{}, err
	}
	starting := kind == model.SessionStart
	if !starting {
		var found bool
		if _, found, err = st.GetSession(ctx, ev.SessionID); err != nil {
			return Result{}, err
		}
		starting = !found // first seen mid-run
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

// runStop is Run's transaction for a Stop: apply with the session's queued
// answers claimed and its open blocking tickets counted on the same
// transaction. The block line is built only once the transaction has
// committed, so an answer is never announced unless its claim is stored.
func runStop(ctx context.Context, ev model.Event, start *startInfo, st *store.Store, cfg config.Config) (Result, error) {
	var out model.Session
	var claimed []model.Decision
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
	if len(claimed) > 0 {
		res.Stdout, err = blockLine(claimed)
		errs = errors.Join(errs, err)
	}
	return res, errs
}

// decide claims, on tx at ev's time, the queued answers of ev's session and
// counts its open blocking tickets, and sets ev.StopBlocked when it claimed
// any. A failed claim has been rolled back and claims nothing; a failed
// count counts as none. Both errors are returned.
func decide(ctx context.Context, tx *store.Tx, ev *model.Event) ([]model.Decision, bool, error) {
	claimed, claimErr := tx.ClaimDeliveries(ctx, ev.SessionID, ev.At)
	n, countErr := tx.OpenBlocking(ctx, ev.SessionID, ev.At)
	ev.StopBlocked = len(claimed) > 0
	return claimed, n > 0, errors.Join(claimErr, countErr)
}

// blockLine is the hook output that blocks a Stop to deliver claimed: one
// line of JSON, with HTML escaping off, and a newline. JSON escapes the
// reason's own line breaks, so it is one line.
func blockLine(claimed []model.Decision) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	err := enc.Encode(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}{"block", decision.StopReason(claimed)})
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
		start.repo, start.branch = env.Git(ev.Cwd)
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
	start.repo, start.branch = env.Git(ev.Cwd)
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
	switch {
	case ev.Kind == model.SessionStart:
		s.Repo, s.Branch = start.repo, start.branch
		s.TmuxSession, s.TmuxWindow = start.tmuxSession, start.window
	case start != nil:
		// First seen mid-run. A SessionStart may have written the row since
		// Run looked, so fill only what is still empty.
		fillEmpty(&s.Repo, start.repo)
		fillEmpty(&s.Branch, start.branch)
		fillEmpty(&s.TmuxSession, start.tmuxSession)
		fillEmpty(&s.TmuxWindow, start.window)
	}
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

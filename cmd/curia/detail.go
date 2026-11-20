package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/transcript"
	"github.com/ZachFara/curia/internal/ui"
)

// The agent detail view's source (spec F24). It only reads: the store's
// getters, a bounded tail of the transcript (read here, never written,
// pushed or served) and the review view's diff stat. The popup's Detail
// call runs in the view's own command with a deadline, never on the render
// path; the branch cost is the review view's costlens cache, which never waits.

const (
	detailTicketLimit  = 20
	detailMessageLimit = 10
	detailDiffWidth    = 72
)

// errDetailNoActivity is what the activity source says until F27 exists.
var errDetailNoActivity = errors.New("activity time is not available")

// detailActivity is F27's data (time by class and the last 10 events), as lines. F27 is built apart: its
// implementation replaces detailNoActivity; with an error the view shows no Activity section.
type detailActivity interface {
	Activity(ctx context.Context, sessionID string) ([]string, error)
}

type detailNoActivity struct{}

func (detailNoActivity) Activity(context.Context, string) ([]string, error) {
	return nil, errDetailNoActivity
}

// detailActivitySource is the activity source the view uses; tests replace it.
var detailActivitySource detailActivity = detailNoActivity{}

type detailSource struct {
	st  *store.Store
	cfg config.Config
}

func newDetailSource(st *store.Store, cfg config.Config) ui.DetailSource {
	return detailSource{st: st, cfg: cfg}
}

// Detail reads the session's rows and transcript tail.
func (d detailSource) Detail(ctx context.Context, id string) (ui.DetailData, error) {
	s, found, err := d.st.GetSession(ctx, id)
	if err != nil {
		return ui.DetailData{}, err
	}
	if !found {
		return ui.DetailData{}, fmt.Errorf("no session %s", id)
	}
	out := ui.DetailData{Session: s}
	if out.Tail, err = transcript.Tail(s.TranscriptPath, 30); err != nil {
		out.TailNote = "transcript unreadable"
	} else if len(out.Tail) == 0 {
		out.TailNote = "no transcript lines yet (none recorded, missing, rotated or empty)"
	}
	if out.Tickets, err = d.tickets(ctx, s); err != nil {
		return out, err
	}
	if out.Messages, err = d.messages(ctx, s); err != nil {
		return out, err
	}
	if t, ok, _ := d.st.GetTask(ctx, s.TaskID); ok && s.TaskID != "" {
		out.TaskCostUSD, out.TaskCostKnown = costlensTaskCost(d.cfg, t)
	}
	if lines, aerr := detailActivitySource.Activity(ctx, id); aerr == nil {
		out.Activity, out.ActivityOK = lines, true
	}
	return out, nil
}

// tickets are the session's decisions and its task's, newest first by id order, without repeats.
func (d detailSource) tickets(ctx context.Context, s model.Session) ([]model.Decision, error) {
	own, err := d.st.ListDecisions(ctx, store.DecisionFilter{SessionID: s.ID, Limit: detailTicketLimit})
	if err != nil {
		return nil, err
	}
	task, err := d.st.TaskDecisions(ctx, s.TaskID)
	if err != nil {
		return nil, err
	}
	for _, t := range task {
		if !slices.ContainsFunc(own, func(o model.Decision) bool { return o.ID == t.ID }) {
			own = append(own, t)
		}
	}
	return own[:min(len(own), 2*detailTicketLimit)], nil
}

// messages are the session's or its task's messages not yet settled: held, queued and delivered.
func (d detailSource) messages(ctx context.Context, s model.Session) ([]model.Message, error) {
	all, err := d.st.ListMessages(ctx, store.MessageFilter{
		Statuses: []model.MessageStatus{model.MsgHeld, model.MsgQueued, model.MsgDelivered}, Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	var out []model.Message
	for _, m := range all {
		if m.TargetSession == s.ID || (s.TaskID != "" && m.TargetTask == s.TaskID) {
			if out = append(out, m); len(out) == detailMessageLimit {
				break
			}
		}
	}
	return out, nil
}

// Diff is the review view's diff stat of the session's task; a session with no task or worktree has none.
func (d detailSource) Diff(ctx context.Context, id string) (ui.DetailDiff, error) {
	s, found, err := d.st.GetSession(ctx, id)
	if err != nil || !found || s.TaskID == "" {
		return ui.DetailDiff{}, err
	}
	t, ok, err := d.st.GetTask(ctx, s.TaskID)
	if err != nil || !ok || t.Worktree == "" {
		return ui.DetailDiff{}, err
	}
	stat, note, _ := reviewDiffStat(ctx, t, detailDiffWidth)
	return ui.DetailDiff{Has: true, Stat: stat, Note: note}, nil
}

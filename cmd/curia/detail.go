package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/transcript"
	"github.com/ZachFara/curia/internal/ui"
	"github.com/ZachFara/curia/internal/usage"
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
	out.Activity, out.ActivityOK = d.activity(ctx, s)
	out.Resources = d.resources(ctx, s)
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

// detailActivityWindow is how far back the Activity panel looks; detailActivityEvents is how many events it lists.
const (
	detailActivityWindow = 24 * time.Hour
	detailActivityEvents = 10
)

// activity is F27's time by class for this one session (its own events in the
// window only, with the user's and its repository's rules) and its last
// events, as lines; false with no events. It runs in the view's background
// read, under that read's deadline.
func (d detailSource) activity(ctx context.Context, s model.Session) ([]string, bool) {
	now := clock()
	since := now.Add(-detailActivityWindow)
	all, err := d.st.ActivityEvents(ctx, since, s.ID)
	evs := all[s.ID]
	if err != nil || len(evs) == 0 {
		return nil, false
	}
	t := usage.ClassifyActivity(evs, activityRulesFor(d.cfg.Activity.Rules)(s.Repo), usage.ActivityOpts{
		IdleCap: max(d.cfg.Activity.IdleCap, time.Minute), Since: since, Now: now,
		Live: s.State != model.StateEnded && s.State != model.StateStale,
	})
	lines := []string{usage.ActivityHeadline(t.ByClass, usage.WallTime(t))}
	var split []string
	for _, c := range usage.ActivityClasses {
		if dur := t.ByClass[c]; dur > 0 {
			split = append(split, c+" "+usage.ActivityDuration(dur))
		}
	}
	if len(split) > 0 {
		lines = append(lines, strings.Join(split, " · "))
	}
	for _, e := range evs[max(len(evs)-detailActivityEvents, 0):] {
		lines = append(lines, sanitize.Line(e.At.Local().Format("15:04:05")+" "+string(e.Kind)+" "+e.Tool, 80))
	}
	return lines, true
}

// resources is the CPU and memory line of the monitor's latest sample of the
// session: the numbers only while current, else a ? that says why.
func (d detailSource) resources(ctx context.Context, s model.Session) string {
	latest, err := d.st.LatestResources(ctx)
	r := latest[s.ID]
	if err != nil || !usage.ResourceFresh(r.At, clock(), d.cfg.Resources.Interval) {
		return "? (the monitor is not running, or has no current sample of this session)"
	}
	return "CPU " + usage.ResourceCell(r.CPU, r.RSSMB*1024, r.At, clock(), d.cfg.Resources.Interval)
}

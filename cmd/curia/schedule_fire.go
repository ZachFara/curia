package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
)

// F22: the monitor's schedule job. A due schedule is claimed in the store
// (one claimer wins, so two monitors or a restart never fire it twice), its
// recipients are resolved then, and one queued instruction per session is
// stored as the user's, in one broadcast, and delivered through
// deliver.Message: the one guarded typing path (an empty prompt, one
// unconfirmed line per session, only a registered pane), else left for the
// Stop hook or check_messages. The monitor types nothing itself.
// shortcut: the monitor ticks every resources.interval, so a schedule fires
// up to that late, not at the exact second; upgrade by sleeping to next_run_at.

// The per-session results of a run.
const (
	resDelivered = "delivered"
	resQueued    = "queued"
	resBusy      = "skipped_busy"
	resNotLim    = "skipped_not_limited"
)

// fireSchedules fires every due schedule once.
func (e *monitorEnv) fireSchedules(ctx context.Context) error {
	now := e.now()
	due, err := e.st.DueSchedules(ctx, now)
	if err != nil {
		return err
	}
	var errs []error
	for _, sc := range due {
		if err = e.fireSchedule(ctx, sc, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *monitorEnv) fireSchedule(ctx context.Context, sc store.Schedule, now time.Time) error {
	var next time.Time // zero: done (a one-time or on-reset schedule)
	if sc.Kind == "daily" {
		next = scheduleNextDaily(sc.Spec, now)
	}
	outcome := "fired"
	late := scheduleLate(sc.NextRun, now, e.cfg.Schedule.LateWindow)
	if late {
		outcome = "skipped_late"
	}
	runID, ok, err := e.st.ClaimSchedule(ctx, sc, next, outcome, now)
	if err != nil || !ok || late {
		return err
	}
	// Past the claim the run is recorded whatever happens; a failure leaves
	// the run as it is, "fired" with no results, and is logged by the caller.
	outcome, results, err := e.deliverScheduled(ctx, sc, now)
	if ferr := e.st.FinishScheduleRun(ctx, runID, outcome, results); ferr != nil {
		err = errors.Join(err, ferr)
	}
	return err
}

// scheduleRecipients are the live sessions the schedule's target names now:
// not ended, not stale, no teammate (a team's messages go to its lead).
func scheduleRecipients(sessions []model.Session, target string, aliases map[string]string) []model.Session {
	var out []model.Session
	for _, s := range sessions {
		if s.State == model.StateEnded || s.State == model.StateStale || s.TeamRole == model.RoleTeammate {
			continue
		}
		kind, name, _ := strings.Cut(target, ":")
		switch kind {
		case "repo":
			if s.Repo == "" || !strings.EqualFold(filepath.Base(s.Repo), sayUnalias(name, aliases)) {
				continue
			}
		case "task":
			if s.TaskID != name {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// scheduleLimited reports whether s was stopped by the usage limit, by the
// fallback definition of F22: some window's latest sample is at or above
// pct and the session is idle and went idle after that sample. (A real
// limit stop, a StopFailure rate_limit, has not been observed.)
func scheduleLimited(s model.Session, usage []model.UsageSample, pct float64) bool {
	return s.State == model.StateIdle && slices.ContainsFunc(usage, func(u model.UsageSample) bool {
		return u.UsedPct >= pct && s.UpdatedAt.After(u.At)
	})
}

// deliverScheduled stores and delivers sc's messages. It returns the run's
// outcome and its results (JSON).
func (e *monitorEnv) deliverScheduled(ctx context.Context, sc store.Schedule, now time.Time) (string, string, error) {
	sessions, err := e.st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return "fired", "", err
	}
	targets := scheduleRecipients(sessions, sc.Target, e.cfg.Routing.Aliases)
	if len(targets) == 0 {
		return "skipped_no_target", `{"sessions":{}}`, nil
	}
	var usage []model.UsageSample
	if sc.OnlyLimited {
		if usage, err = e.st.LatestUsage(ctx); err != nil {
			return "fired", "", err
		}
	}
	res := map[string]string{}
	var rows []model.Message
	for _, s := range targets {
		switch {
		case sc.IfIdle && s.State != model.StateIdle:
			res[s.ID] = resBusy
		case sc.OnlyLimited && !scheduleLimited(s, usage, e.cfg.Schedule.LimitedPct):
			res[s.ID] = resNotLim
		default:
			rows = append(rows, model.Message{
				Text: scheduledText(sc), Kind: model.KindInstruction, Sender: model.SenderUser, RoutedBy: model.RoutedAddress,
				Status: model.MsgQueued, CreatedAt: now, TargetSession: s.ID, TargetTask: s.TaskID, TargetRepo: s.Repo,
			})
		}
	}
	var stored []model.Message
	if len(rows) > 0 {
		if stored, err = e.st.CreateBroadcast(ctx, rows); err != nil {
			return "fired", "", err
		}
	}
	broadcast := ""
	for _, m := range stored {
		broadcast = m.BroadcastID
		res[m.TargetSession] = resQueued
		if out, derr := deliver.Message(ctx, e.st, e.tmuxFor(socketOf(targets, m.TargetSession)), e.log, m.ID, now); derr != nil {
			e.log.Warn("monitor: scheduled delivery failed", "schedule", sc.ID, "message", m.ID, "err", sanitize.Line(derr.Error(), 200))
		} else if out.Message.Status == model.MsgDelivered {
			res[m.TargetSession] = resDelivered
		}
	}
	b, _ := json.Marshal(struct {
		Broadcast string            `json:"broadcast,omitempty"`
		Sessions  map[string]string `json:"sessions"`
	}{broadcast, res})
	return "fired", string(b), nil
}

func socketOf(sessions []model.Session, id string) string {
	for _, s := range sessions {
		if s.ID == id {
			return s.Socket
		}
	}
	return ""
}

// scheduledText labels the message as the schedule's.
func scheduledText(sc store.Schedule) string {
	return fmt.Sprintf("(scheduled %s) %s", sc.ID, sc.Text)
}

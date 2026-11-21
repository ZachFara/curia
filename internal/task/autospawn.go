package task

import (
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// The checks AutoEligible can name: the spec's refusal names (F29), plus the
// reasons a task is not a candidate at all (never logged as a refusal).
const (
	AutoOff        = "off"
	AutoNotReady   = "not_ready"
	AutoPaused     = "paused"
	AutoNoHash     = "no_hash"
	AutoEdited     = "edited"
	AutoBlocked    = "blocked"
	AutoBypass     = "bypass"
	AutoMaxAgents  = "max_agents"
	AutoMaxPerDay  = "max_per_day"
	AutoInterval   = "interval"
	AutoForecast   = "forecast"
	AutoMemory     = "memory"
	AutoBudget     = "budget"
	AutoSetupUnapp = "setup_unapproved"
)

// AutoGate is what the checks of a tick read, for one task's repository.
type AutoGate struct {
	Now                      time.Time
	Enabled                  bool // tasks.auto_spawn.enabled in effect for the task's repository
	Bypass                   bool // some live session's mode is bypass
	Live, MaxAgents          int
	StartedToday, MaxPerDay  int
	LastStart                time.Time // the last auto-spawn, or failed attempt; zero: none
	Interval                 time.Duration
	ForecastWarn, WindowFull bool
	MemKnown                 bool // fresh Docker memory samples exist
	MemPct, MemRefusePct     float64
}

// AutoCand is one task and what is known about it.
type AutoCand struct {
	Task            model.Task
	Status          model.TaskStatus // derived (Derive)
	OpenBlocking    int
	BudgetOver      bool
	SetupUnapproved bool
}

// AutoVerdict says whether a task may be started without the user. Candidate
// is true when the task itself qualifies (so a refusal is worth logging);
// Check names the first rule that failed and Reason says it in words. The
// reason never contains task text.
type AutoVerdict struct {
	OK, Candidate bool
	Check, Reason string
}

// AutoEligible is F29's whole rule, in the spec's order: the setting, then
// the task itself (ready, not paused, marked ready by the user at a
// terminal with text unchanged since, no open blocking ticket), then the
// gate (bypass, live agents, starts today, spacing, forecast, memory), then
// the per-task skips (budget, setup script). It fails on the first rule.
func AutoEligible(g AutoGate, c AutoCand) AutoVerdict {
	if !g.Enabled {
		return autoNo(false, AutoOff, "auto-spawn is off for this repository")
	}
	if v, bad := autoTaskRule(c); bad {
		return v
	}
	if v, bad := autoGateRule(g); bad {
		return v
	}
	switch {
	case c.BudgetOver:
		return autoNo(true, AutoBudget, "its budget is spent")
	case c.SetupUnapproved:
		return autoNo(true, AutoSetupUnapp, "the repository's .curia/setup.sh is not approved: curia setup approve")
	}
	return AutoVerdict{OK: true, Candidate: true}
}

func autoNo(cand bool, check, why string) AutoVerdict {
	return AutoVerdict{Candidate: cand, Check: check, Reason: why}
}

// autoTaskRule is the rules about the task itself.
func autoTaskRule(c AutoCand) (AutoVerdict, bool) {
	switch {
	case c.Status != model.TaskReady:
		return autoNo(false, AutoNotReady, "its status is "+string(c.Status)+", not ready"), true
	case !c.Task.PausedAt.IsZero():
		return autoNo(false, AutoPaused, "it is paused (curia resume)"), true
	case c.Task.ReadyHash == "":
		return autoNo(false, AutoNoHash, "you have not marked it ready at a terminal since it last changed state (a web move never counts): run curia task ready"), true
	case c.Task.ReadyHash != model.ReadyHash(c.Task.Title, c.Task.Body):
		return autoNo(false, AutoEdited, "its title or body changed after it was marked ready: mark it ready again"), true
	case c.OpenBlocking > 0:
		return autoNo(false, AutoBlocked, "it has an open blocking ticket"), true
	}
	return AutoVerdict{}, false
}

// autoGateRule is checks 1 to 6 of the spec, after the setting.
func autoGateRule(g AutoGate) (AutoVerdict, bool) {
	switch {
	case g.Bypass:
		return autoNo(true, AutoBypass, "a session is in bypass mode"), true
	case g.Live >= g.MaxAgents:
		return autoNo(true, AutoMaxAgents, "the live agents are at tasks.auto_spawn.max_agents"), true
	case g.StartedToday >= g.MaxPerDay:
		return autoNo(true, AutoMaxPerDay, "the starts in the last 24 hours are at tasks.auto_spawn.max_per_day"), true
	case !g.LastStart.IsZero() && g.Now.Sub(g.LastStart) < g.Interval:
		return autoNo(true, AutoInterval, "the last start was less than tasks.auto_spawn.interval ago"), true
	case g.ForecastWarn || g.WindowFull:
		return autoNo(true, AutoForecast, "the usage forecast warns or a usage window is full"), true
	case g.MemKnown && g.MemPct > g.MemRefusePct:
		return autoNo(true, AutoMemory, "Docker memory is over the refuse threshold"), true
	}
	return AutoVerdict{}, false
}

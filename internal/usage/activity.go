package usage

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
)

// F27 activity time: classify a session's hook events into time per class.
// Pure; the store read is store.ActivityEvents. Classification happens at
// read time from the stored raw payloads, so rules can change and history
// is reclassified.

// Activity classes (spec F27).
const (
	ClassWaitingMe = "waiting on me"
	ClassCoding    = "coding"
	ClassReading   = "reading"
	ClassTesting   = "testing"
	ClassBuilding  = "building"
	ClassCI        = "waiting on CI"
	ClassGit       = "git"
	ClassThinking  = "thinking"
	ClassOther     = "other"
)

// ActivityClasses lists the classes in report order.
var ActivityClasses = []string{ClassCoding, ClassReading, ClassTesting, ClassBuilding, ClassCI, ClassGit, ClassThinking, ClassWaitingMe, ClassOther}

const (
	maxActivityRules   = 64  // user rules
	maxActivityPattern = 200 // bytes of one pattern
	maxActivityCommand = 200 // runes of a Bash command that rules see
	maxActivityTool    = 100 // runes of a tool name that rules see
)

type activityRule struct {
	class      string
	tool, cmd  *regexp.Regexp // nil matches anything
	bashOnlyCm bool           // cmd is tested only against Bash commands
}

// ActivityRules is a compiled, ordered rules table.
type ActivityRules struct{ rules []activityRule }

// defaultActivityRules are the spec's defaults, in match order: CI before
// git (gh), a command that starts with git before testing (so a commit
// message cannot make it testing), testing before building (make test), and
// git after a shell separator last.
var defaultActivityRules = []config.ActivityRule{
	{Class: ClassCoding, Tool: `^(Edit|Write|MultiEdit|NotebookEdit)$`},
	{Class: ClassReading, Tool: `^(Read|Grep|Glob|LS|WebFetch|WebSearch)$`},
	{Class: ClassCI, Match: `\bgh (run watch|pr checks\b.*--watch)`},
	{Class: ClassGit, Match: `^\s*(git|gh) `},
	{Class: ClassTesting, Match: `\b(go test|npm test|pytest|make test)\b`},
	{Class: ClassBuilding, Match: `\b(go build|npm run build|make|docker build)\b`},
	{Class: ClassGit, Match: `(^|&&|;|\|)\s*(git|gh) `},
}

// NewActivityRules compiles user rules (before the defaults). Go's regexp
// is RE2: matching is linear in the input, so a hostile pattern cannot
// make classification slow; the inputs are bounded as well, and the number
// and length of patterns are capped. An error names the rule and key.
func NewActivityRules(user []config.ActivityRule) (*ActivityRules, error) {
	if len(user) > maxActivityRules {
		return nil, fmt.Errorf("activity.rules: at most %d rules, got %d", maxActivityRules, len(user))
	}
	r := &ActivityRules{}
	for i, u := range user {
		if err := r.add(u, fmt.Sprintf("activity.rules[%d]", i)); err != nil {
			return nil, err
		}
	}
	for _, d := range defaultActivityRules {
		if err := r.add(d, "default rule "+d.Class); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *ActivityRules) add(c config.ActivityRule, name string) error {
	if !slices.Contains(ActivityClasses, c.Class) {
		return fmt.Errorf("%s.class: %q is not one of %s", name, c.Class, strings.Join(ActivityClasses, ", "))
	}
	if c.Tool == "" && c.Match == "" {
		return fmt.Errorf("%s: needs tool or match", name)
	}
	rule := activityRule{class: c.Class, bashOnlyCm: true}
	for _, p := range []struct {
		key, pat string
		dst      **regexp.Regexp
	}{{"tool", c.Tool, &rule.tool}, {"match", c.Match, &rule.cmd}} {
		if p.pat == "" {
			continue
		}
		if len(p.pat) > maxActivityPattern {
			return fmt.Errorf("%s.%s: at most %d bytes", name, p.key, maxActivityPattern)
		}
		re, err := regexp.Compile(p.pat)
		if err != nil {
			return fmt.Errorf("%s.%s: not a valid regular expression: %w", name, p.key, err)
		}
		*p.dst = re
	}
	if rule.cmd != nil && rule.tool == nil { // match alone means a Bash command
		rule.tool = regexp.MustCompile(`^Bash$`)
	}
	r.rules = append(r.rules, rule)
	return nil
}

// Class returns the class of a tool call: the first rule whose tool pattern
// matches the tool name and whose match pattern matches the first 200
// characters of a Bash command. No rule: other.
func (r *ActivityRules) Class(tool, command string) string {
	tool, command = cutRunes(tool, maxActivityTool), cutRunes(command, maxActivityCommand)
	if tool != "Bash" {
		command = ""
	}
	for _, rule := range r.rules {
		if rule.tool != nil && !rule.tool.MatchString(tool) {
			continue
		}
		if rule.cmd != nil && !rule.cmd.MatchString(command) {
			continue
		}
		return rule.class
	}
	return ClassOther
}

func cutRunes(s string, n int) string {
	if len(s) <= n { // bytes >= runes
		return s
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ActivityEvent is one stored hook event as the classifier needs it. Tool
// and Command are set on a PreToolUse only; Command is untrusted and is
// never shown or stored by activity.
type ActivityEvent struct {
	Kind          model.EventKind
	At            time.Time
	Tool, Command string
}

// ActivityOpts bounds one session's classification. Since and Now clip the
// intervals (a zero Since clips nothing); Live counts the open interval up
// to Now (the session has not ended).
type ActivityOpts struct {
	IdleCap    time.Duration
	Since, Now time.Time
	Live       bool
}

// Span is a closed stretch of agent time.
type Span struct{ Start, End time.Time }

// ActivityTime is one session's time per class and the spans it came from.
type ActivityTime struct {
	ByClass map[string]time.Duration
	Spans   []Span
}

// ClassifyActivity turns one session's events (in order) into time per
// class. A tool interval runs from its PreToolUse to its PostToolUse and
// parallel calls count once (first Pre to last Post, in the first call's
// class); the gap before a tool and after it is thinking; a Notification
// until the next event, and a turn end (or session start) until the next
// prompt, are waiting on me, the latter only up to IdleCap in all (a
// following idle Notification does not restart the cap).
//
// shortcut: the first event after the window start has no known earlier
// state, so the interval before it is dropped; a background command's
// runtime after its Post counts as what the events say (thinking or
// waiting on me) until the monitor tracks it (F19).
func ClassifyActivity(evs []ActivityEvent, rules *ActivityRules, o ActivityOpts) ActivityTime {
	r := &activityRun{o: o, out: ActivityTime{ByClass: map[string]time.Duration{}}}
	for _, e := range evs {
		r.flush(e.At)
		r.step(e, rules)
	}
	if o.Live {
		r.flush(o.Now)
	}
	return r.out
}

// activityRun is ClassifyActivity's state: the open interval's class and
// start, the open tool calls, and the idle cap left.
type activityRun struct {
	o              ActivityOpts
	out            ActivityTime
	cur, toolClass string
	t              time.Time
	depth          int
	capped         bool
	capLeft        time.Duration
}

// flush closes the open interval at to.
func (r *activityRun) flush(to time.Time) {
	defer func() { r.t = to }()
	if r.cur == "" || !to.After(r.t) {
		return
	}
	end := to
	if r.capped && r.cur == ClassWaitingMe {
		end = r.t.Add(min(to.Sub(r.t), r.capLeft))
		r.capLeft -= end.Sub(r.t)
	}
	a, b := r.t, end
	if a.Before(r.o.Since) {
		a = r.o.Since
	}
	if b.After(r.o.Now) {
		b = r.o.Now
	}
	if b.After(a) {
		r.out.ByClass[r.cur] += b.Sub(a)
		r.out.Spans = append(r.out.Spans, Span{a, b})
	}
}

// step opens the next interval for event e.
func (r *activityRun) step(e ActivityEvent, rules *ActivityRules) {
	switch e.Kind {
	case model.UserPromptSubmit:
		r.cur, r.capped, r.depth = ClassThinking, false, 0
	case model.PreToolUse:
		if r.depth == 0 {
			r.toolClass = rules.Class(e.Tool, e.Command)
			r.cur = r.toolClass
		}
		r.capped = false
		r.depth++
	case model.PostToolUse:
		switch {
		case r.depth == 0: // a Post with no open Pre
		case r.depth > 1:
			r.depth--
			r.cur = r.toolClass
		default:
			r.depth = 0
			r.cur = ClassThinking
		}
	case model.Notification:
		if r.cur != ClassWaitingMe || !r.capped {
			r.cur, r.capped = ClassWaitingMe, false
		}
	case model.Stop, model.SessionStart:
		r.cur, r.capped, r.capLeft, r.depth = ClassWaitingMe, true, r.o.IdleCap, 0
	case model.SessionEnd:
		r.cur, r.depth = "", 0
	}
}

// WallTime is the length of the union of every span of the sessions: the
// clock time with any session working.
func WallTime(ts ...ActivityTime) time.Duration {
	var spans []Span
	for _, a := range ts {
		spans = append(spans, a.Spans...)
	}
	slices.SortFunc(spans, func(a, b Span) int { return a.Start.Compare(b.Start) })
	var total time.Duration
	var end time.Time
	for _, s := range spans {
		if s.Start.Before(end) {
			s.Start = end
		}
		if s.End.After(s.Start) {
			total += s.End.Sub(s.Start)
			end = s.End
		}
	}
	return total
}

// ActivityTaskType is the first segment of the branch when it is one of
// types, else "untyped" (a spawned curia/T-<n>-<slug> branch is untyped,
// Ruling 155).
func ActivityTaskType(branch string, types []string) string {
	first, rest, ok := strings.Cut(branch, "/")
	if ok && rest != "" && slices.Contains(types, first) {
		return first
	}
	return "untyped"
}

// ActivityHeadline is the report's headline: the share of agent time spent
// waiting on me.
func ActivityHeadline(byClass map[string]time.Duration, wall time.Duration) string {
	var total time.Duration
	for _, d := range byClass {
		total += d
	}
	if total <= 0 {
		return "no agent time in this window"
	}
	pct := float64(byClass[ClassWaitingMe]) * 100 / float64(total)
	return fmt.Sprintf("waiting on me: %.0f%% of %s agent time (wall %s)", pct, ActivityDuration(total), ActivityDuration(wall))
}

// ActivityDuration is "6h 05m", or "7m" under an hour.
func ActivityDuration(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

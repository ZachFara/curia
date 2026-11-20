package usage

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/message"
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
	// Background commands (probe, Claude Code 2.1.329). On a PreToolUse:
	// Background is tool_input.run_in_background and ToolUseID the call's
	// id. On the PostToolUse (fired at once): ToolUseID and TaskID, the
	// response's backgroundTaskId. On a Stop: Listed are the ids of
	// background_tasks still running, and HasList says the field was there.
	// On a UserPromptSubmit: Prompt, its first bytes only (a task
	// notification names the finished id there). All untrusted and
	// bounded by the store read.
	Background bool
	ToolUseID  string
	TaskID     string
	Listed     []string
	HasList    bool
	Prompt     string
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
	// Background is the lifetime of background commands per class, whether
	// or not the agent was working meanwhile (overlapping commands each
	// count in full); Unfinished is how many had no completion seen.
	Background map[string]time.Duration
	Unfinished int
}

// ClassifyActivity turns one session's events (in order) into time per
// class. A tool interval runs from its PreToolUse to its PostToolUse and
// parallel calls count once (first Pre to last Post, in the first call's
// class); the gap before a tool and after it is thinking; a Notification
// until the next event, and a turn end (or session start) until the next
// prompt, are waiting on me, the latter only up to IdleCap in all (a
// following idle Notification does not restart the cap).
//
// Background commands (see backgroundCommands) overlap like this: while the
// agent works (a tool, thinking) its time is that work's class and the
// command adds nothing (agent time counts once); after a turn end, time
// while a command runs is that command's class (the first started, when
// several run) and not waiting on me, and it does not use up the idle cap:
// the user is not blamed for the agent waiting on its own command.
// A task notification or agent message is a system turn, not the user: it
// wakes the agent (thinking) but does not restart the idle cap.
//
// shortcut: the first event after the window start has no known earlier
// state, so the interval before it is dropped, and a command that started
// before the window is not seen; revisit if retention gets long.
func ClassifyActivity(evs []ActivityEvent, rules *ActivityRules, o ActivityOpts) ActivityTime {
	r := &activityRun{o: o, out: ActivityTime{ByClass: map[string]time.Duration{}, Background: map[string]time.Duration{}}}
	r.bg = backgroundCommands(evs, rules, o)
	for _, c := range r.bg {
		a, b := laterOf(c.start, o.Since), earlierOf(c.end, o.Now)
		if b.After(a) {
			r.out.Background[c.class] += b.Sub(a)
		}
		if !c.done && c.end.After(o.Since) {
			r.out.Unfinished++
		}
	}
	for _, e := range evs {
		r.flush(e.At)
		r.step(e, rules)
	}
	if o.Live {
		r.flush(o.Now)
	}
	return r.out
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlierOf(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// maxTaskID bounds a background task id (probe ids are 9 characters).
const maxTaskID = 64

// bgCommand is one background command: its lifetime and class. done says a
// completion was seen (a task notification, or a Stop that no longer lists
// it); otherwise it is unfinished.
type bgCommand struct {
	use, id, class string
	start, end     time.Time
	done           bool
}

// backgroundCommands finds a session's background commands: a PreToolUse
// with run_in_background starts one (class by the usual rules); its
// PostToolUse (same tool_use_id) names its task id; it ends at the task
// notification naming that id, else at the first Stop whose background_tasks
// no longer lists it. One with neither is unfinished and ends at the
// session's end, else now (live), else the last Stop that listed it (or the
// last event). A notification or Stop naming no known command is ignored.
func backgroundCommands(evs []ActivityEvent, rules *ActivityRules, o ActivityOpts) []*bgCommand {
	t := &bgTracker{seen: map[*bgCommand]time.Time{}}
	for _, e := range evs {
		t.last = e.At
		switch {
		case e.Kind == model.PreToolUse && e.Background:
			t.cmds = append(t.cmds, &bgCommand{use: e.ToolUseID, class: rules.Class(e.Tool, e.Command), start: e.At})
		case e.Kind == model.PostToolUse:
			t.named(e)
		case e.Kind == model.UserPromptSubmit && message.AgentBuilt(e.Prompt):
			t.notice(e)
		case e.Kind == model.Stop && e.HasList:
			t.stop(e)
		case e.Kind == model.SessionEnd:
			t.sessEnd = e.At
		}
	}
	for _, c := range t.cmds {
		if !c.done {
			c.end = laterOf(t.fallbackEnd(c, o), c.start)
		}
	}
	return t.cmds
}

// bgTracker is backgroundCommands' state: the commands, the last Stop that
// listed each, the session's end and its last event.
type bgTracker struct {
	cmds          []*bgCommand
	seen          map[*bgCommand]time.Time
	sessEnd, last time.Time
}

func (t *bgTracker) find(pred func(*bgCommand) bool) *bgCommand {
	for i := len(t.cmds) - 1; i >= 0; i-- {
		if pred(t.cmds[i]) {
			return t.cmds[i]
		}
	}
	return nil
}

// named gives the command whose PostToolUse this is its task id.
func (t *bgTracker) named(e ActivityEvent) {
	if e.TaskID == "" || len(e.TaskID) > maxTaskID {
		return
	}
	if c := t.find(func(c *bgCommand) bool { return c.id == "" && c.use == e.ToolUseID }); c != nil {
		c.id = e.TaskID
	}
}

// notice ends the command a task notification names; an unknown id is ignored.
func (t *bgTracker) notice(e ActivityEvent) {
	id := noticeTaskID(e.Prompt)
	if id == "" {
		return
	}
	if c := t.find(func(c *bgCommand) bool { return c.id == id && !c.done }); c != nil {
		c.end, c.done = e.At, true
	}
}

// stop notes which running commands a Stop lists; one it no longer lists ended.
func (t *bgTracker) stop(e ActivityEvent) {
	for _, c := range t.cmds {
		switch {
		case c.id == "" || c.done:
		case slices.Contains(e.Listed, c.id):
			t.seen[c] = e.At
		default:
			c.end, c.done = e.At, true
		}
	}
}

// fallbackEnd is where an unfinished command ends.
func (t *bgTracker) fallbackEnd(c *bgCommand, o ActivityOpts) time.Time {
	switch {
	case !t.sessEnd.IsZero():
		return t.sessEnd
	case o.Live:
		return o.Now
	case !t.seen[c].IsZero():
		return t.seen[c]
	}
	return t.last
}

// noticeTaskID is the id in a task notification's <task-id> element, within
// the bytes the store kept; "" when there is none or it is too long.
func noticeTaskID(prompt string) string {
	_, rest, ok := strings.Cut(prompt, "<task-id>")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "</task-id>")
	if !ok || len(id) > maxTaskID {
		return ""
	}
	return id
}

// activityRun is ClassifyActivity's state: the open interval's class and
// start, the open tool calls, the idle cap left, and the background commands.
type activityRun struct {
	o              ActivityOpts
	out            ActivityTime
	bg             []*bgCommand
	cur, toolClass string
	t              time.Time
	depth          int
	capped         bool
	wake           bool // a system turn woke the capped wait: the cap is not restarted
	capLeft        time.Duration
}

// add counts [a, b) as class, clipped to the window.
func (r *activityRun) add(class string, a, b time.Time) {
	a, b = laterOf(a, r.o.Since), earlierOf(b, r.o.Now)
	if b.After(a) {
		r.out.ByClass[class] += b.Sub(a)
		r.out.Spans = append(r.out.Spans, Span{a, b})
	}
}

// flush closes the open interval at to. A capped wait (after a turn end) is
// cut at the background commands' edges: where one runs it is that
// command's class and free of the cap; the rest is waiting on me, up to the cap.
func (r *activityRun) flush(to time.Time) {
	defer func() { r.t = to }()
	if r.cur == "" || !to.After(r.t) {
		return
	}
	if r.cur != ClassWaitingMe || !r.capped {
		r.add(r.cur, r.t, to)
		return
	}
	cuts := []time.Time{r.t, to}
	for _, c := range r.bg {
		for _, p := range []time.Time{c.start, c.end} {
			if p.After(r.t) && p.Before(to) {
				cuts = append(cuts, p)
			}
		}
	}
	slices.SortFunc(cuts, time.Time.Compare)
	for i := 1; i < len(cuts); i++ {
		a, b := cuts[i-1], cuts[i]
		if c := slices.IndexFunc(r.bg, func(c *bgCommand) bool { return !c.start.After(a) && !c.end.Before(b) }); c >= 0 {
			r.add(r.bg[c].class, a, b)
			continue
		}
		end := a.Add(min(b.Sub(a), r.capLeft))
		r.capLeft -= end.Sub(a)
		r.add(ClassWaitingMe, a, end)
	}
}

// prompt is a UserPromptSubmit: the user typing, or a system turn (a task
// notification, an agent message) that wakes the agent and nobody typed.
func (r *activityRun) prompt(e ActivityEvent) {
	if !message.AgentBuilt(e.Prompt) {
		r.cur, r.capped, r.depth, r.wake = ClassThinking, false, 0, false
		return
	}
	if r.cur == ClassWaitingMe || r.cur == "" {
		r.wake = r.wake || r.capped
		r.cur, r.capped = ClassThinking, false
	}
}

// step opens the next interval for event e.
func (r *activityRun) step(e ActivityEvent, rules *ActivityRules) {
	switch e.Kind {
	case model.UserPromptSubmit:
		r.prompt(e)
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
		if !r.wake || e.Kind == model.SessionStart {
			r.capLeft, r.wake = r.o.IdleCap, false
		}
		r.cur, r.capped, r.depth = ClassWaitingMe, true, 0
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

// ActivityBackgroundLine is one line on background commands, "" without any:
// their lifetime per class (shown beside the table because the agent may
// have worked meanwhile) and how many are unfinished.
func ActivityBackgroundLine(by map[string]time.Duration, unfinished int) string {
	var parts []string
	var total time.Duration
	for _, c := range ActivityClasses {
		if d := by[c]; d > 0 {
			parts = append(parts, c+" "+ActivityDuration(d))
			total += d
		}
	}
	if len(parts) == 0 && unfinished == 0 {
		return ""
	}
	line := fmt.Sprintf("background commands: %s (%s)", ActivityDuration(total), strings.Join(parts, ", "))
	if unfinished > 0 {
		line += fmt.Sprintf(", %d unfinished (no completion seen)", unfinished)
	}
	return line
}

// ActivityDuration is "6h 05m", or "7m" under an hour.
func ActivityDuration(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %02dm", m/60, m%60)
}

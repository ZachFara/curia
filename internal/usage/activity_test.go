package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
)

var actT0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func actAt(sec int) time.Time { return actT0.Add(time.Duration(sec) * time.Second) }

func mustRules(t *testing.T, user ...config.ActivityRule) *ActivityRules {
	t.Helper()
	r, err := NewActivityRules(user)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestActivityDefaultClasses(t *testing.T) {
	r := mustRules(t)
	for _, tc := range []struct{ tool, cmd, want string }{
		{"Edit", "", ClassCoding},
		{"Write", "", ClassCoding},
		{"MultiEdit", "", ClassCoding},
		{"NotebookEdit", "", ClassCoding},
		{"Read", "", ClassReading},
		{"Grep", "", ClassReading},
		{"Glob", "", ClassReading},
		{"LS", "", ClassReading},
		{"WebFetch", "", ClassReading},
		{"WebSearch", "", ClassReading},
		{"Bash", "go test ./...", ClassTesting},
		{"Bash", "cd x && npm test", ClassTesting},
		{"Bash", "pytest -q", ClassTesting},
		{"Bash", "make test", ClassTesting},
		{"Bash", "go build ./...", ClassBuilding},
		{"Bash", "npm run build", ClassBuilding},
		{"Bash", "make", ClassBuilding},
		{"Bash", "docker build .", ClassBuilding},
		{"Bash", "gh run watch 12", ClassCI},
		{"Bash", "gh pr checks 5 --watch", ClassCI},
		{"Bash", "git status", ClassGit},
		{"Bash", "gh pr view 3", ClassGit},
		{"Bash", "cd a && git diff", ClassGit},
		{"Bash", "./check.sh", ClassOther},
		{"Bash", "git commit -m 'go test'", ClassGit},
		{"Task", "", ClassOther},
		{"mcp__x__y", "", ClassOther},
		{"Read", "go test", ClassReading}, // a command is read only for Bash
	} {
		if got := r.Class(tc.tool, tc.cmd); got != tc.want {
			t.Errorf("Class(%s, %q) = %q, want %q", tc.tool, tc.cmd, got, tc.want)
		}
	}
}

func TestActivityUserRulesFirst(t *testing.T) {
	r := mustRules(t,
		config.ActivityRule{Class: ClassTesting, Match: `^\./check\.sh`},
		config.ActivityRule{Class: ClassReading, Tool: `^Edit$`})
	if got := r.Class("Bash", "./check.sh all"); got != ClassTesting {
		t.Errorf("check.sh = %q", got)
	}
	if got := r.Class("Edit", ""); got != ClassReading {
		t.Errorf("user rule did not beat the default: %q", got)
	}
	if got := r.Class("Bash", "go test"); got != ClassTesting {
		t.Errorf("defaults lost: %q", got)
	}
}

func TestActivityRulesValidation(t *testing.T) {
	many := make([]config.ActivityRule, maxActivityRules+1)
	for i := range many {
		many[i] = config.ActivityRule{Class: ClassGit, Match: "x"}
	}
	for name, tc := range map[string]struct {
		rules []config.ActivityRule
		want  string
	}{
		"bad regexp":    {[]config.ActivityRule{{Class: ClassGit, Match: "("}}, "activity.rules[0].match"},
		"bad tool":      {[]config.ActivityRule{{Class: ClassGit, Match: "x"}, {Class: ClassGit, Tool: "[a"}}, "activity.rules[1].tool"},
		"unknown class": {[]config.ActivityRule{{Class: "napping", Match: "x"}}, "activity.rules[0].class"},
		"empty rule":    {[]config.ActivityRule{{Class: ClassGit}}, "needs tool or match"},
		"too long":      {[]config.ActivityRule{{Class: ClassGit, Match: strings.Repeat("a", maxActivityPattern+1)}}, "at most"},
		"too many":      {many, "at most"},
	} {
		_, err := NewActivityRules(tc.rules)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestActivityHostilePatternIsFast(t *testing.T) {
	r := mustRules(t, config.ActivityRule{Class: ClassGit, Match: `(a+)+$`})
	start := time.Now()
	r.Class("Bash", strings.Repeat("a", 1_000_000)+"!")
	if d := time.Since(start); d > time.Second {
		t.Errorf("classification took %v", d)
	}
}

func ev(kind model.EventKind, sec int, tool, cmd string) ActivityEvent {
	return ActivityEvent{Kind: kind, At: actAt(sec), Tool: tool, Command: cmd}
}

func classify(t *testing.T, evs []ActivityEvent, o ActivityOpts) map[string]time.Duration {
	t.Helper()
	if o.IdleCap == 0 {
		o.IdleCap = 30 * time.Minute
	}
	if o.Now.IsZero() {
		o.Now = actAt(100000)
	}
	return ClassifyActivity(evs, mustRules(t), o).ByClass
}

func TestActivityScriptedStream(t *testing.T) {
	evs := []ActivityEvent{
		ev(model.UserPromptSubmit, 0, "", ""),
		ev(model.PreToolUse, 10, "Read", ""), // 10s thinking
		ev(model.PostToolUse, 15, "Read", ""),
		ev(model.PreToolUse, 17, "Bash", "go test ./..."), // 5s reading, 2s thinking
		ev(model.PostToolUse, 47, "Bash", ""),             // 30s testing
		ev(model.PreToolUse, 50, "Bash", "git status"),    // 3s thinking
		ev(model.PostToolUse, 51, "Bash", ""),             // 1s git
		ev(model.Notification, 60, "", ""),                // 9s thinking
		ev(model.PreToolUse, 100, "Edit", ""),             // 40s waiting on me
		ev(model.PostToolUse, 104, "Edit", ""),            // 4s coding
		ev(model.Stop, 110, "", ""),                       // 6s thinking
		ev(model.UserPromptSubmit, 410, "", ""),           // 300s waiting
		ev(model.SessionEnd, 420, "", ""),                 // 10s thinking
	}
	got := classify(t, evs, ActivityOpts{})
	want := map[string]time.Duration{
		ClassThinking: (10 + 2 + 3 + 9 + 6 + 10) * time.Second, ClassReading: 5 * time.Second,
		ClassTesting: 30 * time.Second, ClassGit: time.Second, ClassWaitingMe: 340 * time.Second,
		ClassCoding: 4 * time.Second,
	}
	checkTimes(t, got, want)
}

func checkTimes(t *testing.T, got, want map[string]time.Duration) {
	t.Helper()
	for c, d := range want {
		if got[c] != d {
			t.Errorf("%s = %v, want %v", c, got[c], d)
		}
	}
	for c, d := range got {
		if _, ok := want[c]; !ok && d != 0 {
			t.Errorf("unexpected %s = %v", c, d)
		}
	}
}

func TestActivityParallelToolsCountOnce(t *testing.T) {
	evs := []ActivityEvent{
		ev(model.PreToolUse, 0, "Bash", "go test ./..."),
		ev(model.PreToolUse, 2, "Read", ""),
		ev(model.PostToolUse, 5, "Read", ""),
		ev(model.PostToolUse, 40, "Bash", ""),
		ev(model.SessionEnd, 40, "", ""),
	}
	checkTimes(t, classify(t, evs, ActivityOpts{}), map[string]time.Duration{ClassTesting: 40 * time.Second})
}

func TestActivityIdleCap(t *testing.T) {
	evs := []ActivityEvent{
		ev(model.UserPromptSubmit, 0, "", ""),
		ev(model.Stop, 60, "", ""),
		ev(model.Notification, 120, "", ""), // idle_prompt: still the same capped wait
		ev(model.UserPromptSubmit, 7260, "", ""),
		ev(model.SessionEnd, 7270, "", ""),
	}
	checkTimes(t, classify(t, evs, ActivityOpts{}), map[string]time.Duration{
		ClassThinking: 70 * time.Second, ClassWaitingMe: 30 * time.Minute,
	})
}

func TestActivityLiveTailAndWindow(t *testing.T) {
	evs := []ActivityEvent{ev(model.UserPromptSubmit, 0, "", ""), ev(model.Stop, 100, "", "")}
	got := classify(t, evs, ActivityOpts{Live: true, Now: actAt(160)})
	checkTimes(t, got, map[string]time.Duration{ClassThinking: 100 * time.Second, ClassWaitingMe: 60 * time.Second})
	// Not live: the open interval is not counted.
	checkTimes(t, classify(t, evs, ActivityOpts{Now: actAt(160)}), map[string]time.Duration{ClassThinking: 100 * time.Second})
	// A window clips intervals.
	got = classify(t, evs, ActivityOpts{Live: true, Now: actAt(160), Since: actAt(90)})
	checkTimes(t, got, map[string]time.Duration{ClassThinking: 10 * time.Second, ClassWaitingMe: 60 * time.Second})
}

func TestActivityStrayPostIgnored(t *testing.T) {
	evs := []ActivityEvent{ev(model.UserPromptSubmit, 0, "", ""), ev(model.PostToolUse, 5, "Read", ""), ev(model.SessionEnd, 9, "", "")}
	checkTimes(t, classify(t, evs, ActivityOpts{}), map[string]time.Duration{ClassThinking: 9 * time.Second})
}

func TestActivityWallAndHeadline(t *testing.T) {
	a := ActivityTime{Spans: []Span{{actAt(0), actAt(100)}, {actAt(100), actAt(200)}}}
	b := ActivityTime{Spans: []Span{{actAt(50), actAt(300)}}}
	if got := WallTime(a, b); got != 300*time.Second {
		t.Errorf("wall = %v", got)
	}
	by := map[string]time.Duration{ClassWaitingMe: 38 * time.Hour / 10, ClassCoding: 62 * time.Hour / 10}
	if got := ActivityHeadline(by, 6*time.Hour+5*time.Minute); got != "waiting on me: 38% of 10h 00m agent time (wall 6h 05m)" {
		t.Errorf("headline = %q", got)
	}
	if got := ActivityHeadline(nil, 0); got != "no agent time in this window" {
		t.Errorf("empty headline = %q", got)
	}
}

func TestActivityTaskType(t *testing.T) {
	types := []string{"feat", "fix"}
	for branch, want := range map[string]string{
		"feat/x": "feat", "fix/y/z": "fix", "curia/T-1-slug": "untyped", "main": "untyped", "": "untyped", "docs/a": "untyped", "feat": "untyped",
	} {
		if got := ActivityTaskType(branch, types); got != want {
			t.Errorf("%q = %q, want %q", branch, got, want)
		}
	}
}

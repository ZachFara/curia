package main

// F22 schedule rules: the time forms, the late rule and the target spelling. Times are local.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/task"
)

// scheduleMaxActive caps the schedules that are active or paused (the spec names no
// cap; a typed-into-agents feature gets a small fixed one).
const scheduleMaxActive = 50

var (
	scheduleClockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	scheduleRepoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// scheduleParseAt reads --at: HH:MM (the next one), YYYY-MM-DDTHH:MM or +2h. The
// time must be after now.
func scheduleParseAt(s string, now time.Time) (time.Time, error) {
	var t time.Time
	switch {
	case scheduleClockRe.MatchString(s):
		t = scheduleNextDaily(s, now)
	case strings.HasPrefix(s, "+"):
		d, err := time.ParseDuration(s[1:])
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("--at %q: a duration after + must be positive, like +2h", s)
		}
		t = now.Add(d)
	default:
		var err error
		if t, err = time.ParseInLocation("2006-01-02T15:04", s, now.Location()); err != nil {
			return time.Time{}, fmt.Errorf("--at %q: want HH:MM, YYYY-MM-DDTHH:MM or +2h", s)
		}
	}
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("--at %q is not in the future", s)
	}
	return t, nil
}

// scheduleParseDaily checks --daily's HH:MM and returns it.
func scheduleParseDaily(s string) (string, error) {
	if !scheduleClockRe.MatchString(s) {
		return "", fmt.Errorf("--daily %q: want HH:MM (00:00 to 23:59)", s)
	}
	return s, nil
}

// scheduleNextDaily is the first HH:MM (spec, valid) strictly after after, in its
// location. shortcut: around a daylight-saving change the hour is whatever
// time.Date gives, not exact to the minute (the spec says so).
func scheduleNextDaily(spec string, after time.Time) time.Time {
	var h, m int
	_, _ = fmt.Sscanf(spec, "%d:%d", &h, &m)
	t := time.Date(after.Year(), after.Month(), after.Day(), h, m, 0, 0, after.Location())
	if !t.After(after) {
		t = time.Date(after.Year(), after.Month(), after.Day()+1, h, m, 0, 0, after.Location())
	}
	return t
}

// scheduleLate reports whether a trigger due at due is too old to fire at now.
func scheduleLate(due, now time.Time, window time.Duration) bool { return now.Sub(due) > window }

// scheduleParseTarget spells the one target a schedule names: "all", "repo:<name>"
// or "task:T-42". Exactly one of the three must be given.
func scheduleParseTarget(all bool, repo, taskID string) (string, error) {
	n := 0
	for _, set := range []bool{all, repo != "", taskID != ""} {
		if set {
			n++
		}
	}
	switch {
	case n != 1:
		return "", errors.New("name exactly one target: --all, --repo <name> or --task T-42")
	case all:
		return "all", nil
	case repo != "":
		if !scheduleRepoRe.MatchString(repo) {
			return "", fmt.Errorf("--repo %q: a repository name is 1 to 64 of A-Z a-z 0-9 . _ -", repo)
		}
		return "repo:" + repo, nil
	case !task.ValidID(taskID):
		return "", fmt.Errorf("--task %q is not a task id", taskID)
	}
	return "task:" + taskID, nil
}

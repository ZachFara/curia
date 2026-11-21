package main

import (
	"testing"
	"time"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local)

func TestParseAt(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		bad  bool
	}{
		{"02:00", time.Date(2026, 11, 21, 2, 0, 0, 0, time.Local), false}, // the next 02:00
		{"13:30", time.Date(2026, 11, 20, 13, 30, 0, 0, time.Local), false},
		{"12:00", time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local), false}, // now is not the future
		{"2026-12-01T09:15", time.Date(2026, 12, 1, 9, 15, 0, 0, time.Local), false},
		{"+2h", now.Add(2 * time.Hour), false},
		{"2026-11-20T11:00", time.Time{}, true}, // in the past
		{"+0s", time.Time{}, true},
		{"-2h", time.Time{}, true},
		{"25:00", time.Time{}, true},
		{"tomorrow", time.Time{}, true},
		{"", time.Time{}, true},
	}
	for _, c := range cases {
		got, err := scheduleParseAt(c.in, now)
		if (err != nil) != c.bad || (!c.bad && !got.Equal(c.want)) {
			t.Errorf("scheduleParseAt(%q) = %v, %v; want %v (bad %v)", c.in, got, err, c.want, c.bad)
		}
	}
}

func TestDaily(t *testing.T) {
	if _, err := scheduleParseDaily("24:00"); err == nil {
		t.Error("24:00 accepted")
	}
	spec, err := scheduleParseDaily("2:05")
	if err == nil {
		t.Errorf("2:05 accepted as %q; want HH:MM only", spec)
	}
	if got := scheduleNextDaily("02:00", now); !got.Equal(time.Date(2026, 11, 21, 2, 0, 0, 0, time.Local)) {
		t.Errorf("scheduleNextDaily after noon = %v", got)
	}
	if got := scheduleNextDaily("13:00", now); !got.Equal(time.Date(2026, 11, 20, 13, 0, 0, 0, time.Local)) {
		t.Errorf("scheduleNextDaily before 13:00 = %v", got)
	}
}

func TestLate(t *testing.T) {
	w := 30 * time.Minute
	if scheduleLate(now, now.Add(29*time.Minute), w) || scheduleLate(now, now.Add(30*time.Minute), w) {
		t.Error("29 and 30 minutes are not late")
	}
	if !scheduleLate(now, now.Add(31*time.Minute), w) {
		t.Error("31 minutes is late")
	}
}

func TestParseTarget(t *testing.T) {
	for _, c := range []struct {
		all        bool
		repo, task string
		want       string
		bad        bool
	}{
		{true, "", "", "all", false},
		{false, "api-server", "", "repo:api-server", false},
		{false, "", "T-42", "task:T-42", false},
		{false, "", "", "", true},
		{true, "x", "", "", true},
		{false, "a b", "", "", true},
		{false, "", "T-0", "", true},
	} {
		got, err := scheduleParseTarget(c.all, c.repo, c.task)
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("scheduleParseTarget(%v,%q,%q) = %q, %v", c.all, c.repo, c.task, got, err)
		}
	}
}

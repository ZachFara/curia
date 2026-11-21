package main

import (
	"fmt"
	"strings"
	"testing"
)

// At most 50 schedules are active or paused; a canceled one frees its place.
func TestScheduleCapIs50ActiveOrPaused(t *testing.T) {
	db := schedHome(t)
	for i := range 50 {
		if out, err := schedRun(t, "\n", fmt.Sprintf("message %d", i), "--at", "+2h", "--all"); err != nil {
			t.Fatalf("schedule %d: %v\n%s", i+1, err, out)
		}
	}
	if _, err := schedRun(t, "\n", "one too many", "--at", "+2h", "--all"); err == nil || !strings.Contains(err.Error(), "50 already") {
		t.Fatalf("the 51st schedule: %v, want a refusal naming 50", err)
	}
	if got := len(schedStored(t, db)); got != 50 {
		t.Fatalf("%d schedules stored, want 50", got)
	}
	if out, err := schedRun(t, "", "cancel", "S-1"); err != nil {
		t.Fatalf("cancel: %v\n%s", err, out)
	}
	if out, err := schedRun(t, "\n", "after a cancel", "--at", "+2h", "--all"); err != nil {
		t.Errorf("a schedule after a cancel: %v\n%s", err, out)
	}
}

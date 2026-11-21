package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// schedRun runs `curia schedule …` with stdin and returns stdout and stderr.
func schedRun(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	scheduleResetFlags()
	t.Cleanup(scheduleResetFlags)
	var out bytes.Buffer
	rootCmd.SetIn(strings.NewReader(stdin))
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(append([]string{"schedule"}, args...))
	t.Cleanup(func() { rootCmd.SetIn(nil); rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil) })
	err := rootCmd.Execute()
	return out.String(), err
}

func schedHome(t *testing.T) string {
	t.Helper()
	db, _ := readHome(t, []string{"%1", "%2"}, readSession("a", model.StateIdle, "%1"), readSession("b", model.StateIdle, "%2"))
	prev := sayIsTerminal
	sayIsTerminal = func() bool { return true }
	t.Cleanup(func() { sayIsTerminal = prev })
	return db
}

func schedStored(t *testing.T, db string) []store.Schedule {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	out, err := st.ListSchedules(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScheduleCreateConfirmsWithRecipientsAndStores(t *testing.T) {
	db := schedHome(t)
	out, err := schedRun(t, "\n", "continue please", "--at", "+2h", "--all", "--if-idle")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"continue please", "2 sessions", "a", "b", "resolved again when it fires", "S-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	got := schedStored(t, db)
	if len(got) != 1 || got[0].Text != "continue please" || got[0].Kind != "at" || got[0].Target != "all" || !got[0].IfIdle ||
		got[0].Status != "active" || !got[0].NextRun.Equal(at(12, 0).Add(2*time.Hour)) || got[0].CreatedVia != "cli" {
		t.Errorf("stored = %+v", got)
	}
}

func TestScheduleCreateNeedsEnterAtATerminal(t *testing.T) {
	db := schedHome(t)
	for _, in := range []string{"n\n", "", "yes\n"} { // anything but Enter cancels; EOF is no Enter
		if out, err := schedRun(t, in, "go", "--daily", "02:00", "--repo", "web"); err != nil || !strings.Contains(out, "nothing scheduled") {
			t.Errorf("stdin %q: %v %q, want nothing scheduled", in, err, out)
		}
	}
	if got := schedStored(t, db); len(got) != 0 {
		t.Errorf("stored %+v without Enter", got)
	}
	sayIsTerminal = func() bool { return false }
	if _, err := schedRun(t, "\n", "go", "--daily", "02:00", "--all"); err == nil || !strings.Contains(err.Error(), "confirmation") {
		t.Errorf("no terminal: %v, want the confirmation refusal", err)
	}
}

func TestScheduleCreateRefusesBadInput(t *testing.T) {
	db := schedHome(t)
	for name, args := range map[string][]string{
		"no trigger":         {"go", "--all"},
		"two triggers":       {"go", "--all", "--at", "+1h", "--daily", "02:00"},
		"no target":          {"go", "--at", "+1h"},
		"two targets":        {"go", "--at", "+1h", "--all", "--repo", "x"},
		"past":               {"go", "--at", "2020-01-01T00:00", "--all"},
		"bad daily":          {"go", "--daily", "25:00", "--all"},
		"on not reset":       {"go", "--on", "dawn", "--all"},
		"on reset no window": {"go", "--on", "reset", "--all"},
		"only unknown":       {"go", "--at", "+1h", "--all", "--only", "busy"},
		"empty text":         {"  ", "--at", "+1h", "--all"},
		"1 MB text":          {strings.Repeat("x", 1<<20), "--at", "+1h", "--all"},
	} {
		if out, err := schedRun(t, "\n", args...); err == nil {
			t.Errorf("%s: accepted: %s", name, out)
		}
	}
	if got := schedStored(t, db); len(got) != 0 {
		t.Errorf("stored %+v", got)
	}
}

func TestScheduleTextIsCleaned(t *testing.T) {
	db := schedHome(t)
	hostile := "\x1b[31mgo\x1b[0m \u202eevil\u202c\x1b]0;title\x07 now"
	out, err := schedRun(t, "\n", hostile, "--at", "+1h", "--all")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := schedStored(t, db)
	if len(got) != 1 || strings.ContainsAny(got[0].Text, "\x1b\x07\u202e\u202c") || !strings.Contains(got[0].Text, "go") {
		t.Errorf("stored text %q, want it cleaned", got[0].Text)
	}
	if strings.ContainsAny(out, "\x1b\x07\u202e") {
		t.Errorf("output holds control or bidi characters: %q", out)
	}
}

func TestScheduleOnResetUsesTheUsageWindow(t *testing.T) {
	db := schedHome(t)
	st, _ := store.Open(db)
	reset := at(12, 0).Add(3 * time.Hour)
	if _, err := st.Ingest(context.Background(), store.Reading{
		SessionID: "a", At: at(11, 0),
		Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: 100, ResetsAt: reset}},
	}, at(12, 0)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if out, err := schedRun(t, "\n", "continue", "--on", "reset", "--all", "--only", "limited"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got := schedStored(t, db)
	if len(got) != 1 || got[0].Kind != "on_reset" || !got[0].OnlyLimited || !got[0].NextRun.Equal(reset.Add(time.Minute)) {
		t.Errorf("stored = %+v, want on_reset one minute after %v", got, reset)
	}
}

func TestScheduleListShowCancelPauseResume(t *testing.T) {
	db := schedHome(t)
	for _, a := range [][]string{{"first", "--at", "+1h", "--all"}, {"second", "--daily", "02:00", "--task", "T-9"}} {
		if _, err := schedRun(t, "\n", a...); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := schedRun(t, "", "pause", "S-2"); err != nil || !strings.Contains(out, "paused") {
		t.Fatalf("pause: %v %q", err, out)
	}
	if out, err := schedRun(t, "", "pause", "S-2"); err == nil {
		t.Errorf("pausing a paused schedule: %q", out)
	}
	if out, err := schedRun(t, "", "resume", "S-2"); err != nil || !strings.Contains(out, "resumed") {
		t.Fatalf("resume: %v %q", err, out)
	}
	if out, err := schedRun(t, "", "cancel", "S-1"); err != nil || !strings.Contains(out, "canceled") {
		t.Fatalf("cancel: %v %q", err, out)
	}
	got := schedStored(t, db)
	if got[0].ID != "S-2" || got[0].Status != "active" || !got[0].NextRun.After(at(12, 0)) || got[1].Status != "canceled" {
		t.Errorf("stored = %+v", got)
	}
}

func TestScheduleShowAndBadIDs(t *testing.T) {
	schedHome(t)
	if _, err := schedRun(t, "\n", "first", "--at", "+1h", "--all"); err != nil {
		t.Fatal(err)
	}
	if _, err := schedRun(t, "", "cancel", "S-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := schedRun(t, "", "cancel", "S-99"); err == nil {
		t.Error("canceling a schedule that does not exist")
	}
	if _, err := schedRun(t, "", "cancel", "../etc"); err == nil {
		t.Error("canceling a malformed id")
	}
	out, err := schedRun(t, "", "show", "S-1")
	if err != nil || !strings.Contains(out, "canceled") || !strings.Contains(out, "first") {
		t.Errorf("show: %v %q", err, out)
	}
}

func TestScheduleListJSONGolden(t *testing.T) {
	db := schedHome(t)
	st, _ := store.Open(db)
	for _, sc := range []store.Schedule{
		{Text: "continue", Kind: "at", Spec: "2026-11-12T02:00:00Z", Target: "all", IfIdle: true, NextRun: time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC), CreatedAt: at(9, 0)},
		{Text: "resume work", Kind: "daily", Spec: "02:00", Target: "repo:api-server", OnlyLimited: true, NextRun: time.Date(2026, 11, 12, 2, 0, 0, 0, time.UTC), CreatedAt: at(10, 0)},
	} {
		if _, err := st.CreateSchedule(context.Background(), sc, 50); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	out, err := schedRun(t, "", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "schedules.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != string(want) {
		t.Errorf("schedule ls --json output:\n%s\nwant (testdata/schedules.golden.json):\n%s", out, want)
	}
}

package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/testkit"
)

// settingsBadge is `curia status` after the config commands ran.
func settingsBadge(t *testing.T) string {
	t.Helper()
	b, err := statusBadge(context.Background(), nil)
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	return b
}

// An edit outside Curia shows C! (before and after a command recorded it) until
// doctor acknowledges it at a terminal; Curia's own writes never do.
func TestStatusShowsExternalConfigEdit(t *testing.T) {
	path := configTestEnv(t)
	configRun(t, "set", "notify_bell", "true")
	if b := settingsBadge(t); strings.Contains(b, "C!") {
		t.Errorf("badge after our own set = %q", b)
	}
	if err := os.WriteFile(path, []byte("notify_bell = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b := settingsBadge(t); !strings.Contains(b, "C!") {
		t.Errorf("badge after a hand edit = %q, want C!", b)
	}
	configRun(t, "history") // records the external revision
	if b := settingsBadge(t); !strings.Contains(b, "C!") {
		t.Errorf("badge after it was recorded = %q, want C!", b)
	}
	configRun(t, "doctor")
	if b := settingsBadge(t); strings.Contains(b, "C!") {
		t.Errorf("badge after doctor = %q", b)
	}
}

func TestStatusConfigMarkerNeverFails(t *testing.T) {
	path := configTestEnv(t)
	configRun(t, "set", "notify_bell", "true")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil { // unreadable as a file
		t.Fatal(err)
	}
	if out, err := runRoot(t, "status"); err != nil || strings.Contains(out, "rror") {
		t.Errorf("status on a bad config path = %q, %v, want no error", out, err)
	}
	_ = os.RemoveAll(path)
	if err := os.WriteFile(path, []byte(strings.Repeat("#", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if b := settingsBadge(t); !strings.Contains(b, "C!") || time.Since(start) > time.Second {
		t.Errorf("a huge file: badge %q", b)
	}
}

// The settings view's rows are `config list`'s lines: one data path.
func TestSettingsRowsMatchConfigList(t *testing.T) {
	configTestEnv(t)
	configRun(t, "set", "notify_bell", "true")
	path, _ := configUserPath()
	c, rep, err := configResolve(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range settingsRows(c, rep) {
		lines = append(lines, r.Key+" = "+r.Value+"  ("+r.Source+")")
	}
	got := strings.Join(lines, "\n") + "\n"
	if out := configRun(t, "list"); !strings.HasPrefix(out, got) {
		t.Errorf("list =\n%s\nrows =\n%s", out, got)
	}
	if !strings.Contains(got, "routing.aliases = (0 entries)") {
		t.Errorf("a table is not shown as a count:\n%s", got)
	}
}

// D-11: the settings view's source refuses a change under an agent marker
// by itself (the second layer under the view's read-only mode): the user
// file is not written and the store is not touched.
func TestSettingsSourceSetRefusesAnAgent(t *testing.T) {
	db := testkit.HookHome(t)
	useFakeTmux(t)
	userTerminalAs(t, true)
	t.Setenv("CLAUDECODE", "1")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	refused := userOnly("ui", "change settings from the dashboard")
	if refused == nil {
		t.Fatal("the guard accepted an agent context")
	}
	_, err = settingsSource{st: st, refused: refused}.Set(context.Background(), "notify_bell", "true")
	if err == nil || !strings.Contains(err.Error(), "CLAUDECODE is set") {
		t.Errorf("Set under an agent marker = %v, want the guard's refusal", err)
	}
	home, _ := os.UserHomeDir()
	if _, serr := os.Stat(configFile(home)); !os.IsNotExist(serr) {
		t.Errorf("the user config file was written for an agent: %v", serr)
	}
}

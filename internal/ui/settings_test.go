package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type settingsFake struct {
	rows []SettingRow
	sets []string
	err  error
}

func (f *settingsFake) Rows(context.Context) ([]SettingRow, error) { return f.rows, nil }
func (f *settingsFake) Set(_ context.Context, k, v string) (string, error) {
	f.sets = append(f.sets, k+"="+v)
	return "set " + k, f.err
}

func settingsFakeRows() []SettingRow {
	return []SettingRow{
		{Key: "editor", Value: "vim", Source: "user"},
		{Key: "notify_bell", Value: "false", Source: "default", Editable: true},
		{Key: "routing.aliases", Value: "(2 entries)", Source: "user"},
		{Key: "routing.auto_threshold", Value: "0.9", Source: "default", Editable: true, Refused: "0.1: would lower the threshold 日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語日本語\x1b[31m"},
	}
}

// settingsDrive feeds keys, 2 seconds apart, running every command it gets back.
type settingsDrive struct {
	t   *testing.T
	m   tea.Model
	now time.Time
}

func settingsStart(t *testing.T, f *settingsFake, readOnly string) *settingsDrive {
	d := &settingsDrive{t: t, now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	m := NewSettings(f, func() time.Time { return d.now }, readOnly)
	d.m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	d.pump(m.Init())
	return d
}

func (d *settingsDrive) pump(cmd tea.Cmd) {
	for cmd != nil {
		msg := cmd()
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				d.pump(c)
			}
			return
		}
		d.m, cmd = d.m.Update(msg)
	}
}

func (d *settingsDrive) key(k string, gap time.Duration) {
	d.now = d.now.Add(gap)
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	if k == "enter" {
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	}
	var cmd tea.Cmd
	d.m, cmd = d.m.Update(msg)
	d.pump(cmd)
}

func TestSettingsViewGroupsFitsAndIsClean(t *testing.T) {
	d := settingsStart(t, &settingsFake{rows: settingsFakeRows()}, "")
	v := d.m.View()
	for _, want := range []string{"[general]", "[routing]", "notify_bell = false  (default)", "routing.aliases = (2 entries)", "repository value refused"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "\x1b") {
		t.Errorf("an escape reached the view:\n%q", v)
	}
	for _, l := range strings.Split(v, "\n") {
		if lipgloss.Width(l) > 80 {
			t.Errorf("line wider than 80: %q", l)
		}
	}
	if strings.Count(v, "\n")+1 > 20 {
		t.Errorf("view taller than the window")
	}
}

func TestSettingsEditSetsAfterConfirmation(t *testing.T) {
	f := &settingsFake{rows: settingsFakeRows()}
	d := settingsStart(t, f, "")
	d.key("e", 2*time.Second) // the cursor starts on the first editable key
	d.key("true", 2*time.Second)
	d.key("enter", 2*time.Second)
	if len(f.sets) != 0 {
		t.Fatalf("set before the confirmation: %v", f.sets)
	}
	d.key("y", 2*time.Second)
	if len(f.sets) != 1 || f.sets[0] != "notify_bell=true" {
		t.Errorf("sets = %v", f.sets)
	}
}

func TestSettingsBurstAndPasteDoNotConfirm(t *testing.T) {
	f := &settingsFake{rows: settingsFakeRows()}
	d := settingsStart(t, f, "")
	d.key("e", 2*time.Second)
	d.key("true", 2*time.Second)
	d.key("enter", 2*time.Second)
	d.key("y", 10*time.Millisecond) // right behind the Enter: a pasted line
	if len(f.sets) != 0 || !strings.Contains(d.m.View(), noticeTooSoon) {
		t.Errorf("a y in a burst set %v:\n%s", f.sets, d.m.View())
	}
	d.now = d.now.Add(2 * time.Second)
	var cmd tea.Cmd
	d.m, cmd = d.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y"), Paste: true})
	d.pump(cmd)
	if len(f.sets) != 0 {
		t.Errorf("a pasted y set %v", f.sets)
	}
}

func TestSettingsReadOnlyAndNotEditable(t *testing.T) {
	f := &settingsFake{rows: settingsFakeRows()}
	d := settingsStart(t, f, "curia ui cannot run inside an agent session (CLAUDECODE is set)")
	d.key("e", 2*time.Second)
	if v := d.m.View(); !strings.Contains(v, "CLAUDECODE is set") {
		t.Errorf("read-only view:\n%s", v)
	}
	if len(f.sets) != 0 {
		t.Errorf("set while read-only: %v", f.sets)
	}
	d = settingsStart(t, f, "")
	d.key("k", 2*time.Second) // up from the first editable key onto editor
	d.key("e", 2*time.Second)
	if v := d.m.View(); !strings.Contains(v, "not changed from here") {
		t.Errorf("a locked key:\n%s", v)
	}
}

func TestSettingsSetErrorIsShown(t *testing.T) {
	f := &settingsFake{rows: settingsFakeRows(), err: errors.New("not written, the old file is kept")}
	d := settingsStart(t, f, "")
	d.key("e", 2*time.Second)
	d.key("zzz", 2*time.Second)
	d.key("enter", 2*time.Second)
	d.key("y", 2*time.Second)
	if v := d.m.View(); !strings.Contains(v, "not changed: not written, the old file is kept") {
		t.Errorf("view:\n%s", v)
	}
}

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestInstallAndUninstallRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")
	conf := filepath.Join(home, ".tmux.conf")
	snippet := filepath.Join(home, ".config", "curia", "curia.tmux")
	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	userSettings := `{"model":"opus"}`
	userConf := "set -g mouse on\n"
	for p, c := range map[string]string{settings: userSettings, conf: userConf} {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runRoot(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{"Restart", "tmux source-file ~/.tmux.conf"} {
		if !strings.Contains(out, want) {
			t.Errorf("install output missing %q:\n%s", want, out)
		}
	}
	got, _ := os.ReadFile(filepath.Clean(settings))
	if !strings.Contains(string(got), " hook SessionStart") {
		t.Errorf("settings missing hooks: %s", got)
	}
	if _, err := os.Stat(snippet); err != nil {
		t.Errorf("snippet not written: %v", err)
	}
	if baks, _ := filepath.Glob(settings + ".bak.*"); len(baks) != 1 {
		t.Errorf("backups = %v", baks)
	}

	if out, err := runRoot(t, "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	gotConf, _ := os.ReadFile(filepath.Clean(conf))
	if string(gotConf) != userConf {
		t.Errorf("conf = %q, want original", gotConf)
	}
	gotSettings, _ := os.ReadFile(filepath.Clean(settings))
	if strings.Contains(string(gotSettings), "hook ") || !strings.Contains(string(gotSettings), "opus") {
		t.Errorf("settings after uninstall: %s", gotSettings)
	}
	if _, err := os.Stat(snippet); !os.IsNotExist(err) {
		t.Errorf("snippet still present: %v", err)
	}
}

func TestInstallRefusesInvalidSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, "install"); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(filepath.Join(home, ".tmux.conf")); !os.IsNotExist(err) {
		t.Errorf("tmux conf touched after settings failure: %v", err)
	}
}

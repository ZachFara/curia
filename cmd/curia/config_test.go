package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configTestEnv is a home with no files, a terminal the guard accepts and a
// confirmation that answers yes.
func configTestEnv(t *testing.T) string {
	t.Helper()
	hookHome(t)
	useFakeTmux(t)
	userTerminalAs(t, true)
	old := setupConfirm
	setupConfirm = func(string) (string, error) { return "yes", nil }
	t.Cleanup(func() { setupConfirm = old })
	home, _ := os.UserHomeDir()
	return configFile(home)
}

func configRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runRoot(t, append([]string{"config"}, args...)...)
	if err != nil {
		t.Fatalf("config %v: %v\n%s", args, err, out)
	}
	return out
}

func TestConfigSetHistoryRollback(t *testing.T) {
	path := configTestEnv(t)
	if out := configRun(t, "get", "routing.auto_threshold"); !strings.Contains(out, "0.9  (default)") {
		t.Errorf("get = %q", out)
	}
	configRun(t, "set", "routing.auto_threshold", "0.95")
	configRun(t, "set", "notify_bell", "true")
	configRun(t, "set", "notify_bell", "false")
	if out := configRun(t, "get", "routing.auto_threshold"); !strings.Contains(out, "0.95  (user)") {
		t.Errorf("get = %q", out)
	}
	hist := configRun(t, "history")
	if strings.Count(hist, "\n") != 3 || !strings.Contains(hist, "cli") {
		t.Errorf("history = %q", hist)
	}
	if out := configRun(t, "rollback", "2"); !strings.Contains(out, "restored revision 2 as revision 4") {
		t.Errorf("rollback = %q", out)
	}
	b, _ := os.ReadFile(filepath.Clean(path))
	if !strings.Contains(string(b), "notify_bell = true") {
		t.Errorf("file after rollback = %q", b)
	}
	if strings.Count(configRun(t, "history"), "\n") != 4 {
		t.Error("rollback deleted or did not append history")
	}
	if _, err := runRoot(t, "config", "rollback", "99"); err == nil || !strings.Contains(err.Error(), "no revision 99") {
		t.Errorf("rollback 99 = %v", err)
	}
}

func TestConfigSetInvalidKeepsFile(t *testing.T) {
	path := configTestEnv(t)
	configRun(t, "set", "notify_bell", "true")
	before, _ := os.ReadFile(filepath.Clean(path))
	for _, kv := range [][]string{{"assume_expiry", "0s"}, {"routing.auto_threshold", "7"}, {"nope", "1"}} {
		if _, err := runRoot(t, "config", "set", kv[0], kv[1]); err == nil {
			t.Errorf("set %v succeeded", kv)
		}
	}
	if after, _ := os.ReadFile(filepath.Clean(path)); string(after) != string(before) {
		t.Errorf("file changed: %q", after)
	}
}

func TestConfigSetRefusedInAgentSession(t *testing.T) {
	path := configTestEnv(t)
	t.Setenv("CLAUDECODE", "1")
	_, err := runRoot(t, "config", "set", "notify_bell", "true")
	if err == nil || !strings.Contains(err.Error(), "only the user may") {
		t.Errorf("set in an agent = %v", err)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Error("the file was written")
	}
}

func TestConfigExternalEditAndDoctor(t *testing.T) {
	path := configTestEnv(t)
	configRun(t, "set", "notify_bell", "true")
	if err := os.WriteFile(path, []byte("notify_bell = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hist := configRun(t, "history")
	if !strings.Contains(hist, "external") {
		t.Errorf("history = %q", hist)
	}
	if out := configRun(t, "doctor"); !strings.Contains(out, "outside Curia") || !strings.Contains(out, "acknowledged") {
		t.Errorf("doctor = %q", out)
	}
	if out := configRun(t, "doctor"); strings.TrimSpace(out) != "ok" {
		t.Errorf("second doctor = %q", out)
	}
}

func TestConfigEditValidatesAndUsesEditorOption(t *testing.T) {
	path := configTestEnv(t)
	t.Setenv("EDITOR", "/bin/false")
	if _, err := runRoot(t, "config", "edit"); err == nil || !strings.Contains(err.Error(), "no editor set") {
		t.Fatalf("edit without the option = %v", err)
	}
	configRun(t, "set", "editor", "myeditor")
	old := taskEditor
	t.Cleanup(func() { taskEditor = old })
	write := func(body string) {
		taskEditor = func(_ context.Context, argv []string, p string) error {
			if argv[0] != "myeditor" {
				t.Errorf("editor argv = %v", argv)
			}
			return os.WriteFile(p, []byte(body), 0o600)
		}
	}
	before, _ := os.ReadFile(filepath.Clean(path))
	write("assume_expiry = \"0s\"\n")
	if _, err := runRoot(t, "config", "edit"); err == nil {
		t.Error("an invalid edit was accepted")
	}
	if after, _ := os.ReadFile(filepath.Clean(path)); string(after) != string(before) {
		t.Error("the old file was not kept")
	}
	write("notify_macos = true\n")
	if out := configRun(t, "edit"); !strings.Contains(out, "saved (revision 2)") {
		t.Errorf("edit = %q", out)
	}
}

func TestConfigGetShowsRefusedRepoValue(t *testing.T) {
	configTestEnv(t)
	repo := t.TempDir()
	for _, d := range []string{".git", ".curia"} {
		if err := os.Mkdir(filepath.Join(repo, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".curia", "config.toml"), []byte("[routing]\nauto_send = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	out := configRun(t, "get", "routing.auto_send")
	if !strings.Contains(out, "false  (default)") || !strings.Contains(out, "repository value refused: routing.auto_send") {
		t.Errorf("get = %q", out)
	}
	if out = configRun(t, "doctor"); !strings.Contains(out, "routing.auto_send") {
		t.Errorf("doctor = %q", out)
	}
}

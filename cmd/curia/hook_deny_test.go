package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/testkit"
)

// A store that cannot be opened still lets the hook deny an edit of
// Curia's own files, and prints nothing for any other call.
func TestHookDenyNeedsNoStore(t *testing.T) {
	testkit.HookHome(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURIA_DB", filepath.Join(blocker, "curia.db")) // a path below a file: cannot be opened
	home, _ := os.UserHomeDir()
	body := func(path string) *strings.Reader {
		return strings.NewReader(`{"session_id":"A","cwd":"/tmp","tool_name":"Edit","tool_input":{"file_path":"` + path + `"}}`)
	}
	got := runHookCmd(t, body(filepath.Join(home, ".config/curia/config.toml")), "hook", "PreToolUse")
	if !strings.Contains(got.output, `"permissionDecision":"deny"`) || got.exitCode != 0 {
		t.Errorf("output %q exit %d, want a deny and exit 0", got.output, got.exitCode)
	}
	got = runHookCmd(t, body("/tmp/other.go"), "hook", "PreToolUse")
	if got.output != "" || got.exitCode != 0 {
		t.Errorf("output %q exit %d, want nothing", got.output, got.exitCode)
	}
}

// One log line per kind of undecided check, however many calls repeat it.
func TestHookUndecidedLoggedOnce(t *testing.T) {
	cfg := config.Default()
	cfg.LogPath = filepath.Join(t.TempDir(), "curia.log")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	for range 20 {
		hookLogUndecided(cfg, logger, []string{"budget"})
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 || !strings.Contains(buf.String(), "budget") {
		t.Errorf("log = %q, want one line naming budget", buf.String())
	}
}

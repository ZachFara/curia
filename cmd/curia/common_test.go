package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

func TestLoadConfigReadsDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CURIA_DB", "")
	p := filepath.Join(home, ".config", "curia", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("default_context_max = 1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultContextMax != 1234 {
		t.Errorf("DefaultContextMax = %d, want 1234 from %s", cfg.DefaultContextMax, p)
	}
	if cfg.DBPath != filepath.Join(home, ".local/share/curia/curia.db") {
		t.Errorf("DBPath = %q", cfg.DBPath)
	}

	t.Setenv("CURIA_DB", filepath.Join(home, "other.db"))
	if cfg, err = loadConfig(); err != nil || cfg.DBPath != filepath.Join(home, "other.db") {
		t.Errorf("with CURIA_DB: DBPath %q, err %v", cfg.DBPath, err)
	}
}

func TestLoadConfigReportsInvalidFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".config", "curia", "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("default_context_max = \"x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(); err == nil {
		t.Error("loadConfig returned nil error for invalid TOML")
	}
}

func TestOpenStoreUsesDBPath(t *testing.T) {
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "nested", "curia.db")
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, _, err := st.GetSession(context.Background(), "x"); err != nil {
		t.Errorf("GetSession on new store: %v", err)
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		t.Errorf("database not at DBPath: %v", err)
	}
}

func TestOpenLogAppendsOneLinePerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "curia", "curia.log")
	for _, msg := range []string{"first", "second\nlevel=INFO msg=forged\r "} {
		log, closeLog, err := openLog(path)
		if err != nil {
			t.Fatal(err)
		}
		log.Error("hook failed", "err", msg)
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "first") || !strings.Contains(lines[1], "second") {
		t.Errorf("log = %q, want two appended lines", lines)
	}
	if strings.ContainsAny(string(b), "\r ") {
		t.Errorf("log holds a raw line break: %q", b)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, %v, want 0600", info.Mode().Perm(), err)
	}
}

// redGoneSession waits unseen in window @3, in a pane that is gone.
var redGoneSession = model.Session{
	ID: "red", Agent: "claude", State: model.StateWaiting, Pane: "%9", TmuxWindow: "@3",
	StartedAt: at(9, 0), UpdatedAt: at(11, 0),
}

// When reconcile marks a red session stale, its window's marker is
// re-applied, so the window does not stay red until it closes.
func TestReconcileReappliesMarkerOfStaleWindows(t *testing.T) {
	_, fake := fleetHome(t, []string{"%1"}, redGoneSession)
	if _, err := runRoot(t, "ls"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ListPanes", "SetWindowOption @3 @curia_marker "}; !slices.Equal(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
	// Nothing new is stale on the next run: no marker call.
	fake.Calls = nil
	if _, err := runRoot(t, "ls"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ListPanes"}; !slices.Equal(fake.Calls, want) {
		t.Errorf("second run tmux calls = %q, want %q", fake.Calls, want)
	}
}

func TestReconcileMarkerFailureIsLoggedNotFatal(t *testing.T) {
	db, _ := fleetHome(t, nil, redGoneSession)
	failing := &failingTmux{}
	failing.Panes = []tmux.Pane{{ID: "%1"}}
	prev := newTmuxClient
	newTmuxClient = func() tmux.Client { return failing }
	t.Cleanup(func() { newTmuxClient = prev })
	out, err := runRoot(t, "ls", "--json")
	if err != nil || !strings.Contains(out, `"state": "stale"`) {
		t.Errorf("ls --json = %s, %v, want the session listed as stale", out, err)
	}
	if log := readLog(t); len(log) != 1 || !strings.Contains(log[0], "no server") || !strings.Contains(log[0], "@3") {
		t.Errorf("log = %q, want one line with the failed marker update", log)
	}
	if s := getSession(t, db, "red"); s.State != model.StateStale {
		t.Errorf("red = %s, want stale", s.State)
	}
}

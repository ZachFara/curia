package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/config"
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

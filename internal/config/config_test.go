package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	c := config.Default()
	if !strings.HasSuffix(c.DBPath, ".local/share/curia/curia.db") {
		t.Errorf("DBPath = %q", c.DBPath)
	}
	if !strings.HasSuffix(c.LogPath, ".local/state/curia/curia.log") {
		t.Errorf("LogPath = %q", c.LogPath)
	}
	if c.DefaultContextMax != 200000 {
		t.Errorf("DefaultContextMax = %d", c.DefaultContextMax)
	}
	if c.ContextWarnPct != 60 || c.ContextCritPct != 85 {
		t.Errorf("pct = %d/%d", c.ContextWarnPct, c.ContextCritPct)
	}
	if c.StaleHideAfter != 10*time.Minute {
		t.Errorf("StaleHideAfter = %v", c.StaleHideAfter)
	}
	if c.AssumeExpiry != 24*time.Hour || c.NotifyBell || c.NotifyMacOS {
		t.Errorf("decision defaults = %v/%v/%v", c.AssumeExpiry, c.NotifyBell, c.NotifyMacOS)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	got, err := config.Load("/nonexistent/curia/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, config.Default()) {
		t.Errorf("got %+v, want defaults", got)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	p := writeConfig(t, "context_warn_pct = 50\nstale_hide_after = \"3m\"\n\n[context_max]\n\"claude-opus-5-5\" = 1000000\n")
	got, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	def := config.Default()
	if got.ContextWarnPct != 50 {
		t.Errorf("ContextWarnPct = %d", got.ContextWarnPct)
	}
	if got.ContextMax["claude-opus-5-5"] != 1000000 {
		t.Errorf("ContextMax = %v", got.ContextMax)
	}
	if got.StaleHideAfter != 3*time.Minute {
		t.Errorf("StaleHideAfter = %v", got.StaleHideAfter)
	}
	if got.ContextCritPct != def.ContextCritPct || got.DefaultContextMax != def.DefaultContextMax ||
		got.DBPath != def.DBPath || got.LogPath != def.LogPath {
		t.Errorf("other fields not default: %+v", got)
	}
}

func TestLoadExpandsTilde(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	p := writeConfig(t, "db_path = \"~/x/curia.db\"\nlog_path = \"~/x/curia.log\"\n")
	got, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.DBPath != filepath.Join(home, "x/curia.db") || got.LogPath != filepath.Join(home, "x/curia.log") {
		t.Errorf("paths = %q, %q", got.DBPath, got.LogPath)
	}
}

func TestLoadInvalidTOMLReturnsError(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	if _, err := config.Load(writeConfig(t, "context_warn_pct = [")); err == nil {
		t.Fatal("expected error")
	}
}

func TestCuriaDBEnvOverride(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "x.db")
	t.Setenv("CURIA_DB", want)

	got, err := config.Load("/nonexistent/curia/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if got.DBPath != want {
		t.Errorf("DBPath = %q, want %q", got.DBPath, want)
	}

	// The env var wins over the file.
	got, err = config.Load(writeConfig(t, "db_path = \"/from/file.db\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.DBPath != want {
		t.Errorf("with file: DBPath = %q, want %q", got.DBPath, want)
	}
}

func TestDecisionDefaults(t *testing.T) {
	c := config.Default()
	if c.AssumeExpiry != 24*time.Hour {
		t.Errorf("AssumeExpiry = %v", c.AssumeExpiry)
	}
	if c.NotifyBell || c.NotifyMacOS {
		t.Errorf("NotifyBell/NotifyMacOS = %v/%v", c.NotifyBell, c.NotifyMacOS)
	}
}

func TestLoadDecisionOverrides(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	got, err := config.Load(writeConfig(t, "assume_expiry = \"36h\"\nnotify_bell = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.AssumeExpiry != 36*time.Hour || !got.NotifyBell || got.NotifyMacOS {
		t.Errorf("got %v/%v/%v", got.AssumeExpiry, got.NotifyBell, got.NotifyMacOS)
	}
}

func TestNonPositiveAssumeExpiryRejected(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	for _, v := range []string{"0s", "-1h"} {
		_, err := config.Load(writeConfig(t, "assume_expiry = \""+v+"\"\n"))
		if err == nil || !strings.Contains(err.Error(), "assume_expiry") {
			t.Errorf("assume_expiry %s: err = %v", v, err)
		}
	}
}

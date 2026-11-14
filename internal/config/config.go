// Package config loads Curia's TOML configuration.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is Curia's runtime configuration.
type Config struct {
	DBPath            string         `toml:"db_path"`
	LogPath           string         `toml:"log_path"`
	ContextMax        map[string]int `toml:"context_max"`
	DefaultContextMax int            `toml:"default_context_max"`
	ContextWarnPct    int            `toml:"context_warn_pct"`
	ContextCritPct    int            `toml:"context_crit_pct"`
	StaleHideAfter    time.Duration  `toml:"stale_hide_after"`
	AssumeExpiry      time.Duration  `toml:"assume_expiry"`
	NotifyBell        bool           `toml:"notify_bell"`
	NotifyMacOS       bool           `toml:"notify_macos"`

	// Plan 3 additions.
	Routing Routing `toml:"routing"`
	// The tmux prefix-table keys curia install's snippet binds for the
	// dashboard popup, the decide popup, curia next and the message popup; ""
	// binds none.
	KeyDashboard string `toml:"key_dashboard"`
	KeyDecisions string `toml:"key_decisions"`
	KeyNext      string `toml:"key_next"`
	KeyMessage   string `toml:"key_message"`
}

// Default returns the configuration used when no file overrides it.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DBPath:            filepath.Join(home, ".local/share/curia/curia.db"),
		LogPath:           filepath.Join(home, ".local/state/curia/curia.log"),
		ContextMax:        map[string]int{},
		DefaultContextMax: 200000,
		ContextWarnPct:    60,
		ContextCritPct:    85,
		StaleHideAfter:    10 * time.Minute,
		AssumeExpiry:      24 * time.Hour,

		// Plan 3 additions.
		Routing:      defaultRouting(),
		KeyDashboard: "a",
		KeyDecisions: "A",
		KeyNext:      "N",
		KeyMessage:   "m",
	}
}

// Load reads the TOML file at path over Default(). A missing file yields the
// defaults. A leading "~" in the paths is expanded, and the CURIA_DB
// environment variable, when set, overrides DBPath last.
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(filepath.Clean(path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, fmt.Errorf("read config: %w", err)
	default:
		if err := toml.Unmarshal(data, &c); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if c.AssumeExpiry <= 0 {
		return Config{}, fmt.Errorf("config %s: assume_expiry must be positive, got %v", path, c.AssumeExpiry)
	}
	if err := c.Routing.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	c.DBPath = expandHome(c.DBPath)
	c.LogPath = expandHome(c.LogPath)
	if env := os.Getenv("CURIA_DB"); env != "" {
		c.DBPath = env
	}
	return c, nil
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

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
	"unicode"

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
	// Editor is the command `curia task edit` opens on a temp file, the
	// file's path appended as the last argument. It is split on single
	// spaces into an argument vector and run without a shell; there are no
	// quoting rules, so a path with spaces is not supported. The first word
	// is an absolute path or a bare program name looked up on PATH. Empty
	// (the default): no editor.
	Editor string `toml:"editor"`
}

// maxEditor bounds the editor option, in bytes.
const maxEditor = 1024

// EditorArgs is Editor as an argument vector, or nil when it is empty.
func (c Config) EditorArgs() []string {
	if c.Editor == "" {
		return nil
	}
	return strings.Split(c.Editor, " ")
}

// validateEditor checks the editor option: at most maxEditor bytes, no
// control or other unprintable character, every word between single spaces
// non-empty, and a first word that is an absolute path or a bare name.
func validateEditor(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxEditor {
		return fmt.Errorf("editor must be at most %d bytes, got %d", maxEditor, len(v))
	}
	for _, r := range v {
		if r != ' ' && !unicode.IsPrint(r) {
			return fmt.Errorf("editor must not hold control or unprintable characters, found %U", r)
		}
	}
	words := strings.Split(v, " ")
	for _, w := range words {
		if w == "" {
			return errors.New("editor must be words separated by single spaces, with none at either end (a path with spaces is not supported)")
		}
	}
	if p := words[0]; strings.Contains(p, "/") && !filepath.IsAbs(p) {
		return fmt.Errorf("editor's program %q must be an absolute path or a bare name looked up on PATH", p)
	}
	return nil
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
	if err := validateEditor(c.Editor); err != nil {
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

package config

import (
	"fmt"
	"slices"
	"time"
)

// Mode configures F20. Default is the mode a session has until the user sets
// one: only normal or edits (away and bypass are never a default, and the
// repository file cannot change any of these keys).
type Mode struct {
	Default string `toml:"default"`
	// AwayAllow are command prefixes (words) a Bash command may start with in
	// away mode: the rest must be plain, option-free arguments (spec F20, but
	// not its globs: a glob let "git diff --output=..." through). Commands that
	// run repository hooks (git commit) are the user's to add.
	AwayAllow []string `toml:"away_allow"`
	// BypassFor is the expiry bypass gets when --for is not given.
	BypassFor time.Duration `toml:"bypass_for"`
}

func defaultMode() Mode {
	return Mode{
		Default:   "normal",
		AwayAllow: []string{"git status", "git diff", "git log", "git add"},
		BypassFor: 2 * time.Hour,
	}
}

func (m Mode) validate() error {
	if !slices.Contains([]string{"normal", "edits"}, m.Default) {
		return fmt.Errorf("mode.default must be normal or edits, got %q", m.Default)
	}
	if m.BypassFor <= 0 || m.BypassFor > 24*time.Hour {
		return fmt.Errorf("mode.bypass_for must be between 1ns and 24h, got %v", m.BypassFor)
	}
	return nil
}

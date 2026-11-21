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
	// AwayAllow are globs a Bash command may match in away mode (spec F20).
	AwayAllow []string `toml:"away_allow"`
	// BypassFor is the expiry bypass gets when --for is not given.
	BypassFor time.Duration `toml:"bypass_for"`
}

func defaultMode() Mode {
	return Mode{
		Default:   "normal",
		AwayAllow: []string{"git status*", "git diff*", "git log*", "git add *", "git commit *"},
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

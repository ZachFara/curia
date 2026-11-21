package config

import (
	"fmt"
	"time"
)

// Schedule configures F22 scheduled messages (spec section 7).
type Schedule struct {
	// LateWindow is how late a trigger may fire after a monitor outage or sleep (default 30m).
	LateWindow time.Duration `toml:"late_window"`
	// ResetGrace is how long after a window's resets_at `--on reset` fires (default 1m).
	ResetGrace time.Duration `toml:"reset_grace"`
	// LimitedPct is the usage percent at which `--only limited` counts a window as stopped (default 100).
	LimitedPct float64 `toml:"limited_pct"`
}

func defaultSchedule() Schedule {
	return Schedule{LateWindow: 30 * time.Minute, ResetGrace: time.Minute, LimitedPct: 100}
}

func (s Schedule) validate() error {
	switch {
	case s.LateWindow < 0 || s.LateWindow > 24*time.Hour:
		return fmt.Errorf("schedule.late_window must be between 0 and 24h, got %v", s.LateWindow)
	case s.ResetGrace < 0 || s.ResetGrace > time.Hour:
		return fmt.Errorf("schedule.reset_grace must be between 0 and 1h, got %v", s.ResetGrace)
	case s.LimitedPct <= 0 || s.LimitedPct > 100:
		return fmt.Errorf("schedule.limited_pct must be above 0 and at most 100, got %v", s.LimitedPct)
	}
	return nil
}

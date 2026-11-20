package config

import (
	"fmt"
	"time"
)

// Resources configures F19 sampling. The container caps and the Docker
// memory threshold are added with the Docker part.
type Resources struct {
	// Enabled turns the monitor's per-session sampling on (default true).
	Enabled bool `toml:"enabled"`
	// Interval is the monitor's tick (default 10s, 1s to 1h).
	Interval time.Duration `toml:"interval"`
}

// Monitor configures `curia monitor` (spec section 4).
type Monitor struct {
	// Autostart lets the tmux snippet's client-attached line start the
	// monitor (default true); false makes that line do nothing.
	Autostart bool `toml:"autostart"`
}

func defaultResources() Resources { return Resources{Enabled: true, Interval: 10 * time.Second} }

func (r Resources) validate() error {
	if r.Interval < time.Second || r.Interval > time.Hour {
		return fmt.Errorf("resources.interval must be between 1s and 1h, got %v", r.Interval)
	}
	return nil
}

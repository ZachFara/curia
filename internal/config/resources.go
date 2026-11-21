package config

import (
	"fmt"
	"math"
	"time"
)

// Resources configures F19 sampling. The container caps and limits are part 2 (Docker).
type Resources struct {
	// Enabled turns the monitor's per-session sampling on (default true).
	Enabled bool `toml:"enabled"`
	// Interval is the monitor's tick (default 10s, 1s to 1h).
	Interval time.Duration `toml:"interval"`
	// MaxContainersPerSession and MaxContainersTotal cap the containers the
	// docker wrapper lets run and create start (defaults 3 and 8); a
	// repository file may only lower them.
	MaxContainersPerSession int `toml:"max_containers_per_session"`
	MaxContainersTotal      int `toml:"max_containers_total"`
	// ContainerMemoryMB and ContainerCPUs are the limits the wrapper adds to
	// run and create unless the command sets a stricter one (0 = none; a
	// repository file may only lower a nonzero value).
	ContainerMemoryMB int     `toml:"container_memory_mb"`
	ContainerCPUs     float64 `toml:"container_cpus"`
}

// Monitor configures `curia monitor` (spec section 4).
type Monitor struct {
	// Autostart lets the tmux snippet's client-attached line start the
	// monitor (default true); false makes that line do nothing.
	Autostart bool `toml:"autostart"`
}

func defaultResources() Resources {
	return Resources{Enabled: true, Interval: 10 * time.Second, MaxContainersPerSession: 3, MaxContainersTotal: 8}
}

func (r Resources) validate() error {
	if r.Interval < time.Second || r.Interval > time.Hour {
		return fmt.Errorf("resources.interval must be between 1s and 1h, got %v", r.Interval)
	}
	if r.MaxContainersPerSession < 1 || r.MaxContainersPerSession > 1000 || r.MaxContainersTotal < 1 || r.MaxContainersTotal > 1000 {
		return fmt.Errorf("resources.max_containers_per_session and max_containers_total must be between 1 and 1000, got %d and %d", r.MaxContainersPerSession, r.MaxContainersTotal)
	}
	if r.ContainerMemoryMB < 0 || r.ContainerMemoryMB > 1<<22 || math.IsNaN(r.ContainerCPUs) || r.ContainerCPUs < 0 || r.ContainerCPUs > 1024 {
		return fmt.Errorf("resources.container_memory_mb or container_cpus out of range, got %d and %v", r.ContainerMemoryMB, r.ContainerCPUs)
	}
	return nil
}

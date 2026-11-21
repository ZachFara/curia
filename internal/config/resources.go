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
	// docker wrapper lets run, create and compose up start (defaults 3 and
	// 8); 0 switches that cap off. DockerMemoryRefusePct refuses them while
	// Docker's memory in use is above that percent of its limit (default 80;
	// 0 = off). A repository file may only lower a nonzero value.
	MaxContainersPerSession int `toml:"max_containers_per_session"`
	MaxContainersTotal      int `toml:"max_containers_total"`
	DockerMemoryRefusePct   int `toml:"docker_memory_refuse_pct"`
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
	return Resources{Enabled: true, Interval: 10 * time.Second, MaxContainersPerSession: 3, MaxContainersTotal: 8, DockerMemoryRefusePct: 80}
}

func (r Resources) validate() error {
	if r.Interval < time.Second || r.Interval > time.Hour {
		return fmt.Errorf("resources.interval must be between 1s and 1h, got %v", r.Interval)
	}
	if bad := func(n int) bool { return n < 0 || n > 1000 }; bad(r.MaxContainersPerSession) || bad(r.MaxContainersTotal) || r.DockerMemoryRefusePct < 0 || r.DockerMemoryRefusePct > 100 {
		return fmt.Errorf("resources.max_containers_* must be 0 (off) to 1000 and docker_memory_refuse_pct 0 (off) to 100, got %d, %d and %d",
			r.MaxContainersPerSession, r.MaxContainersTotal, r.DockerMemoryRefusePct)
	}
	if r.ContainerMemoryMB < 0 || r.ContainerMemoryMB > 1<<22 || math.IsNaN(r.ContainerCPUs) || r.ContainerCPUs < 0 || r.ContainerCPUs > 1024 {
		return fmt.Errorf("resources.container_memory_mb or container_cpus out of range, got %d and %v", r.ContainerMemoryMB, r.ContainerCPUs)
	}
	return nil
}

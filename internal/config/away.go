package config

import (
	"fmt"
	"time"
)

// Away configures F20's `curia away`: PrepareWait is how long it waits for
// every agent to end its turn after the leaving message before it goes on.
type Away struct {
	PrepareWait time.Duration `toml:"prepare_wait"`
}

func defaultAway() Away { return Away{PrepareWait: 10 * time.Minute} }

func (a Away) validate() error {
	if a.PrepareWait < 0 || a.PrepareWait > time.Hour {
		return fmt.Errorf("away.prepare_wait must be between 0 and 1h, got %v", a.PrepareWait)
	}
	return nil
}

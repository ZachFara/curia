package config

import (
	"fmt"
	"time"
)

// Tasks configures tasks (F5 amendment, Ruling 141).
type Tasks struct {
	AgentFiled AgentFiled `toml:"agent_filed"`
	AutoSpawn  AutoSpawn  `toml:"auto_spawn"`
}

// AutoSpawn is F29's setting. A repository file may only turn it off, lower
// the caps and lengthen the interval (Ruling 159); it can never enable it.
type AutoSpawn struct {
	Enabled   bool          `toml:"enabled"`
	MaxAgents int           `toml:"max_agents"`
	MaxPerDay int           `toml:"max_per_day"`
	Interval  time.Duration `toml:"interval"`
}

// AgentFiled is file_task's setting: Level is off, backlog or ready; a
// repository file may only lower it (PolicyLevel). MaxPerDay is per filing
// session per rolling 24 hours.
type AgentFiled struct {
	Level     string `toml:"level"`
	MaxPerDay int    `toml:"max_per_day"`
}

func defaultTasks() Tasks {
	return Tasks{
		AgentFiled: AgentFiled{Level: "backlog", MaxPerDay: 5},
		AutoSpawn:  AutoSpawn{MaxAgents: 3, MaxPerDay: 5, Interval: 5 * time.Minute},
	}
}

// levelRank orders the levels; a higher rank gives an agent more power.
var levelRank = map[string]int{"off": 0, "backlog": 1, "ready": 2}

func (t Tasks) validate() error {
	if _, ok := levelRank[t.AgentFiled.Level]; !ok {
		return fmt.Errorf("tasks.agent_filed.level must be off, backlog or ready, got %q", t.AgentFiled.Level)
	}
	if t.AgentFiled.MaxPerDay < 1 {
		return fmt.Errorf("tasks.agent_filed.max_per_day must be at least 1 (use level = \"off\" to stop filing), got %d", t.AgentFiled.MaxPerDay)
	}
	a := t.AutoSpawn
	if a.MaxAgents < 1 || a.MaxPerDay < 1 {
		return fmt.Errorf("tasks.auto_spawn.max_agents and max_per_day must be at least 1 (use enabled = false to stop), got %d and %d", a.MaxAgents, a.MaxPerDay)
	}
	if a.Interval < 30*time.Second || a.Interval > 24*time.Hour {
		return fmt.Errorf("tasks.auto_spawn.interval must be between 30s and 24h, got %v", a.Interval)
	}
	return nil
}

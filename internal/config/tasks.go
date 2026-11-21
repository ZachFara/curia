package config

import "fmt"

// Tasks configures tasks (F5 amendment, Ruling 141).
type Tasks struct {
	AgentFiled AgentFiled `toml:"agent_filed"`
}

// AgentFiled is file_task's setting: Level is off, backlog or ready; a
// repository file may only lower it (PolicyLevel). MaxPerDay is per filing
// session per rolling 24 hours.
type AgentFiled struct {
	Level     string `toml:"level"`
	MaxPerDay int    `toml:"max_per_day"`
}

func defaultTasks() Tasks { return Tasks{AgentFiled{Level: "backlog", MaxPerDay: 5}} }

// levelRank orders the levels; a higher rank gives an agent more power.
var levelRank = map[string]int{"off": 0, "backlog": 1, "ready": 2}

func (t Tasks) validate() error {
	if _, ok := levelRank[t.AgentFiled.Level]; !ok {
		return fmt.Errorf("tasks.agent_filed.level must be off, backlog or ready, got %q", t.AgentFiled.Level)
	}
	if t.AgentFiled.MaxPerDay < 1 {
		return fmt.Errorf("tasks.agent_filed.max_per_day must be at least 1 (use level = \"off\" to stop filing), got %d", t.AgentFiled.MaxPerDay)
	}
	return nil
}

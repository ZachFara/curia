package config

import "time"

// Activity configures F27 activity time. Rules are plain strings here; the
// usage package compiles and bounds them (a bad rule fails `curia time`
// with its position, never a hook).
type Activity struct {
	// IdleCap bounds how much of a turn-end-to-prompt gap counts as waiting
	// on me (default 30 minutes).
	IdleCap time.Duration `toml:"idle_cap"`
	// TaskTypes are the branch prefixes that name a task type; any other
	// branch is "untyped".
	TaskTypes []string `toml:"task_types"`
	// Rules go before the built-in ones, first match wins.
	Rules []ActivityRule `toml:"rules"`
}

// ActivityRule is one [[activity.rules]] entry: Class, and regular
// expressions over the tool name (Tool) and the first 200 characters of a
// Bash command (Match). Match alone means a Bash command.
type ActivityRule struct {
	Class string `toml:"class"`
	Tool  string `toml:"tool"`
	Match string `toml:"match"`
}

func defaultActivity() Activity {
	return Activity{
		IdleCap:   30 * time.Minute,
		TaskTypes: []string{"feat", "fix", "docs", "chore", "refactor", "test", "perf"},
	}
}

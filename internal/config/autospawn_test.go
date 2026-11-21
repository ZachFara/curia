package config

import (
	"testing"
	"time"
)

func TestAutoSpawnDefaultsAndPolicy(t *testing.T) {
	d := Default().Tasks.AutoSpawn
	if d.Enabled || d.MaxAgents != 3 || d.MaxPerDay != 5 || d.Interval != 5*time.Minute {
		t.Fatalf("defaults = %+v, want off, 3, 5, 5m", d)
	}
	on := "[tasks.auto_spawn]\nenabled = true\nmax_agents = 3\nmax_per_day = 5\ninterval = \"5m\"\n"
	for _, c := range []struct {
		name, user, repo string
		want             AutoSpawn
		refusedKey       string
	}{
		{"repo cannot enable", "", "[tasks.auto_spawn]\nenabled = true\n", d, "tasks.auto_spawn.enabled"},
		{"repo cannot raise max_agents", on, "[tasks.auto_spawn]\nmax_agents = 9\n", AutoSpawn{true, 3, 5, 5 * time.Minute}, "tasks.auto_spawn.max_agents"},
		{"repo cannot raise max_per_day", on, "[tasks.auto_spawn]\nmax_per_day = 9\n", AutoSpawn{true, 3, 5, 5 * time.Minute}, "tasks.auto_spawn.max_per_day"},
		{"repo cannot shorten interval", on, "[tasks.auto_spawn]\ninterval = \"1m\"\n", AutoSpawn{true, 3, 5, 5 * time.Minute}, "tasks.auto_spawn.interval"},
		{"repo may disable", on, "[tasks.auto_spawn]\nenabled = false\n", AutoSpawn{false, 3, 5, 5 * time.Minute}, ""},
		{"repo may lower a cap", on, "[tasks.auto_spawn]\nmax_agents = 1\n", AutoSpawn{true, 1, 5, 5 * time.Minute}, ""},
		{"repo may lengthen the interval", on, "[tasks.auto_spawn]\ninterval = \"20m\"\n", AutoSpawn{true, 3, 5, 20 * time.Minute}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, rep, err := Resolve(layerRepo(t, c.user, c.repo))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Tasks.AutoSpawn != c.want {
				t.Errorf("effective = %+v, want %+v", cfg.Tasks.AutoSpawn, c.want)
			}
			if c.refusedKey != "" && !refused(rep, c.refusedKey) {
				t.Errorf("%s not refused: %+v", c.refusedKey, rep.Refused)
			}
		})
	}
}

func TestAutoSpawnRejectsBadValues(t *testing.T) {
	for _, user := range []string{"[tasks.auto_spawn]\nmax_agents = 0\n", "[tasks.auto_spawn]\nmax_per_day = 0\n", "[tasks.auto_spawn]\ninterval = \"1s\"\n"} {
		if _, _, err := Resolve(layerRepo(t, user, "")); err == nil {
			t.Errorf("%q was accepted", user)
		}
	}
}

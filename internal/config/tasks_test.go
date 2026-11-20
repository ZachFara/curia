package config

import "testing"

func TestAgentFiledPolicy(t *testing.T) {
	d := Default()
	if d.Tasks.AgentFiled.Level != "backlog" || d.Tasks.AgentFiled.MaxPerDay != 5 {
		t.Fatalf("defaults = %+v, want backlog and 5", d.Tasks.AgentFiled)
	}
	for _, c := range []struct {
		name, user, repo, wantLevel string
		wantMax                     int
		refusedKey                  string
	}{
		{"repo raises backlog to ready", "", "[tasks.agent_filed]\nlevel = \"ready\"\n", "backlog", 5, "tasks.agent_filed.level"},
		{"repo lowers to off", "", "[tasks.agent_filed]\nlevel = \"off\"\n", "off", 5, ""},
		{"repo cannot raise off", "[tasks.agent_filed]\nlevel = \"off\"\n", "[tasks.agent_filed]\nlevel = \"backlog\"\n", "off", 5, "tasks.agent_filed.level"},
		{"repo lowers ready to backlog", "[tasks.agent_filed]\nlevel = \"ready\"\n", "[tasks.agent_filed]\nlevel = \"backlog\"\n", "backlog", 5, ""},
		{"repo cannot raise cap", "", "[tasks.agent_filed]\nmax_per_day = 50\n", "backlog", 5, "tasks.agent_filed.max_per_day"},
		{"repo lowers cap", "", "[tasks.agent_filed]\nmax_per_day = 1\n", "backlog", 1, ""},
		{"user may choose ready", "[tasks.agent_filed]\nlevel = \"ready\"\n", "", "ready", 5, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, rep, err := Resolve(layerRepo(t, c.user, c.repo))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Tasks.AgentFiled.Level != c.wantLevel || cfg.Tasks.AgentFiled.MaxPerDay != c.wantMax {
				t.Errorf("effective = %+v, want %s and %d", cfg.Tasks.AgentFiled, c.wantLevel, c.wantMax)
			}
			if c.refusedKey != "" && !refused(rep, c.refusedKey) {
				t.Errorf("%s not refused: %+v", c.refusedKey, rep.Refused)
			}
			if c.refusedKey == "" && len(rep.Refused) > 0 {
				t.Errorf("unexpected refusal %+v", rep.Refused)
			}
		})
	}
}

func TestAgentFiledRejectsBadValues(t *testing.T) {
	for _, user := range []string{"[tasks.agent_filed]\nlevel = \"always\"\n", "[tasks.agent_filed]\nmax_per_day = 0\n"} {
		if _, _, err := Resolve(layerRepo(t, user, "")); err == nil {
			t.Errorf("%q was accepted", user)
		}
	}
}

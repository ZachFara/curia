package config

import "testing"

func TestRepoFileCanOnlyTightenContainerLimits(t *testing.T) {
	for _, c := range []struct {
		name, user, repo, key string
		per, mem              int
		refused               bool
	}{
		{"lower cap taken", "", "[resources]\nmax_containers_per_session = 2\n", "", 2, 0, false},
		{"higher cap refused", "", "[resources]\nmax_containers_per_session = 9\n", "resources.max_containers_per_session", 3, 0, true},
		{"a limit cannot be imposed over none", "", "[resources]\ncontainer_memory_mb = 256\n", "resources.container_memory_mb", 3, 0, true},
		{"a repository cannot switch a cap off", "", "[resources]\nmax_containers_per_session = 0\n", "resources.max_containers_per_session", 3, 0, true},
		{"nor over a user's off", "[resources]\nmax_containers_per_session = 0\n", "[resources]\nmax_containers_per_session = 2\n", "resources.max_containers_per_session", 0, 0, true},
		{"memory limit cannot be raised", "[resources]\ncontainer_memory_mb = 256\n", "[resources]\ncontainer_memory_mb = 1024\n", "resources.container_memory_mb", 3, 256, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, rep, err := Resolve(layerRepo(t, c.user, c.repo))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Resources.MaxContainersPerSession != c.per || cfg.Resources.ContainerMemoryMB != c.mem || refused(rep, c.key) != c.refused {
				t.Errorf("effective %+v refused=%v", cfg.Resources, rep.Refused)
			}
		})
	}
	if d := Default().Resources; d.MaxContainersPerSession != 3 || d.MaxContainersTotal != 8 {
		t.Errorf("defaults = %+v, want 3 and 8", d)
	}
}

func TestZeroCapsShowOffAndValidate(t *testing.T) {
	cfg, _, err := Resolve(layerRepo(t, "[resources]\nmax_containers_total = 0\ndocker_memory_refuse_pct = 0\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"resources.max_containers_total", "resources.docker_memory_refuse_pct"} {
		if v, _ := Get(cfg, k); v != "off" {
			t.Errorf("%s shows %q, want off", k, v)
		}
	}
	if v, _ := Get(cfg, "resources.max_containers_per_session"); v != "3" {
		t.Errorf("per-session = %q", v)
	}
	if _, _, err = Resolve(layerRepo(t, "[resources]\ndocker_memory_refuse_pct = 101\n", "")); err == nil {
		t.Error("101 percent accepted")
	}
}

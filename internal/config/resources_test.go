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
		{"a limit over none tightens", "", "[resources]\ncontainer_memory_mb = 256\n", "", 3, 256, false},
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

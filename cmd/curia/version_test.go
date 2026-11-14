package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionString(t *testing.T) {
	settings := func(kv ...string) []debug.BuildSetting {
		var out []debug.BuildSetting
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return out
	}
	const rev = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name              string
		ver, commit, date string
		bi                *debug.BuildInfo
		want              string
	}{
		{"no build info", "", "", "", nil, "dev"},
		{"empty build info", "", "", "", &debug.BuildInfo{}, "dev"},
		{
			"go install at a tag", "", "", "",
			&debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, "v0.1.0",
		},
		{
			"go install of an untagged commit", "", "", "",
			&debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261113120000-0123456789ab"}},
			"v0.0.0-20261113120000-0123456789ab",
		},
		{
			"local build, revision only", "", "", "",
			&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("vcs.revision", rev, "vcs.modified", "false")},
			"0123456",
		},
		{
			"local dirty build", "", "", "",
			&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("vcs.revision", rev, "vcs.modified", "true")},
			"0123456-dirty",
		},
		{
			"short revision is kept whole", "", "", "",
			&debug.BuildInfo{Settings: settings("vcs.revision", "abc")}, "abc",
		},
		{
			"(devel) without vcs data", "", "", "",
			&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev",
		},
		{
			"link-time version wins over build info", "v0.2.0", rev, "2026-11-13T12:00:00Z",
			&debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, "v0.2.0 (0123456, 2026-11-13T12:00:00Z)",
		},
		{"link-time version alone", "v0.2.0", "", "", nil, "v0.2.0"},
		{"link-time version and date only", "v0.2.0", "", "2026-11-13", nil, "v0.2.0 (2026-11-13)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionString(tc.ver, tc.commit, tc.date, tc.bi); got != tc.want {
				t.Errorf("versionString = %q, want %q", got, tc.want)
			}
		})
	}
}

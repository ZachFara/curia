package config

import (
	"os"
	"path/filepath"
	"testing"
)

func modeWrite(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRepoCannotRaiseAMode(t *testing.T) {
	o := layerRepo(t, "", "[mode]\ndefault = \"edits\"\naway_allow = [\"*\"]\nbypass_for = \"24h\"\n")
	c, r, err := Resolve(o)
	if err != nil || c.Mode.Default != "normal" || c.Mode.BypassFor != Default().Mode.BypassFor || len(c.Mode.AwayAllow) != 5 {
		t.Errorf("repository changed the mode config: %v %+v", err, c.Mode)
	}
	for _, k := range []string{"mode.default", "mode.away_allow", "mode.bypass_for"} {
		if !refused(r, k) {
			t.Errorf("%s was not refused: %+v", k, r.Refused)
		}
	}
}

func TestUserModeDefaultIsNormalOrEditsOnly(t *testing.T) {
	if c, err := Load(modeWrite(t, "[mode]\ndefault = \"edits\"\n")); err != nil || c.Mode.Default != "edits" {
		t.Errorf("edits: %v %+v", err, c.Mode)
	}
	for _, d := range []string{"away", "bypass", "wide"} {
		if _, err := Load(modeWrite(t, "[mode]\ndefault = \""+d+"\"\n")); err == nil {
			t.Errorf("default %q was accepted", d)
		}
	}
}

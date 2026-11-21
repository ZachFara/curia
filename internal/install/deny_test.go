package install

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/ZachFara/curia/internal/hook"
)

func readDeny(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, r := range asList(asMap(readSettings(t, path)["permissions"])["deny"]) {
		s, _ := r.(string)
		out = append(out, s)
	}
	return out
}

// install allows the seven tools Curia registers; the hook's list is the six a
// budget-stopped task may still call, so file_task is the one difference.
func TestHookKnowsTheSixToolRules(t *testing.T) {
	for _, r := range toolRules {
		if hook.IsCuriaTool(r) == (r == "mcp__curia__file_task") {
			t.Errorf("hook.IsCuriaTool(%q) is false", r)
		}
	}
}

func TestDenyOwnFilesIsAdditiveAndUninstallRemovesExactlyThose(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, `{"model":"opus","permissions":{"allow":["Bash(ls:*)"],"deny":["Bash(rm:*)","Edit(~/.config/curia/**)"],"ask":["Bash(git push:*)"]}}`)
	if err := DenyOwnFiles(path, testNow); err != nil {
		t.Fatal(err)
	}
	got := readDeny(t, path)
	for _, r := range ownFileDenyRules {
		if !slices.Contains(got, r) {
			t.Errorf("deny %q lacks %q", got, r)
		}
	}
	if got[0] != "Bash(rm:*)" || len(got) != 1+len(ownFileDenyRules) {
		t.Errorf("deny = %q, want the user's rule first and nothing dropped", got)
	}
	m := readSettings(t, path)
	perms := asMap(m["permissions"])
	if m["model"] != "opus" || len(asList(perms["allow"])) != 1 || len(asList(perms["ask"])) != 1 {
		t.Errorf("other settings changed: %v", m)
	}
	once := readFile(t, path)
	if err := DenyOwnFiles(path, testNow.Add(1)); err != nil || readFile(t, path) != once {
		t.Errorf("second DenyOwnFiles changed the file or failed: %v", err)
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 1 {
		t.Errorf("backups = %v, want one", matches)
	}
	if err := Unhooks(path, "/usr/local/bin/curia"); err != nil {
		t.Fatal(err)
	}
	// The rule that was the user's own before install is removed with ours: the same
	// limit as the six tool rules (a rule is ours by its text).
	if got := readDeny(t, path); !slices.Equal(got, []string{"Bash(rm:*)"}) {
		t.Errorf("deny after uninstall = %q, want the user's other rule only", got)
	}
	if got := readAllow(t, path); !slices.Equal(got, []string{"Bash(ls:*)"}) {
		t.Errorf("allow after uninstall = %q", got)
	}
}

func TestDenyOwnFilesCreatesAndRefusesBadShape(t *testing.T) {
	path := settingsIn(t)
	if err := DenyOwnFiles(path, testNow); err != nil {
		t.Fatal(err)
	}
	if got := readDeny(t, path); !slices.Equal(got, ownFileDenyRules) {
		t.Errorf("deny = %q, want %q", got, ownFileDenyRules)
	}
	bad := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, bad, `{"permissions":{"deny":"x"}}`)
	if err := DenyOwnFiles(bad, testNow); err == nil || readFile(t, bad) != `{"permissions":{"deny":"x"}}` {
		t.Errorf("a deny of the wrong type: err %v, file changed or not refused", err)
	}
}

// Review A-7: the deny rules cover the directories actually in use (XDG variables, a moved
// database), stay additive, and uninstall removes them and the home-default rules of an older install.
func TestDenyOwnFilesFollowsXDGAndRelocatedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xc"))
	t.Setenv("XDG_DATA_HOME", "/srv/data")
	t.Setenv("XDG_STATE_HOME", "")
	db := "/srv/db/curia.db"
	path := settingsIn(t)
	writeFile(t, path, `{"permissions":{"deny":["Bash(rm:*)"]}}`)
	if err := DenyOwnFiles(path, testNow, db); err != nil {
		t.Fatal(err)
	}
	got := readDeny(t, path)
	for _, want := range []string{
		"Bash(rm:*)", "Edit(~/.config/curia/**)", "Edit(~/.local/share/curia/**)", "Edit(~/.local/state/curia/**)",
		"Edit(~/xc/curia/**)", "Edit(//srv/data/curia/**)", "Edit(//srv/db/curia.db)", "Edit(//srv/db/curia.db-wal)", "Edit(//srv/db/curia.db-shm)",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("deny %q lacks %q", got, want)
		}
	}
	if got[0] != "Bash(rm:*)" {
		t.Errorf("the user's rule moved: %q", got)
	}
	// An older install wrote the home-default forms only; uninstall removes both generations.
	writeFile(t, path, `{"permissions":{"deny":["Bash(rm:*)","Edit(~/.config/curia/**)","Edit(~/.local/share/curia/**)","Edit(~/.local/state/curia/**)"]}}`)
	if err := DenyOwnFiles(path, testNow.Add(1), db); err != nil {
		t.Fatal(err)
	}
	if err := Unhooks(path, "/usr/local/bin/curia", db); err != nil {
		t.Fatal(err)
	}
	if got := readDeny(t, path); !slices.Equal(got, []string{"Bash(rm:*)"}) {
		t.Errorf("deny after uninstall = %q, want the user's rule only", got)
	}
}

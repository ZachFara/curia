package install

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// brewLayout is a Homebrew-like tree in a temp dir: bin/curia is a symlink to
// Cellar/curia/1/bin/curia.
type brewLayout struct{ root, stable string }

func (l brewLayout) cellar(version string) string {
	return filepath.Join(l.root, "Cellar", "curia", version, "bin", "curia")
}

// install writes a stand-in binary for version and points the stable symlink
// at it.
func (l brewLayout) install(t *testing.T, version string) {
	t.Helper()
	p := l.cellar(version)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "curia "+version)
	if err := os.Remove(l.stable); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(l.stable), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p, l.stable); err != nil {
		t.Fatal(err)
	}
}

// upgrade is brew upgrade: version becomes current and the old one is deleted.
func (l brewLayout) upgrade(t *testing.T, from, to string) {
	t.Helper()
	l.install(t, to)
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(l.cellar(from)))); err != nil {
		t.Fatal(err)
	}
}

func newBrewLayout(t *testing.T) brewLayout {
	t.Helper()
	root := t.TempDir()
	return brewLayout{root: root, stable: filepath.Join(root, "bin", "curia")}
}

func commandsOf(t *testing.T, settings string) []string {
	t.Helper()
	var cmds []string
	m := readSettings(t, settings)
	if m["hooks"] == nil {
		return nil
	}
	for _, ev := range hookEvents {
		for _, e := range eventEntries(t, m, ev) {
			entry, _ := e.(map[string]any)
			inner, _ := entry["hooks"].([]any)
			for _, h := range inner {
				hook, _ := h.(map[string]any)
				cmd, _ := hook["command"].(string)
				cmds = append(cmds, cmd)
			}
		}
	}
	return cmds
}

func TestHooksUpgradeRepairsStaleEntries(t *testing.T) {
	l := newBrewLayout(t)
	l.install(t, "1")
	settings := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, settings, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo mine"}]}]}}`)

	// An earlier install recorded the resolved Cellar path.
	if err := Hooks(settings, l.cellar("1"), testNow); err != nil {
		t.Fatal(err)
	}
	l.upgrade(t, "1", "2")

	if err := Hooks(settings, l.stable, testNow); err != nil {
		t.Fatal(err)
	}
	cmds := commandsOf(t, settings)
	if len(cmds) != len(hookEvents)+1 {
		t.Fatalf("commands = %q, want one per event plus the user's", cmds)
	}
	for _, c := range cmds {
		if strings.Contains(c, "Cellar") {
			t.Errorf("leftover stale entry %q", c)
		}
	}
	if !slices.Contains(cmds, "echo mine") || !slices.Contains(cmds, l.stable+" hook Stop") {
		t.Errorf("commands = %q, want the user's hook and the stable path", cmds)
	}
	before := readFile(t, settings)
	if err := Hooks(settings, l.stable, testNow); err != nil || readFile(t, settings) != before {
		t.Errorf("second install changed the file (err %v)", err)
	}
}

func TestHooksRepairsEntryThatStillResolvesToThisBinary(t *testing.T) {
	l := newBrewLayout(t)
	l.install(t, "1")
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := Hooks(settings, l.cellar("1"), testNow); err != nil {
		t.Fatal(err)
	}
	if err := Hooks(settings, l.stable, testNow); err != nil {
		t.Fatal(err)
	}
	for _, c := range commandsOf(t, settings) {
		if strings.Contains(c, "Cellar") {
			t.Errorf("leftover %q", c)
		}
	}
}

func TestUnhooksAfterUpgradeRemovesEverything(t *testing.T) {
	l := newBrewLayout(t)
	l.install(t, "1")
	settings := filepath.Join(t.TempDir(), "settings.json")
	writeFile(t, settings, `{"model":"opus"}`)
	if err := Hooks(settings, l.cellar("1"), testNow); err != nil {
		t.Fatal(err)
	}
	l.upgrade(t, "1", "2")
	if err := Unhooks(settings, l.stable); err != nil {
		t.Fatal(err)
	}
	if cmds := commandsOf(t, settings); len(cmds) != 0 {
		t.Errorf("commands left after uninstall: %q", cmds)
	}
}

func TestStaleRecognitionIsNarrow(t *testing.T) {
	l := newBrewLayout(t)
	l.install(t, "1")
	other := filepath.Join(l.root, "other", "curia")
	writeFile(t, filepath.Join(l.root, "other-curia"), "x")
	for name, cmd := range map[string]string{
		"missing, not versioned":       "/usr/local/bin/curia hook Stop",
		"missing versioned, not curia": l.root + "/Cellar/curia/0.1.0/bin/other hook Stop",
		"missing versioned, not hook":  l.root + "/Cellar/curia/0.1.0/bin/curia notify Stop",
		"missing versioned, unknown":   l.root + "/Cellar/curia/0.1.0/bin/curia hook Nope",
		"command that mentions curia":  "echo " + l.root + "/Cellar/curia/0.1.0/bin/curia hook Stop",
		"existing different file":      filepath.Join(l.root, "other-curia") + " hook Stop",
		"relative":                     "Cellar/curia/0.1.0/bin/curia hook Stop",
		"never existed, other":         other + " hook Stop",
	} {
		hook := map[string]any{"command": cmd}
		if isOurs(hook, l.stable) {
			t.Errorf("%s: %q recognized as ours", name, cmd)
		}
	}
	for name, cmd := range map[string]string{
		"versioned and gone":     l.root + "/Cellar/curia/0.1.0/bin/curia hook Stop",
		"quoted, versioned":      "'" + l.root + "/Cel lar/curia/v2/bin/curia' hook SessionEnd",
		"symlink to this binary": l.stable + " hook Stop",
	} {
		if !isOurs(map[string]any{"command": cmd}, l.cellar("1")) {
			t.Errorf("%s: %q not recognized as ours", name, cmd)
		}
	}
}

func TestMCPUpgradeReregistersAndUninstallRemoves(t *testing.T) {
	l := newBrewLayout(t)
	l.install(t, "1")
	stale := ourConfig(l.cellar("1"))
	l.upgrade(t, "1", "2")

	r := newRecorder(t)
	state, err := AddMCP(context.Background(), r.run, configWith(t, stale), l.stable)
	want := []string{"mcp remove --scope user curia", "mcp add --scope user curia -- " + l.stable + " mcp"}
	if err != nil || state != MCPUpdated || !slices.Equal(r.calls, want) {
		t.Errorf("AddMCP = %v, %v, calls %q; want MCPUpdated and %q", state, err, r.calls, want)
	}

	r = newRecorder(t)
	state, err = RemoveMCP(context.Background(), r.run, configWith(t, stale), l.stable)
	if err != nil || state != MCPRemoved || !slices.Equal(r.calls, want[:1]) {
		t.Errorf("RemoveMCP = %v, %v, calls %q; want MCPRemoved", state, err, r.calls)
	}

	r = newRecorder(t)
	state, err = AddMCP(context.Background(), r.run, configWith(t, ourConfig(l.stable)), l.stable)
	if err != nil || state != MCPAlreadyOurs || len(r.calls) != 0 {
		t.Errorf("AddMCP with the stable path = %v, %v, calls %q; want MCPAlreadyOurs", state, err, r.calls)
	}
}

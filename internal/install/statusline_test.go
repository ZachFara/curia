package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// statusLineOf returns the settings file's statusLine value and whether the
// key is there.
func statusLineOf(t *testing.T, path string) (any, bool) {
	t.Helper()
	v, ok := readSettings(t, path)["statusLine"]
	return v, ok
}

// backupsOf returns the backups written beside path.
func backupsOf(path string) []string {
	m, _ := filepath.Glob(path + ".bak.*")
	return m
}

// ourStatusLine is the value StatusLine writes for bin.
func ourStatusLine(bin string) map[string]any {
	return map[string]any{"type": "command", "command": StatusLineCommand(bin)}
}

const statusLineUserSettings = `{
  "model": "opus",
  "bigNumber": 12345678901234567890,
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]},
  "permissions": {"allow": ["Bash(ls:*)"], "deny": ["Read(.env)"]}
}`

func TestStatusLineAddsWhenAbsent(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, statusLineUserSettings)
	before := readSettings(t, path)

	state, err := StatusLine(path, testBin, testNow)
	if err != nil || state != StatusLineAdded {
		t.Fatalf("StatusLine = %v, %v; want StatusLineAdded", state, err)
	}
	after := readSettings(t, path)
	want := map[string]any{"type": "command", "command": "/opt/curia/bin/curia statusline"}
	if got := after["statusLine"]; !reflect.DeepEqual(got, want) {
		t.Errorf("statusLine = %v, want %v", got, want)
	}
	delete(after, "statusLine")
	if !reflect.DeepEqual(after, before) {
		t.Errorf("other keys changed:\n%v\nwant\n%v", after, before)
	}
	if !strings.Contains(readFile(t, path), "12345678901234567890") {
		t.Error("large number not preserved")
	}
	if b := backupsOf(path); len(b) != 1 || readFile(t, b[0]) != statusLineUserSettings {
		t.Errorf("backups = %v, want one holding the original bytes", b)
	}
}

func TestStatusLineCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".claude", "settings.json")
	state, err := StatusLine(path, testBin, testNow)
	if err != nil || state != StatusLineAdded {
		t.Fatalf("StatusLine = %v, %v; want StatusLineAdded", state, err)
	}
	m := readSettings(t, path)
	if len(m) != 1 || !reflect.DeepEqual(m["statusLine"], ourStatusLine(testBin)) {
		t.Errorf("settings = %v, want only our statusLine", m)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("new file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if b := backupsOf(path); len(b) != 0 {
		t.Errorf("backup written for a new file: %v", b)
	}
}

func TestStatusLineIdempotent(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, statusLineUserSettings)
	if _, err := StatusLine(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	once := readFile(t, path)
	state, err := StatusLine(path, testBin, testNow.Add(time.Second))
	if err != nil || state != StatusLineOurs {
		t.Fatalf("second StatusLine = %v, %v; want StatusLineOurs", state, err)
	}
	if got := readFile(t, path); got != once {
		t.Errorf("second call changed the file:\n%s", got)
	}
	if b := backupsOf(path); len(b) != 1 {
		t.Errorf("backups = %v, want only the first call's", b)
	}
}

// Ours with extra keys the user added (padding) is still ours, and kept.
func TestStatusLineOursWithExtraKeysKept(t *testing.T) {
	path := settingsIn(t)
	content := `{"statusLine": {"type": "command", "command": "/opt/curia/bin/curia statusline", "padding": 2}}`
	writeFile(t, path, content)
	state, err := StatusLine(path, testBin, testNow)
	if err != nil || state != StatusLineOurs {
		t.Fatalf("StatusLine = %v, %v; want StatusLineOurs", state, err)
	}
	if got := readFile(t, path); got != content {
		t.Errorf("file changed: %s", got)
	}
	if removed, err := RemoveStatusLine(path, testBin); err != nil || !removed {
		t.Errorf("RemoveStatusLine = %v, %v; want removed", removed, err)
	}
}

// statusLineForeignCases are statusLine values that are not Curia's for
// testBin, whatever they contain.
var statusLineForeignCases = map[string]string{
	"user's command":            `{"type": "command", "command": "~/bin/sl.sh"}`,
	"a string":                  `"/opt/curia/bin/curia statusline"`,
	"an array":                  `[{"type": "command", "command": "/opt/curia/bin/curia statusline"}]`,
	"null":                      `null`,
	"a number":                  `3`,
	"a bool":                    `true`,
	"empty object":              `{}`,
	"object without command":    `{"type": "command"}`,
	"command not a string":      `{"type": "command", "command": 7}`,
	"type not command":          `{"type": "static", "command": "/opt/curia/bin/curia statusline"}`,
	"no type":                   `{"command": "/opt/curia/bin/curia statusline"}`,
	"ours piped on":             `{"type": "command", "command": "/opt/curia/bin/curia statusline | cat"}`,
	"ours with an argument":     `{"type": "command", "command": "/opt/curia/bin/curia statusline --x"}`,
	"mentions curia":            `{"type": "command", "command": "echo /opt/curia/bin/curia statusline"}`,
	"chained into the user's":   `{"type": "command", "command": "input=$(cat); printf '%s' \"$input\" | /opt/curia/bin/curia ingest --statusline; printf '%s' \"$input\" | ~/bin/sl.sh"}`,
	"another missing curia":     `{"type": "command", "command": "/usr/local/bin/curia statusline"}`,
	"relative curia":            `{"type": "command", "command": "curia statusline"}`,
	"another curia subcommand":  `{"type": "command", "command": "/usr/local/bin/curia status"}`,
	"leading space":             `{"type": "command", "command": " /opt/curia/bin/curia statusline"}`,
	"trailing space":            `{"type": "command", "command": "/opt/curia/bin/curia statusline "}`,
	"unterminated quoted word":  `{"type": "command", "command": "'/opt/curia/bin/curia statusline"}`,
	"versioned curia, other op": `{"type": "command", "command": "/x/Cellar/curia/0.1.0/bin/curia status"}`,
}

func TestStatusLineForeignUntouched(t *testing.T) {
	for name, value := range statusLineForeignCases {
		t.Run(name, func(t *testing.T) {
			path := settingsIn(t)
			content := `{"model": "opus", "statusLine": ` + value + `}`
			writeFile(t, path, content)
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
			state, err := StatusLine(path, testBin, testNow)
			if err != nil || state != StatusLineForeign {
				t.Errorf("StatusLine = %v, %v; want StatusLineForeign", state, err)
			}
			if removed, err := RemoveStatusLine(path, testBin); err != nil || removed {
				t.Errorf("RemoveStatusLine = %v, %v; want false", removed, err)
			}
			checkUntouched(t, path, content)
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o400 {
				t.Errorf("mode changed: %v, %v", fi.Mode().Perm(), err)
			}
		})
	}
}

// checkUntouched asserts path still holds content and has no backup.
func checkUntouched(t *testing.T, path, content string) {
	t.Helper()
	if got := readFile(t, path); got != content {
		t.Errorf("file changed: %q", got)
	}
	if b := backupsOf(path); len(b) != 0 {
		t.Errorf("backups written: %v", b)
	}
}

// A settings file that is not a JSON object is an error for both calls, and
// nothing is written.
func TestStatusLineInvalidSettingsUntouched(t *testing.T) {
	for name, content := range map[string]string{
		"invalid JSON":     `{`,
		"top level array":  `[]`,
		"top level null":   `null`,
		"trailing garbage": `{} {}`,
		"empty file":       ``,
	} {
		t.Run(name, func(t *testing.T) {
			path := settingsIn(t)
			writeFile(t, path, content)
			if _, err := StatusLine(path, testBin, testNow); err == nil {
				t.Error("StatusLine: want error")
			}
			if _, err := RemoveStatusLine(path, testBin); err == nil {
				t.Error("RemoveStatusLine: want error")
			}
			checkUntouched(t, path, content)
		})
	}
}

// Curia's command for another binary that exists and is a different file is
// not this binary's: it is foreign to install and only that binary's
// uninstall removes it.
func TestStatusLineOtherBinaryIsForeign(t *testing.T) {
	dir := t.TempDir()
	other, this := filepath.Join(dir, "a", "curia"), filepath.Join(dir, "b", "curia")
	for _, p := range []string{other, this} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		writeFile(t, p, p)
	}
	for name, otherBin := range map[string]string{"existing other file": other, "missing, not versioned": testBin} {
		t.Run(name, func(t *testing.T) { checkOtherBinaryIsForeign(t, otherBin, this) })
	}
}

// checkOtherBinaryIsForeign registers the status line for otherBin and
// checks that this binary's install and uninstall leave it, and otherBin's
// uninstall removes it.
func checkOtherBinaryIsForeign(t *testing.T, otherBin, this string) {
	t.Helper()
	path := settingsIn(t)
	if _, err := StatusLine(path, otherBin, testNow); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	state, err := StatusLine(path, this, testNow)
	if err != nil || state != StatusLineForeign {
		t.Errorf("StatusLine for this binary = %v, %v; want StatusLineForeign", state, err)
	}
	if removed, err := RemoveStatusLine(path, this); err != nil || removed {
		t.Errorf("RemoveStatusLine for this binary = %v, %v; want false", removed, err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("file changed: %s", got)
	}
	if removed, err := RemoveStatusLine(path, otherBin); err != nil || !removed {
		t.Errorf("RemoveStatusLine for the other binary = %v, %v; want removed", removed, err)
	}
	if _, ok := statusLineOf(t, path); ok {
		t.Error("statusLine still there")
	}
}

// After an upgrade deleted the versioned path an earlier install recorded,
// install repairs the entry to the stable path, and uninstall removes it.
func TestStatusLineUpgradeRepairsStaleEntry(t *testing.T) {
	l := newBrewLayout(t)
	l.install(t, "3")
	path := settingsIn(t)
	writeFile(t, path, `{"statusLine": {"type": "command", "command": `+jsonString(StatusLineCommand(l.cellar("3")))+`, "padding": 1}, "model": "opus"}`)
	l.upgrade(t, "3", "4")

	state, err := StatusLine(path, l.stable, testNow)
	if err != nil || state != StatusLineUpdated {
		t.Fatalf("StatusLine = %v, %v; want StatusLineUpdated", state, err)
	}
	m := readSettings(t, path)
	want := map[string]any{"type": "command", "command": StatusLineCommand(l.stable), "padding": float64(1)}
	if !reflect.DeepEqual(m["statusLine"], want) || m["model"] != "opus" {
		t.Errorf("settings = %v, want statusLine %v and the model kept", m, want)
	}
	if b := backupsOf(path); len(b) != 1 {
		t.Errorf("backups = %v, want one", b)
	}
	once := readFile(t, path)
	if state, err := StatusLine(path, l.stable, testNow); err != nil || state != StatusLineOurs || readFile(t, path) != once {
		t.Errorf("second StatusLine = %v, %v (file changed: %v); want StatusLineOurs", state, err, readFile(t, path) != once)
	}

	writeFile(t, path, `{"statusLine": {"type": "command", "command": `+jsonString(StatusLineCommand(l.cellar("3")))+`}}`)
	if removed, err := RemoveStatusLine(path, l.stable); err != nil || !removed {
		t.Errorf("RemoveStatusLine of the stale entry = %v, %v; want removed", removed, err)
	}
}

// An entry naming a path that still resolves to this binary (here the
// versioned file the stable symlink points at) is repaired too.
func TestStatusLineRepairsEntryThatResolvesToThisBinary(t *testing.T) {
	l := newBrewLayout(t)
	l.install(t, "1")
	path := settingsIn(t)
	if _, err := StatusLine(path, l.cellar("1"), testNow); err != nil {
		t.Fatal(err)
	}
	state, err := StatusLine(path, l.stable, testNow)
	if err != nil || state != StatusLineUpdated {
		t.Fatalf("StatusLine = %v, %v; want StatusLineUpdated", state, err)
	}
	if v, _ := statusLineOf(t, path); !reflect.DeepEqual(v, ourStatusLine(l.stable)) {
		t.Errorf("statusLine = %v", v)
	}
}

func TestRemoveStatusLineOnlyOurs(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, statusLineUserSettings)
	before := readSettings(t, path)
	if _, err := StatusLine(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveStatusLine(path, testBin)
	if err != nil || !removed {
		t.Fatalf("RemoveStatusLine = %v, %v; want removed", removed, err)
	}
	if after := readSettings(t, path); !reflect.DeepEqual(after, before) {
		t.Errorf("after removal = %v, want %v", after, before)
	}
	if b := backupsOf(path); len(b) != 2 {
		t.Errorf("backups = %v, want one per write", b)
	}

	foreign := `{"statusLine": {"type": "command", "command": "~/bin/sl.sh"}}`
	writeFile(t, path, foreign)
	if removed, err := RemoveStatusLine(path, testBin); err != nil || removed || readFile(t, path) != foreign {
		t.Errorf("foreign: RemoveStatusLine = %v, %v, file %q; want false and byte-equal", removed, err, readFile(t, path))
	}

	absent := `{"model": "opus"}`
	writeFile(t, path, absent)
	if removed, err := RemoveStatusLine(path, testBin); err != nil || removed || readFile(t, path) != absent {
		t.Errorf("absent: RemoveStatusLine = %v, %v, file %q; want false and byte-equal", removed, err, readFile(t, path))
	}

	missing := filepath.Join(t.TempDir(), "settings.json")
	if removed, err := RemoveStatusLine(missing, testBin); err != nil || removed {
		t.Errorf("missing file: RemoveStatusLine = %v, %v; want false", removed, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("RemoveStatusLine created the file: %v", err)
	}
}

// statusLineAwkwardBins are awkwardBins plus a path with a $, which the
// shell must not expand.
func statusLineAwkwardBins() map[string]string {
	bins := map[string]string{
		"/Users/a b/curia":        `'/Users/a b/curia'`,
		"/opt/my $HOME dir/curia": `'/opt/my $HOME dir/curia'`,
	}
	for b, q := range awkwardBins {
		bins[b] = q
	}
	return bins
}

func TestStatusLineBinWithSpaces(t *testing.T) {
	for bin, quoted := range statusLineAwkwardBins() {
		t.Run(bin, func(t *testing.T) {
			path := settingsIn(t)
			if _, err := StatusLine(path, bin, testNow); err != nil {
				t.Fatal(err)
			}
			v, _ := statusLineOf(t, path)
			cmd, _ := asMap(v)["command"].(string)
			if want := quoted + " statusline"; cmd != want {
				t.Errorf("command = %q, want %q", cmd, want)
			}
			out, err := exec.Command("/bin/sh", "-c", "printf '[%s]' "+cmd).Output()
			if want := "[" + bin + "][statusline]"; err != nil || string(out) != want {
				t.Errorf("sh parses %q as %q (%v), want %q", cmd, out, err, want)
			}
			// The quoted form is recognized as ours.
			if state, err := StatusLine(path, bin, testNow); err != nil || state != StatusLineOurs {
				t.Errorf("second StatusLine = %v, %v; want StatusLineOurs", state, err)
			}
			if removed, err := RemoveStatusLine(path, bin); err != nil || !removed {
				t.Errorf("RemoveStatusLine = %v, %v; want removed", removed, err)
			}
		})
	}
}

// The chain line install prints for a user's own status line runs under a
// POSIX sh: Curia's ingest and the user's command each get the whole input.
// "curia" is a symlink to /bin/sh, so "<bin> ingest --statusline" runs the
// script ./ingest from the working directory, which records its input.
func TestStatusLineChainRunsInSh(t *testing.T) {
	for _, sub := range []string{"plain", "it's a $HOME dir"} {
		t.Run(sub, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), sub)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "curia")
			if err := os.Symlink("/bin/sh", bin); err != nil {
				t.Fatal(err)
			}
			work := t.TempDir()
			writeFile(t, filepath.Join(work, "ingest"), `[ "$1" = --statusline ] && cat > got`+"\n")
			chain := StatusLineChain(bin)
			if !strings.HasSuffix(chain, "| <your command>") {
				t.Fatalf("chain = %q, want it to end with the placeholder", chain)
			}
			c := exec.Command("/bin/sh", "-c", strings.TrimSuffix(chain, "<your command>")+"tr a-z A-Z")
			c.Dir = work
			c.Stdin = strings.NewReader(`{"session_id":"s1"}`)
			out, err := c.Output()
			if err != nil || string(out) != `{"SESSION_ID":"S1"}` {
				t.Errorf("user's command printed %q (%v)", out, err)
			}
			if got := readFile(t, filepath.Join(work, "got")); got != `{"session_id":"s1"}` {
				t.Errorf("ingest got %q", got)
			}
		})
	}
}

func TestStatusLineSymlinkedSettingsSurvive(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "repo", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, target, statusLineUserSettings)
	link := filepath.Join(dir, "home", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	checkLink := func(step string) {
		t.Helper()
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: link replaced: %v %v", step, fi, err)
		}
	}
	if _, err := StatusLine(link, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	checkLink("StatusLine")
	if v, _ := statusLineOf(t, target); !reflect.DeepEqual(v, ourStatusLine(testBin)) {
		t.Errorf("target statusLine = %v", v)
	}
	if removed, err := RemoveStatusLine(link, testBin); err != nil || !removed {
		t.Fatalf("RemoveStatusLine = %v, %v", removed, err)
	}
	checkLink("RemoveStatusLine")
	if _, ok := statusLineOf(t, target); ok || readSettings(t, target)["model"] != "opus" {
		t.Errorf("target after removal = %v", readSettings(t, target))
	}
	if got := readFile(t, link+".bak.1700000000"); got != statusLineUserSettings {
		t.Errorf("backup beside link = %q", got)
	}
}

// A rewritten settings file keeps its mode.
func TestStatusLineKeepsFileMode(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, statusLineUserSettings)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := StatusLine(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o400 {
		t.Errorf("after StatusLine: mode %v, %v; want 0400", fi.Mode().Perm(), err)
	}
	if _, err := RemoveStatusLine(path, testBin); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o400 {
		t.Errorf("after RemoveStatusLine: mode %v, %v; want 0400", fi.Mode().Perm(), err)
	}
}

// Hooks and the status line share the file: each keeps what the other wrote,
// and Unhooks leaves the status line to RemoveStatusLine.
func TestStatusLineAndHooksShareTheFile(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, statusLineUserSettings)
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := StatusLine(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	checkOurEntries(t, m)
	if !reflect.DeepEqual(m["statusLine"], ourStatusLine(testBin)) {
		t.Errorf("statusLine = %v", m["statusLine"])
	}
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	if v, _ := statusLineOf(t, path); !reflect.DeepEqual(v, ourStatusLine(testBin)) {
		t.Errorf("Unhooks touched the status line: %v", v)
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

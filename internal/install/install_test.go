package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testBin = "/opt/curia/bin/curia"

var testNow = time.Unix(1700000000, 0)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// eventEntries returns settings["hooks"][event] as a slice of entries.
func eventEntries(t *testing.T, m map[string]any, event string) []any {
	t.Helper()
	hooks, ok := m["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks missing or wrong type: %v", m["hooks"])
	}
	entries, _ := hooks[event].([]any)
	return entries
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func countOurs(entries []any) int {
	n := 0
	for _, e := range entries {
		inner, _ := e.(map[string]any)["hooks"].([]any)
		for _, h := range inner {
			cmd, _ := h.(map[string]any)["command"].(string)
			if strings.HasPrefix(cmd, testBin+" hook ") {
				n++
			}
		}
	}
	return n
}

func settingsIn(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "settings.json")
}

const userSettings = `{
  "model": "opus",
  "bigNumber": 12345678901234567890,
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}],
    "Custom": [{"hooks": []}]
  }
}`

func TestHooksMergePreservesUserHooks(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, userSettings)

	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, path)
	if !strings.Contains(raw, "12345678901234567890") {
		t.Errorf("large number not preserved: %s", raw)
	}
	m := readSettings(t, path)
	if m["model"] != "opus" {
		t.Errorf("model = %v", m["model"])
	}
	stop := eventEntries(t, m, "Stop")
	if len(stop) != 2 {
		t.Fatalf("Stop entries = %d, want user's plus ours", len(stop))
	}
	first := asList(asMap(stop[0])["hooks"])
	if asMap(first[0])["command"] != "say done" {
		t.Errorf("user hook not first: %v", stop[0])
	}
	if countOurs(stop) != 1 {
		t.Errorf("ours in Stop = %d", countOurs(stop))
	}
	if _, ok := asMap(m["hooks"])["Custom"]; !ok {
		t.Error("unknown event key dropped")
	}
	checkOurEntries(t, m)
	if len(hookEvents) != 8 {
		t.Errorf("hookEvents = %d, want 8", len(hookEvents))
	}
}

// checkOurEntries asserts each event ends with exactly one well-formed entry of ours.
func checkOurEntries(t *testing.T, m map[string]any) {
	t.Helper()
	for _, ev := range hookEvents {
		entries := eventEntries(t, m, ev)
		if countOurs(entries) != 1 {
			t.Errorf("%s: ours = %d", ev, countOurs(entries))
		}
		last := asMap(entries[len(entries)-1])
		_, hasMatcher := last["matcher"]
		if want := ev == "PreToolUse" || ev == "PostToolUse"; hasMatcher != want {
			t.Errorf("%s: matcher present = %v, want %v", ev, hasMatcher, want)
		} else if want && last["matcher"] != "*" {
			t.Errorf("%s: matcher = %v", ev, last["matcher"])
		}
		cmd := asMap(asList(last["hooks"])[0])
		if cmd["type"] != "command" || cmd["command"] != testBin+" hook "+ev {
			t.Errorf("%s: hook = %v", ev, cmd)
		}
	}
}

func TestHooksIdempotent(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, userSettings)
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	once := readFile(t, path)
	if err := Hooks(path, testBin, testNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if twice := readFile(t, path); twice != once {
		t.Errorf("second Hooks changed the file:\n%s\nvs\n%s", once, twice)
	}
	m := readSettings(t, path)
	for _, ev := range hookEvents {
		if n := countOurs(eventEntries(t, m, ev)); n != 1 {
			t.Errorf("%s: ours = %d", ev, n)
		}
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 1 {
		t.Errorf("backups = %v, want only the first run's", matches)
	}
}

func TestHooksBackupWritten(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, userSettings)
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path+".bak.1700000000"); got != userSettings {
		t.Errorf("backup = %q, want original bytes", got)
	}
}

func TestInvalidJSONUntouched(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, "{")
	if err := Hooks(path, testBin, testNow); err == nil {
		t.Error("Hooks on invalid JSON: want error")
	}
	if err := Unhooks(path, testBin); err == nil {
		t.Error("Unhooks on invalid JSON: want error")
	}
	if got := readFile(t, path); got != "{" {
		t.Errorf("file changed: %q", got)
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 0 {
		t.Errorf("backups written: %v", matches)
	}
}

func TestUnexpectedTypesUntouched(t *testing.T) {
	cases := map[string]string{
		"hooks is a string":      `{"hooks": "nope"}`,
		"hooks is an array":      `{"hooks": []}`,
		"event is an object":     `{"hooks": {"Stop": {"a": 1}}}`,
		"event is a string":      `{"hooks": {"PreToolUse": "x"}}`,
		"top level is an array":  `[]`,
		"top level is null":      `null`,
		"hooks is null":          `{"hooks": null}`,
		"trailing garbage":       `{} {}`,
		"event is null":          `{"hooks": {"Stop": null}}`,
		"event is a number":      `{"hooks": {"Notification": 3}}`,
		"hooks is a number":      `{"hooks": 1}`,
		"hooks is a bool":        `{"hooks": true}`,
		"event is bool":          `{"hooks": {"SessionEnd": false}}`,
		"top level is a string":  `"x"`,
		"top level is a number":  `1`,
		"empty file":             ``,
		"whitespace only":        `   `,
		"event is nested object": `{"hooks": {"SessionStart": {"hooks": []}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := settingsIn(t)
			writeFile(t, path, content)
			if err := Hooks(path, testBin, testNow); err == nil {
				t.Error("Hooks: want error")
			}
			if err := Unhooks(path, testBin); err == nil {
				t.Error("Unhooks: want error")
			}
			if got := readFile(t, path); got != content {
				t.Errorf("file changed: %q", got)
			}
			if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 0 {
				t.Errorf("backups written: %v", matches)
			}
		})
	}
}

func TestMissingSettingsCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".claude", "settings.json")
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	for _, ev := range hookEvents {
		if n := countOurs(eventEntries(t, m, ev)); n != 1 {
			t.Errorf("%s: ours = %d", ev, n)
		}
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 0 {
		t.Errorf("backup written for a new file: %v", matches)
	}
}

func TestUnhooksRemovesOnlyOurs(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, userSettings)
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	if m["model"] != "opus" {
		t.Errorf("model = %v", m["model"])
	}
	hooks := asMap(m["hooks"])
	stop := eventEntries(t, m, "Stop")
	if len(stop) != 1 || countOurs(stop) != 0 {
		t.Errorf("Stop = %v, want only the user's hook", stop)
	}
	if _, ok := hooks["Custom"]; !ok {
		t.Error("user's Custom event removed")
	}
	for _, ev := range hookEvents {
		if ev == "Stop" {
			continue
		}
		if _, ok := hooks[ev]; ok {
			t.Errorf("empty event %s left behind", ev)
		}
	}
}

func TestUnhooksKeepsUserCommandInSharedEntry(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, `{"hooks":{"Stop":[{"hooks":[
		{"type":"command","command":"say done"},
		{"type":"command","command":"`+testBin+` hook Stop"},
		{"type":"command","command":"`+testBin+`-other hook Stop"}]}]}}`)
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	stop := eventEntries(t, readSettings(t, path), "Stop")
	inner := asList(asMap(stop[0])["hooks"])
	if len(stop) != 1 || len(inner) != 2 {
		t.Fatalf("Stop = %v, want user's two commands kept", stop)
	}
}

func TestUnhooksNothingToDoWritesNothing(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, userSettings)
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != userSettings {
		t.Errorf("file rewritten: %q", got)
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 0 {
		t.Errorf("backups written: %v", matches)
	}
	if err := Unhooks(filepath.Join(t.TempDir(), "absent.json"), testBin); err != nil {
		t.Errorf("Unhooks on missing file: %v", err)
	}
}

func TestUnhooksBacksUpBeforeWriting(t *testing.T) {
	path := settingsIn(t)
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(path + ".bak.*")
	if len(matches) != 1 || readFile(t, matches[0]) != before {
		t.Errorf("backups = %v, want one equal to the pre-Unhooks bytes", matches)
	}
}

func TestTmuxFenceAddedOnceAndRemoved(t *testing.T) {
	dir := t.TempDir()
	snippet := filepath.Join(dir, "cfg", "curia", "curia.tmux")
	conf := filepath.Join(dir, ".tmux.conf")
	original := "set -g mouse on\nbind r source-file ~/.tmux.conf\n"
	writeFile(t, conf, original)

	for range 2 {
		if err := WriteTmux(snippet, conf, testBin); err != nil {
			t.Fatal(err)
		}
	}
	got := readFile(t, conf)
	if n := strings.Count(got, "# >>> curia >>>"); n != 1 {
		t.Errorf("begin fences = %d\n%s", n, got)
	}
	if !strings.HasPrefix(got, original) || !strings.Contains(got, "source-file '"+snippet+"'") {
		t.Errorf("conf = %q", got)
	}
	snip := readFile(t, snippet)
	for _, want := range []string{
		"set -g status-interval 2\n",
		"set -ag status-right ' #(" + testBin + " status)'\n",
		"bind a display-popup -E -w 90% -h 80% '" + testBin + " ui'\n",
		"bind N run-shell '" + testBin + " next'\n",
		`set-hook -g pane-focus-in 'run-shell -b "` + testBin + ` seen #{pane_id}"'` + "\n",
		"set -g window-status-current-format '#{?#{==:#{@curia_marker},red},#[fg=red]● #[default],}#I:#W'\n",
	} {
		if !strings.Contains(snip, want) {
			t.Errorf("snippet missing %q", want)
		}
	}

	if err := RemoveTmux(conf); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, conf); got != original {
		t.Errorf("after RemoveTmux conf = %q, want original", got)
	}
}

func TestTmuxConfCreatedAndEmptiedWhenMissing(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, ".tmux.conf")
	if err := RemoveTmux(conf); err != nil {
		t.Errorf("RemoveTmux on missing conf: %v", err)
	}
	if err := WriteTmux(filepath.Join(dir, "curia.tmux"), conf, testBin); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTmux(conf); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, conf); got != "" {
		t.Errorf("conf = %q, want empty", got)
	}
}

func TestRemoveTmuxLeavesUnfencedAndBrokenConfAlone(t *testing.T) {
	conf := filepath.Join(t.TempDir(), ".tmux.conf")
	plain := "set -g mouse on"
	writeFile(t, conf, plain)
	if err := RemoveTmux(conf); err != nil || readFile(t, conf) != plain {
		t.Errorf("unfenced conf changed or errored: %v", err)
	}
	broken := "set -g mouse on\n# >>> curia >>>\nsource-file x\nmy own lines\n"
	writeFile(t, conf, broken)
	if err := RemoveTmux(conf); err == nil {
		t.Error("fence without end: want error")
	}
	if got := readFile(t, conf); got != broken {
		t.Errorf("conf changed: %q", got)
	}
}

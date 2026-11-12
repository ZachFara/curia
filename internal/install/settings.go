// Package install registers Curia's hooks in the user's Claude Code settings
// and adds the tmux snippet to the user's tmux configuration. Both files are
// owned by the user, so every edit preserves what Curia did not add.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// hookEvents are the Claude Code hook names Curia registers for.
var hookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse",
	"Notification", "Stop", "SubagentStop", "SessionEnd",
}

// Hooks adds Curia's hook entry for each event to the settings file at
// settingsPath, creating the file if it is missing. Entries already there are
// kept and a second call changes nothing. Before rewriting an existing file it
// copies the file to settingsPath+".bak.<unix seconds of now>". A file that is
// not valid JSON, or whose hooks have an unexpected shape, is left untouched
// and an error is returned.
func Hooks(settingsPath, bin string, now time.Time) error {
	settingsPath = filepath.Clean(settingsPath)
	raw, settings, err := loadSettings(settingsPath)
	if err != nil {
		return err
	}
	hooks, err := hooksMap(settings)
	if err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for _, ev := range hookEvents {
		entries, err := eventList(hooks, ev)
		if err != nil {
			return fmt.Errorf("%s: %w", settingsPath, err)
		}
		if containsOurs(entries, bin) {
			continue
		}
		hooks[ev] = append(entries, ourEntry(bin, ev))
		changed = true
	}
	if !changed {
		return nil
	}
	settings["hooks"] = hooks
	return saveSettings(settingsPath, raw, settings, now)
}

// Unhooks removes the hook commands Curia added from the settings file at
// settingsPath. Entries that became empty are dropped; everything else is
// kept. A missing file, or one with nothing of ours in it, is left alone.
// Before rewriting it copies the file to settingsPath+".bak.<unix seconds>".
func Unhooks(settingsPath, bin string) error {
	settingsPath = filepath.Clean(settingsPath)
	raw, settings, err := loadSettings(settingsPath)
	if err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	hooks, err := hooksMap(settings)
	if err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	changed := false
	for _, ev := range hookEvents {
		entries, err := eventList(hooks, ev)
		if err != nil {
			return fmt.Errorf("%s: %w", settingsPath, err)
		}
		kept, removed := withoutOurs(entries, bin)
		if !removed {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	if !changed {
		return nil
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return saveSettings(settingsPath, raw, settings, time.Now())
}

// loadSettings reads and decodes the settings file. A missing file yields nil
// raw bytes and an empty settings map. Numbers decode as json.Number so large
// integers survive the round trip.
func loadSettings(path string) ([]byte, map[string]any, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var settings map[string]any
	if err := dec.Decode(&settings); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid settings JSON: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%s: invalid settings JSON: unexpected content after the object", path)
	}
	if settings == nil {
		return nil, nil, fmt.Errorf("%s: settings must be a JSON object", path)
	}
	return raw, settings, nil
}

// hooksMap returns settings["hooks"], or nil when absent.
func hooksMap(settings map[string]any) (map[string]any, error) {
	v, present := settings["hooks"]
	if !present {
		return nil, nil
	}
	hooks, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf(`"hooks" must be an object, found %T`, v)
	}
	return hooks, nil
}

// eventList returns hooks[event], or nil when absent.
func eventList(hooks map[string]any, event string) ([]any, error) {
	v, present := hooks[event]
	if !present {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("hooks.%s must be an array, found %T", event, v)
	}
	return list, nil
}

// hookPrefix is how every hook command of ours for bin starts.
func hookPrefix(bin string) string {
	return shellQuote(bin) + " hook "
}

// shellQuote returns s as one POSIX shell word: unchanged when it holds only
// [A-Za-z0-9_./-], else single-quoted with each ' written as '\”.
func shellQuote(s string) string {
	const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./-"
	if s != "" && strings.Trim(s, safe) == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ourEntry(bin, event string) map[string]any {
	entry := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": hookPrefix(bin) + event}},
	}
	if event == "PreToolUse" || event == "PermissionRequest" || event == "PostToolUse" {
		entry["matcher"] = "*"
	}
	return entry
}

// isOurs reports whether a hook command was added by Curia for this binary,
// in the form ourEntry writes it.
func isOurs(hook any, bin string) bool {
	h, ok := hook.(map[string]any)
	if !ok {
		return false
	}
	cmd, _ := h["command"].(string)
	return strings.HasPrefix(cmd, hookPrefix(bin))
}

func containsOurs(entries []any, bin string) bool {
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			if isOurs(h, bin) {
				return true
			}
		}
	}
	return false
}

// withoutOurs drops Curia's commands from entries, and any entry that held
// nothing else, and reports whether it removed anything.
func withoutOurs(entries []any, bin string) (kept []any, removed bool) {
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		inner, isList := entry["hooks"].([]any)
		if !ok || !isList {
			kept = append(kept, e)
			continue
		}
		var rest []any
		for _, h := range inner {
			if isOurs(h, bin) {
				removed = true
			} else {
				rest = append(rest, h)
			}
		}
		if len(rest) == len(inner) {
			kept = append(kept, e)
		} else if len(rest) > 0 {
			entry["hooks"] = rest
			kept = append(kept, entry)
		}
	}
	return kept, removed
}

// saveSettings backs up the original bytes (when the file existed) and then
// replaces the file through a temporary file in the same directory.
func saveSettings(path string, original []byte, settings map[string]any, now time.Time) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return err
	}
	if original != nil {
		if err := writeBackup(path, original, now); err != nil {
			return fmt.Errorf("back up settings: %w", err)
		}
	}
	return writeAtomic(path, buf.Bytes())
}

// writeBackup stores original beside path as path+".bak.<unix seconds>",
// adding ".1", ".2" and so on when that name is taken. It never overwrites an
// existing backup.
func writeBackup(path string, original []byte, now time.Time) error {
	base := path + ".bak." + strconv.FormatInt(now.Unix(), 10)
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name += "." + strconv.Itoa(i)
		}
		f, err := os.OpenFile(filepath.Clean(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(original); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
}

// writeAtomic writes data to path via a temporary file and a rename, creating
// the parent directory if needed. If path is a symlink the write lands on the
// file it points to, so the link survives. A file that exists keeps its
// permission bits; a new one is 0600.
func writeAtomic(path string, data []byte) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	mode := fs.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".curia-*") // created 0600
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

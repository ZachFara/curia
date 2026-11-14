// Package install registers Curia's hooks (and, on request, its MCP tool
// rules) in the user's Claude Code settings, adds the tmux snippet to the
// user's tmux configuration, writes the decisions skill and registers the
// curia MCP server through the claude CLI. These files are owned by the user,
// so every edit preserves what Curia did not add, and Claude Code's own
// config JSON is only ever read.
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
	"slices"
	"strconv"
	"strings"
	"time"
)

// hookEvents are the Claude Code hook names Curia registers for.
var hookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
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

// toolRules are the permission rules that let the five curia MCP tools run
// without a prompt.
var toolRules = []string{
	"mcp__curia__search_decisions", "mcp__curia__request_decision",
	"mcp__curia__check_decision", "mcp__curia__withdraw_decision",
	"mcp__curia__check_messages",
}

// AllowTools adds each of the five curia MCP tool rules that is not already
// in permissions.allow of the settings file at settingsPath, creating the
// file, permissions and allow as needed, in one write with the usual backup.
// When all five are there it writes nothing. A permissions or allow value of
// an unexpected type is an error and nothing is written.
func AllowTools(settingsPath string, now time.Time) error {
	settingsPath = filepath.Clean(settingsPath)
	raw, settings, err := loadSettings(settingsPath)
	if err != nil {
		return err
	}
	perms, allow, err := allowList(settings)
	if err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	changed := false
	for _, rule := range toolRules {
		if !slices.Contains(allow, any(rule)) {
			allow = append(allow, rule)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	perms["allow"] = allow
	settings["permissions"] = perms
	return saveSettings(settingsPath, raw, settings, now)
}

// ToolsAllowed reports whether all five curia MCP tool rules are already in
// permissions.allow of the settings file at settingsPath. A file that is
// missing, unreadable or of an unexpected shape reports false.
func ToolsAllowed(settingsPath string) bool {
	_, settings, err := loadSettings(filepath.Clean(settingsPath))
	if err != nil {
		return false
	}
	_, allow, err := allowList(settings)
	if err != nil {
		return false
	}
	for _, rule := range toolRules {
		if !slices.Contains(allow, any(rule)) {
			return false
		}
	}
	return true
}

// allowList returns settings["permissions"] and its "allow" list; an absent
// one is returned empty and not yet stored in settings.
func allowList(settings map[string]any) (map[string]any, []any, error) {
	v, present := settings["permissions"]
	if !present {
		return map[string]any{}, nil, nil
	}
	perms, ok := v.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf(`"permissions" must be an object, found %T`, v)
	}
	a, present := perms["allow"]
	if !present {
		return perms, nil, nil
	}
	allow, ok := a.([]any)
	if !ok {
		return nil, nil, fmt.Errorf(`permissions.allow must be an array, found %T`, a)
	}
	return perms, allow, nil
}

// Unhooks removes the hook commands Curia added, and the five curia MCP tool
// rules whoever added them, from the settings file at settingsPath, in one
// write. Hook entries that became empty are dropped; the allow list is kept
// even when empty, and everything else is kept as it is. A missing file, or
// one with nothing of ours in it, is left alone. Before rewriting it copies
// the file to settingsPath+".bak.<unix seconds>".
func Unhooks(settingsPath, bin string) error {
	settingsPath = filepath.Clean(settingsPath)
	raw, settings, err := loadSettings(settingsPath)
	if err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	hooksChanged, err := removeOurHooks(settings, bin)
	if err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	rulesChanged, err := removeToolRules(settings)
	if err != nil {
		return fmt.Errorf("%s: %w", settingsPath, err)
	}
	if !hooksChanged && !rulesChanged {
		return nil
	}
	return saveSettings(settingsPath, raw, settings, time.Now())
}

// removeOurHooks drops Curia's hook commands for bin from settings, and the
// hooks object if that empties it, and reports whether it changed anything.
func removeOurHooks(settings map[string]any, bin string) (bool, error) {
	hooks, err := hooksMap(settings)
	if err != nil {
		return false, err
	}
	changed := false
	for _, ev := range hookEvents {
		entries, err := eventList(hooks, ev)
		if err != nil {
			return false, err
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
	if changed && len(hooks) == 0 {
		delete(settings, "hooks")
	}
	return changed, nil
}

// removeToolRules drops the five tool rules from permissions.allow and
// reports whether it removed any.
func removeToolRules(settings map[string]any) (bool, error) {
	perms, allow, err := allowList(settings)
	if err != nil {
		return false, err
	}
	kept := make([]any, 0, len(allow))
	for _, rule := range allow {
		if s, ok := rule.(string); !ok || !slices.Contains(toolRules, s) {
			kept = append(kept, rule)
		}
	}
	if len(kept) == len(allow) {
		return false, nil
	}
	perms["allow"] = kept
	return true, nil
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
	if event == "PreToolUse" || event == "PostToolUse" {
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

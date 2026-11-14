package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWriteSkillCreatesFile(t *testing.T) {
	skills := filepath.Join(t.TempDir(), ".claude", "skills")
	if err := WriteSkill(skills); err != nil {
		t.Fatal(err)
	}
	path := SkillPath(skills)
	if got := readFile(t, path); got != string(skillMD) {
		t.Errorf("SKILL.md = %q, want the embedded bytes", got)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("SKILL.md mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if !bytes.Contains(skillMD, []byte("\n"+skillMarker+"\n")) {
		t.Errorf("embedded skill has no marker line %q", skillMarker)
	}
}

func TestWriteSkillIdempotentAndUpgrades(t *testing.T) {
	skills := t.TempDir()
	for range 2 {
		if err := WriteSkill(skills); err != nil {
			t.Fatal(err)
		}
	}
	path := SkillPath(skills)
	if got := readFile(t, path); got != string(skillMD) {
		t.Errorf("after two writes SKILL.md = %q", got)
	}
	writeFile(t, path, "---\nname: curia-decisions\n---\n"+skillMarker+"\n\nAn older version.\n")
	if err := WriteSkill(skills); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != string(skillMD) {
		t.Errorf("older file with the marker not replaced: %q", got)
	}
}

func TestWriteSkillLeavesForeignFile(t *testing.T) {
	skills := t.TempDir()
	path := SkillPath(skills)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	// The marker text inside a line is not the marker line.
	foreign := "---\nname: curia-decisions\n---\nMy own skill. Not " + skillMarker + " at all.\n"
	writeFile(t, path, foreign)
	if err := WriteSkill(skills); !errors.Is(err, ErrForeignSkill) {
		t.Errorf("WriteSkill = %v, want ErrForeignSkill", err)
	}
	if got := readFile(t, path); got != foreign {
		t.Errorf("foreign SKILL.md changed: %q", got)
	}
}

func TestRemoveSkillOnlyOurs(t *testing.T) {
	skills := t.TempDir()
	if err := WriteSkill(skills); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSkill(skills); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(SkillPath(skills))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("skill dir still there: %v", err)
	}
	if _, err := os.Stat(skills); err != nil {
		t.Errorf("skills dir itself removed: %v", err)
	}

	// Missing: nothing to do.
	if err := RemoveSkill(skills); err != nil {
		t.Errorf("RemoveSkill on missing skill: %v", err)
	}
	if err := RemoveSkill(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("RemoveSkill on missing skills dir: %v", err)
	}

	// Foreign: untouched.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	foreign := "my own skill\n"
	writeFile(t, SkillPath(skills), foreign)
	if err := RemoveSkill(skills); !errors.Is(err, ErrForeignSkill) {
		t.Errorf("RemoveSkill on foreign file = %v, want ErrForeignSkill", err)
	}
	if got := readFile(t, SkillPath(skills)); got != foreign {
		t.Errorf("foreign SKILL.md changed: %q", got)
	}
}

func TestRemoveSkillKeepsOtherFilesInDir(t *testing.T) {
	skills := t.TempDir()
	if err := WriteSkill(skills); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(SkillPath(skills)), "notes.md")
	writeFile(t, other, "mine\n")
	if err := RemoveSkill(skills); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SkillPath(skills)); !os.IsNotExist(err) {
		t.Errorf("SKILL.md still there: %v", err)
	}
	if got := readFile(t, other); got != "mine\n" {
		t.Errorf("other file = %q", got)
	}
}

func TestSkillSymlinkedDirSurvives(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "skills")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "home", ".claude", "skills")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	checkLink := func(step string) {
		t.Helper()
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("link replaced after %s: %v %v", step, fi, err)
		}
	}
	if err := WriteSkill(link); err != nil {
		t.Fatal(err)
	}
	checkLink("WriteSkill")
	if got := readFile(t, SkillPath(target)); got != string(skillMD) {
		t.Errorf("skill not written through the link: %q", got)
	}
	if err := RemoveSkill(link); err != nil {
		t.Fatal(err)
	}
	checkLink("RemoveSkill")
	if _, err := os.Stat(filepath.Dir(SkillPath(target))); !os.IsNotExist(err) {
		t.Errorf("skill dir in the link's target still there: %v", err)
	}
}

func TestRemoveSkillUnlinksSymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, target, string(skillMD))
	skills := filepath.Join(dir, "skills")
	link := SkillPath(skills)
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSkill(skills); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("link still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(link)); !os.IsNotExist(err) {
		t.Errorf("emptied skill dir still there: %v", err)
	}
	if got := readFile(t, target); got != string(skillMD) {
		t.Errorf("link target changed: %q", got)
	}
}

func TestRemoveSkillSymlinkedSkillDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "curia-decisions")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skills, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(skills, "curia-decisions")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteSkill(skills); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(target, "SKILL.md")); got != string(skillMD) {
		t.Fatalf("skill not written through the dir link: %q", got)
	}
	if err := RemoveSkill(skills); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("dir link still there: %v", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Errorf("link target = %v, %v; want kept and empty", entries, err)
	}
}

// recorder is a Runner that records each call as its space-joined arguments
// and answers with err.
type recorder struct {
	calls []string
	err   error
}

func (r *recorder) run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	return nil, r.err
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	return &recorder{}
}

// configWith writes a Claude config JSON holding content into a temp dir and
// returns its path.
func configWith(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".claude.json")
	writeFile(t, path, content)
	return path
}

func ourConfig(bin string) string {
	b, _ := json.Marshal(map[string]any{
		"numStartups": 3,
		"mcpServers": map[string]any{
			"other": map[string]any{"type": "stdio", "command": "x", "args": []string{}, "env": map[string]any{}},
			"curia": map[string]any{"type": "stdio", "command": bin, "args": []string{"mcp"}, "env": map[string]any{}},
		},
	})
	return string(b)
}

const wantAdd = "mcp add --scope user curia -- " + testBin + " mcp"

func TestAddMCPSequence(t *testing.T) {
	for name, path := range map[string]string{
		"missing file":     filepath.Join(t.TempDir(), ".claude.json"),
		"no mcpServers":    configWith(t, `{"numStartups": 3}`),
		"no curia entry":   configWith(t, `{"mcpServers": {"other": {"command": "x", "args": []}}}`),
		"empty mcpServers": configWith(t, `{"mcpServers": {}}`),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(t)
			state, err := AddMCP(context.Background(), r.run, path, testBin)
			if err != nil || state != MCPAdded {
				t.Errorf("AddMCP = %v, %v; want MCPAdded", state, err)
			}
			if !slices.Equal(r.calls, []string{wantAdd}) {
				t.Errorf("calls = %q, want [%q]", r.calls, wantAdd)
			}
		})
	}
}

func TestAddMCPAddFailureIsError(t *testing.T) {
	r := newRecorder(t)
	r.err = errors.New("exit status 1")
	state, err := AddMCP(context.Background(), r.run, filepath.Join(t.TempDir(), ".claude.json"), testBin)
	if err == nil {
		t.Errorf("AddMCP = %v, nil; want an error when claude mcp add fails", state)
	}
}

func TestAddMCPAlreadyOurs(t *testing.T) {
	r := newRecorder(t)
	path := configWith(t, `{"mcpServers":{"curia":{"type":"stdio","command":"`+testBin+`","args":["mcp"],"env":{}}}}`)
	state, err := AddMCP(context.Background(), r.run, path, testBin)
	if err != nil || state != MCPAlreadyOurs {
		t.Errorf("AddMCP = %v, %v; want MCPAlreadyOurs", state, err)
	}
	if len(r.calls) != 0 {
		t.Errorf("calls = %q, want none", r.calls)
	}
}

// foreignConfigs are Claude config files whose curia entry is not ours, or
// that Curia cannot read as a config at all.
var foreignConfigs = map[string]string{
	"other command":        `{"mcpServers":{"curia":{"command":"/usr/local/bin/curia","args":["mcp"]}}}`,
	"other args":           `{"mcpServers":{"curia":{"command":"` + testBin + `","args":["mcp","--debug"]}}}`,
	"no args":              `{"mcpServers":{"curia":{"command":"` + testBin + `"}}}`,
	"args not strings":     `{"mcpServers":{"curia":{"command":"` + testBin + `","args":[1]}}}`,
	"command in other key": `{"mcpServers":{"curia":{"COMMAND":"` + testBin + `","args":["mcp"]}}}`,
	"entry is a string":    `{"mcpServers":{"curia":"x"}}`,
	"entry is null":        `{"mcpServers":{"curia":null}}`,
	"mcpServers is array":  `{"mcpServers":[]}`,
	"mcpServers is null":   `{"mcpServers":null}`,
	"top level is array":   `[]`,
	"invalid JSON":         `{"mcpServers":`,
	"trailing garbage":     `{} {}`,
	"empty file":           ``,
}

func TestAddMCPForeign(t *testing.T) {
	for name, content := range foreignConfigs {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(t)
			state, err := AddMCP(context.Background(), r.run, configWith(t, content), testBin)
			if err != nil || state != MCPForeign {
				t.Errorf("AddMCP = %v, %v; want MCPForeign", state, err)
			}
			if len(r.calls) != 0 {
				t.Errorf("calls = %q, want none", r.calls)
			}
		})
	}
}

func TestAddMCPUnreadableConfigIsError(t *testing.T) {
	r := newRecorder(t)
	// A directory where the file should be makes reading it fail.
	path := filepath.Join(t.TempDir(), ".claude.json")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if state, err := AddMCP(context.Background(), r.run, path, testBin); err == nil {
		t.Errorf("AddMCP = %v, nil; want an error", state)
	}
	if state, err := RemoveMCP(context.Background(), r.run, path, testBin); err == nil {
		t.Errorf("RemoveMCP = %v, nil; want an error", state)
	}
	if len(r.calls) != 0 {
		t.Errorf("calls = %q, want none", r.calls)
	}
}

// notFound is what exec returns when claude is not on PATH.
var notFound = &exec.Error{Name: "claude", Err: exec.ErrNotFound}

func TestAddMCPNoClaude(t *testing.T) {
	r := newRecorder(t)
	r.err = notFound
	state, err := AddMCP(context.Background(), r.run, filepath.Join(t.TempDir(), ".claude.json"), testBin)
	if err != nil || state != MCPNoClaude {
		t.Errorf("AddMCP = %v, %v; want MCPNoClaude, nil", state, err)
	}
	r = newRecorder(t)
	r.err = notFound
	state, err = RemoveMCP(context.Background(), r.run, configWith(t, ourConfig(testBin)), testBin)
	if err != nil || state != MCPNoClaude {
		t.Errorf("RemoveMCP = %v, %v; want MCPNoClaude, nil", state, err)
	}
}

func TestRemoveMCPOnlyOurs(t *testing.T) {
	r := newRecorder(t)
	state, err := RemoveMCP(context.Background(), r.run, configWith(t, ourConfig(testBin)), testBin)
	if err != nil || state != MCPRemoved {
		t.Errorf("RemoveMCP(ours) = %v, %v; want MCPRemoved", state, err)
	}
	if want := []string{"mcp remove --scope user curia"}; !slices.Equal(r.calls, want) {
		t.Errorf("calls = %q, want %q", r.calls, want)
	}

	for name, content := range foreignConfigs {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(t)
			state, err := RemoveMCP(context.Background(), r.run, configWith(t, content), testBin)
			if err != nil || state != MCPForeign {
				t.Errorf("RemoveMCP = %v, %v; want MCPForeign", state, err)
			}
			if len(r.calls) != 0 {
				t.Errorf("calls = %q, want none", r.calls)
			}
		})
	}

	for name, path := range map[string]string{
		"missing file":   filepath.Join(t.TempDir(), ".claude.json"),
		"no mcpServers":  configWith(t, `{}`),
		"no curia entry": configWith(t, `{"mcpServers":{"other":{}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(t)
			state, err := RemoveMCP(context.Background(), r.run, path, testBin)
			if err != nil || state != MCPAbsent {
				t.Errorf("RemoveMCP = %v, %v; want MCPAbsent", state, err)
			}
			if len(r.calls) != 0 {
				t.Errorf("calls = %q, want none", r.calls)
			}
		})
	}
}

func TestRemoveMCPFailureIsError(t *testing.T) {
	r := newRecorder(t)
	r.err = errors.New("exit status 1")
	if state, err := RemoveMCP(context.Background(), r.run, configWith(t, ourConfig(testBin)), testBin); err == nil {
		t.Errorf("RemoveMCP = %v, nil; want an error when claude mcp remove fails", state)
	}
}

func TestBinWithSpacesIsOneArgument(t *testing.T) {
	const bin = "/my tools/curia"
	var args []string
	run := func(_ context.Context, a ...string) ([]byte, error) {
		args = a
		return nil, nil
	}
	if _, err := AddMCP(context.Background(), run, filepath.Join(t.TempDir(), ".claude.json"), bin); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "--")
	if i < 0 || i+3 != len(args) || args[i+1] != bin || args[i+2] != "mcp" {
		t.Errorf("add args = %q, want %q then \"mcp\" after \"--\"", args, bin)
	}
	path := configWith(t, ourConfig(bin))
	if state, err := AddMCP(context.Background(), run, path, bin); err != nil || state != MCPAlreadyOurs {
		t.Errorf("AddMCP with that command = %v, %v; want MCPAlreadyOurs", state, err)
	}
	if state, err := RemoveMCP(context.Background(), run, path, bin); err != nil || state != MCPRemoved {
		t.Errorf("RemoveMCP with that command = %v, %v; want MCPRemoved", state, err)
	}
}

// mcpConfigCases are every kind of Claude config the MCP tests use, by name;
// "" stands for a missing file.
func mcpConfigCases() map[string]string {
	cases := map[string]string{
		"missing file":   "",
		"ours":           ourConfig(testBin),
		"no mcpServers":  `{"numStartups": 3}`,
		"no curia entry": `{"mcpServers":{"other":{"command":"x","args":[]}}}`,
	}
	for name, content := range foreignConfigs {
		cases[name] = content
	}
	return cases
}

func TestNeverRunsMCPGetOrList(t *testing.T) {
	var calls []string
	for name, content := range mcpConfigCases() {
		for _, runErr := range []error{nil, notFound, errors.New("exit status 1")} {
			path := filepath.Join(t.TempDir(), ".claude.json")
			if content != "" {
				path = configWith(t, content)
			}
			r := newRecorder(t)
			r.err = runErr
			_, _ = AddMCP(context.Background(), r.run, path, testBin)
			_, _ = RemoveMCP(context.Background(), r.run, path, testBin)
			for _, c := range r.calls {
				calls = append(calls, name+": "+c)
			}
		}
	}
	var adds, removes int
	for _, c := range calls {
		_, args, _ := strings.Cut(c, ": ")
		switch {
		case strings.HasPrefix(args, "mcp get"), strings.HasPrefix(args, "mcp list"):
			t.Errorf("runner call %q starts the server", c)
		case strings.HasPrefix(args, "mcp add"):
			adds++
		case strings.HasPrefix(args, "mcp remove"):
			removes++
		default:
			t.Errorf("unexpected runner call %q", c)
		}
	}
	if adds == 0 || removes == 0 {
		t.Errorf("adds = %d, removes = %d; the cases should reach both", adds, removes)
	}
}

func TestConfigJSONNeverWritten(t *testing.T) {
	for name, content := range mcpConfigCases() {
		if content == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			path := configWith(t, content)
			// 0400 rather than Curia's 0600, so a rewrite would show in the mode.
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
			r := newRecorder(t)
			_, _ = AddMCP(context.Background(), r.run, path, testBin)
			_, _ = RemoveMCP(context.Background(), r.run, path, testBin)
			if got := readFile(t, path); got != content {
				t.Errorf("config changed: %q", got)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o400 {
				t.Errorf("config mode = %v, %v; want 0400", fi.Mode().Perm(), err)
			}
			if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
				t.Errorf("config dir holds %d entries, want only the config", len(entries))
			}
		})
	}
	// A missing config stays missing.
	path := filepath.Join(t.TempDir(), ".claude.json")
	r := newRecorder(t)
	_, _ = AddMCP(context.Background(), r.run, path, testBin)
	_, _ = RemoveMCP(context.Background(), r.run, path, testBin)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("missing config created: %v", err)
	}
}

func TestClaudeConfigPath(t *testing.T) {
	if got := ClaudeConfigPath("/h", "/c"); got != "/c/.claude.json" {
		t.Errorf(`ClaudeConfigPath("/h", "/c") = %q`, got)
	}
	if got := ClaudeConfigPath("/h", ""); got != "/h/.claude.json" {
		t.Errorf(`ClaudeConfigPath("/h", "") = %q`, got)
	}
}

func TestClaudeDir(t *testing.T) {
	if got := ClaudeDir("/h", "/c"); got != "/c" {
		t.Errorf(`ClaudeDir("/h", "/c") = %q`, got)
	}
	if got := ClaudeDir("/h", ""); got != "/h/.claude" {
		t.Errorf(`ClaudeDir("/h", "") = %q`, got)
	}
}

func TestMCPAddCommandQuotesBin(t *testing.T) {
	if got, want := MCPAddCommand("/my tools/curia"), "claude mcp add --scope user curia -- '/my tools/curia' mcp"; got != want {
		t.Errorf("MCPAddCommand = %q, want %q", got, want)
	}
}

// wantToolRules are the five permission rules AllowTools adds.
var wantToolRules = []string{
	"mcp__curia__search_decisions", "mcp__curia__request_decision",
	"mcp__curia__check_decision", "mcp__curia__withdraw_decision",
	"mcp__curia__check_messages",
}

// readAllow returns settings["permissions"]["allow"] as strings.
func readAllow(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, v := range asList(asMap(readSettings(t, path)["permissions"])["allow"]) {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func TestHooksAddNoPermissionRule(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, `{"permissions":{"allow":["Bash(ls:*)"]}}`)
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	if got := readAllow(t, path); !slices.Equal(got, []string{"Bash(ls:*)"}) {
		t.Errorf("allow = %q, want unchanged", got)
	}
}

func TestAllowToolsAddsFifthToAPlan2Install(t *testing.T) {
	path := settingsIn(t)
	writeFile(t, path, `{"model":"opus","permissions":{"allow":["Bash(ls:*)","mcp__curia__check_decision"],"deny":["Bash(rm:*)"]}}`)
	if err := AllowTools(path, testNow); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Bash(ls:*)", "mcp__curia__check_decision",
		"mcp__curia__search_decisions", "mcp__curia__request_decision", "mcp__curia__withdraw_decision",
		"mcp__curia__check_messages",
	}
	if got := readAllow(t, path); !slices.Equal(got, want) {
		t.Errorf("allow = %q, want %q", got, want)
	}
	once := readFile(t, path)
	if err := AllowTools(path, testNow.Add(1)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != once {
		t.Errorf("second AllowTools changed the file:\n%s", got)
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 1 {
		t.Errorf("backups = %v, want one", matches)
	}
	m := readSettings(t, path)
	if m["model"] != "opus" || len(asList(asMap(m["permissions"])["deny"])) != 1 {
		t.Errorf("other settings changed: %v", m)
	}
}

func TestAllowToolsCreatesMissingParts(t *testing.T) {
	for name, content := range map[string]string{
		"missing file":     "",
		"no permissions":   `{"model":"opus"}`,
		"no allow":         `{"permissions":{"deny":[]}}`,
		"empty allow list": `{"permissions":{"allow":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := settingsIn(t)
			if content != "" {
				writeFile(t, path, content)
			}
			if err := AllowTools(path, testNow); err != nil {
				t.Fatal(err)
			}
			if got := readAllow(t, path); !slices.Equal(got, wantToolRules) {
				t.Errorf("allow = %q, want %q", got, wantToolRules)
			}
		})
	}
}

func TestToolsAllowed(t *testing.T) {
	all, _ := json.Marshal(append([]string{"Bash(ls:*)"}, wantToolRules...))
	three, _ := json.Marshal(wantToolRules[:4])
	for content, want := range map[string]bool{
		`{"permissions":{"allow":` + string(all) + `}}`:   true,
		`{"permissions":{"allow":` + string(three) + `}}`: false,
		`{"model":"opus"}`:    false,
		`{"permissions":"x"}`: false,
		`{`:                   false,
	} {
		path := settingsIn(t)
		writeFile(t, path, content)
		if got := ToolsAllowed(path); got != want {
			t.Errorf("ToolsAllowed(%s) = %v, want %v", content, got, want)
		}
		if got := readFile(t, path); got != content {
			t.Errorf("ToolsAllowed changed the file: %q", got)
		}
	}
	if ToolsAllowed(filepath.Join(t.TempDir(), "absent.json")) {
		t.Error("ToolsAllowed on a missing file = true")
	}
}

func TestUnhooksRemovesTheFiveToolRules(t *testing.T) {
	path := settingsIn(t)
	allow, _ := json.Marshal(append([]string{"Bash(ls:*)", "mcp__curia"}, wantToolRules...))
	writeFile(t, path, `{"permissions":{"allow":`+string(allow)+`,"deny":["Bash(rm:*)"]}}`)
	if err := Hooks(path, testBin, testNow); err != nil {
		t.Fatal(err)
	}
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	if got := readAllow(t, path); !slices.Equal(got, []string{"Bash(ls:*)", "mcp__curia"}) {
		t.Errorf("allow = %q", got)
	}
	deny := asList(asMap(readSettings(t, path)["permissions"])["deny"])
	if len(deny) != 1 || deny[0] != "Bash(rm:*)" {
		t.Errorf("deny = %v, want untouched", deny)
	}
	if strings.Contains(readFile(t, path), " hook ") {
		t.Error("hooks left after Unhooks")
	}
}

func TestUnhooksToolRulesOnlyIsOneWrite(t *testing.T) {
	path := settingsIn(t)
	allow, _ := json.Marshal(wantToolRules)
	writeFile(t, path, `{"permissions":{"allow":`+string(allow)+`}}`)
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	if got, ok := asMap(m["permissions"])["allow"].([]any); !ok || len(got) != 0 {
		t.Errorf("allow = %v, want an empty array kept", asMap(m["permissions"])["allow"])
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 1 {
		t.Errorf("backups = %v, want one", matches)
	}
}

func TestPermissionsOfWrongTypeUntouched(t *testing.T) {
	for name, content := range map[string]string{
		"permissions is a string": `{"permissions": "x"}`,
		"permissions is null":     `{"permissions": null}`,
		"allow is an object":      `{"permissions": {"allow": {}}}`,
		"allow is a string":       `{"permissions": {"allow": "mcp__curia__check_decision"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := settingsIn(t)
			writeFile(t, path, content)
			if err := AllowTools(path, testNow); err == nil {
				t.Error("AllowTools: want error")
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

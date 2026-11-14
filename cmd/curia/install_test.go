package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ZachFara/curia/internal/install"
)

func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// installUnexpectedRunner is claudeRunner under test: no test may run the
// real claude, so a test that reaches the runner without installing a
// recorder fails.
func installUnexpectedRunner(_ context.Context, args ...string) ([]byte, error) {
	panic("claudeRunner called without a recorder: claude " + strings.Join(args, " "))
}

func init() {
	claudeRunner = installUnexpectedRunner
}

// installRecorder is a claudeRunner that records each call's arguments and
// succeeds.
type installRecorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *installRecorder) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	return nil, nil
}

func (r *installRecorder) joined() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// installRecord replaces claudeRunner with a recorder for the test.
func installRecord(t *testing.T) *installRecorder {
	t.Helper()
	r := &installRecorder{}
	claudeRunner = r.run
	t.Cleanup(func() { claudeRunner = installUnexpectedRunner })
	return r
}

// installRun runs curia with args, resetting the install flag before and
// after, because cobra keeps a flag's value between runs.
func installRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	installNoAllowMCPTools = false
	t.Cleanup(func() { installNoAllowMCPTools = false })
	return runRoot(t, args...)
}

// installHome is a temp HOME (and CLAUDE_CONFIG_DIR) and the paths in them.
type installHome struct {
	home, claudeDir, settings, skill, config, bin string
}

// installSetup points HOME and CLAUDE_CONFIG_DIR at temp dirs and writes
// settings, when not empty, to the settings file Claude reads.
func installSetup(t *testing.T, settings string) installHome {
	t.Helper()
	return installSetupDirs(t, settings, t.TempDir())
}

// installSetupDirs is installSetup with CLAUDE_CONFIG_DIR set to configDir;
// "" sets it empty, which counts as unset.
func installSetupDirs(t *testing.T, settings, configDir string) installHome {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	bin, err := executablePath()
	if err != nil {
		t.Fatal(err)
	}
	claudeDir, config := configDir, filepath.Join(configDir, ".claude.json")
	if configDir == "" {
		claudeDir, config = filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json")
	}
	h := installHome{
		home:      home,
		claudeDir: claudeDir,
		settings:  filepath.Join(claudeDir, "settings.json"),
		skill:     install.SkillPath(filepath.Join(claudeDir, "skills")),
		config:    config,
		bin:       bin,
	}
	if settings != "" {
		if err := os.MkdirAll(filepath.Dir(h.settings), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(h.settings, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// installAllow returns the settings file's permissions.allow list.
func installAllow(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m.Permissions.Allow
}

var installToolRules = []string{
	"mcp__curia__search_decisions", "mcp__curia__request_decision",
	"mcp__curia__check_decision", "mcp__curia__withdraw_decision",
	"mcp__curia__check_messages",
}

const installWantToolsHint = "MCP tools not allowed without a prompt; to allow the five curia tools, run curia install without --no-allow-mcp-tools"

const installWantAllowedLine = "MCP tools: the five curia tools are allowed without a prompt in %s; to skip this step, run curia install --no-allow-mcp-tools"

func installAddCall(bin string) string {
	return "mcp add --scope user curia -- " + bin + " mcp"
}

func TestInstallAndUninstallRoundTrip(t *testing.T) {
	installRecord(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")
	conf := filepath.Join(home, ".tmux.conf")
	snippet := filepath.Join(home, ".config", "curia", "curia.tmux")
	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	userSettings := `{"model":"opus"}`
	userConf := "set -g mouse on\n"
	for p, c := range map[string]string{settings: userSettings, conf: userConf} {
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	for _, want := range []string{"Restart", "tmux source-file ~/.tmux.conf"} {
		if !strings.Contains(out, want) {
			t.Errorf("install output missing %q:\n%s", want, out)
		}
	}
	got, _ := os.ReadFile(filepath.Clean(settings))
	if !strings.Contains(string(got), " hook SessionStart") {
		t.Errorf("settings missing hooks: %s", got)
	}
	if _, err := os.Stat(snippet); err != nil {
		t.Errorf("snippet not written: %v", err)
	}
	if baks, _ := filepath.Glob(settings + ".bak.*"); len(baks) != 2 {
		t.Errorf("backups = %v, want one from the hooks step and one from the tool rules", baks)
	}

	if out, err := installRun(t, "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	gotConf, _ := os.ReadFile(filepath.Clean(conf))
	if string(gotConf) != userConf {
		t.Errorf("conf = %q, want original", gotConf)
	}
	gotSettings, _ := os.ReadFile(filepath.Clean(settings))
	if strings.Contains(string(gotSettings), "hook ") || !strings.Contains(string(gotSettings), "opus") {
		t.Errorf("settings after uninstall: %s", gotSettings)
	}
	if _, err := os.Stat(snippet); !os.IsNotExist(err) {
		t.Errorf("snippet still present: %v", err)
	}
}

// Re-sourcing ~/.tmux.conf unsets nothing in a running server, so uninstall
// says how to undo the snippet instead.
func TestUninstallSaysHowToUndoTmux(t *testing.T) {
	installRecord(t)
	t.Setenv("HOME", t.TempDir())
	out, err := installRun(t, "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Restart tmux",
		"tmux set-hook -gu pane-focus-in; tmux set -gu @curia_marker_format\n",
		`for k in a A N; do tmux list-keys -T prefix "$k" 2>/dev/null | grep -q curia && tmux unbind "$k"; done` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("uninstall output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "source-file") {
		t.Errorf("uninstall output still says to re-source:\n%s", out)
	}
}

func TestInstallRefusesInvalidSettings(t *testing.T) {
	rec := installRecord(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := installRun(t, "install"); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(filepath.Join(home, ".tmux.conf")); !os.IsNotExist(err) {
		t.Errorf("tmux conf touched after settings failure: %v", err)
	}
	if calls := rec.joined(); len(calls) != 0 {
		t.Errorf("claude calls after settings failure = %q", calls)
	}
}

func TestInstallWritesSkillAndRegistersMCP(t *testing.T) {
	rec := installRecord(t)
	h := installSetup(t, "")
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(filepath.Clean(h.skill)); err != nil || !strings.Contains(string(b), "<!-- curia:managed -->") {
		t.Errorf("skill file = %q, %v", b, err)
	}
	if calls := rec.joined(); !slices.Equal(calls, []string{installAddCall(h.bin)}) {
		t.Errorf("claude calls = %q, want only the add", calls)
	}
	for _, want := range []string{"Hooks:", "tmux snippet:", "Skill:", "MCP server:"} {
		if !strings.Contains(out, want) {
			t.Errorf("install output does not name %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, h.skill) {
		t.Errorf("install output does not say where the skill went:\n%s", out)
	}
	if _, err := os.Stat(h.config); !os.IsNotExist(err) {
		t.Errorf("install wrote the Claude config: %v", err)
	}
}

// TestInstallThroughStubClaude runs the real claudeRunner against a stub:
// "claude" on PATH is a symlink to /bin/sh, so `claude mcp add ...` runs the
// script ./mcp from the working directory, which records its arguments.
func TestInstallThroughStubClaude(t *testing.T) {
	h := installSetup(t, "")
	stubDir, work := t.TempDir(), t.TempDir()
	if err := os.Symlink("/bin/sh", filepath.Join(stubDir, "claude")); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(work, "calls")
	script := "printf '%s\\n' \"$@\" >> '" + calls + "'\n"
	if err := os.WriteFile(filepath.Join(work, "mcp"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)
	t.Chdir(work)
	claudeRunner = installRunClaude
	t.Cleanup(func() { claudeRunner = installUnexpectedRunner })

	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Clean(calls))
	if err != nil {
		t.Fatalf("stub claude not run: %v\n%s", err, out)
	}
	want := strings.Join([]string{"add", "--scope", "user", "curia", "--", h.bin, "mcp"}, "\n") + "\n"
	if string(got) != want {
		t.Errorf("stub claude got arguments %q, want %q", got, want)
	}
}

func TestInstallReportsMissingClaude(t *testing.T) {
	h := installSetup(t, "")
	t.Setenv("PATH", t.TempDir())
	claudeRunner = installRunClaude
	t.Cleanup(func() { claudeRunner = installUnexpectedRunner })
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if want := "claude mcp add --scope user curia -- " + h.bin + " mcp"; !strings.Contains(out, want) {
		t.Errorf("install output does not say to run %q by hand:\n%s", want, out)
	}
}

func TestUninstallReversesPlan2Items(t *testing.T) {
	rec := installRecord(t)
	h := installSetup(t, `{"permissions":{"allow":["Bash(ls:*)"]}}`)
	if out, err := installRun(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := installAllow(t, h.settings); len(got) != 6 {
		t.Fatalf("allow after install = %q", got)
	}
	// What claude mcp add writes, which the recorder does not.
	entry := `{"mcpServers":{"curia":{"type":"stdio","command":` + installJSONString(h.bin) + `,"args":["mcp"],"env":{}}}}`
	if err := os.WriteFile(h.config, []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := installRun(t, "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Dir(h.skill)); !os.IsNotExist(err) {
		t.Errorf("skill dir still there: %v", err)
	}
	if got := installAllow(t, h.settings); !slices.Equal(got, []string{"Bash(ls:*)"}) {
		t.Errorf("allow after uninstall = %q, want as before", got)
	}
	want := []string{installAddCall(h.bin), "mcp remove --scope user curia"}
	if calls := rec.joined(); !slices.Equal(calls, want) {
		t.Errorf("claude calls = %q, want %q", calls, want)
	}
	if b, _ := os.ReadFile(filepath.Clean(h.config)); string(b) != entry {
		t.Errorf("uninstall wrote the Claude config: %s", b)
	}
}

// installJSONString returns s as a JSON string.
func installJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestInstallAllowsToolsByDefault(t *testing.T) {
	installRecord(t)
	h := installSetup(t, "")
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := installAllow(t, h.settings); !slices.Equal(got, installToolRules) {
		t.Errorf("allow = %q, want exactly the five rules", got)
	}
	if want := fmt.Sprintf(installWantAllowedLine, h.settings) + "\n"; strings.Count(out, want) != 1 {
		t.Errorf("install output lacks the line %q:\n%s", want, out)
	}
	if strings.Contains(out, installWantToolsHint) {
		t.Errorf("install printed the hint:\n%s", out)
	}
}

func TestInstallNoAllowFlagSkipsRules(t *testing.T) {
	installRecord(t)
	h := installSetup(t, `{"permissions":{"allow":["Bash(ls:*)"]}}`)
	out, err := installRun(t, "install", "--no-allow-mcp-tools")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := installAllow(t, h.settings); !slices.Equal(got, []string{"Bash(ls:*)"}) {
		t.Errorf("allow with the flag = %q, want unchanged", got)
	}
	if strings.Count(out, installWantToolsHint+"\n") != 1 {
		t.Errorf("install output lacks the hint line %q:\n%s", installWantToolsHint, out)
	}
	if out, err = installRun(t, "install", "--allow-mcp-tools"); err == nil {
		t.Errorf("the removed --allow-mcp-tools flag was accepted:\n%s", out)
	}
}

func TestInstallDefaultIsIdempotent(t *testing.T) {
	installRecord(t)
	h := installSetup(t, `{"permissions":{"allow":["Bash(ls:*)"]}}`)
	if out, err := installRun(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	before, _ := os.ReadFile(filepath.Clean(h.settings))
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	if after, _ := os.ReadFile(filepath.Clean(h.settings)); string(after) != string(before) {
		t.Errorf("second install rewrote settings:\n%s", after)
	}
	if !strings.Contains(out, "already allowed without a prompt in "+h.settings) || strings.Contains(out, installWantToolsHint) {
		t.Errorf("second install output:\n%s", out)
	}
}

func TestInstallKeepsUsersOwnRules(t *testing.T) {
	installRecord(t)
	h := installSetup(t, `{"permissions":{"allow":["mcp__curia",42,{"a":1},"mcp__curia__check_decision","Bash(ls:*)"]}}`)
	if out, err := installRun(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	read := func() []any {
		b, err := os.ReadFile(filepath.Clean(h.settings))
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Permissions struct {
				Allow []any `json:"allow"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m.Permissions.Allow
	}
	got := read()
	if len(got) != 9 || got[0] != "mcp__curia" || got[1] != float64(42) || got[4] != "Bash(ls:*)" {
		t.Errorf("allow after install = %v", got)
	}
	if out, err := installRun(t, "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	got = read()
	if len(got) != 4 || got[0] != "mcp__curia" || got[1] != float64(42) || got[3] != "Bash(ls:*)" {
		t.Errorf("allow after uninstall = %v", got)
	}
}

func TestInstallBadPermissionsShape(t *testing.T) {
	for _, settings := range []string{`{"permissions":"nope"}`, `{"permissions":{"allow":{"a":1}}}`} {
		rec := installRecord(t)
		h := installSetup(t, settings)
		out, err := installRun(t, "install")
		if err == nil {
			t.Fatalf("install with %s succeeded:\n%s", settings, out)
		}
		if !strings.Contains(out, "Hooks: registered") || strings.Contains(out, "tmux snippet:") {
			t.Errorf("install output with %s:\n%s", settings, out)
		}
		b, _ := os.ReadFile(filepath.Clean(h.settings))
		if !strings.Contains(string(b), " hook SessionStart") || strings.Contains(string(b), "mcp__curia") {
			t.Errorf("settings with %s after install: %s", settings, b)
		}
		if baks, _ := filepath.Glob(h.settings + ".bak.*"); len(baks) != 1 {
			t.Errorf("backups = %v, want only the hooks step's", baks)
		}
		if calls := rec.joined(); len(calls) != 0 {
			t.Errorf("claude calls after the failed step = %q", calls)
		}
	}
}

func TestInstallForeignSkillContinues(t *testing.T) {
	rec := installRecord(t)
	h := installSetup(t, "")
	if err := os.MkdirAll(filepath.Dir(h.skill), 0o750); err != nil {
		t.Fatal(err)
	}
	foreign := "my own skill\n"
	if err := os.WriteFile(h.skill, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, h.skill) || !strings.Contains(out, "left alone") {
		t.Errorf("foreign skill not reported:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Clean(h.skill)); string(b) != foreign {
		t.Errorf("foreign skill changed: %q", b)
	}
	if calls := rec.joined(); !slices.Equal(calls, []string{installAddCall(h.bin)}) {
		t.Errorf("claude calls = %q, want the add", calls)
	}
}

func TestInstallForeignMCPEntryContinues(t *testing.T) {
	rec := installRecord(t)
	h := installSetup(t, "")
	foreign := `{"mcpServers":{"curia":{"command":"/elsewhere/curia","args":["mcp"]}}}`
	if err := os.WriteFile(h.config, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"install"}, {"uninstall"}} {
		out, err := installRun(t, args...)
		if err != nil {
			t.Fatalf("%s: %v\n%s", args[0], err, out)
		}
		if !strings.Contains(out, h.config) || !strings.Contains(out, "left alone") {
			t.Errorf("%s: foreign MCP entry not reported:\n%s", args[0], out)
		}
		if args[0] == "install" && !strings.Contains(out, "run: claude mcp remove --scope user curia, then curia install again") {
			t.Errorf("install does not say how to recover:\n%s", out)
		}
	}
	if calls := rec.joined(); len(calls) != 0 {
		t.Errorf("claude calls = %q, want none", calls)
	}
}

func TestUninstallReportsSkill(t *testing.T) {
	installRecord(t)
	h := installSetup(t, "")
	if out, err := installRun(t, "install"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	out, err := installRun(t, "uninstall")
	if err != nil || !strings.Contains(out, "Skill: removed "+h.skill) {
		t.Errorf("uninstall of our skill: %v\n%s", err, out)
	}

	if err = os.MkdirAll(filepath.Dir(h.skill), 0o750); err != nil {
		t.Fatal(err)
	}
	foreign := "my own skill\n"
	if err = os.WriteFile(h.skill, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = installRun(t, "uninstall")
	if err != nil {
		t.Fatalf("uninstall with a foreign skill: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Skill: "+h.skill+" is not Curia's; left alone") || !strings.Contains(out, "Curia is uninstalled") {
		t.Errorf("foreign skill not reported:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Clean(h.skill)); string(b) != foreign {
		t.Errorf("foreign skill changed: %q", b)
	}
}

// installCheckClaudeDir installs and uninstalls, and
// checks that hooks, tool rules and the skill land in h.claudeDir and
// nowhere else under HOME, and that uninstall reverses them.
func installCheckClaudeDir(t *testing.T, h installHome) {
	t.Helper()
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Hooks: registered in "+h.settings+"\n") {
		t.Errorf("install output does not name the settings path %s:\n%s", h.settings, out)
	}
	settings, err := os.ReadFile(filepath.Clean(h.settings))
	if err != nil || !strings.Contains(string(settings), " hook SessionStart") {
		t.Errorf("hooks not in %s: %v %s", h.settings, err, settings)
	}
	if got := installAllow(t, h.settings); !slices.Equal(got, append([]string{"Bash(ls:*)"}, installToolRules...)) {
		t.Errorf("allow = %q", got)
	}
	if _, err := os.Stat(h.skill); err != nil {
		t.Errorf("skill not at %s: %v", h.skill, err)
	}
	if h.claudeDir != filepath.Join(h.home, ".claude") {
		if _, err := os.Stat(filepath.Join(h.home, ".claude")); !os.IsNotExist(err) {
			t.Errorf("install created %s/.claude: %v", h.home, err)
		}
	}

	if out, err := installRun(t, "uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	settings, _ = os.ReadFile(filepath.Clean(h.settings))
	if strings.Contains(string(settings), " hook ") {
		t.Errorf("hooks left in %s: %s", h.settings, settings)
	}
	if got := installAllow(t, h.settings); !slices.Equal(got, []string{"Bash(ls:*)"}) {
		t.Errorf("allow after uninstall = %q", got)
	}
	if _, err := os.Stat(filepath.Dir(h.skill)); !os.IsNotExist(err) {
		t.Errorf("skill dir left: %v", err)
	}
	if h.claudeDir != filepath.Join(h.home, ".claude") {
		if _, err := os.Stat(filepath.Join(h.home, ".claude")); !os.IsNotExist(err) {
			t.Errorf("uninstall created %s/.claude: %v", h.home, err)
		}
	}
}

// Ruling 56: with CLAUDE_CONFIG_DIR set, settings.json and skills/ live in it.
func TestInstallUsesClaudeConfigDir(t *testing.T) {
	rec := installRecord(t)
	h := installSetup(t, `{"permissions":{"allow":["Bash(ls:*)"]}}`)
	installCheckClaudeDir(t, h)
	if calls := rec.joined(); !slices.Equal(calls, []string{installAddCall(h.bin)}) {
		t.Errorf("claude calls = %q", calls)
	}
}

// Ruling 56: with CLAUDE_CONFIG_DIR empty, they live in $HOME/.claude.
func TestInstallWithoutClaudeConfigDir(t *testing.T) {
	installRecord(t)
	h := installSetupDirs(t, `{"permissions":{"allow":["Bash(ls:*)"]}}`, "")
	if h.claudeDir != filepath.Join(h.home, ".claude") {
		t.Fatalf("claudeDir = %s", h.claudeDir)
	}
	installCheckClaudeDir(t, h)
}

// installThemedHome is a temp HOME whose ~/.tmux.conf loads a theme and
// binds prefix a to another tool, with the snippet from before the fix in
// place, and the config.toml body given (none when empty).
func installThemedHome(t *testing.T, configBody string) installHome {
	t.Helper()
	h := installSetup(t, "")
	writeTestFile(t, filepath.Join(h.home, ".tmux.conf"),
		"set -g prefix C-s\nbind a display-popup -E -w 80% -h 80% recon\nrun '~/.tmux/plugins/tpm/tpm'\n")
	writeTestFile(t, filepath.Join(h.home, ".config", "curia", "curia.tmux"),
		"set -g focus-events on\nset -g window-status-format '#{?#{==:#{@curia_marker},red},#[fg=red]● #[default],}#I:#W'\n")
	if configBody != "" {
		writeTestFile(t, filepath.Join(h.home, ".config", "curia", "config.toml"), configBody)
	}
	return h
}

// writeTestFile writes content to path, creating its directory.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Install says, per key, whether the snippet binds it or leaves a key the
// user's configuration binds already (with the line to bind another), that
// the curia block must stay last, and, after the old snippet, that a running
// tmux keeps the plain formats it set.
func TestInstallSaysWhatTheTmuxSnippetDoes(t *testing.T) {
	installRecord(t)
	h := installThemedHome(t, "")
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	conf, cfg := filepath.Join(h.home, ".tmux.conf"), filepath.Join(h.home, ".config", "curia", "config.toml")
	want := "tmux snippet: written to " + filepath.Join(h.home, ".config", "curia", "curia.tmux") + " and sourced from " + conf + "\n" +
		"  prefix a (dashboard): already bound in " + conf + ", left alone; to use another key, set key_dashboard in " + cfg +
		" and run curia install again, or add after the curia block: bind <key> display-popup -E -w 90% -h 80% '" + h.bin + " ui'\n" +
		"  prefix A (decisions): bound by the snippet\n" +
		"  prefix N (next): bound by the snippet\n" +
		"  The snippet binds a key only if it is free (or already Curia's) when tmux loads it, and adds a marker to your " +
		"window formats: keep the curia block last in " + conf + ", after any theme or plugin line.\n" +
		"  The previous snippet replaced your window formats with plain ones; a running tmux keeps them until it restarts " +
		"or your theme sets them again.\n"
	if !strings.Contains(out, want) {
		t.Errorf("install output:\n%s\nwant it to contain:\n%s", out, want)
	}

	out, err = installRun(t, "install")
	if err != nil {
		t.Fatalf("second install: %v\n%s", err, out)
	}
	if strings.Contains(out, "previous snippet") {
		t.Errorf("second install still reports the old snippet:\n%s", out)
	}
}

// The keys come from config.toml: an empty one is reported with the line to
// bind it by hand, and uninstall's unbind loop names the configured keys.
func TestInstallUsesConfiguredTmuxKeys(t *testing.T) {
	installRecord(t)
	h := installThemedHome(t, "key_decisions = \"\"\nkey_next = \"C-y\"\n")
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	cfg := filepath.Join(h.home, ".config", "curia", "config.toml")
	for _, want := range []string{
		"  decisions: no key (key_decisions is empty in " + cfg + "); to bind one, add: bind <key> display-popup -E -w 90% -h 80% '" + h.bin + " decide'\n",
		"  prefix C-y (next): bound by the snippet\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install output missing %q:\n%s", want, out)
		}
	}
	snip, err := os.ReadFile(filepath.Join(h.home, ".config", "curia", "curia.tmux"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snip), "list-keys -T prefix C-y ") || strings.Contains(string(snip), " decide'") {
		t.Errorf("snippet does not follow the configured keys:\n%s", snip)
	}

	out, err = installRun(t, "uninstall")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if want := "for k in a C-y; do"; !strings.Contains(out, want) {
		t.Errorf("uninstall output missing %q:\n%s", want, out)
	}
}

// A configured key tmux could not take safely stops install with the
// option's name.
func TestInstallRefusesBadTmuxKey(t *testing.T) {
	installRecord(t)
	installThemedHome(t, "key_next = \"a;b\"\n")
	out, err := installRun(t, "install")
	if err == nil || !strings.Contains(err.Error(), "key_next") {
		t.Fatalf("install: err = %v, want one naming key_next\n%s", err, out)
	}
}

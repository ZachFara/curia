package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	installAllowMCPTools = false
	t.Cleanup(func() { installAllowMCPTools = false })
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
}

const installWantToolsHint = "MCP tools not allowed without a prompt; to allow the four curia tools run: curia install --allow-mcp-tools"

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
	if baks, _ := filepath.Glob(settings + ".bak.*"); len(baks) != 1 {
		t.Errorf("backups = %v", baks)
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
	for _, want := range []string{"Restart tmux", "tmux set-hook -gu pane-focus-in; tmux unbind a; tmux unbind N; tmux unbind A"} {
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
	if out, err := installRun(t, "install", "--allow-mcp-tools"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := installAllow(t, h.settings); len(got) != 5 {
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

func TestInstallAllowsToolsOnlyWithFlag(t *testing.T) {
	installRecord(t)
	h := installSetup(t, `{"permissions":{"allow":["Bash(ls:*)"]}}`)
	out, err := installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if got := installAllow(t, h.settings); !slices.Equal(got, []string{"Bash(ls:*)"}) {
		t.Errorf("allow without the flag = %q, want unchanged", got)
	}
	if strings.Count(out, installWantToolsHint+"\n") != 1 {
		t.Errorf("install output lacks the hint line %q:\n%s", installWantToolsHint, out)
	}

	out, err = installRun(t, "install", "--allow-mcp-tools")
	if err != nil {
		t.Fatalf("install --allow-mcp-tools: %v\n%s", err, out)
	}
	if got := installAllow(t, h.settings); !slices.Equal(got, append([]string{"Bash(ls:*)"}, installToolRules...)) {
		t.Errorf("allow with the flag = %q", got)
	}
	if strings.Contains(out, installWantToolsHint) || !strings.Contains(out, "without a prompt") {
		t.Errorf("install --allow-mcp-tools output:\n%s", out)
	}

	// All four there: a plain install says so instead of the hint (Ruling 57).
	before, _ := os.ReadFile(filepath.Clean(h.settings))
	out, err = installRun(t, "install")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if strings.Contains(out, installWantToolsHint) || !strings.Contains(out, "already allowed without a prompt in "+h.settings) {
		t.Errorf("install with the four rules present:\n%s", out)
	}
	if after, _ := os.ReadFile(filepath.Clean(h.settings)); string(after) != string(before) {
		t.Errorf("plain install rewrote settings:\n%s", after)
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

// installCheckClaudeDir installs with --allow-mcp-tools and uninstalls, and
// checks that hooks, tool rules and the skill land in h.claudeDir and
// nowhere else under HOME, and that uninstall reverses them.
func installCheckClaudeDir(t *testing.T, h installHome) {
	t.Helper()
	out, err := installRun(t, "install", "--allow-mcp-tools")
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

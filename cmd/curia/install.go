package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/install"
)

// installNoAllowMCPTools is curia install's --no-allow-mcp-tools flag.
var installNoAllowMCPTools bool

// claudeRunner runs the claude CLI for curia install and uninstall; tests
// replace it, and no test runs the real claude.
var claudeRunner install.Runner = installRunClaude

const (
	// installClaudeTimeout bounds one claude invocation.
	installClaudeTimeout = 30 * time.Second
	// installClaudeWaitDelay bounds the wait for claude's output after the
	// timeout kills it, should a child it started keep the pipe open.
	installClaudeWaitDelay = 3 * time.Second
)

// installRunClaude runs claude from PATH with args and returns its combined
// output.
func installRunClaude(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, installClaudeTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "claude", args...)
	c.WaitDelay = installClaudeWaitDelay
	return c.CombinedOutput()
}

// installToolsHint is what install prints when --no-allow-mcp-tools skipped
// the rules.
const installToolsHint = "MCP tools not allowed without a prompt; to allow the five curia tools, run curia install without --no-allow-mcp-tools"

// installPaths are the user-owned files the install commands edit, and the
// Claude config JSON they only read.
type installPaths struct {
	settings, snippet, tmuxConf, skills, claudeConfig string
	// config is Curia's own configuration, which install only reads.
	config string
}

func homePaths() (installPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return installPaths{}, err
	}
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	claudeDir := install.ClaudeDir(home, configDir)
	return installPaths{
		settings:     filepath.Join(claudeDir, "settings.json"),
		snippet:      filepath.Join(home, ".config", "curia", "curia.tmux"),
		tmuxConf:     filepath.Join(home, ".tmux.conf"),
		skills:       filepath.Join(claudeDir, "skills"),
		claudeConfig: install.ClaudeConfigPath(home, configDir),
		config:       configFile(home),
	}, nil
}

// binaryPath is the path install records for this binary.
type binaryPath struct {
	// path is absolute, cleaned and names the running binary.
	path string
	// onPath is true when path is the curia found on PATH.
	onPath bool
}

// sameFile reports whether a and b, symlinks followed, are one file.
func sameFile(a, b string) bool {
	fa, err := os.Stat(filepath.Clean(a))
	if err != nil {
		return false
	}
	fb, err := os.Stat(filepath.Clean(b))
	return err == nil && os.SameFile(fa, fb)
}

// chooseBinary picks the path install records, the one the user would type
// (Ruling 104), so a symlink that a package manager keeps pointing at the
// current version survives an upgrade. exe is os.Executable's result. In
// order:
//  1. the curia that lookPath finds (made absolute and cleaned, not
//     resolved), when it is the same file as the running binary;
//  2. exe as invoked (absolute, cleaned, not resolved), when it is that file;
//  3. exe with symlinks resolved.
//
// A path from anywhere else is never recorded, and every result exists.
func chooseBinary(exe string, lookPath func(string) (string, error)) (binaryPath, error) {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return binaryPath{}, err
	}
	if found, err := lookPath("curia"); err == nil {
		if abs, err := filepath.Abs(found); err == nil && sameFile(abs, resolved) {
			return binaryPath{path: abs, onPath: true}, nil
		}
	}
	if abs, err := filepath.Abs(exe); err == nil && sameFile(abs, resolved) {
		return binaryPath{path: abs}, nil
	}
	return binaryPath{path: resolved}, nil
}

// currentBinary is chooseBinary for the running executable and PATH.
func currentBinary() (binaryPath, error) {
	exe, err := os.Executable()
	if err != nil {
		return binaryPath{}, err
	}
	return chooseBinary(exe, exec.LookPath)
}

// executablePath is the path install records for this binary.
func executablePath() (string, error) {
	b, err := currentBinary()
	return b.path, err
}

// installTarget returns the paths and this binary's recorded path.
func installTarget() (installPaths, binaryPath, error) {
	p, err := homePaths()
	if err != nil {
		return installPaths{}, binaryPath{}, err
	}
	bin, err := currentBinary()
	return p, bin, err
}

// installSayBinary says which path install recorded and, for one that a
// move or an upgrade would break, how to avoid that.
func installSayBinary(w io.Writer, b binaryPath) {
	installSay(w, "Binary: recorded %s", b.path)
	if !b.onPath || strings.Contains(b.path, "/Cellar/") {
		installSay(w, "  Hooks, the MCP server and the tmux bindings will break if that file moves or is replaced; "+
			"to avoid that, put curia on PATH (a stable symlink such as /opt/homebrew/bin/curia) and run curia install again.")
	}
}

// installSay prints one line of what install or uninstall did.
func installSay(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// runInstall does each step in order and says what it did; a step that
// fails stops it, after the lines for the steps already done.
func runInstall(cmd *cobra.Command, _ []string) error {
	p, target, err := installTarget()
	if err != nil {
		return err
	}
	bin := target.path
	out := cmd.OutOrStdout()
	installSayBinary(out, target)
	if err = install.Hooks(p.settings, bin, time.Now()); err != nil {
		return err
	}
	installSay(out, "Hooks: registered in %s", p.settings)
	if err = installTools(out, p.settings); err != nil {
		return err
	}
	if err = installTmux(out, p, bin); err != nil {
		return err
	}
	if err = installSkill(out, p.skills); err != nil {
		return err
	}
	if err = installMCP(cmd.Context(), out, p.claudeConfig, bin); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Curia is installed.\n"+
		"Next steps:\n"+
		"  1. Restart any running Claude Code sessions so they pick up the hooks, skill and MCP server.\n"+
		"  2. Run: tmux source-file ~/.tmux.conf")
	return err
}

// tmuxKeys are the snippet's keys from Curia's configuration.
func tmuxKeys() (install.TmuxKeys, error) {
	cfg, err := loadConfig()
	if err != nil {
		return install.TmuxKeys{}, err
	}
	return install.TmuxKeys{Dashboard: cfg.KeyDashboard, Decisions: cfg.KeyDecisions, Next: cfg.KeyNext, Message: cfg.KeyMessage}, nil
}

// installTmux writes the snippet and says, per key, whether the snippet binds
// it or leaves it to the user, and that the curia block must stay last.
func installTmux(out io.Writer, p installPaths, bin string) error {
	keys, err := tmuxKeys()
	if err != nil {
		return err
	}
	res, err := install.WriteTmux(p.snippet, p.tmuxConf, bin, keys)
	if err != nil {
		return err
	}
	installSay(out, "tmux snippet: written to %s and sourced from %s", p.snippet, p.tmuxConf)
	for _, k := range res.Keys {
		switch {
		case k.Key == "":
			installSay(out, "  %s: no key (%s is empty in %s); to bind one, add: bind <key> %s",
				k.Name, k.Option, p.config, k.Command)
		case k.Taken:
			installSay(out, "  prefix %s (%s): already bound in %s, left alone; to use another key, set %s in %s "+
				"and run curia install again, or add after the curia block: bind <key> %s",
				k.Key, k.Name, p.tmuxConf, k.Option, p.config, k.Command)
		case k.Default != "":
			installSay(out, "  prefix %s (%s): bound by the snippet, in place of tmux's own %q (curia uninstall restores it)",
				k.Key, k.Name, k.Default)
		default:
			installSay(out, "  prefix %s (%s): bound by the snippet", k.Key, k.Name)
		}
	}
	installSay(out, "  The snippet binds a key only if it is free (or already Curia's) when tmux loads it, "+
		"and adds a marker to your window formats: keep the curia block last in %s, after any theme or plugin line.",
		p.tmuxConf)
	if res.OldSnippet {
		installSay(out, "  The previous snippet replaced your window formats with plain ones; a running tmux keeps them "+
			"until it restarts or your theme sets them again.")
	}
	return nil
}

// installTools adds the five tool rules unless --no-allow-mcp-tools was given
// (Ruling 78); with the flag it says how to allow them, unless all five are
// there already (Ruling 57).
func installTools(out io.Writer, settings string) error {
	if installNoAllowMCPTools {
		if install.ToolsAllowed(settings) {
			installSay(out, "MCP tools: the five curia tools are already allowed without a prompt in %s", settings)
		} else {
			installSay(out, "%s", installToolsHint)
		}
		return nil
	}
	if install.ToolsAllowed(settings) {
		installSay(out, "MCP tools: the five curia tools are already allowed without a prompt in %s", settings)
		return nil
	}
	if err := install.AllowTools(settings, time.Now()); err != nil {
		return err
	}
	installSay(out, "MCP tools: the five curia tools are allowed without a prompt in %s; "+
		"to skip this step, run curia install --no-allow-mcp-tools", settings)
	return nil
}

// installSkill writes the skill; a SKILL.md that is not Curia's is reported
// and left alone.
func installSkill(out io.Writer, skills string) error {
	path := install.SkillPath(skills)
	err := install.WriteSkill(skills)
	if errors.Is(err, install.ErrForeignSkill) {
		installSay(out, "Skill: %s exists and is not Curia's; left alone", path)
		return nil
	}
	if err != nil {
		return err
	}
	installSay(out, "Skill: written to %s", path)
	return nil
}

// installMCP registers the MCP server and says what it found.
func installMCP(ctx context.Context, out io.Writer, config, bin string) error {
	state, err := install.AddMCP(ctx, claudeRunner, config, bin)
	if err != nil {
		return err
	}
	switch state {
	case install.MCPAdded:
		installSay(out, "MCP server: registered with Claude Code as curia")
	case install.MCPAlreadyOurs:
		installSay(out, "MCP server: already registered with Claude Code as curia")
	case install.MCPUpdated:
		installSay(out, "MCP server: curia entry pointed at another path for this binary; re-registered with %s", bin)
	case install.MCPForeign:
		installSay(out, "MCP server: %s has a curia entry that is not this binary's, or is not valid JSON; left alone. "+
			"To replace it, run: claude mcp remove --scope user curia, then curia install again", config)
	case install.MCPNoClaude:
		installSay(out, "MCP server: claude is not on PATH; to register it, run: %s", install.MCPAddCommand(bin))
	}
	return nil
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	p, target, err := installTarget()
	if err != nil {
		return err
	}
	bin := target.path
	if err = install.Unhooks(p.settings, bin); err != nil {
		return err
	}
	if err = install.RemoveTmux(p.tmuxConf); err != nil {
		return err
	}
	if err = os.Remove(p.snippet); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = uninstallSkill(cmd.OutOrStdout(), p.skills); err != nil {
		return err
	}
	if err = uninstallMCP(cmd.Context(), cmd.OutOrStdout(), p.claudeConfig, bin); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Curia is uninstalled.\n"+
		"Restart running Claude Code sessions.\n"+uninstallTmuxHint())
	return err
}

// uninstallTmuxHint says how to undo the snippet in a running server, since
// re-sourcing ~/.tmux.conf unsets nothing: unset the hook, unset the option
// the window formats refer to (the marker fragment left in them then shows
// nothing) and unbind each of Curia's keys that is still Curia's. Keys come
// from the configuration, or its defaults when it does not load.
func uninstallTmuxHint() string {
	keys, err := tmuxKeys()
	if err != nil {
		d := config.Default()
		keys = install.TmuxKeys{Dashboard: d.KeyDashboard, Decisions: d.KeyDecisions, Next: d.KeyNext, Message: d.KeyMessage}
	}
	var names []string
	for _, k := range []string{keys.Dashboard, keys.Decisions, keys.Next} {
		if install.ValidKey(k) {
			names = append(names, k)
		}
	}
	// Unlike the others, tmux binds the message key by default (marking a
	// pane): while it is still Curia's, uninstall gives that binding back.
	restore := keys.Message == install.MessageDefaultKey
	if !restore && install.ValidKey(keys.Message) {
		names = append(names, keys.Message)
	}
	hint := "Restart tmux to drop Curia's key bindings, hook and window marker, or run:\n" +
		"  tmux set-hook -gu pane-focus-in; tmux set -gu @curia_marker_format"
	if len(names) > 0 {
		hint += "\n  for k in " + strings.Join(names, " ") + "; do " +
			`tmux list-keys -T prefix "$k" 2>/dev/null | grep -q curia && tmux unbind "$k"; done`
	}
	if restore {
		hint += "\n  tmux list-keys -T prefix " + keys.Message + " 2>/dev/null | grep -q curia && tmux bind " +
			keys.Message + " " + install.MessageDefaultCommand +
			"\n  (the last line gives prefix " + keys.Message + " back to tmux's own binding, which marks a pane)"
	}
	return hint
}

// uninstallSkill removes the skill if it is ours and says what it found; a
// SKILL.md that is not Curia's is reported and left alone.
func uninstallSkill(out io.Writer, skills string) error {
	path := install.SkillPath(skills)
	_, statErr := os.Lstat(path)
	err := install.RemoveSkill(skills)
	switch {
	case errors.Is(err, install.ErrForeignSkill):
		installSay(out, "Skill: %s is not Curia's; left alone", path)
		return nil
	case err != nil:
		return err
	case statErr != nil:
		installSay(out, "Skill: not installed; nothing to remove")
	default:
		installSay(out, "Skill: removed %s", path)
	}
	return nil
}

// uninstallMCP removes the MCP server if it is ours and says what it found.
func uninstallMCP(ctx context.Context, out io.Writer, config, bin string) error {
	state, err := install.RemoveMCP(ctx, claudeRunner, config, bin)
	if err != nil {
		return err
	}
	switch state {
	case install.MCPRemoved:
		installSay(out, "MCP server: removed from Claude Code")
	case install.MCPAbsent:
		installSay(out, "MCP server: not registered; nothing to remove")
	case install.MCPForeign:
		installSay(out, "MCP server: %s has a curia entry that is not this binary's, or is not valid JSON; left alone. "+
			"To remove it anyway, run: claude mcp remove --scope user curia", config)
	case install.MCPNoClaude:
		installSay(out, "MCP server: claude is not on PATH; to remove it, run: claude mcp remove --scope user curia")
	}
	return nil
}

func init() {
	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Register Curia's hooks, skill and MCP server with Claude Code and add the tmux snippet",
		Args:  cobra.NoArgs,
		RunE:  runInstall,
	}
	installCmd.Flags().BoolVar(&installNoAllowMCPTools, "no-allow-mcp-tools", false,
		"do not add the five curia MCP tools to Claude's permissions.allow (they are allowed by default)")
	rootCmd.AddCommand(
		installCmd,
		&cobra.Command{
			Use:   "uninstall",
			Short: "Remove what curia install added",
			Args:  cobra.NoArgs,
			RunE:  runUninstall,
		},
	)
}

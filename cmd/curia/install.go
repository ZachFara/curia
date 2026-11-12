package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/install"
)

// installAllowMCPTools is curia install's --allow-mcp-tools flag.
var installAllowMCPTools bool

// claudeRunner runs the claude CLI for curia install and uninstall; tests
// replace it, and no test runs the real claude.
var claudeRunner install.Runner = installRunClaude

// installClaudeTimeout bounds one claude invocation.
const installClaudeTimeout = 30 * time.Second

// installRunClaude runs claude from PATH with args and returns its combined
// output.
func installRunClaude(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, installClaudeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "claude", args...).CombinedOutput()
}

// installToolsHint is what install prints when --allow-mcp-tools is not given.
const installToolsHint = "MCP tools not allowed without a prompt; to allow the four curia tools run: curia install --allow-mcp-tools"

// installPaths are the user-owned files the install commands edit, and the
// Claude config JSON they only read.
type installPaths struct {
	settings, snippet, tmuxConf, skills, claudeConfig string
}

func homePaths() (installPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return installPaths{}, err
	}
	return installPaths{
		settings:     filepath.Join(home, ".claude", "settings.json"),
		snippet:      filepath.Join(home, ".config", "curia", "curia.tmux"),
		tmuxConf:     filepath.Join(home, ".tmux.conf"),
		skills:       filepath.Join(home, ".claude", "skills"),
		claudeConfig: install.ClaudeConfigPath(home, os.Getenv("CLAUDE_CONFIG_DIR")),
	}, nil
}

// executablePath is this binary's path with symlinks resolved.
func executablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// installTarget returns the paths and this binary's path.
func installTarget() (installPaths, string, error) {
	p, err := homePaths()
	if err != nil {
		return installPaths{}, "", err
	}
	bin, err := executablePath()
	return p, bin, err
}

// installSay prints one line of what install or uninstall did.
func installSay(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// runInstall does each step in order and says what it did; a step that
// fails stops it, after the lines for the steps already done.
func runInstall(cmd *cobra.Command, _ []string) error {
	p, bin, err := installTarget()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if err = install.Hooks(p.settings, bin, time.Now()); err != nil {
		return err
	}
	installSay(out, "Hooks: registered in %s", p.settings)
	if err = installTools(out, p.settings); err != nil {
		return err
	}
	if err = install.WriteTmux(p.snippet, p.tmuxConf, bin); err != nil {
		return err
	}
	installSay(out, "tmux snippet: written to %s and sourced from %s", p.snippet, p.tmuxConf)
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

// installTools adds the four tool rules with --allow-mcp-tools, and otherwise
// says how to.
func installTools(out io.Writer, settings string) error {
	if !installAllowMCPTools {
		installSay(out, "%s", installToolsHint)
		return nil
	}
	if err := install.AllowTools(settings, time.Now()); err != nil {
		return err
	}
	installSay(out, "MCP tools: the four curia tools are allowed without a prompt in %s", settings)
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
	case install.MCPForeign:
		installSay(out, "MCP server: %s has a curia entry that is not this binary's, or is not valid JSON; left alone", config)
	case install.MCPNoClaude:
		installSay(out, "MCP server: claude is not on PATH; to register it, run: %s", install.MCPAddCommand(bin))
	}
	return nil
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	p, bin, err := installTarget()
	if err != nil {
		return err
	}
	if err = install.Unhooks(p.settings, bin); err != nil {
		return err
	}
	if err = install.RemoveTmux(p.tmuxConf); err != nil {
		return err
	}
	if err = os.Remove(p.snippet); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = install.RemoveSkill(p.skills); err != nil {
		return err
	}
	if err = uninstallMCP(cmd.Context(), cmd.OutOrStdout(), p.claudeConfig, bin); err != nil {
		return err
	}
	// Re-sourcing ~/.tmux.conf unsets nothing in a running server.
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Curia is uninstalled.\n"+
		"Restart running Claude Code sessions.\n"+
		"Restart tmux to drop Curia's key bindings and hook, or run:\n"+
		"  tmux set-hook -gu pane-focus-in; tmux unbind a; tmux unbind N; tmux unbind A")
	return err
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
		installSay(out, "MCP server: %s has a curia entry that is not this binary's, or is not valid JSON; left alone", config)
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
	installCmd.Flags().BoolVar(&installAllowMCPTools, "allow-mcp-tools", false,
		"also let the four curia MCP tools run without a permission prompt")
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

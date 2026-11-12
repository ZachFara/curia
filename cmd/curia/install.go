package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/install"
)

// installPaths are the user-owned files the install commands edit.
type installPaths struct {
	settings, snippet, tmuxConf string
}

func homePaths() (installPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return installPaths{}, err
	}
	return installPaths{
		settings: filepath.Join(home, ".claude", "settings.json"),
		snippet:  filepath.Join(home, ".config", "curia", "curia.tmux"),
		tmuxConf: filepath.Join(home, ".tmux.conf"),
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

func runInstall(cmd *cobra.Command, _ []string) error {
	p, err := homePaths()
	if err != nil {
		return err
	}
	bin, err := executablePath()
	if err != nil {
		return err
	}
	if err = install.Hooks(p.settings, bin, time.Now()); err != nil {
		return err
	}
	if err = install.WriteTmux(p.snippet, p.tmuxConf, bin); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Curia is installed.\n"+
		"Next steps:\n"+
		"  1. Restart any running Claude Code sessions so they pick up the hooks.\n"+
		"  2. Run: tmux source-file ~/.tmux.conf")
	return err
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	p, err := homePaths()
	if err != nil {
		return err
	}
	bin, err := executablePath()
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
	// Re-sourcing ~/.tmux.conf unsets nothing in a running server.
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Curia is uninstalled.\n"+
		"Restart running Claude Code sessions.\n"+
		"Restart tmux to drop Curia's key bindings and hook, or run:\n"+
		"  tmux set-hook -gu pane-focus-in; tmux unbind a; tmux unbind N")
	return err
}

func init() {
	rootCmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Register Curia's hooks in Claude Code and add the tmux snippet",
			Args:  cobra.NoArgs,
			RunE:  runInstall,
		},
		&cobra.Command{
			Use:   "uninstall",
			Short: "Remove what curia install added",
			Args:  cobra.NoArgs,
			RunE:  runUninstall,
		},
	)
}

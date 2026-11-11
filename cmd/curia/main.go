// Command curia tracks every Claude Code session running in tmux and shows
// which ones need attention.
//
// Every subcommand lives in its own file in this directory and registers
// itself on rootCmd from that file's init function, so main.go never changes
// when a command is added.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

// version is the release version, overridden at build time with
// -ldflags "-X main.version=<version>".
var version = "dev"

// rootCmd is the top-level curia command. Subcommands attach themselves to it
// with rootCmd.AddCommand in their own init functions.
var rootCmd = &cobra.Command{
	Use:          "curia",
	Short:        "A tmux control plane for parallel coding agents",
	Version:      version,
	SilenceUsage: true,
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

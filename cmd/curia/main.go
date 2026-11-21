// Command curia tracks every Claude Code session running in tmux and shows
// which ones need attention.
//
// Every subcommand lives in its own file in this directory and registers
// itself on rootCmd from that file's init function, so main.go never changes
// when a command is added.
package main

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// rootCmd is the top-level curia command. Subcommands attach themselves to it
// with rootCmd.AddCommand in their own init functions.
var rootCmd = &cobra.Command{
	Use:          "curia",
	Short:        "A tmux control plane for parallel coding agents",
	Version:      resolveVersion(),
	SilenceUsage: true,
}

func main() {
	if filepath.Base(os.Args[0]) == "docker" { // the docker wrapper symlink (docker.go)
		dockerWrapperMain(os.Args[1:])
	}
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

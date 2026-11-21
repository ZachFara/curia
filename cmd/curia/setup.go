package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

func init() {
	parent := &cobra.Command{
		Use:   "setup",
		Short: "Approve a repository's .curia/setup.sh for curia spawn",
	}
	parent.AddCommand(&cobra.Command{
		Use:         "approve <task id>",
		Short:       "Show the task's .curia/setup.sh and approve its exact content for the repository",
		Annotations: userOnlyMark(),
		Long: "Shows the whole setup script in the task's worktree (path, size, SHA-256 and\n" +
			"every line, numbered) and asks you to type the first 8 characters of its SHA-256\n" +
			"on your terminal; then that exact content is approved for the task's repository.\n" +
			"curia spawn runs .curia/setup.sh only when its SHA-256 matches the approval, and\n" +
			"any change needs a new approval. A script holding anything the display would hide\n" +
			"(control or invisible characters, lines over 200 characters, more than 300 lines)\n" +
			"cannot be approved. It refuses inside an agent session.",
		Args: cobra.ExactArgs(1),
		RunE: runSetupApprove,
	})
	rootCmd.AddCommand(parent)
}

// runSetupApprove is curia setup approve <id>.
func runSetupApprove(cmd *cobra.Command, args []string) error {
	// No agent may approve code to run (spec section 7, Ruling 108).
	if err := userOnly("setup approve", "approve a setup script"); err != nil {
		return err
	}
	id := args[0]
	if !task.ValidID(id) {
		return fmt.Errorf("%q is not a task id (T-<n>)", sanitize.Line(id, 40))
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ctx := cmd.Context()
	t, s, err := lifecycle.SetupApprovable(ctx, st, id)
	if err != nil {
		return err
	}
	lifecycle.SetupShow(cmd.OutOrStdout(), t, s)
	if err = lifecycle.SetupConfirmed(s); err != nil {
		return err
	}
	if err = st.ApproveSetup(ctx, t.Repo, s.Sum); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Approved for %s: curia spawn runs this exact content; any change needs a new approval.\n",
		sanitize.Line(t.Repo, 300))
	return err
}

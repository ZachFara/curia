package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
)

// modeFlags are the flags of `curia mode <mode>`; tests reset them.
var modeFlags struct {
	all    bool
	repo   string
	forDur time.Duration
	native bool
}

// modeBypassWord is what the user types on the terminal to turn bypass on.
const modeBypassWord = "bypass"

func init() {
	parent := &cobra.Command{
		Use:   "mode",
		Short: "Show how much of Claude Code's permission prompting Curia answers; subcommands set it",
		Long: "curia mode prints each live session's effective mode. curia mode normal|edits|away|bypass\n" +
			"[--all | --repo [<path>]] [--for 6h] sets it for the live sessions of the current repository\n" +
			"(the default), of a repository, or of everyone. Only the user may set a mode, never an agent.",
		Args: cobra.NoArgs,
		RunE: runModeShow,
	}
	for _, m := range model.Modes {
		c := &cobra.Command{
			Use:         m,
			Short:       "Set the mode to " + m + " (the user only)",
			Annotations: userOnlyMark(),
			Args:        cobra.NoArgs,
			RunE:        func(cmd *cobra.Command, _ []string) error { return runModeSet(cmd, m) },
		}
		c.Flags().BoolVar(&modeFlags.all, "all", false, "every live session")
		c.Flags().StringVar(&modeFlags.repo, "repo", "", "the sessions of a repository (default: the current directory's)")
		c.Flags().Lookup("repo").NoOptDefVal = "."
		c.Flags().DurationVar(&modeFlags.forDur, "for", 0, "how long the mode lasts (bypass: default mode.bypass_for)")
		if m == model.ModeBypass {
			c.Flags().BoolVar(&modeFlags.native, "native", false, "relaunch idle agents with Claude Code's own bypass flag (needs --for)")
		}
		parent.AddCommand(c)
	}
	rootCmd.AddCommand(parent)
}

func runModeShow(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	list, err := st.ListSessions(cmd.Context(), store.ListFilter{})
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "default mode: %s\n", cfg.Mode.Default)
	for _, s := range list {
		_, _ = fmt.Fprintf(out, "%s  %s  %s\n", lifecycle.SpawnShortID(s.ID), modeText(s, cfg, clock()), sanitize.Line(s.Repo, 80))
	}
	return nil
}

// modeNow is a session's mode at now: its own unless unset or lapsed.
func modeNow(s model.Session, cfg config.Config, now time.Time) string {
	if s.Mode == "" || (!s.ModeUntil.IsZero() && !now.Before(s.ModeUntil)) {
		return cfg.Mode.Default
	}
	return s.Mode
}

// modeText is "<mode>" with its expiry when it has one, for a view.
func modeText(s model.Session, cfg config.Config, now time.Time) string {
	m := modeNow(s, cfg, now)
	if m != "" && s.Mode == m && !s.ModeUntil.IsZero() {
		return m + " until " + s.ModeUntil.Local().Format("15:04")
	}
	return m
}

func runModeSet(cmd *cobra.Command, mode string) error {
	if err := userOnly("mode "+mode, "set a permission mode"); err != nil {
		return err
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
	scope, label, err := modeScope(cmd.Context())
	if err != nil {
		return err
	}
	return modeApply(cmd.Context(), st, cfg, scope, label, mode, cmd.OutOrStdout())
}

// modeScope is the scope the flags name: --all, --repo, else the repository
// of the current directory (an error outside one).
func modeScope(ctx context.Context) (store.ModeScope, string, error) {
	if modeFlags.all {
		if modeFlags.repo != "" {
			return store.ModeScope{}, "", errors.New("--all and --repo cannot be used together")
		}
		return store.ModeScope{All: true}, "everyone", nil
	}
	dir := modeFlags.repo
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return store.ModeScope{}, "", err
	}
	repo, _, _ := gitops.Info(ctx, abs, nil, subprocessTimeout)
	if repo == "" {
		return store.ModeScope{}, "", fmt.Errorf("%s is not inside a git repository: use --all, or --repo <path>", sanitize.Line(abs, 120))
	}
	return store.ModeScope{Repo: repo}, sanitize.Line(repo, 120), nil
}

// modeApply sets mode for the live sessions in scope and says what it did.
// bypass shows what it allows and what still prompts, and is set only after
// the user types the word on the terminal.
func modeApply(ctx context.Context, st *store.Store, cfg config.Config, scope store.ModeScope, label, mode string, out io.Writer) error {
	now := clock()
	before, err := modeLive(ctx, st, scope)
	if err != nil {
		return err
	}
	var until time.Time
	if d := modeFlags.forDur; d > 0 {
		until = now.Add(d)
	}
	if mode == model.ModeBypass {
		if until.IsZero() {
			until = now.Add(cfg.Mode.BypassFor)
		}
		if err = modeConfirmBypass(out, label, until, modeFlags.native); err != nil {
			return err
		}
	}
	n, err := st.SetModes(ctx, scope, mode, until)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%d live session(s) of %s now in mode %s%s.\n", n, label, mode, modeUntilText(until))
	modeSayNoRoot(out, before)
	leaving := slices.ContainsFunc(before, func(s model.Session) bool {
		m := modeNow(s, cfg, now)
		return m == model.ModeAway || m == model.ModeBypass
	})
	if leaving && (mode == model.ModeNormal || mode == model.ModeEdits) {
		_, _ = fmt.Fprintln(out, "This restores Curia's own mode only. It cannot restore Claude Code's own permission mode: a session resumed "+
			"during away or bypass is in Claude Code's default mode now, and Claude Code, not Curia, decides its prompts.")
	}
	if modeFlags.native && mode == model.ModeBypass {
		modeNativeReport(out, before)
	}
	return nil
}

func modeUntilText(until time.Time) string {
	if until.IsZero() {
		return ""
	}
	return " until " + until.Local().Format("2006-01-02 15:04")
}

// modeLive is the live sessions in scope.
func modeLive(ctx context.Context, st *store.Store, scope store.ModeScope) ([]model.Session, error) {
	all, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(s model.Session) bool { return !scope.All && s.Repo != scope.Repo }), nil
}

// modeConfirmBypass prints what bypass allows and what still prompts, and
// reads the word from the terminal (never stdin; anything else cancels).
func modeConfirmBypass(out io.Writer, label string, until time.Time, native bool) error {
	if native && modeFlags.forDur <= 0 {
		return errors.New("--native needs --for")
	}
	if native && os.Geteuid() == 0 {
		return errors.New("--native is refused as root (Claude Code refuses its bypass flag there)")
	}
	_, _ = fmt.Fprintf(out, "Bypass for %s%s: Curia's hook allows every tool call except what your Claude Code settings forbid.\n"+
		"A hook's allow cannot override a deny rule, an ask rule, a managed policy or plan mode, so those still hold.\n"+
		"Still prompting: AskUserQuestion and tools that need you, rm and rmdir on critical paths, ask rules, edits in plan mode.\n"+
		"Curia's own files and the task budget stop stay protected. Any prompt injection that reaches an agent runs unprompted.\n",
		label, modeUntilText(until))
	answer, err := promptTerminal("To turn it on, type the word " + modeBypassWord + ": ")
	if err != nil || strings.TrimSpace(answer) != modeBypassWord {
		return errors.New("not turned on: bypass needs the word " + modeBypassWord + " typed on the terminal; nothing changed")
	}
	return nil
}

// modeNativeCommand is the ONE place Claude Code's bypass flag is built:
// the line that resumes session id in dir with it.
func modeNativeCommand(id, dir string) string {
	return "cd '" + strings.ReplaceAll(dir, "'", `'\''`) + "' && claude --resume " + id + " --dangerously-skip-permissions"
}

// modeNativeReport lists, for each idle session in scope, the line that
// relaunches it with Claude Code's own bypass flag, and the busy ones it
// skips. It runs and types nothing: the first use of the flag shows an
// acceptance dialog that only the user may answer.
func modeNativeReport(out io.Writer, sessions []model.Session) {
	_, _ = fmt.Fprintln(out, "--native: this skips prompts in Claude Code itself, not only through Curia's hook, and outlasts the hook until the agent is relaunched.")
	_, _ = fmt.Fprintln(out, "Curia relaunches and types nothing. Exit each idle agent yourself (/exit), then run its line; the first time, Claude Code shows an acceptance dialog that only you answer:")
	for _, s := range sessions {
		dir := modeDir(s)
		switch {
		case s.State != model.StateIdle:
			_, _ = fmt.Fprintf(out, "  skipped %s: not idle (%s)\n", lifecycle.SpawnShortID(s.ID), s.State)
		case !filepath.IsAbs(dir) || strings.ContainsFunc(dir, unicode.IsControl) || s.Agent == model.AgentCodex || !modeSafeID(s.ID):
			_, _ = fmt.Fprintf(out, "  skipped %s: not a Claude Code session with a usable directory\n", lifecycle.SpawnShortID(s.ID))
		default:
			_, _ = fmt.Fprintf(out, "  %s\n", modeNativeCommand(s.ID, dir))
		}
	}
}

func modeDir(s model.Session) string {
	if s.Worktree != "" {
		return s.Worktree
	}
	return s.Cwd
}

// modeSafeID reports whether id is only [A-Za-z0-9_-], so it can be pasted into a shell line.
func modeSafeID(id string) bool {
	return id != "" && len(id) <= 64 && !strings.ContainsFunc(id, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	})
}

// modeSayNoRoot names the sessions with no repository or worktree: the mode
// froze no root for them, so it allows no path.
func modeSayNoRoot(out io.Writer, sessions []model.Session) {
	noRoot := slices.DeleteFunc(slices.Clone(sessions), func(s model.Session) bool { return s.Worktree != "" || s.Repo != "" })
	if len(noRoot) == 0 {
		return
	}
	ids := make([]string, 0, len(noRoot))
	for _, s := range noRoot {
		ids = append(ids, lifecycle.SpawnShortID(s.ID))
	}
	_, _ = fmt.Fprintf(out, "%d session(s) have no repository or worktree (%s), so they have no path to allow in: edits allows nothing for them and away denies.\n",
		len(noRoot), sanitize.Line(strings.Join(ids, ", "), 120))
}

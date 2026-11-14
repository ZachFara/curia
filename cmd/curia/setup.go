package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// A worktree's .curia/setup.sh comes from the repository, which may be
// someone else's or written by an agent, so curia spawn runs it only when
// the user has approved that exact content for that repository (Ruling
// 108). The script is read once, hashed, compared with the approval and
// then run from a private copy of the bytes that were hashed, so the file
// in the worktree cannot be swapped between the check and the run.

const (
	setupName       = ".curia/setup.sh"
	setupMaxBytes   = 64 << 10 // a setup script larger than this is not run
	setupMaxShebang = 256      // bytes of the #! line
	setupLogCap     = 1 << 20  // the setup log's size limit
	setupTailLines  = 20       // setup log lines shown on failure
	setupHeadLines  = 40       // script lines curia setup approve shows
	setupLineMax    = 200      // runes of a shown line
)

// setupEnvNames are the variables of this process a setup script gets,
// besides CURIA_TASK_ID, CURIA_REPO and CURIA_WORKTREE. Nothing else is
// passed, so no API key or token in the user's environment reaches it.
var setupEnvNames = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM"}

// errSetupAbsent is setupRead's error when the worktree has no setup script.
var errSetupAbsent = errors.New("no " + setupName)

// setupScript is a setup script read once: where it was, its bytes, their
// hex SHA-256 and the interpreter its #! line names (with its one optional
// argument).
type setupScript struct {
	Path   string
	Body   []byte
	Sum    string
	Interp []string
}

// setupRead reads the worktree's .curia/setup.sh once. It is errSetupAbsent
// when there is none, and an error saying why it is not to be run when it
// resolves outside the worktree, is not an executable regular file, is
// larger than setupMaxBytes or has no #! line naming an absolute
// interpreter.
func setupRead(worktree string) (setupScript, error) {
	link := filepath.Join(worktree, ".curia", "setup.sh")
	if _, err := os.Lstat(link); errors.Is(err, fs.ErrNotExist) {
		return setupScript{}, errSetupAbsent
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		return setupScript{}, errors.New("cannot be resolved")
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil || !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return setupScript{}, errors.New("resolves outside the worktree")
	}
	body, err := setupReadFile(resolved)
	if err != nil {
		return setupScript{}, err
	}
	interp, err := setupInterpreter(body)
	if err != nil {
		return setupScript{}, err
	}
	sum := sha256.Sum256(body)
	return setupScript{Path: resolved, Body: body, Sum: hex.EncodeToString(sum[:]), Interp: interp}, nil
}

// setupReadFile reads path, which must be an executable regular file of at
// most setupMaxBytes. It is checked before the open (a FIFO would block)
// and again on the open handle, which is what is read.
func setupReadFile(path string) ([]byte, error) {
	if err := requireRegular(os.Stat(path)); err != nil {
		return nil, errors.New("is not a regular file")
	}
	f, err := os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot be opened")
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	switch {
	case requireRegular(fi, err) != nil:
		return nil, errors.New("is not a regular file")
	case fi.Mode().Perm()&0o111 == 0:
		return nil, errors.New("is not executable")
	}
	body, err := io.ReadAll(io.LimitReader(f, setupMaxBytes+1))
	switch {
	case err != nil:
		return nil, errors.New("cannot be read")
	case len(body) > setupMaxBytes:
		return nil, fmt.Errorf("is larger than %d KiB", setupMaxBytes>>10)
	}
	return body, nil
}

// setupInterpreter reads the #! line of body as the kernel does: an
// absolute interpreter, then at most one argument (the rest of the line).
func setupInterpreter(body []byte) ([]string, error) {
	line, _, _ := bytes.Cut(body[:min(len(body), setupMaxShebang)], []byte("\n"))
	rest, ok := strings.CutPrefix(strings.TrimSuffix(string(line), "\r"), "#!")
	rest = strings.TrimSpace(rest)
	if !ok || rest == "" {
		return nil, errors.New("has no #! line")
	}
	interp, arg, _ := strings.Cut(rest, " ")
	if !filepath.IsAbs(interp) || strings.ContainsFunc(rest, func(r rune) bool { return r < 0x20 && r != '\t' }) {
		return nil, errors.New("has no #! line naming an absolute interpreter")
	}
	if arg = strings.TrimSpace(arg); arg != "" {
		return []string{interp, arg}, nil
	}
	return []string{interp}, nil
}

// setupApproveCommand is the command that approves task id's setup script.
func setupApproveCommand(id string) string { return "curia setup approve " + id }

// setupStep is spawn's step 3. An absent script is said in one line; one
// not to be run, or not approved for t's repository in its exact content,
// is skipped with one line saying why and the spawn goes on. An approved
// one is run (setupExec); its failure stops the spawn, with the log's path
// and its last lines, cleaned.
func setupStep(ctx context.Context, st *store.Store, cfg config.Config, t model.Task, out io.Writer) error {
	s, err := setupRead(t.Worktree)
	if errors.Is(err, errSetupAbsent) {
		_, _ = fmt.Fprintf(out, "Setup: no %s\n", setupName)
		return nil
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "Setup: %s %s; skipped\n", setupName, err)
		return nil
	}
	approved, err := st.SetupApproval(ctx, t.Repo)
	if err != nil {
		return err
	}
	if approved != s.Sum {
		_, _ = fmt.Fprintf(out, "Setup: %s skipped: this content is not approved for %s (or it changed since); read it and approve it with: %s\n",
			setupName, sanitize.Line(t.Repo, 300), setupApproveCommand(t.ID))
		return nil
	}
	logPath := filepath.Join(filepath.Dir(cfg.LogPath), "setup", t.ID+".log")
	runErr, err := setupExec(ctx, t, s, logPath)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if runErr == nil {
		_, _ = fmt.Fprintf(out, "Setup: ran %s (log: %s)\n", setupName, logPath)
		return nil
	}
	_, _ = fmt.Fprintf(out, "Setup: %s failed: %s. Last lines of %s:\n", setupName, runErr, logPath)
	for _, l := range setupTail(logPath) {
		_, _ = fmt.Fprintf(out, "  %s\n", l)
	}
	return fmt.Errorf("setup script failed: %w; its log is %s", runErr, logPath)
}

// setupExec runs s for t: its bytes, as read and hashed, are written to a
// private file (0600) in a directory it creates (0700) and run by the
// interpreter of its #! line, with no shell, the worktree as its
// directory, stdin from /dev/null, setupEnv, spawnSetupLimit, its own
// process group (the limit kills what it started too) and its output in
// the log at logPath (truncated first, capped at 1 MiB). It returns the
// script's failure as runErr, and a failure to run it at all as err.
func setupExec(ctx context.Context, t model.Task, s setupScript, logPath string) (runErr, err error) {
	dir, err := os.MkdirTemp("", "curia-setup-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	copyPath := filepath.Join(dir, "setup.sh")
	if err = os.WriteFile(copyPath, s.Body, 0o600); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Clean(logPath), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithTimeout(ctx, spawnSetupLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Interp[0], append(s.Interp[1:], copyPath)...)
	cmd.Dir = t.Worktree
	cmd.Env = setupEnv(t)
	w := &setupCapWriter{w: f, left: setupLogCap}
	cmd.Stdout, cmd.Stderr = w, w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	runErr = cmd.Run()
	switch {
	case errors.Is(runErr, exec.ErrWaitDelay):
		runErr = nil // it succeeded; something it left running holds its output
	case runErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		runErr = fmt.Errorf("timed out after %s", spawnSetupLimit)
	}
	return runErr, nil
}

// setupEnv is the setup script's whole environment: setupEnvNames as this
// process has them, then CURIA_TASK_ID, CURIA_REPO and CURIA_WORKTREE.
func setupEnv(t model.Task) []string {
	var env []string
	for _, name := range setupEnvNames {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return append(env, "CURIA_TASK_ID="+t.ID, "CURIA_REPO="+t.Repo, "CURIA_WORKTREE="+t.Worktree)
}

// setupCapWriter writes to w until left bytes are written and then drops
// the rest, reporting every write as complete so the script never sees an
// error.
type setupCapWriter struct {
	w    io.Writer
	left int
}

func (c *setupCapWriter) Write(p []byte) (int, error) {
	if c.left > 0 {
		n := min(len(p), c.left)
		if _, err := c.w.Write(p[:n]); err != nil {
			return 0, err
		}
		c.left -= n
	}
	return len(p), nil
}

// setupTail returns the last setupTailLines lines of the log at path, each
// cleaned to one line of at most setupLineMax runes.
func setupTail(path string) []string {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return []string{"(the log cannot be read: " + sanitize.Line(err.Error(), setupLineMax) + ")"}
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	lines = lines[max(0, len(lines)-setupTailLines):]
	for i, l := range lines {
		lines[i] = sanitize.Line(l, setupLineMax)
	}
	return lines
}

func init() {
	parent := &cobra.Command{
		Use:   "setup",
		Short: "Approve a repository's .curia/setup.sh for curia spawn",
	}
	parent.AddCommand(&cobra.Command{
		Use:   "approve <task id>",
		Short: "Show the task's .curia/setup.sh and approve its exact content for the repository",
		Long: "Shows the setup script in the task's worktree (path, size, SHA-256 and its first\n" +
			"40 lines) and approves that exact content for the task's repository: curia spawn\n" +
			"runs .curia/setup.sh only when its SHA-256 matches the approval, and any change\n" +
			"needs a new approval. Only the user may approve: it refuses inside an agent session.",
		Args: cobra.ExactArgs(1),
		RunE: runSetupApprove,
	})
	rootCmd.AddCommand(parent)
}

// runSetupApprove is curia setup approve <id>.
func runSetupApprove(cmd *cobra.Command, args []string) error {
	// No agent may approve code to run (spec section 7, Ruling 108).
	if refuseInsideAgent() != nil {
		return errors.New("curia setup approve cannot run inside an agent session (CLAUDECODE is set): only the user may approve a setup script")
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
	t, s, err := setupApprovable(ctx, st, id)
	if err != nil {
		return err
	}
	setupShow(cmd.OutOrStdout(), t, s)
	if err = st.ApproveSetup(ctx, t.Repo, s.Sum); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Approved for %s: curia spawn runs this exact content; any change needs a new approval.\n",
		sanitize.Line(t.Repo, 300))
	return err
}

// setupApprovable loads task id and reads the setup script in its worktree.
func setupApprovable(ctx context.Context, st *store.Store, id string) (model.Task, setupScript, error) {
	t, found, err := st.GetTask(ctx, id)
	if err != nil {
		return model.Task{}, setupScript{}, err
	}
	if !found {
		return model.Task{}, setupScript{}, fmt.Errorf("no task %s", id)
	}
	ours, err := worktreeIsOurs(ctx, t.Repo, t.Worktree, t.Branch)
	if t.Worktree == "" || err != nil || !ours {
		return model.Task{}, setupScript{}, fmt.Errorf("%s has no worktree to read %s from: run curia spawn %s first", id, setupName, id)
	}
	s, err := setupRead(t.Worktree)
	if err != nil {
		return model.Task{}, setupScript{}, fmt.Errorf("%s: %s %w", sanitize.Line(t.Worktree, 300), setupName, err)
	}
	return t, s, nil
}

// setupShow prints what is being approved: the script's path, size, hash
// and first setupHeadLines lines, cleaned with their indentation kept.
func setupShow(out io.Writer, t model.Task, s setupScript) {
	_, _ = fmt.Fprintf(out, "Repository: %s\nScript: %s\nSize: %d bytes\nSHA-256: %s\nFirst lines:\n",
		sanitize.Line(t.Repo, 300), sanitize.Line(s.Path, 300), len(s.Body), s.Sum)
	lines := strings.Split(strings.TrimRight(string(s.Body), "\n"), "\n")
	for _, l := range lines[:min(len(lines), setupHeadLines)] {
		// Block keeps the indentation a reader needs; the bound is in bytes.
		_, _ = fmt.Fprintf(out, "  %s\n", sanitize.Block(l, 4*setupLineMax))
	}
	if n := len(lines) - setupHeadLines; n > 0 {
		_, _ = fmt.Fprintf(out, "  (%d more lines not shown)\n", n)
	}
}

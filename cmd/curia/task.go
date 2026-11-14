package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/ui"
)

// Flags of `curia task`; tests reset them.
var (
	taskAddRepo, taskAddBody, taskAddBase string
	taskAddTags                           []string
	taskLsStatus, taskLsRepo              string
	taskLsJSON                            bool
	taskEditTitle, taskEditBody           string
	taskEditBase, taskEditStatus          string
	taskEditTags                          []string
)

// taskEditMax bounds the read of the file the editor leaves: a title, a
// blank line and a body, each later checked against its own limit.
const taskEditMax = task.MaxBody + 4<<10

// taskCmd is `curia task`; each subcommand file adds itself to it.
var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Add, list and edit tasks",
}

// taskEditor runs the editor argv (the config's editor option) on path and
// waits for it; tests replace it.
var taskEditor = taskDefaultEditor

func init() {
	add := &cobra.Command{
		Use:   "add <title>",
		Short: "Add a task to the backlog",
		Long: "Adds a task for the git repository holding --repo (default: the current\n" +
			"directory), which must be a main worktree, not a linked one. --base defaults\n" +
			"to the branch checked out there. --body reads the brief from a file, or from\n" +
			"stdin with -.",
		Args: cobra.ExactArgs(1),
		RunE: runTaskAdd,
	}
	add.Flags().StringVar(&taskAddRepo, "repo", ".", "a directory inside the task's repository")
	add.Flags().StringVar(&taskAddBody, "body", "", "a file holding the brief, or - for stdin")
	add.Flags().StringVar(&taskAddBase, "base", "", "the branch the task starts from (default: the checked-out branch)")
	add.Flags().StringArrayVar(&taskAddTags, "tag", nil, "a tag (repeatable)")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List tasks with their derived status",
		Long: "Lists tasks by board column, then id. --status is backlog, ready, running,\n" +
			"needs_decision, review, done, dropped or all; the default leaves out dropped.\n" +
			"--json prints a JSON array.",
		Args: cobra.NoArgs,
		RunE: runTaskLs,
	}
	ls.Flags().StringVar(&taskLsStatus, "status", "", "only tasks with this status, or all (default: all but dropped)")
	ls.Flags().StringVar(&taskLsRepo, "repo", "", "only tasks of the repository holding this directory")
	ls.Flags().BoolVar(&taskLsJSON, "json", false, "print a JSON array instead of a table")
	edit := &cobra.Command{
		Use:   "edit <id>",
		Short: "Change a task",
		Long: "Changes the fields given by flags; --body reads the brief from a file, or\n" +
			"from stdin with -. With no flag, on a terminal, opens the editor set by the\n" +
			"editor option of the config file on the title, a blank line and the body.\n" +
			"With no editor set, use the flags.",
		Args: cobra.ExactArgs(1),
		RunE: runTaskEdit,
	}
	edit.Flags().StringVar(&taskEditTitle, "title", "", "the new title")
	edit.Flags().StringVar(&taskEditBody, "body", "", "a file holding the new brief, or - for stdin")
	edit.Flags().StringVar(&taskEditBase, "base", "", "the new base branch")
	edit.Flags().StringArrayVar(&taskEditTags, "tag", nil, "a tag (repeatable; replaces the tags)")
	edit.Flags().StringVar(&taskEditStatus, "status", "", "backlog, ready or dropped")
	ready := &cobra.Command{
		Use:   "ready <id>",
		Short: "Mark a task ready to spawn",
		Args:  cobra.ExactArgs(1),
		RunE:  runTaskReady,
	}
	taskCmd.AddCommand(add, ls, edit, ready)
	rootCmd.AddCommand(taskCmd)
}

// taskDefaultEditor runs argv with path appended as the last argument,
// without a shell and with the terminal attached. A first word that is not
// an absolute path is looked up on PATH.
func taskDefaultEditor(ctx context.Context, argv []string, path string) error {
	if len(argv) == 0 {
		return errors.New("no editor configured")
	}
	prog, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	c := exec.CommandContext(ctx, prog, append(argv[1:], path)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

// taskLogger opens the log file for gitInfo's warnings; when it does not
// open, warnings are dropped.
func taskLogger(cfg config.Config) (*slog.Logger, func()) {
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return slog.New(slog.DiscardHandler), func() {}
	}
	return logger, func() { _ = closeLog() }
}

// taskGitRepo resolves dir to its main repository's top level, the branch
// checked out in dir (empty when HEAD is detached) and, when dir is in a
// linked worktree, that worktree's top level.
func taskGitRepo(ctx context.Context, dir string, logger *slog.Logger) (repo, branch, worktree string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", "", err
	}
	if fi, serr := os.Stat(abs); serr != nil || !fi.IsDir() {
		return "", "", "", fmt.Errorf("%s is not a directory", sanitize.Line(abs, 200))
	}
	repo, branch, worktree = gitInfo(ctx, abs, logger)
	if repo == "" {
		return "", "", "", fmt.Errorf("%s is not inside a git repository", sanitize.Line(abs, 200))
	}
	return repo, branch, worktree, nil
}

// taskMainRepo is taskGitRepo for commands that store a repository: a
// linked worktree is refused.
func taskMainRepo(ctx context.Context, dir string, logger *slog.Logger) (repo, branch string, err error) {
	repo, branch, wt, err := taskGitRepo(ctx, dir, logger)
	if err == nil && wt != "" {
		err = fmt.Errorf("%s is a linked worktree of %s; give the main worktree", sanitize.Line(wt, 200), sanitize.Line(repo, 200))
	}
	return repo, branch, err
}

// taskReadFile reads at most limit bytes of the regular file at path, or
// of stdin when path is "-". Anything else (a directory, a FIFO, which
// would block the open) is an error, checked before the open and again on
// the open handle; the open does not block.
func taskReadFile(stdin io.Reader, path string, limit int) (string, error) {
	r := stdin
	if path != "-" {
		p := filepath.Clean(path)
		if err := requireRegular(os.Stat(p)); err != nil {
			return "", err
		}
		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		if err = requireRegular(f.Stat()); err != nil {
			return "", err
		}
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(b) > limit {
		return "", fmt.Errorf("must be at most %d bytes", limit)
	}
	return string(b), nil
}

// taskBodyProblem reads the body flag value arg into *body, or returns the
// problem.
func taskBodyProblem(stdin io.Reader, arg string, body *string) []task.Problem {
	b, err := taskReadFile(stdin, arg, task.MaxBody)
	if err != nil {
		return []task.Problem{{Field: "body", Problem: err.Error()}}
	}
	*body = b
	return nil
}

// taskCheck normalises t and returns early followed by every problem
// task.Validate finds in a field early does not already name.
func taskCheck(t model.Task, early []task.Problem) (model.Task, []task.Problem) {
	t = task.Normalize(t)
	ps := early
	for _, p := range task.Validate(t) {
		named := false
		for _, e := range early {
			named = named || e.Field == p.Field
		}
		if !named {
			ps = append(ps, p)
		}
	}
	return t, ps
}

// taskProblemsError lists every problem, one per line, after what.
func taskProblemsError(what string, ps []task.Problem) error {
	var b strings.Builder
	b.WriteString(what)
	for _, p := range ps {
		fmt.Fprintf(&b, "\n  %s: %s", p.Field, sanitize.Line(p.Problem, 300))
	}
	return errors.New(b.String())
}

func runTaskAdd(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger, closeLog := taskLogger(cfg)
	defer closeLog()
	t := model.Task{
		Title: args[0], BaseBranch: taskAddBase, Tags: taskAddTags,
		Status: model.TaskBacklog, Source: model.SourceLocal, CreatedAt: clock(),
	}
	var early []task.Problem
	repo, branch, err := taskMainRepo(cmd.Context(), taskAddRepo, logger)
	switch {
	case err != nil:
		early = append(early, task.Problem{Field: "repo", Problem: err.Error()})
	case t.BaseBranch == "" && branch == "":
		early = append(early, task.Problem{Field: "base_branch", Problem: "HEAD is detached; give --base"})
	case t.BaseBranch == "":
		t.BaseBranch = branch
	}
	t.Repo = repo
	if taskAddBody != "" {
		early = append(early, taskBodyProblem(cmd.InOrStdin(), taskAddBody, &t.Body)...)
	}
	t, ps := taskCheck(t, early)
	if len(ps) > 0 {
		return taskProblemsError("task not added:", ps)
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	stored, err := st.CreateTask(cmd.Context(), t)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s added (%s)\n", stored.ID, stored.Status)
	return err
}

// taskCards lists every task, the sessions and the open blocking tickets
// by task, and derives each task's card (task.Cards).
func taskCards(ctx context.Context, st *store.Store, now time.Time) ([]task.Card, error) {
	tasks, err := st.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		return nil, err
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	blocking, err := st.OpenBlockingByTask(ctx, now)
	if err != nil {
		return nil, err
	}
	return task.Cards(tasks, sessions, blocking), nil
}

// taskStatusFilter reports which derived statuses --status lists.
func taskStatusFilter(name string) (func(model.TaskStatus) bool, error) {
	switch s := model.TaskStatus(name); s {
	case "":
		return func(st model.TaskStatus) bool { return st != model.TaskDropped }, nil
	case "all":
		return func(model.TaskStatus) bool { return true }, nil
	case model.TaskBacklog, model.TaskReady, model.TaskRunning, model.TaskNeedsDecision,
		model.TaskReview, model.TaskDone, model.TaskDropped:
		return func(st model.TaskStatus) bool { return st == s }, nil
	}
	return nil, fmt.Errorf("invalid --status %q: want backlog, ready, running, needs_decision, review, done, dropped or all",
		sanitize.Line(name, 40))
}

func runTaskLs(cmd *cobra.Command, _ []string) error {
	want, err := taskStatusFilter(taskLsStatus)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repo := ""
	if taskLsRepo != "" {
		logger, closeLog := taskLogger(cfg)
		repo, _, _, err = taskGitRepo(cmd.Context(), taskLsRepo, logger)
		closeLog()
		if err != nil {
			return fmt.Errorf("--repo: %w", err)
		}
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cards, err := taskCards(cmd.Context(), st, clock())
	if err != nil {
		return err
	}
	shown := make([]task.Card, 0, len(cards))
	for _, c := range cards {
		if want(c.Status) && (repo == "" || c.Task.Repo == repo) {
			shown = append(shown, c)
		}
	}
	if taskLsJSON {
		return taskPrintJSON(cmd.OutOrStdout(), shown)
	}
	return taskPrintTable(cmd.OutOrStdout(), shown)
}

// taskRow is one task in `curia task ls --json`. Its keys are the columns
// of the tasks table except n, with status the derived status and the
// stored one as stored_status, plus sessions: the ids of the live linked
// sessions. A zero time is null.
type taskRow struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	Body          string           `json:"body"`
	Repo          string           `json:"repo"`
	BaseBranch    string           `json:"base_branch"`
	Status        model.TaskStatus `json:"status"`
	StoredStatus  model.TaskStatus `json:"stored_status"`
	Source        model.TaskSource `json:"source"`
	SourceRef     string           `json:"source_ref"`
	Tags          []string         `json:"tags"`
	Branch        string           `json:"branch"`
	Worktree      string           `json:"worktree"`
	Team          bool             `json:"team"`
	AgentModel    string           `json:"agent_model"`
	Pane          string           `json:"pane"`
	ResumeSession string           `json:"resume_session"`
	Sessions      []string         `json:"sessions"`
	CreatedAt     *time.Time       `json:"created_at"`
	UpdatedAt     *time.Time       `json:"updated_at"`
	LaunchedAt    *time.Time       `json:"launched_at"`
	PausedAt      *time.Time       `json:"paused_at"`
	CleanedAt     *time.Time       `json:"cleaned_at"`
}

func taskToRow(c task.Card) taskRow {
	t := c.Task
	tags, sessions := t.Tags, make([]string, 0, len(c.Sessions))
	if tags == nil {
		tags = []string{}
	}
	for _, s := range c.Sessions {
		sessions = append(sessions, s.ID)
	}
	return taskRow{
		ID: t.ID, Title: t.Title, Body: t.Body, Repo: t.Repo, BaseBranch: t.BaseBranch,
		Status: c.Status, StoredStatus: t.Status, Source: t.Source, SourceRef: t.SourceRef, Tags: tags,
		Branch: t.Branch, Worktree: t.Worktree, Team: t.Team, AgentModel: t.AgentModel, Pane: t.Pane,
		ResumeSession: t.ResumeSession, Sessions: sessions,
		CreatedAt: jsonTime(t.CreatedAt), UpdatedAt: jsonTime(t.UpdatedAt), LaunchedAt: jsonTime(t.LaunchedAt),
		PausedAt: jsonTime(t.PausedAt), CleanedAt: jsonTime(t.CleanedAt),
	}
}

func taskPrintJSON(w io.Writer, cards []task.Card) error {
	rows := make([]taskRow, 0, len(cards)) // never nil, so no tasks is []
	for _, c := range cards {
		rows = append(rows, taskToRow(c))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		return err
	}
	_, err := io.WriteString(w, escapeUnprintable(buf.String()))
	return err
}

// taskCell makes task text, which is untrusted, safe as one table cell.
func taskCell(s string) string { return ui.Cell(sanitize.Line(s, task.MaxTitle)) }

// taskStatusText is c's status with what the status cannot say: paused
// when the task is paused, worktree when it has one and no live session.
func taskStatusText(c task.Card) string {
	var notes []string
	if !c.Task.PausedAt.IsZero() {
		notes = append(notes, "paused")
	}
	if c.Task.Worktree != "" && len(c.Sessions) == 0 {
		notes = append(notes, "worktree")
	}
	if len(notes) == 0 {
		return string(c.Status)
	}
	return string(c.Status) + " (" + strings.Join(notes, ", ") + ")"
}

// taskAgentText is the pane of c's lead (or solo) live session, with the
// number of its other live sessions; empty when it has none.
func taskAgentText(c task.Card) string {
	if len(c.Sessions) == 0 {
		return ""
	}
	first := c.Pane
	if first == "" {
		first = c.Sessions[0].ID
	}
	if n := len(c.Sessions) - 1; n > 0 {
		return fmt.Sprintf("%s +%d", first, n)
	}
	return first
}

// taskPrintTable prints one row per task, the repo as its basename.
func taskPrintTable(w io.Writer, cards []task.Card) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tREPO\tTITLE\tAGENT")
	for _, c := range cards {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", taskCell(c.Task.ID), taskCell(taskStatusText(c)),
			taskCell(ui.RepoName(c.Task.Repo)), taskCell(c.Task.Title), taskCell(taskAgentText(c)))
	}
	return tw.Flush()
}

// taskCheckID refuses an argument that is not a task id.
func taskCheckID(id string) error {
	if !task.ValidID(id) {
		return fmt.Errorf("invalid task id %q: want T-<number>", sanitize.Line(id, 40))
	}
	return nil
}

// taskUpdate applies change to task id in one write transaction, then
// checks the result (every problem together) and stamps updated_at.
func taskUpdate(ctx context.Context, cfg config.Config, id string, early []task.Problem, change func(*model.Task)) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	now := clock()
	_, err = st.UpdateTask(ctx, id, func(cur model.Task) (model.Task, error) {
		next := cur
		change(&next)
		next, ps := taskCheck(next, early)
		if len(ps) > 0 {
			return model.Task{}, taskProblemsError(id+" not changed:", ps)
		}
		next.UpdatedAt = now
		return next, nil
	})
	if errors.Is(err, store.ErrNoTask) {
		return fmt.Errorf("%s: no such task", id)
	}
	return err
}

// taskEditStatusOf checks --status: only a stored status may be set.
func taskEditStatusOf(name string) (model.TaskStatus, error) {
	switch s := model.TaskStatus(name); s {
	case model.TaskBacklog, model.TaskReady, model.TaskDropped:
		return s, nil
	}
	return "", fmt.Errorf("invalid --status %q: want backlog, ready or dropped", sanitize.Line(name, 40))
}

func runTaskEdit(cmd *cobra.Command, args []string) error {
	id := args[0]
	if err := taskCheckID(id); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	f := cmd.Flags()
	if !f.Changed("title") && !f.Changed("body") && !f.Changed("base") && !f.Changed("tag") && !f.Changed("status") {
		if len(cfg.EditorArgs()) == 0 {
			return taskNoEditorError()
		}
		if !taskIsTerminal(cmd.InOrStdin()) {
			return errors.New("nothing to change: give --title, --body, --base, --tag or --status, or run on a terminal to open the editor")
		}
		return taskEditInEditor(cmd, cfg, id)
	}
	var status model.TaskStatus
	if f.Changed("status") {
		if status, err = taskEditStatusOf(taskEditStatus); err != nil {
			return err
		}
	}
	var body string
	var early []task.Problem
	if f.Changed("body") {
		early = taskBodyProblem(cmd.InOrStdin(), taskEditBody, &body)
	}
	if err = taskUpdate(cmd.Context(), cfg, id, early, taskEditChange(f.Changed, body, status)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s updated\n", id)
	return err
}

// taskEditChange applies the edit flags that were given.
func taskEditChange(changed func(name string) bool, body string, status model.TaskStatus) func(*model.Task) {
	return func(t *model.Task) {
		if changed("title") {
			t.Title = taskEditTitle
		}
		if changed("body") {
			t.Body = body
		}
		if changed("base") {
			t.BaseBranch = taskEditBase
		}
		if changed("tag") {
			t.Tags = taskEditTags
		}
		if status != "" {
			t.Status = status
		}
	}
}

// taskNoEditorError says, in one line, how to edit without an editor set.
func taskNoEditorError() error {
	where := "the config file"
	if home, err := os.UserHomeDir(); err == nil {
		where = configFile(home)
	}
	return fmt.Errorf("no editor set: add editor = \"/path/to/editor\" to %s, or give --title, --body <file>|-, --base, --tag or --status",
		sanitize.Line(where, 200))
}

// taskIsTerminal reports whether in is a terminal (a character device).
func taskIsTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// taskEditInEditor writes task id's title, a blank line and its body to a
// new temp file (mode 0600), runs taskEditor on it, reads it back the same
// way and removes it. An unchanged or empty file changes nothing.
func taskEditInEditor(cmd *cobra.Command, cfg config.Config, id string) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	cur, found, err := st.GetTask(cmd.Context(), id)
	_ = st.Close()
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%s: no such task", id)
	}
	edited, err := taskRunEditor(cmd.Context(), cfg.EditorArgs(), id, cur.Title+"\n\n"+cur.Body+"\n")
	if err != nil {
		return err
	}
	title, body, _ := strings.Cut(edited, "\n")
	body = strings.TrimPrefix(body, "\n")
	next := task.Normalize(model.Task{Title: title, Body: body})
	out := cmd.OutOrStdout()
	if strings.TrimSpace(edited) == "" || (next.Title == cur.Title && next.Body == cur.Body) {
		_, err = fmt.Fprintf(out, "%s unchanged\n", id)
		return err
	}
	var early []task.Problem
	if len(body) > task.MaxBody {
		early = append(early, task.Problem{Field: "body", Problem: fmt.Sprintf("must be at most %d bytes", task.MaxBody)})
	}
	err = taskUpdate(cmd.Context(), cfg, id, early, func(t *model.Task) { t.Title, t.Body = title, body })
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s updated\n", id)
	return err
}

// taskRunEditor writes content to a new temp file named after id, runs
// taskEditor (argv) on it and returns what it then holds, bounded.
func taskRunEditor(ctx context.Context, argv []string, id, content string) (string, error) {
	f, err := os.CreateTemp("", "curia-"+id+"-*.md")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	_, err = f.WriteString(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if err = taskEditor(ctx, argv, path); err != nil {
		return "", fmt.Errorf("editor: %w", err)
	}
	edited, err := taskReadFile(nil, path, taskEditMax)
	if err != nil {
		return "", fmt.Errorf("edited file: %w", err)
	}
	return edited, nil
}

func runTaskReady(cmd *cobra.Command, args []string) error {
	id := args[0]
	if err := taskCheckID(id); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err = taskUpdate(cmd.Context(), cfg, id, nil, func(t *model.Task) { t.Status = model.TaskReady }); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s ready\n", id)
	return err
}

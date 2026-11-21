package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/linear"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
)

// taskImportMaxFiles is the most Backlog.md files one import reads.
const taskImportMaxFiles = 500

// taskImportBacklogMD is --backlog-md; tests reset it.
var taskImportBacklogMD bool

func init() {
	c := &cobra.Command{
		Use:         "import (--backlog-md [<dir>] | --linear [--repo <dir>] [--team <KEY>])",
		Short:       "Import tasks from Backlog.md or Linear",
		Annotations: userOnlyMark(),
		Long: "--backlog-md reads the task files in backlog/tasks/ of the repository holding\n" +
			"<dir> (default: the current directory): at most 500 regular .md files, by name.\n" +
			"A file that is not a task, or is Done, is skipped.\n\n" +
			"--linear sends one read-only query to linear.url of the config file, one\n" +
			"request per page (at most 4 pages of 50), with the API key from\n" +
			"CURIA_LINEAR_API_KEY in the Authorization header, and reads the issues\n" +
			"assigned to you in a backlog or unstarted state (of --team, else linear.team,\n" +
			"when given); if any page fails, nothing is imported. The issues become tasks of\n" +
			"the repository holding --repo, starting from its checked-out branch. An issue no\n" +
			"longer listed (closed, reassigned or deleted in Linear) leaves its task as it is.\n\n" +
			"A task imported before is updated (title, body and tags); its status and\n" +
			"priority stay as you set them.",
		Args: cobra.MaximumNArgs(1),
		RunE: userOnlyRunE("task import", "import tasks", linearRunImport),
	}
	c.Flags().BoolVar(&taskImportBacklogMD, "backlog-md", false, "import Backlog.md task files")
	c.Flags().BoolVar(&linearFlag, "linear", false, "import the Linear issues assigned to you")
	c.Flags().StringVar(&linearRepoFlag, "repo", ".", "with --linear: a directory inside the tasks' repository")
	c.Flags().StringVar(&linearTeamFlag, "team", "", "with --linear: only this team's issues (a key such as ENG)")
	taskCmd.AddCommand(c)
}

// taskBacklogFile is one Backlog.md file read for import.
type taskBacklogFile struct {
	name string
	data []byte
}

// taskImportCounts is what one import did.
type taskImportCounts struct{ imported, updated, skipped int }

// runTaskImport is the Backlog.md import.
func runTaskImport(cmd *cobra.Command, args []string) error {
	dir := "."
	if len(args) == 1 {
		dir = args[0]
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger, closeLog := taskLogger(cfg)
	repo, branch, err := taskMainRepo(cmd.Context(), dir, logger)
	closeLog()
	if err != nil {
		return err
	}
	if branch == "" {
		return fmt.Errorf("HEAD is detached in %s: check out the branch imported tasks start from", sanitize.Line(repo, 200))
	}
	files, unread, counts, err := taskReadBacklog(repo)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if unread > 0 {
		_, _ = fmt.Fprintf(out, "backlog/tasks holds %d more .md files: read the first %d by name\n", unread, taskImportMaxFiles)
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	now := clock()
	for _, f := range files {
		imp, perr := task.ParseBacklogMD(f.name, f.data)
		t, ps := taskCheck(model.Task{
			Title: imp.Title, Body: imp.Body, Tags: imp.Tags, Repo: repo, BaseBranch: branch,
			Status: model.TaskBacklog, Source: model.SourceBacklogMD, SourceRef: imp.Ref, CreatedAt: now,
		}, nil)
		if perr != nil || imp.Done || len(ps) > 0 {
			counts.skipped++
			continue
		}
		_, created, ierr := st.ImportTask(cmd.Context(), t)
		if ierr != nil {
			return ierr
		}
		if created {
			counts.imported++
		} else {
			counts.updated++
		}
	}
	_, err = fmt.Fprintf(out, "imported %d, updated %d, skipped %d\n", counts.imported, counts.updated, counts.skipped)
	return err
}

// taskReadBacklog reads the regular *.md files directly inside
// <repo>/backlog/tasks, the first taskImportMaxFiles by name, through an
// os.Root so neither the directory nor a file can lead out of the
// repository. It returns them, how many more there were (unread), and the
// count of .md entries skipped: not regular files, or too large.
func taskReadBacklog(repo string) (files []taskBacklogFile, unread int, counts taskImportCounts, err error) {
	root, err := os.OpenRoot(repo)
	if err != nil {
		return nil, 0, counts, err
	}
	defer func() { _ = root.Close() }()
	sub, err := root.OpenRoot(filepath.Join("backlog", "tasks"))
	if err != nil {
		return nil, 0, counts, fmt.Errorf("no backlog/tasks directory in %s: %w", sanitize.Line(repo, 200), err)
	}
	defer func() { _ = sub.Close() }()
	names, unread, skipped, err := taskBacklogNames(sub)
	if err != nil {
		return nil, 0, counts, err
	}
	counts.skipped = skipped
	for _, name := range names {
		data, ok := taskReadBacklogFile(sub, name)
		if !ok {
			counts.skipped++
			continue
		}
		files = append(files, taskBacklogFile{name: name, data: data})
	}
	return files, unread, counts, nil
}

// taskBacklogNames lists the regular *.md files in dir by name, at most
// taskImportMaxFiles, how many more there were, and how many .md entries
// were not regular files.
func taskBacklogNames(dir *os.Root) (names []string, unread, skipped int, err error) {
	d, err := dir.Open(".")
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, 0, 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		switch {
		case !strings.HasSuffix(e.Name(), ".md"):
		case !e.Type().IsRegular():
			skipped++
		case len(names) == taskImportMaxFiles:
			unread++
		default:
			names = append(names, e.Name())
		}
	}
	return names, unread, skipped, nil
}

// taskReadBacklogFile reads one file of dir, or false when it is not a
// regular file or is larger than task.MaxBacklogFile. The open does not
// block on a FIFO swapped in after the listing.
func taskReadBacklogFile(dir *os.Root, name string) ([]byte, bool) {
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > task.MaxBacklogFile {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, task.MaxBacklogFile+1))
	if err != nil || len(data) > task.MaxBacklogFile {
		return nil, false
	}
	return data, true
}

// ---- The Linear import (plan section 4.9) ----

const (
	// linearKeyVar holds the API key; it is never read from a file.
	linearKeyVar = "CURIA_LINEAR_API_KEY"
	// linearLimit bounds the whole Linear import.
	linearLimit = 2 * time.Minute
	// linearMostRead is the most issues one import reads (the client's cap).
	linearMostRead = linear.MaxPages * linear.PageSize
	// linearKeyRefusal is the answer to a missing or malformed key. It
	// never quotes the value or its length.
	linearKeyRefusal = "set " + linearKeyVar + " to a Linear API key (read access is enough); " +
		"keep the key in your system keychain (on macOS a Keychain item your shell exports), not in a file"
	// linearNoteRunes bounds the last column of a printed row.
	linearNoteRunes = 72
)

// What happened to one issue, as printed.
const (
	linearImported  = "imported"
	linearUpdated   = "updated"
	linearUnchanged = "unchanged"
	linearSkipped   = "skipped"
)

// --linear, --repo and --team; tests reset them.
var (
	linearFlag     bool
	linearRepoFlag string
	linearTeamFlag string
)

// linearClientFor builds the client of one import from the config file's
// URL and the key: the only way Curia reaches Linear. Tests replace it.
var linearClientFor = func(url, key string) linear.Client { return linear.Client{URL: url, Key: key} }

// linearRow is one printed line of an import: the issue, what happened,
// and the task's id and title or why the issue was skipped.
type linearRow struct {
	ref, outcome, note string
	replaced           string // the id of a task whose differing local text the import replaced
}

// linearResult is what one Linear import did.
type linearResult struct {
	rows                                  []linearRow
	imported, updated, unchanged, skipped int
	// capped is set when the client may have stopped at its last page.
	capped bool
}

func (r *linearResult) add(row linearRow) {
	r.rows = append(r.rows, row)
	switch row.outcome {
	case linearImported:
		r.imported++
	case linearUpdated:
		r.updated++
	case linearUnchanged:
		r.unchanged++
	default:
		r.skipped++
	}
}

// linearRunImport is `curia task import`: exactly one source, then its import.
func linearRunImport(cmd *cobra.Command, args []string) error {
	withLinear := cmd.Flags().Changed("repo") || cmd.Flags().Changed("team")
	switch {
	case taskImportBacklogMD == linearFlag:
		return errors.New("give exactly one of --backlog-md and --linear")
	case taskImportBacklogMD && withLinear:
		return errors.New("--repo and --team go with --linear; --backlog-md takes the directory as <dir>")
	case taskImportBacklogMD:
		return runTaskImport(cmd, args)
	case len(args) > 0:
		return errors.New("--linear takes no <dir>: give --repo <dir>")
	}
	return linearImportCmd(cmd)
}

// linearImportCmd checks the key and the team before anything is opened or sent,
// resolves the repository, imports and prints what it did.
func linearImportCmd(cmd *cobra.Command) error {
	key := os.Getenv(linearKeyVar)
	if !linear.ValidKey(key) {
		return errors.New(linearKeyRefusal)
	}
	if linearTeamFlag != "" && !linear.ValidTeam(linearTeamFlag) {
		return errors.New("--team must be a Linear team key: a capital letter, then up to 9 capitals or digits (for example ENG)")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	team := cfg.Linear.Team
	if cmd.Flags().Changed("team") {
		team = linearTeamFlag
	}
	logger, closeLog := taskLogger(cfg)
	repo, branch, err := taskMainRepo(cmd.Context(), linearRepoFlag, logger)
	closeLog()
	if err != nil {
		return err
	}
	if branch == "" {
		return fmt.Errorf("HEAD is detached in %s: check out the branch imported tasks start from", sanitize.Line(repo, 200))
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ctx, cancel := context.WithTimeout(cmd.Context(), linearLimit)
	defer cancel()
	res, err := linearImport(ctx, st, linearClientFor(cfg.Linear.URL, key), team, repo, branch, clock())
	if err != nil {
		return err
	}
	return linearPrint(cmd.OutOrStdout(), res)
}

// linearImport fetches the issues and stores them as tasks of repo from
// base. The client returns every page or nothing, so a failed fetch stores
// nothing. Each task is one store.ImportTask; a store error stops the
// import, and running it again repairs it (an import is idempotent).
func linearImport(ctx context.Context, st *store.Store, c linear.Client, team, repo, base string, now time.Time) (linearResult, error) {
	issues, err := c.Issues(ctx, team)
	if err != nil {
		return linearResult{}, linearFetchError(err, c.Key, team)
	}
	prior, err := linearPrior(ctx, st, repo)
	if err != nil {
		return linearResult{}, err
	}
	res := linearResult{capped: len(issues) >= linearMostRead}
	fields, invalid := linear.Map(issues)
	reasons := make(map[int]string, len(invalid))
	for _, inv := range invalid {
		reasons[inv.Index] = inv.Reason
	}
	next := 0
	for i, is := range issues {
		if reason, bad := reasons[i]; bad {
			res.add(linearRow{ref: linearSkippedRef(is, i), outcome: linearSkipped, note: reason})
			continue
		}
		f := fields[next]
		next++
		row, err := linearStore(ctx, st, f, prior, repo, base, now)
		if err != nil {
			return res, fmt.Errorf("store %s: %w (run the import again)", f.SourceRef, err)
		}
		res.add(row)
	}
	return res, nil
}

// linearFetchError says what a failed fetch means. The client's text never
// holds the URL, a header or the key.
func linearFetchError(err error, key, team string) error {
	var se *linear.StatusError
	switch {
	case errors.Is(err, linear.ErrInvalid) && linear.ValidKey(key) && (team == "" || linear.ValidTeam(team)):
		// The key and team are good, so the client refused the URL, which
		// passed the config file's own, wider check.
		return fmt.Errorf("linear.url in %s is not an address Curia sends to: it must be https://<host>/graphql, "+
			"or http://<loopback IP>/graphql with an IP such as 127.0.0.1 (not localhost); nothing was sent", configFile("~"))
	case errors.Is(err, linear.ErrInvalid):
		return fmt.Errorf("%w; nothing was sent", err)
	case errors.Is(err, linear.ErrUnauthorized):
		return fmt.Errorf("%w: Linear refused the key in %s; nothing was imported", err, linearKeyVar)
	case errors.As(err, &se) && se.RetryAfter > 0:
		return fmt.Errorf("%w: try again in %s; nothing was imported", err, se.RetryAfter)
	default:
		return fmt.Errorf("%w; nothing was imported", err)
	}
}

// linearPrior is the Linear tasks of repo by source ref, to tell an update
// from an import that changed nothing.
func linearPrior(ctx context.Context, st *store.Store, repo string) (map[string]model.Task, error) {
	ts, err := st.ListTasks(ctx, store.TaskFilter{Repo: repo})
	if err != nil {
		return nil, err
	}
	prior := map[string]model.Task{}
	for _, t := range ts {
		if t.Source == model.SourceLinear {
			prior[t.SourceRef] = t
		}
	}
	return prior, nil
}

// linearStore stores one mapped issue and says what changed.
func linearStore(ctx context.Context, st *store.Store, f linear.Fields, prior map[string]model.Task, repo, base string, now time.Time) (linearRow, error) {
	row := linearRow{ref: f.SourceRef, outcome: linearSkipped}
	t, reason := linearTask(f, repo, base, now)
	if reason != "" {
		row.note = reason
		return row, nil
	}
	stored, created, err := st.ImportTask(ctx, t)
	if err != nil {
		return row, err
	}
	before, seen := prior[f.SourceRef]
	switch {
	case created:
		row.outcome = linearImported
	case seen && before.Title == stored.Title && before.Body == stored.Body && slices.Equal(before.Tags, stored.Tags):
		row.outcome = linearUnchanged
	default:
		row.outcome = linearUpdated
		if seen && !created && (before.Title != stored.Title || before.Body != stored.Body || !slices.Equal(before.Tags, stored.Tags)) {
			// shortcut: a local edit cannot be told from a change in Linear (no copy of the last import is stored); a migration for one is not worth it.
			row.replaced = stored.ID
		}
	}
	row.note = stored.ID + " " + stored.Title
	return row, nil
}

// linearTask makes the task of one mapped issue for repo and base, or
// says why task.Validate still refuses it after task.Normalize. It is
// always a backlog task with source linear: Linear's state is never mapped.
func linearTask(f linear.Fields, repo, base string, now time.Time) (model.Task, string) {
	t, ps := taskCheck(model.Task{
		Title: f.Title, Body: f.Body, Tags: f.Tags, Priority: f.Priority, Repo: repo, BaseBranch: base,
		Status: model.TaskBacklog, Source: model.SourceLinear, SourceRef: f.SourceRef, CreatedAt: now,
	}, nil)
	if len(ps) > 0 {
		return model.Task{}, sanitize.Line(ps[0].Field+" "+ps[0].Problem, 200)
	}
	return t, ""
}

// linearSkippedRef names a skipped issue: its identifier cleaned, or its
// place in Linear's answer when it has none.
func linearSkippedRef(is linear.Issue, i int) string {
	if ref := sanitize.Line(is.Identifier, 24); ref != "" {
		return ref
	}
	return fmt.Sprintf("#%d", i+1)
}

// linearPrint writes one row per issue, the cap when it may have been
// reached, and the summary line. Every printed field is clean.
func linearPrint(w io.Writer, res linearResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range res.rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", sanitize.Line(r.ref, 24), r.outcome, sanitize.Line(r.note, linearNoteRunes))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range res.rows {
		if r.replaced != "" {
			_, _ = fmt.Fprintf(w, "%s: the local title, body or tags differ from Linear's and were replaced by Linear's\n", sanitize.Line(r.replaced, 24))
		}
	}
	if res.capped {
		_, _ = fmt.Fprintf(w, "imported the first %d of your assigned issues, the most one import reads: there may be more; narrow with --team <KEY>\n", linearMostRead)
	}
	_, err := fmt.Fprintf(w, "imported %d, updated %d, unchanged %d, skipped %d\n", res.imported, res.updated, res.unchanged, res.skipped)
	return err
}

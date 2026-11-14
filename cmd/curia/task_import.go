package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

// taskImportMaxFiles is the most Backlog.md files one import reads.
const taskImportMaxFiles = 500

// taskImportBacklogMD is --backlog-md; tests reset it.
var taskImportBacklogMD bool

func init() {
	c := &cobra.Command{
		Use:   "import --backlog-md [<dir>]",
		Short: "Import tasks from Backlog.md",
		Long: "Reads the task files in backlog/tasks/ of the repository holding <dir>\n" +
			"(default: the current directory): at most 500 regular .md files, by name.\n" +
			"A file that is not a task, or is Done, is skipped. A task imported before is\n" +
			"updated (title, body and tags); its status stays as you set it.",
		Args: cobra.MaximumNArgs(1),
		RunE: runTaskImport,
	}
	c.Flags().BoolVar(&taskImportBacklogMD, "backlog-md", false, "import Backlog.md task files")
	taskCmd.AddCommand(c)
}

// taskBacklogFile is one Backlog.md file read for import.
type taskBacklogFile struct {
	name string
	data []byte
}

// taskImportCounts is what one import did.
type taskImportCounts struct{ imported, updated, skipped int }

func runTaskImport(cmd *cobra.Command, args []string) error {
	if !taskImportBacklogMD {
		return errors.New("give --backlog-md: Backlog.md is the only source to import from")
	}
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

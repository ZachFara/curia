package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/export"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/ui"
)

// exportAll is `curia export obsidian --all`; tests reset it.
var exportAll bool

// exportRetry is `curia export obsidian --retry-skipped`; tests reset it.
var exportRetry bool

// Bounds of one Obsidian sweep (plan section 4.7).
const (
	exportMaxNotes     = 500             // notes a run writes
	exportMaxEntries   = 4096            // folder entries a run lists
	exportHeadBytes    = 4 << 10         // bytes of a candidate read for the marker
	exportAfterTimeout = 5 * time.Second // the sweep after an answer
	exportPathLimit    = 300             // runes of a printed path
	exportFailedLine   = "obsidian export failed: see the log"
	exportCommonsWarn  = "this is the agent's and your own text: read it before you publish it"
	exportTempPrefix   = ".curia-export-"
)

func init() {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export decisions: notes into an Obsidian folder, or an Agent Commons entry",
	}
	obsidian := &cobra.Command{
		Use:   "obsidian [<id>...]",
		Short: "Write decision notes into the Obsidian folder (the user only)",
		Long: "Writes a note for every decided decision that is due (never exported, or\n" +
			"changed since) into obsidian.dir, an existing folder. --all re-exports\n" +
			"every decided decision; ids export only those. A file of the note's name\n" +
			"without Curia's marker is left alone, reported with its path and skipped:\n" +
			"later runs pass it by until the decision is answered or changed again, or\n" +
			"--retry-skipped tries it anew. Nothing is deleted.",
		Annotations: userOnlyMark(),
		RunE:        userOnlyRunE("export obsidian", "write notes into the vault", runExportObsidian),
	}
	obsidian.Flags().BoolVar(&exportAll, "all", false, "re-export every decided decision")
	obsidian.Flags().BoolVar(&exportRetry, "retry-skipped", false, "also try the decisions skipped because of a foreign note")
	commons := &cobra.Command{
		Use:   "commons <id>",
		Short: "Print a decided global decision as an Agent Commons entry (the user only)",
		Long: "Prints the arguments of Agent Commons's propose tool for one decided,\n" +
			"not superseded, global decision. Sends nothing: read it, then publish it\n" +
			"yourself.",
		Args:        cobra.ExactArgs(1),
		Annotations: userOnlyMark(),
		RunE:        userOnlyRunE("export commons", "export a decision", runExportCommons),
	}
	cmd.AddCommand(obsidian, commons)
	rootCmd.AddCommand(cmd)
}

// exportFailure is one note that could not be written or marked.
type exportFailure struct {
	ID  string
	Err error
}

// exportSkip is a decision skipped because a foreign file holds its path.
type exportSkip struct {
	ID   string
	Name string // the foreign file's name inside the folder
}

// exportResult counts one sweep.
type exportResult struct {
	Written int
	Foreign []string        // ids whose note exists without Curia's marker
	Skipped []exportSkip    // the same, with the foreign file's name; remembered in the store
	Failed  []exportFailure // notes not written or not marked; the caller logs them
}

func runExportObsidian(cmd *cobra.Command, args []string) error {
	if exportAll && len(args) > 0 {
		return errors.New("--all exports every decision: give ids or --all, not both")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dir := cfg.Obsidian.Dir
	if dir == "" {
		return errors.New("obsidian.dir is not set")
	}
	if err = exportCheckDir(dir); err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	res, err := exportSweep(cmd.Context(), st, dir, exportAll, exportRetry, args, clock())
	exportLog(cfg.LogPath, res)
	if err != nil {
		return err
	}
	if len(res.Failed) == 0 || res.Written > 0 || len(res.Foreign) > 0 {
		if _, perr := fmt.Fprintln(cmd.OutOrStdout(), exportSummary(dir, res)); perr != nil {
			return perr
		}
	}
	for _, k := range res.Skipped {
		if _, perr := fmt.Fprintln(cmd.OutOrStdout(), exportSkipLine(dir, k)); perr != nil {
			return perr
		}
	}
	if n := len(res.Failed); n > 0 {
		return fmt.Errorf("%d %s failed: see the log", n, exportPlural(n, "note"))
	}
	return nil
}

// exportCheckDir requires dir to exist and be a directory; Curia never creates it.
func exportCheckDir(dir string) error {
	shown := sanitize.Line(dir, exportPathLimit)
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("obsidian.dir %s does not exist (Curia never creates it)", shown)
	case err != nil:
		return fmt.Errorf("obsidian.dir %s: %w", shown, err)
	case !info.IsDir():
		return fmt.Errorf("obsidian.dir %s is not a directory", shown)
	}
	return nil
}

// exportPlural is word, with an "s" unless n is 1.
func exportPlural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// exportSummary is the line `curia export obsidian` prints for res.
func exportSummary(dir string, res exportResult) string {
	if res.Written == 0 && len(res.Foreign) == 0 && len(res.Failed) == 0 {
		return "nothing to export"
	}
	s := fmt.Sprintf("exported %d %s to %s", res.Written, exportPlural(res.Written, "note"), sanitize.Line(dir, exportPathLimit))
	if n := len(res.Foreign); n > 0 {
		s += fmt.Sprintf(" (%d foreign %s left alone: %s)", n, exportPlural(n, "note"),
			sanitize.Line(strings.Join(res.Foreign, ", "), exportPathLimit))
	}
	return s
}

// exportSkipLine is the line printed for one skipped decision.
func exportSkipLine(dir string, k exportSkip) string {
	return fmt.Sprintf("skipped %s: a note of yours is at %s (not Curia's; left alone). Rename or remove it, or run with --retry-skipped",
		k.ID, sanitize.Line(filepath.Join(dir, k.Name), exportPathLimit))
}

// exportLog writes each failed note to the log at logPath, and each foreign note at INFO.
func exportLog(logPath string, res exportResult) {
	for _, f := range res.Failed {
		logWarn(logPath, "obsidian export: note failed", "id", f.ID, "err", f.Err.Error())
	}
	if len(res.Foreign) > 0 {
		if logger, closeLog, err := openLog(logPath); err == nil {
			logger.Info("obsidian export: foreign notes left alone and skipped", "ids", strings.Join(res.Foreign, ","))
			_ = closeLog()
		}
	}
}

// exportObsidian writes the notes that are due (all: every decided decision; ids: only those) into dir and marks them.
func exportObsidian(ctx context.Context, st *store.Store, dir string, all bool, ids []string, now time.Time) (exportResult, error) {
	return exportSweep(ctx, st, dir, all, false, ids, now)
}

// exportSweep is exportObsidian; retry also tries the decisions skipped
// because of a foreign note.
func exportSweep(ctx context.Context, st *store.Store, dir string, all, retry bool, ids []string, now time.Time) (exportResult, error) {
	if err := exportCheckDir(dir); err != nil {
		return exportResult{}, err
	}
	ds, err := exportDue(ctx, st, all, retry, ids)
	if err != nil {
		return exportResult{}, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return exportResult{}, fmt.Errorf("open obsidian.dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	names, err := exportList(root)
	if err != nil {
		return exportResult{}, err
	}
	var res exportResult
	for _, d := range ds {
		if err := ctx.Err(); err != nil {
			return res, err // the rest stay due for the next run
		}
		note := export.Note(d)
		state, foreignName, err := exportWrite(root, names, d, note)
		switch {
		case err != nil:
			res.Failed = append(res.Failed, exportFailure{d.ID, err})
			continue
		case state == exportForeign:
			res.Foreign = append(res.Foreign, d.ID)
			res.Skipped = append(res.Skipped, exportSkip{d.ID, foreignName})
			// Remembered (only for this version of the decision), so the
			// next sweep moves past it.
			if _, err := st.MarkExportSkipped(ctx, d, now); err != nil {
				res.Failed = append(res.Failed, exportFailure{d.ID, err})
			}
			continue
		case state == exportWritten:
			res.Written++
		}
		if err := exportMark(ctx, st, d, now); err != nil {
			res.Failed = append(res.Failed, exportFailure{d.ID, err})
		}
	}
	return res, nil
}

// exportDue is the decisions to export: the store's queue (at most
// exportMaxNotes), or the named ids, each of which must be a decided
// decision. A bad id refuses the whole list, so nothing is half done.
func exportDue(ctx context.Context, st *store.Store, all, retry bool, ids []string) ([]model.Decision, error) {
	if len(ids) == 0 {
		return st.ExportQueueWith(ctx, all, retry, exportMaxNotes)
	}
	if len(ids) > exportMaxNotes {
		return nil, fmt.Errorf("at most %d ids at a time", exportMaxNotes)
	}
	var out []model.Decision
	var bad []error
	for _, id := range ids {
		if !decision.ValidID(id) {
			bad = append(bad, fmt.Errorf("invalid decision id %q: want D-<number>", sanitize.Line(id, 40)))
			continue
		}
		if slices.ContainsFunc(out, func(d model.Decision) bool { return d.ID == id }) {
			continue
		}
		d, found, err := st.GetDecision(ctx, id)
		switch {
		case err != nil:
			return nil, err
		case !found:
			bad = append(bad, fmt.Errorf("no decision %s", id))
		case !decision.IsDecided(d.Status):
			bad = append(bad, fmt.Errorf("%s is not decided", id))
		default:
			out = append(out, d)
		}
	}
	if len(bad) > 0 {
		return nil, errors.Join(bad...)
	}
	return out, nil
}

// exportList is the names of at most exportMaxEntries entries of the folder.
func exportList(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("list obsidian.dir: %w", err)
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(exportMaxEntries)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("list obsidian.dir: %w", err)
	}
	return names, nil
}

// exportState is what exportWrite did with a note.
type exportState int

const (
	exportWritten   exportState = iota // created or replaced
	exportUnchanged                    // Curia's own note already holds this content
	exportForeign                      // a file of the note's name without Curia's marker: left alone
)

// exportExisting is Curia's own note of a decision, found in the folder.
type exportExisting struct {
	name string
	data []byte // at most the note's length plus one byte
	perm fs.FileMode
}

// exportWrite puts note in the folder: over Curia's own note of d under the
// name it has (only when the content differs, keeping its mode), or as a
// new file under export.NoteName (mode 0600) that never replaces anything.
// Any other file of the note's name makes it foreign, and its name is
// returned. Curia's own earlier note is recognized by its marker alone
// (export.OwnsNote), not by any mark in the store, so a note written before
// a crash that left the decision unmarked is updated, never foreign.
func exportWrite(root *os.Root, names []string, d model.Decision, note []byte) (exportState, string, error) {
	for range 2 {
		own, foreign, err := exportFind(root, names, d, len(note))
		switch {
		case err != nil:
			return 0, "", err
		case own != nil && bytes.Equal(own.data, note):
			return exportUnchanged, "", nil
		case own != nil:
			return exportWritten, "", exportReplace(root, own, note)
		case foreign != "":
			return exportForeign, foreign, nil
		}
		err = exportCreate(root, export.NoteName(d), note)
		if !errors.Is(err, fs.ErrExist) {
			return exportWritten, "", err
		}
		// Created meanwhile (another sweep, or by hand): look again once.
	}
	return exportForeign, export.NoteName(d), nil
}

// exportFind looks at every candidate of d (a listed name that
// export.IsNoteOf accepts, and the computed name): the first in name order
// that is a regular file holding Curia's marker is returned; foreign is the
// first candidate that exists and is not Curia's ("" when none).
func exportFind(root *os.Root, names []string, d model.Decision, noteLen int) (*exportExisting, string, error) {
	cands := []string{export.NoteName(d)}
	for _, n := range names {
		if export.IsNoteOf(n, d.ID) && !slices.Contains(cands, n) {
			cands = append(cands, n)
		}
	}
	slices.Sort(cands)
	foreign := ""
	for _, name := range cands {
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("look at %s: %w", sanitize.Line(name, exportPathLimit), err)
		}
		if !info.Mode().IsRegular() { // a directory, a symlink, a device
			foreign = cmp.Or(foreign, name)
			continue
		}
		data, err := exportReadOwn(root, name, info, noteLen)
		if err != nil {
			return nil, "", err
		}
		if data == nil || !export.OwnsNote(data[:min(len(data), exportHeadBytes)], d.ID) {
			foreign = cmp.Or(foreign, name)
			continue
		}
		return &exportExisting{name: name, data: data, perm: info.Mode().Perm()}, "", nil
	}
	return nil, foreign, nil
}

// exportReadOwn reads the start of the regular file name (enough for the
// marker and to compare with a note of noteLen bytes); nil when the file
// opened is not the one Lstat saw.
func exportReadOwn(root *os.Root, name string, seen fs.FileInfo, noteLen int) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sanitize.Line(name, exportPathLimit), err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sanitize.Line(name, exportPathLimit), err)
	}
	if !os.SameFile(seen, info) || !info.Mode().IsRegular() {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max(exportHeadBytes, noteLen+1))))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sanitize.Line(name, exportPathLimit), err)
	}
	return data, nil
}

// exportReplace writes note to a temporary file with own's mode and renames
// it over own, inside the root.
func exportReplace(root *os.Root, own *exportExisting, note []byte) error {
	tmp, err := exportTemp(root, note, own.perm)
	if err != nil {
		return err
	}
	if err := root.Rename(tmp, own.name); err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("replace %s: %w", sanitize.Line(own.name, exportPathLimit), err)
	}
	return nil
}

// exportCreate writes note to a temporary file and links it to name, which
// fails with fs.ErrExist when anything holds that name, so a new note never
// replaces a file; the temporary name is then removed.
func exportCreate(root *os.Root, name string, note []byte) error {
	tmp, err := exportTemp(root, note, 0o600)
	if err != nil {
		return err
	}
	lerr := root.Link(tmp, name)
	rerr := root.Remove(tmp)
	if lerr != nil {
		return fmt.Errorf("create %s: %w", sanitize.Line(name, exportPathLimit), lerr)
	}
	if rerr != nil {
		return fmt.Errorf("remove temporary %s: %w", tmp, rerr)
	}
	return nil
}

// exportTemp writes data to a new temporary file inside the root (created
// 0600, then given perm) and syncs it; on failure it removes that file and
// nothing else.
func exportTemp(root *os.Root, data []byte, perm fs.FileMode) (string, error) {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	name := exportTempPrefix + hex.EncodeToString(rnd[:]) + ".tmp"
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create a temporary note: %w", err)
	}
	_, werr := f.Write(data)
	if werr == nil && perm != 0o600 {
		werr = f.Chmod(perm)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = root.Remove(name)
		return "", fmt.Errorf("write a temporary note: %w", err)
	}
	return name, nil
}

// exportMark marks decision d exported at now, its note (built from d, as
// read before the note was written) being in the vault. The store marks
// only if the decision is still exactly that version, in one statement, so
// a note built from an older state is never marked: the decision stays due
// and the next sweep writes it again. The order is write, then mark, so a
// crash between them leaves the decision unmarked and, the note being
// recognized as Curia's own, the next sweep updates or confirms it.
func exportMark(ctx context.Context, st *store.Store, d model.Decision, now time.Time) error {
	_, err := st.MarkExportedIf(ctx, d, now)
	return err
}

// exportAfterAnswer runs the due sweep when obsidian.dir is set; it returns "" or the one line to show.
func exportAfterAnswer(ctx context.Context, st *store.Store, cfg config.Config, logPath string, now time.Time) (line string) {
	if cfg.Obsidian.Dir == "" {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			logWarn(logPath, "obsidian export panicked", "panic", fmt.Sprint(r))
			line = exportFailedLine
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, exportAfterTimeout)
	defer cancel()
	res, err := exportObsidian(ctx, st, cfg.Obsidian.Dir, false, nil, now)
	exportLog(logPath, res)
	if err != nil {
		logWarn(logPath, "obsidian export failed", "err", err.Error())
		return exportFailedLine
	}
	if len(res.Failed) > 0 {
		return exportFailedLine
	}
	return ""
}

// exportCountingSource is the decide view's source, counting the answers
// it stored, so the sweep runs when the popup closes after one.
type exportCountingSource struct {
	ui.DecideSource
	answered atomic.Int64
}

func (s *exportCountingSource) Answer(ctx context.Context, id string, a model.Answer) (model.Outcome, error) {
	out, err := s.DecideSource.Answer(ctx, id, a)
	if err == nil {
		s.answered.Add(1)
	}
	return out, err
}

// exportBackground starts the Obsidian sweep after an answer given in the
// dashboard's decide view, as `curia decide` does after its answer, but in
// the background: Kick never blocks, at most one sweep runs at a time (a
// kick during a sweep asks for one more run after it), each sweep is
// bounded by exportAfterTimeout, and the sweep's failure is logged once by
// exportAfterAnswer and never shown. Wait lets a running sweep finish
// (bounded) before the store is closed.
type exportBackground struct {
	st      *store.Store
	cfg     config.Config
	running atomic.Bool
	again   atomic.Bool
	wg      sync.WaitGroup
}

// Kick starts a sweep when obsidian.dir is set and none is running.
func (b *exportBackground) Kick() {
	if b.cfg.Obsidian.Dir == "" {
		return
	}
	if !b.running.CompareAndSwap(false, true) {
		b.again.Store(true)
		return
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			b.again.Store(false)
			exportAfterAnswer(context.Background(), b.st, b.cfg, b.cfg.LogPath, clock())
			b.running.Store(false)
			if !b.again.Load() || !b.running.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

// Wait blocks until the running sweep, if any, has finished.
func (b *exportBackground) Wait() { b.wg.Wait() }

// exportKickSource is a decide view's source that starts a background
// sweep after every answer that was stored; the answer itself is untouched.
type exportKickSource struct {
	ui.DecideSource
	bg *exportBackground
}

func (s exportKickSource) Answer(ctx context.Context, id string, a model.Answer) (model.Outcome, error) {
	out, err := s.DecideSource.Answer(ctx, id, a)
	if err == nil {
		s.bg.Kick()
	}
	return out, err
}

func runExportCommons(cmd *cobra.Command, args []string) error {
	id := args[0]
	if !decision.ValidID(id) {
		return fmt.Errorf("invalid decision id %q: want D-<number>", sanitize.Line(id, 40))
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
	d, found, err := st.GetDecision(cmd.Context(), id)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("no decision %s", id)
	}
	e, err := export.CommonsEntry(d)
	if errors.Is(err, export.ErrNotPublishable) {
		return decideErr{id + " is not a decided global decision", err}
	}
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err = enc.Encode(e); err != nil {
		return err
	}
	if _, err = io.WriteString(cmd.OutOrStdout(), escapeUnprintable(buf.String())); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.ErrOrStderr(), exportCommonsWarn)
	return err
}

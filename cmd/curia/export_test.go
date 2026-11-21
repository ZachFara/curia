package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/export"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/ui"
)

// exportSpec is one seeded decision: decided ones are answered B with a
// reason through the store, at exportAnswered.
type exportSpec struct {
	question string
	scope    model.Scope
	decided  bool
}

var exportAnswered = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

// exportClock is the test's wall clock; set moves it.
type exportClock struct{ t time.Time }

func (c *exportClock) now() time.Time { return c.t }

// exportHome isolates HOME and the store, writes a config whose obsidian.dir
// is vault (none when empty), fixes the clock at 12:00, makes the caller the
// user at a terminal and seeds specs as D-1, D-2, ... It returns the
// database path and the clock.
func exportHome(t *testing.T, vault string, specs ...exportSpec) (string, *exportClock) {
	t.Helper()
	db := testkit.HookHome(t)
	useFakeTmux(t)
	userTerminalAs(t, true)
	exportWriteConfig(t, vault)
	c := &exportClock{t: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)}
	prev := clock
	clock = c.now
	t.Cleanup(func() {
		clock = prev
		exportAll, exportRetry = false, false
		decisionsStatus, decisionsJSON, decisionsScope = "open", false, ""
		decisionsSupersede, decisionsSupersededBy = false, ""
	})
	exportSeed(t, db, specs...)
	return db, c
}

// exportWriteConfig writes the config file with obsidian.dir set to vault,
// or with no obsidian section when vault is empty.
func exportWriteConfig(t *testing.T, vault string) {
	t.Helper()
	p := configFile(os.Getenv("HOME"))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	if vault != "" {
		body = "[obsidian]\ndir = " + strconv.Quote(vault) + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// exportSeed creates the specs in order and answers the decided ones.
func exportSeed(t *testing.T, db string, specs ...exportSpec) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	for i, sp := range specs {
		scope := sp.scope
		if scope == "" {
			scope = model.ScopeRepo
		}
		d := model.Decision{
			SessionID: "s1", Repo: "/src/api-server", TaskID: "T-42", Question: sp.question,
			Context: "We publish events.",
			Options: []model.Option{
				{Key: "A", Label: "Postgres", Tradeoffs: "Durable."},
				{Key: "B", Label: "Redis", Tradeoffs: "Fast."},
			},
			Recommendation: "A", Rationale: "Replays need history.", Reversibility: model.ReversCostly,
			Mode: model.ModeBlocking, Scope: scope, Tags: []string{"db", "events"}, Status: model.StatusOpen,
			ArgsHash: fmt.Sprintf("hash-%d-%s", i, sp.question), Delivery: model.DeliveryNone,
			CreatedAt: exportAnswered.Add(-time.Hour),
		}
		stored, _, err := st.CreateDecision(ctx, d)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if sp.decided {
			a := model.Answer{Key: "B", Reasoning: "Speed matters more here.", Via: model.AnsweredCLI}
			if _, err := st.AnswerDecision(ctx, stored.ID, a, exportAnswered); err != nil {
				t.Fatalf("answer %s: %v", stored.ID, err)
			}
		}
	}
}

// exportRun runs `curia <args>` in-process and returns stdout and stderr apart.
func exportRun(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	exportAll, exportRetry = false, false
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

// exportObsidianOK runs `curia export obsidian <args>` and fails on an error.
func exportObsidianOK(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := exportRun(t, append([]string{"export", "obsidian"}, args...)...)
	if err != nil {
		t.Fatalf("export obsidian %v: %v\nstdout %q\nstderr %q", args, err, out, errOut)
	}
	return strings.TrimSpace(out)
}

// exportFiles maps every entry name in dir to its content ("" for a
// non-regular entry) and mode.
type exportEntry struct {
	data string
	mode fs.FileMode
	link string
}

// exportSnapshot reads every entry under dir, not following symlinks.
func exportSnapshot(t *testing.T, dir string) map[string]exportEntry {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	out := map[string]exportEntry{}
	err = fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := root.Lstat(p)
		if err != nil {
			return err
		}
		e := exportEntry{mode: info.Mode()}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			e.link, _ = root.Readlink(p)
		case info.Mode().IsRegular():
			b, rerr := root.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			e.data = string(b)
		}
		out[p] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// exportGet reads decision id from the store.
func exportGet(t *testing.T, db, id string) model.Decision {
	t.Helper()
	return decisionsGet(t, db, id)
}

// exportNames lists dir's entry names, sorted.
func exportNames(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

// exportRead reads a file in the vault.
func exportRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// exportIsDue says whether id is in the store's export queue.
func exportIsDue(t *testing.T, db, id string) bool {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	q, err := st.ExportQueue(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(q, func(d model.Decision) bool { return d.ID == id })
}

func TestExportWritesDueNotes(t *testing.T) {
	vault := t.TempDir()
	db, _ := exportHome(t, vault,
		exportSpec{question: "Postgres or Redis for events?", decided: true},
		exportSpec{question: "Which index?", decided: true},
		exportSpec{question: "Which queue?"})

	out := exportObsidianOK(t)
	if want := "exported 2 notes to " + vault; out != want {
		t.Errorf("output %q, want %q", out, want)
	}
	var want []string
	for _, id := range []string{"D-1", "D-2"} {
		d := exportGet(t, db, id)
		name := export.NoteName(d)
		want = append(want, name)
		if got := exportRead(t, filepath.Join(vault, name)); !bytes.Equal(got, export.Note(d)) {
			t.Errorf("%s content:\n%s\nwant:\n%s", name, got, export.Note(d))
		}
		info, err := os.Stat(filepath.Join(vault, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", name, info.Mode().Perm())
		}
		if d.ExportedAt.IsZero() {
			t.Errorf("%s not marked exported", id)
		}
	}
	slices.Sort(want)
	if got := exportNames(t, vault); !slices.Equal(got, want) {
		t.Errorf("vault holds %q, want %q", got, want)
	}
	if d := exportGet(t, db, "D-3"); !d.ExportedAt.IsZero() {
		t.Errorf("open D-3 marked exported at %v", d.ExportedAt)
	}
}

func TestExportSecondRunWritesNothing(t *testing.T) {
	vault := t.TempDir()
	exportHome(t, vault,
		exportSpec{question: "Postgres or Redis for events?", decided: true},
		exportSpec{question: "Which index?", decided: true})
	exportObsidianOK(t)
	// Old modification times, so a rewrite in the same second still shows.
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range exportNames(t, vault) {
		if err := os.Chtimes(filepath.Join(vault, n), old, old); err != nil {
			t.Fatal(err)
		}
	}
	before := exportSnapshot(t, vault)

	if out := exportObsidianOK(t); out != "nothing to export" {
		t.Errorf("second run: %q, want nothing to export", out)
	}
	for _, n := range exportNames(t, vault) {
		info, err := os.Stat(filepath.Join(vault, n))
		if err != nil {
			t.Fatal(err)
		}
		if !info.ModTime().Equal(old) {
			t.Errorf("%s modified at %v by the second run", n, info.ModTime())
		}
	}
	if after := exportSnapshot(t, vault); !exportSameTree(before, after) {
		t.Errorf("second run changed the vault:\n%v\n%v", before, after)
	}
}

// exportSameTree compares two snapshots.
func exportSameTree(a, b map[string]exportEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func TestExportAgainAfterSupersede(t *testing.T) {
	vault := t.TempDir()
	db, c := exportHome(t, vault, exportSpec{question: "Postgres or Redis for events?", decided: true})
	exportObsidianOK(t)
	name := export.NoteName(exportGet(t, db, "D-1"))

	c.t = c.t.Add(time.Hour)
	if out, err := runRoot(t, "decisions", "edit", "D-1", "--supersede"); err != nil {
		t.Fatalf("supersede: %v\n%s", err, out)
	}
	c.t = c.t.Add(time.Hour)
	if out := exportObsidianOK(t); out != "exported 1 note to "+vault {
		t.Errorf("output %q", out)
	}
	d := exportGet(t, db, "D-1")
	got := exportRead(t, filepath.Join(vault, name))
	if !bytes.Equal(got, export.Note(d)) || !strings.Contains(string(got), "\nstatus: superseded\n") {
		t.Errorf("note after supersede:\n%s", got)
	}
	if got := exportNames(t, vault); !slices.Equal(got, []string{name}) {
		t.Errorf("vault holds %q, want only %q", got, name)
	}
}

func TestExportAgainAfterRescope(t *testing.T) {
	vault := t.TempDir()
	db, _ := exportHome(t, vault, exportSpec{question: "Postgres or Redis for events?", decided: true})
	exportObsidianOK(t)
	if out, err := runRoot(t, "decisions", "edit", "D-1", "--scope", "global"); err != nil {
		t.Fatalf("rescope: %v\n%s", err, out)
	}
	if out := exportObsidianOK(t); out != "exported 1 note to "+vault {
		t.Errorf("output %q", out)
	}
	d := exportGet(t, db, "D-1")
	if got := exportRead(t, filepath.Join(vault, export.NoteName(d))); !strings.Contains(string(got), "\nscope: global\n") {
		t.Errorf("note after rescope:\n%s", got)
	}
}

func TestExportKeepsMyRename(t *testing.T) {
	vault := t.TempDir()
	db, _ := exportHome(t, vault, exportSpec{question: "Postgres or Redis for events?", decided: true})
	exportObsidianOK(t)
	d := exportGet(t, db, "D-1")
	mine := filepath.Join(vault, "D-1 database choice.md")
	if err := os.Rename(filepath.Join(vault, export.NoteName(d)), mine); err != nil {
		t.Fatal(err)
	}
	// An edit of mine below the marker, and a mode of mine.
	if err := os.WriteFile(mine, append(export.Note(d), "my edit\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mine, 0o400); err != nil {
		t.Fatal(err)
	}

	if out := exportObsidianOK(t, "--all"); out != "exported 1 note to "+vault {
		t.Errorf("output %q", out)
	}
	if got := exportNames(t, vault); !slices.Equal(got, []string{"D-1 database choice.md"}) {
		t.Errorf("vault holds %q, want only my renamed note", got)
	}
	if got := exportRead(t, mine); !bytes.Equal(got, export.Note(d)) {
		t.Errorf("renamed note not rewritten:\n%s", got)
	}
	info, err := os.Stat(mine)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("rewritten note mode %v, want mine (0400)", info.Mode().Perm())
	}
}

func TestExportAllLeavesCurrentNotesAlone(t *testing.T) {
	vault := t.TempDir()
	exportHome(t, vault, exportSpec{question: "Postgres or Redis for events?", decided: true})
	exportObsidianOK(t)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	name := exportNames(t, vault)[0]
	if err := os.Chtimes(filepath.Join(vault, name), old, old); err != nil {
		t.Fatal(err)
	}
	if out := exportObsidianOK(t, "--all"); out != "nothing to export" {
		t.Errorf("--all over current notes: %q", out)
	}
	info, err := os.Stat(filepath.Join(vault, name))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("a note whose content is current was rewritten (%v)", info.ModTime())
	}
}

func TestExportLeavesForeignNote(t *testing.T) {
	vault := t.TempDir()
	db, _ := exportHome(t, vault,
		exportSpec{question: "Postgres or Redis for events?", decided: true},
		exportSpec{question: "Which index?", decided: true},
		exportSpec{question: "Which queue?", decided: true},
		exportSpec{question: "Which cache?", decided: true})
	d3, d4 := exportGet(t, db, "D-3"), exportGet(t, db, "D-4")
	foreign := map[string]string{
		// A hand-written note for D-2.
		"D-2 my own notes.md": "hand written\n",
		// D-3's computed name, holding another decision's marker.
		export.NoteName(d3): "---\ncuria_id: D-30\n---\nmine\n",
		// D-4's computed name, the marker outside the front matter.
		export.NoteName(d4): "# notes\n\ncuria_id: D-4\n",
	}
	for n, body := range foreign {
		if err := os.WriteFile(filepath.Join(vault, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := exportSnapshot(t, vault)

	out := exportObsidianOK(t)
	want := "exported 1 note to " + vault + " (3 foreign notes left alone: D-2, D-3, D-4)\n" +
		"skipped D-2: a note of yours is at " + filepath.Join(vault, "D-2 my own notes.md") + " (not Curia's; left alone). Rename or remove it, or run with --retry-skipped\n" +
		"skipped D-3: a note of yours is at " + filepath.Join(vault, export.NoteName(d3)) + " (not Curia's; left alone). Rename or remove it, or run with --retry-skipped\n" +
		"skipped D-4: a note of yours is at " + filepath.Join(vault, export.NoteName(d4)) + " (not Curia's; left alone). Rename or remove it, or run with --retry-skipped"
	if out != want {
		t.Errorf("output %q, want %q", out, want)
	}
	after := exportSnapshot(t, vault)
	for n := range foreign {
		if after[n] != before[n] {
			t.Errorf("foreign note %q changed: %+v -> %+v", n, before[n], after[n])
		}
	}
	for _, id := range []string{"D-2", "D-3", "D-4"} {
		if d := exportGet(t, db, id); !d.ExportedAt.IsZero() || exportIsDue(t, db, id) {
			t.Errorf("%s marked exported though its note is foreign, or not skipped", id)
		}
	}
	if !strings.Contains(strings.Join(exportNames(t, vault), "|"), "D-1 ") {
		t.Errorf("D-1 not written: %q", exportNames(t, vault))
	}
}

func TestExportStaysInsideFolder(t *testing.T) {
	outside := t.TempDir()
	hosts := filepath.Join(outside, "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(outside, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The configured folder is itself a symlink to the real folder.
	realDir := t.TempDir()
	vault := filepath.Join(t.TempDir(), "Decisions")
	if err := os.Symlink(realDir, vault); err != nil {
		t.Fatal(err)
	}
	db, _ := exportHome(t, vault,
		exportSpec{question: "../../escape", decided: true},
		exportSpec{question: "/etc/passwd", decided: true},
		exportSpec{question: "x", decided: true},
		exportSpec{question: "Which cache?", decided: true},
		exportSpec{question: "Which queue?", decided: true},
		exportSpec{question: "Which format?", decided: true})
	d6 := exportGet(t, db, "D-6")
	links := map[string]string{
		"D-3 x.md":          hosts,                                   // a note name that is a symlink outside
		"D-5 sub.md":        filepath.Join(outside, "sub"),           // a symlinked directory
		export.NoteName(d6): filepath.Join(outside, "sub", "new.md"), // a dangling link at the new name
	}
	for n, target := range links {
		if err := os.Symlink(target, filepath.Join(realDir, n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(realDir, "D-4 dir.md"), 0o700); err != nil { // a directory under a note's name
		t.Fatal(err)
	}
	outBefore := exportSnapshot(t, outside)
	parentBefore := exportSnapshot(t, filepath.Dir(vault))

	out, _, _ := strings.Cut(exportObsidianOK(t), "\n")
	if want := "exported 2 notes to " + vault + " (4 foreign notes left alone: D-3, D-4, D-5, D-6)"; out != want {
		t.Errorf("output %q, want %q", out, want)
	}
	if after := exportSnapshot(t, outside); !exportSameTree(outBefore, after) {
		t.Errorf("the export changed a directory outside the folder:\n%v\n%v", outBefore, after)
	}
	if after := exportSnapshot(t, filepath.Dir(vault)); !exportSameTree(parentBefore, after) {
		t.Errorf("the export changed the folder's parent:\n%v\n%v", parentBefore, after)
	}
	for n, target := range links {
		if got, err := os.Readlink(filepath.Join(realDir, n)); err != nil || got != target {
			t.Errorf("symlink %q = %q, %v; want it untouched -> %q", n, got, err, target)
		}
	}
	names := exportNames(t, realDir)
	for _, want := range []string{"D-1 escape.md", "D-2 etc passwd.md"} {
		if !slices.Contains(names, want) {
			t.Errorf("resolved folder holds %q, want %q", names, want)
		}
	}
}

func TestExportNeverDeletes(t *testing.T) {
	vault := t.TempDir()
	db, _ := exportHome(t, vault,
		exportSpec{question: "Postgres or Redis for events?", decided: true},
		exportSpec{question: "Which index?", decided: true},
		exportSpec{question: "Which queue?", decided: true})
	exportObsidianOK(t)
	// My files beside Curia's: a foreign note, an unrelated note, a renamed
	// note of Curia's and a sub-folder.
	for n, body := range map[string]string{
		"D-3 mine.md": "hand written\n", "Inbox.md": "todo\n", "D-10 other.md": "---\ncuria_id: D-10\n---\n",
	} {
		if err := os.WriteFile(filepath.Join(vault, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(vault, "attachments"), 0o700); err != nil {
		t.Fatal(err)
	}
	d2 := exportGet(t, db, "D-2")
	if err := os.Rename(filepath.Join(vault, export.NoteName(d2)), filepath.Join(vault, "D-2 renamed.md")); err != nil {
		t.Fatal(err)
	}
	before := exportSnapshot(t, vault)

	exportObsidianOK(t, "--all")
	after := exportSnapshot(t, vault)
	for n := range before {
		if _, ok := after[n]; !ok {
			t.Errorf("%q disappeared", n)
		}
	}
	for n := range after {
		if strings.HasPrefix(n, ".curia") {
			t.Errorf("temporary file %q left behind", n)
		}
	}
	if after["D-3 mine.md"] != before["D-3 mine.md"] || after["Inbox.md"] != before["Inbox.md"] {
		t.Error("a file of mine changed")
	}
}

func TestExportByID(t *testing.T) {
	vault := t.TempDir()
	db, _ := exportHome(t, vault,
		exportSpec{question: "Postgres or Redis for events?", decided: true},
		exportSpec{question: "Which index?", decided: true},
		exportSpec{question: "Which queue?", decided: true},
		exportSpec{question: "Which cache?"})

	if out := exportObsidianOK(t, "D-1"); out != "exported 1 note to "+vault {
		t.Errorf("output %q", out)
	}
	if got, want := exportNames(t, vault), []string{export.NoteName(exportGet(t, db, "D-1"))}; !slices.Equal(got, want) {
		t.Errorf("vault holds %q, want %q", got, want)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"D-99"}, "no decision D-99"},
		{[]string{"D-4"}, "D-4 is not decided"},
		{[]string{"D-2", "D-99"}, "no decision D-99"},
		{[]string{"bogus"}, `invalid decision id "bogus"`},
		{[]string{"--all", "D-2"}, "--all exports every decision"},
	} {
		out, _, err := exportRun(t, append([]string{"export", "obsidian"}, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("export obsidian %v = %v (output %q), want %q", c.args, err, out, c.want)
		}
	}
	// A refused list writes nothing, not even its good ids.
	if got := exportNames(t, vault); len(got) != 1 {
		t.Errorf("vault holds %q after refused runs", got)
	}
	if d := exportGet(t, db, "D-2"); !d.ExportedAt.IsZero() {
		t.Error("D-2 exported by a refused run")
	}
}

func TestExportFolderUnsetOrMissing(t *testing.T) {
	exportHome(t, "", exportSpec{question: "Postgres or Redis for events?", decided: true})
	if _, _, err := exportRun(t, "export", "obsidian"); err == nil || err.Error() != "obsidian.dir is not set" {
		t.Errorf("unset folder: %v", err)
	}

	parent := t.TempDir()
	missing := filepath.Join(parent, "Decisions")
	file := filepath.Join(parent, "notes.md")
	if err := os.WriteFile(file, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{missing, file} {
		exportWriteConfig(t, dir)
		_, _, err := exportRun(t, "export", "obsidian")
		if err == nil || !strings.Contains(err.Error(), dir) {
			t.Errorf("folder %s: %v, want an error naming it", dir, err)
		}
	}
	if got := exportNames(t, parent); !slices.Equal(got, []string{"notes.md"}) {
		t.Errorf("parent holds %q: the export created something", got)
	}
	if got := exportRead(t, file); string(got) != "mine\n" {
		t.Errorf("file changed: %q", got)
	}
}

func TestExportBound(t *testing.T) {
	vault := t.TempDir()
	specs := make([]exportSpec, 600)
	for i := range specs {
		specs[i] = exportSpec{question: fmt.Sprintf("Question %d?", i+1), decided: true}
	}
	exportHome(t, vault, specs...)
	if out := exportObsidianOK(t); out != "exported 500 notes to "+vault {
		t.Errorf("first run: %q", out)
	}
	if out := exportObsidianOK(t); out != "exported 100 notes to "+vault {
		t.Errorf("second run: %q", out)
	}
	if out := exportObsidianOK(t); out != "nothing to export" {
		t.Errorf("third run: %q", out)
	}
	if n := len(exportNames(t, vault)); n != 600 {
		t.Errorf("vault holds %d notes, want 600", n)
	}
}

func TestExportNeverMarksStaleNote(t *testing.T) {
	_, c := exportHome(t, t.TempDir(), exportSpec{question: "Postgres or Redis for events?", decided: true})
	db := os.Getenv("CURIA_DB")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	built, _, err := st.GetDecision(ctx, "D-1")
	if err != nil {
		t.Fatal(err)
	}

	// A rescope lands between reading the decision and marking it.
	if _, err := st.EditDecision(ctx, "D-1", model.DecisionEdit{Scope: model.ScopeGlobal}, c.t); err != nil {
		t.Fatal(err)
	}
	if err := exportMark(ctx, st, built, c.t); err != nil {
		t.Fatal(err)
	}
	if !exportIsDue(t, db, "D-1") {
		t.Error("a note built before a rescope was marked current")
	}

	// A note built from the current state is marked.
	d, _, _ := st.GetDecision(ctx, "D-1")
	if err := exportMark(ctx, st, d, c.t); err != nil {
		t.Fatal(err)
	}
	if exportIsDue(t, db, "D-1") {
		t.Error("a current note was not marked")
	}
}

// A crash after the note is written and before the mark leaves the decision
// unmarked; the next sweep must treat the note as Curia's own (by its
// marker), update it when the decision changed meanwhile, and mark it.
func TestExportCrashBeforeMarkRecognizesOwnNote(t *testing.T) {
	vault := t.TempDir()
	db, c := exportHome(t, vault,
		exportSpec{question: "Postgres or Redis for events?", decided: true},
		exportSpec{question: "Which index?", decided: true})
	exportObsidianOK(t)
	names := exportNames(t, vault)
	// The crash: both notes are in the vault, neither is marked; D-2 also
	// changed before the next sweep.
	raw := rawExportDB(t, db)
	if _, err := raw.Exec(`UPDATE decisions SET exported_at = NULL`); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EditDecision(context.Background(), "D-2", model.DecisionEdit{Scope: model.ScopeGlobal}, c.t); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	out := exportObsidianOK(t)
	if want := "exported 1 note to " + vault; out != want {
		t.Errorf("after the crash: %q, want %q (D-2 updated, D-1 confirmed, none foreign)", out, want)
	}
	if got := exportNames(t, vault); !slices.Equal(got, names) {
		t.Errorf("vault names changed: %q -> %q", names, got)
	}
	if !strings.Contains(string(exportRead(t, filepath.Join(vault, export.NoteName(exportGet(t, db, "D-2"))))), "scope: global") {
		t.Error("D-2's own earlier note was not updated")
	}
	for _, id := range []string{"D-1", "D-2"} {
		if exportIsDue(t, db, id) {
			t.Errorf("%s still due after the recovering sweep", id)
		}
	}
}

// A decision that changes between the sweep's read and its mark is not
// marked: it stays due, and the next sweep writes it again.
func TestExportChangeBetweenWriteAndMarkStaysDue(t *testing.T) {
	vault := t.TempDir()
	db, c := exportHome(t, vault, exportSpec{question: "Postgres or Redis for events?", decided: true})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	d, _, _ := st.GetDecision(ctx, "D-1")
	root, err := os.OpenRoot(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, _, err := exportWrite(root, nil, d, export.Note(d)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EditDecision(ctx, "D-1", model.DecisionEdit{Supersede: true}, c.t); err != nil {
		t.Fatal(err)
	}
	if err := exportMark(ctx, st, d, c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !exportIsDue(t, db, "D-1") {
		t.Fatal("a superseded decision was marked with the older note")
	}
	if out := exportObsidianOK(t); out != "exported 1 note to "+vault {
		t.Errorf("next sweep: %q", out)
	}
	if !strings.Contains(string(exportRead(t, filepath.Join(vault, export.NoteName(exportGet(t, db, "D-1"))))), "status: superseded") {
		t.Error("the note was not rewritten as superseded")
	}
}

func rawExportDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestExportSkipsForeignAndMovesOn(t *testing.T) {
	vault := t.TempDir()
	db, c := exportHome(t, vault,
		exportSpec{question: "Which index?", decided: true},
		exportSpec{question: "Which queue?", decided: true})
	d1 := exportGet(t, db, "D-1")
	foreign := filepath.Join(vault, export.NoteName(d1))
	if err := os.WriteFile(foreign, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := exportObsidianOK(t)
	if !strings.Contains(out, "skipped D-1: a note of yours is at "+foreign) || strings.Contains(out, "D-2:") {
		t.Errorf("first run: %q", out)
	}
	// The next sweep passes it by, silently; --all too.
	for _, args := range [][]string{nil, {"--all"}} {
		if out := exportObsidianOK(t, args...); strings.Contains(out, "D-1") {
			t.Errorf("sweep %v mentions the skipped decision: %q", args, out)
		}
	}
	// --retry-skipped tries it again (and it is still foreign).
	if out := exportObsidianOK(t, "--retry-skipped"); !strings.Contains(out, "skipped D-1:") {
		t.Errorf("--retry-skipped: %q", out)
	}
	// The user moves the file away; a retry writes the note.
	if err := os.Rename(foreign, filepath.Join(vault, "moved by me.md")); err != nil {
		t.Fatal(err)
	}
	if out := exportObsidianOK(t); strings.Contains(out, "exported 1") {
		t.Errorf("a skipped decision was retried without --retry-skipped: %q", out)
	}
	if out := exportObsidianOK(t, "--retry-skipped"); out != "exported 1 note to "+vault {
		t.Errorf("retry after moving the file away: %q", out)
	}
	if got := exportRead(t, foreign); !strings.Contains(string(got), "curia_id: D-1") {
		t.Errorf("note not written: %q", got)
	}
	_ = c
}

func TestExportSkipLapsesOnChange(t *testing.T) {
	vault := t.TempDir()
	db, c := exportHome(t, vault, exportSpec{question: "Which index?", decided: true})
	foreign := filepath.Join(vault, export.NoteName(exportGet(t, db, "D-1")))
	if err := os.WriteFile(foreign, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exportObsidianOK(t)
	if exportIsDue(t, db, "D-1") {
		t.Fatal("not skipped")
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.EditDecision(context.Background(), "D-1", model.DecisionEdit{Scope: model.ScopeGlobal}, c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !exportIsDue(t, db, "D-1") {
		t.Error("a rescope did not make the skipped decision eligible again")
	}
	if out := exportObsidianOK(t); !strings.Contains(out, "skipped D-1:") {
		t.Errorf("sweep after the change did not look at it again: %q", out)
	}
	if got := exportRead(t, foreign); string(got) != "mine\n" {
		t.Errorf("foreign note changed: %q", got)
	}
}

// 500 decisions with foreign notes no longer block the ones after them.
func TestExportForeignNotesDoNotStarveLaterDecisions(t *testing.T) {
	vault := t.TempDir()
	specs := make([]exportSpec, exportMaxNotes+2)
	for i := range specs {
		specs[i] = exportSpec{question: fmt.Sprintf("Question %d?", i+1), decided: true}
	}
	db, _ := exportHome(t, vault, specs...)
	for i := 1; i <= exportMaxNotes; i++ {
		d := exportGet(t, db, fmt.Sprintf("D-%d", i))
		if err := os.WriteFile(filepath.Join(vault, export.NoteName(d)), []byte("mine\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exportObsidianOK(t) // skips the first 500
	for _, args := range [][]string{nil, {"--all"}} {
		out := exportObsidianOK(t, args...)
		if !strings.HasPrefix(out, "exported 2 notes to "+vault) && args == nil {
			t.Errorf("sweep %v: %.80q", args, out)
		}
		if strings.Contains(out, "skipped") {
			t.Errorf("sweep %v re-reports skipped decisions", args)
		}
	}
	for _, id := range []string{fmt.Sprintf("D-%d", exportMaxNotes+1), fmt.Sprintf("D-%d", exportMaxNotes+2)} {
		if exportIsDue(t, db, id) {
			t.Errorf("%s still due behind the foreign notes", id)
		}
	}
}

func TestExportCommonsPrintsEntry(t *testing.T) {
	vault := t.TempDir()
	db, _ := exportHome(t, vault, exportSpec{question: "Postgres or Redis for events? <b>&</b>", scope: model.ScopeGlobal, decided: true})
	accepted := exportListen(t)
	homeBefore := exportSnapshot(t, os.Getenv("HOME"))

	out, errOut, err := exportRun(t, "export", "commons", "D-1")
	if err != nil {
		t.Fatalf("export commons: %v\n%s", err, errOut)
	}
	e, err := export.CommonsEntry(exportGet(t, db, "D-1"))
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	enc := json.NewEncoder(&want)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(e); err != nil {
		t.Fatal(err)
	}
	if out != want.String() {
		t.Errorf("stdout:\n%s\nwant:\n%s", out, want.String())
	}
	if !strings.Contains(out, "<b>&</b>") {
		t.Errorf("HTML escaping is on: %s", out)
	}
	if errOut != "this is the agent's and your own text: read it before you publish it\n" {
		t.Errorf("stderr %q", errOut)
	}
	if got := exportNames(t, vault); len(got) != 0 {
		t.Errorf("commons wrote %q into the vault", got)
	}
	for n, v := range exportSnapshot(t, os.Getenv("HOME")) {
		if b, ok := homeBefore[n]; (!ok || b != v) && !strings.HasPrefix(n, ".local") && !strings.HasPrefix(n, "curia.db") {
			t.Errorf("commons changed %q", n)
		}
	}
	select {
	case <-accepted:
		t.Error("commons opened a connection")
	case <-time.After(50 * time.Millisecond):
	}
}

// exportListen starts a loopback listener the test owns; the channel
// receives once if anything connects to it.
func exportListen(t *testing.T) <-chan struct{} {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		if conn, aerr := ln.Accept(); aerr == nil {
			_ = conn.Close()
			accepted <- struct{}{}
		}
	}()
	return accepted
}

func TestExportCommonsRefusals(t *testing.T) {
	exportHome(t, t.TempDir(),
		exportSpec{question: "Postgres or Redis for events?", decided: true}, // repo scope
		exportSpec{question: "Which index?", scope: model.ScopeGlobal},       // open
		exportSpec{question: "Which queue?", scope: model.ScopeGlobal, decided: true})
	if out, err := runRoot(t, "decisions", "edit", "D-3", "--supersede"); err != nil {
		t.Fatalf("supersede: %v\n%s", err, out)
	}
	for _, cs := range []struct{ id, want string }{
		{"D-1", "D-1 is not a decided global decision"},
		{"D-2", "D-2 is not a decided global decision"},
		{"D-3", "D-3 is not a decided global decision"},
		{"D-99", "no decision D-99"},
		{"bogus", `invalid decision id "bogus"`},
	} {
		out, _, err := exportRun(t, "export", "commons", cs.id)
		if err == nil || !strings.HasPrefix(err.Error(), cs.want) {
			t.Errorf("commons %s = %v, want %q", cs.id, err, cs.want)
		}
		if out != "" {
			t.Errorf("commons %s printed %q", cs.id, out)
		}
	}
}

func TestExportSummary(t *testing.T) {
	for _, c := range []struct {
		res  exportResult
		want string
	}{
		{exportResult{}, "nothing to export"},
		{exportResult{Written: 1}, "exported 1 note to /v"},
		{exportResult{Written: 3, Foreign: []string{"D-9"}}, "exported 3 notes to /v (1 foreign note left alone: D-9)"},
		{exportResult{Foreign: []string{"D-2", "D-9"}}, "exported 0 notes to /v (2 foreign notes left alone: D-2, D-9)"},
		{exportResult{Failed: []exportFailure{{ID: "D-1", Err: errors.New("x")}}}, "exported 0 notes to /v"},
	} {
		if got := exportSummary("/v", c.res); got != c.want {
			t.Errorf("exportSummary(%+v) = %q, want %q", c.res, got, c.want)
		}
	}
	if got := exportSummary("/v\x1b[31m\n", exportResult{Written: 1}); strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("unclean path printed: %q", got)
	}
}

// A note created while a file of its name appeared (another sweep, or by
// hand, between the look and the write) never replaces that file.
func TestExportCreateNeverReplaces(t *testing.T) {
	vault := t.TempDir()
	mine := filepath.Join(vault, "D-1 x.md")
	if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(vault)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := exportCreate(root, "D-1 x.md", []byte("curia's\n")); !errors.Is(err, fs.ErrExist) {
		t.Errorf("exportCreate over an existing file = %v, want fs.ErrExist", err)
	}
	if got := exportRead(t, mine); string(got) != "mine\n" {
		t.Errorf("existing file replaced: %q", got)
	}
	if got := exportNames(t, vault); !slices.Equal(got, []string{"D-1 x.md"}) {
		t.Errorf("vault holds %q: the temporary file was left behind", got)
	}
}

// exportFakeDecide is a decide source whose Answer returns err (or succeeds).
type exportFakeDecide struct {
	ui.DecideSource
	err error
}

func (f exportFakeDecide) Answer(context.Context, string, model.Answer) (model.Outcome, error) {
	return model.Outcome{}, f.err
}

// exportBackgroundFor opens the store and config of exportHome's setup.
func exportBackgroundFor(t *testing.T) *exportBackground {
	t.Helper()
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &exportBackground{st: st, cfg: cfg}
}

// An answer in the dashboard's decide view starts the sweep in the
// background; the answer's own result is untouched, and the source returns
// before the sweep has necessarily run.
func TestExportDashboardAnswerStartsSweep(t *testing.T) {
	vault := t.TempDir()
	exportHome(t, vault, exportSpec{question: "Which index?", decided: true})
	bg := exportBackgroundFor(t)
	src := exportKickSource{DecideSource: exportFakeDecide{}, bg: bg}
	if _, err := src.Answer(context.Background(), "D-1", model.Answer{}); err != nil {
		t.Fatalf("answer: %v", err)
	}
	bg.Wait()
	if n := len(exportNames(t, vault)); n != 1 {
		t.Errorf("vault holds %d notes after the dashboard answer, want 1", n)
	}
}

func TestExportDashboardFailedAnswerOrNoVaultStartsNothing(t *testing.T) {
	vault := t.TempDir()
	exportHome(t, vault, exportSpec{question: "Which index?", decided: true})
	bg := exportBackgroundFor(t)
	boom := errors.New("no such ticket")
	src := exportKickSource{DecideSource: exportFakeDecide{err: boom}, bg: bg}
	if _, err := src.Answer(context.Background(), "D-1", model.Answer{}); !errors.Is(err, boom) {
		t.Errorf("the answer's error was changed: %v", err)
	}
	bg.Wait()
	if n := len(exportNames(t, vault)); n != 0 {
		t.Errorf("a failed answer started a sweep: %d notes", n)
	}
	bg.cfg.Obsidian.Dir = ""
	bg.Kick()
	bg.Wait()
	if n := len(exportNames(t, vault)); n != 0 {
		t.Errorf("a sweep ran with export unconfigured: %d notes", n)
	}
}

// A broken vault is logged and never fails the answer, and a burst of
// answers does not pile up sweeps.
func TestExportDashboardSweepErrorIsOnlyLogged(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "missing")
	exportHome(t, vault, exportSpec{question: "Which index?", decided: true})
	bg := exportBackgroundFor(t)
	src := exportKickSource{DecideSource: exportFakeDecide{}, bg: bg}
	for range 20 {
		if _, err := src.Answer(context.Background(), "D-1", model.Answer{}); err != nil {
			t.Fatalf("answer failed because of the vault: %v", err)
		}
	}
	bg.Wait()
	log, _ := os.ReadFile(bg.cfg.LogPath)
	if n := strings.Count(string(log), "obsidian export failed"); n < 1 || n > 20 {
		t.Errorf("log holds %d failure lines, want at least one and a bounded number:\n%s", n, log)
	}
	if bg.running.Load() {
		t.Error("a sweep is still marked running after Wait")
	}
}

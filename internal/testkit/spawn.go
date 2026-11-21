package testkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/tmux"
)

// HookHome points HOME (and so the default log and config paths) and
// CURIA_DB at a temp dir, outside tmux, and returns the database path.
func HookHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMUX_PANE", "")
	db := filepath.Join(home, "curia.db")
	t.Setenv("CURIA_DB", db)
	return db
}

// SpawnTestBin is the curia binary path spawn tests give the window.
const SpawnTestBin = "/opt/curia/bin/curia"

// SpawnFixture is one spawn test's world: a git repository named
// api-server, a store, a fake tmux, a fake clock that lifecycle.SpawnSleep advances,
// and lifecycle.SpawnBin fixed at SpawnTestBin.
type SpawnFixture struct {
	T       *testing.T
	DB      string
	St      *store.Store
	Cfg     config.Config
	Fake    *tmux.Fake
	Repo    string
	Start   time.Time
	Now     time.Time
	Sleeps  int
	OnSleep func(n int) // called after the n-th fake sleep
}

// NewSpawnFixture builds the fixture and restores the lifecycle seams it
// replaces when the test ends.
func NewSpawnFixture(t *testing.T) *SpawnFixture {
	t.Helper()
	f := &SpawnFixture{T: t, DB: HookHome(t), Fake: &tmux.Fake{}}
	f.Fake.Socket = "/tmp/tmux-501/first" // the server the task's windows open on
	f.Start = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	f.Now = f.Start
	f.Repo = WorktreeRepo(t, "api-server")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(config.XDGDir("XDG_CONFIG_HOME", home, ".config"), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	f.Cfg = cfg
	st, err := store.Open(f.DB)
	if err != nil {
		t.Fatal(err)
	}
	f.St = st
	prevClock, prevSleep, prevBin, prevLimit, prevLook := lifecycle.Clock, lifecycle.SpawnSleep, lifecycle.SpawnBin, lifecycle.SpawnSetupLimit, lifecycle.LaunchLookPath
	lifecycle.LaunchLookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil } // every agent is installed
	lifecycle.Clock = func() time.Time { return f.Now }
	lifecycle.SpawnSleep = func(d time.Duration) {
		f.Now = f.Now.Add(d)
		f.Sleeps++
		if f.OnSleep != nil {
			f.OnSleep(f.Sleeps)
		}
	}
	lifecycle.SpawnBin = func() (string, error) { return SpawnTestBin, nil }
	seamsSet(t)
	t.Cleanup(func() {
		lifecycle.Clock, lifecycle.SpawnSleep, lifecycle.SpawnBin, lifecycle.SpawnSetupLimit, lifecycle.LaunchLookPath = prevClock, prevSleep, prevBin, prevLimit, prevLook
		_ = st.Close()
	})
	return f
}

// Task creates a backlog task on the fixture's repository, based on main.
func (f *SpawnFixture) Task(title, body string) model.Task {
	f.T.Helper()
	tk, err := f.St.CreateTask(context.Background(), task.Normalize(model.Task{
		Title: title, Body: body, Repo: f.Repo, BaseBranch: "main", Status: model.TaskBacklog, CreatedAt: f.Now,
	}))
	if err != nil {
		f.T.Fatal(err)
	}
	return tk
}

// Update changes task id in the store.
func (f *SpawnFixture) Update(id string, fn func(*model.Task)) {
	f.T.Helper()
	if _, err := f.St.UpdateTask(context.Background(), id, func(tk model.Task) (model.Task, error) {
		fn(&tk)
		return tk, nil
	}); err != nil {
		f.T.Fatal(err)
	}
}

// Get returns task id from the store.
func (f *SpawnFixture) Get(id string) model.Task {
	f.T.Helper()
	tk, found, err := f.St.GetTask(context.Background(), id)
	if err != nil || !found {
		f.T.Fatalf("GetTask(%s): found %v, err %v", id, found, err)
	}
	return tk
}

// Spawn runs lifecycle.SpawnTask and returns its result, what it printed and its error.
func (f *SpawnFixture) Spawn(id string, o lifecycle.SpawnOptions) (lifecycle.SpawnResult, string, error) {
	f.T.Helper()
	var out bytes.Buffer
	o.Out = &out
	r, err := lifecycle.SpawnTask(context.Background(), f.St, f.Fake, f.Cfg, id, o)
	return r, out.String(), err
}

// Windows returns the fake's NewWindow calls.
func (f *SpawnFixture) Windows() []string {
	return slices.DeleteFunc(slices.Clone(f.Fake.Calls), func(c string) bool { return !strings.HasPrefix(c, "NewWindow") })
}

// Worktree is the path of task id's worktree.
func (f *SpawnFixture) Worktree(id string) string { return task.WorktreePath(f.Repo, id) }

// CommitSetup commits .curia/setup.sh with content to the repository,
// executable when exe is set (through the index, so no test writes a file
// with an execute bit: the worktree checkout does). It is not approved.
func (f *SpawnFixture) CommitSetup(content string, exe bool) {
	f.T.Helper()
	WorktreeWrite(f.T, filepath.Join(f.Repo, ".curia", "setup.sh"), content)
	GitRun(f.T, f.Repo, "add", "-A")
	if exe {
		GitRun(f.T, f.Repo, "update-index", "--chmod=+x", ".curia/setup.sh")
	}
	GitRun(f.T, f.Repo, "commit", "-q", "-m", "setup")
}

// SetupScript commits .curia/setup.sh (see CommitSetup) and approves its
// content for the repository.
func (f *SpawnFixture) SetupScript(content string, exe bool) {
	f.T.Helper()
	f.CommitSetup(content, exe)
	f.Approve(content)
}

// Approve records content as the approved setup script of the repository.
func (f *SpawnFixture) Approve(content string) {
	f.T.Helper()
	sum := sha256.Sum256([]byte(content))
	if err := f.St.ApproveSetup(context.Background(), f.Repo, hex.EncodeToString(sum[:])); err != nil {
		f.T.Fatal(err)
	}
}

// SetupLog is the setup log of task T-1.
func (f *SpawnFixture) SetupLog() string {
	return filepath.Join(filepath.Dir(f.Cfg.LogPath), "setup", "T-1.log")
}

// WantNothingStarted checks that a refused spawn of id opened no window,
// made no worktree and holds no launch claim.
func (f *SpawnFixture) WantNothingStarted(id string) {
	f.T.Helper()
	if w := f.Windows(); len(w) != 0 {
		f.T.Errorf("windows opened: %q", w)
	}
	if WorktreeExists(f.Worktree(id)) {
		f.T.Errorf("worktree %s was created", f.Worktree(id))
	}
	if tk, found, err := f.St.GetTask(context.Background(), id); err == nil && found && !tk.LaunchedAt.IsZero() {
		f.T.Errorf("%s launched_at = %v, want the claim released", id, tk.LaunchedAt)
	}
}

// seamsSet gives a test binary that is not cmd/curia (which sets them from
// cmd/curia/lifecycle.go) the two seams lifecycle needs: the reconcile of
// sessions with tmux's panes (without the commands' window markers and team
// refresh) and a warning log that drops its lines.
func seamsSet(t *testing.T) {
	t.Helper()
	prevReconcile, prevWarn := lifecycle.Reconcile, lifecycle.LogWarn
	if prevReconcile == nil {
		lifecycle.Reconcile = func(ctx context.Context, st *store.Store, client tmux.Client, _ string, now time.Time) error {
			panes, err := client.ListPanes(ctx)
			if err == nil { // tmux unreachable: nothing to reconcile
				_, err = fleet.Reconcile(ctx, st, panes, now)
				return err
			}
			return nil
		}
	}
	if prevWarn == nil {
		lifecycle.LogWarn = func(string, string, ...any) {}
	}
	t.Cleanup(func() { lifecycle.Reconcile, lifecycle.LogWarn = prevReconcile, prevWarn })
}

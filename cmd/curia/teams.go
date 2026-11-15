package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/ZachFara/curia/internal/install"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/team"
)

// teamsMaxDirs is how many team directories a refresh reads: the first by
// name.
const teamsMaxDirs = 64

const (
	// teamsNameMax bounds a team directory's name as a team id (section 4.12).
	teamsNameMax = 64
	// teamsReadBatch is how many directory names are read at a time while
	// keeping the first teamsMaxDirs by name.
	teamsReadBatch = 256
)

// teamsDir is the directory Claude Code keeps its teams in: <Claude directory>/teams.
func teamsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find teams directory: %w", err)
	}
	return filepath.Join(install.ClaudeDir(home, os.Getenv("CLAUDE_CONFIG_DIR")), "teams"), nil
}

// refreshTeams reads every team config, assigns members to the live sessions and stores the result.
// It never returns an error for a missing or unreadable directory or file: those are logged to logPath and skipped.
//
// A missing teams directory means no team: a session still holding a team
// column is cleared. A teams directory that cannot be listed changes
// nothing, and a team whose config cannot be read keeps the members it has.
// The store is written only when something differs, so a refresh that
// changes nothing takes no write lock.
func refreshTeams(ctx context.Context, st *store.Store, logPath string) error {
	teams, failed, ok := teamsRead(logPath)
	if !ok {
		return nil
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return err
	}
	assigned := team.Assign(sessions, teams)
	for _, s := range sessions {
		if _, taken := assigned[s.ID]; !taken && failed[s.TeamID] {
			assigned[s.ID] = team.Membership{TeamID: s.TeamID, Role: s.TeamRole, Member: s.TeamMember}
		}
	}
	if !teamsNeedWrite(sessions, assigned) {
		return nil
	}
	_, err = st.SetTeams(ctx, assigned)
	return err
}

// teamsRead parses every readable team config, returning the teams, the
// ids of the team directories it skipped, and false when the teams
// directory exists but cannot be listed.
func teamsRead(logPath string) ([]team.Team, map[string]bool, bool) {
	dir, err := teamsDir()
	if err != nil {
		teamsWarn(logPath, "teams directory unreadable", "", err)
		return nil, nil, false
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, true
	}
	if err == nil && !fi.IsDir() {
		err = fmt.Errorf("%s is not a directory (%v)", dir, fi.Mode().Type())
	}
	var names []string
	if err == nil {
		names, err = teamsNames(dir)
	}
	if err != nil {
		teamsWarn(logPath, "teams directory unreadable", dir, err)
		return nil, nil, false
	}
	var teams []team.Team
	failed := map[string]bool{}
	for _, name := range names {
		t, err := teamsReadOne(dir, name)
		if err != nil {
			teamsWarn(logPath, "team config skipped", filepath.Join(dir, name), err)
			failed[sanitize.Line(name, teamsNameMax)] = true
			continue
		}
		teams = append(teams, t)
	}
	return teams, failed, true
}

// teamsNames returns the first teamsMaxDirs entry names of dir in name
// order, holding at most teamsMaxDirs+teamsReadBatch names at a time.
func teamsNames(dir string) ([]string, error) {
	f, err := os.OpenFile(filepath.Clean(dir), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var names []string
	for {
		batch, err := f.Readdirnames(teamsReadBatch)
		names = append(names, batch...)
		slices.Sort(names)
		names = names[:min(len(names), teamsMaxDirs)]
		if errors.Is(err, io.EOF) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// teamsReadOne parses dir/name/config.json. The team directory must be a
// directory and not a symlink; the config must be a regular file of at most
// team.MaxConfigBytes, checked before the open and again on the handle, and
// the open neither blocks nor follows a symlink.
func teamsReadOne(dir, name string) (team.Team, error) {
	td := filepath.Join(dir, name)
	fi, err := os.Lstat(td)
	if err != nil {
		return team.Team{}, err
	}
	if !fi.IsDir() {
		return team.Team{}, fmt.Errorf("not a directory (%v)", fi.Mode().Type())
	}
	path := filepath.Clean(filepath.Join(td, "config.json"))
	if err = teamsCheckFile(os.Lstat(path)); err != nil {
		return team.Team{}, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return team.Team{}, err
	}
	defer func() { _ = f.Close() }()
	if err = teamsCheckFile(f.Stat()); err != nil {
		return team.Team{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, team.MaxConfigBytes+1))
	if err != nil {
		return team.Team{}, err
	}
	return team.Parse(name, data)
}

// teamsCheckFile is requireRegular plus the size bound of a team config.
func teamsCheckFile(fi os.FileInfo, statErr error) error {
	if err := requireRegular(fi, statErr); err != nil {
		return err
	}
	if fi.Size() > team.MaxConfigBytes {
		return fmt.Errorf("%s is %d bytes, over %d", fi.Name(), fi.Size(), team.MaxConfigBytes)
	}
	return nil
}

// teamsWarned holds the failures already logged by this process, so the
// dashboard, which refreshes every second, logs each one once.
var teamsWarned sync.Map

// teamsWarn logs a skipped teams directory or config once per process.
func teamsWarn(logPath, msg, path string, err error) {
	if _, seen := teamsWarned.LoadOrStore(msg+"\x00"+path, true); seen {
		return
	}
	logWarn(logPath, msg, "path", path, "err", err.Error())
}

// teamsNeedWrite reports whether SetTeams(assigned) would change a row of
// sessions (every session that has not ended): a team column that differs
// from what assigned gives it (nothing, for a stale session), or a teammate
// with no task whose lead has one.
func teamsNeedWrite(sessions []model.Session, assigned map[string]team.Membership) bool {
	leadTask := map[string]string{}
	for _, s := range sessions {
		var want team.Membership
		if s.State != model.StateStale {
			want = assigned[s.ID]
		}
		if want != (team.Membership{TeamID: s.TeamID, Role: s.TeamRole, Member: s.TeamMember}) {
			return true
		}
		if want.Role == model.RoleLead && s.TaskID != "" && leadTask[want.TeamID] == "" {
			leadTask[want.TeamID] = s.TaskID
		}
	}
	for _, s := range sessions {
		if s.TeamRole == model.RoleTeammate && s.TaskID == "" && leadTask[s.TeamID] != "" {
			return true
		}
	}
	return false
}

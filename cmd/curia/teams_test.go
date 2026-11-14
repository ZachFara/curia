package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// The captured config (probe P5): team directory session-bf9b79ad, lead
// session bf9b79ad-…, teammates alpha in %17 and beta in %18.
const (
	teamsCapturedDir  = "session-bf9b79ad"
	teamsCapturedLead = "bf9b79ad-5a7a-472e-94a5-8c58b9033764"
)

// teamsHome isolates HOME, CURIA_DB and CLAUDE_CONFIG_DIR, seeds the store
// with sessions and returns the open store and the teams directory (not
// created).
func teamsHome(t *testing.T, sessions ...model.Session) (*store.Store, string) {
	t.Helper()
	db := hookHome(t)
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, s := range sessions {
		if err := st.UpsertSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	return st, filepath.Join(claude, "teams")
}

// teamsSession is an idle session in pane, updated and seen at 11:00.
func teamsSession(id, pane string) model.Session {
	return model.Session{ID: id, Agent: "claude", Pane: pane, State: model.StateIdle, UpdatedAt: at(11, 0), SeenAt: at(11, 0)}
}

// teamsCapturedSessions are the lead and both teammates of the captured
// config, and a solo session.
func teamsCapturedSessions() []model.Session {
	lead := teamsSession(teamsCapturedLead, "%16")
	lead.TaskID = "T-5"
	return []model.Session{lead, teamsSession("alpha-s", "%17"), teamsSession("beta-s", "%18"), teamsSession("solo", "%19")}
}

// teamsCaptured is the captured config's content.
func teamsCaptured(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "team", "testdata", "config-captured.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// teamsWriteCaptured writes the captured config in its team directory,
// teams/session-bf9b79ad/config.json.
func teamsWriteCaptured(t *testing.T, teams string) {
	t.Helper()
	teamsWriteConfig(t, teams, teamsCapturedDir, teamsCaptured(t))
}

// teamsWriteConfig writes data as teams/<name>/config.json.
func teamsWriteConfig(t *testing.T, teams, name string, data []byte) {
	t.Helper()
	dir := filepath.Join(teams, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(filepath.Join(dir, "config.json")), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// teamsLeadConfig is a config whose only member is a lead with session id.
func teamsLeadConfig(id string) []byte {
	return fmt.Appendf(nil, `{"leadSessionId":%q,"members":[{"name":"team-lead","agentType":"team-lead","tmuxPaneId":"leader"}]}`, id)
}

func teamsRefresh(t *testing.T, st *store.Store) {
	t.Helper()
	if err := refreshTeams(context.Background(), st, config.Default().LogPath); err != nil {
		t.Fatalf("refreshTeams: %v", err)
	}
}

// teamsAssert fails unless session id has the team columns want (team id,
// role, member; all empty for no team).
func teamsAssert(t *testing.T, st *store.Store, id string, want [3]string) {
	t.Helper()
	s, found, err := st.GetSession(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetSession(%s) = found %v, err %v", id, found, err)
	}
	if got := [3]string{s.TeamID, s.TeamRole, s.TeamMember}; got != want {
		t.Errorf("%s team = %q, want %q", id, got, want)
	}
}

func TestRefreshTeamsAssignsFromCapturedConfig(t *testing.T) {
	st, teams := teamsHome(t, teamsCapturedSessions()...)
	teamsWriteCaptured(t, teams)
	teamsRefresh(t, st)
	teamsAssert(t, st, teamsCapturedLead, [3]string{teamsCapturedDir, model.RoleLead, "team-lead"})
	teamsAssert(t, st, "alpha-s", [3]string{teamsCapturedDir, model.RoleTeammate, "alpha"})
	teamsAssert(t, st, "beta-s", [3]string{teamsCapturedDir, model.RoleTeammate, "beta"})
	teamsAssert(t, st, "solo", [3]string{})
	// The teammates take the lead's task; nothing looks like activity.
	for _, id := range []string{"alpha-s", "beta-s"} {
		s, _, _ := st.GetSession(context.Background(), id)
		if s.TaskID != "T-5" || !s.UpdatedAt.Equal(at(11, 0)) || !s.SeenAt.Equal(at(11, 0)) {
			t.Errorf("%s task %q updated %v seen %v, want T-5 and both unchanged", id, s.TaskID, s.UpdatedAt, s.SeenAt)
		}
	}
	if log := readLog(t); len(log) != 0 {
		t.Errorf("log = %q, want nothing", log)
	}
}

func TestRefreshTeamsClearsFinishedTeam(t *testing.T) {
	st, teams := teamsHome(t, teamsCapturedSessions()...)
	lead := [3]string{teamsCapturedDir, model.RoleLead, "team-lead"}
	// The team directory removed, the teams directory kept.
	teamsWriteCaptured(t, teams)
	teamsRefresh(t, st)
	teamsAssert(t, st, teamsCapturedLead, lead)
	if err := os.RemoveAll(filepath.Join(teams, teamsCapturedDir)); err != nil {
		t.Fatal(err)
	}
	teamsRefresh(t, st)
	for _, id := range []string{teamsCapturedLead, "alpha-s", "beta-s"} {
		teamsAssert(t, st, id, [3]string{})
	}
	// The teams directory removed too.
	teamsWriteCaptured(t, teams)
	teamsRefresh(t, st)
	teamsAssert(t, st, teamsCapturedLead, lead)
	if err := os.RemoveAll(teams); err != nil {
		t.Fatal(err)
	}
	teamsRefresh(t, st)
	for _, id := range []string{teamsCapturedLead, "alpha-s", "beta-s"} {
		teamsAssert(t, st, id, [3]string{})
	}
}

func TestRefreshTeamsFailsSoft(t *testing.T) {
	cases := []struct {
		name string
		// setup builds what is at the path of the teams directory.
		setup func(t *testing.T, teams string)
		warns int
	}{
		{"no teams dir", func(*testing.T, string) {}, 0},
		{"a file named teams", func(t *testing.T, teams string) {
			teamsMust(t, os.WriteFile(teams, teamsLeadConfig(teamsCapturedLead), 0o600))
		}, 1},
		{"teams is a symlink", func(t *testing.T, teams string) {
			target := filepath.Join(t.TempDir(), "teams")
			teamsWriteCaptured(t, target)
			teamsMust(t, os.Symlink(target, teams))
		}, 1},
		{"a team dir that is a symlink", func(t *testing.T, teams string) {
			target := t.TempDir()
			teamsWriteCaptured(t, target)
			teamsMust(t, os.MkdirAll(teams, 0o700))
			teamsMust(t, os.Symlink(filepath.Join(target, teamsCapturedDir), filepath.Join(teams, teamsCapturedDir)))
		}, 1},
		{"config.json a FIFO", func(t *testing.T, teams string) {
			teamsMust(t, os.MkdirAll(filepath.Join(teams, "x"), 0o700))
			teamsMust(t, syscall.Mkfifo(filepath.Join(teams, "x", "config.json"), 0o600))
		}, 1},
		{"config.json a directory", func(t *testing.T, teams string) {
			teamsMust(t, os.MkdirAll(filepath.Join(teams, "x", "config.json"), 0o700))
		}, 1},
		{"config.json a symlink", func(t *testing.T, teams string) {
			src := filepath.Join(t.TempDir(), "config.json")
			teamsMust(t, os.WriteFile(filepath.Clean(src), teamsCaptured(t), 0o600))
			teamsMust(t, os.MkdirAll(filepath.Join(teams, teamsCapturedDir), 0o700))
			teamsMust(t, os.Symlink(src, filepath.Join(teams, teamsCapturedDir, "config.json")))
		}, 1},
		{"config.json missing", func(t *testing.T, teams string) {
			teamsMust(t, os.MkdirAll(filepath.Join(teams, "x"), 0o700))
		}, 1},
		{"config.json 2 MiB", func(t *testing.T, teams string) {
			big := append(teamsLeadConfig(teamsCapturedLead), make([]byte, 2<<20)...)
			teamsWriteConfig(t, teams, teamsCapturedDir, big)
		}, 1},
		{"config.json malformed", func(t *testing.T, teams string) {
			teamsWriteConfig(t, teams, teamsCapturedDir, []byte(`{"leadSessionId":`))
		}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, teams := teamsHome(t, teamsCapturedSessions()...)
			c.setup(t, teams)
			teamsAssertFailsSoft(t, st, c.warns)
		})
	}
}

// teamsAssertFailsSoft refreshes twice and checks the refresh did not block
// or fail, changed no session and logged warns WARN lines in all.
func teamsAssertFailsSoft(t *testing.T, st *store.Store, warns int) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- refreshTeams(context.Background(), st, config.Default().LogPath) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("refreshTeams = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refreshTeams blocked")
	}
	for _, s := range teamsCapturedSessions() {
		teamsAssert(t, st, s.ID, [3]string{})
	}
	// A second refresh logs the same failure no more.
	teamsRefresh(t, st)
	log := readLog(t)
	if len(log) != warns {
		t.Fatalf("log = %q, want %d line(s)", log, warns)
	}
	for _, l := range log {
		if !strings.Contains(l, "level=WARN") {
			t.Errorf("log line %q is not a WARN", l)
		}
	}
}

// A team whose config cannot be read keeps the members it had, so a
// half-written file does not flap a team apart.
func TestRefreshTeamsKeepsTeamWhoseConfigFails(t *testing.T) {
	st, teams := teamsHome(t, teamsCapturedSessions()...)
	teamsWriteCaptured(t, teams)
	teamsRefresh(t, st)
	teamsWriteConfig(t, teams, teamsCapturedDir, []byte(`{"members":`))
	teamsRefresh(t, st)
	teamsAssert(t, st, teamsCapturedLead, [3]string{teamsCapturedDir, model.RoleLead, "team-lead"})
	teamsAssert(t, st, "alpha-s", [3]string{teamsCapturedDir, model.RoleTeammate, "alpha"})
	if log := readLog(t); len(log) != 1 {
		t.Errorf("log = %q, want one WARN", log)
	}
}

func TestRefreshTeamsCapsDirectories(t *testing.T) {
	var sessions []model.Session
	for i := range 70 {
		sessions = append(sessions, teamsSession(fmt.Sprintf("s%02d", i), fmt.Sprintf("%%%d", i)))
	}
	st, teams := teamsHome(t, sessions...)
	// Written in reverse, so directory order is not name order.
	for i := 69; i >= 0; i-- {
		teamsWriteConfig(t, teams, fmt.Sprintf("team-%02d", i), teamsLeadConfig(fmt.Sprintf("s%02d", i)))
	}
	teamsRefresh(t, st)
	for i := range 70 {
		want := [3]string{}
		if i < teamsMaxDirs {
			want = [3]string{fmt.Sprintf("team-%02d", i), model.RoleLead, "team-lead"}
		}
		teamsAssert(t, st, fmt.Sprintf("s%02d", i), want)
	}
}

// With nothing to change, a refresh takes no write lock: it returns at once
// while another connection holds it.
func TestRefreshTeamsWritesNothingWhenUnchanged(t *testing.T) {
	st, teams := teamsHome(t, teamsCapturedSessions()...)
	teamsWriteCaptured(t, teams)
	teamsRefresh(t, st)
	before, err := st.ListSessions(context.Background(), store.ListFilter{})
	teamsMust(t, err)

	raw, err := sql.Open("sqlite", os.Getenv("CURIA_DB"))
	teamsMust(t, err)
	defer func() { _ = raw.Close() }()
	conn, err := raw.Conn(context.Background())
	teamsMust(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE")
	teamsMust(t, err)
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()

	start := time.Now()
	teamsRefresh(t, st)
	if d := time.Since(start); d > time.Second {
		t.Errorf("unchanged refresh took %v behind a held write lock, want no write", d)
	}
	after, err := st.ListSessions(context.Background(), store.ListFilter{})
	teamsMust(t, err)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("sessions changed:\n%v\n%v", before, after)
	}
	if log := readLog(t); len(log) != 0 {
		t.Errorf("log = %q, want nothing", log)
	}
}

// A store error is returned.
func TestRefreshTeamsReturnsStoreError(t *testing.T) {
	st, teams := teamsHome(t, teamsCapturedSessions()...)
	teamsWriteCaptured(t, teams)
	teamsMust(t, st.Close())
	if err := refreshTeams(context.Background(), st, config.Default().LogPath); err == nil {
		t.Error("refreshTeams on a closed store returned nil")
	}
}

func TestTeamsDirFollowsClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if d, err := teamsDir(); err != nil || d != filepath.Join(home, ".claude", "teams") {
		t.Errorf("teamsDir() = %q, %v", d, err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/c")
	if d, err := teamsDir(); err != nil || d != "/c/teams" {
		t.Errorf("with CLAUDE_CONFIG_DIR: teamsDir() = %q, %v", d, err)
	}
}

// status never refreshes teams: a FIFO config would be logged, and a valid
// config would assign its members, if it did.
func TestStatusNeverReadsTeams(t *testing.T) {
	db, _ := fleetHome(t, nil, teamsCapturedSessions()...)
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	teams := filepath.Join(claude, "teams")
	teamsWriteCaptured(t, teams)
	teamsMust(t, os.MkdirAll(filepath.Join(teams, "fifo"), 0o700))
	teamsMust(t, syscall.Mkfifo(filepath.Join(teams, "fifo", "config.json"), 0o600))
	done := make(chan error, 1)
	go func() {
		_, err := runRoot(t, "status")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status blocked")
	}
	if s := getSession(t, db, teamsCapturedLead); s.TeamID != "" {
		t.Errorf("status assigned team %q", s.TeamID)
	}
	if log := readLog(t); len(log) != 0 {
		t.Errorf("log = %q, want nothing from status", log)
	}
}

func teamsMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

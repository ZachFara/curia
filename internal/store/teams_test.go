package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/team"
)

// teamTeam is the team id the tests use: Claude Code names a team's
// directory after its lead's session id.
const teamTeam = "session-0a1b2c3d"

// teamSession stores a session in pane with state and task, last updated
// and seen at t0.
func teamSession(t *testing.T, s *store.Store, id, pane string, state model.State, task string) {
	t.Helper()
	upsert(t, s, model.Session{
		ID: id, Agent: "claude", Pane: pane, State: state, TaskID: task, UpdatedAt: t0, SeenAt: t0,
	})
}

// teamLead is the membership of the test team's lead.
func teamLead() team.Membership {
	return team.Membership{TeamID: teamTeam, Role: model.RoleLead, Member: "team-lead"}
}

func teamMate(name string) team.Membership {
	return team.Membership{TeamID: teamTeam, Role: model.RoleTeammate, Member: name}
}

// setTeams runs SetTeams and returns how many rows it changed.
func setTeams(t *testing.T, s *store.Store, assigned map[string]team.Membership) int {
	t.Helper()
	n, err := s.SetTeams(context.Background(), assigned)
	if err != nil {
		t.Fatalf("SetTeams: %v", err)
	}
	return n
}

// assertMembership fails unless session id is stored with membership m (the
// zero value: no team).
func assertMembership(t *testing.T, s *store.Store, id string, m team.Membership) {
	t.Helper()
	got := get(t, s, id)
	if got.TeamID != m.TeamID || got.TeamRole != m.Role || got.TeamMember != m.Member {
		t.Errorf("%s team = %q %q %q, want %q %q %q", id, got.TeamID, got.TeamRole, got.TeamMember, m.TeamID, m.Role, m.Member)
	}
}

func TestSetTeamsSetsAndClears(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		teamSession(t, s, id, fmt.Sprintf("%%%d", i+1), model.StateIdle, "")
	}
	teamSession(t, s, "gone", "%9", model.StateEnded, "")
	setTeam(t, path, "gone") // an ended session keeps what it had
	assigned := map[string]team.Membership{"a": teamLead(), "b": teamMate("tester"), "c": teamMate("writer")}
	// An ended session is never written, even when the map names it.
	withGone := map[string]team.Membership{"gone": teamMate("ghost")}
	for id, m := range assigned {
		withGone[id] = m
	}
	if n := setTeams(t, s, withGone); n != 3 {
		t.Errorf("first SetTeams changed %d rows, want 3", n)
	}
	for id, m := range assigned {
		assertMembership(t, s, id, m)
	}
	assertMembership(t, s, "d", team.Membership{})
	assertMembership(t, s, "e", team.Membership{})
	assertTeam(t, get(t, s, "gone"))

	delete(assigned, "c")
	if n := setTeams(t, s, assigned); n != 1 {
		t.Errorf("SetTeams without c changed %d rows, want 1", n)
	}
	assertMembership(t, s, "c", team.Membership{})
	var id, role, member sql.NullString
	err := rawDB(t, path).QueryRow(`SELECT team_id, team_role, team_member FROM sessions WHERE id = 'c'`).Scan(&id, &role, &member)
	if err != nil {
		t.Fatal(err)
	}
	if id.Valid || role.Valid || member.Valid {
		t.Errorf("cleared team columns = %v %v %v, want NULL", id, role, member)
	}
	assertMembership(t, s, "a", teamLead())
	assertTeam(t, get(t, s, "gone"))
}

func TestTeamSetTeamsNeverAssignsStale(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	teamSession(t, s, "lead", "%1", model.StateWorking, "")
	teamSession(t, s, "old", "%2", model.StateStale, "")
	setTeam(t, path, "old")
	// A stale session is in no team: its columns are cleared, and a map
	// that names it does not assign it.
	if n := setTeams(t, s, map[string]team.Membership{"lead": teamLead(), "old": teamMate("tester")}); n != 2 {
		t.Errorf("SetTeams changed %d rows, want 2 (lead set, stale cleared)", n)
	}
	assertMembership(t, s, "old", team.Membership{})
	assertMembership(t, s, "lead", teamLead())
}

func TestSetTeamsWritesOnlyChanges(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	teamSession(t, s, "lead", "%1", model.StateWorking, "")
	teamSession(t, s, "mate", "%2", model.StateIdle, "")
	teamSession(t, s, "solo", "%3", model.StateIdle, "")
	before := map[string]model.Session{}
	for _, id := range []string{"lead", "mate", "solo"} {
		before[id] = get(t, s, id)
	}
	assigned := map[string]team.Membership{"lead": teamLead(), "mate": teamMate("tester")}
	if n := setTeams(t, s, assigned); n != 2 {
		t.Errorf("first SetTeams changed %d rows, want 2", n)
	}
	if n := setTeams(t, s, assigned); n != 0 {
		t.Errorf("same map again changed %d rows, want 0", n)
	}
	if n := setTeams(t, s, nil); n != 2 {
		t.Errorf("empty map changed %d rows, want 2", n)
	}
	if n := setTeams(t, s, nil); n != 0 {
		t.Errorf("empty map again changed %d rows, want 0", n)
	}
	for id, b := range before {
		got := get(t, s, id)
		if !got.UpdatedAt.Equal(b.UpdatedAt) || !got.SeenAt.Equal(b.SeenAt) {
			t.Errorf("%s updated_at %v seen_at %v, want %v %v", id, got.UpdatedAt, got.SeenAt, b.UpdatedAt, b.SeenAt)
		}
		if got.State != b.State || got.Pane != b.Pane || got.TaskID != b.TaskID {
			t.Errorf("%s = %+v, want only team columns changed from %+v", id, got, b)
		}
	}
}

func TestTeammateInheritsLeadTask(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	teamSession(t, s, "lead", "%1", model.StateWorking, "T-3")
	teamSession(t, s, "mate", "%2", model.StateIdle, "")
	teamSession(t, s, "own", "%3", model.StateIdle, "T-7")
	teamSession(t, s, "done", "%4", model.StateEnded, "")
	rawExec(t, path, `UPDATE sessions SET team_id = ?, team_role = 'teammate', team_member = 'old' WHERE id = 'done'`, teamTeam)
	assigned := map[string]team.Membership{"lead": teamLead(), "mate": teamMate("tester"), "own": teamMate("writer")}
	if n := setTeams(t, s, assigned); n != 3 {
		t.Errorf("SetTeams changed %d rows, want 3 (each row once)", n)
	}
	if got := get(t, s, "mate").TaskID; got != "T-3" {
		t.Errorf("teammate task = %q, want the lead's T-3", got)
	}
	if got := get(t, s, "own").TaskID; got != "T-7" {
		t.Errorf("teammate with its own task = %q, want T-7", got)
	}
	if got := get(t, s, "done").TaskID; got != "" {
		t.Errorf("ended teammate task = %q, want none", got)
	}
	if n := setTeams(t, s, assigned); n != 0 {
		t.Errorf("SetTeams again changed %d rows, want 0", n)
	}
}

func TestTeamTeammateIgnoresEndedLead(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	teamSession(t, s, "oldlead", "%1", model.StateEnded, "T-9")
	rawExec(t, path, `UPDATE sessions SET team_id = ?, team_role = 'lead', team_member = 'team-lead' WHERE id = 'oldlead'`, teamTeam)
	teamSession(t, s, "lead", "%2", model.StateWorking, "")
	teamSession(t, s, "mate", "%3", model.StateIdle, "")
	setTeams(t, s, map[string]team.Membership{"lead": teamLead(), "mate": teamMate("tester")})
	if got := get(t, s, "mate").TaskID; got != "" {
		t.Errorf("teammate task = %q, want none (only a live lead's task is inherited)", got)
	}
	// A teammate whose lead has no task, in a team of a different name, is
	// not given another team's lead's task.
	teamSession(t, s, "lead2", "%4", model.StateWorking, "T-4")
	setTeams(t, s, map[string]team.Membership{
		"lead": teamLead(), "mate": teamMate("tester"),
		"lead2": {TeamID: "session-99999999", Role: model.RoleLead, Member: "team-lead"},
	})
	if got := get(t, s, "mate").TaskID; got != "" {
		t.Errorf("teammate task = %q, want none (the other team's lead does not count)", got)
	}
}

func TestSetTeamsSurvivesConcurrentHook(t *testing.T) {
	const sessions, rounds = 5, 200
	path := dbPath(t)
	hs := handles(t, path, sessions)
	ctx := context.Background()
	ids := make([]string, sessions)
	full := map[string]team.Membership{}
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i)
		teamSession(t, hs[0], ids[i], fmt.Sprintf("%%%d", i+1), model.StateWorking, "")
		full[ids[i]] = teamMate(fmt.Sprintf("member-%d", i))
	}
	full[ids[0]] = teamLead()
	partial := map[string]team.Membership{ids[0]: full[ids[0]], ids[2]: full[ids[2]]}
	maps := []map[string]team.Membership{full, partial}

	var wg sync.WaitGroup
	var hookErr, teamErr error
	wg.Go(func() {
		for i := range rounds {
			hookErr = retryBusy(func() error {
				return hs[i%sessions].UpdateSession(ctx, ids[i%sessions], func(cur model.Session, _ bool) model.Session {
					cur.ContextUsed++
					cur.UpdatedAt = t0.Add(time.Duration(i+1) * time.Second)
					return cur
				})
			}, busyBeforeAnything)
			if hookErr != nil {
				return
			}
		}
	})
	wg.Go(func() {
		for i := range rounds {
			teamErr = retryBusy(func() error {
				_, err := hs[(i+1)%sessions].SetTeams(ctx, maps[i%2])
				return err
			}, busyAtBegin)
			if teamErr != nil {
				return
			}
		}
	})
	wg.Wait()
	if hookErr != nil || teamErr != nil {
		t.Fatalf("hook %v, teams %v", hookErr, teamErr)
	}
	last := maps[(rounds-1)%2]
	for i, id := range ids {
		got := get(t, hs[0], id)
		if got.ContextUsed != rounds/sessions {
			t.Errorf("%s context_used = %d, want %d (a hook update was lost)", id, got.ContextUsed, rounds/sessions)
		}
		if want := t0.Add(time.Duration(rounds-sessions+i+1) * time.Second); !got.UpdatedAt.Equal(want) {
			t.Errorf("%s updated_at = %v, want the last hook's %v", id, got.UpdatedAt, want)
		}
		assertMembership(t, hs[0], id, last[id])
	}
}

package fleet_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
)

func member(id string, state model.State, updated time.Time, team, role string) model.Session {
	s := sess(id, state, "%"+id, updated)
	s.TeamID, s.TeamRole, s.TeamMember = team, role, id
	return s
}

func unitIDs(us []fleet.Unit) [][]string {
	out := make([][]string, len(us))
	for i, u := range us {
		out[i] = append([]string{u.Lead.ID}, ids(u.Teammates)...)
	}
	return out
}

func TestUnitsSoloOnly(t *testing.T) {
	t0 := time.Unix(1000, 0)
	in := []model.Session{
		sess("a", model.StateWorking, "%1", t0),
		sess("b", model.StateNeedsDecision, "%2", t0),
		sess("c", model.StateWaiting, "%3", t0),
	}
	us := fleet.Units(in)
	if got, want := unitIDs(us), [][]string{{"b"}, {"c"}, {"a"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("units = %v, want %v", got, want)
	}
	for _, u := range us {
		if len(u.Teammates) != 0 || u.Worst.ID != u.Lead.ID {
			t.Errorf("solo unit %+v malformed", u)
		}
	}
}

func TestUnitsGroupsTeam(t *testing.T) {
	t0 := time.Unix(1000, 0)
	lead := member("lead", model.StateIdle, t0, "T", model.RoleLead)
	lead.SeenAt = t0.Add(time.Minute)
	a := member("a", model.StateWorking, t0, "T", model.RoleTeammate)
	b := member("b", model.StateWaiting, t0, "T", model.RoleTeammate)
	us := fleet.Units([]model.Session{lead, a, b})
	if len(us) != 1 {
		t.Fatalf("got %d units, want 1", len(us))
	}
	u := us[0]
	if u.Lead.ID != "lead" || u.Worst.ID != "b" {
		t.Errorf("Lead %s Worst %s, want lead and b", u.Lead.ID, u.Worst.ID)
	}
	if got, want := ids(u.Teammates), []string{"b", "a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("teammates = %v, want %v", got, want)
	}
}

func TestUnitsRankByWorst(t *testing.T) {
	t0 := time.Unix(1000, 0)
	lead := member("lead", model.StateWorking, t0, "T", model.RoleLead)
	mate := member("mate", model.StateWaiting, t0, "T", model.RoleTeammate)
	in := []model.Session{
		sess("work", model.StateWorking, "%1", t0),
		lead, mate,
		sess("need", model.StateNeedsDecision, "%2", t0),
	}
	if got, want := unitIDs(fleet.Units(in)), [][]string{{"need"}, {"lead", "mate"}, {"work"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("units = %v, want %v", got, want)
	}
}

func TestUnitsTeamWithoutLeadIsSolo(t *testing.T) {
	t0 := time.Unix(1000, 0)
	in := []model.Session{
		member("a", model.StateWorking, t0, "T", model.RoleTeammate),
		member("b", model.StateWaiting, t0, "T", model.RoleTeammate),
	}
	us := fleet.Units(in)
	if got, want := unitIDs(us), [][]string{{"b"}, {"a"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("units = %v, want %v", got, want)
	}
}

func TestUnitsTwoTeams(t *testing.T) {
	t0 := time.Unix(1000, 0)
	in := []model.Session{
		member("l1", model.StateWorking, t0, "T1", model.RoleLead),
		member("m1", model.StateWorking, t0, "T1", model.RoleTeammate),
		member("l2", model.StateIdle, t0, "T2", model.RoleLead),
		member("m2", model.StateNeedsDecision, t0, "T2", model.RoleTeammate),
	}
	us := fleet.Units(in)
	if got, want := unitIDs(us), [][]string{{"l2", "m2"}, {"l1", "m1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("units = %v, want %v", got, want)
	}
	if us[0].Worst.ID != "m2" {
		t.Errorf("Worst = %s, want m2", us[0].Worst.ID)
	}
}

func TestUnitsWorstCanBeLead(t *testing.T) {
	t0 := time.Unix(1000, 0)
	us := fleet.Units([]model.Session{
		member("l", model.StateNeedsDecision, t0, "T", model.RoleLead),
		member("m", model.StateWorking, t0, "T", model.RoleTeammate),
	})
	if us[0].Worst.ID != "l" || len(us[0].Teammates) != 1 {
		t.Errorf("unit = %+v", us[0])
	}
}

func TestUnitsDoesNotMutateInput(t *testing.T) {
	t0 := time.Unix(1000, 0)
	in := []model.Session{
		member("m", model.StateWaiting, t0, "T", model.RoleTeammate),
		sess("x", model.StateNeedsDecision, "%9", t0),
		member("l", model.StateWorking, t0, "T", model.RoleLead),
	}
	before := append([]model.Session(nil), in...)
	fleet.Units(in)
	if !reflect.DeepEqual(in, before) {
		t.Fatal("Units changed its input")
	}
}

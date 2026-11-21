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

func withCost(s model.Session, usd float64) model.Session {
	s.CostUSD, s.CostKnown = usd, true
	return s
}

func withCache(s model.Session, expires time.Time) model.Session {
	s.CacheExpiresAt = expires
	return s
}

func TestUnitCost(t *testing.T) {
	base := time.Unix(5000, 0)
	lead := withCost(member("l", model.StateIdle, base, "tm", model.RoleLead), 1.50)
	t1 := withCost(member("a", model.StateIdle, base, "tm", "member"), 2.00)
	t2 := member("b", model.StateIdle, base, "tm", "member")
	if usd, known := (fleet.Unit{Lead: lead, Teammates: []model.Session{t1, t2}}).Cost(); !known || usd != 3.50 {
		t.Errorf("team Cost = %v, %v; want 3.5, true", usd, known)
	}
	if usd, known := (fleet.Unit{Lead: member("l", model.StateIdle, base, "tm", model.RoleLead), Teammates: []model.Session{t2}}).Cost(); known || usd != 0 {
		t.Errorf("none known Cost = %v, %v; want 0, false", usd, known)
	}
	solo := withCost(sess("s", model.StateWorking, "%1", base), 0.25)
	if usd, known := (fleet.Unit{Lead: solo}).Cost(); !known || usd != 0.25 {
		t.Errorf("solo Cost = %v, %v; want 0.25, true", usd, known)
	}
	zero := withCost(sess("z", model.StateWorking, "%1", base), 0)
	if usd, known := (fleet.Unit{Lead: zero}).Cost(); !known || usd != 0 {
		t.Errorf("known zero Cost = %v, %v; want 0, true", usd, known)
	}
}

func TestUnitCacheOf(t *testing.T) {
	now := time.Unix(5000, 0)
	at, base := now.Add, now
	lead := withCache(member("l", model.StateIdle, base, "tm", model.RoleLead), at(12*time.Minute))
	waiting := withCache(member("w", model.StateWaiting, base, "tm", "member"), at(4*time.Minute))
	working := withCache(member("k", model.StateWorking, base, "tm", "member"), at(time.Minute))
	got, ok := (fleet.Unit{Lead: lead, Teammates: []model.Session{waiting, working}}).CacheOf(now)
	if !ok || got.ID != "w" {
		t.Errorf("CacheOf = %q, %v; want the 4m teammate w", got.ID, ok)
	}

	decision := withCache(member("d", model.StateNeedsDecision, base, "tm", "member"), at(2*time.Minute))
	got, ok = (fleet.Unit{Lead: lead, Teammates: []model.Session{waiting, decision}}).CacheOf(now)
	if !ok || got.ID != "d" {
		t.Errorf("needs_decision member: got %q, %v; want d", got.ID, ok)
	}

	// A warm idle member wins over a cold one.
	cold := withCache(member("c", model.StateIdle, base, "tm", "member"), at(-time.Minute))
	got, ok = (fleet.Unit{Lead: lead, Teammates: []model.Session{cold}}).CacheOf(now)
	if !ok || got.ID != "l" {
		t.Errorf("warm beats cold: got %q, %v; want l", got.ID, ok)
	}

	// Only cold members: a cold one. Expiring exactly now is cold.
	edge := withCache(member("e", model.StateIdle, base, "tm", "member"), now)
	got, ok = (fleet.Unit{
		Lead:      withCache(member("l", model.StateIdle, base, "tm", model.RoleLead), at(-time.Hour)),
		Teammates: []model.Session{edge},
	}).CacheOf(now)
	if !ok || !got.CacheExpiresAt.Before(now.Add(time.Nanosecond)) {
		t.Errorf("only cold: got %q, %v; want a cold member", got.ID, ok)
	}

	// Idle members all unknown: false.
	unk := member("u", model.StateIdle, base, "tm", "member")
	if _, ok := (fleet.Unit{Lead: member("l", model.StateIdle, base, "tm", model.RoleLead), Teammates: []model.Session{unk}}).CacheOf(now); ok {
		t.Error("all unknown: want false")
	}

	// A solo working session with a warm cache: false.
	solo := withCache(sess("s", model.StateWorking, "%1", base), at(10*time.Minute))
	if _, ok := (fleet.Unit{Lead: solo}).CacheOf(now); ok {
		t.Error("solo working: want false")
	}

	// A solo idle session is its own.
	idle := withCache(sess("i", model.StateIdle, "%1", base), at(10*time.Minute))
	if got, ok := (fleet.Unit{Lead: idle}).CacheOf(now); !ok || got.ID != "i" {
		t.Errorf("solo idle: got %q, %v", got.ID, ok)
	}
}

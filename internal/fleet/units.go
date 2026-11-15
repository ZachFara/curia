package fleet

import "github.com/ZachFara/curia/internal/model"

// Unit is one dashboard group: a solo session, or a team's lead with its teammates.
type Unit struct {
	Lead      model.Session
	Teammates []model.Session // empty for a solo session; ranked
	Worst     model.Session   // the member that ranks first: its state is the unit's
}

// Units groups sessions by team and orders the units by their Worst member,
// as Rank orders sessions. Sessions with the same non-empty TeamID form one
// unit led by the member whose TeamRole is model.RoleLead; a team with no lead
// among the sessions is not a unit, so its members are solo. Units returns new
// slices; sessions is unchanged.
func Units(sessions []model.Session) []Unit {
	leads := map[string]bool{}
	for _, s := range sessions {
		if s.TeamID != "" && s.TeamRole == model.RoleLead {
			leads[s.TeamID] = true
		}
	}
	teams := map[string][]model.Session{}
	for _, s := range sessions {
		if leads[s.TeamID] {
			teams[s.TeamID] = append(teams[s.TeamID], s)
		}
	}
	var units []Unit
	done := map[string]bool{}
	// Walking the ranked sessions makes a team appear at its worst member.
	for _, s := range Rank(sessions) {
		if !leads[s.TeamID] {
			units = append(units, Unit{Lead: s, Worst: s})
			continue
		}
		if done[s.TeamID] {
			continue
		}
		done[s.TeamID] = true
		units = append(units, teamUnit(s, teams[s.TeamID]))
	}
	return units
}

// teamUnit builds the unit of a team whose first-ranked member is worst.
func teamUnit(worst model.Session, members []model.Session) Unit {
	u := Unit{Worst: worst}
	haveLead := false
	for _, m := range Rank(members) {
		if !haveLead && m.TeamRole == model.RoleLead {
			u.Lead, haveLead = m, true
			continue
		}
		u.Teammates = append(u.Teammates, m)
	}
	return u
}

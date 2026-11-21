package fleet

import (
	"time"

	"github.com/ZachFara/curia/internal/model"
)

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

// members lists the lead and then the teammates.
func (u Unit) members() []model.Session {
	return append([]model.Session{u.Lead}, u.Teammates...)
}

// Cost is the sum of the unit's members' known costs; known is false when no
// member has one.
func (u Unit) Cost() (usd float64, known bool) {
	for _, m := range u.members() {
		if m.CostKnown {
			usd += m.CostUSD
			known = true
		}
	}
	return usd, known
}

// CacheOf returns the member whose prompt cache stands for the unit at now:
// among members that are idle, waiting or needs_decision and have a known
// cache state, the one whose warm cache expires soonest, else one whose cache
// is cold (a known expiry at or before now, in any state). An unknown cache
// state is a zero CacheExpiresAt. Ties go to the earlier member, the lead first.
func (u Unit) CacheOf(now time.Time) (model.Session, bool) {
	var warm, cold model.Session
	haveWarm, haveCold := false, false
	for _, m := range u.members() {
		if m.CacheExpiresAt.IsZero() {
			continue
		}
		if !m.CacheExpiresAt.After(now) {
			if !haveCold {
				cold, haveCold = m, true
			}
			continue
		}
		if m.State != model.StateIdle && m.State != model.StateWaiting && m.State != model.StateNeedsDecision {
			continue
		}
		if !haveWarm || m.CacheExpiresAt.Before(warm.CacheExpiresAt) {
			warm, haveWarm = m, true
		}
	}
	if haveWarm {
		return warm, true
	}
	return cold, haveCold
}

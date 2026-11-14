// Package team reads an agent team's config file and decides which live
// sessions belong to which team. It is pure: it opens no file and touches no
// store. The config is untrusted input, so every string is cleaned and every
// size is bounded.
package team

import (
	"encoding/json"
	"errors"
	"regexp"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Member is one entry of a team's config.
type Member struct{ Name, AgentType, SessionID, Pane, Cwd string }

// Team is one parsed team config.
type Team struct {
	ID            string // the team's directory name, cleaned
	LeadSessionID string
	Members       []Member
}

// Membership says which team a session belongs to and in what role.
type Membership struct{ TeamID, Role, Member string }

// Limits and the agent type that marks a team's lead.
const (
	MaxConfigBytes = 1 << 20
	MaxMembers     = 64
	LeadAgentType  = "team-lead"
	// LeaderPane is the literal pane id Claude Code records for the lead.
	LeaderPane = "leader"
	maxString  = 64
)

// ErrBadConfig is returned by Parse for a config that cannot be used.
var ErrBadConfig = errors.New("team: unusable config")

var (
	paneRE    = regexp.MustCompile(`^(%[0-9]+|leader)$`)
	sessionRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// wireConfig and wireMember are the only place that names the config file's
// JSON fields. Unverified until probe P5's capture: change the tags here.
type wireConfig struct {
	LeadSessionID string       `json:"leadSessionId"`
	Members       []wireMember `json:"members"`
}

type wireMember struct {
	Name      string `json:"name"`
	AgentType string `json:"agentType"`
	SessionID string `json:"sessionId"`
	Pane      string `json:"tmuxPaneId"`
	Cwd       string `json:"cwd"`
}

// Parse reads one team config. dirName is the team's directory name.
func Parse(dirName string, data []byte) (Team, error) {
	if len(data) > MaxConfigBytes {
		return Team{}, ErrBadConfig
	}
	var w wireConfig
	if err := json.Unmarshal(data, &w); err != nil {
		return Team{}, ErrBadConfig
	}
	if len(w.Members) == 0 || len(w.Members) > MaxMembers {
		return Team{}, ErrBadConfig
	}
	t := Team{
		ID:            sanitize.Line(dirName, maxString),
		LeadSessionID: keep(sessionRE, w.LeadSessionID),
		Members:       make([]Member, 0, len(w.Members)),
	}
	hasLead := false
	for _, m := range w.Members {
		cm := Member{
			Name:      sanitize.Line(m.Name, maxString),
			AgentType: sanitize.Line(m.AgentType, maxString),
			SessionID: keep(sessionRE, m.SessionID),
			Pane:      keep(paneRE, m.Pane),
			Cwd:       sanitize.Line(m.Cwd, maxString),
		}
		hasLead = hasLead || isLead(cm)
		t.Members = append(t.Members, cm)
	}
	if !hasLead {
		return Team{}, ErrBadConfig
	}
	return t, nil
}

// isLead reports whether a member is the team's lead: Claude Code records the
// lead's pane as the literal "leader".
func isLead(m Member) bool {
	return m.Pane == LeaderPane || m.AgentType == LeadAgentType
}

func keep(re *regexp.Regexp, s string) string {
	if re.MatchString(s) {
		return s
	}
	return ""
}

// Assign maps session ids to their team membership. Only sessions that are
// not ended or stale count. The lead matches by the config's lead session id. A team whose lead matches no live session
// assigns nobody; a session matched by two teams belongs to the first.
func Assign(sessions []model.Session, teams []Team) map[string]Membership {
	out := map[string]Membership{}
	var live []model.Session
	for _, s := range sessions {
		if s.State != model.StateEnded && s.State != model.StateStale {
			live = append(live, s)
		}
	}
	for _, t := range teams {
		assignTeam(out, live, t)
	}
	return out
}

func assignTeam(out map[string]Membership, live []model.Session, t Team) {
	lead := -1
	for i, m := range t.Members {
		if isLead(m) {
			lead = i
			break
		}
	}
	if lead < 0 {
		return
	}
	found := map[string]Membership{}
	taken := func(id string) bool {
		_, a := out[id]
		_, b := found[id]
		return a || b
	}
	// The lead first: its own match, else the config's lead session id.
	leadID := match(live, t.Members[lead], taken)
	if leadID == "" && t.LeadSessionID != "" {
		for _, s := range live {
			if s.ID == t.LeadSessionID && !taken(s.ID) {
				leadID = s.ID
			}
		}
	}
	if leadID == "" {
		return
	}
	found[leadID] = Membership{t.ID, model.RoleLead, t.Members[lead].Name}
	for i, m := range t.Members {
		if i == lead {
			continue
		}
		if id := match(live, m, taken); id != "" {
			found[id] = Membership{t.ID, model.RoleTeammate, m.Name}
		}
	}
	for id, ms := range found {
		out[id] = ms
	}
}

// match finds the live session of a member: by its session id when the
// config gives one, else by its pane.
func match(live []model.Session, m Member, taken func(string) bool) string {
	for _, s := range live {
		if taken(s.ID) {
			continue
		}
		if m.SessionID != "" {
			if s.ID == m.SessionID {
				return s.ID
			}
			continue
		}
		if m.Pane != "" && m.Pane != LeaderPane && s.Pane == m.Pane {
			return s.ID
		}
	}
	return ""
}

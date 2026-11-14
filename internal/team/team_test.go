package team

import (
	"errors"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/ZachFara/curia/internal/model"
)

func TestParseCapturedConfig(t *testing.T) {
	data, err := os.ReadFile("testdata/config-captured.json")
	if err != nil {
		t.Fatal(err)
	}
	tm, err := Parse("session-bf9b79ad", data)
	if err != nil {
		t.Fatal(err)
	}
	if tm.ID != "session-bf9b79ad" || tm.LeadSessionID != "bf9b79ad-5a7a-472e-94a5-8c58b9033764" || len(tm.Members) != 3 {
		t.Fatalf("team = %+v", tm)
	}
	want := []Member{
		{"team-lead", "team-lead", "", "leader", "/home/dev/src/.curia-worktrees/api-T-5"},
		{"alpha", "general-purpose", "", "%17", "/home/dev/src/.curia-worktrees/api-T-5"},
		{"beta", "general-purpose", "", "%18", "/home/dev/src/.curia-worktrees/api-T-5"},
	}
	for i, w := range want {
		if tm.Members[i] != w {
			t.Errorf("member %d = %+v, want %+v", i, tm.Members[i], w)
		}
	}
	got := Assign([]model.Session{sess("bf9b79ad-5a7a-472e-94a5-8c58b9033764", "%1", model.StateIdle), sess("x", "%17", model.StateIdle), sess("y", "%18", model.StateIdle)}, []Team{tm})
	if got["x"].Member != "alpha" || got["y"].Role != model.RoleTeammate || got["bf9b79ad-5a7a-472e-94a5-8c58b9033764"].Role != model.RoleLead {
		t.Errorf("assign = %+v", got)
	}
	// the literal "leader" pane never matches a session
	if g := Assign([]model.Session{sess("z", "leader", model.StateIdle)}, []Team{tm}); len(g) != 0 {
		t.Errorf("leader pane matched: %+v", g)
	}
}

func members(n int, lead bool) string {
	var parts []string
	for i := 0; i < n; i++ {
		at := "worker"
		if lead && i == 0 {
			at = LeadAgentType
		}
		parts = append(parts, `{"name":"m","agentType":"`+at+`"}`)
	}
	return `{"members":[` + strings.Join(parts, ",") + `]}`
}

func TestParseHostile(t *testing.T) {
	bad := map[string]string{
		"empty":       "",
		"array":       "[]",
		"null":        "null",
		"members str": `{"members":"x"}`,
		"65 members":  members(65, true),
		"no lead":     members(2, false),
		"padding":     `{"members":[],"pad":"` + strings.Repeat("a", 2<<20) + `"}`,
	}
	for name, in := range bad {
		if _, err := Parse("t", []byte(in)); !errors.Is(err, ErrBadConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := Parse("t", []byte(members(64, true))); err != nil {
		t.Errorf("64 members: %v", err)
	}
	hostile := `{"leadSessionId":"x;y","members":[{"name":"a\u001b[2Jb\u202ec","agentType":"team-lead","tmuxPaneId":"%1;kill-server","sessionId":"bad id!"}]}`
	tm, err := Parse("te\u001b[2Jam\u202e", []byte(hostile))
	if err != nil {
		t.Fatal(err)
	}
	if tm.LeadSessionID != "" {
		t.Errorf("lead session id %q", tm.LeadSessionID)
	}
	m := tm.Members[0]
	if m.Pane != "" || m.SessionID != "" {
		t.Errorf("member = %+v", m)
	}
	for _, s := range []string{tm.ID, m.Name, m.AgentType} {
		for _, r := range s {
			if unicode.IsControl(r) || r == '\u202e' {
				t.Errorf("%q holds %U", s, r)
			}
		}
	}
}

func sess(id, pane string, st model.State) model.Session {
	return model.Session{ID: id, Pane: pane, State: st}
}

func tm(id, lead string, ms ...Member) Team {
	return Team{ID: id, LeadSessionID: lead, Members: ms}
}

func TestAssignBySessionIDThenPane(t *testing.T) {
	team := tm("t", "", Member{Name: "boss", AgentType: LeadAgentType, Pane: "%1"},
		Member{Name: "w", SessionID: "s2", Pane: "%2"})
	got := Assign([]model.Session{sess("s1", "%1", model.StateIdle), sess("s3", "%2", model.StateIdle), sess("s2", "%9", model.StateIdle)}, []Team{team})
	if got["s2"] != (Membership{"t", model.RoleTeammate, "w"}) {
		t.Errorf("s2 = %+v", got["s2"])
	}
	if _, ok := got["s3"]; ok {
		t.Errorf("s3 must not match by pane: %+v", got["s3"])
	}
	if got["s1"] != (Membership{"t", model.RoleLead, "boss"}) {
		t.Errorf("s1 = %+v", got["s1"])
	}
}

func TestAssignLeadByLeadSessionID(t *testing.T) {
	team := tm("t", "L", Member{Name: "boss", AgentType: LeadAgentType, Pane: "%1"}, Member{Name: "w", Pane: "%2"})
	got := Assign([]model.Session{sess("L", "%7", model.StateIdle), sess("w1", "%2", model.StateIdle)}, []Team{team})
	if got["L"] != (Membership{"t", model.RoleLead, "boss"}) || got["w1"].Role != model.RoleTeammate {
		t.Errorf("got = %+v", got)
	}
}

func TestAssignNoMatchIsSolo(t *testing.T) {
	team := tm("t", "", Member{Name: "boss", AgentType: LeadAgentType, Pane: "%1"}, Member{Name: "w", Pane: "%2"})
	if got := Assign([]model.Session{sess("a", "%8", model.StateIdle)}, []Team{team}); len(got) != 0 {
		t.Errorf("no match: %+v", got)
	}
	// the lead is unmatched: the teammate is not assigned
	if got := Assign([]model.Session{sess("a", "%2", model.StateIdle)}, []Team{team}); len(got) != 0 {
		t.Errorf("unmatched lead: %+v", got)
	}
	if got := Assign(nil, nil); got == nil || len(got) != 0 {
		t.Errorf("nil inputs: %+v", got)
	}
}

func TestAssignSkipsEndedAndStale(t *testing.T) {
	team := tm("t", "", Member{Name: "boss", AgentType: LeadAgentType, Pane: "%1"}, Member{Name: "w", Pane: "%2"})
	got := Assign([]model.Session{
		sess("e", "%1", model.StateEnded), sess("s", "%2", model.StateStale),
	}, []Team{team})
	if len(got) != 0 {
		t.Errorf("got = %+v", got)
	}
	got = Assign([]model.Session{
		sess("e", "%1", model.StateEnded), sess("l", "%1", model.StateIdle), sess("w", "%2", model.StateWorking),
	}, []Team{team})
	if got["l"].Role != model.RoleLead || got["w"].Role != model.RoleTeammate || len(got) != 2 {
		t.Errorf("got = %+v", got)
	}
}

func TestAssignFirstTeamWins(t *testing.T) {
	a := tm("a", "", Member{Name: "al", AgentType: LeadAgentType, Pane: "%1"}, Member{Name: "aw", Pane: "%2"})
	b := tm("b", "", Member{Name: "bl", AgentType: LeadAgentType, Pane: "%3"}, Member{Name: "bw", Pane: "%2"})
	got := Assign([]model.Session{sess("l1", "%1", model.StateIdle), sess("l2", "%3", model.StateIdle), sess("w", "%2", model.StateIdle)}, []Team{a, b})
	if got["w"] != (Membership{"a", model.RoleTeammate, "aw"}) || got["l2"].TeamID != "b" {
		t.Errorf("got = %+v", got)
	}
}

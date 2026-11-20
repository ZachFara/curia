package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/usage"
)

// activityHome seeds two sessions and a scripted event stream for each, in
// the hour before fleetNow, and resets the command's flags afterwards.
func activityHome(t *testing.T) string {
	t.Helper()
	db, _ := fleetHome(t, nil,
		model.Session{ID: "sa", Repo: "/src/web", Branch: "feat/x", TaskID: "T-1", State: model.StateEnded},
		model.Session{ID: "sb", Repo: "/src/api", Branch: "curia/T-2-y", TaskID: "T-2", State: model.StateEnded})
	t.Cleanup(func() {
		activitySince, activityRepo, activityTask, activityBy, activityJSON = 24*time.Hour, "", "", "class", false
	})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	base := fleetNow.Add(-time.Hour)
	add := func(sid string, k model.EventKind, sec int, raw string) {
		if err := st.AppendEvent(context.Background(), model.Event{Kind: k, SessionID: sid, At: base.Add(time.Duration(sec) * time.Second)}, []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	bash := func(c string) string { return `{"tool_name":"Bash","tool_input":{"command":"` + c + `"}}` }
	// sa: 10s thinking, 60s testing, 30s waiting on me (turn end to prompt), 20s thinking.
	add("sa", model.UserPromptSubmit, 0, `{}`)
	add("sa", model.PreToolUse, 10, bash("go test ./..."))
	add("sa", model.PostToolUse, 70, `{}`)
	add("sa", model.Stop, 70, `{}`)
	add("sa", model.UserPromptSubmit, 100, `{}`)
	add("sa", model.SessionEnd, 120, `{}`)
	// sb overlaps sa: 40s git then ends.
	add("sb", model.UserPromptSubmit, 50, `{}`)
	add("sb", model.PreToolUse, 50, bash("git status"))
	add("sb", model.PostToolUse, 90, `{}`)
	add("sb", model.SessionEnd, 90, `{}`)
	return db
}

func TestActivityCommandByClass(t *testing.T) {
	activityHome(t)
	out, err := runRoot(t, "time")
	if err != nil {
		t.Fatal(err)
	}
	// agent: sa 120s, sb 40s = 160s; waiting 30s = 19%; wall: 0..120 union 50..90 = 120s.
	for _, want := range []string{"waiting on me: 19% of 3m agent time (wall 2m)", "testing", "git", "waiting on me"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestActivityCommandJSONByTypeAndFilters(t *testing.T) {
	activityHome(t)
	out, err := runRoot(t, "time", "--json", "--by", "type")
	if err != nil {
		t.Fatal(err)
	}
	var got activityReport
	if err = json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	if got.AgentSecs != 160 || got.WallSecs != 120 || got.WaitShare == nil || got.ByClass["testing"] != 60 || got.ByClass["git"] != 40 {
		t.Errorf("report = %+v", got)
	}
	if len(got.Rows) != 2 || got.Rows[0].Key != "feat" || got.Rows[0].AgentSecs != 120 || got.Rows[1].Key != "untyped" || len(got.Sessions) != 2 {
		t.Errorf("rows = %+v", got.Rows)
	}
	activityFilters(t)
}

func activityFilters(t *testing.T) {
	t.Helper()
	for _, args := range [][]string{{"--repo", "api"}, {"--task", "T-2"}} {
		out, err := runRoot(t, append([]string{"time", "--json", "--by", "task"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		var got activityReport
		if err = json.Unmarshal([]byte(out), &got); err != nil || got.AgentSecs != 40 || len(got.Rows) != 1 || got.Rows[0].Key != "T-2" {
			t.Errorf("%v: %v %+v", args, err, got)
		}
	}
}

func TestActivityCommandEmptyAndBadFlags(t *testing.T) {
	fleetHome(t, nil)
	t.Cleanup(func() { activityBy, activityJSON = "class", false })
	out, err := runRoot(t, "time")
	if err != nil || !strings.Contains(out, "no agent time in this window") {
		t.Errorf("empty = %q, %v", out, err)
	}
	if _, err = runRoot(t, "time", "--by", "nope"); err == nil {
		t.Error("a bad --by is accepted")
	}
}

// A repository's rules (F28, later) come through rulesFor and change only
// that repository's measurement.
func TestActivityRepoRulesOverrideOnlyThatRepo(t *testing.T) {
	db := activityHome(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	repoRules, err := usage.NewActivityRules([]config.ActivityRule{{Class: usage.ClassBuilding, Match: `^git status`}})
	if err != nil {
		t.Fatal(err)
	}
	def, _ := usage.NewActivityRules(nil)
	rulesFor := func(repo string) *usage.ActivityRules {
		if repo == "/src/api" {
			return repoRules
		}
		return def
	}
	rep, err := activityBuild(context.Background(), st, fleetNow, rulesFor, 30*time.Minute, []string{"feat"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ByClass["building"] != 40 || rep.ByClass["git"] != 0 || rep.ByClass["testing"] != 60 {
		t.Errorf("by class = %v", rep.ByClass)
	}
}

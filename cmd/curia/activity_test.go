package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// activityRepoDir makes a repository directory with the given
// .curia/config.toml.
func activityRepoDir(t *testing.T, repoToml string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, ".curia"), 0o750); err != nil {
		t.Fatal(err)
	}
	if repoToml != "" {
		if err := os.WriteFile(filepath.Join(d, ".curia", "config.toml"), []byte(repoToml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestActivityRepositoryRulesApplyOnlyToThatRepository(t *testing.T) {
	a := activityRepoDir(t, "[[activity.rules]]\nclass = \"git\"\nmatch = \"^zzship\"\n")
	b := activityRepoDir(t, "")
	user := []config.ActivityRule{{Class: usage.ClassBuilding, Match: `^user-rule`}}
	rulesFor := activityRulesFor(user)
	if got := rulesFor(a).Class("Bash", "zzship it"); got != "git" {
		t.Errorf("repository rule not applied in its own repository: %q", got)
	}
	if got := rulesFor(b).Class("Bash", "zzship it"); got == "git" {
		t.Errorf("a repository's rule applied to another repository: %q", got)
	}
	if got := rulesFor(b).Class("Bash", "user-rule x"); got != "building" {
		t.Errorf("the user's rule is lost: %q", got)
	}
}

func TestActivityHostileRepositoryRulesCannotBreakTime(t *testing.T) {
	var many strings.Builder
	for i := 0; i < 80; i++ {
		many.WriteString("[[activity.rules]]\nclass = \"git\"\nmatch = \"^x" + strconv.Itoa(i) + "\"\n")
	}
	cases := map[string]string{
		"bad regexp":    "[[activity.rules]]\nclass = \"git\"\nmatch = \"(\"\n[[activity.rules]]\nclass = \"building\"\nmatch = \"^ok\"\n",
		"bad class":     "[[activity.rules]]\nclass = \"nope\"\nmatch = \"a\"\n",
		"long pattern":  "[[activity.rules]]\nclass = \"git\"\nmatch = \"" + strings.Repeat("a", 500) + "\"\n",
		"invalid TOML":  "[[activity.rules\n",
		"wrong type":    "[activity]\nrules = 5\n",
		"too many":      many.String(),
		"oversized":     "# " + strings.Repeat("x", 70<<10),
		"empty rule":    "[[activity.rules]]\nclass = \"git\"\n",
		"not a table":   "activity = 1\n",
		"unknown field": "[[activity.rules]]\nclass = \"git\"\nmatch = \"^a\"\nexec = \"/bin/sh\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			d := activityRepoDir(t, body)
			r := activityRulesFor(nil)(d) // must not panic or fail
			if r == nil || r.Class("Bash", "go test ./...") != "testing" {
				t.Errorf("the default rules are lost")
			}
			if _, problems := activityRepoRules(nil, d); name == "bad regexp" && len(problems) != 1 {
				t.Errorf("problems = %v, want the one bad rule named", problems)
			}
		})
	}
	// A symlinked file pointing outside the repository is not read.
	d := activityRepoDir(t, "")
	out := filepath.Join(t.TempDir(), "evil.toml")
	if err := os.WriteFile(out, []byte("[[activity.rules]]\nclass = \"building\"\nmatch = \"^evil\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(d, ".curia", "config.toml")); err != nil {
		t.Fatal(err)
	}
	if got := activityRulesFor(nil)(d).Class("Bash", "evil"); got == "building" {
		t.Error("a symlinked repository file was read")
	}
	// And `curia time` itself still runs.
	activityHome(t)
	t.Chdir(activityRepoDir(t, "[[activity.rules]]\nclass = \"git\"\nmatch = \"(\"\n"))
	if _, err := runRoot(t, "time"); err != nil {
		t.Errorf("curia time failed on a hostile repository file: %v", err)
	}
}

func TestConfigListShowsActivityKeys(t *testing.T) {
	configTestEnv(t)
	out := configRun(t, "list")
	for _, k := range []string{"activity.idle_cap = 30m0s  (default)", "activity.rules = (0 entries)  (default)", "activity.task_types = (7 entries)  (default)"} {
		if !strings.Contains(out, k) {
			t.Errorf("list lacks %q:\n%s", k, out)
		}
	}
}

func TestActivityCommandShowsUnfinishedBackground(t *testing.T) {
	db, _ := fleetHome(t, nil, model.Session{ID: "sc", Repo: "/src/web", Branch: "feat/x", TaskID: "T-1", State: model.StateEnded})
	t.Cleanup(func() {
		activitySince, activityRepo, activityTask, activityBy, activityJSON = 24*time.Hour, "", "", "class", false
	})
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	base := fleetNow.Add(-time.Hour)
	for i, e := range []struct {
		k   model.EventKind
		sec int
		raw string
	}{
		{model.UserPromptSubmit, 0, `{"prompt":"go"}`},
		{model.PreToolUse, 10, `{"tool_name":"Bash","tool_use_id":"u1","tool_input":{"command":"go test ./...","run_in_background":true}}`},
		{model.PostToolUse, 10, `{"tool_name":"Bash","tool_use_id":"u1","tool_response":{"backgroundTaskId":"b1"}}`},
		{model.Stop, 20, `{"background_tasks":[{"id":"b1","type":"shell","status":"running"}]}`},
		{model.SessionEnd, 620, `{}`},
	} {
		if err = st.AppendEvent(context.Background(), model.Event{Kind: e.k, SessionID: "sc", At: base.Add(time.Duration(e.sec) * time.Second)}, []byte(e.raw)); err != nil {
			t.Fatal(i, err)
		}
	}
	_ = st.Close()
	out, err := runRoot(t, "time")
	if err != nil {
		t.Fatal(err)
	}
	// 10 minutes in all; the agent's own command is testing, not waiting on me.
	if !strings.Contains(out, "waiting on me: 0% of 10m") || !strings.Contains(out, "background commands: 10m (testing 10m), 1 unfinished") {
		t.Errorf("output:\n%s", out)
	}
	out, err = runRoot(t, "time", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Unfinished int                `json:"unfinished_background"`
		Background map[string]float64 `json:"background_by_class"`
	}
	if err = json.Unmarshal([]byte(out), &rep); err != nil || rep.Unfinished != 1 || rep.Background["testing"] != 610 {
		t.Errorf("json %v, %+v:\n%s", err, rep, out)
	}
}

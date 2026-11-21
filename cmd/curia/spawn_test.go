package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/gitops"
	"github.com/ZachFara/curia/internal/lifecycle"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/task"
	"github.com/ZachFara/curia/internal/testkit"
	"github.com/ZachFara/curia/internal/tmux"
)

// spawnTestBin is the curia binary path spawn tests give the window.
const spawnTestBin = testkit.SpawnTestBin

// spawnFixture is the shared spawn fixture (internal/testkit) with the
// commands' clock and tmux client pointed at it; the spawn tests' own
// helpers hang off it.
type spawnFixture struct{ *testkit.SpawnFixture }

func spawnNewFixture(t *testing.T) *spawnFixture {
	t.Helper()
	userTerminalAs(t, true) // the user, at a terminal
	f := &spawnFixture{testkit.NewSpawnFixture(t)}
	prevClock, prevTmux := clock, newTmuxClient
	clock = func() time.Time { return f.Now }
	newTmuxClient = func() tmux.Client { return f.Fake }
	t.Cleanup(func() { clock, newTmuxClient = prevClock, prevTmux })
	return f
}

// spawnRun runs `curia spawn args...` in-process and returns its output.
func spawnRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	defer func() { spawnAgent, spawnFlagModel, spawnTeam = "", "", false }()
	return runRoot(t, append([]string{"spawn"}, args...)...)
}

func TestSpawnCreatesWorktreeWindowAndRecords(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Fix the auth refresh", "Make it refresh.")
	mainHead := testkit.WorktreeGit(t, f.Repo, "rev-parse", "main")

	out, err := spawnRun(t, "T-1")
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	wt := f.Worktree("T-1")
	branch := "curia/T-1-fix-the-auth-refresh"
	if got := testkit.WorktreeGit(t, wt, "symbolic-ref", "HEAD"); got != "refs/heads/"+branch {
		t.Errorf("worktree HEAD = %q, want refs/heads/%s", got, branch)
	}
	if got := testkit.WorktreeGit(t, wt, "rev-parse", "HEAD"); got != mainHead {
		t.Errorf("worktree starts at %s, want main's %s", got, mainHead)
	}
	want := []string{"NewWindow api-server T-1 " + wt + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1"}
	if got := f.Windows(); !slices.Equal(got, want) {
		t.Errorf("windows = %q, want %q", got, want)
	}
	got := f.Get("T-1")
	if got.Branch != branch || got.Worktree != wt || got.Pane != "%100" || !got.LaunchedAt.Equal(f.Start) ||
		got.Status != model.TaskReady || got.Team || got.AgentModel != "" {
		t.Errorf("task = %+v", got)
	}
	for _, line := range []string{wt, branch, "T-1 launched in api-server:T-1 (%100); the agent has not reported yet"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}
	if f.Sleeps != int(lifecycle.SpawnWait/lifecycle.SpawnPoll) {
		t.Errorf("waited %d polls, want %d (20 s)", f.Sleeps, lifecycle.SpawnWait/lifecycle.SpawnPoll)
	}
}

func TestSpawnModelAndTeamFlags(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("One", "")
	f.Task("Two", "")
	if out, err := spawnRun(t, "T-1", "--model", "opus", "--team"); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	want := []string{"NewWindow api-server T-1 " + f.Worktree("T-1") + " CURIA_TASK_ID=T-1 " + spawnTestBin + " launch T-1 --model opus --team"}
	if got := f.Windows(); !slices.Equal(got, want) {
		t.Errorf("windows = %q, want %q", got, want)
	}
	if got := f.Get("T-1"); got.AgentModel != "opus" || !got.Team {
		t.Errorf("task model %q team %v, want opus, true", got.AgentModel, got.Team)
	}

	f.Fake.Calls = nil
	if out, err := spawnRun(t, "T-2", "--model", "x;y"); err == nil {
		t.Fatalf("spawn --model x;y succeeded:\n%s", out)
	}
	f.WantNothingStarted("T-2")

	// A model stored on the task is used when no --model is given.
	f.Update("T-2", func(tk *model.Task) { tk.AgentModel = "sonnet" })
	if _, out, err := f.Spawn("T-2", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn T-2: %v\n%s", err, out)
	}
	if w := f.Windows(); len(w) != 1 || !strings.HasSuffix(w[0], " launch T-2 --model sonnet") {
		t.Errorf("windows = %q, want the stored model", w)
	}
	if got := f.Get("T-2"); got.AgentModel != "sonnet" || got.Team {
		t.Errorf("T-2 model %q team %v, want sonnet, false", got.AgentModel, got.Team)
	}
}

func TestSpawnRefusals(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Dropped", "")
	f.Update("T-1", func(tk *model.Task) { tk.Status = model.TaskDropped })
	f.Task("Paused", "")
	f.Update("T-2", func(tk *model.Task) { tk.PausedAt = f.Now.Add(-time.Hour) })
	f.Task("Plain", "")
	f.Task("Bad base", "")
	f.Update("T-4", func(tk *model.Task) { tk.BaseBranch = "no-such-base" })

	cases := []struct {
		args []string
		id   string
		want string
	}{
		{[]string{"T-9"}, "T-9", "no task T-9"},
		{[]string{"T-1;x"}, "T-1;x", "not a task id"},
		{[]string{"T-1"}, "T-1", "dropped"},
		{[]string{"T-2"}, "T-2", "use curia resume"},
		{[]string{"T-3", "--agent", "gemini"}, "T-3", "claude or codex"},
		{[]string{"T-4"}, "T-4", "does not resolve"},
	}
	for _, c := range cases {
		out, err := spawnRun(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("spawn %q = %v, want an error containing %q\n%s", c.args, err, c.want, out)
		}
		if task.ValidID(c.id) {
			f.WantNothingStarted(c.id)
		}
	}
	if ok, err := gitops.RefExists(context.Background(), f.Repo, "refs/heads/"+task.Branch("T-4", "Bad base")); err != nil || ok {
		t.Errorf("branch of T-4: exists %v, err %v", ok, err)
	}
}

func TestSpawnSetupFailureLeavesWorktree(t *testing.T) {
	f := spawnNewFixture(t)
	f.SetupScript("#!/bin/sh\necho broken\nexit 1\n", true)
	f.Task("Setup", "")
	if _, _, err := f.Spawn("T-1", lifecycle.SpawnOptions{}); err == nil {
		t.Fatal("spawn with a failing setup succeeded")
	}
	wt := f.Worktree("T-1")
	if ok, err := gitops.WorktreeIsOurs(context.Background(), f.Repo, wt, f.Get("T-1").Branch); err != nil || !ok {
		t.Fatalf("worktree after a failed setup: ours %v, err %v", ok, err)
	}
	if w := f.Windows(); len(w) != 0 {
		t.Errorf("windows opened: %q", w)
	}
	if got := f.Get("T-1").LaunchedAt; !got.IsZero() {
		t.Errorf("launched_at = %v, want released", got)
	}
	// Fixing the script in the worktree (the file keeps its mode) lets a
	// second spawn through.
	testkit.WorktreeWrite(t, filepath.Join(wt, ".curia", "setup.sh"), "#!/bin/sh\necho fixed\n")
	setupAnswer(t, testkit.SetupSum("#!/bin/sh\necho fixed\n")[:8], nil)
	if out, err := runRoot(t, "setup", "approve", "T-1"); err != nil {
		t.Fatalf("setup approve: %v\n%s", err, out)
	}
	_, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("second spawn: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(f.SetupLog()); string(b) != "fixed\n" {
		t.Errorf("setup log = %q, want it truncated for the second run", b)
	}
	if w := f.Windows(); len(w) != 1 {
		t.Errorf("windows = %q, want one", w)
	}
}

func TestAgentSpawnNeedsReady(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Backlog one", "")
	f.Task("Ready one", "")
	f.Update("T-2", func(tk *model.Task) { tk.Status = model.TaskReady })
	userTerminalAs(t, false) // an agent's tool shell: no controlling terminal
	out, err := spawnRun(t, "T-1")
	if err == nil || !strings.Contains(err.Error(), "T-1 is backlog: an agent may spawn only a task the user marked ready") {
		t.Fatalf("agent spawn of a backlog task = %v (%q), want the refusal", err, out)
	}
	f.WantNothingStarted("T-1")
	if out, err = spawnRun(t, "T-2"); err != nil {
		t.Fatalf("agent spawn of a ready task: %v\n%s", err, out)
	}
	// The user at a terminal spawns a backlog task, which becomes ready.
	userTerminalAs(t, true)
	f.Task("Another", "")
	if out, err = spawnRun(t, "T-3"); err != nil {
		t.Fatalf("the user's spawn of a backlog task: %v\n%s", err, out)
	}
	if got := f.Get("T-3").Status; got != model.TaskReady {
		t.Errorf("T-3 status = %s, want ready", got)
	}
}

// TestAgentCannotSpawnReviewedOrDoneTask: a task stored ready whose newest
// review is pending, approved or merged is in review or done, and an agent
// may not start it again; one whose newest review asked for changes may be
// started, and the user may start any of them.
func TestAgentCannotSpawnReviewedOrDoneTask(t *testing.T) {
	for _, c := range []struct {
		status model.ReviewStatus
		want   string // "" when the spawn is allowed
	}{
		{model.ReviewPending, "T-1 is review (R-1 is pending)"},
		{model.ReviewApproved, "T-1 is done (R-1 is approved)"},
		{model.ReviewMerged, "T-1 is done (R-1 is merged)"},
		{model.ReviewChanges, ""},
	} {
		t.Run(string(c.status), func(t *testing.T) { spawnReviewedCase(t, c.status, c.want) })
	}
}

// spawnReviewedCase is one case of TestAgentCannotSpawnReviewedOrDoneTask.
func spawnReviewedCase(t *testing.T, status model.ReviewStatus, want string) {
	ctx := context.Background()
	f := spawnNewFixture(t)
	f.Task("Finished", "")
	f.Update("T-1", func(tk *model.Task) { tk.Status = model.TaskReady })
	if _, _, err := f.St.CompleteTask(ctx, model.Review{TaskID: "T-1", Summary: "done", Tests: model.TestsPassed, CreatedAt: f.Now}); err != nil {
		t.Fatal(err)
	}
	if status != model.ReviewPending {
		if _, _, err := f.St.CloseReview(ctx, "R-1", store.ReviewClose{Status: status, How: model.ApproveMerge, Via: model.AnsweredCLI}, f.Now); err != nil {
			t.Fatal(err)
		}
	}
	userTerminalAs(t, false) // an agent's tool shell
	out, err := spawnRun(t, "T-1")
	if want == "" {
		if err != nil {
			t.Fatalf("agent spawn after changes were requested: %v\n%s", err, out)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want+": an agent may spawn only a task the user marked ready that is neither in review nor done") {
		t.Fatalf("agent spawn = %v (%q), want the refusal", err, out)
	}
	f.WantNothingStarted("T-1")
}

// spawnResets is the reset of the five-hour window in the forecast tests:
// 1h50m after the fixture's start.
const spawnResets = time.Hour + 50*time.Minute

// spawnIngest stores one five-hour usage sample at, used pct, the window
// resetting spawnResets after the fixture's start.
func (f *spawnFixture) spawnIngest(at time.Time, pct float64) {
	f.T.Helper()
	usageFeedSession(f.T, f.St, "usage-feed")
	if _, err := f.St.Ingest(context.Background(), store.Reading{
		SessionID: "usage-feed", At: at,
		Limits: []model.UsageSample{{Window: model.WindowFiveHour, UsedPct: pct, ResetsAt: f.Start.Add(spawnResets)}},
	}, f.Now); err != nil {
		f.T.Fatal(err)
	}
}

// spawnWarning ingests a burn that reaches the limit before the reset: 50%
// ten minutes ago, 62% now (72 points an hour, the limit in about 31
// minutes, the reset in 1h50m).
func (f *spawnFixture) spawnWarning() {
	f.T.Helper()
	f.spawnIngest(f.Now.Add(-10*time.Minute), 50)
	f.spawnIngest(f.Now, 62)
}

// spawnWarningNote is the note of section 4.4 for spawnWarning and task id.
func spawnWarningNote(id string) string {
	return "limit forecast (5h 62% → limit ~31m (resets 1h50m)): starting " + id + " on sonnet; pass --model to choose"
}

func TestSpawnDefaultsToCheaperModelUnderWarning(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Cheap", "")
	f.spawnWarning()
	out, err := spawnRun(t, "T-1")
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1", "--model", "sonnet")}) {
		t.Errorf("windows = %q, want --model sonnet", got)
	}
	// The cheaper model is this spawn's only (Ruling P5-R7): not stored.
	if got := f.Get("T-1").AgentModel; got != "" {
		t.Errorf("agent_model = %q, want none stored", got)
	}
	if first, _, _ := strings.Cut(out, "\n"); first != spawnWarningNote("T-1") {
		t.Errorf("first line = %q, want %q\n%s", first, spawnWarningNote("T-1"), out)
	}
	if strings.Count(out, "limit forecast") != 1 {
		t.Errorf("the note is not printed exactly once:\n%s", out)
	}

	// lifecycle.SpawnTask itself returns the note for callers that show no output
	// (the clock back where the CLI spawn's 20 s wait started).
	f.Now = f.Start
	f.Task("Cheap too", "")
	r, _, err := f.Spawn("T-2", lifecycle.SpawnOptions{})
	if err != nil || r.Note != spawnWarningNote("T-2") {
		t.Errorf("lifecycle.SpawnTask: note %q, err %v; want %q", r.Note, err, spawnWarningNote("T-2"))
	}
}

func TestSpawnModelFlagBeatsForecast(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Mine", "")
	f.spawnWarning()
	out, err := spawnRun(t, "T-1", "--model", "opus")
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1", "--model", "opus")}) {
		t.Errorf("windows = %q, want --model opus", got)
	}
	if strings.Contains(out, "limit forecast") || f.Get("T-1").AgentModel != "opus" {
		t.Errorf("model %q, output:\n%s\nwant opus and no note", f.Get("T-1").AgentModel, out)
	}
}

func TestSpawnKeepsTaskModelUnderWarning(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Kept", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{Model: "opus"}); err != nil {
		t.Fatalf("first spawn: %v\n%s", err, out)
	}
	f.Fake.Panes, f.Fake.Calls = nil, nil // the agent exited, its window closed
	f.Now = f.Now.Add(time.Minute)
	f.spawnWarning()
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("second spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1", "--model", "opus")}) {
		t.Errorf("windows = %q, want the task's opus", got)
	}
	if r.Note != "" || strings.Contains(out, "limit forecast") || f.Get("T-1").AgentModel != "opus" {
		t.Errorf("note %q, model %q, output:\n%s", r.Note, f.Get("T-1").AgentModel, out)
	}
}

func TestSpawnNoCheaperModelWithoutWarning(t *testing.T) {
	for _, c := range []struct {
		name string
		feed func(f *spawnFixture)
	}{
		{"no samples", func(*spawnFixture) {}},
		{"steady", func(f *spawnFixture) {
			f.spawnIngest(f.Now.Add(-15*time.Minute), 62)
			f.spawnIngest(f.Now.Add(-5*time.Minute), 62)
			f.spawnIngest(f.Now, 62)
		}},
		// 6 points an hour: the limit is about 6 hours away, after the reset.
		{"limit after the reset", func(f *spawnFixture) {
			f.spawnIngest(f.Now.Add(-10*time.Minute), 61)
			f.spawnIngest(f.Now, 62)
		}},
		// A warning burn that is older than the burn window.
		{"old burn", func(f *spawnFixture) {
			f.spawnIngest(f.Now.Add(-40*time.Minute), 50)
			f.spawnIngest(f.Now.Add(-30*time.Minute), 62)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := spawnNewFixture(t)
			f.Task("Plain", "")
			c.feed(f)
			r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
			if err != nil {
				t.Fatalf("spawn: %v\n%s", err, out)
			}
			if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) {
				t.Errorf("windows = %q, want no --model", got)
			}
			if r.Note != "" || strings.Contains(out, "limit forecast") || f.Get("T-1").AgentModel != "" {
				t.Errorf("note %q, model %q, output:\n%s", r.Note, f.Get("T-1").AgentModel, out)
			}
		})
	}
}

func TestSpawnCheaperModelOff(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Off", "")
	f.spawnWarning()
	f.Cfg.Usage.CheaperModel = ""
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) || r.Note != "" {
		t.Errorf("windows = %q, note %q; want no --model and no note", got, r.Note)
	}
	if log := strings.Join(readLog(t), "\n"); strings.Contains(log, "cheaper_model") || strings.Contains(log, "limit forecast") {
		t.Errorf("an empty cheaper_model is a choice, not a fault, yet it was logged:\n%s", log)
	}

	// The same through the config file.
	// The fixture's store is in its HOME.
	testkit.WorktreeWrite(t, filepath.Join(filepath.Dir(f.DB), ".config", "curia", "config.toml"), "[usage]\ncheaper_model = \"\"\n")
	f.Fake.Panes, f.Fake.Calls = nil, nil
	f.Task("Off too", "")
	out, err = spawnRun(t, "T-2")
	if err != nil {
		t.Fatalf("spawn T-2: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-2")}) || strings.Contains(out, "limit forecast") {
		t.Errorf("windows = %q, output:\n%s\nwant no --model and no note", got, out)
	}
}

// TestSpawnHostileCheaperModelIgnored: a cheaper model that is not a valid
// model name, or would read as an option, never reaches the window (the
// config loader refuses it too; this is the second line).
func TestSpawnHostileCheaperModelIgnored(t *testing.T) {
	for _, bad := range []string{"x;rm -rf ~", "--dangerously-skip-permissions", "-s", "$(id)", "a b"} {
		t.Run(bad, func(t *testing.T) {
			f := spawnNewFixture(t)
			f.Task("Hostile", "")
			f.spawnWarning()
			f.Cfg.Usage.CheaperModel = bad
			r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
			if err != nil {
				t.Fatalf("spawn: %v\n%s", err, out)
			}
			if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) || r.Note != "" {
				t.Errorf("windows = %q, note %q; want no --model and no note", got, r.Note)
			}
			if f.Get("T-1").AgentModel != "" {
				t.Errorf("agent_model = %q", f.Get("T-1").AgentModel)
			}
		})
	}
}

func TestSpawnResumeIgnoresForecast(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Resumed", "")
	f.spawnWarning()
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{Resume: launchUUID})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1", "--resume", launchUUID)}) || r.Note != "" {
		t.Errorf("windows = %q, note %q; want no --model and no note", got, r.Note)
	}

	// The task's stored model still applies on a resume.
	f.Task("Resumed with a model", "")
	f.Update("T-2", func(tk *model.Task) { tk.AgentModel = "opus" })
	f.Fake.Calls = nil
	if r, out, err = f.Spawn("T-2", lifecycle.SpawnOptions{Resume: launchUUID}); err != nil {
		t.Fatalf("spawn T-2: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-2", "--model", "opus", "--resume", launchUUID)}) || r.Note != "" {
		t.Errorf("windows = %q, note %q; want the stored opus and no note", got, r.Note)
	}
}

func TestSpawnUsageReadFailureDoesNotBlock(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Broken usage", "")
	f.spawnWarning()
	statusExec(t, f.DB, "DROP TABLE usage_samples")
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) || r.Note != "" {
		t.Errorf("windows = %q, note %q; want no --model and no note", got, r.Note)
	}
	if log := strings.Join(readLog(t), "\n"); !strings.Contains(log, "limit forecast") || !strings.Contains(log, "T-1") {
		t.Errorf("the failed read is not logged:\n%s", log)
	}
}

func TestSpawnAgentCodexIsStored(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Codex", "")
	out, err := spawnRun(t, "T-1", "--agent", "codex")
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if got := f.Get("T-1").Agent; got != model.AgentCodex {
		t.Errorf("tasks.agent = %q, want codex", got)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) {
		t.Errorf("windows = %q, want [<bin> launch T-1]", got)
	}

	// Without --agent the task's own agent is used; --agent claude switches.
	again := func(o lifecycle.SpawnOptions, want string) {
		t.Helper()
		f.Fake.Panes, f.Fake.Calls = nil, nil
		f.Now = f.Now.Add(time.Minute)
		if _, out, err := f.Spawn("T-1", o); err != nil {
			t.Fatalf("spawn %+v: %v\n%s", o, err, out)
		}
		if got := f.Get("T-1").Agent; got != want {
			t.Errorf("spawn %+v: tasks.agent = %q, want %q", o, got, want)
		}
	}
	again(lifecycle.SpawnOptions{}, model.AgentCodex)
	again(lifecycle.SpawnOptions{Agent: model.AgentClaude}, model.AgentClaude)
	again(lifecycle.SpawnOptions{Agent: model.AgentCodex}, model.AgentCodex)
	// A resume keeps the stored agent whatever the option says.
	again(lifecycle.SpawnOptions{Agent: model.AgentClaude, Resume: launchUUID}, model.AgentCodex)

	// A task never spawned is claude.
	f.Task("Default", "")
	f.Fake.Panes = nil
	if _, out, err := f.Spawn("T-2", lifecycle.SpawnOptions{}); err != nil {
		t.Fatalf("spawn T-2: %v\n%s", err, out)
	}
	if got := f.Get("T-2").Agent; got != model.AgentClaude {
		t.Errorf("T-2 agent = %q, want claude", got)
	}
}

func TestSpawnAgentRefusals(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Refused", "")
	f.Task("Codex task", "")
	f.Update("T-2", func(tk *model.Task) { tk.Agent = model.AgentCodex })
	cases := []struct {
		id   string
		args []string
		want string
	}{
		{"T-1", []string{"--agent", "gemini"}, `--agent "gemini" is not supported: claude or codex`},
		{"T-1", []string{"--agent", "codex; rm -rf ~"}, "is not supported"},
		{"T-1", []string{"--agent", "$(touch x)"}, "is not supported"},
		{"T-1", []string{"--agent", "CODEX"}, "is not supported"},
		{"T-1", []string{"--agent", "codex\n--dangerously-bypass-approvals-and-sandbox"}, "is not supported"},
		{"T-1", []string{"--agent", "codex", "--team"}, "agent teams are Claude Code's; spawn T-1 without --team or without --agent codex"},
		// A task stored as codex, spawned with --team and no --agent.
		{"T-2", []string{"--team"}, "agent teams are Claude Code's; spawn T-2 without --team or without --agent codex"},
	}
	for _, c := range cases {
		out, err := spawnRun(t, append([]string{c.id}, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("spawn %s %q = %v, want an error containing %q\n%s", c.id, c.args, err, c.want, out)
		}
		if err != nil && strings.ContainsFunc(err.Error(), func(r rune) bool { return r < 0x20 }) {
			t.Errorf("spawn %s %q: error not cleaned: %q", c.id, c.args, err)
		}
		f.WantNothingStarted(c.id)
	}
	if got := f.Get("T-1").Agent; got != model.AgentClaude {
		t.Errorf("T-1 agent = %q after refusals, want claude", got)
	}
}

func TestSpawnCodexNeverGetsCheaperModel(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Codex under a warning", "")
	f.spawnWarning()
	out, err := spawnRun(t, "T-1", "--agent", "codex")
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) {
		t.Errorf("windows = %q, want no --model", got)
	}
	if strings.Contains(out, "limit forecast") || f.Get("T-1").AgentModel != "" {
		t.Errorf("model %q, output:\n%s", f.Get("T-1").AgentModel, out)
	}

	// A task stored as codex, spawned again with no --agent: the same.
	f.Fake.Panes, f.Fake.Calls = nil, nil
	f.Now = f.Now.Add(time.Minute)
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("second spawn: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) || r.Note != "" {
		t.Errorf("windows = %q, note %q", got, r.Note)
	}
}

// TestSpawnCheaperModelOnlyForThatSpawn: the cheaper model chosen under a
// limit warning reaches that spawn's window and is not stored, so the next
// spawn after the warning lifted starts on no model (Ruling P5-R7).
func TestSpawnCheaperModelOnlyForThatSpawn(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Cheap once", "")
	f.spawnWarning()
	r, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn under the warning: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1", "--model", "sonnet")}) || r.Note != spawnWarningNote("T-1") {
		t.Fatalf("windows = %q, note %q; want --model sonnet and the note", got, r.Note)
	}
	if got := f.Get("T-1").AgentModel; got != "" {
		t.Errorf("agent_model = %q after a spawn on the cheaper model, want none stored", got)
	}
	// The window closed and the warning lifted: the burn is outside the window.
	f.Fake.Panes, f.Fake.Calls = nil, nil
	f.Now = f.Now.Add(45 * time.Minute)
	r, out, err = f.Spawn("T-1", lifecycle.SpawnOptions{})
	if err != nil {
		t.Fatalf("spawn after the warning: %v\n%s", err, out)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1")}) || r.Note != "" {
		t.Errorf("windows = %q, note %q; want no --model once the warning lifted", got, r.Note)
	}
}

func TestSpawnCodexModelNeverReachesClaude(t *testing.T) {
	f := spawnNewFixture(t)
	f.Task("Switch back", "")
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{Agent: model.AgentCodex}); err != nil {
		t.Fatalf("codex spawn: %v\n%s", err, out)
	}
	f.Fake.Panes, f.Fake.Calls = nil, nil
	f.Now = f.Now.Add(time.Minute)
	if _, out, err := f.Spawn("T-1", lifecycle.SpawnOptions{Model: "gpt-5"}); err != nil {
		t.Fatalf("codex spawn with a model: %v\n%s", err, out)
	}
	if tk := f.Get("T-1"); tk.Agent != model.AgentCodex || tk.AgentModel != "gpt-5" {
		t.Fatalf("stored %q/%q, want codex/gpt-5", tk.Agent, tk.AgentModel)
	}
	f.Fake.Panes, f.Fake.Calls = nil, nil
	f.Now = f.Now.Add(time.Minute)
	f.spawnWarning()
	r, outText, err := f.Spawn("T-1", lifecycle.SpawnOptions{Agent: model.AgentClaude})
	if err != nil {
		t.Fatalf("claude spawn: %v\n%s", err, outText)
	}
	if got := f.Windows(); !slices.Equal(got, []string{f.SpawnWindowOf("T-1", "--model", "sonnet")}) {
		t.Errorf("windows = %q, want the cheaper sonnet and never gpt-5", got)
	}
	// Two lines: the stored model not used, then the forecast's (its reset
	// is two minutes nearer than spawnWarningNote's).
	want := testkit.SpawnOtherAgentNote("T-1", "gpt-5", "codex", "claude") + "\n" +
		"limit forecast (5h 62% → limit ~31m (resets 1h48m)): starting T-1 on sonnet; pass --model to choose"
	if r.Note != want || !strings.HasPrefix(outText, want+"\n") {
		t.Errorf("note %q, output:\n%s\nwant %q", r.Note, outText, want)
	}
	if tk := f.Get("T-1"); tk.Agent != model.AgentClaude || tk.AgentModel != "" {
		t.Errorf("stored %q/%q, want claude and no model (the cheaper one is not stored)", tk.Agent, tk.AgentModel)
	}
}

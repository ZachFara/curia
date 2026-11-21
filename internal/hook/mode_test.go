package hook_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/hook"
	latgate "github.com/ZachFara/curia/internal/latency"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// modeSession makes session A (so its row exists) and gives it mode until.
func modeSession(t *testing.T, st *store.Store, env hook.Env, cfg config.Config, proj, mode string, until time.Time) {
	t.Helper()
	pre(t, st, env, cfg, preBody("Bash", `{"command":"ls"}`, proj, "")) // creates the row
	if err := st.UpdateSession(context.Background(), "A", func(s model.Session, _ bool) model.Session { s.Worktree = proj; return s }); err != nil {
		t.Fatal(err)
	}
	if mode == "" {
		return
	}
	if n, err := st.SetModes(context.Background(), store.ModeScope{All: true}, mode, until); err != nil || n != 1 {
		t.Fatalf("SetModes = %d, %v", n, err)
	}
}

// wantAllow checks the observed allow form: the deny form with "allow".
func wantAllow(t *testing.T, res hook.Result, what string) {
	t.Helper()
	raw := map[string]map[string]string{}
	if err := json.Unmarshal(res.Stdout, &raw); err != nil || strings.Count(string(res.Stdout), "\n") != 1 {
		t.Fatalf("%s: stdout %q is not one allow line: %v", what, res.Stdout, err)
	}
	h := raw["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "allow" || h["permissionDecisionReason"] == "" || len(h) != 3 {
		t.Fatalf("%s: stdout %q, want a PreToolUse allow with a reason", what, res.Stdout)
	}
}

// TestModeDecisionTable: (mode, tool, input) -> allow | deny | quiet, with the
// protected file and the budget stop winning in every mode.
func TestModeDecisionTable(t *testing.T) {
	home, proj, cfg := denyHome(t)
	cfgDir := filepath.Join(home, ".config", "curia")
	outside := filepath.Join(home, "other", "x.go")
	in := func(tool, key, p string) [2]string { return [2]string{tool, `{"` + key + `":` + quote(p) + `}`} }
	cases := []struct {
		name string
		call [2]string
		want map[string]string // mode -> allow | deny | quiet
	}{
		{"write inside", in("Write", "file_path", proj+"/a.go"), map[string]string{"normal": "quiet", "edits": "allow", "away": "allow", "bypass": "allow"}},
		{"edit relative inside", in("Edit", "file_path", "a.go"), map[string]string{"normal": "quiet", "edits": "allow", "away": "allow", "bypass": "allow"}},
		{"notebook inside", in("NotebookEdit", "notebook_path", proj+"/n.ipynb"), map[string]string{"normal": "quiet", "edits": "allow", "away": "allow", "bypass": "allow"}},
		{"write outside", in("Write", "file_path", outside), map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"dotdot out", in("Write", "file_path", proj+"/../other/x.go"), map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"protected file", in("Write", "file_path", cfgDir+"/config.toml"), map[string]string{"normal": "deny", "edits": "deny", "away": "deny", "bypass": "deny"}},
		{"bash status", [2]string{"Bash", `{"command":"git status"}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "allow", "bypass": "allow"}},
		{"bash glob arg", [2]string{"Bash", `{"command":"git add src/a.go"}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "allow", "bypass": "allow"}},
		{"bash chained", [2]string{"Bash", `{"command":"git status; rm -rf x"}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"bash subst", [2]string{"Bash", `{"command":"git log $(id)"}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"bash newline", [2]string{"Bash", "{\"command\":\"git status\\nrm x\"}"}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"bash other", [2]string{"Bash", `{"command":"make test"}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"read inside", in("Read", "file_path", proj+"/a.go"), map[string]string{"normal": "quiet", "edits": "quiet", "away": "allow", "bypass": "allow"}},
		{"read outside", in("Read", "file_path", outside), map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"web fetch", [2]string{"WebFetch", `{"url":"https://example.com"}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"other server's mcp tool", [2]string{"mcp__other__run", `{}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"curia tool", [2]string{"mcp__curia__request_decision", `{}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "allow", "bypass": "allow"}},
		{"imitation of a curia tool", [2]string{"mcp__curia2__request_decision", `{}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "allow"}},
		{"asks the user", [2]string{"AskUserQuestion", `{}`}, map[string]string{"normal": "quiet", "edits": "quiet", "away": "deny", "bypass": "quiet"}},
	}
	for _, mode := range []string{"normal", "edits", "away", "bypass"} {
		for _, c := range cases {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				st := openStore(t)
				env := (&fakeEnv{}).env("%3")
				modeSession(t, st, env, cfg, proj, mode, time.Time{})
				res := pre(t, st, env, cfg, preBody(c.call[0], c.call[1], proj, ""))
				switch want := c.want[mode]; want {
				case "allow":
					wantAllow(t, res, c.name)
				case "deny":
					wantDeny(t, res, c.name, "Curia")
				default:
					wantQuiet(t, res, c.name)
				}
			})
		}
	}
}

// The default mode comes from the config when the session has none.
func TestModeDefaultFromConfig(t *testing.T) {
	_, proj, cfg := denyHome(t)
	cfg.Mode.Default = "edits"
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, "", time.Time{})
	wantAllow(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, "")), "default edits")
}

// An expired mode is absent: the default applies again.
func TestModeExpiredIsAbsent(t *testing.T) {
	_, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3") // the clock is t0
	body := preBody("Bash", `{"command":"make test"}`, proj, "")
	modeSession(t, st, env, cfg, proj, "away", t0.Add(-time.Second))
	wantQuiet(t, pre(t, st, env, cfg, body), "expired away")
	if _, err := st.SetModes(context.Background(), store.ModeScope{All: true}, "away", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantDeny(t, pre(t, st, env, cfg, body), "unexpired away")
}

// The budget stop beats an allow in every mode.
func TestModeBudgetStopWins(t *testing.T) {
	_, proj, cfg := denyHome(t)
	cfg.Cost.BudgetHardStop = true
	for _, mode := range []string{"edits", "away", "bypass"} {
		st := openStore(t)
		env := budgetSession(t, st, 5, 9)
		if _, err := st.SetModes(context.Background(), store.ModeScope{All: true}, mode, time.Time{}); err != nil {
			t.Fatal(err)
		}
		wantDeny(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, "")), mode+" over budget", "budget")
	}
}

// A subagent's call carries the parent's session id and gets the parent's mode.
func TestModeSubagentInherits(t *testing.T) {
	_, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, "away", time.Time{})
	extra := `,"agent_id":"a0f34baa8208d3607","agent_type":"general-purpose"`
	wantAllow(t, pre(t, st, env, cfg, preBody("Bash", `{"command":"git diff"}`, proj, extra)), "subagent git diff")
	wantDeny(t, pre(t, st, env, cfg, preBody("Bash", `{"command":"make"}`, proj, extra)), "subagent make")
}

// Where the session has a worktree, that is the root, not the cwd.
func TestModeEditsRootIsTheWorktree(t *testing.T) {
	_, proj, cfg := denyHome(t)
	sub := filepath.Join(proj, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, sub, "edits", time.Time{})
	wantAllow(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":"a.go"}`, sub, "")), "inside cwd")
	wantQuiet(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, sub, "")), "above cwd")
}

// Hostile paths go through the one shared fail-closed walk.
func TestModeHostilePaths(t *testing.T) {
	home, proj, cfg := denyHome(t)
	if err := os.Symlink(filepath.Join(home, "other"), filepath.Join(proj, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop2", filepath.Join(proj, "loop1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop1", filepath.Join(proj, "loop2")); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		proj + "/out/x.go",                            // a symlink out of the worktree
		proj + "/loop1/x.go",                          // a symlink loop
		proj + "/nope/../../other/x.go",               // dot-dot after a missing directory
		proj + "/" + strings.Repeat("a/", 3000) + "x", // too long
		"~/other/x.go",
	}
	for _, mode := range []string{"edits", "away"} {
		st := openStore(t)
		env := (&fakeEnv{}).env("%3")
		modeSession(t, st, env, cfg, proj, mode, time.Time{})
		for _, p := range bad {
			res := pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(p)+`}`, proj, ""))
			if strings.Contains(string(res.Stdout), `"allow"`) {
				t.Errorf("%s: %q allowed", mode, p)
			}
		}
	}
}

// With the store unreadable no allow is given (the mode cannot be known);
// denials that need no store still hold.
func TestModeStoreUnreadable(t *testing.T) {
	home, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, "bypass", time.Time{})
	_ = st.Close()
	res, err := hook.Run(context.Background(), model.PreToolUse, preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, ""), env, st, cfg)
	if err == nil {
		t.Fatal("Run on a closed store returned no error")
	}
	wantQuiet(t, res, "unreadable store")
	res, _ = hook.Run(context.Background(), model.PreToolUse, preBody("Write", `{"file_path":`+quote(home+"/.config/curia/config.toml")+`}`, proj, ""), env, st, cfg)
	wantDeny(t, res, "protected file, unreadable store")
	if out := hook.PreToolUseNoStore(preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, ""), cfg); out != nil {
		t.Errorf("no store: %q, want no opinion", out)
	}
}

// Every away denial is stored, once per call, with cleaned bounded text.
func TestModeAwayDenialIsStored(t *testing.T) {
	_, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, "away", time.Time{})
	pre(t, st, env, cfg, preBody("Bash", `{"command":"rm -rf x\u001b[31m `+strings.Repeat("y", 5000)+`"}`, proj, ""))
	evs, err := st.PermissionEvents(context.Background(), "A")
	if err != nil || len(evs) != 1 || evs[0].Verdict != "deny" || evs[0].Tool != "Bash" || len(evs[0].Summary) > 200 || strings.ContainsAny(evs[0].Summary, "\x1b") {
		t.Fatalf("events = %+v, %v", evs, err)
	}
}

// The mode read adds no second store round trip: the gate stays far under 20 ms.
func TestPreToolUseWithModeLatency(t *testing.T) {
	_, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, "away", time.Time{})
	body := preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, "")
	latgate.Gate(t, "PreToolUse under away", 20*time.Millisecond, func() time.Duration {
		d := make([]time.Duration, 0, latencyGateRuns)
		for range latencyGateRuns {
			start := time.Now()
			if _, err := hook.Run(context.Background(), model.PreToolUse, body, env, st, cfg); err != nil {
				t.Fatal(err)
			}
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		t.Logf("PreToolUse under away over %d runs: p50 %v, p95 %v", len(d), d[len(d)/2], d[len(d)*95/100])
		return d[len(d)*95/100]
	})
}

func notAllowed(t *testing.T, res hook.Result, what string) {
	t.Helper()
	if strings.Contains(string(res.Stdout), `"allow"`) {
		t.Errorf("%s: allowed: %s", what, res.Stdout)
	}
}

// Finding 1: no allowed edit inside a .git directory, a .git file's gitdir,
// or another auto-executed config at the root.
func TestModeNeverAllowsGitInternals(t *testing.T) {
	_, proj, cfg := denyHome(t)
	for _, d := range []string{".git/hooks", "sub/.git", ".claude", ".curia", "wt/gd"} {
		if err := os.MkdirAll(filepath.Join(proj, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A worktree-style pointer file one level down, to a gitdir without ".git" in its name.
	if err := os.MkdirAll(filepath.Join(proj, "wt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "wt", ".git"), []byte("gitdir: "+filepath.Join(proj, "wt", "gd")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		".git/config", ".git/hooks/pre-commit", "sub/.git/config", ".claude/settings.json", ".curia/setup.sh",
		".gitattributes", ".gitmodules", ".mcp.json", ".envrc", ".vscode/tasks.json", ".idea/x", ".pre-commit-config.yaml", ".githooks/pre-commit", "lefthook.yml", ".lefthook.yml", ".lefthook/x", "wt/.git", "wt/gd/config", ".GIT/config",
	}
	for _, mode := range []string{"edits", "away"} {
		st := openStore(t)
		env := (&fakeEnv{}).env("%3")
		modeSession(t, st, env, cfg, proj, mode, time.Time{})
		for _, p := range paths {
			notAllowed(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(filepath.Join(proj, p))+`}`, proj, "")), mode+" "+p)
		}
	}
}

// Finding 2: an allow-list entry matches by whole words, and every other
// argument is a plain word, inside the root when it looks like a path.
func TestModeAwayCommandWords(t *testing.T) {
	_, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, "away", time.Time{})
	ok := []string{"git status", "git diff", "git diff HEAD~1", "git log", "git add src/a.go", "git add .", "git diff main..feat"}
	bad := []string{
		"git diff --output=/tmp/x", "git diff --no-index /etc/a /etc/b", "git diff --ext-diff", "git log --exec-path", "git -c core.pager=x log",
		"git diff /etc/passwd", "git add ../x", "git add *", "git add \"a b\"", "git statusx", "git commit -m x", "git diff ~/x", "git add $HOME", "git status --porcelain",
	}
	for _, c := range ok {
		wantAllow(t, pre(t, st, env, cfg, preBody("Bash", `{"command":`+quote(c)+`}`, proj, "")), c)
	}
	for _, c := range bad {
		wantDeny(t, pre(t, st, env, cfg, preBody("Bash", `{"command":`+quote(c)+`}`, proj, "")), c)
	}
}

// Finding 3: Glob and Grep only with an explicit path inside the root and a
// pattern that cannot leave it.
func TestModeAwayGlobGrep(t *testing.T) {
	_, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, "away", time.Time{})
	p := quote(proj)
	for _, c := range [][2]string{{"Glob", `{"path":` + p + `,"pattern":"**/*.go"}`}, {"Grep", `{"path":` + p + `,"pattern":"x","glob":"*.go"}`}, {"Grep", `{"path":` + p + `,"pattern":"x"}`}} {
		wantAllow(t, pre(t, st, env, cfg, preBody(c[0], c[1], proj, "")), c[0]+c[1])
	}
	for _, c := range [][2]string{
		{"Glob", `{"pattern":"**/*.go"}`},
		{"Glob", `{"path":` + p + `,"pattern":"/etc/*"}`},
		{"Glob", `{"path":` + p + `,"pattern":"../*"}`},
		{"Glob", `{"path":` + p + `,"pattern":"a/../../*"}`},
		{"Glob", `{"path":` + p + `,"pattern":"~/x"}`},
		{"Glob", `{"path":` + p + `,"pattern":"{../secret,a}"}`},
		{"Glob", `{"path":` + p + `,"pattern":"@(..)/x"}`},
		{"Glob", `{"path":` + p + `,"pattern":"**/../x"}`},
		{"Glob", `{"path":` + p + `,"pattern":"[a-z]*"}`},
		{"Grep", `{"path":` + p + `,"pattern":"x","glob":"../**"}`},
		{"Grep", `{"pattern":"x"}`},
		{"Grep", `{"path":"/etc","pattern":"x"}`},
	} {
		wantDeny(t, pre(t, st, env, cfg, preBody(c[0], c[1], proj, "")), c[0]+c[1])
	}
}

// Finding 4: the root comes from the stored session, never the event's cwd,
// and never the home directory, a parent of it, or "/".
func TestModeRootFromTheStoredSession(t *testing.T) {
	home, proj, cfg := denyHome(t)
	for _, mode := range []string{"edits", "away"} {
		st := openStore(t)
		env := (&fakeEnv{}).env("%3")
		modeSession(t, st, env, cfg, proj, mode, time.Time{})
		// An event whose cwd is elsewhere does not move the root.
		other := filepath.Join(home, "other")
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		notAllowed(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":"x.go"}`, other, "")), mode+" event cwd outside the stored root")
		for _, root := range []string{home, filepath.Dir(home), "/"} {
			if err := st.UpdateSession(context.Background(), "A", func(s model.Session, _ bool) model.Session { s.Worktree = root; return s }); err != nil {
				t.Fatal(err)
			}
			res := pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, ""))
			notAllowed(t, res, mode+" root "+root)
		}
	}
	// No worktree: the stored repo is the root, not the event's cwd.
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	pre(t, st, env, cfg, preBody("Bash", `{"command":"ls"}`, proj, ""))
	if _, err := st.SetModes(context.Background(), store.ModeScope{All: true}, "edits", time.Time{}); err != nil {
		t.Fatal(err)
	}
	notAllowed(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, "")), "repo /src/curia is not the cwd")
}

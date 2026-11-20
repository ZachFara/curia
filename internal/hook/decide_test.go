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

// denyHome sets HOME to a temp dir holding a project directory and returns
// the home, the project and a config read after HOME was set.
func denyHome(t *testing.T) (home, proj string, cfg config.Config) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	proj = filepath.Join(home, "proj")
	for _, d := range []string{"proj", ".config/curia", ".local/share/curia", ".local/state/curia"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home, proj, config.Default()
}

// preBody is a PreToolUse payload for session A; input is the raw tool_input.
func preBody(tool, input, cwd, extra string) []byte {
	b, _ := json.Marshal(cwd)
	return []byte(`{"session_id":"A","transcript_path":"/p/a.jsonl","cwd":` + string(b) + `,"hook_event_name":"PreToolUse","tool_name":` +
		`"` + tool + `","tool_input":` + input + extra + `}`)
}

func pre(t *testing.T, st *store.Store, env hook.Env, cfg config.Config, body []byte) hook.Result {
	t.Helper()
	res, err := hook.Run(context.Background(), model.PreToolUse, body, env, st, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func wantQuiet(t *testing.T, res hook.Result, what string) {
	t.Helper()
	if res.Stdout != nil {
		t.Errorf("%s: printed %q, want nothing", what, res.Stdout)
	}
}

// wantDeny checks the observed deny form (probe P1) and that every want is in the reason.
func wantDeny(t *testing.T, res hook.Result, what string, want ...string) string {
	t.Helper()
	var out struct {
		H struct {
			Event, Decision, Reason string
		} `json:"hookSpecificOutput"`
	}
	if !strings.HasSuffix(string(res.Stdout), "\n") || strings.Count(string(res.Stdout), "\n") != 1 {
		t.Fatalf("%s: stdout %q, want one JSON line", what, res.Stdout)
	}
	raw := map[string]map[string]string{}
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		t.Fatalf("%s: stdout %q is not the deny form: %v", what, res.Stdout, err)
	}
	h := raw["hookSpecificOutput"]
	out.H.Event, out.H.Decision, out.H.Reason = h["hookEventName"], h["permissionDecision"], h["permissionDecisionReason"]
	if out.H.Event != "PreToolUse" || out.H.Decision != "deny" || len(h) != 3 {
		t.Fatalf("%s: stdout %q, want a PreToolUse deny with a reason and nothing else", what, res.Stdout)
	}
	for _, w := range want {
		if !strings.Contains(out.H.Reason, w) {
			t.Errorf("%s: reason %q lacks %q", what, out.H.Reason, w)
		}
	}
	if strings.ContainsAny(out.H.Reason, "\x1b\n\r\t") {
		t.Errorf("%s: reason %q holds control characters", what, out.H.Reason)
	}
	return out.H.Reason
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestProtectedFilesDeniedInEveryTool(t *testing.T) {
	home, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	cfgDir := filepath.Join(home, ".config", "curia")
	denied := []struct{ name, path, cwd string }{
		{"config file", filepath.Join(cfgDir, "config.toml"), proj},
		{"config dir file", filepath.Join(cfgDir, "sub", "x"), proj},
		{"store", filepath.Join(home, ".local/share/curia/curia.db"), proj},
		{"monitor lock", filepath.Join(home, ".local/share/curia/monitor.lock"), proj},
		{"log", filepath.Join(home, ".local/state/curia/curia.log"), proj},
		{"dotdot", proj + "/../.config/curia/config.toml", proj},
		{"dotdot out and in", proj + "/x/../../.config/curia/config.toml", proj},
		{"relative to hostile cwd", "config.toml", cfgDir},
		{"relative dotdot", "../.config/curia/config.toml", proj},
		{"tilde", "~/.config/curia/config.toml", proj},
		{"case", filepath.Join(home, ".Config", "CURIA", "config.toml"), proj},
		{"the directory itself", cfgDir, proj},
	}
	for _, c := range denied {
		for _, tool := range []string{"Edit", "Write", "MultiEdit"} {
			res := pre(t, st, env, cfg, preBody(tool, `{"file_path":`+quote(c.path)+`}`, c.cwd, ""))
			wantDeny(t, res, c.name+" "+tool, "Curia")
		}
		res := pre(t, st, env, cfg, preBody("NotebookEdit", `{"notebook_path":`+quote(c.path)+`}`, c.cwd, ""))
		wantDeny(t, res, c.name+" NotebookEdit")
	}
	quiet := []struct{ name, tool, input, cwd string }{
		{"project file", "Edit", `{"file_path":` + quote(proj+"/a.go") + `}`, proj},
		{"sibling prefix", "Edit", `{"file_path":` + quote(home+"/.config/curiafoo/x") + `}`, proj},
		{"sibling dash", "Write", `{"file_path":` + quote(home+"/.config/curia-old/x") + `}`, proj},
		{"read is not an edit", "Read", `{"file_path":` + quote(cfgDir+"/config.toml") + `}`, proj},
		{"bash is not parsed", "Bash", `{"command":"echo x > ` + cfgDir + `/config.toml"}`, proj},
		{"no path", "Edit", `{}`, proj},
	}
	for _, c := range quiet {
		wantQuiet(t, pre(t, st, env, cfg, preBody(c.tool, c.input, c.cwd, "")), c.name)
	}
}

func TestProtectedFilesFollowSymlinks(t *testing.T) {
	home, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	cfgDir := filepath.Join(home, ".config", "curia")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"dirlink": cfgDir, "filelink": filepath.Join(cfgDir, "config.toml"), "deeper": filepath.Join(home, ".local/share")} {
		if err := os.Symlink(target, filepath.Join(proj, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"dirlink/config.toml", "dirlink/not-yet/new.toml", "filelink", "deeper/curia/curia.db"} {
		wantDeny(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(filepath.Join(proj, p))+`}`, proj, "")), p)
		wantDeny(t, pre(t, st, env, cfg, preBody("Edit", `{"file_path":`+quote(p)+`}`, proj, "")), p+" relative")
	}
	wantQuiet(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/deeper/other.txt")+`}`, proj, "")), "a symlink to a directory beside Curia's")
}

func TestProtectedFilesHostilePayloads(t *testing.T) {
	home, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	cfgFile := filepath.Join(home, ".config/curia/config.toml")
	huge := strings.Repeat("a", 900<<10)
	res := pre(t, st, env, cfg, preBody("Edit", `{"file_path":`+quote(proj+"/"+huge)+`}`, proj, ""))
	wantDeny(t, res, "a 900 KB path is too long to check")
	res = pre(t, st, env, cfg, preBody("Edit", `{"file_path":`+quote(filepath.Join(home, ".config/curia", huge))+`}`, proj, ""))
	if reason := wantDeny(t, res, "a 900 KB path inside"); len(reason) > 600 {
		t.Errorf("reason is %d bytes, want it bounded", len(reason))
	}
	for _, input := range []string{`{"file_path":123}`, `{"file_path":null}`, `{"file_path":{"a":1}}`, `{"file_path":["x"]}`, `{"notebook_path":true}`, `[]`, `"x"`, `null`, `5`} {
		wantQuiet(t, pre(t, st, env, cfg, preBody("Edit", input, proj, "")), "tool_input "+input)
	}
	res = pre(t, st, env, cfg, preBody("Edit", `{"file_path":5,"notebook_path":`+quote(cfgFile)+`}`, proj, ""))
	wantDeny(t, res, "a bad file_path beside a protected notebook_path")
	for _, cwd := range []string{"", "relative/dir", "/nonexistent/dir"} {
		wantQuiet(t, pre(t, st, env, cfg, preBody("Edit", `{"file_path":"a.go"}`, cwd, "")), "cwd "+cwd)
		wantDeny(t, pre(t, st, env, cfg, preBody("Edit", `{"file_path":`+quote(cfgFile)+`}`, cwd, "")), "absolute path, cwd "+cwd)
	}
	res = pre(t, st, env, cfg, preBody("Edit", `{"file_path":`+quote(cfgFile+"\x1b[31m\nforged line")+`}`, proj, ""))
	wantDeny(t, res, "control characters in the path")
}

// The deny does not need the store: a closed one is the broken-store case.
func TestProtectedFilesNeedNoStore(t *testing.T) {
	home, proj, cfg := denyHome(t)
	st := openStore(t)
	_ = st.Close()
	env := (&fakeEnv{}).env("%3")
	body := preBody("Write", `{"file_path":`+quote(filepath.Join(home, ".config/curia/config.toml"))+`}`, proj, "")
	res, err := hook.Run(context.Background(), model.PreToolUse, body, env, st, cfg)
	if err == nil {
		t.Fatal("Run on a closed store returned no error")
	}
	wantDeny(t, res, "closed store")
	if out := hook.PreToolUseNoStore(body, cfg); string(out) != string(res.Stdout) {
		t.Errorf("PreToolUseNoStore = %q, want %q", out, res.Stdout)
	}
	if out := hook.PreToolUseNoStore(preBody("Write", `{"file_path":"a"}`, proj, ""), cfg); out != nil {
		t.Errorf("PreToolUseNoStore for a project file = %q, want nothing", out)
	}
}

// budgetSession makes a task with limit dollars, a session A linked to it
// that has spent spent (a negative spent: no cost known), and returns the env.
func budgetSession(t *testing.T, st *store.Store, limit, spent float64) hook.Env {
	t.Helper()
	tk, err := st.CreateTask(context.Background(), model.Task{
		Title: "t", Repo: "/src/curia", Status: model.TaskBacklog,
		Source: model.SourceLocal, CreatedAt: time.Now(), BudgetUSD: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	env := taskEnv(&fakeEnv{}, tk.ID)
	run(t, st, env, model.SessionStart, payload("A", "/p/a.jsonl", ""))
	if spent >= 0 {
		if _, err = st.Ingest(context.Background(), store.Reading{SessionID: "A", CostUSD: &spent}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

var curiaTools = []string{"search_decisions", "request_decision", "check_decision", "withdraw_decision", "check_messages", "complete_task"}

func TestBudgetHardStop(t *testing.T) {
	_, proj, cfg := denyHome(t)
	cfg.Cost.BudgetHardStop = true
	for _, c := range []struct {
		name         string
		limit, spent float64
		stop         bool
	}{
		{"exactly 100%", 5, 5, true},
		{"over", 5, 7.5, true},
		{"just under", 5, 4.99, false},
		{"unknown cost is never exhausted", 5, -1, false},
		{"no budget", 0, 3, false},
	} {
		st := openStore(t)
		env := budgetSession(t, st, c.limit, c.spent)
		res := pre(t, st, env, cfg, preBody("Bash", `{"command":"ls"}`, proj, ""))
		if !c.stop {
			wantQuiet(t, res, c.name)
			continue
		}
		wantDeny(t, res, c.name, "budget", "$5", "Only the user", "one tool call", "request_decision", "complete_task")
		// The Curia tools still run; imitations do not.
		for _, tool := range curiaTools {
			wantQuiet(t, pre(t, st, env, cfg, preBody("mcp__curia__"+tool, `{}`, proj, "")), tool)
		}
		for _, tool := range []string{
			"mcp__curia2__x", "mcp__curiaX__request_decision", "mcp__curia__request_decision2",
			"mcp__curia__", "mcp__curia__request_decision ", "MCP__curia__request_decision", "mcp__other__curia__request_decision", "Read", "",
		} {
			wantDeny(t, pre(t, st, env, cfg, preBody(tool, `{}`, proj, "")), "tool "+tool)
		}
		// A subagent carries the parent's session id: the stop applies.
		sub := `,"agent_id":"a0f34baa8208d3607","agent_type":"general-purpose"`
		wantDeny(t, pre(t, st, env, cfg, preBody("Bash", `{"command":"touch x"}`, proj, sub)), "subagent")
		wantQuiet(t, pre(t, st, env, cfg, preBody("mcp__curia__complete_task", `{}`, proj, sub)), "subagent's Curia tool")
		// Only PreToolUse is stopped.
		got, err := hook.Run(context.Background(), model.PostToolUse, preBody("Bash", `{}`, proj, ""), env, st, cfg)
		if err != nil || got.Stdout != nil {
			t.Errorf("PostToolUse of a stopped task: %q, %v; want nothing", got.Stdout, err)
		}
	}
}

func TestBudgetStopOffAndNoTask(t *testing.T) {
	_, proj, cfg := denyHome(t)
	st := openStore(t)
	env := budgetSession(t, st, 5, 9)
	wantQuiet(t, pre(t, st, env, cfg, preBody("Bash", `{}`, proj, "")), "budget_hard_stop false")
	cfg.Cost.BudgetHardStop = true
	st2 := openStore(t)
	plain := (&fakeEnv{}).env("%3")
	run(t, st2, plain, model.SessionStart, payload("A", "/p/a.jsonl", ""))
	wantQuiet(t, pre(t, st2, plain, cfg, preBody("Bash", `{}`, proj, "")), "a session with no task")
}

// A store that cannot be read is no opinion for the budget, and says so.
func TestBudgetUndecidedWhenStoreFails(t *testing.T) {
	_, proj, cfg := denyHome(t)
	cfg.Cost.BudgetHardStop = true
	st := openStore(t)
	env := budgetSession(t, st, 5, 9)
	_ = st.Close()
	res, err := hook.Run(context.Background(), model.PreToolUse, preBody("Bash", `{}`, proj, ""), env, st, cfg)
	if err == nil {
		t.Fatal("Run on a closed store returned no error")
	}
	wantQuiet(t, res, "unreadable store")
	if !slices.Equal(res.Undecided, []string{"budget"}) {
		t.Errorf("Undecided = %v, want [budget]", res.Undecided)
	}
}

func TestPreToolUseWithBudgetCheckLatency(t *testing.T) {
	_, proj, cfg := denyHome(t)
	cfg.Cost.BudgetHardStop = true
	st := openStore(t)
	env := budgetSession(t, st, 5, 1)
	body := preBody("Edit", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, "")
	latgate.Gate(t, "PreToolUse with the budget check", 20*time.Millisecond, func() time.Duration {
		d := make([]time.Duration, 0, latencyGateRuns)
		for range latencyGateRuns {
			start := time.Now()
			if _, err := hook.Run(context.Background(), model.PreToolUse, body, env, st, cfg); err != nil {
				t.Fatal(err)
			}
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		t.Logf("PreToolUse with the budget check over %d runs: p50 %v, p95 %v, max %v", len(d), d[len(d)/2], d[len(d)*95/100], d[len(d)-1])
		return d[len(d)*95/100]
	})
}

// A symlink followed by ".." lands where the disk says, not where the text says.
func TestProtectedFilesSymlinkThenDotDot(t *testing.T) {
	home, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	cfgDir := filepath.Join(home, ".config", "curia")
	if err := os.MkdirAll(filepath.Join(cfgDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// proj/link -> ~/.config/curia/sub, so proj/link/.. is ~/.config/curia.
	if err := os.Symlink(filepath.Join(cfgDir, "sub"), filepath.Join(proj, "link")); err != nil {
		t.Fatal(err)
	}
	// ~/.config/curia/back -> proj, so ~/.config/curia/back/.. is home: outside.
	if err := os.Symlink(proj, filepath.Join(cfgDir, "back")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{proj + "/link/../config.toml", "link/../config.toml", "link/../new/dir/x", proj + "/link/../sub/../config.toml"} {
		wantDeny(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(p)+`}`, proj, "")), "bypass via "+p)
	}
	// Lexically inside, really outside: denying is fine; pinned so a change is noticed.
	wantDeny(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(cfgDir+"/back/../x")+`}`, proj, "")), "lexically inside")
	wantQuiet(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/../proj/b.go")+`}`, proj, "")), "dotdot over existing components")
}

// A path that cannot be checked is denied, not waved through: padding past
// the length limit, and ".." after a component that does not exist.
func TestProtectedFilesFailClosed(t *testing.T) {
	home, proj, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	cfgDir := filepath.Join(home, ".config", "curia")
	if err := os.Symlink(cfgDir, filepath.Join(proj, "link")); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("./", 3000)
	for name, p := range map[string]string{
		"dot padding":      proj + "/" + long + "link/config.toml",
		"dotdot padding":   proj + "/" + strings.Repeat("a/../", 1000) + "link/config.toml",
		"relative":         long + "link/config.toml",
		"missing, dotdot":  proj + "/missing/../link/config.toml",
		"missing deeper":   proj + "/missing/x/../../link/config.toml",
		"relative missing": "missing/../link/config.toml",
	} {
		reason := wantDeny(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(p)+`}`, proj, "")), name)
		if len(reason) > 600 {
			t.Errorf("%s: reason is %d bytes", name, len(reason))
		}
	}
	// A long path that goes nowhere near Curia is denied too: no edit has a 4 kB path.
	wantDeny(t, pre(t, st, env, cfg, preBody("Edit", `{"file_path":`+quote(proj+"/"+long+"a.go")+`}`, proj, "")), "long harmless path")
	// ".." in the part that exists is resolved on disk, and is fine.
	wantQuiet(t, pre(t, st, env, cfg, preBody("Edit", `{"file_path":`+quote(proj+"/link/../../proj/new.go")+`}`, proj, "")), "dotdot over existing components")
}

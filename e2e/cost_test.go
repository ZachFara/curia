package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Plan 5 acceptance test: the built binary as the user, tmux, Claude
// Code, Codex and cost-lens would run it, with a real SQLite file, a real git
// repository, private tmux servers, Plan 3's stub `claude`, a stub `codex`
// and a stub `node` (testdata/cost), and stand-ins for ntfy and Linear on
// 127.0.0.1. Usage and cost arrive through curia ingest and curia statusline;
// the badge forecasts the limit, curia usage names who burns it and what to
// pause, a spawn under the forecast gets the cheaper model, a push names the
// cache's stakes, decisions are exported to an Obsidian folder and as an
// Agent Commons entry, Linear issues are imported, a Codex session is
// tracked and spawned, and a review shows the branch's cost from the
// analyzer. Nothing reaches another host, the real home or the default tmux
// server, and the Linear key appears in no output.

// The sessions of the script besides tasksLead and tasksSecond (T-1's and
// T-2's agents, which the claude stub derives from their task ids).
const (
	costCodex = "0199f3a2-7c41-7d10-9b2e-5a8c3e1f4d67" // codex_session_start.json's session, in %0
	costT7    = "00000007-0000-4000-9000-000000000007" // T-7's Codex agent, derived by the codex stub
	// costKey is the Linear API key, given only to step 9's import.
	costKey = "e2e-key"
	// costStatusText is what curia statusline prints for statusline.json.
	costStatusText = "Opus 5.5 · ctx 71% · $4.12 · 5h 62%\n"
)

// costFixtures and costGolden are the test's fixture and golden directories,
// relative to the repository root.
var (
	costFixtures = filepath.Join("testdata", "cost")
	costGolden   = filepath.Join("e2e", "testdata", "cost")
)

// TestCostEndToEnd runs steps 1 to 11 of Plan 5's acceptance script, as
// built.
func TestCostEndToEnd(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	costStatSources(t, root)
	r := newCostRun(t, root, build(t, root))

	r.install(t)         // 1
	r.spawnTwo(t)        // 2
	r.badIngests(t)      // 3
	r.forecast(t)        // 4
	r.statusLine(t)      // 5
	r.cheaperModel(t)    // 6
	r.pushStakes(t)      // 7
	r.exports(t)         // 8
	r.linearImport(t)    // 9
	r.codex(t)           // 10
	r.reviewCost(t)      // 11
	r.pauseSuggestion(t) // the pause suggestion: P, then y
	r.finalState(t)      // 12: silence, no key anywhere, no error
	t.Logf("TestCostEndToEnd took %v", time.Since(r.began).Round(time.Millisecond))
}

// costRun is one run of the script. It reuses Plan 4's run (Plan 3's stub,
// terminal and listings, the stand-in ntfy, T-1's agent's hooks) with this
// test's configuration and stubs.
type costRun struct {
	*reviewsRun
	linear    *costLinear
	began     time.Time
	clock     time.Time // the test's clock for the readings of step 4
	settings  string    // Claude Code's settings.json in the temp CLAUDE_CONFIG_DIR
	codexHome string
	vault     string // the Obsidian folder
	analyzer  string // the cost-lens analyzer the config names
}

func newCostRun(t *testing.T, root, bin string) *costRun {
	t.Helper()
	tmp := t.TempDir()
	tr := &tasksRun{h: newHome(t, bin), root: root, tmp: tmp, repo: filepath.Join(tmp, "src", "api-server")}
	tr.stubDir, tr.panes = filepath.Join(tmp, "bin"), filepath.Join(tmp, "panes")
	r := &costRun{
		reviewsRun: &reviewsRun{tasksRun: tr}, began: time.Now(),
		settings:  filepath.Join(tmp, "claude", "settings.json"),
		codexHome: filepath.Join(tmp, "codex"), vault: filepath.Join(tmp, "vault"),
		analyzer: filepath.Join(tr.h.dir, "home", "cost-lens", "analyzer", "cli.js"),
	}
	for _, d := range []string{tr.repo, tr.stubDir, tr.panes, filepath.Join(tmp, "claude"), r.codexHome, r.vault} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.makeRepo(t)
	tr.makeStub(t, bin)
	for name, target := range map[string]string{"codex": "codex-stub.sh", "node": "node-stub.sh"} {
		if err := os.Symlink(filepath.Join(root, costFixtures, target), filepath.Join(tr.stubDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	tasksWrite(t, filepath.Join(tr.stubDir, "summary.json"), readFile(t, filepath.Join(root, costFixtures, "summary.json")))
	tasksWrite(t, r.analyzer, "// stands in for cost-lens's analyzer/cli.js; the stub node never reads it\n")
	r.ntfy = startReviewsNtfy(t, readFile(t, filepath.Join(root, reviewsReply)))
	r.linear = startCostLinear(t, readFile(t, filepath.Join(root, costFixtures, "linear_page1.json")),
		readFile(t, filepath.Join(root, costFixtures, "linear_page2.json")))
	cfg := strings.NewReplacer("<ntfy_url>", r.ntfy.srv.URL, "<linear_url>", r.linear.srv.URL+"/graphql",
		"<obsidian_dir>", r.vault, "<analyzer>", r.analyzer).Replace(readFile(t, filepath.Join(root, costFixtures, "config.toml.tmpl")))
	tasksWrite(t, filepath.Join(tr.h.dir, "home", ".config", "curia", "config.toml"), cfg)
	tr.srv = startTmux(t, filepath.Join(tmp, "tmuxhome"), tr.paneFile("p0"), tr.paneFile("p1"), tr.paneFile("p2"))
	tr.setEnv(t)
	tr.h.env = append(tr.h.env, "CODEX_HOME="+r.codexHome)
	tr.srv.mustRun(t, "set-environment", "-g", "CODEX_HOME", r.codexHome)
	tr.term = startTerminal(t)
	for _, kv := range tr.h.env {
		if name, _, _ := strings.Cut(kv, "="); name == "CURIA_LINEAR_API_KEY" || name == "CLAUDECODE" || name == "CURIA_NTFY_TOKEN" {
			t.Fatalf("the run's environment holds %s", name)
		}
	}
	return r
}

// Step 1: curia install registers curia statusline in Claude Code's
// settings; a second install finds it and changes nothing; uninstall removes
// it and a third install puts it back. Nothing is written for Codex.
func (r *costRun) install(t *testing.T) {
	t.Helper()
	want := filepath.Join(r.stubDir, "curia") + " statusline"
	if out := r.user(t, "install"); !strings.Contains(out, "status line: registered curia statusline (") {
		t.Errorf("curia install printed:\n%s\nwant the status line registered", out)
	}
	if got := r.statusLineCommand(t); got != want {
		t.Errorf("statusLine.command after curia install = %q, want %q", got, want)
	}
	first := readFile(t, r.settings)
	if out := r.user(t, "install"); !strings.Contains(out, "status line: already registered\n") {
		t.Errorf("a second curia install printed:\n%s\nwant %q", out, "status line: already registered")
	}
	if got := readFile(t, r.settings); got != first {
		t.Errorf("a second curia install changed settings.json:\n%s\nwas:\n%s", got, first)
	}
	if out := r.user(t, "uninstall"); !strings.Contains(out, "status line: removed\n") {
		t.Errorf("curia uninstall printed:\n%s\nwant %q", out, "status line: removed")
	}
	if got := r.statusLineCommand(t); got != "" {
		t.Errorf("statusLine.command after curia uninstall = %q, want none", got)
	}
	if out := r.user(t, "install"); !strings.Contains(out, "status line: registered curia statusline (") {
		t.Errorf("curia install after uninstall printed:\n%s\nwant the status line registered again", out)
	}
	if got := readFile(t, r.settings); got != first {
		t.Errorf("settings.json after uninstall and install:\n%s\nwant it as the first install wrote it:\n%s", got, first)
	}
	// The stub claude writes no .claude.json, so each install finds no MCP
	// entry and asks it to add one; uninstall finds none to remove.
	if got := strings.Count(readFile(t, filepath.Join(r.stubDir, "argv-.txt")), "arg mcp\narg add\n"); got != 3 {
		t.Errorf("the stub claude was asked to add the MCP server %d times, want once per install", got)
	}
	if entries, err := os.ReadDir(r.codexHome); err != nil || len(entries) != 0 {
		t.Errorf("CODEX_HOME after curia install without --codex holds %v (%v), want nothing", entries, err)
	}
}

// statusLineCommand is settings.json's statusLine.command, "" without one.
func (r *costRun) statusLineCommand(t *testing.T) string {
	t.Helper()
	var s struct {
		StatusLine *struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal([]byte(readFile(t, r.settings)), &s); err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	if s.StatusLine == nil {
		return ""
	}
	if s.StatusLine.Type != "command" {
		t.Errorf("statusLine.type = %q, want command", s.StatusLine.Type)
	}
	return s.StatusLine.Command
}

// Step 2: T-1 and T-2 (P3) are added and spawned: two stub agents, two
// sessions.
func (r *costRun) spawnTwo(t *testing.T) {
	t.Helper()
	if got := r.user(t, "task", "add", "Fix the auth refresh", "--repo", r.repo); got != "T-1 added (backlog)\n" {
		t.Errorf("curia task add = %q, want %q", got, "T-1 added (backlog)\n")
	}
	if got := r.user(t, "task", "add", "Tidy the docs", "--repo", r.repo, "--priority", "P3"); got != "T-2 added (backlog)\n" {
		t.Errorf("curia task add --priority P3 = %q, want %q", got, "T-2 added (backlog)\n")
	}
	r.leadPane = r.spawnedPane(t, "T-1", r.user(t, "spawn", "T-1"))
	r.spawnedPane(t, "T-2", r.user(t, "spawn", "T-2"))
	r.wantStates(t, map[string]string{tasksLead: "idle", tasksSecond: "idle"})
	r.wantTasks(t, map[string]string{"T-1": "running", "T-2": "running"})
	r.wantBadge(t, "", "after the two spawns")
}

// Step 3: a document that is not one, an empty one and one of 1 MiB are
// each refused in silence, and nothing is stored.
func (r *costRun) badIngests(t *testing.T) {
	t.Helper()
	for name, stdin := range map[string][]byte{
		"echo garbage":       []byte("garbage\n"),
		"/dev/null":          nil,
		"ingest_garbage.txt": []byte(readFile(t, filepath.Join(r.root, costFixtures, "ingest_garbage.txt"))),
	} {
		if n := len(stdin); name == "ingest_garbage.txt" && n != 1<<20 {
			t.Fatalf("ingest_garbage.txt holds %d bytes, want 1 MiB", n)
		}
		r.ingest(t, name, stdin)
	}
	if got, want := r.h.mustRun(t, "usage"), "no usage has been ingested; see curia install (status line) or cost-lens\n"; got != want {
		t.Errorf("curia usage after the refused documents = %q, want %q", got, want)
	}
}

// ingest runs `curia ingest` with stdin and requires exit 0 and no output.
func (r *costRun) ingest(t *testing.T, name string, stdin []byte, args ...string) {
	t.Helper()
	stdout, stderr, _, err := r.h.run(stdin, "", append([]string{"ingest"}, args...)...)
	if err != nil || stdout != "" || stderr != "" {
		t.Errorf("curia ingest < %s: err %v, stdout %q, stderr %q, want exit 0 and no output", name, err, stdout, stderr)
	}
}

// Step 4: three readings for T-1's session 15, 8 and 1 minutes before the
// test's clock, and three costs for T-2's: the badge forecasts the limit
// before the reset, and curia usage names T-1 as the bigger burn and T-2
// (P3) as the task to pause.
func (r *costRun) forecast(t *testing.T) {
	t.Helper()
	r.clock = time.Now().UTC().Truncate(time.Second)
	tmpl := readFile(t, filepath.Join(r.root, costFixtures, "ingest.json.tmpl"))
	for i, reading := range []struct {
		ago         time.Duration
		pct         string
		cost, cost2 string
	}{
		{15 * time.Minute, "50", "3.00", "0.10"},
		{8 * time.Minute, "56", "3.60", "0.20"},
		{1 * time.Minute, "62", "4.12", "0.30"},
	} {
		at := r.clock.Add(-reading.ago).Format(time.RFC3339)
		doc := strings.NewReplacer("<session_id>", tasksLead, "<at>", at, "<cost_usd>", reading.cost,
			"<five_hour_pct>", reading.pct, "<resets_at>", r.resets().Format(time.RFC3339),
			"<cache_expires_at>", r.clock.Add(12*time.Minute).Format(time.RFC3339)).Replace(tmpl)
		r.ingest(t, fmt.Sprintf("T-1's reading %d", i+1), []byte(doc))
		r.ingest(t, fmt.Sprintf("T-2's reading %d", i+1), []byte(fmt.Sprintf(
			`{"schema_version":1,"session_id":%q,"at":%q,"cost_usd":%s}`, tasksSecond, at, reading.cost2)))
	}
	badge := regexp.MustCompile(`^5h 62% → limit ~4[0-9]m \(resets 1h(49|50)m\)$`)
	if got := r.h.mustRun(t, "status"); !badge.MatchString(got) {
		t.Errorf("curia status = %q, want it to match %s", got, badge)
	}
	got := costPerHour.ReplaceAllStringFunc(normalise(r.h.mustRun(t, "usage", "--json"), r.tmp), func(m string) string {
		v, err := strconv.ParseFloat(costPerHour.FindStringSubmatch(m)[1], 64)
		if err != nil {
			t.Fatalf("usd_per_hour in %q: %v", m, err)
		}
		return fmt.Sprintf(`"usd_per_hour":%.1f`, v)
	})
	if want := r.compactGolden(t, "usage.golden.json"); strings.TrimSuffix(got, "\n") != want {
		t.Errorf("curia usage --json, normalized:\n%s\nwant (usage.golden.json):\n%s", got, want)
	}
}

// resets is when the five-hour window resets: 110 minutes after the clock.
func (r *costRun) resets() time.Time { return r.clock.Add(110 * time.Minute) }

// costPerHour matches one usd_per_hour of curia usage --json.
var costPerHour = regexp.MustCompile(`"usd_per_hour":([0-9.eE+-]+)`)

// Step 5: Claude Code's status-line payload prints Curia's line, and the
// chained form (inside Claude Code) stores it in silence.
func (r *costRun) statusLine(t *testing.T) {
	t.Helper()
	payload := []byte(strings.ReplaceAll(readFile(t, filepath.Join(r.root, costFixtures, "statusline.json")),
		`"<resets_at>"`, strconv.FormatInt(r.resets().Unix(), 10)))
	stdout, stderr, _, err := r.h.run(payload, "", "statusline")
	if err != nil || stdout != costStatusText || stderr != "" {
		t.Errorf("curia statusline < statusline.json: err %v, stdout %q, stderr %q, want exit 0 and %q", err, stdout, stderr, costStatusText)
	}
	stdout, stderr, _, err = r.h.run(payload, "CLAUDECODE=1", "ingest", "--statusline")
	if err != nil || stdout != "" || stderr != "" {
		t.Errorf("CLAUDECODE=1 curia ingest --statusline: err %v, stdout %q, stderr %q, want exit 0 and no output", err, stdout, stderr)
	}
}

// Step 6: under the forecast a spawn starts the agent on the cheaper model
// and says so first; --model wins without a note. The cheaper model is this
// spawn's only (Ruling P5-R7): the argv carries it, the task does not store it
// (tasks.golden.json: T-3 agent_model ""), while --model opus is stored (T-4).
func (r *costRun) cheaperModel(t *testing.T) {
	t.Helper()
	r.user(t, "task", "add", "Add the audit log", "--repo", r.repo)
	out := r.user(t, "spawn", "T-3")
	if first, _, _ := strings.Cut(out, "\n"); !strings.HasPrefix(first, "limit forecast (5h 62%") ||
		!strings.HasSuffix(first, "): starting T-3 on sonnet; pass --model to choose") {
		t.Errorf("curia spawn T-3 printed:\n%s\nwant the first line to be the limit forecast's note naming sonnet", out)
	}
	r.spawnedPane(t, "T-3", out)
	if got, want := r.argv(t, "argv-T-3.txt", "T-3"), r.costGolden(t, "stub_argv_t3.golden.txt"); got != want {
		t.Errorf("argv-T-3.txt, normalized:\n%s\nwant (stub_argv_t3.golden.txt):\n%s", got, want)
	}
	r.user(t, "task", "add", "Rename the flag", "--repo", r.repo)
	out = r.user(t, "spawn", "T-4", "--model", "opus")
	if strings.Contains(out, "limit forecast") {
		t.Errorf("curia spawn T-4 --model opus printed:\n%s\nwant no note", out)
	}
	r.spawnedPane(t, "T-4", out)
	if got := r.argv(t, "argv-T-4.txt", "T-4"); !strings.HasPrefix(got, "argc 3\narg --model\narg opus\narg You are working on Curia task T-4 ") {
		t.Errorf("argv-T-4.txt:\n%s\nwant --model opus before the brief", got)
	}
}

// argv waits for the stub's record name of task id's start and returns it
// with the task's worktree as <worktree>.
func (r *costRun) argv(t *testing.T, name, id string) string {
	t.Helper()
	path := filepath.Join(r.stubDir, name)
	var got string
	for deadline := time.Now().Add(reviewsWait); !strings.Contains(got, "\ntask "+id+"\n"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s after %v: %q, want one start of %s", name, reviewsWait, got, id)
		}
		got = reviewsRead(path)
	}
	return strings.ReplaceAll(got, r.worktreeOf(id), tasksWorktreeToken)
}

// worktreeOf is task id's worktree, symlink-resolved, as git reports it.
func (r *costRun) worktreeOf(id string) string {
	return filepath.Join(filepath.Dir(r.repos[0]), ".curia-worktrees", "api-server-"+id)
}

// Step 7: T-1's agent raises three blocking tickets; each is pushed, and
// D-3's push ends with the stakes of T-1's warm cache.
func (r *costRun) pushStakes(t *testing.T) {
	t.Helper()
	env := append(slices.Clone(r.h.env), "CLAUDE_PROJECT_DIR="+r.worktree, "TMUX_PANE="+r.leadPane)
	r.agent = startClient(t, r.h, "client_agent", r.worktree, readLines(t, filepath.Join(r.root, costFixtures, "client_agent.jsonl")), env)
	r.agent.handshake(t)
	for i := range 3 {
		if resp := r.agent.call(t); !strings.Contains(resp, fmt.Sprintf(`\"id\":\"D-%d\",\"status\":\"open\"`, i+1)) {
			t.Fatalf("request_decision %d = %s, want D-%d open", i+1, resp, i+1)
		}
	}
	pushes := r.ntfy.wait(t, 3)
	r.wantStakesPush(t, pushes[2], "push_d3.golden.json")
	if got := r.h.mustRun(t, "status"); !strings.HasPrefix(got, "D3 5h ") {
		t.Errorf("curia status with D-1 to D-3 open = %q, want D3 and the forecast", got)
	}
}

// costStakes matches the stakes line of a push for T-1's warm cache.
var costStakes = regexp.MustCompile(`\\nanswer within (1[0-2])m to keep cache warm \(rebuild ≈ \$1\.68\)"`)

// wantStakesPush checks one push as reviewsRun.wantPush does, with the
// minutes of its stakes line (10 to 12, by how long the run took since the
// clock) put back as <n>.
func (r *costRun) wantStakesPush(t *testing.T, got reviewsRequest, name string) {
	t.Helper()
	if got.method != http.MethodPost || got.path != "/" || got.contentType != "application/json" || got.auth != "" {
		t.Errorf("push %s: %s %s, Content-Type %q, Authorization %q, want a JSON POST to / with no access token",
			name, got.method, got.path, got.contentType, got.auth)
	}
	tokens := regexp.MustCompile(`"Bearer ([A-Za-z0-9_-]{43})"`).FindAllStringSubmatch(got.body, -1)
	norm := got.body
	for _, m := range tokens {
		if m[1] != tokens[0][1] {
			t.Errorf("push %s: two button tokens %q and %q, want one", name, m[1], tokens[0][1])
		}
		norm = strings.ReplaceAll(norm, m[1], reviewsToken)
	}
	if !costStakes.MatchString(norm) {
		t.Errorf("push %s: message %s\nwant it to end with a line matching %s", name, norm, costStakes)
	}
	norm = costStakes.ReplaceAllLiteralString(norm, `\nanswer within <n>m to keep cache warm (rebuild ≈ $1.68)"`)
	if want := r.compactGolden(t, name); norm != want {
		t.Errorf("push received, normalized:\n%s\nwant (%s):\n%s", norm, name, want)
	}
}

// Step 8: an answer exports its note; a global decision prints a Commons
// entry, a repo one is refused; a note the user wrote is never touched.
func (r *costRun) exports(t *testing.T) {
	t.Helper()
	r.user(t, "decide", "D-1", "--option", "B", "--reason", "one database to run")
	r.waitTyped(t, 1)
	note := "D-1 Which database should the audit log write to.md"
	if got := r.vaultNames(t); !slices.Equal(got, []string{note}) {
		t.Fatalf("the vault after curia decide D-1 holds %q, want %q", got, note)
	}
	if got, want := costNoteDate.ReplaceAllString(readFile(t, filepath.Join(r.vault, note)), "date: <date>"),
		r.costGolden(t, "note_d1.golden.md"); got != want {
		t.Errorf("%s, date normalized:\n%s\nwant (note_d1.golden.md):\n%s", note, got, want)
	}
	r.user(t, "decide", "D-2", "--option", "A", "--reason", "r")
	r.waitTyped(t, 2)
	stdout, stderr, code := r.term.run(t, r.h, r.h.env, "export", "commons", "D-2")
	if want := r.costGolden(t, "commons_d2.golden.json"); code != 0 || stdout != want {
		t.Errorf("curia export commons D-2: exit %d, stdout:\n%s\nwant exit 0 and (commons_d2.golden.json):\n%s", code, stdout, want)
	}
	if want := "this is the agent's and your own text: read it before you publish it\n"; stderr != want {
		t.Errorf("curia export commons D-2: stderr %q, want the one line %q", stderr, want)
	}
	if stderr := r.userFails(t, "export", "commons", "D-1"); !strings.Contains(stderr, "D-1 is not a decided global decision") {
		t.Errorf("curia export commons D-1 (repo scope): stderr %q, want the refusal", stderr)
	}
	if got := r.user(t, "export", "obsidian"); got != "nothing to export\n" {
		t.Errorf("curia export obsidian after both sweeps = %q, want %q", got, "nothing to export\n")
	}
	mine := filepath.Join(r.vault, "D-3 mine.md")
	tasksWrite(t, mine, "# My own note on D-3\n\nNot Curia's: no front matter.\n")
	before := readFile(t, mine)
	// The sweep that curia decide D-3 runs is the first to meet the foreign
	// note: it marks the decision skipped (#139), logs it and prints nothing
	// (only the export command prints the skip line); the file is left be.
	skip := "skipped D-3: a note of yours is at " + mine + " (not Curia's; left alone). Rename or remove it, or run with --retry-skipped\n"
	if got := r.user(t, "decide", "D-3", "--option", "A", "--reason", "r"); strings.Contains(got, "skipped") {
		t.Errorf("curia decide D-3 printed %q, want no skip line (the after-answer sweep logs it)", got)
	}
	r.waitTyped(t, 3)
	if got := readFile(t, mine); got != before {
		t.Errorf("D-3 mine.md after curia decide D-3 = %q, want it unchanged %q", got, before)
	}
	// A later sweep passes over the skipped decision.
	if got := r.user(t, "export", "obsidian", "--all"); got != "nothing to export\n" {
		t.Errorf("curia export obsidian --all after the skip = %q, want %q", got, "nothing to export\n")
	}
	// --retry-skipped tries it anew and reports the foreign note again.
	retry := "exported 0 notes to " + r.vault + " (1 foreign note left alone: D-3)\n" + skip
	if got := r.user(t, "export", "obsidian", "--retry-skipped"); got != retry {
		t.Errorf("curia export obsidian --retry-skipped = %q, want %q", got, retry)
	}
	if got := readFile(t, mine); got != before {
		t.Errorf("D-3 mine.md after --retry-skipped = %q, want it unchanged %q", got, before)
	}
	if got := r.vaultNames(t); !slices.Equal(got, []string{"D-1 Which database should the audit log write to.md", "D-2 Should errors be wrapped with w at every package boundary.md", "D-3 mine.md"}) {
		t.Errorf("the vault holds %q, want D-1's and D-2's notes and the user's D-3 mine.md", got)
	}
}

// costNoteDate matches a note's answer date.
var costNoteDate = regexp.MustCompile(`(?m)^date: \d{4}-\d\d-\d\d$`)

// vaultNames is the Obsidian folder's file names, sorted.
func (r *costRun) vaultNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(r.vault)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// waitTyped waits for the stub of T-1 to have recorded n typed lines.
func (r *costRun) waitTyped(t *testing.T, n int) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(reviewsWait); strings.Count(got, "\n") < n; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("input-T-1.txt after %v: %q, want %d typed lines", reviewsWait, got, n)
		}
		got = reviewsRead(r.inputFile())
	}
	if strings.Count(got, "\n") != n {
		t.Errorf("input-T-1.txt = %q, want %d typed lines", got, n)
	}
}

// Step 9: the Linear import, with the key given to it alone: two pages read,
// two tasks imported and one issue skipped; run again it changes nothing.
func (r *costRun) linearImport(t *testing.T) {
	t.Helper()
	env := envWith(r.h.env, "CURIA_LINEAR_API_KEY="+costKey)
	for run, want := range []string{"imported 2, updated 0, unchanged 0, skipped 1\n", "imported 0, updated 0, unchanged 2, skipped 1\n"} {
		stdout, stderr, code := r.term.run(t, r.h, env, "task", "import", "--linear", "--repo", r.repo)
		if code != 0 || stderr != "" || !strings.HasSuffix(stdout, "\n"+want) || strings.Count(stdout, "\n") != 4 {
			t.Errorf("curia task import --linear, run %d: exit %d, stderr %q, stdout:\n%s\nwant three issue rows and %q", run+1, code, stderr, stdout, want)
		}
		if strings.Contains(stdout+stderr, costKey) {
			t.Errorf("curia task import --linear printed the key")
		}
		r.wantLinearRequests(t, 2*(run+1))
	}
	if got, want := r.normalise(r.h.mustRun(t, "task", "ls", "--status", "all", "--json")), r.costGolden(t, "tasks.golden.json"); got != want {
		t.Errorf("curia task ls --status all --json, normalized:\n%s\nwant (tasks.golden.json):\n%s", got, want)
	}
}

// wantLinearRequests fails unless the stand-in Linear has received n
// requests, each a POST to /graphql with the key alone as its Authorization
// and a body holding one query and no mutation.
func (r *costRun) wantLinearRequests(t *testing.T, n int) {
	t.Helper()
	reqs := r.linear.requests()
	if len(reqs) != n {
		t.Errorf("the stand-in Linear received %d requests, want %d (two pages per import)", len(reqs), n)
	}
	for i, req := range reqs {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal([]byte(req.body), &body); err != nil {
			t.Errorf("request %d: body %q: %v", i+1, req.body, err)
		}
		if req.method != http.MethodPost || req.path != "/graphql" || req.auth != costKey ||
			!strings.HasPrefix(body.Query, "query(") || strings.Contains(req.body, "mutation") {
			t.Errorf("request %d: %s %s, Authorization %q, body %s\nwant a POST to /graphql with the key alone as Authorization and one query, no mutation",
				i+1, req.method, req.path, req.auth, req.body)
		}
	}
}

// Step 10: a Codex session is tracked through `--agent codex` and its Stop
// prints nothing; a task spawned with --agent codex starts the stub codex
// with the brief as its one argument. Installing for Codex waits for probe
// X1.
func (r *costRun) codex(t *testing.T) {
	t.Helper()
	start := strings.ReplaceAll(readFile(t, filepath.Join(r.root, costFixtures, "codex_session_start.json")), repoToken, r.repo)
	r.codexHook(t, "SessionStart", []byte(start))
	stop := hookPayload(t, map[string]any{
		"session_id": costCodex, "cwd": r.repo, "permission_mode": "default", "hook_event_name": "Stop",
		"stop_hook_active": false, "last_assistant_message": "All tests pass.",
	})
	r.codexHook(t, "Stop", stop)
	if s := r.agentOf(t)[costCodex]; s.Agent != "codex" || s.Pane != "%0" || s.State != "idle" {
		t.Errorf("the Codex session in curia ls --json = %+v, want agent codex, idle in %%0", s)
	}
	if got := r.user(t, "task", "add", "Port the client", "--repo", r.repo); got != "T-7 added (backlog)\n" {
		t.Errorf("curia task add = %q, want %q", got, "T-7 added (backlog)\n")
	}
	r.spawnedPane(t, "T-7", r.user(t, "spawn", "T-7", "--agent", "codex"))
	if got, want := r.argv(t, "codex-argv-T-7.txt", "T-7"), r.costGolden(t, "codex_argv_t7.golden.txt"); got != want {
		t.Errorf("codex-argv-T-7.txt, normalized:\n%s\nwant (codex_argv_t7.golden.txt):\n%s", got, want)
	}
	if s := r.agentOf(t)[costT7]; s.Agent != "codex" || s.TaskID != "T-7" {
		t.Errorf("T-7's session in curia ls --json = %+v, want a codex session of T-7", s)
	}
	if exists(filepath.Join(r.stubDir, "argv-T-7.txt")) {
		t.Errorf("the stub claude was started for T-7")
	}
	t.Run("install --codex", func(t *testing.T) {
		t.Skip("probe X1 has not run: Task 10 (curia install --codex) is held until it records where Codex reads its hooks")
	})
}

// codexHook runs `curia hook event --agent codex` in %0 and requires exit 0
// and no output.
func (r *costRun) codexHook(t *testing.T, event string, payload []byte) {
	t.Helper()
	stdout, stderr, _, err := r.h.run(payload, "TMUX_PANE=%0", "hook", event, "--agent", "codex")
	if err != nil || stdout != "" || stderr != "" {
		t.Errorf("curia hook %s --agent codex: err %v, stdout %q, stderr %q, want exit 0 and no output", event, err, stdout, stderr)
	}
}

// costSession is a session as `curia ls --json` shows it, with its agent.
type costSession struct {
	Agent  string `json:"agent"`
	Pane   string `json:"tmux_pane"`
	State  string `json:"state"`
	TaskID string `json:"task_id"`
}

// agentOf is `curia ls --json` by session id.
func (r *costRun) agentOf(t *testing.T) map[string]costSession {
	t.Helper()
	out := r.h.mustRun(t, "ls", "--json")
	var rows []struct {
		ID string `json:"id"`
		costSession
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("curia ls --json: %v\n%s", err, out)
	}
	got := map[string]costSession{}
	for _, row := range rows {
		got[row.ID] = row.costSession
	}
	return got
}

// Step 11: T-1's agent commits and calls complete_task; the review shows
// the sessions' cost and the branch's cost from the analyzer, which the
// stub node was asked for with T-1's worktree.
func (r *costRun) reviewCost(t *testing.T) {
	t.Helper()
	r.agentWorks(t, "audit/log.go", "package audit\n\n// Log records logins and token rotations.\n")
	if resp := r.agent.call(t); !strings.Contains(resp, `\"review\":\"R-1\",\"task\":\"T-1\"`) {
		t.Fatalf("complete_task = %s, want R-1 for T-1", resp)
	}
	r.agentStops(t)
	r.agent.close(t)
	r.ntfy.wait(t, 4)
	got := commitID.ReplaceAllString(costDuration.ReplaceAllString(r.normalise(r.user(t, "review", "T-1", "--json")),
		`"duration_seconds":"<n>"`), `"<commit>"`)
	if want := r.compactGolden(t, "review_t1.golden.json"); strings.TrimSuffix(got, "\n") != want {
		t.Errorf("curia review T-1 --json, normalized:\n%s\nwant (review_t1.golden.json):\n%s", got, want)
	}
	if got, want := readFile(t, filepath.Join(r.stubDir, "node-argv.txt")), "argc 3\narg "+r.analyzer+"\narg --project\narg "+r.worktree+"\n"; got != want {
		t.Errorf("node-argv.txt = %q, want one run: %q", got, want)
	}
}

// costDuration matches the review's duration, which depends on the run.
var costDuration = regexp.MustCompile(`"duration_seconds":\d+`)

// pauseSuggestion: the warning series of step 4 is still in force and T-2
// (P3) runs, so the dashboard's notice line suggests it; T-2's status line
// says its cache is cold with only tokens known, so its CACHE cell reads
// "cold 85k tok". P opens the prompt, and a y that comes as a key of its own
// after the dwell pauses the task.
func (r *costRun) pauseSuggestion(t *testing.T) {
	t.Helper()
	doc := hookPayload(t, map[string]any{
		"session_id": tasksSecond, "model": map[string]any{"display_name": "Opus 5.5"},
		"prompt_cache": map[string]any{
			"expires_at": r.clock.Add(-time.Minute).Unix(), "warm": false, "ttl": "5m", "recache_tokens_if_cold": 85000,
		},
	})
	r.ingest(t, "T-2's cold cache", doc, "--statusline")
	pane := r.view(t, "ui")
	r.screen(t, pane, "tab sessions") // the popup opens on the inbox (Ruling 154)
	r.keys(t, pane, "\t")
	got := r.screen(t, pane, "cold 85k tok")
	if !strings.Contains(got, "P pauses T-2 (P3) api-server") {
		t.Errorf("the dashboard shows:\n%s\nwant the suggestion to pause T-2 (P3) api-server", got)
	}
	if strings.Contains(got, "cold $") {
		t.Errorf("the dashboard shows an invented dollar figure for the cold cache:\n%s", got)
	}
	r.keys(t, pane, "P")
	r.screen(t, pane, `pause T-2 "Tidy the docs"?`)
	// A y before the prompt's dwell is ignored and restarts a longer wait (750 ms): send it again, spaced past that, until the result shows.
	paused := false
	for deadline := time.Now().Add(terminalDeadline); !paused; {
		r.keys(t, pane, "y")
		for until := time.Now().Add(time.Second); time.Now().Before(until) && !paused; time.Sleep(20 * time.Millisecond) {
			got, _ := r.term.srv.run("capture-pane", "-p", "-J", "-t", pane)
			paused = strings.Contains(got, "Paused T-2: curia resume T-2 brings it back (")
		}
		if !paused && time.Now().After(deadline) {
			t.Fatalf("pane %s never confirmed the pause of T-2", pane)
		}
	}
	// A paused task goes back to ready: curia resume T-2 starts it again.
	r.wantTasks(t, map[string]string{"T-2": "ready"})
}

// finalState: nothing typed into a pane but T-1's, exactly four pushes, no
// Linear key in any file the run wrote, and no error in the log.
func (r *costRun) finalState(t *testing.T) {
	t.Helper()
	for _, name := range []string{"p0", "p1", "p2"} {
		if got := readFile(t, r.paneFile(name)); got != "" {
			t.Errorf("pane %s received %q, want nothing", name, got)
		}
	}
	r.ntfy.wait(t, 4) // D-1, D-2, D-3 and R-1
	for _, dir := range []string{r.tmp, r.h.dir, r.term.dir} {
		costNoKey(t, dir)
	}
	checkNoErrors(t, r.h.logPath)
}

// costNoKey fails if any file under dir (the store, the log, every captured
// stdout and stderr, the stubs' records) holds the Linear key.
func costNoKey(t *testing.T, dir string) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(costKey)) {
			t.Errorf("%s holds the Linear key", filepath.Join(dir, path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (r *costRun) costGolden(t *testing.T, name string) string {
	t.Helper()
	return readFile(t, filepath.Join(r.root, costGolden, name))
}

// compactGolden is golden JSON file name in one line, as the binary writes it.
func (r *costRun) compactGolden(t *testing.T, name string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(r.costGolden(t, name))); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return b.String()
}

// costLinear stands in for Linear's GraphQL endpoint: an httptest server on
// 127.0.0.1 that records every request and answers the first page to a
// request without a cursor and the second to one with it.
type costLinear struct {
	srv          *httptest.Server
	page1, page2 string
	mu           sync.Mutex
	reqs         []reviewsRequest
}

func startCostLinear(t *testing.T, page1, page2 string) *costLinear {
	t.Helper()
	l := &costLinear{page1: page1, page2: page2}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(req.Body, 64<<10))
		l.mu.Lock()
		l.reqs = append(l.reqs, reviewsRequest{
			method: req.Method, path: req.URL.Path, contentType: req.Header.Get("Content-Type"),
			auth: req.Header.Get("Authorization"), body: string(b),
		})
		l.mu.Unlock()
		var body struct {
			Variables struct {
				After *string `json:"after"`
			} `json:"variables"`
		}
		page := l.page1
		if json.Unmarshal(b, &body) == nil && body.Variables.After != nil && *body.Variables.After != "" {
			page = l.page2
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, page)
	}))
	t.Cleanup(l.srv.Close)
	if u, err := url.Parse(l.srv.URL); err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatalf("the stand-in Linear is on %s, want 127.0.0.1", l.srv.URL)
	}
	return l
}

func (l *costLinear) requests() []reviewsRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

// costStatSources stats every fixture and golden file of this test and of
// the tests it borrows from, so a changed one cannot reuse a cached pass.
func costStatSources(t *testing.T, root string) {
	t.Helper()
	reviewsStatSources(t, root)
	for _, dir := range []string{costFixtures, costGolden} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			_, err = os.Stat(path)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// costLatencyRuns is how many ingest processes the latency pass times (the
// brief's 400: each run is one store write transaction).
const costLatencyRuns = 400

// TestIngestProcessLatency is step 12: on a store holding ten sessions and
// their cost and usage samples, after one untimed warm-up, costLatencyRuns
// sequential `curia ingest` processes are timed, stdio as files, each
// storing a new reading. Their p95 must be under p95Budget, every run exits
// 0 in silence, and the log holds no error.
func TestIngestProcessLatency(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	h := newHome(t, build(t, root))
	now := time.Now().UTC()
	for i := range 10 {
		start := hookPayload(t, map[string]any{
			"session_id": fmt.Sprintf("00000000-0000-4000-a000-%012d", i+1), "cwd": h.dir,
			"hook_event_name": "SessionStart", "source": "startup",
		})
		if out := h.mustRunStdin(t, start, "hook", "SessionStart"); out != "" {
			t.Fatalf("curia hook SessionStart printed %q", out)
		}
	}
	doc := func(i int) []byte {
		return hookPayload(t, map[string]any{
			"schema_version": 1, "session_id": fmt.Sprintf("00000000-0000-4000-a000-%012d", i%10+1),
			"cost_usd": 1 + float64(i)/100, "cache_expires_at": now.Add(5 * time.Minute).Format(time.RFC3339),
			"rate_limits": map[string]any{"five_hour": map[string]any{
				"used_percentage": 10 + float64(i)/20, "resets_at": now.Add(2 * time.Hour).Format(time.RFC3339),
			}},
		})
	}
	timed := func(i int) time.Duration {
		stdout, stderr, took, err := h.run(doc(i), "", "ingest")
		if err != nil || stdout != "" || stderr != "" {
			t.Fatalf("curia ingest %d: err %v, stdout %q, stderr %q, want exit 0 and no output", i, err, stdout, stderr)
		}
		return took
	}
	timed(0) // warm-up, untimed
	sent := 0
	latencyGate(t, "curia ingest", func() time.Duration {
		var runs []time.Duration
		for len(runs) < costLatencyRuns {
			sent++
			runs = append(runs, timed(sent))
		}
		t.Logf("%-12s %s (load average %s)", "ingest", stats(runs), loadAverage())
		return percentile(runs, 95)
	})
	if out := h.mustRun(t, "usage", "--json"); !strings.Contains(out, fmt.Sprintf(`"window":"five_hour","used_pct":%g`, 10+float64(sent)/20)) {
		t.Errorf("curia usage --json after the pass = %s, want the last reading stored", out)
	}
	checkNoErrors(t, h.logPath)
}

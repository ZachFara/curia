package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/mcp"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// mcpUnexpectedRun is mcpRun under test: no test may run osascript.
func mcpUnexpectedRun(_ context.Context, name string, args ...string) error {
	panic("mcpRun called in a test: " + name + " " + strings.Join(args, " "))
}

func init() {
	mcpRun = mcpUnexpectedRun
}

// mcpEnv gives a test its own HOME (so its own config and log) and
// database, clears every caller variable and returns the database path.
func mcpEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	db := filepath.Join(home, "curia.db")
	t.Setenv("CURIA_DB", db)
	for _, name := range []string{"CURIA_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "TMUX_PANE", "CLAUDE_PROJECT_DIR"} {
		t.Setenv(name, "")
	}
	return db
}

// mcpOpenStore opens the test's database next to the server's.
func mcpOpenStore(t *testing.T, db string) *store.Store {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// mcpPutSession stores a session row.
func mcpPutSession(t *testing.T, st *store.Store, id, pane string, state model.State, updated time.Time, edit ...func(*model.Session)) {
	t.Helper()
	s := model.Session{ID: id, Agent: "claude", Pane: pane, State: state, Repo: "/repo/" + id, StartedAt: updated, UpdatedAt: updated}
	for _, e := range edit {
		e(&s)
	}
	if err := st.UpsertSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// mcpProc is one in-process run of `curia mcp` on real pipes.
type mcpProc struct {
	t      *testing.T
	inW    *os.File
	outR   *os.File
	reader *bufio.Reader
	stderr bytes.Buffer
	done   chan struct{}
	exit   int
	nextID int
	lines  []string
}

func mcpStart(t *testing.T) *mcpProc {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p := &mcpProc{t: t, inW: inW, outR: outR, reader: bufio.NewReaderSize(outR, 1<<20), done: make(chan struct{}), exit: -1}
	prevExit := mcpExit
	mcpExit = func(code int) { p.exit = code }
	rootCmd.SetIn(inR)
	rootCmd.SetOut(outW)
	rootCmd.SetErr(&p.stderr)
	rootCmd.SetArgs([]string{"mcp"})
	go func() {
		defer close(p.done)
		_ = rootCmd.Execute()
		_ = outW.Close()
		_ = inR.Close()
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Error("curia mcp did not end after stdin closed")
		}
		_ = outR.Close()
		mcpExit = prevExit
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	return p
}

// rpc sends one request and returns its response.
func (p *mcpProc) rpc(method string, params any) map[string]any {
	p.t.Helper()
	p.nextID++
	msg, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": p.nextID, "method": method, "params": params})
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.inW.Write(append(msg, '\n')); err != nil {
		p.t.Fatalf("write: %v", err)
	}
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := p.reader.ReadString('\n')
		ch <- res{l, err}
	}()
	var r res
	select {
	case r = <-ch:
	case <-time.After(10 * time.Second):
		p.t.Fatalf("no response to %s", method)
	}
	if r.err != nil {
		p.t.Fatalf("read response to %s: %v (stderr %q)", method, r.err, p.stderr.String())
	}
	p.lines = append(p.lines, r.line)
	var m map[string]any
	if err := json.Unmarshal([]byte(r.line), &m); err != nil {
		p.t.Fatalf("response %q is not JSON: %v", r.line, err)
	}
	if m["id"] != float64(p.nextID) {
		p.t.Fatalf("response id = %v, want %d", m["id"], p.nextID)
	}
	return m
}

// tool calls a tool and returns its decoded result object and isError.
func (p *mcpProc) tool(name string, args any) (map[string]any, bool) {
	p.t.Helper()
	m := p.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	res, ok := m["result"].(map[string]any)
	if !ok {
		p.t.Fatalf("tools/call %s: no result: %v", name, m)
	}
	content := mcpAs[[]any](p.t, res["content"])
	if len(content) != 1 {
		p.t.Fatalf("tools/call %s: content = %v, want one item", name, content)
	}
	text := mcpAs[string](p.t, mcpAs[map[string]any](p.t, content[0])["text"])
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		p.t.Fatalf("tool text %q is not a JSON object: %v", text, err)
	}
	return out, res["isError"] == true
}

// request raises a blocking ticket with question and returns its id.
func (p *mcpProc) request(question string) string {
	p.t.Helper()
	out, isErr := p.tool("request_decision", mcpTicket(question))
	if isErr {
		p.t.Fatalf("request_decision: %v", out)
	}
	return mcpAs[string](p.t, out["id"])
}

// close ends stdin, waits for the command and returns any unread output.
func (p *mcpProc) close() []string {
	p.t.Helper()
	_ = p.inW.Close()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.t.Fatal("curia mcp did not end after stdin closed")
	}
	var rest []string
	for {
		l, err := p.reader.ReadString('\n')
		if l != "" {
			rest = append(rest, l)
		}
		if err != nil {
			break
		}
	}
	return rest
}

// mcpAs asserts that v is a T, failing the test otherwise.
func mcpAs[T any](t *testing.T, v any) T {
	t.Helper()
	x, ok := v.(T)
	if !ok {
		t.Fatalf("%v (%T) is not a %T", v, v, x)
	}
	return x
}

func mcpTicket(question string) map[string]any {
	return map[string]any{
		"question":       question,
		"options":        []map[string]any{{"key": "A", "label": "Postgres"}, {"key": "B", "label": "Redis"}},
		"recommendation": "A",
		"rationale":      "We already run it.",
		"reversibility":  "costly",
		"mode":           "blocking",
	}
}

// mcpCallerOf raises a ticket through a fresh server and returns the
// session id and repo stored on it.
func mcpCallerOf(t *testing.T, st *store.Store, question string) (session, repo string) {
	t.Helper()
	p := mcpStart(t)
	id := p.request(question)
	p.close()
	d, found, err := st.GetDecision(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("GetDecision(%s): %v %v", id, found, err)
	}
	return d.SessionID, d.Repo
}

// mcpHandshake checks initialize and tools/list.
func mcpHandshake(t *testing.T, p *mcpProc) {
	t.Helper()
	initRes := p.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	res := mcpAs[map[string]any](t, initRes["result"])
	if name := mcpAs[map[string]any](t, res["serverInfo"])["name"]; name != "curia" {
		t.Errorf("serverInfo.name = %v", name)
	}
	if res["instructions"] != mcp.Instructions {
		t.Errorf("instructions = %q", res["instructions"])
	}
	list := p.rpc("tools/list", map[string]any{})
	var names []string
	for _, tool := range mcpAs[[]any](t, mcpAs[map[string]any](t, list["result"])["tools"]) {
		names = append(names, mcpAs[string](t, mcpAs[map[string]any](t, tool)["name"]))
	}
	if want := []string{"search_decisions", "request_decision", "check_decision", "withdraw_decision"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

// mcpQuietEnd closes stdin and checks the run wrote only JSON lines to
// stdout, nothing to stderr, and exited 0.
func mcpQuietEnd(t *testing.T, p *mcpProc) {
	t.Helper()
	if rest := p.close(); len(rest) != 0 {
		t.Errorf("unexpected output after the last response: %q", rest)
	}
	for _, l := range p.lines {
		if !strings.HasSuffix(l, "\n") || strings.Count(l, "\n") != 1 || !json.Valid([]byte(l)) {
			t.Errorf("stdout line %q is not one JSON line", l)
		}
	}
	if p.stderr.Len() != 0 || p.exit != -1 {
		t.Errorf("stderr %q, exit %d; want nothing", p.stderr.String(), p.exit)
	}
}

func TestMCPOverPipes(t *testing.T) {
	db := mcpEnv(t)
	t.Setenv("CURIA_SESSION_ID", "S1")
	p := mcpStart(t)
	mcpHandshake(t, p)
	out, isErr := p.tool("request_decision", mcpTicket("Store webhook events in Postgres or Redis?"))
	if isErr || out["id"] != "D-1" || out["status"] != "open" || out["duplicate"] != false {
		t.Errorf("request_decision = %v (isError %v)", out, isErr)
	}
	if out, isErr := p.tool("check_decision", map[string]any{"id": "D-2"}); !isErr || mcpAs[map[string]any](t, out["error"])["code"] != "not_found" {
		t.Errorf("check D-2 = %v (isError %v), want not_found", out, isErr)
	}
	if m := p.rpc("tools/call", map[string]any{"name": "answer_decision", "arguments": map[string]any{"id": "D-1"}}); m["error"] == nil {
		t.Errorf("answer_decision = %v, want a protocol error", m)
	}
	mcpQuietEnd(t, p)
	st := mcpOpenStore(t, db)
	if d, _, _ := st.GetDecision(context.Background(), "D-1"); d.SessionID != "S1" {
		t.Errorf("D-1 session = %q, want S1", d.SessionID)
	}
}

func TestMCPStoreOpenFailureExitsOne(t *testing.T) {
	mcpEnv(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURIA_DB", filepath.Join(blocker, "curia.db"))
	p := mcpStart(t)
	rest := p.close()
	if p.exit != 1 || len(rest) != 0 || p.stderr.Len() != 0 {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and no output", p.exit, rest, p.stderr.String())
	}
	if log := strings.Join(readLog(t), "\n"); !strings.Contains(log, "store") {
		t.Errorf("log = %q, want the store error", log)
	}
}

func TestCallerFromCuriaSessionID(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	now := time.Now()
	mcpPutSession(t, st, "PANE", "%3", model.StateWorking, now)
	mcpPutSession(t, st, "CC", "%4", model.StateWorking, now)
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "CC")
	t.Setenv("CURIA_SESSION_ID", "SPAWNED")
	if s, _ := mcpCallerOf(t, st, "First question here?"); s != "SPAWNED" {
		t.Errorf("session = %q, want SPAWNED (no row)", s)
	}
	mcpPutSession(t, st, "SPAWNED", "%7", model.StateIdle, now)
	if s, r := mcpCallerOf(t, st, "Second question here?"); s != "SPAWNED" || r != "/repo/SPAWNED" {
		t.Errorf("session %q repo %q, want SPAWNED with its row's repo", s, r)
	}
}

func TestCallerFromTmuxPane(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	now := time.Now()
	mcpPutSession(t, st, "ENDED", "%3", model.StateEnded, now)
	mcpPutSession(t, st, "OLDER", "%3", model.StateIdle, now.Add(-2*time.Minute))
	mcpPutSession(t, st, "NEWER", "%3", model.StateWorking, now.Add(-time.Minute))
	mcpPutSession(t, st, "ELSEWHERE", "%4", model.StateWorking, now)
	t.Setenv("TMUX_PANE", "%3")
	if s, r := mcpCallerOf(t, st, "Which queue?"); s != "NEWER" || r != "/repo/NEWER" {
		t.Errorf("session %q repo %q, want NEWER", s, r)
	}
}

func TestCallerFromClaudeCodeSessionID(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	now := time.Now()
	mcpPutSession(t, st, "LIVE", "%3", model.StateIdle, now)
	mcpPutSession(t, st, "GONE", "%4", model.StateEnded, now)
	t.Setenv("TMUX_PANE", "%8") // no session in this pane
	for i, c := range []struct{ env, want string }{{"LIVE", "LIVE"}, {"GONE", ""}, {"NOROW", ""}} {
		t.Setenv("CLAUDE_CODE_SESSION_ID", c.env)
		if s, _ := mcpCallerOf(t, st, "Question number "+strconv.Itoa(i)+"?"); s != c.want {
			t.Errorf("CLAUDE_CODE_SESSION_ID=%s: session %q, want %q", c.env, s, c.want)
		}
	}
}

// mcpResolved is dir with its symlinks resolved (t.TempDir is under a
// symlink on macOS).
func mcpResolved(t *testing.T, dir string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCallerUnknown(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	proj, cwd := t.TempDir(), t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "NOROW")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("CLAUDE_PROJECT_DIR", proj)
	if s, r := mcpCallerOf(t, st, "From the project dir?"); s != "" || r != mcpResolved(t, proj) {
		t.Errorf("session %q repo %q, want unknown in %q", s, r, mcpResolved(t, proj))
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if s, r := mcpCallerOf(t, st, "From the working dir?"); s != "" || r != mcpResolved(t, cwd) {
		t.Errorf("session %q repo %q, want unknown in %q", s, r, mcpResolved(t, cwd))
	}
}

func TestCallerAfterClear(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	now := time.Now()
	mcpPutSession(t, st, "S1", "%3", model.StateEnded, now) // SessionEnd reason clear
	mcpPutSession(t, st, "S2", "%3", model.StateIdle, now.Add(-time.Second))
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "S1")
	if s, _ := mcpCallerOf(t, st, "After clear?"); s != "S2" {
		t.Errorf("session = %q, want S2", s)
	}
}

func TestCallerResolvedOnEveryCall(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	now := time.Now()
	mcpPutSession(t, st, "S1", "%3", model.StateIdle, now)
	t.Setenv("TMUX_PANE", "%3")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "S1")
	p := mcpStart(t)
	first := p.request("Before the rebind?")
	mcpPutSession(t, st, "S1", "%3", model.StateEnded, now.Add(time.Second))
	mcpPutSession(t, st, "S2", "%3", model.StateIdle, now.Add(time.Second))
	second := p.request("After the rebind?")
	p.close()
	for id, want := range map[string]string{first: "S1", second: "S2"} {
		if d, _, _ := st.GetDecision(context.Background(), id); d.SessionID != want {
			t.Errorf("%s session = %q, want %q", id, d.SessionID, want)
		}
	}
}

func TestProjectDirDoesNotIdentifyACaller(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	dir := t.TempDir()
	mcpPutSession(t, st, "S1", "%3", model.StateIdle, time.Now(), func(s *model.Session) { s.Cwd = dir })
	t.Setenv("CLAUDE_PROJECT_DIR", dir)
	if s, _ := mcpCallerOf(t, st, "Who am I?"); s != "" {
		t.Errorf("session = %q, want unknown", s)
	}
}

// TestRepoIsTheDirectoryWithoutGit (was TestRepoFallsBackToGit; Ruling 70):
// the server never runs git, so a caller started in a subdirectory of a
// repository gets that subdirectory, not the top level.
func TestRepoIsTheDirectoryWithoutGit(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	dir := filepath.Join(hermeticGit(t), "repo")
	gitInit(t, dir, "main")
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	t.Setenv("PATH", t.TempDir()) // no git to run
	want := mcpResolved(t, sub)
	if s, r := mcpCallerOf(t, st, "Which repo?"); s != "" || r != want {
		t.Errorf("session %q repo %q, want unknown in %q", s, r, want)
	}
	// A session without a repo falls back too.
	mcpPutSession(t, st, "NOREPO", "%3", model.StateIdle, time.Now(), func(s *model.Session) { s.Repo = "" })
	t.Setenv("TMUX_PANE", "%3")
	if s, r := mcpCallerOf(t, st, "Which repo now?"); s != "NOREPO" || r != want {
		t.Errorf("session %q repo %q, want NOREPO in %q", s, r, want)
	}
}

func TestRepoResolvesSymlinkedDirectory(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", link+"/./")
	if _, r := mcpCallerOf(t, st, "Through a symlink?"); r != mcpResolved(t, target) {
		t.Errorf("repo %q, want %q", r, mcpResolved(t, target))
	}
}

func TestRepoRelativeIsEmpty(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	t.Chdir(t.TempDir())
	if err := os.Mkdir("rel", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "rel")
	if _, r := mcpCallerOf(t, st, "A relative project dir?"); r != "" {
		t.Errorf("repo %q, want none", r)
	}
}

func TestRepoMissingPathIsEmpty(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Join(t.TempDir(), "missing"))
	if _, r := mcpCallerOf(t, st, "A missing project dir?"); r != "" {
		t.Errorf("repo %q, want none", r)
	}
}

// mcpFakeClock sets clock to a time the test moves, safe to read from the
// server's goroutine.
func mcpFakeClock(t *testing.T, start time.Time) *atomic.Int64 {
	t.Helper()
	var now atomic.Int64
	now.Store(start.UnixNano())
	prev := clock
	clock = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	t.Cleanup(func() { clock = prev })
	return &now
}

func TestNotifyRateLimited(t *testing.T) {
	db := mcpEnv(t)
	cfgDir := filepath.Join(filepath.Dir(db), ".config", "curia") // db is in HOME
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("notify_bell = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := useFakeTmux(t)
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	now := mcpFakeClock(t, start)
	st := mcpOpenStore(t, db)
	mcpPutSession(t, st, "S1", "%3", model.StateWorking, start)
	t.Setenv("CURIA_SESSION_ID", "S1")
	p := mcpStart(t)
	p.request("First blocking question?")
	now.Store(start.Add(time.Second).UnixNano())
	p.request("Second blocking question?")
	out, _ := p.tool("request_decision", mcpTicket("Second blocking question?")) // duplicate
	if out["duplicate"] != true {
		t.Fatalf("retry = %v, want a duplicate", out)
	}
	p.close()
	if want := []string{"Bell %3"}; !slices.Equal(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}

	// Past the 30 s window the next blocking ticket rings again.
	now.Store(start.Add(31 * time.Second).UnixNano())
	p = mcpStart(t)
	p.request("Third blocking question?")
	p.close()
	if want := []string{"Bell %3", "Bell %3"}; !slices.Equal(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
}

func TestNotifyOffClaimsNoSlot(t *testing.T) {
	db := mcpEnv(t)
	fake := useFakeTmux(t)
	st := mcpOpenStore(t, db)
	mcpPutSession(t, st, "S1", "%3", model.StateWorking, time.Now())
	t.Setenv("CURIA_SESSION_ID", "S1")
	p := mcpStart(t)
	p.request("Blocking with notifications off?")
	p.close()
	if len(fake.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", fake.Calls)
	}
	won, err := st.ClaimNotifySlot(context.Background(), time.Now(), time.Hour)
	if err != nil || !won {
		t.Errorf("slot already claimed (won %v, err %v): notify with both flags off must not claim it", won, err)
	}
}

// mcpEvents inserts n events at at, in one transaction.
func mcpEvents(t *testing.T, db string, n int, at time.Time) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stamp := at.UTC().Format("2006-01-02T15:04:05.000000000Z")
	for range n {
		if _, err := tx.Exec(`INSERT INTO events (session_id, kind, at) VALUES ('S', 'Stop', ?)`, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// mcpCountEvents counts the events before and after cut.
func mcpCountEvents(t *testing.T, db string, cut time.Time) (older, newer int) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	stamp := cut.UTC().Format("2006-01-02T15:04:05.000000000Z")
	if err := raw.QueryRow(`SELECT count(*) FILTER (WHERE at < ?), count(*) FILTER (WHERE at >= ?) FROM events`,
		stamp, stamp).Scan(&older, &newer); err != nil {
		t.Fatal(err)
	}
	return older, newer
}

func TestMCPStartupPrunesEvents(t *testing.T) {
	db := mcpEnv(t)
	st := mcpOpenStore(t, db)
	now := time.Now()
	for i := range 10 {
		at := now.Add(-time.Minute)
		if i < 5 {
			at = now.Add(-8 * 24 * time.Hour)
		}
		if err := st.AppendEvent(context.Background(), model.Event{SessionID: "S", Kind: model.Stop, At: at}, nil); err != nil {
			t.Fatal(err)
		}
	}
	p := mcpStart(t)
	p.rpc("ping", nil) // answered only once startup is done
	p.close()
	if older, newer := mcpCountEvents(t, db, now.Add(-7*24*time.Hour)); older != 0 || newer != 5 {
		t.Errorf("events: %d old, %d new; want 0 and 5", older, newer)
	}
}

func TestMCPStartupPruneIsBounded(t *testing.T) {
	db := mcpEnv(t)
	mcpOpenStore(t, db) // migrates
	now := time.Now()
	mcpEvents(t, db, 10_050, now.Add(-30*24*time.Hour))
	mcpEvents(t, db, 3, now)
	p := mcpStart(t)
	p.rpc("ping", nil)
	p.close()
	if older, newer := mcpCountEvents(t, db, now.Add(-7*24*time.Hour)); older != 50 || newer != 3 {
		t.Errorf("events: %d old, %d new; want 50 left by 10 batches of 1000, and 3", older, newer)
	}
}

func TestNotifyWithoutPaneLeavesSlot(t *testing.T) {
	db := mcpEnv(t)
	cfgDir := filepath.Join(filepath.Dir(db), ".config", "curia") // db is in HOME
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("notify_bell = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := useFakeTmux(t)
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	now := mcpFakeClock(t, start)
	st := mcpOpenStore(t, db)
	t.Setenv("CURIA_SESSION_ID", "S1") // no row yet: no pane to ring
	p := mcpStart(t)
	p.request("Blocking without a pane?")
	mcpPutSession(t, st, "S1", "%3", model.StateWorking, start)
	now.Store(start.Add(time.Second).UnixNano())
	p.request("Blocking with a pane?")
	p.close()
	if want := []string{"Bell %3"}; !slices.Equal(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q: a ticket that rings nothing must not take the slot", fake.Calls, want)
	}
}

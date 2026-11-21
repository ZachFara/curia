package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// A fake docker executable (a shell script) on PATH records every
// invocation's arguments and prints fixture output. The wrapper under test is
// this very test binary run through a symlink named docker (TestMain).
// The real docker is never run, nothing is pulled, no network is used.

const dockerFakeScript = `#!/bin/sh
PATH=$PATH:/usr/bin:/bin
printf '%s\0' "$@" >> "$FAKE_LOG"; printf '\n' >> "$FAKE_LOG"
case "$1" in
ps) [ -n "$FAKE_DOWN" ] && { echo "Cannot connect to the Docker daemon" >&2; exit 1; }; cat "$FAKE_PS";;
stats) [ -n "$FAKE_HANG" ] && exec sleep 30; [ -n "$FAKE_DOWN" ] && exit 1; cat "$FAKE_STATS";;
info) cat "$FAKE_INFO";;
*) echo "out $*"; echo "err $*" >&2; exit "${FAKE_EXIT:-0}";;
esac
`

type dockerRig struct {
	t        *testing.T
	wrapper  string // the directory holding the docker symlink
	fake     string
	log      string
	home     string
	ps, stat string
	info     string
}

func newDockerRig(t *testing.T, config string) *dockerRig {
	t.Helper()
	d := t.TempDir()
	r := &dockerRig{
		t: t, wrapper: filepath.Join(d, "w", dockerWrapperDirName), fake: filepath.Join(d, "fake"), log: filepath.Join(d, "log"),
		home: filepath.Join(d, "home"), ps: filepath.Join(d, "ps"), stat: filepath.Join(d, "stats"), info: filepath.Join(d, "info"),
	}
	for _, dir := range []string{r.wrapper, r.fake, r.home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rwx := os.FileMode(0o700) // a variable: the linter flags a literal above 0600
	if err = os.Symlink(exe, filepath.Join(r.wrapper, "docker")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(r.fake, "docker"), []byte(dockerFakeScript), rwx); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{r.ps, r.stat, r.info} {
		if err = os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgDir := filepath.Join(r.home, ".config", "curia")
	if err = os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "db_path = " + `"` + filepath.Join(d, "c.db") + `"` + "\nlog_path = " + `"` + filepath.Join(d, "c.log") + `"` + "\n" + config
	if err = os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *dockerRig) write(path, s string) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// run executes the wrapper with PATH path (default: wrapper, fake, wrapper)
// and extra env, returning stdout, stderr and the exit code.
func (r *dockerRig) run(path string, extra []string, args ...string) (string, string, int) {
	r.t.Helper()
	if path == "" {
		path = r.wrapper + ":" + r.fake + ":" + r.wrapper
	}
	cmd := exec.Command(filepath.Join(r.wrapper, "docker"), args...)
	cmd.Env = append([]string{"PATH=" + path, "HOME=" + r.home, "FAKE_LOG=" + r.log, "FAKE_PS=" + r.ps, "FAKE_STATS=" + r.stat, "FAKE_INFO=" + r.info}, extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		r.t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// calls is every recorded docker invocation, in order.
func (r *dockerRig) calls() [][]string {
	b, _ := os.ReadFile(r.log)
	var out [][]string
	for line := range strings.Lines(string(b)) {
		out = append(out, strings.Split(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\x00"), "\x00"))
	}
	return out
}

func psRow(id, session, dir, project string) string {
	q := func(s string) string { return `"` + s + `"` }
	return "[" + strings.Join([]string{q(id), q("n-" + id), q(session), q(""), q(dir), q(project)}, ",") + "]\n"
}

func TestDockerWrapperPassesEverythingElseThroughUnchanged(t *testing.T) {
	r := newDockerRig(t, "[resources]\ncontainer_memory_mb = 512\n")
	cases := [][]string{
		{"logs", "-f", "--tail", "5", "c"},
		{"exec", "-it", "c", "run", "x", "--memory", "1"},
		{"-H", "tcp://h:1", "volume", "ls"},
		{"--context=x", "images"},
		{"compose", "ps"},
		{"frobnicate", "run", "img"},
		{"--", "run", "img"},
		{"run", "--frobnicate", "img"}, // a run option it does not know: untouched
		{"run", "--help"},
		{},
	}
	for _, args := range cases {
		_ = os.Remove(r.log)
		out, errOut, code := r.run("", []string{"FAKE_EXIT=3"}, args...)
		calls := r.calls()
		if len(calls) != 1 || (len(args) > 0 && !slices.Equal(calls[0], args)) || (len(args) == 0 && len(calls[0]) != 1) {
			t.Errorf("%q: docker saw %q, want exactly the same one call", args, calls)
		}
		if code != 3 || !strings.HasPrefix(out, "out ") || !strings.HasPrefix(errOut, "err ") {
			t.Errorf("%q: exit %d stdout %q stderr %q, want docker's own", args, code, out, errOut)
		}
	}
}

func TestDockerWrapperAddsLabelsAndLimitsOnce(t *testing.T) {
	r := newDockerRig(t, "[resources]\ncontainer_memory_mb = 512\ncontainer_cpus = 1.5\n")
	env := []string{"CURIA_SESSION_ID=S1", "CURIA_TASK_ID=T-9"}
	_, _, code := r.run("", env, "-H", "h", "run", "-d", "--name", "x", "alpine", "sh", "-c", "echo --memory 1m --label a=b")
	calls := r.calls()
	got := calls[len(calls)-1]
	want := []string{
		"-H", "h", "run", "-d", "--name", "x", "--label", "curia.session=S1", "--label", "curia.task=T-9", "--memory", "512m", "--cpus", "1.5",
		"alpine", "sh", "-c", "echo --memory 1m --label a=b",
	}
	if code != 0 || !slices.Equal(got, want) {
		t.Errorf("exit %d, docker run saw\n%q\nwant\n%q", code, got, want)
	}
	// create and container run too; a stricter user limit stays the only one.
	_ = os.Remove(r.log)
	r.run("", env, "container", "create", "-m", "256m", "--cpus=0.5", "img")
	calls = r.calls()
	got = calls[len(calls)-1]
	want = []string{"container", "create", "-m", "256m", "--cpus=0.5", "--label", "curia.session=S1", "--label", "curia.task=T-9", "img"}
	if !slices.Equal(got, want) {
		t.Errorf("stricter user limits: saw %q, want %q", got, want)
	}
	// An id that is not plain text is never put in a label.
	_ = os.Remove(r.log)
	r.run("", []string{"CURIA_SESSION_ID=bad id\x1b[2J"}, "run", "img")
	calls = r.calls()
	if strings.Contains(strings.Join(calls[len(calls)-1], " "), "curia.session") {
		t.Errorf("hostile session id reached a label: %q", calls[len(calls)-1])
	}
}

func TestDockerWrapperRefusesAtTheCapAndOnlyThatSession(t *testing.T) {
	r := newDockerRig(t, "")
	r.write(r.ps, psRow("aaaaaaaaaaa1", "S1", "", "")+psRow("aaaaaaaaaaa2", "S1", "", "")+psRow("aaaaaaaaaaa3", "S1", "", "")+psRow("bbbbbbbbbbb1", "S2", "", ""))
	// the sessions must be live for their labels to count
	st, err := store.Open(filepath.Join(filepath.Dir(r.log), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	dockerSeedSessions(t, st, "S1", "S2")
	_ = st.Close()
	_, errOut, code := r.run("", []string{"CURIA_SESSION_ID=S1"}, "run", "-d", "img")
	if code != 1 || strings.Count(errOut, "\n") != 1 || !strings.Contains(errOut, "resources.max_containers_per_session") || !strings.HasPrefix(errOut, "curia: refused:") {
		t.Errorf("exit %d stderr %q, want one refusal line naming the cap", code, errOut)
	}
	for _, c := range r.calls() {
		if c[0] == "run" {
			t.Errorf("docker run was reached after a refusal: %q", c)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(r.log), "c.log")); !strings.Contains(string(b), "docker: refused") {
		t.Errorf("the refusal is not in Curia's log: %q", b)
	}
	if _, _, code = r.run("", []string{"CURIA_SESSION_ID=S2"}, "run", "-d", "img"); code != 0 {
		t.Errorf("another session's run was refused (exit %d)", code)
	}
}

func TestDockerWrapperComposeIsWarnedNotRefused(t *testing.T) {
	r := newDockerRig(t, "[resources]\nmax_containers_total = 1\n")
	r.write(r.ps, psRow("aaaaaaaaaaa1", "S1", "", ""))
	st, _ := store.Open(filepath.Join(filepath.Dir(r.log), "c.db"))
	dockerSeedSessions(t, st, "S1")
	_ = st.Close()
	args := []string{"compose", "-p", "x", "up", "-d"}
	out, errOut, code := r.run("", []string{"CURIA_SESSION_ID=S1"}, args...)
	calls := r.calls()
	if code != 0 || !slices.Equal(calls[len(calls)-1], args) || !strings.HasPrefix(out, "out ") {
		t.Errorf("compose up: exit %d, saw %q: must pass through unchanged", code, calls[len(calls)-1])
	}
	if !strings.Contains(errOut, "WARNING") || !strings.Contains(errOut, "resources.max_containers_total") {
		t.Errorf("no warning on stderr: %q", errOut)
	}
}

func TestDockerWrapperNoRecursionDockerAbsentDaemonDown(t *testing.T) {
	r := newDockerRig(t, "")
	// The wrapper directory twice and only that: no real docker to find.
	_, errOut, code := r.run(r.wrapper+":"+r.wrapper, nil, "ps")
	if code != 127 || !strings.Contains(errOut, "no docker found") {
		t.Errorf("docker absent: exit %d stderr %q", code, errOut)
	}
	// The daemon is down: the count cannot be taken, so run goes ahead (docker
	// reports its own error) with its label.
	_, _, code = r.run("", []string{"FAKE_DOWN=1", "CURIA_SESSION_ID=S1"}, "run", "img")
	calls := r.calls()
	if code != 0 || !slices.Contains(calls[len(calls)-1], "curia.session=S1") {
		t.Errorf("daemon down: exit %d, saw %q", code, calls[len(calls)-1])
	}
}

func dockerSeedSessions(t *testing.T, st *store.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		now := time.Unix(1700000000, 0)
		if err := st.UpsertSession(context.Background(), model.Session{ID: id, Pane: "%" + id, State: model.StateWorking, StartedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- the monitor's sampling ----

func dockerMonitorRig(t *testing.T) (*dockerRig, *monitorEnv, *store.Store) {
	t.Helper()
	r := newDockerRig(t, "")
	env, st := monitorFixture(t)
	t.Setenv("PATH", r.fake+":"+r.wrapper)
	t.Setenv("FAKE_LOG", r.log)
	t.Setenv("FAKE_PS", r.ps)
	t.Setenv("FAKE_STATS", r.stat)
	t.Setenv("FAKE_INFO", r.info)
	old := dockerSampleTimeout
	dockerSampleTimeout = 400 * time.Millisecond
	t.Cleanup(func() { dockerSampleTimeout = old })
	return r, env, st
}

func TestSampleContainersAttributesAndStoresCleaned(t *testing.T) {
	r, env, st := dockerMonitorRig(t)
	ctx := context.Background()
	big := strings.Repeat("N", 1<<20)
	r.write(r.ps, psRow("aaaaaaaaaaa1", "s1", "", "")+
		psRow("bbbbbbbbbbb1", "", "/shared/app", "p")+
		`["ccccccccccc1","\u001b[31mevil\u202e`+big+`","\u001b[2Jx","","",""]`+"\n")
	r.write(r.stat, `{"ID":"aaaaaaaaaaa1","CPUPerc":"2.00%","MemUsage":"100MiB / 7.6GiB"}`+"\n"+`{"ID":"bbbbbbbbbbb1","CPUPerc":"1%","MemUsage":"50MiB / 7.6GiB"}`+"\n")
	r.write(r.info, "8217341952\n")
	if err := env.sampleContainers(ctx); err != nil {
		t.Fatal(err)
	}
	rows, _, err := st.LatestContainers(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows %+v %v", rows, err)
	}
	dockerCheckRows(t, rows)
	vm, ok, _ := st.LatestDockerVM(ctx)
	if !ok || vm.LimitMB != 7836 || vm.UsedMB != 150 {
		t.Errorf("vm = %+v %v, want 150 of 7836 MiB", vm, ok)
	}
}

func dockerCheckRows(t *testing.T, rows []store.ContainerRow) {
	t.Helper()
	by := map[string]store.ContainerRow{}
	for _, c := range rows {
		by[c.ID] = c
	}
	if a := by["aaaaaaaaaaa1"]; a.SessionID != "s1" || a.By != "label" || a.MemMB != 100 || a.CPU != 2 {
		t.Errorf("labeled = %+v", a)
	}
	if b := by["bbbbbbbbbbb1"]; b.SessionID != "" || b.By != "none" {
		t.Errorf("a compose directory outside every session must stay unattributed: %+v", b)
	}
	if c := by["ccccccccccc1"]; strings.ContainsAny(c.Name, "\x1b\u202e") || len([]rune(c.Name)) > 200 || c.By != "none" {
		t.Errorf("hostile container stored uncleaned: %d runes", len([]rune(c.Name)))
	}
}

func TestSampleContainersIsQuietWhenDockerCannotAnswer(t *testing.T) {
	r, env, st := dockerMonitorRig(t)
	ctx := context.Background()
	r.write(r.ps, psRow("aaaaaaaaaaa1", "s1", "", ""))
	t.Setenv("FAKE_DOWN", "1") // the daemon is down
	if err := env.sampleContainers(ctx); err != nil {
		t.Errorf("daemon down: %v, want a quiet skip", err)
	}
	t.Setenv("FAKE_DOWN", "")
	t.Setenv("FAKE_HANG", "1") // docker stats hangs: the deadline ends it
	began := time.Now()
	if err := env.sampleContainers(ctx); err != nil || time.Since(began) > 5*time.Second {
		t.Errorf("hanging stats: %v after %v, want a quiet skip within the deadline", err, time.Since(began))
	}
	if rows, _, _ := st.LatestContainers(ctx); len(rows) != 0 {
		t.Errorf("a failed tick stored %+v", rows)
	}
	t.Setenv("PATH", r.wrapper) // docker absent
	if err := env.sampleContainers(ctx); err != nil {
		t.Errorf("docker absent: %v", err)
	}
}

func TestResourcesShowContainersBesideProcesses(t *testing.T) {
	st := resourcesWorld(t)
	ctx := context.Background()
	at := fleetNow.Add(-5 * time.Second)
	if err := st.ReplaceContainers(ctx, []store.ContainerRow{
		{ID: "aaaaaaaaaaa1", SessionID: "s1", Name: "web", By: "label", CPU: 3, MemMB: 120},
		{ID: "bbbbbbbbbbb1", Name: "stray", By: "none", MemMB: 7},
	}, at); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDockerVM(ctx, at, 127, 7836); err != nil {
		t.Fatal(err)
	}
	b, err := storeSource{st: st, tm: &tmux.Fake{}, cfg: config.Default()}.Budget(ctx)
	if err != nil || b.Res["s1"] != "13% 340M 1c" {
		t.Errorf("cell = %q %v, want the container count after the process figures", b.Res["s1"], err)
	}
	out, err := runRoot(t, "resources")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"web", "label", "unattributed", "VM 0.1/7.7 GB"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDockerLaunchEnvPutsTheWrapperFirstOnce(t *testing.T) {
	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "c.db")
	t.Setenv("PATH", "/usr/bin:/bin")
	env := dockerLaunchEnv(cfg, []string{"A=1", "PATH=/usr/bin:/bin"})
	dir := filepath.Join(filepath.Dir(cfg.DBPath), dockerWrapperDirName)
	var paths []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			paths = append(paths, v)
		}
	}
	if len(paths) != 1 || paths[0] != dir+":/usr/bin:/bin" || !slices.Contains(env, "A=1") {
		t.Fatalf("env = %q", env)
	}
	if target, err := os.Readlink(filepath.Join(dir, "docker")); err != nil || target == "" {
		t.Errorf("no docker symlink: %v", err)
	}
}

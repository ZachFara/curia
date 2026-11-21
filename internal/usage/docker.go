package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ZachFara/curia/internal/sanitize"
)

// F19 Docker: the pure parts. The wrapper's argument reading and the
// attribution and cap rules live here; running docker is cmd/curia's job.
// Everything read from docker (names, labels) is untrusted: cleaned and
// bounded at the parse.

const (
	dockerFieldMax = 120     // runes kept of any text from docker
	maxDockerRows  = 2000    // rows read from one listing
	maxDockerBytes = 8 << 20 // bytes read from one listing
)

// DockerCall is what the wrapper learned from a command line.
type DockerCall struct {
	// Kind is "run" (docker run, create, container run|create), "compose"
	// (compose up|run) or "" (anything else, or anything not understood:
	// passed through untouched).
	Kind string
	// At is, for "run", the index where added flags go: the image, or the
	// "--" before it. Everything the user wrote stays in its order around it.
	At int
	// The user's own limits, read from the options before the image only.
	MemSet, MemOK bool
	Mem           int64 // bytes; 0 is unlimited
	CPUSet, CPUOK bool
	CPUs          float64
	CPUOther      bool // --cpu-quota and friends: --cpus would conflict
}

// flagSet says which flags take a value; any flag not named is unknown and
// makes the whole command line pass through untouched.
type flagSet struct{ valLong, boolLong, valShort, boolShort string }

func (f flagSet) has(list, name string) bool { return strings.Contains(" "+list+" ", " "+name+" ") }

var (
	dockerGlobal = flagSet{
		valLong: "config context host log-level tlscacert tlscert tlskey", boolLong: "debug tls tlsverify",
		valShort: "c H l", boolShort: "D",
	}
	dockerRunFlags = flagSet{
		valLong: "add-host annotation attach blkio-weight blkio-weight-device cap-add cap-drop cgroup-parent cgroupns cidfile " +
			"cpu-period cpu-quota cpu-rt-period cpu-rt-runtime cpu-shares cpus cpuset-cpus cpuset-mems detach-keys device " +
			"device-cgroup-rule device-read-bps device-read-iops device-write-bps device-write-iops dns dns-option dns-search " +
			"domainname entrypoint env env-file expose gpus group-add health-cmd health-interval health-retries " +
			"health-start-interval health-start-period health-timeout hostname io-maxbandwidth io-maxiops ip ip6 ipc isolation " +
			"kernel-memory label label-file link link-local-ip log-driver log-opt mac-address memory memory-reservation " +
			"memory-swap memory-swappiness mount name network network-alias oom-score-adj pid pids-limit platform publish pull " +
			"restart runtime security-opt shm-size stop-signal stop-timeout storage-opt sysctl tmpfs ulimit user userns uts " +
			"volume volume-driver volumes-from workdir",
		boolLong: "detach interactive tty quiet rm init privileged read-only sig-proxy oom-kill-disable no-healthcheck " +
			"publish-all disable-content-trust",
		valShort: "a c e h l m p u v w", boolShort: "d i t q P",
	}
	dockerComposeFlags = flagSet{
		valLong:  "file project-name project-directory profile env-file ansi parallel progress",
		boolLong: "compatibility dry-run all-resources verbose", valShort: "f p", boolShort: "",
	}
)

// scanFlags walks the options from args[i] and returns the index of the first
// argument that is not an option (or of the "--" that ends them), calling
// visit with each option's name and value. ok is false on an option it does
// not know or a missing value.
func scanFlags(args []string, i int, fs flagSet, visit func(name, val string)) (next int, ok bool) {
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--" || a == "-" || !strings.HasPrefix(a, "-"):
			return i, true
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			switch {
			case fs.has(fs.valLong, name) && !hasVal:
				if i+1 >= len(args) {
					return i, false
				}
				val = args[i+1]
				i++
			case fs.has(fs.valLong, name) || fs.has(fs.boolLong, name):
			default:
				return i, false
			}
			visit(name, val)
			i++
		default:
			var done bool
			if i, done = scanCluster(args, i, fs, visit); !done {
				return i, false
			}
		}
	}
	return i, true
}

// scanCluster reads one short option cluster (-itd, -m512m, -e X) at args[i]
// and returns the index after it.
func scanCluster(args []string, i int, fs flagSet, visit func(name, val string)) (int, bool) {
	cl := args[i][1:]
	for j := 0; j < len(cl); j++ {
		c := string(cl[j])
		switch {
		case fs.has(fs.boolShort, c):
		case fs.has(fs.valShort, c):
			if val := cl[j+1:]; val != "" {
				visit(c, strings.TrimPrefix(val, "="))
				return i + 1, true
			}
			if i+1 >= len(args) {
				return i, false
			}
			visit(c, args[i+1])
			return i + 2, true
		default:
			return i, false
		}
	}
	return i + 1, true
}

// ParseDocker reads a docker command line (without argv[0]). It is exact
// about where the global options end and the subcommand begins; anything it
// does not recognize yields Kind "".
func ParseDocker(args []string) DockerCall {
	i, ok := scanFlags(args, 0, dockerGlobal, func(string, string) {})
	if !ok || i >= len(args) || args[i] == "--" {
		return DockerCall{}
	}
	switch sub := args[i]; sub {
	case "run", "create":
		return parseRun(args, i+1)
	case "container":
		if i+1 < len(args) && (args[i+1] == "run" || args[i+1] == "create") {
			return parseRun(args, i+2)
		}
	case "compose":
		j, ok := scanFlags(args, i+1, dockerComposeFlags, func(string, string) {})
		if ok && j < len(args) && (args[j] == "up" || args[j] == "run") {
			return DockerCall{Kind: "compose"}
		}
	}
	return DockerCall{}
}

func parseRun(args []string, from int) DockerCall {
	c := DockerCall{Kind: "run"}
	at, ok := scanFlags(args, from, dockerRunFlags, func(name, val string) {
		switch name {
		case "m", "memory":
			c.Mem, c.MemSet = 0, true
			if b, err := dockerBytes(val); err == nil {
				c.Mem, c.MemOK = b, true
			} else {
				c.MemOK = false
			}
		case "cpus":
			c.CPUs, c.CPUSet = 0, true
			f, err := strconv.ParseFloat(val, 64)
			c.CPUOK = err == nil && f >= 0 && !math.IsNaN(f) && !math.IsInf(f, 0)
			if c.CPUOK {
				c.CPUs = f
			}
		case "cpu-period", "cpu-quota", "cpu-rt-period", "cpu-rt-runtime":
			c.CPUOther = true
		}
	})
	if !ok || at >= len(args) || (args[at] == "--" && at+1 >= len(args)) {
		return DockerCall{}
	}
	c.At = at
	return c
}

// dockerBytes reads docker's size syntax: a number and an optional b, k, m or
// g (with an optional b or ib after the letter), case-insensitive.
func dockerBytes(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSuffix(s, u.suf), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || math.IsNaN(f) || f > 1e15 {
		return 0, fmt.Errorf("not a size")
	}
	return int64(f * float64(mult)), nil
}

// DockerLimits is the flags to add for the configured per-container limits
// (0 = none): a limit is added unless the user's command already sets a
// stricter one, or one we cannot read, or (CPU) a quota flag that conflicts.
func DockerLimits(c DockerCall, memMB int64, cpus float64) []string {
	var add []string
	if memMB > 0 && (!c.MemSet || (c.MemOK && (c.Mem == 0 || c.Mem > memMB<<20))) {
		add = append(add, "--memory", strconv.FormatInt(memMB, 10)+"m")
	}
	if cpus > 0 && !c.CPUOther && (!c.CPUSet || (c.CPUOK && (c.CPUs == 0 || c.CPUs > cpus))) {
		add = append(add, "--cpus", strconv.FormatFloat(cpus, 'f', -1, 64))
	}
	return add
}

// DockerInsert returns args with add inserted before index at; args is not
// changed and no argument is dropped or reordered.
func DockerInsert(args []string, at int, add []string) []string {
	out := make([]string, 0, len(args)+len(add))
	out = append(out, args[:at]...)
	out = append(out, add...)
	return append(out, args[at:]...)
}

// Container is one container as docker listed it, with what Curia decided.
type Container struct {
	ID, Name       string
	SessionLabel   string // curia.session
	TaskLabel      string // curia.task
	ComposeDir     string // com.docker.compose.project.working_dir
	ComposeProject string
	CPU            float64
	MemMB          int64
	SessionID      string // the session it belongs to, "" for none
	By             string // label | compose | none
}

var dockerID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// DockerPSFormat is the `docker ps --format` that ParseDockerPS reads: one
// JSON array per container, so no label can break a row.
const DockerPSFormat = `[{{json .ID}},{{json .Names}},{{json (.Label "curia.session")}},{{json (.Label "curia.task")}},` +
	`{{json (.Label "com.docker.compose.project.working_dir")}},{{json (.Label "com.docker.compose.project")}}]`

// ParseDockerPS reads that output. A row of another shape or with an id that
// is not hex is skipped; text is cleaned and cut; rows and bytes are bounded.
func ParseDockerPS(out string) []Container {
	if len(out) > maxDockerBytes {
		out = out[:maxDockerBytes]
	}
	var cs []Container
	for line := range strings.Lines(out) {
		var f []string
		if json.Unmarshal([]byte(line), &f) != nil || len(f) != 6 || !dockerID.MatchString(f[0]) {
			continue
		}
		c := Container{
			ID: f[0], Name: dockerText(f[1]), SessionLabel: dockerText(f[2]), TaskLabel: dockerText(f[3]),
			ComposeDir: dockerText(f[4]), ComposeProject: dockerText(f[5]),
		}
		if cs = append(cs, c); len(cs) == maxDockerRows {
			break
		}
	}
	return cs
}

func dockerText(s string) string { return sanitize.Line(s, dockerFieldMax) }

// DockerStat is one container's CPU percent and memory in MiB.
type DockerStat struct {
	CPU   float64
	MemMB int64
}

// ParseDockerStats reads `docker stats --no-stream --format '{{json .}}'` by
// container id; a row without a hex id, or unreadable, is skipped, and a
// figure docker could not give ("--") is 0.
func ParseDockerStats(out string) map[string]DockerStat {
	if len(out) > maxDockerBytes {
		out = out[:maxDockerBytes]
	}
	m := map[string]DockerStat{}
	for line := range strings.Lines(out) {
		var r struct{ ID, CPUPerc, MemUsage string }
		if json.Unmarshal([]byte(line), &r) != nil || !dockerID.MatchString(r.ID) {
			continue
		}
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(r.CPUPerc), "%"), 64)
		if cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) {
			cpu = 0
		}
		use, _, _ := strings.Cut(r.MemUsage, "/")
		b, _ := dockerBytes(use)
		if m[r.ID] = (DockerStat{CPU: cpu, MemMB: b >> 20}); len(m) == maxDockerRows {
			break
		}
	}
	return m
}

// SessionRef is what attribution needs to know of a live session.
type SessionRef struct{ ID, TaskID, Worktree, Cwd string }

// AttributeContainers decides which session each container belongs to, never
// guessing: the curia.session label of a known session, else the curia.task
// label of a known task's session, else a Compose working directory under a
// session's worktree or cwd (the deepest one; a tie between sessions is no
// match), else every container of a Compose project one of whose containers
// was attributed that way. The rest is "none".
func AttributeContainers(cs []Container, sessions []SessionRef) []Container {
	known, byTask := map[string]bool{}, map[string]string{}
	for _, s := range sessions {
		known[s.ID] = true
		if s.TaskID != "" {
			byTask[s.TaskID] = s.ID
		}
	}
	out := make([]Container, len(cs))
	project := map[string]string{} // project -> session; "" marks a conflict
	for i, c := range cs {
		c.SessionID, c.By = "", "none"
		switch {
		case known[c.SessionLabel]:
			c.SessionID, c.By = c.SessionLabel, "label"
		case byTask[c.TaskLabel] != "" && c.TaskLabel != "":
			c.SessionID, c.By = byTask[c.TaskLabel], "label"
		default:
			if id := composeOwner(c.ComposeDir, sessions); id != "" {
				c.SessionID, c.By = id, "compose"
				noteProject(project, c.ComposeProject, id)
			}
		}
		out[i] = c
	}
	for i, c := range out {
		if c.By == "none" && c.ComposeProject != "" && project[c.ComposeProject] != "" {
			out[i].SessionID, out[i].By = project[c.ComposeProject], "compose"
		}
	}
	return out
}

// composeOwner is the session whose worktree or cwd is the deepest ancestor of
// dir ("" for none or a tie).
func composeOwner(dir string, sessions []SessionRef) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	dir = filepath.Clean(dir)
	best, bestLen := "", 0
	for _, s := range sessions {
		for _, root := range []string{s.Worktree, s.Cwd} {
			if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) == "/" {
				continue
			}
			root = filepath.Clean(root)
			if dir != root && !strings.HasPrefix(dir, root+"/") {
				continue
			}
			switch {
			case len(root) > bestLen:
				best, bestLen = s.ID, len(root)
			case len(root) == bestLen && best != s.ID:
				best = ""
			}
		}
	}
	return best
}

// DockerOverCap is the one count check, for run and create (refused) and for
// compose (warned): "" while under the caps, else one line naming the cap.
// session "" (a caller no session is known for) is held to the total only.
func DockerOverCap(cs []Container, session string, maxPer, maxTotal int) string {
	per, total := 0, 0
	for _, c := range cs {
		if c.SessionID != "" {
			total++
			if c.SessionID == session {
				per++
			}
		}
	}
	const tail = " Stop one, or ask the user to raise the cap."
	switch {
	case session != "" && per >= maxPer:
		return fmt.Sprintf("curia: refused: this session already runs %d containers (resources.max_containers_per_session).%s", per, tail)
	case total >= maxTotal:
		return fmt.Sprintf("curia: refused: %d containers already run for all sessions (resources.max_containers_total).%s", total, tail)
	}
	return ""
}

// noteProject records that a container of project belongs to session id; a
// project seen under two sessions is marked "" (conflict) and attributes nothing.
func noteProject(project map[string]string, name, id string) {
	if name == "" {
		return
	}
	if p, seen := project[name]; !seen || p == id {
		project[name] = id
	} else {
		project[name] = ""
	}
}

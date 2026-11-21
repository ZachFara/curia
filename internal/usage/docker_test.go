package usage

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDockerKinds(t *testing.T) {
	cases := []struct {
		name string
		args []string
		kind string
		at   int // where flags go, for run
	}{
		{"run", []string{"run", "alpine"}, "run", 1},
		{"create", []string{"create", "alpine", "sh"}, "run", 1},
		{"container run", []string{"container", "run", "alpine"}, "run", 2},
		{"global flags with values", []string{"-H", "tcp://x:1", "--context=foo", "-D", "run", "-d", "alpine"}, "run", 6},
		{"short cluster", []string{"run", "-itd", "--rm", "alpine", "sh"}, "run", 3},
		{"value then bool", []string{"run", "-e", "A=--memory=1m", "-p", "80:80", "--name", "x", "img", "--memory", "1m", "--cpus", "9"}, "run", 7},
		{"attached short value", []string{"run", "-m512m", "-eFOO=1", "img"}, "run", 3},
		{"dashdash before image", []string{"run", "--rm", "--", "img", "--memory=1"}, "run", 2},
		{"cmd flags look like ours", []string{"run", "img", "--label", "x", "-m", "1g", "--cpus=1"}, "run", 1},
		{"compose up", []string{"compose", "-f", "a.yml", "-p", "n", "up", "-d"}, "compose", 0},
		{"compose run is not counted", []string{"compose", "run", "web"}, "", 0},
		{"compose down", []string{"compose", "down"}, "", 0},
		{"compose ps is not counted", []string{"compose", "ps"}, "", 0},
		{"compose unknown flag", []string{"compose", "--weird", "up"}, "", 0},
		{"ps", []string{"ps", "-a"}, "", 0},
		{"exec", []string{"exec", "-it", "c", "run", "x"}, "", 0},
		{"unknown subcommand", []string{"frobnicate", "run"}, "", 0},
		{"unknown global flag", []string{"--frob", "run", "img"}, "", 0},
		{"dashdash first", []string{"--", "run", "img"}, "", 0},
		{"unknown run flag", []string{"run", "--frobnicate", "img"}, "", 0},
		{"run no image", []string{"run", "--help"}, "", 0},
		{"version flag", []string{"-v", "run", "img"}, "", 0},
		{"empty", nil, "", 0},
		{"missing flag value", []string{"run", "-e"}, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseDocker(c.args)
			if got.Kind != c.kind || (c.kind == "run" && got.At != c.at) {
				t.Errorf("ParseDocker(%q) = %q at %d, want %q at %d", c.args, got.Kind, got.At, c.kind, c.at)
			}
		})
	}
}

func TestDockerLimitsKeepStricterUser(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"none set", []string{"run", "img"}, []string{"--memory", "512m", "--cpus", "1.5"}},
		{"stricter memory kept", []string{"run", "-m", "256m", "img"}, []string{"--cpus", "1.5"}},
		{"looser memory replaced", []string{"run", "--memory=2g", "img"}, []string{"--memory", "512m", "--cpus", "1.5"}},
		{"zero is unlimited", []string{"run", "--memory", "0", "img"}, []string{"--memory", "512m", "--cpus", "1.5"}},
		{"unparseable kept", []string{"run", "--memory", "lots", "img"}, []string{"--cpus", "1.5"}},
		{"stricter cpus kept", []string{"run", "--cpus=0.5", "img"}, []string{"--memory", "512m"}},
		{"cpu quota conflicts", []string{"run", "--cpu-quota", "5000", "img"}, []string{"--memory", "512m"}},
		{"in the command is not the user's", []string{"run", "img", "--memory=1m", "--cpus", "0.1"}, []string{"--memory", "512m", "--cpus", "1.5"}},
		{"kb unit", []string{"run", "-m", "100k", "img"}, []string{"--cpus", "1.5"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DockerLimits(ParseDocker(c.args), 512, 1.5); !reflect.DeepEqual(got, c.want) {
				t.Errorf("limits = %q, want %q", got, c.want)
			}
		})
	}
	if got := DockerLimits(ParseDocker([]string{"run", "img"}), 0, 0); len(got) != 0 {
		t.Errorf("no configured limit added %q", got)
	}
}

func TestDockerInsertKeepsEveryArgument(t *testing.T) {
	args := []string{"-H", "h", "run", "-it", "--", "img", "--label", "x"}
	call := ParseDocker(args)
	got := DockerInsert(args, call.At, []string{"--label", "curia.session=S"})
	want := []string{"-H", "h", "run", "-it", "--label", "curia.session=S", "--", "img", "--label", "x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(args) != 8 || args[4] != "--" {
		t.Errorf("input changed: %q", args)
	}
}

func TestDockerOverCap(t *testing.T) {
	cs := []Container{{SessionID: "A"}, {SessionID: "A"}, {SessionID: "A"}, {SessionID: "B"}, {}}
	cases := []struct {
		session  string
		per, tot int
		want     string
	}{
		{"A", 3, 8, "resources.max_containers_per_session"},
		{"B", 3, 8, ""},
		{"B", 3, 4, "resources.max_containers_total"},
		{"", 3, 8, ""}, // no session: only the total applies
		{"A", 4, 8, ""},
	}
	for _, c := range cases {
		msg := DockerOverCap(cs, c.session, c.per, c.tot)
		if (c.want == "") != (msg == "") || !strings.Contains(msg, c.want) {
			t.Errorf("session %q caps %d/%d: %q, want mention of %q", c.session, c.per, c.tot, msg, c.want)
		}
		if msg != "" && (strings.Contains(msg, "\n") || !strings.HasPrefix(msg, "curia: ")) {
			t.Errorf("not one clear line: %q", msg)
		}
	}
}

func TestParseDockerPSCleansHostileFields(t *testing.T) {
	big := strings.Repeat("A", 1<<20)
	out := `["0123456789ab","web","curia.session=S","T-1","","",null]` + "\n" + // wrong shape: skipped
		`["0123456789ab","web","S1","T-1","/w/a","proj"]` + "\n" +
		`["badid","web","","","",""]` + "\n" +
		`["abcdef012345","\u001b[31mred\u202ename` + big + `","S\u001b[2J","","",""]` + "\n" +
		"not json\n"
	got := ParseDockerPS(out)
	if len(got) != 2 {
		t.Fatalf("got %d containers, want 2: %+v", len(got), got)
	}
	if got[0].ID != "0123456789ab" || got[0].SessionLabel != "S1" || got[0].TaskLabel != "T-1" || got[0].ComposeDir != "/w/a" || got[0].ComposeProject != "proj" {
		t.Errorf("first = %+v", got[0])
	}
	h := got[1]
	for _, s := range []string{h.Name, h.SessionLabel} {
		if strings.ContainsAny(s, "\x1b\u202e") || len([]rune(s)) > 120 {
			t.Errorf("hostile text not cleaned or bounded (%d runes): %.40q", len([]rune(s)), s)
		}
	}
}

func TestParseDockerStats(t *testing.T) {
	out := `{"ID":"0123456789ab","Name":"web","CPUPerc":"12.50%","MemUsage":"146.4MiB / 7.653GiB"}` + "\n" +
		`{"ID":"abcdef012345","Name":"db","CPUPerc":"--","MemUsage":"524KiB / 7.653GiB"}` + "\n" +
		`{"ID":"x","Name":"bad"}` + "\n" + "junk\n"
	got := ParseDockerStats(out)
	if len(got) != 2 || got["0123456789ab"].CPU != 12.5 || got["0123456789ab"].MemMB != 146 || got["abcdef012345"].MemMB != 0 || got["abcdef012345"].CPU != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestAttributeContainers(t *testing.T) {
	sessions := []SessionRef{
		{ID: "S1", TaskID: "T-1", Worktree: "/w/one", Cwd: "/w/one/sub"},
		{ID: "S2", Cwd: "/w/two"},
		{ID: "S3", Cwd: "/w"}, // a parent of the others: the most specific wins
	}
	cs := []Container{
		{ID: "a", SessionLabel: "S2"},
		{ID: "b", SessionLabel: "gone", TaskLabel: "T-1"},                          // unknown session, known task
		{ID: "c", SessionLabel: "gone"},                                            // unknown everything
		{ID: "d", ComposeDir: "/w/one/app", ComposeProject: "p"},                   // under S1's worktree
		{ID: "e", ComposeDir: "/elsewhere/app", ComposeProject: "p"},               // same project as d
		{ID: "f", ComposeDir: "/w/twofold", ComposeProject: "q"},                   // prefix of a name, not a parent: S3 only
		{ID: "g", ComposeDir: "/shared", ComposeProject: "r"},                      // outside
		{ID: "h", ComposeDir: "/w/two/x", ComposeProject: "s"},                     // S2 is deeper than S3
		{ID: "i", ComposeDir: "/w/../w/one/./y", ComposeProject: "t"},              // cleaned first
		{ID: "j", SessionLabel: "S2", ComposeDir: "/w/one/z", ComposeProject: "u"}, // label wins over compose
	}
	want := map[string][2]string{
		"a": {"S2", "label"}, "b": {"S1", "label"}, "c": {"", "none"}, "d": {"S1", "compose"}, "e": {"S1", "compose"},
		"f": {"S3", "compose"}, "g": {"", "none"}, "h": {"S2", "compose"}, "i": {"S1", "compose"}, "j": {"S2", "label"},
	}
	for _, c := range AttributeContainers(cs, sessions) {
		if got := [2]string{c.SessionID, c.By}; got != want[c.ID] {
			t.Errorf("%s = %v, want %v", c.ID, got, want[c.ID])
		}
	}
}

func TestComposeRunningNeedsTheSameProjectInTheSameSession(t *testing.T) {
	cs := []Container{{ComposeProject: "my-app", ComposeDir: "/w/My-App", SessionID: "S1"}, {ComposeProject: "theirs", SessionID: "S2"}}
	for _, c := range []struct {
		name    string
		args    []string
		session string
		want    bool
	}{
		{"same -p", []string{"compose", "-p", "my-app", "up"}, "S1", true},
		{"same directory, other -p", []string{"compose", "-p", "other", "up"}, "S1", false},
		{"default name from cwd", []string{"compose", "up"}, "S1", true},
		{"name from the -f directory", []string{"compose", "-f", "/x/Other/c.yml", "up"}, "S1", false},
		{"another session's project", []string{"compose", "-p", "theirs", "up"}, "S1", false},
		{"no session known", []string{"compose", "-p", "my-app", "up"}, "", false},
	} {
		if got := ComposeRunning(cs, ParseDocker(c.args), "", "/w/My-App", c.session); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCapOffAndMemoryThreshold(t *testing.T) {
	cs := []Container{{SessionID: "A"}, {SessionID: "A"}, {SessionID: "A"}}
	if m := DockerOverCap(cs, "A", 0, 0); m != "" {
		t.Errorf("both caps off: %q", m)
	}
	if m := DockerOverCap(cs, "A", 0, 3); !strings.Contains(m, "max_containers_total") {
		t.Errorf("per-session off, total on: %q", m)
	}
	if m := DockerOverCap(cs, "A", 3, 0); !strings.Contains(m, "max_containers_per_session") {
		t.Errorf("total off, per-session on: %q", m)
	}
	if DockerMemoryOver(81, 100, 80) == "" || DockerMemoryOver(80, 100, 80) != "" || DockerMemoryOver(99, 100, 0) != "" || DockerMemoryOver(5, 0, 80) != "" {
		t.Error("memory threshold wrong")
	}
}

package config_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	c := config.Default()
	if !strings.HasSuffix(c.DBPath, ".local/share/curia/curia.db") {
		t.Errorf("DBPath = %q", c.DBPath)
	}
	if !strings.HasSuffix(c.LogPath, ".local/state/curia/curia.log") {
		t.Errorf("LogPath = %q", c.LogPath)
	}
	if c.DefaultContextMax != 200000 {
		t.Errorf("DefaultContextMax = %d", c.DefaultContextMax)
	}
	if c.ContextWarnPct != 60 || c.ContextCritPct != 85 {
		t.Errorf("pct = %d/%d", c.ContextWarnPct, c.ContextCritPct)
	}
	if c.StaleHideAfter != 10*time.Minute {
		t.Errorf("StaleHideAfter = %v", c.StaleHideAfter)
	}
	if c.AssumeExpiry != 24*time.Hour || c.NotifyBell || c.NotifyMacOS {
		t.Errorf("decision defaults = %v/%v/%v", c.AssumeExpiry, c.NotifyBell, c.NotifyMacOS)
	}
	if c.Routing.AutoSend || c.Routing.AutoThreshold != 0.9 || c.Routing.Aliases == nil || len(c.Routing.Aliases) != 0 {
		t.Errorf("routing defaults = %+v", c.Routing)
	}
	assertPlan4Defaults(t, c)
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	got, err := config.Load("/nonexistent/curia/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, config.Default()) {
		t.Errorf("got %+v, want defaults", got)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	p := writeConfig(t, "context_warn_pct = 50\nstale_hide_after = \"3m\"\n\n[context_max]\n\"claude-opus-5-5\" = 1000000\n")
	got, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	def := config.Default()
	if got.ContextWarnPct != 50 {
		t.Errorf("ContextWarnPct = %d", got.ContextWarnPct)
	}
	if got.ContextMax["claude-opus-5-5"] != 1000000 {
		t.Errorf("ContextMax = %v", got.ContextMax)
	}
	if got.StaleHideAfter != 3*time.Minute {
		t.Errorf("StaleHideAfter = %v", got.StaleHideAfter)
	}
	if got.ContextCritPct != def.ContextCritPct || got.DefaultContextMax != def.DefaultContextMax ||
		got.DBPath != def.DBPath || got.LogPath != def.LogPath {
		t.Errorf("other fields not default: %+v", got)
	}
}

func TestLoadExpandsTilde(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	p := writeConfig(t, "db_path = \"~/x/curia.db\"\nlog_path = \"~/x/curia.log\"\n")
	got, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.DBPath != filepath.Join(home, "x/curia.db") || got.LogPath != filepath.Join(home, "x/curia.log") {
		t.Errorf("paths = %q, %q", got.DBPath, got.LogPath)
	}
}

func TestLoadInvalidTOMLReturnsError(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	if _, err := config.Load(writeConfig(t, "context_warn_pct = [")); err == nil {
		t.Fatal("expected error")
	}
}

func TestCuriaDBEnvOverride(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "x.db")
	t.Setenv("CURIA_DB", want)

	got, err := config.Load("/nonexistent/curia/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if got.DBPath != want {
		t.Errorf("DBPath = %q, want %q", got.DBPath, want)
	}

	// The env var wins over the file.
	got, err = config.Load(writeConfig(t, "db_path = \"/from/file.db\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.DBPath != want {
		t.Errorf("with file: DBPath = %q, want %q", got.DBPath, want)
	}
}

func TestDecisionDefaults(t *testing.T) {
	c := config.Default()
	if c.AssumeExpiry != 24*time.Hour {
		t.Errorf("AssumeExpiry = %v", c.AssumeExpiry)
	}
	if c.NotifyBell || c.NotifyMacOS {
		t.Errorf("NotifyBell/NotifyMacOS = %v/%v", c.NotifyBell, c.NotifyMacOS)
	}
}

func TestLoadDecisionOverrides(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	got, err := config.Load(writeConfig(t, "assume_expiry = \"36h\"\nnotify_bell = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.AssumeExpiry != 36*time.Hour || !got.NotifyBell || got.NotifyMacOS {
		t.Errorf("got %v/%v/%v", got.AssumeExpiry, got.NotifyBell, got.NotifyMacOS)
	}
}

func TestNonPositiveAssumeExpiryRejected(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	for _, v := range []string{"0s", "-1h"} {
		_, err := config.Load(writeConfig(t, "assume_expiry = \""+v+"\"\n"))
		if err == nil || !strings.Contains(err.Error(), "assume_expiry") {
			t.Errorf("assume_expiry %s: err = %v", v, err)
		}
	}
}

func TestRoutingDefaults(t *testing.T) {
	r := config.Default().Routing
	if r.AutoSend || r.AutoThreshold != 0.9 {
		t.Errorf("routing = %+v", r)
	}
	if r.Aliases == nil || len(r.Aliases) != 0 {
		t.Errorf("Aliases = %#v, want non-nil and empty", r.Aliases)
	}
}

func TestLoadRoutingOverrides(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	body := "[routing]\nauto_send = true\nauto_threshold = 0.95\n[routing.aliases]\napi = \"api-server\"\n"
	got, err := config.Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	r := got.Routing
	if !r.AutoSend || r.AutoThreshold != 0.95 || !reflect.DeepEqual(r.Aliases, map[string]string{"api": "api-server"}) {
		t.Errorf("routing = %+v", r)
	}
}

func TestLoadRoutingPartialKeepsDefaults(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	got, err := config.Load(writeConfig(t, "[routing]\nauto_send = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Routing.AutoThreshold != 0.9 || got.Routing.Aliases == nil {
		t.Errorf("routing = %+v", got.Routing)
	}
}

func TestBadRoutingRejected(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	long := strings.Repeat("x", 201)
	cases := []struct{ name, body, key string }{
		{"zero", "[routing]\nauto_threshold = 0\n", "routing.auto_threshold"},
		{"high", "[routing]\nauto_threshold = 1.5\n", "routing.auto_threshold"},
		{"negative", "[routing]\nauto_threshold = -1\n", "routing.auto_threshold"},
		{"alias name space", "[routing.aliases]\n\"a b\" = \"x\"\n", "routing.aliases.a b"},
		{"alias name empty", "[routing.aliases]\n\"\" = \"x\"\n", "routing.aliases."},
		{"alias name long", "[routing.aliases]\n" + strings.Repeat("a", 65) + " = \"x\"\n", "routing.aliases." + strings.Repeat("a", 65)},
		{"alias value empty", "[routing.aliases]\napi = \"\"\n", "routing.aliases.api"},
		{"alias value long", "[routing.aliases]\napi = \"" + long + "\"\n", "routing.aliases.api"},
	}
	for _, tc := range cases {
		_, err := config.Load(writeConfig(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s: err = %v, want mention of %q", tc.name, err, tc.key)
		}
	}
}

func TestRoutingBoundariesAccepted(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	body := "[routing]\nauto_threshold = 1\n[routing.aliases]\n" + strings.Repeat("a", 64) + " = \"" + strings.Repeat("x", 200) + "\"\n\"A.b_c-1\" = \"y\"\n"
	if _, err := config.Load(writeConfig(t, body)); err != nil {
		t.Fatal(err)
	}
}

// The snippet's keys default to a, A, N and m; a file can change each one, and
// an empty value (binds no key) is kept, not replaced by the default.
func TestTmuxKeys(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	c := config.Default()
	if c.KeyDashboard != "a" || c.KeyDecisions != "A" || c.KeyNext != "N" || c.KeyMessage != "m" {
		t.Errorf("default keys = %q %q %q %q, want a A N m", c.KeyDashboard, c.KeyDecisions, c.KeyNext, c.KeyMessage)
	}
	got, err := config.Load(writeConfig(t, "key_dashboard = \"F5\"\nkey_decisions = \"\"\nkey_message = \"C-m\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyDashboard != "F5" || got.KeyDecisions != "" || got.KeyNext != "N" || got.KeyMessage != "C-m" {
		t.Errorf("loaded keys = %q %q %q %q, want F5, empty, N, C-m", got.KeyDashboard, got.KeyDecisions, got.KeyNext, got.KeyMessage)
	}
}

// editor defaults to empty (no editor); a set value is split on single
// spaces into an argument vector, with no shell and no quoting.
func TestEditorOption(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	if c := config.Default(); c.Editor != "" || c.EditorArgs() != nil {
		t.Errorf("default editor = %q, args %q, want none", c.Editor, c.EditorArgs())
	}
	for value, want := range map[string][]string{
		"/usr/bin/vim -f":  {"/usr/bin/vim", "-f"},
		"code --wait":      {"code", "--wait"},
		"nano":             {"nano"},
		"/opt/é/ed -p ➜ x": {"/opt/é/ed", "-p", "➜", "x"},
	} {
		got, err := config.Load(writeConfig(t, "editor = \""+value+"\"\n"))
		if err != nil {
			t.Errorf("editor %q: %v", value, err)
			continue
		}
		if got.Editor != value || !reflect.DeepEqual(got.EditorArgs(), want) {
			t.Errorf("editor %q: %q, args %q, want args %q", value, got.Editor, got.EditorArgs(), want)
		}
	}
}

func TestBadEditorRejected(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	for name, value := range map[string]string{
		"leading space":  ` vim`,
		"trailing space": `vim `,
		"double space":   `vim  -f`,
		"only spaces":    `   `,
		"tab":            `vim\t-f`,
		"escape":         `vim\u001b[31m`,
		"newline":        `vim\n-f`,
		"bidi override":  `vim\u202e`,
		"relative path":  `./bin/vim`,
		"too long":       "vim " + strings.Repeat("x", 1024),
	} {
		_, err := config.Load(writeConfig(t, "editor = \""+value+"\"\n"))
		if err == nil || !strings.Contains(err.Error(), "editor") {
			t.Errorf("%s: err = %v, want one naming editor", name, err)
		}
	}
}

func assertPlan4Defaults(t *testing.T, c config.Config) {
	t.Helper()
	if c.Review.Approve != "merge" || c.Review.Remote != "origin" || c.Review.DiffTool != "git" {
		t.Errorf("review defaults = %+v", c.Review)
	}
	if c.Ntfy.URL != "" || c.Ntfy.Quiet != "" || c.Ntfy.Devices == nil || len(c.Ntfy.Devices) != 0 {
		t.Errorf("ntfy defaults = %+v", c.Ntfy)
	}
	if c.Serve.Listen != "" || c.Serve.PublicURL != "" {
		t.Errorf("serve defaults = %+v", c.Serve)
	}
	if c.Restore.MaxAge != 24*time.Hour {
		t.Errorf("restore max_age = %v", c.Restore.MaxAge)
	}
}

func loadErr(t *testing.T, body string) error {
	t.Helper()
	t.Setenv("CURIA_DB", "")
	_, err := config.Load(writeConfig(t, body))
	return err
}

func wantErrNaming(t *testing.T, body, key string) {
	t.Helper()
	err := loadErr(t, body)
	if err == nil {
		t.Errorf("%q: accepted, want an error naming %s", body, key)
	} else if !strings.Contains(err.Error(), key) {
		t.Errorf("%q: error %q does not name %s", body, err, key)
	}
}

func wantAccepted(t *testing.T, body string) {
	t.Helper()
	if err := loadErr(t, body); err != nil {
		t.Errorf("%q: %v", body, err)
	}
}

func TestPlan4Defaults(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	assertPlan4Defaults(t, config.Default())
}

func TestLoadPlan4Overrides(t *testing.T) {
	t.Setenv("CURIA_DB", "")
	c, err := config.Load(writeConfig(t, `
[review]
approve = "pr"
remote = "up.stream_1"
diff_tool = "delta"
[ntfy]
url = "https://ntfy.sh/"
quiet = "22:00-07:00"
[ntfy.devices]
phone = "curia-k3j9x2m4"
tablet = "t_2"
[serve]
listen = "127.0.0.1:9000"
public_url = "https://box.tail1234.ts.net/"
[restore]
max_age = "2h"
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Review != (config.Review{Approve: "pr", Remote: "up.stream_1", DiffTool: "delta"}) {
		t.Errorf("review = %+v", c.Review)
	}
	if c.Ntfy.URL != "https://ntfy.sh" || c.Ntfy.Quiet != "22:00-07:00" ||
		!reflect.DeepEqual(c.Ntfy.Devices, map[string]string{"phone": "curia-k3j9x2m4", "tablet": "t_2"}) {
		t.Errorf("ntfy = %+v", c.Ntfy)
	}
	if c.Serve.Listen != "127.0.0.1:9000" || c.Serve.PublicURL != "https://box.tail1234.ts.net" {
		t.Errorf("serve = %+v", c.Serve)
	}
	if c.Restore.MaxAge != 2*time.Hour {
		t.Errorf("restore = %+v", c.Restore)
	}
}

func TestBadReviewConfigRejected(t *testing.T) {
	wantErrNaming(t, "[review]\napprove = \"squash\"\n", "review.approve")
	wantErrNaming(t, "[review]\ndiff_tool = \"vimdiff; rm\"\n", "review.diff_tool")
	for _, r := range []string{"-x", "a b", "", strings.Repeat("a", 65), "a/b"} {
		wantErrNaming(t, "[review]\nremote = \""+r+"\"\n", "review.remote")
	}
	for _, ok := range []string{"merge", "pr"} {
		wantAccepted(t, "[review]\napprove = \""+ok+"\"\n")
	}
	for _, ok := range []string{"git", "delta", "lazygit", "nvim"} {
		wantAccepted(t, "[review]\ndiff_tool = \""+ok+"\"\n")
	}
	wantAccepted(t, "[review]\nremote = \""+strings.Repeat("a", 64)+"\"\n")
}

func TestBadNtfyConfigRejected(t *testing.T) {
	urls := []string{
		"ftp://x", "https://", "https://u:p@ntfy.sh", "https://ntfy.sh?x=1", "https://ntfy.sh#f",
		"http://example.com", "https://" + strings.Repeat("a", 193), "not a url", "https://ntfy.sh/%zz",
	}
	for _, u := range urls {
		wantErrNaming(t, "[ntfy]\nurl = \""+u+"\"\n", "ntfy.url")
	}
	for _, q := range []string{"22:00", "25:00-07:00", "07:00-07:00", "a-b", "22:60-07:00", "7:00-08:00"} {
		wantErrNaming(t, "[ntfy]\nquiet = \""+q+"\"\n", "ntfy.quiet")
	}
	var many strings.Builder
	many.WriteString("[ntfy.devices]\n")
	for i := range 9 {
		many.WriteString("d" + string(rune('a'+i)) + " = \"t\"\n")
	}
	wantErrNaming(t, many.String(), "ntfy.devices")
	wantErrNaming(t, "[ntfy.devices]\n\"Phone\" = \"t\"\n", "ntfy.devices.Phone")
	wantErrNaming(t, "[ntfy.devices]\n\"a b\" = \"t\"\n", "ntfy.devices.a b")
	for _, topic := range []string{"", "a/b", strings.Repeat("a", 65)} {
		wantErrNaming(t, "[ntfy.devices]\nphone = \""+topic+"\"\n", "ntfy.devices.phone")
	}
	wantAccepted(t, "[ntfy]\nquiet = \"00:00-23:59\"\n")
}

// TestNtfyTopicRule is ntfy 2.38.0's rule as probe N1 observed it: 1 to 64
// letters, digits, "-" and "_"; anything else is 400 (code 40009).
func TestNtfyTopicRule(t *testing.T) {
	for _, topic := range []string{"a", "-", "_", "curia-k3j9x2m4", "A_b-9", strings.Repeat("z", 64)} {
		wantAccepted(t, "[ntfy.devices]\nphone = \""+topic+"\"\n")
	}
	for _, topic := range []string{"", " ", "a b", "a/b", "a.b", "a+b", "é", "ａ", "a\\nb", strings.Repeat("z", 65)} {
		wantErrNaming(t, "[ntfy.devices]\nphone = \""+topic+"\"\n", "ntfy.devices.phone")
	}
}

// TestNtfyURLHasNoPath: Curia publishes JSON to the server's base URL, and
// ntfy stores a JSON body POSTed to /<topic> as plain text, so the URL may
// not carry a path (probe N1).
func TestNtfyURLHasNoPath(t *testing.T) {
	for _, u := range []string{"https://ntfy.sh", "https://ntfy.sh/", "http://127.0.0.1:8080/"} {
		wantAccepted(t, "[ntfy]\nurl = \""+u+"\"\n")
	}
	for _, u := range []string{"https://ntfy.sh/curia-k3j9x2m4", "https://ntfy.sh/a/", "https://ntfy.sh//", "http://127.0.0.1:8080/x"} {
		wantErrNaming(t, "[ntfy]\nurl = \""+u+"\"\n", "ntfy.url")
	}
	// serve.public_url is not the ntfy server and keeps its own rule.
	wantAccepted(t, "[serve]\npublic_url = \"https://box.tail1234.ts.net/curia\"\n")
}

func TestPlainHTTPOnlyInsideTailnet(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1:8080", "http://localhost:8080", "http://100.101.102.103",
		"http://box.tail1234.ts.net", "http://[::1]:8080", "http://[fd7a:115c:a1e0::1]",
	} {
		wantAccepted(t, "[ntfy]\nurl = \""+u+"\"\n")
		wantAccepted(t, "[serve]\npublic_url = \""+u+"\"\n")
	}
	for _, u := range []string{"http://10.0.0.1", "http://100.128.0.1", "http://ts.net.evil.com", "http://example.com"} {
		wantErrNaming(t, "[ntfy]\nurl = \""+u+"\"\n", "ntfy.url")
		wantErrNaming(t, "[serve]\npublic_url = \""+u+"\"\n", "serve.public_url")
	}
}

func TestBadServePublicURLRejected(t *testing.T) {
	for _, u := range []string{"ftp://x", "https://", "https://u@x.io", "https://x.io?a=1", "https://x.io#f", "https://" + strings.Repeat("a", 193)} {
		wantErrNaming(t, "[serve]\npublic_url = \""+u+"\"\n", "serve.public_url")
	}
}

func TestBadServeConfigRejected(t *testing.T) {
	for _, l := range []string{"8787", "0.0.0.0:8787", "192.168.1.5:8787", "example.com:80", "100.101.102.103:0", ":8787", "127.0.0.1:65536", "127.0.0.1:x", "localhost:8787"} {
		wantErrNaming(t, "[serve]\nlisten = \""+l+"\"\n", "serve.listen")
	}
	for _, l := range []string{"127.0.0.1:8787", "100.101.102.103:8787", "[fd7a:115c:a1e0::1]:8787", "[::1]:65535"} {
		wantAccepted(t, "[serve]\nlisten = \""+l+"\"\n")
	}
}

func TestNonPositiveRestoreMaxAgeRejected(t *testing.T) {
	wantErrNaming(t, "[restore]\nmax_age = \"0s\"\n", "restore.max_age")
	wantErrNaming(t, "[restore]\nmax_age = \"-1h\"\n", "restore.max_age")
}

func TestParseQuiet(t *testing.T) {
	for _, c := range []struct {
		in         string
		start, end int
		ok         bool
	}{
		{"22:00-07:00", 1320, 420, true},
		{"00:00-23:59", 0, 1439, true},
		{"", 0, 0, false},
		{"22:00", 0, 0, false},
		{"24:00-07:00", 0, 0, false},
		{"07:00-07:00", 0, 0, false},
		{"a-b", 0, 0, false},
	} {
		s, e, ok := config.ParseQuiet(c.in)
		if s != c.start || e != c.end || ok != c.ok {
			t.Errorf("ParseQuiet(%q) = %d, %d, %v", c.in, s, e, ok)
		}
	}
}

func TestTailscaleAddr(t *testing.T) {
	for a, want := range map[string]bool{
		"100.64.0.0": true, "100.127.255.255": true, "fd7a:115c:a1e0::1": true,
		"100.128.0.0": false, "100.63.255.255": false, "10.0.0.1": false, "127.0.0.1": false, "::1": false,
		"fd7a:115c:a1e1::1": false,
	} {
		if got := config.TailscaleAddr(netip.MustParseAddr(a)); got != want {
			t.Errorf("TailscaleAddr(%s) = %v, want %v", a, got, want)
		}
	}
}

func TestNtfyEnabledAndDeviceNames(t *testing.T) {
	if (config.Ntfy{URL: "https://ntfy.sh"}).Enabled() {
		t.Error("URL without devices is enabled")
	}
	if (config.Ntfy{Devices: map[string]string{"a": "t"}}).Enabled() {
		t.Error("devices without URL is enabled")
	}
	n := config.Ntfy{URL: "https://ntfy.sh", Devices: map[string]string{"b": "t", "a": "u", "c": "v"}}
	if !n.Enabled() {
		t.Error("URL and a device is not enabled")
	}
	if got := n.DeviceNames(); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("DeviceNames = %v", got)
	}
}

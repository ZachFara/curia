package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// serverRunner is a TmuxRunner on the private test server that tm runs.
func serverRunner(tm func(args ...string) (string, error)) TmuxRunner {
	return func(_ context.Context, args ...string) (string, error) {
		out, err := tm(args...)
		if err != nil {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
		}
		return out, nil
	}
}

// mustTmux runs a tmux command on the test server, failing on an error.
func mustTmux(t *testing.T, tm func(args ...string) (string, error), args ...string) string {
	t.Helper()
	out, err := tm(args...)
	if err != nil {
		t.Fatalf("tmux %q: %v: %s", args, err, out)
	}
	return strings.TrimSuffix(out, "\n")
}

// focusHooks is show-hooks' list of pane-focus-in entries on the test server.
func focusHooks(t *testing.T, tm func(args ...string) (string, error)) string {
	t.Helper()
	return mustTmux(t, tm, "show-hooks", "-g", "pane-focus-in")
}

// ourHook is how show-hooks prints the snippet's focus hook for bin.
func ourHook(index int, bin string) string {
	return fmt.Sprintf(`pane-focus-in[%d] run-shell -b "%s seen #{pane_id}"`, index, shellQuote(bin))
}

// TestSnippetKeepsOtherFocusHooks: sourcing the snippet (twice, then for a
// moved binary) sets Curia's one focus hook entry and keeps the entries
// before and after it; an entry of the user's at Curia's index is kept and
// the snippet then sets none.
func TestSnippetKeepsOtherFocusHooks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[0]", "display-message first")
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[100]", "display-message last")

	sourceSnippet(t, tm, dir, testBin, 2)
	want := "pane-focus-in[0] display-message first\n" + ourHook(77, testBin) + "\npane-focus-in[100] display-message last"
	if got := focusHooks(t, tm); got != want {
		t.Errorf("hooks after sourcing twice =\n%s\nwant\n%s", got, want)
	}

	moved := "/usr/local/bin/curia"
	sourceSnippet(t, tm, dir, moved, 1)
	want = "pane-focus-in[0] display-message first\n" + ourHook(77, moved) + "\npane-focus-in[100] display-message last"
	if got := focusHooks(t, tm); got != want {
		t.Errorf("hooks after sourcing for a moved binary =\n%s\nwant\n%s", got, want)
	}

	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[77]", "display-message mine")
	sourceSnippet(t, tm, dir, testBin, 1)
	want = "pane-focus-in[0] display-message first\npane-focus-in[77] display-message mine\npane-focus-in[100] display-message last"
	if got := focusHooks(t, tm); got != want {
		t.Errorf("hooks with the user's own entry at Curia's index =\n%s\nwant\n%s", got, want)
	}
}

// unloadFixture is a server that loaded the snippet for bin over a themed
// status-right, with foreign focus hook entries before and after Curia's and
// one more from an earlier snippet's binary. It returns the runner, the
// configuration path and the window formats from before the snippet.
func unloadFixture(ctx context.Context, t *testing.T, bin string) (tm func(...string) (string, error), conf string, formats [2]string) {
	t.Helper()
	dir := t.TempDir()
	tm = realTmux(ctx, t, dir)
	for i, opt := range []string{"window-status-format", "window-status-current-format"} {
		formats[i] = mustTmux(t, tm, "show", "-gv", opt)
	}
	mustTmux(t, tm, "set", "-g", "status-right", "#[fg=blue]before")
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[0]", "display-message first")
	mustTmux(t, tm, "set-hook", "-g", "client-attached[0]", "display-message mine")
	sourceSnippet(t, tm, dir, bin, 1)
	mustTmux(t, tm, "set", "-ag", "status-right", " #[fg=red]after")
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[5]", `run-shell -b "/old/curia seen #{pane_id}"`)
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[100]", "display-message last")
	conf = filepath.Join(dir, ".tmux.conf")
	if err := RemoveTmux(conf); err != nil {
		t.Fatal(err)
	}
	return tm, conf, formats
}

// TestUnloadTmuxUndoesOnlyTheSnippet: Curia's hook entries (this binary's and
// an earlier one's), its badge in the middle of a themed status-right, its
// marker and its keys go; everything else stays byte for byte, and a second
// pass changes nothing. The binary's path needs shell quoting.
func TestUnloadTmuxUndoesOnlyTheSnippet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bin := "/opt/my $tools/it's/curia"
	tm, conf, formats := unloadFixture(ctx, t, bin)
	mustTmux(t, tm, "bind", "a", "display-message", "mine") // the user rebound Curia's a

	lines := UnloadTmux(ctx, serverRunner(tm), conf, bin, defaultKeys, defaultKeys)

	b := shellQuote(bin)
	socket := mustTmux(t, tm, "display-message", "-p", "#{socket_path}")
	want := []string{
		"tmux: " + conf + " no longer loads Curia; undoing what it loaded in the running server " + socket + ":",
		"  pane-focus-in[0]: not Curia's, kept: display-message first",
		`  pane-focus-in[5]: removed Curia's hook: run-shell -b "/old/curia seen #{pane_id}"`,
		`  pane-focus-in[77]: removed Curia's hook: run-shell -b "'/opt/my \$tools/it'\\''s/curia' seen #{pane_id}"`,
		"  pane-focus-in[100]: not Curia's, kept: display-message last",
		"  client-attached[0]: not Curia's, kept: display-message mine",
		`  client-attached[78]: removed Curia's hook: run-shell -b "'/opt/my \$tools/it'\\''s/curia' monitor start --quiet"`,
		"  status-right: removed Curia's badge #[default] #{@curia_badge}#(" + b + " status); the rest is as it was",
		"  window-status-format: removed Curia's marker #{E:@curia_marker_format}; the rest is as it was",
		"  window-status-current-format: removed Curia's marker #{E:@curia_marker_format}; the rest is as it was",
		"  @curia_marker_format: unset",
		"  prefix a: not Curia's binding, left as it is: display-message mine",
		"  prefix A: unbound (it ran Curia's decisions command)",
		"  prefix N: unbound (it ran Curia's next command)",
		"  prefix m: Curia's message binding replaced by tmux's own select-pane -m",
		"  left as they are: focus-events, status-interval and status-right-length (their values from before curia install " +
			"are not known) and each window's @curia_marker value",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	checkUnloaded(t, tm, formats)

	again := UnloadTmux(ctx, serverRunner(tm), conf, bin, defaultKeys, defaultKeys)
	for _, w := range []string{"  pane-focus-in: no Curia hook to remove", "  client-attached: no Curia hook to remove", "  status-right: holds no Curia badge", "  @curia_marker_format: not set"} {
		if !slices.Contains(again, w) {
			t.Errorf("second pass lacks %q:\n%s", w, strings.Join(again, "\n"))
		}
	}
	checkUnloaded(t, tm, formats)
}

// checkUnloaded checks the server unloadFixture built after UnloadTmux.
func checkUnloaded(t *testing.T, tm func(...string) (string, error), formats [2]string) {
	t.Helper()
	if got, want := focusHooks(t, tm), "pane-focus-in[0] display-message first\npane-focus-in[100] display-message last"; got != want {
		t.Errorf("hooks =\n%s\nwant\n%s", got, want)
	}
	if got := mustTmux(t, tm, "show", "-gv", "status-right"); got != "#[fg=blue]before #[fg=red]after" {
		t.Errorf("status-right = %q, want the theme's before and after it", got)
	}
	for i, opt := range []string{"window-status-format", "window-status-current-format"} {
		if got := mustTmux(t, tm, "show", "-gv", opt); got != formats[i] {
			t.Errorf("%s = %q, want %q", opt, got, formats[i])
		}
	}
	if out, err := tm("show", "-gv", "@curia_marker_format"); err == nil {
		t.Errorf("@curia_marker_format still set: %q", out)
	}
	if got := mustTmux(t, tm, "show-hooks", "-g", "client-attached"); got != "client-attached[0] display-message mine" {
		t.Errorf("client-attached hooks = %q, want only the user's entry", got)
	}
	for key, want := range map[string]string{"a": "display-message mine", "m": "select-pane -m", "A": "", "N": ""} {
		out, _ := tm("list-keys", "-T", "prefix", key)
		if got := strings.TrimSpace(out); (want == "" && !strings.HasPrefix(got, "unknown key")) || !strings.HasSuffix(got, want) {
			t.Errorf("key %s = %q, want %q", key, got, cmp.Or(want, "unbound"))
		}
	}
}

// TestUnloadTmuxLeavesTheUsersOwn: what a line of the user's own outside the
// curia block sets is left in the server, even in Curia's exact form; so is
// a badge the user or a theme edited or added twice, and a key bound to a
// command that only mentions curia.
func TestUnloadTmuxLeavesTheUsersOwn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tm, conf, _ := unloadFixture(ctx, t, testBin)
	own := "bind N run-shell '" + testBin + " next'\nbind u display-popup -E -w 90% -h 80% '" + testBin + " ui'\n"
	writeFile(t, conf, readFile(t, conf)+own)
	mustTmux(t, tm, "source-file", conf)
	edited := "#[fg=blue]before#[default] #{@curia_badge}#(" + testBin + " status --short) #[fg=red]after"
	mustTmux(t, tm, "set", "-g", "status-right", edited)
	doubled := "x #{@curia_badge}#(" + testBin + " status) y #{@curia_badge}#(" + testBin + " status)"
	mustTmux(t, tm, "set", "-g", "status-left", doubled)

	lines := UnloadTmux(ctx, serverRunner(tm), conf, testBin, defaultKeys, defaultKeys)

	for _, w := range []string{
		"  status-right: holds @curia_badge, but not exactly once as curia install added it; left as it is: " + edited,
		"  status-left: holds @curia_badge, but not exactly once as curia install added it; left as it is: " + doubled,
		"  prefix N: runs Curia's next command, but " + conf + " binds prefix N outside the curia block; left as it is",
		"  prefix u: mentions curia but is not one of Curia's keys; left as it is: display-popup -E -h 80% -w 90% " + testBin + " ui",
	} {
		if !slices.Contains(lines, w) {
			t.Errorf("lines lack %q:\n%s", w, strings.Join(lines, "\n"))
		}
	}
	for opt, want := range map[string]string{"status-right": edited, "status-left": doubled} {
		if got := mustTmux(t, tm, "show", "-gv", opt); got != want {
			t.Errorf("%s = %q, want it untouched", opt, got)
		}
	}
	for _, key := range []string{"N", "u"} {
		if got := mustTmux(t, tm, "list-keys", "-T", "prefix", key); !strings.Contains(got, testBin) {
			t.Errorf("the user's key %s = %q, want it untouched", key, got)
		}
	}

	// A configuration line of the user's that sets the hook or the badge
	// keeps those too.
	tm2, conf2, _ := unloadFixture(ctx, t, testBin)
	writeFile(t, conf2, "set-hook -ag pane-focus-in 'run-shell \"curia seen\"'\nset -ag status-right '#{@curia_badge}'\n")
	hooks := focusHooks(t, tm2)
	right := mustTmux(t, tm2, "show", "-gv", "status-right")
	UnloadTmux(ctx, serverRunner(tm2), conf2, testBin, defaultKeys, defaultKeys)
	if got := focusHooks(t, tm2); got != hooks {
		t.Errorf("hooks = %q, want them untouched: %q", got, hooks)
	}
	if got := mustTmux(t, tm2, "show", "-gv", "status-right"); got != right {
		t.Errorf("status-right = %q, want it untouched: %q", got, right)
	}
}

// TestUnloadTmuxWithNoServer: with no server to reach, UnloadTmux says so
// and gives the hand commands, leaving out what the user's lines bind.
func TestUnloadTmuxWithNoServer(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, ".tmux.conf")
	writeFile(t, conf, "bind a display-popup -E recon\n")
	down := func(context.Context, ...string) (string, error) {
		return "", errors.New("no server running on /tmp/tmux-501/default")
	}
	lines := UnloadTmux(context.Background(), down, conf, testBin, defaultKeys, defaultKeys)
	want := []string{
		"tmux: " + conf + " no longer loads Curia; no running tmux server was reached (no server running on /tmp/tmux-501/default)",
		"tmux: a tmux server that is still running keeps Curia's hook, keys, badge and marker until it restarts " +
			"(tmux kill-server, which closes its sessions); to clear them by hand instead, run these in a shell inside it:",
		`  tmux show-hooks -g pane-focus-in | grep -F -- 'run-shell -b "` + testBin + ` seen #{pane_id}"' | cut -d' ' -f1 | ` +
			`while read -r h; do tmux set-hook -gu "$h"; done`,
		`  tmux show-hooks -g client-attached | grep -F -- 'run-shell -b "` + testBin + ` monitor start --quiet"' | cut -d' ' -f1 | ` +
			`while read -r h; do tmux set-hook -gu "$h"; done`,
		"  (prefix a is left out: " + conf + " binds it outside the curia block)",
		`  tmux list-keys -T prefix A 2>/dev/null | grep -qF -- '"` + testBin + ` decide"' && tmux unbind-key -T prefix A`,
		`  tmux list-keys -T prefix N 2>/dev/null | grep -qF -- '"` + testBin + ` next"' && tmux unbind-key -T prefix N`,
		`  tmux list-keys -T prefix m 2>/dev/null | grep -qF -- '"` + testBin + ` say"' && tmux bind-key -T prefix m select-pane -m`,
		`  tmux show -gv status-right | grep -qF -- '#[default] #{@curia_badge}#(` + testBin + ` status)' && ` +
			`tmux set -g status-right "$(tmux show -gv status-right | B='#[default] #{@curia_badge}#(` + testBin + ` status)' ` +
			`awk '{ i = index($0, ENVIRON["B"]); if (i) $0 = substr($0, 1, i - 1) substr($0, i + length(ENVIRON["B"])) } 1')"`,
		"  tmux set -gu @curia_marker_format  # the marker left in your window formats then draws nothing",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// TestRefreshTmuxOverStalePath: a server still holding an earlier install's
// entries for an old binary path (a legacy focus hook between two foreign
// ones, a badge inside a themed status-right, a key) ends up, after install
// and a source of the snippet, with one focus hook entry for the new binary,
// the foreign entries where they were, the badge in its place for the new
// binary and the key on the new binary. A second refresh changes nothing.
func TestRefreshTmuxOverStalePath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	old := "/old/curia"
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[0]", "display-message first")
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[1]", `run-shell -b "`+old+` seen #{pane_id}"`)
	mustTmux(t, tm, "set-hook", "-g", "pane-focus-in[2]", "display-message last")
	mustTmux(t, tm, "set", "-g", "status-right", "#[fg=blue]before#[default] #{@curia_badge}#("+old+" status) #[fg=red]after")
	mustTmux(t, tm, "bind", "A", "display-popup", "-E", "-w", "90%", "-h", "80%", old+" decide")
	mustTmux(t, tm, "bind", "u", "display-popup", "-E", "-w", "90%", "-h", "80%", old+" ui") // not one of the keys
	conf := filepath.Join(dir, ".tmux.conf")

	lines := RefreshTmux(ctx, serverRunner(tm), conf, testBin, defaultKeys)
	want := []string{
		`tmux: pane-focus-in[1] removed, an earlier curia install's: run-shell -b "/old/curia seen #{pane_id}" ` +
			"(sourcing ~/.tmux.conf sets this binary's as pane-focus-in[77])",
		"tmux: status-right: Curia's badge now runs " + testBin + " (it ran /old/curia); the rest is as it was",
		"tmux: prefix A: Curia's decisions binding now runs " + testBin + " (it ran /old/curia)",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	sourceSnippet(t, tm, dir, testBin, 2)

	wantHooks := "pane-focus-in[0] display-message first\npane-focus-in[2] display-message last\n" + ourHook(77, testBin)
	if got := focusHooks(t, tm); got != wantHooks {
		t.Errorf("hooks =\n%s\nwant\n%s", got, wantHooks)
	}
	wantRight := "#[fg=blue]before#[default] #{@curia_badge}#(" + testBin + " status) #[fg=red]after"
	if got := mustTmux(t, tm, "show", "-gv", "status-right"); got != wantRight {
		t.Errorf("status-right = %q, want %q", got, wantRight)
	}
	if got := mustTmux(t, tm, "list-keys", "-T", "prefix", "A"); !strings.HasSuffix(got, `"`+testBin+` decide"`) || strings.Contains(got, old) {
		t.Errorf("key A = %q, want the new binary's decide popup", got)
	}
	if got := mustTmux(t, tm, "list-keys", "-T", "prefix", "u"); !strings.Contains(got, old) {
		t.Errorf("key u = %q, want it untouched", got)
	}
	if again := RefreshTmux(ctx, serverRunner(tm), conf, testBin, defaultKeys); len(again) != 0 {
		t.Errorf("second refresh = %q, want nothing", again)
	}
	if lines := RefreshTmux(ctx, func(context.Context, ...string) (string, error) {
		return "", errors.New("no server running")
	}, conf, testBin, defaultKeys); lines != nil {
		t.Errorf("refresh with no server = %q, want nothing", lines)
	}
}

// TestFindBadges covers the badge forms: the current one, the Plan 1 one
// (a space before it), a quoted binary, and forms that are not Curia's.
func TestFindBadges(t *testing.T) {
	for _, tc := range []struct {
		v, want string // want: the badge cut out of v, "" for none
		marks   int
	}{
		{"a#[default] #{@curia_badge}#(/x/curia status)b", "ab", 1},
		{"a #{@curia_badge}#(/x/curia status)b", "ab", 1},
		{"a#[default] #{@curia_badge}#('/my x/curia' status)b", "ab", 1},
		{"a#{@curia_badge}#(/x/curia status)b", "", 1},      // nothing before it
		{"a #{@curia_badge}#(curia status)b", "", 1},        // not an absolute path
		{"a #{@curia_badge}#('/x'y/curia' status)b", "", 1}, // not shellQuote's form
		{"a #{@curia_badge}#(/x/curia status --x)b", "", 1}, // edited
		{"a #(/x/curia status)b", "", 0},                    // no mark
	} {
		found, marks := findBadges(tc.v)
		got := ""
		if len(found) == 1 {
			got = tc.v[:found[0].start] + tc.v[found[0].end:]
		}
		if got != tc.want || marks != tc.marks || len(found) > 1 {
			t.Errorf("findBadges(%q) = %v (%q), %d; want %q, %d", tc.v, found, got, marks, tc.want, tc.marks)
		}
	}
}

// TestTmuxWords covers how tmux prints commands: quotes, escapes inside
// double quotes, and a brace that marks a command Curia never binds.
func TestTmuxWords(t *testing.T) {
	for _, tc := range []struct {
		line  string
		want  []string
		plain bool
	}{
		{
			`bind-key  -T prefix A display-popup -E -h "80%" "'/p q/cu\$r\"ia' decide"`,
			[]string{"bind-key", "-T", "prefix", "A", "display-popup", "-E", "-h", "80%", `'/p q/cu$r"ia' decide`},
			true,
		},
		{`run-shell -b "'/it'\\''s/curia' seen #{pane_id}"`, []string{"run-shell", "-b", `'/it'\''s/curia' seen #{pane_id}`}, true},
		{`bind-key -T prefix \; send-keys x`, []string{"bind-key", "-T", "prefix", ";", "send-keys", "x"}, true},
		{`display-menu x { kill-pane }`, []string{"display-menu", "x", "{", "kill-pane", "}"}, false},
		{`run-shell "unfinished`, []string{"run-shell", "unfinished"}, false},
	} {
		got, plain := tmuxWords(tc.line)
		if !slices.Equal(got, tc.want) || plain != tc.plain {
			t.Errorf("tmuxWords(%q) = %q, %v; want %q, %v", tc.line, got, plain, tc.want, tc.plain)
		}
	}
}

// monitorHooks is show-hooks' list of client-attached entries.
func monitorHooks(t *testing.T, tm func(args ...string) (string, error)) string {
	t.Helper()
	return mustTmux(t, tm, "show-hooks", "-g", "client-attached")
}

// TestSnippetStartsTheMonitorOnAttach: sourcing the snippet (twice, then for
// a moved binary) sets one client-attached entry that runs `monitor start
// --quiet`, keeps the user's own entries, and leaves a user's entry at
// Curia's index. A client that really attaches runs the command once, and
// installing over an existing setup (another source) starts nothing: the
// line only runs on an attach.
func TestSnippetStartsTheMonitorOnAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	mustTmux(t, tm, "set-hook", "-g", "client-attached[0]", "display-message first")

	// A script that records each run, in a directory whose name needs quoting.
	bin := filepath.Join(dir, "my bin", "curia")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, bin, "#!/bin/sh\necho \"$@\" >> '"+filepath.Join(dir, "ran")+"'\n")
	if out, err := exec.Command("chmod", "700", bin).CombinedOutput(); err != nil {
		t.Fatalf("chmod: %v %s", err, out)
	}
	sourceSnippet(t, tm, dir, bin, 2)
	want := "client-attached[0] display-message first\n" +
		fmt.Sprintf(`client-attached[78] run-shell -b "%s monitor start --quiet"`, shellQuote(bin))
	if got := monitorHooks(t, tm); got != want {
		t.Errorf("hooks after sourcing twice =\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("sourcing the snippet ran the monitor command: only an attach may")
	}

	// A real attach (a control-mode client needs no terminal).
	cmd := exec.CommandContext(ctx, "tmux", "-L", realSocket, "-C", "attach")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	var ran string
	for time.Now().Before(deadline) && !strings.Contains(ran, "start") {
		time.Sleep(50 * time.Millisecond)
		ran = readFileIfAny(filepath.Join(dir, "ran"))
	}
	if strings.Count(ran, "monitor start --quiet\n") != 1 {
		t.Errorf("the attach ran %q, want exactly one `monitor start --quiet`", ran)
	}

	moved := "/usr/local/bin/curia"
	sourceSnippet(t, tm, dir, moved, 1)
	if got, w := monitorHooks(t, tm), "client-attached[0] display-message first\n"+fmt.Sprintf(`client-attached[78] run-shell -b "%s monitor start --quiet"`, moved); got != w {
		t.Errorf("hooks for a moved binary =\n%s\nwant\n%s", got, w)
	}
	mustTmux(t, tm, "set-hook", "-g", "client-attached[78]", "display-message mine")
	sourceSnippet(t, tm, dir, testBin, 1)
	if got, w := monitorHooks(t, tm), "client-attached[0] display-message first\nclient-attached[78] display-message mine"; got != w {
		t.Errorf("hooks with the user's own entry at Curia's index =\n%s\nwant\n%s", got, w)
	}
}

func readFileIfAny(path string) string {
	b, _ := os.ReadFile(filepath.Clean(path))
	return string(b)
}

// TestRefreshTmuxMovesTheMonitorHook: a server holding an earlier binary's
// monitor hook (another index too) ends with none of those and the new
// source gives one for the new binary.
func TestRefreshTmuxMovesTheMonitorHook(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	tm := realTmux(ctx, t, dir)
	mustTmux(t, tm, "set-hook", "-g", "client-attached[0]", "display-message first")
	mustTmux(t, tm, "set-hook", "-g", "client-attached[5]", `run-shell -b "/old/curia monitor start --quiet"`)
	sourceSnippet(t, tm, dir, "/old/curia", 1)
	sourceSnippet(t, tm, dir, testBin, 1)
	lines := RefreshTmux(ctx, serverRunner(tm), filepath.Join(dir, ".tmux.conf"), testBin, defaultKeys)
	if got, w := monitorHooks(t, tm), "client-attached[0] display-message first\nclient-attached[78] run-shell -b \""+testBin+" monitor start --quiet\""; got != w {
		t.Errorf("hooks =\n%s\nwant\n%s\n(lines %q)", got, w, lines)
	}
}

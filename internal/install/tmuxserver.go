package install

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// TmuxRunner runs one tmux command, each argument passed as it is (never
// through a shell), on the running server plain tmux reaches, and returns
// its standard output; the error carries tmux's own message.
type TmuxRunner func(ctx context.Context, args ...string) (string, error)

// hookKind is one hook the snippet sets as one entry of the hook's array:
// the hook, Curia's index in it, the end of the command Curia runs (after
// the binary) and a regular expression for how show-hooks prints that end.
// Sourcing the snippet again, or for a new binary, replaces Curia's own
// entry and keeps every other entry where it is (a plain set-hook replaces
// the whole array).
type hookKind struct {
	hook   string
	index  int
	suffix string
	tail   string
}

func (k hookKind) name() string { return k.hook + "[" + strconv.Itoa(k.index) + "]" }

// The hooks the snippet sets: the focus hook (seen) and, when a client
// attaches, the monitor's autostart (`curia monitor start --quiet`, which
// does nothing when monitor.autostart is false or a monitor already runs).
var (
	focusKind  = hookKind{"pane-focus-in", 77, " seen #{pane_id}", ` seen .\{pane_id\}.?`}
	attachKind = hookKind{"client-attached", 78, " monitor start --quiet", ` monitor start --quiet.?`}
	hookKinds  = []hookKind{focusKind, attachKind}
)

// The badge in status-right is badgeStyle (or, from the Plan 1 snippet, a
// space), badgeOpen, the binary as one shell word and badgeClose. The
// never-set @curia_badge option marks it.
const (
	badgeStyle = "#[default] "
	badgeOpen  = "#{@curia_badge}#("
	badgeClose = " status)"
)

// markerRef is what the snippet appends to each window format.
const markerRef = "#{E:@curia_marker_format}"

// markerOption is the option markerRef refers to.
const markerOption = "@curia_marker_format"

// curiaActions are the curia subcommands the snippet's keys run, by the
// key's name in keyStates.
var curiaActions = map[string]string{"dashboard": "ui", "decisions": "decide", "next": "next", "message": "say"}

// tmuxWords splits one line of tmux's own output (show-hooks, list-keys)
// into words as tmux's parser reads them: blanks separate words, single
// quotes keep text as it is, and a backslash takes the next character as it
// is, inside double quotes too. plain is false when the line holds what
// Curia's commands never do: an unquoted brace or ;, or an unfinished quote.
func tmuxWords(line string) (words []string, plain bool) {
	w := wordScanner{plain: true}
	for _, r := range line {
		w.next(r)
	}
	w.flush()
	return w.words, w.plain && w.quote == 0 && !w.escaped
}

// wordScanner is tmuxWords's state.
type wordScanner struct {
	words           []string
	cur             strings.Builder
	inWord, escaped bool
	quote           rune
	plain           bool
}

func (w *wordScanner) next(r rune) {
	switch {
	case w.escaped:
		w.cur.WriteRune(r)
		w.escaped = false
	case w.quote == '\'':
		w.inSingle(r)
	case r == '\\':
		w.escaped, w.inWord = true, true
	case w.quote == '"':
		w.inDouble(r)
	case r == '\'' || r == '"':
		w.quote, w.inWord = r, true
	case r == ' ' || r == '\t' || r == '\n':
		w.flush()
	default:
		if r == '{' || r == '}' || r == ';' {
			w.plain = false
		}
		w.cur.WriteRune(r)
		w.inWord = true
	}
}

func (w *wordScanner) inSingle(r rune) {
	if r == '\'' {
		w.quote = 0
		return
	}
	w.cur.WriteRune(r)
}

func (w *wordScanner) inDouble(r rune) {
	if r == '"' {
		w.quote = 0
		return
	}
	w.cur.WriteRune(r)
}

func (w *wordScanner) flush() {
	if w.inWord {
		w.words = append(w.words, w.cur.String())
		w.cur.Reset()
		w.inWord = false
	}
}

// commandKey is a command's words with its flags in a fixed order: list-keys
// prints a command's flags in an order of its own. A flag followed by a word
// that is not a flag is taken with that word as its value; the first word is
// the command and the last its argument.
func commandKey(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "\x00")
	}
	mid := words[1 : len(words)-1]
	var parts []string
	for i := 0; i < len(mid); i++ {
		part := mid[i]
		if strings.HasPrefix(part, "-") && i+1 < len(mid) && !strings.HasPrefix(mid[i+1], "-") {
			part += "\x00" + mid[i+1]
			i++
		}
		parts = append(parts, part)
	}
	slices.Sort(parts)
	return words[0] + "\x01" + strings.Join(parts, "\x01") + "\x01" + words[len(words)-1]
}

// shellUnquote returns the absolute path that shellQuote turned into p, and
// false when p is not exactly shellQuote's form of an absolute path.
func shellUnquote(p string) (string, bool) {
	s := p
	if len(p) >= 2 && strings.HasPrefix(p, "'") && strings.HasSuffix(p, "'") {
		s = strings.ReplaceAll(p[1:len(p)-1], `'\''`, "'")
	}
	return s, filepath.IsAbs(s) && shellQuote(s) == p
}

// ourBinding reports whether a key's command, as words, is exactly the
// command the snippet binds for one of its keys, for some binary: it returns
// that key's state (Name and Command) and the binary.
func ourBinding(words []string, plain bool) (KeyState, string, bool) {
	if !plain || len(words) < 2 {
		return KeyState{}, "", false
	}
	last, got := words[len(words)-1], commandKey(words)
	for name, action := range curiaActions {
		quoted, ok := strings.CutSuffix(last, " "+action)
		if !ok {
			continue
		}
		bin, ok := shellUnquote(quoted)
		if !ok {
			continue
		}
		ours := keyNamed(bin, name)
		if want, _ := tmuxWords(ours.Command); commandKey(want) == got {
			return ours, bin, true
		}
	}
	return KeyState{}, "", false
}

// keyNamed is the snippet's key state called name for the binary at bin.
func keyNamed(bin, name string) KeyState {
	all := TmuxKeys{Dashboard: "x", Decisions: "x", Next: "x", Message: "x"}
	i := slices.IndexFunc(keyStates(bin, all), func(k KeyState) bool { return k.Name == name })
	return keyStates(bin, all)[i]
}

// hookEntry is one entry of the focus hook's array.
type hookEntry struct {
	name    string // such as pane-focus-in[0]
	index   int
	command string // as show-hooks prints it
	bin     string // Curia's binary when the entry is the snippet's command, else ""
}

// ourHookBin returns the binary when command, as show-hooks prints it, is
// exactly the snippet's focus hook for some binary, else "".
func ourHookBin(command string, k hookKind) string {
	w, plain := tmuxWords(command)
	if !plain || len(w) != 3 || w[0] != "run-shell" || w[1] != "-b" {
		return ""
	}
	quoted, ok := strings.CutSuffix(w[2], k.suffix)
	if !ok {
		return ""
	}
	bin, ok := shellUnquote(quoted)
	if !ok {
		return ""
	}
	return bin
}

// parseHooks reads show-hooks output: one "name[index] command" line per
// entry (a hook with no entry prints its bare name).
func parseHooks(out string, k hookKind) []hookEntry {
	var entries []hookEntry
	for _, line := range strings.Split(out, "\n") {
		name, command, _ := strings.Cut(line, " ")
		digits, ok := strings.CutPrefix(name, k.hook+"[")
		if !ok || command == "" {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSuffix(digits, "]"))
		if err != nil || !strings.HasSuffix(digits, "]") {
			continue
		}
		entries = append(entries, hookEntry{name: name, index: index, command: command, bin: ourHookBin(command, k)})
	}
	return entries
}

// badge is one of Curia's badges in an option's value: v[start:end], which
// runs bin, written in the value as quoted.
type badge struct {
	start, end  int
	bin, quoted string
}

// findBadges returns Curia's badges in v, and how many times v mentions
// @curia_badge at all.
func findBadges(v string) (found []badge, marks int) {
	marks = strings.Count(v, "@curia_badge")
	for i := 0; ; {
		j := strings.Index(v[i:], badgeOpen)
		if j < 0 {
			return found, marks
		}
		open := i + j
		i = open + len(badgeOpen)
		quoted, _, ok := strings.Cut(v[i:], badgeClose)
		if !ok {
			continue
		}
		bin, ok := shellUnquote(quoted)
		start := open
		switch {
		case strings.HasSuffix(v[:open], badgeStyle):
			start -= len(badgeStyle)
		case strings.HasSuffix(v[:open], " "):
			start--
		default:
			ok = false
		}
		if ok {
			found = append(found, badge{start: start, end: i + len(quoted) + len(badgeClose), bin: bin, quoted: quoted})
		}
	}
}

// userTmux is what the user's own lines of the tmux configuration, outside
// the curia block, set that Curia's snippet sets too: such a line is the
// user's, and what it set is never touched in a server.
type userTmux struct {
	keys          map[string]bool // prefix keys bound
	hook          bool            // a pane-focus-in line that mentions curia
	attach        bool            // a client-attached line that mentions curia
	badge, marker bool            // a line that mentions @curia_badge, @curia_marker_format
}

// hookOf reports whether a line of the user's own sets hook k's hook with curia in it.
func (u userTmux) hookOf(k hookKind) bool {
	if k == attachKind {
		return u.attach
	}
	return u.hook
}

// readUserTmux reads the user's lines from the tmux configuration at path.
func readUserTmux(path string) (userTmux, error) {
	u := userTmux{keys: map[string]bool{}}
	conf, err := readConf(path)
	if err != nil {
		return u, err
	}
	inFence := false
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == fenceBegin:
			inFence = true
		case trimmed == fenceEnd:
			inFence = false
		case !inFence && !strings.HasPrefix(trimmed, "#"):
			if key, _, ok := prefixBinding(confWords(trimmed)); ok {
				u.keys[key] = true
			}
			u.hook = u.hook || (strings.Contains(trimmed, focusKind.hook) && strings.Contains(trimmed, "curia"))
			u.attach = u.attach || (strings.Contains(trimmed, attachKind.hook) && strings.Contains(trimmed, "curia"))
			u.badge = u.badge || strings.Contains(trimmed, "@curia_badge")
			u.marker = u.marker || strings.Contains(trimmed, markerOption)
		}
	}
	return u, nil
}

// tmuxSession is one pass over a running server, collecting one line per
// thing done or left.
type tmuxSession struct {
	ctx      context.Context
	run      TmuxRunner
	confPath string
	user     userTmux
	lead     string // starts every line
	lines    []string
}

func (s *tmuxSession) say(format string, args ...any) {
	s.lines = append(s.lines, s.lead+fmt.Sprintf(format, args...))
}

// do runs a tmux command that changes the server, saying so when it fails.
func (s *tmuxSession) do(what string, args ...string) bool {
	if _, err := s.run(s.ctx, args...); err != nil {
		s.say("%s: could not be changed (%v); left as it is", what, err)
		return false
	}
	return true
}

// option returns a global option's value, false when it is not set or
// cannot be read.
func (s *tmuxSession) option(name string) (string, bool) {
	out, err := s.run(s.ctx, "show-options", "-gv", name)
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(out, "\n"), true
}

// binding returns a prefix key's command as words, whether those parsed
// plainly, and false when the key is not bound.
func (s *tmuxSession) binding(key string) (words []string, plain, bound bool) {
	out, err := s.run(s.ctx, "list-keys", "-T", "prefix", key)
	if err != nil {
		return nil, false, false
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return bindingWords(line)
}

// bindingWords splits one list-keys line into the command after its key.
func bindingWords(line string) (words []string, plain, bound bool) {
	w, plain := tmuxWords(line)
	i := slices.Index(w, "-T")
	if len(w) == 0 || w[0] != "bind-key" || i < 0 || i+3 >= len(w) {
		return nil, false, false
	}
	return w[i+3:], plain, true
}

// reach asks the server plain tmux reaches for its socket path.
func reach(ctx context.Context, run TmuxRunner) (string, error) {
	out, err := run(ctx, "display-message", "-p", "#{socket_path}")
	return strings.TrimSpace(out), err
}

// UnloadTmux undoes, in the running tmux server that run reaches, what
// Curia's snippet loaded and nothing else, once RemoveTmux has taken the
// snippet out of the configuration at confPath, and returns one line for
// each thing it did or left:
//   - each pane-focus-in entry that is exactly the snippet's command (for
//     any binary) is unset; every other entry stays where it is;
//   - Curia's badge is cut out of status-right (and status-left) when the
//     option holds it exactly once as install added it, the rest kept byte
//     for byte; an edited or doubled one is left;
//   - the marker reference is cut out of each window format the same way,
//     and @curia_marker_format is unset;
//   - each of keys and defaults whose current binding is exactly Curia's
//     command is unbound, or, for tmux's own m, bound back to tmux's command;
//     a key bound to anything else is left;
//   - whatever a line of the user's own outside the curia block sets is left.
//
// When no server answers it says so and gives the commands that clear a
// server by hand. It never fails.
func UnloadTmux(ctx context.Context, run TmuxRunner, confPath, bin string, keys, defaults TmuxKeys) []string {
	user, err := readUserTmux(confPath)
	if err != nil {
		return handLines(confPath, bin, keys, userTmux{keys: map[string]bool{}}, err)
	}
	socket, err := reach(ctx, run)
	if err != nil {
		return handLines(confPath, bin, keys, user, err)
	}
	s := &tmuxSession{ctx: ctx, run: run, confPath: confPath, user: user, lead: "  "}
	s.lines = []string{fmt.Sprintf("tmux: %s no longer loads Curia; undoing what it loaded in the running server %s:", confPath, socket)}
	for _, k := range hookKinds {
		s.unloadHooks(k)
	}
	for _, opt := range []string{"status-right", "status-left"} {
		s.unloadBadge(opt)
	}
	s.unloadMarker()
	s.unloadKeys(keys, defaults)
	s.say("left as they are: focus-events, status-interval and status-right-length (their values from before curia install " +
		"are not known) and each window's @curia_marker value")
	return s.lines
}

func (s *tmuxSession) unloadHooks(k hookKind) {
	out, err := s.run(s.ctx, "show-hooks", "-g", k.hook)
	if err != nil {
		s.say("%s: could not be read (%v); left as it is", k.hook, err)
		return
	}
	removed, own := 0, s.user.hookOf(k)
	for _, e := range parseHooks(out, k) {
		switch {
		case e.bin == "":
			s.say("%s: not Curia's, kept: %s", e.name, e.command)
		case own:
			s.say("%s: kept: %s sets a %s hook that mentions curia outside the curia block: %s", e.name, s.confPath, k.hook, e.command)
		case s.do(e.name, "set-hook", "-gu", e.name):
			removed++
			s.say("%s: removed Curia's hook: %s", e.name, e.command)
		}
	}
	if removed == 0 && !own {
		s.say("%s: no Curia hook to remove", k.hook)
	}
}

func (s *tmuxSession) unloadBadge(opt string) {
	v, ok := s.option(opt)
	if !ok {
		s.say("%s: could not be read; left as it is", opt)
		return
	}
	found, marks := findBadges(v)
	switch {
	case marks == 0:
		if opt == "status-right" || strings.Contains(v, "curia") {
			s.say("%s: holds no Curia badge", opt)
		}
	case s.user.badge:
		s.say("%s: left as it is: %s mentions @curia_badge outside the curia block", opt, s.confPath)
	case len(found) != 1 || marks != 1:
		s.say("%s: holds @curia_badge, but not exactly once as curia install added it; left as it is: %s", opt, v)
	case s.do(opt, "set-option", "-g", "--", opt, v[:found[0].start]+v[found[0].end:]):
		s.say("%s: removed Curia's badge %s; the rest is as it was", opt, v[found[0].start:found[0].end])
	}
}

func (s *tmuxSession) unloadMarker() {
	if s.user.marker {
		s.say("window formats and %s: left as they are: %s mentions %s outside the curia block", markerOption, s.confPath, markerOption)
		return
	}
	for _, opt := range []string{"window-status-format", "window-status-current-format"} {
		v, ok := s.option(opt)
		switch n := strings.Count(v, markerRef); {
		case !ok:
			s.say("%s: could not be read; left as it is", opt)
		case n == 0:
			s.say("%s: holds no Curia marker", opt)
		case n > 1:
			s.say("%s: holds Curia's marker %s %d times, not as curia install added it; left as it is", opt, markerRef, n)
		case s.do(opt, "set-option", "-g", "--", opt, strings.Replace(v, markerRef, "", 1)):
			s.say("%s: removed Curia's marker %s; the rest is as it was", opt, markerRef)
		}
	}
	if _, ok := s.option(markerOption); !ok {
		s.say("%s: not set", markerOption)
	} else if s.do(markerOption, "set-option", "-gu", markerOption) {
		s.say("%s: unset", markerOption)
	}
}

// keyNames returns the valid keys of keys, in order, without repeats.
func keyNames(keys ...TmuxKeys) []string {
	var names []string
	for _, k := range keys {
		for _, key := range []string{k.Dashboard, k.Decisions, k.Next, k.Message} {
			if ValidKey(key) && !slices.Contains(names, key) {
				names = append(names, key)
			}
		}
	}
	return names
}

func (s *tmuxSession) unloadKeys(keys, defaults TmuxKeys) {
	configured, all := keyNames(keys), keyNames(keys, defaults)
	for _, key := range all {
		s.unloadKey(key, slices.Contains(configured, key))
	}
	out, err := s.run(s.ctx, "list-keys", "-T", "prefix")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "curia") {
			continue
		}
		w, _ := tmuxWords(line)
		if i := slices.Index(w, "-T"); i >= 0 && i+3 < len(w) && !slices.Contains(all, w[i+2]) {
			s.say("prefix %s: mentions curia but is not one of Curia's keys; left as it is: %s", w[i+2], strings.Join(w[i+3:], " "))
		}
	}
}

func (s *tmuxSession) unloadKey(key string, configured bool) {
	words, plain, bound := s.binding(key)
	if !bound {
		return
	}
	k, _, ours := ourBinding(words, plain)
	what := "prefix " + key
	switch {
	case !ours:
		if configured || strings.Contains(strings.Join(words, " "), "curia") {
			s.say("%s: not Curia's binding, left as it is: %s", what, strings.Join(words, " "))
		}
	case s.user.keys[key]:
		s.say("%s: runs Curia's %s command, but %s binds prefix %s outside the curia block; left as it is", what, k.Name, s.confPath, key)
	case key == MessageDefaultKey:
		if s.do(what, append([]string{"bind-key", "-T", "prefix", key}, strings.Fields(MessageDefaultCommand)...)...) {
			s.say("%s: Curia's %s binding replaced by tmux's own %s", what, k.Name, MessageDefaultCommand)
		}
	case s.do(what, "unbind-key", "-T", "prefix", key):
		s.say("%s: unbound (it ran Curia's %s command)", what, k.Name)
	}
}

// handLines says that no server was reached and gives, one per line, the
// commands that clear Curia's snippet from a server that is still running
// (each acts only on exactly what the snippet for bin loaded).
func handLines(confPath, bin string, keys TmuxKeys, user userTmux, cause error) []string {
	b := shellQuote(bin)
	lines := []string{
		fmt.Sprintf("tmux: %s no longer loads Curia; no running tmux server was reached (%v)", confPath, cause),
		"tmux: a tmux server that is still running keeps Curia's hook, keys, badge and marker until it restarts " +
			"(tmux kill-server, which closes its sessions); to clear them by hand instead, run these in a shell inside it:",
	}
	for _, hk := range hookKinds {
		if !user.hookOf(hk) {
			lines = append(lines, "  tmux show-hooks -g "+hk.hook+" | grep -F -- "+shellQuote(`run-shell -b "`+b+hk.suffix+`"`)+
				` | cut -d' ' -f1 | while read -r h; do tmux set-hook -gu "$h"; done`)
		}
	}
	for _, k := range keyStates(bin, keys) {
		if k.Key == "" || !ValidKey(k.Key) {
			continue
		}
		if user.keys[k.Key] {
			lines = append(lines, "  (prefix "+k.Key+" is left out: "+confPath+" binds it outside the curia block)")
			continue
		}
		then := "tmux unbind-key -T prefix " + k.Key
		if k.Key == MessageDefaultKey {
			then = "tmux bind-key -T prefix " + k.Key + " " + MessageDefaultCommand
		}
		pattern := `"` + b + " " + curiaActions[k.Name] + `"`
		lines = append(lines, "  tmux list-keys -T prefix "+k.Key+" 2>/dev/null | grep -qF -- "+shellQuote(pattern)+" && "+then)
	}
	if !user.badge {
		seg := shellQuote(badgeStyle + badgeOpen + b + badgeClose)
		lines = append(lines, "  tmux show -gv status-right | grep -qF -- "+seg+` && tmux set -g status-right "$(tmux show -gv status-right | B=`+seg+
			` awk '{ i = index($0, ENVIRON["B"]); if (i) $0 = substr($0, 1, i - 1) substr($0, i + length(ENVIRON["B"])) } 1')"`)
	}
	if !user.marker {
		lines = append(lines, "  tmux set -gu "+markerOption+"  # the marker left in your window formats then draws nothing")
	}
	return lines
}

// RefreshTmux brings the running server that run reaches up to date with
// the snippet for bin, where sourcing the snippet cannot: it removes each
// focus hook entry of an earlier snippet (another binary, or another index),
// points Curia's badge at bin in place, and binds each of Curia's keys that
// still runs another binary to bin's command. It touches nothing else, and
// nothing a line of the user's own sets. It returns one line per change, or
// none when no server answers.
func RefreshTmux(ctx context.Context, run TmuxRunner, confPath, bin string, keys TmuxKeys) []string {
	if _, err := reach(ctx, run); err != nil {
		return nil
	}
	user, err := readUserTmux(confPath)
	if err != nil {
		return nil
	}
	s := &tmuxSession{ctx: ctx, run: run, confPath: confPath, user: user, lead: "tmux: "}
	for _, k := range hookKinds {
		s.refreshHooks(bin, k)
	}
	for _, opt := range []string{"status-right", "status-left"} {
		s.refreshBadge(opt, bin)
	}
	s.refreshKeys(bin, keys)
	return s.lines
}

func (s *tmuxSession) refreshHooks(bin string, k hookKind) {
	if s.user.hookOf(k) {
		return
	}
	out, err := s.run(s.ctx, "show-hooks", "-g", k.hook)
	if err != nil {
		return
	}
	for _, e := range parseHooks(out, k) {
		if e.bin == "" || (e.bin == bin && e.index == k.index) {
			continue
		}
		if s.do(e.name, "set-hook", "-gu", e.name) {
			s.say("%s removed, an earlier curia install's: %s (sourcing ~/.tmux.conf sets this binary's as %s)", e.name, e.command, k.name())
		}
	}
}

func (s *tmuxSession) refreshBadge(opt, bin string) {
	v, ok := s.option(opt)
	if !ok || s.user.badge {
		return
	}
	found, marks := findBadges(v)
	switch {
	case marks == 0 || (len(found) == 1 && found[0].bin == bin):
	case len(found) != 1 || marks != 1:
		s.say("%s holds @curia_badge, but not exactly once as curia install adds it; left as it is: %s", opt, v)
	default:
		b := found[0]
		seg := strings.Replace(v[b.start:b.end], badgeOpen+b.quoted+badgeClose, badgeOpen+shellQuote(bin)+badgeClose, 1)
		if s.do(opt, "set-option", "-g", "--", opt, v[:b.start]+seg+v[b.end:]) {
			s.say("%s: Curia's badge now runs %s (it ran %s); the rest is as it was", opt, bin, b.bin)
		}
	}
}

func (s *tmuxSession) refreshKeys(bin string, keys TmuxKeys) {
	for _, k := range keyStates(bin, keys) {
		if k.Key == "" || !ValidKey(k.Key) || s.user.keys[k.Key] {
			continue
		}
		words, plain, bound := s.binding(k.Key)
		was, wasBin, ours := ourBinding(words, plain)
		if !bound || !ours || was.Name != k.Name || wasBin == bin {
			continue
		}
		command, _ := tmuxWords(k.Command)
		if s.do("prefix "+k.Key, append([]string{"bind-key", "-T", "prefix", k.Key}, command...)...) {
			s.say("prefix %s: Curia's %s binding now runs %s (it ran %s)", k.Key, k.Name, bin, wasBin)
		}
	}
}

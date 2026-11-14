package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	fenceBegin = "# >>> curia >>>"
	fenceEnd   = "# <<< curia <<<"
)

// markerFormat is the window marker: a red dot after a window whose
// @curia_marker is red. It is kept in the @curia_marker_format option and
// each window format refers to it, so the formats themselves are only ever
// appended to, and unsetting the option on a running server turns the marker
// off. It sits at the end of the window's own format: it changes only the
// foreground and styles nothing after the dot, so the tab before it renders
// as the theme drew it. (The status line resets the style after each window
// format; #[default] or push/pop-default inside it would reset a themed tab.)
const markerFormat = "#{?#{==:#{@curia_marker},red},#[fg=red] ●,}"

// oldSnippetLine is a line only the snippet before the marker option wrote:
// it replaced the user's window formats.
const oldSnippetLine = "\nset -g window-status-format "

// TmuxKeys are the tmux prefix-table keys the snippet binds for the dashboard
// popup, the decide popup, curia next and the message popup; "" binds none.
type TmuxKeys struct {
	Dashboard, Decisions, Next, Message string
}

// MessageDefaultKey is the one key that tmux itself binds by default among
// Curia's, to marking a pane; the message key may replace that binding and
// nothing else a user bound.
const MessageDefaultKey = "m"

// MessageDefaultCommand is tmux's own command for MessageDefaultKey, which
// uninstall binds again.
const MessageDefaultCommand = "select-pane -m"

// KeyState is what the snippet does with one of its keys.
type KeyState struct {
	Name   string // what the key opens: dashboard, decisions, next or message
	Option string // its option in Curia's config.toml
	Key    string // the key, "" when the option is empty
	// Taken is set when the tmux configuration binds Key in the prefix table
	// to something that is not Curia's; the snippet then leaves the key alone.
	Taken bool
	// Command is the tmux command the key runs, for a bind line of the
	// user's own.
	Command string
	// Default is tmux's own binding of Key, which the snippet may replace;
	// "" when tmux binds Key to nothing by default.
	Default string
}

// TmuxResult is what WriteTmux found.
type TmuxResult struct {
	Keys []KeyState
	// OldSnippet is set when the snippet replaced was the earlier one that
	// overwrote the window formats.
	OldSnippet bool
}

// validKey matches a tmux key name such as a, A, F5, C-a or M-Up: letters,
// digits and '-', starting with a letter or digit. Nothing in it is special
// to tmux's parser, its formats or the shell.
var validKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,31}$`)

// ValidKey reports whether key is a tmux key name the snippet accepts.
func ValidKey(key string) bool {
	return validKey.MatchString(key)
}

// keyStates returns the snippet's keys, in order, with the binding commands
// for the binary at bin.
func keyStates(bin string, keys TmuxKeys) []KeyState {
	b := shellQuote(bin)
	popup := "display-popup -E -w 90% -h 80% "
	msg := KeyState{
		Name: "message", Option: "key_message", Key: keys.Message,
		Command: "display-popup -E -w 80% -h 12 " + tmuxQuote(b+" say"),
	}
	if keys.Message == MessageDefaultKey {
		msg.Default = MessageDefaultCommand
	}
	return []KeyState{
		{Name: "dashboard", Option: "key_dashboard", Key: keys.Dashboard, Command: popup + tmuxQuote(b+" ui")},
		{Name: "decisions", Option: "key_decisions", Key: keys.Decisions, Command: popup + tmuxQuote(b+" decide")},
		{Name: "next", Option: "key_next", Key: keys.Next, Command: "run-shell " + tmuxQuote(b+" next")},
		msg,
	}
}

// bindLine binds key to command only if the key is free in the prefix table
// or already runs Curia (its binding mentions curia), or, when def is set,
// is still tmux's own binding def, so a re-source or an upgrade rebinds
// Curia's own keys and never the user's. The shell asks the
// server that runs it: tmux sets TMUX in every job to its own socket, and
// with TMUX empty, or that server not answering, nothing is bound.
func bindLine(key, command, def string) string {
	grep := "grep -qv curia"
	if def != "" {
		// list-keys prints "bind-key -T prefix m select-pane -m": the
		// default is that command after the key and nothing else.
		grep = `grep -qvE "curia| -T prefix +` + key + ` +` + def + `$"`
	}
	check := `[ -n "$TMUX" ] && tmux show -g status >/dev/null 2>&1 && ` +
		"! tmux list-keys -T prefix " + key + " 2>/dev/null | " + grep
	return "if-shell " + tmuxQuote(check) + " " + tmuxQuote("bind "+key+" "+command)
}

// snippet is the tmux configuration Curia installs for the binary at bin. It
// changes the user's settings only as far as Curia needs:
//   - focus-events on, because tmux runs pane-focus-in only with it.
//   - status-interval lowered to 2 when it is 0 (no refresh) or larger.
//   - status-right-length raised to 80 when smaller (compared as numbers,
//     e|<): tmux's default status-right already fills the default of 40.
//   - The badge appended to status-right unless there already: the never-set
//     @curia_badge option expands to nothing and marks it. It starts with
//     #[default] and a space, so it is drawn in the status line's own style
//     after a themed segment that ends in its own colors.
//   - The marker appended to whichever window formats are in effect, unless
//     they mention @curia_marker already. set -ag appends the text as it is,
//     without expanding anything in the user's format.
//   - Each key bound only when free (bindLine).
//
// Each command that reaches a shell holds bin as one shell word, wrapped in
// one tmux quoting layer per tmux parse on the way.
func snippet(bin string, keys []KeyState) string {
	b := shellQuote(bin)
	lines := []string{
		"set -g focus-events on",
		"if -F '#{||:#{==:#{status-interval},0},#{e|>:#{status-interval},2}}' 'set -g status-interval 2'",
		"if -F '#{e|<:#{status-right-length},80}' 'set -g status-right-length 80'",
		"if -F '#{m:*@curia_badge*,#{status-right}}' '' " +
			tmuxQuote("set -ag status-right "+tmuxQuote("#[default] #{@curia_badge}#("+b+" status)")),
		"set -g @curia_marker_format " + tmuxQuote(markerFormat),
	}
	for _, opt := range []string{"window-status-format", "window-status-current-format"} {
		lines = append(lines, "if -F '#{m:*@curia_marker*,#{"+opt+"}}' '' "+
			tmuxQuote("set -ag "+opt+" '#{E:@curia_marker_format}'"))
	}
	for _, k := range keys {
		if k.Key != "" {
			lines = append(lines, bindLine(k.Key, k.Command, k.Default))
		}
	}
	lines = append(lines, "set-hook -g pane-focus-in "+tmuxQuote("run-shell -b "+tmuxQuote(b+" seen #{pane_id}")))
	return strings.Join(lines, "\n") + "\n"
}

// tmuxDoubleQuoted escapes what tmux reads specially inside double quotes.
var tmuxDoubleQuoted = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)

// tmuxQuote returns s as one tmux configuration word that parses back to s:
// single-quoted (nothing inside is special) unless s holds a single quote,
// else double-quoted with backslash, double quote and dollar escaped.
func tmuxQuote(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + s + "'"
	}
	return `"` + tmuxDoubleQuoted.Replace(s) + `"`
}

// WriteTmux writes the Curia tmux snippet, binding keys, to snippetPath and
// adds one fenced source-file line for it to the tmux configuration at
// tmuxConfPath, creating either file if missing. If the fence is already
// there the configuration is left as it is. It reports which keys the
// configuration already binds to something else, read from tmuxConfPath
// alone (the snippet itself checks the running server when tmux sources it),
// and whether the snippet it replaced overwrote the window formats.
func WriteTmux(snippetPath, tmuxConfPath, bin string, keys TmuxKeys) (TmuxResult, error) {
	snippetPath = filepath.Clean(snippetPath)
	tmuxConfPath = filepath.Clean(tmuxConfPath)
	states := keyStates(bin, keys)
	for _, k := range states {
		if k.Key != "" && !ValidKey(k.Key) {
			return TmuxResult{}, fmt.Errorf("config %s = %q: not a tmux key name of letters, digits and '-' "+
				"(such as a, A, F5 or C-a); leave it empty to bind no key", k.Option, k.Key)
		}
	}
	conf, err := readConf(tmuxConfPath)
	if err != nil {
		return TmuxResult{}, err
	}
	old, err := readConf(snippetPath)
	if err != nil {
		return TmuxResult{}, err
	}
	res := TmuxResult{Keys: states, OldSnippet: strings.Contains("\n"+old, oldSnippetLine)}
	bound := boundKeys(conf)
	for i, k := range res.Keys {
		res.Keys[i].Taken = k.Key != "" && bound[k.Key]
	}
	if err := writeAtomic(snippetPath, []byte(snippet(bin, states))); err != nil {
		return TmuxResult{}, err
	}
	if hasFence(conf) {
		return res, nil
	}
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		conf += "\n"
	}
	conf += fenceBegin + "\nsource-file '" + snippetPath + "'\n" + fenceEnd + "\n"
	return res, writeAtomic(tmuxConfPath, []byte(conf))
}

// boundKeys returns the keys that bind or bind-key lines in conf, outside
// the curia block, put in the prefix table for something that is not
// Curia's (the rest of the line does not mention curia). Commands chained
// with \; and files the configuration sources are not followed.
func boundKeys(conf string) map[string]bool {
	keys := map[string]bool{}
	inFence := false
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == fenceBegin:
			inFence = true
		case trimmed == fenceEnd:
			inFence = false
		case !inFence && !strings.Contains(trimmed, "curia"):
			if key, ok := prefixBinding(confWords(trimmed)); ok {
				keys[key] = true
			}
		}
	}
	return keys
}

// prefixBinding returns the key of a bind or bind-key command, given as
// words, when it binds in the prefix table.
func prefixBinding(words []string) (string, bool) {
	if len(words) < 2 || (words[0] != "bind" && words[0] != "bind-key") {
		return "", false
	}
	table := "prefix"
	for i := 1; i < len(words); i++ {
		w := words[i]
		if w == "--" {
			w = wordAt(words, i+1)
			return w, w != "" && table == "prefix"
		}
		if !strings.HasPrefix(w, "-") || w == "-" {
			return w, table == "prefix"
		}
		// -T and -N take a value: the rest of the word, or the next word.
		flags, value := w[1:], ""
		if k := strings.IndexAny(flags, "TN"); k >= 0 {
			flags, value = flags[:k+1], flags[k+1:]
			if value == "" {
				i++
				value = wordAt(words, i)
			}
		}
		if strings.Contains(flags, "n") {
			table = "root"
		}
		if strings.HasSuffix(flags, "T") {
			table = value
		}
	}
	return "", false
}

// wordAt returns words[i], or "" past the end.
func wordAt(words []string, i int) string {
	if i < len(words) {
		return words[i]
	}
	return ""
}

// confWords splits one tmux configuration line into words as tmux's parser
// does for the parts that matter here: blanks separate words, quotes group
// a word, and an unquoted # at the start of a word starts a comment.
// Backslash escapes are kept as written: the words read here (the command,
// its flags and the key) are compared with plain key names only.
func confWords(line string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '#' && !inWord:
			return words
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// RemoveTmux removes the fenced block, fence lines included, from the tmux
// configuration at tmuxConfPath. A missing file or one without the block is
// left alone; a begin fence with no end fence is an error and changes nothing.
func RemoveTmux(tmuxConfPath string) error {
	tmuxConfPath = filepath.Clean(tmuxConfPath)
	conf, err := readConf(tmuxConfPath)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(conf, "\n")
	var out []string
	inFence, found := false, false
	for _, line := range lines {
		switch {
		case !inFence && strings.TrimRight(line, "\r\n") == fenceBegin:
			inFence, found = true, true
		case inFence && strings.TrimRight(line, "\r\n") == fenceEnd:
			inFence = false
		case !inFence:
			out = append(out, line)
		}
	}
	if inFence {
		return fmt.Errorf("%s: %q has no matching %q; remove the block by hand", tmuxConfPath, fenceBegin, fenceEnd)
	}
	if !found {
		return nil
	}
	return writeAtomic(tmuxConfPath, []byte(strings.Join(out, "")))
}

// hasFence reports whether conf has a begin fence line, with or without a
// trailing newline.
func hasFence(conf string) bool {
	for _, line := range strings.Split(conf, "\n") {
		if strings.TrimRight(line, "\r") == fenceBegin {
			return true
		}
	}
	return false
}

// readConf returns the file's text, or "" when it does not exist.
func readConf(path string) (string, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

package hook

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
)

// Spec F20. modeLine runs only after the denials of preToolUse, so an allow
// can never undo one. A hook's "allow" is only advice to Claude Code: it
// never overrides a deny rule, an ask rule or plan mode (probe P1), which is
// why bypass here means "allow what the user's settings do not forbid".

// THE DECISION TABLE (after the denials of preToolUse; "none" = no opinion).
//
//	mode    edit tool                  read tool (Read LS Glob Grep TodoWrite Task Agent)  Bash                        Curia tool  other / other MCP
//	normal  none                       none                                                none                        none        none
//	edits   allow: path inside root,   none                                                none                        none        none
//	        not auto-executed config
//	away    as edits, else deny        allow: inside root (Glob/Grep: explicit path and    allow: awayCommand, else    allow       deny
//	                                   a pattern that cannot leave it), else deny          deny
//	bypass  allow                      allow                                               allow                       allow       allow, but none for AskUserQuestion, ExitPlanMode
//
// root is the session's stored worktree, else its stored repository, else
// its checked cwd (never the event's cwd), and never the home directory, a
// parent of it or "/": then nothing is allowed by path (none in edits, deny
// in away). The deny of away is the one that raises a ticket.

// readTools never change anything: away allows them inside the root. Agent
// and Task start a subagent whose own calls are checked one by one (they
// carry the parent's session id). pathTools must name a path.
var (
	readTools = []string{"Read", "Glob", "Grep", "LS", "TodoWrite", "Task", "Agent"}
	pathTools = []string{"Read", "Glob", "Grep", "LS"}
	// searchTools also carry a pattern that can reach outside the root: key
	// of that pattern.
	searchTools = map[string]string{"Glob": "pattern", "Grep": "glob"}
)

// autoExecNames are path components whose files Git or the tooling runs
// later (hooks, config, filters, settings): an edit inside one is never
// allowed by a mode. .git covers a repository's config and hooks.
var autoExecNames = []string{".git", ".claude", ".curia", ".gitattributes", ".gitmodules", ".gitconfig", ".husky"}

// askTools need the user; bypass gives no opinion on them.
var askTools = []string{"AskUserQuestion", "ExitPlanMode"}

// maxAwayCommand bounds a Bash command that can match away_allow, in bytes.
const maxAwayCommand = 2000

// shellMeta are the characters a command may not contain to match away_allow
// (spec F20), besides control characters (newline included).
const shellMeta = ";&|$()`<>\\"

// Fixed wording: no value from the call is put in a reason except a cleaned,
// bounded tool name.
const (
	reasonEdits = "Curia: edits mode allows file edits inside this session's worktree."
	reasonAway  = "Curia: away mode allows this (a file edit or read inside the worktree, an allowed command, or a Curia tool)."
	reasonOpen  = "Curia: bypass mode is on for this session; the deny and ask rules in the user's settings still apply."
)

// modeLine is the decision of the session's mode: nil (no opinion) or one
// allow or deny line. sess is the session as the hook loaded it for this
// event (no second store read); a zero sess means the row could not be read,
// so the mode is unknown and nothing is allowed. st may be nil.
func modeLine(ctx context.Context, ev model.Event, payload []byte, cfg config.Config, sess model.Session, st *store.Store) []byte {
	mode := effectiveMode(ev, cfg, sess)
	if mode == "" || mode == model.ModeNormal {
		return nil
	}
	root := modeRoot(sess)
	in := toolInput(payload)
	if mode == model.ModeBypass {
		if slices.Contains(askTools, ev.ToolName) {
			return nil
		}
		return decisionLine("allow", reasonOpen)
	}
	editOK := slices.Contains(editTools, ev.ToolName) && editAllowed(ev, in, root)
	if mode == model.ModeEdits {
		if editOK {
			return decisionLine("allow", reasonEdits)
		}
		return nil
	}
	// away: nothing may be left to a prompt nobody will answer.
	switch {
	case editOK,
		slices.Contains(readTools, ev.ToolName) && readAllowed(ev, in, root),
		IsCuriaTool(ev.ToolName) || ev.ToolName == "mcp__curia__file_task",
		ev.ToolName == "Bash" && awayCommand(ev, in, root, cfg.Mode.AwayAllow):
		return decisionLine("allow", reasonAway)
	}
	if st != nil {
		_ = st.AddPermissionEvent(ctx, store.PermissionEvent{
			SessionID: sess.ID, At: ev.At, Mode: mode, Tool: sanitize.Line(ev.ToolName, 60),
			Summary: sanitize.Line(deniedSummary(ev.ToolName, in), 150), Verdict: "deny",
		})
	}
	return decisionLine("deny", "Curia: the owner is away (mode away), so "+sanitize.Line(ev.ToolName, 40)+
		" would need a prompt nobody will answer and is not allowed now. Raise an assume ticket with request_decision saying what you need, "+
		"then continue with other work that needs no approval.")
}

// effectiveMode is the session's mode now: its own unless it has lapsed or
// was never set (then the configured default); "" when it cannot be known
// (no row, a Codex session whose output form is unobserved, a value that is
// not a mode).
func effectiveMode(ev model.Event, cfg config.Config, sess model.Session) string {
	if sess.ID == "" || sess.Agent == model.AgentCodex {
		return ""
	}
	m := sess.Mode
	if m == "" || (!sess.ModeUntil.IsZero() && !ev.At.Before(sess.ModeUntil)) {
		m = cfg.Mode.Default
	}
	if !slices.Contains(model.Modes, m) {
		return ""
	}
	return m
}

// modeRoot is the directory paths must be inside: the session's stored
// worktree, else its stored repository, else the directory Curia checked for
// it; "" when that is not an absolute path, is "/", or is the home directory
// or one of its parents (everything would be inside). Never the event's cwd.
func modeRoot(sess model.Session) string {
	root := cmp.Or(sess.Worktree, sess.Repo, sess.CheckedCwd)
	if !filepath.IsAbs(root) {
		return ""
	}
	root = filepath.Clean(root)
	home, err := os.UserHomeDir()
	if err != nil || home == "" || filepath.Dir(root) == root {
		return ""
	}
	for _, h := range []string{home, resolved(home)} {
		if p := strings.ToLower(h); p == strings.ToLower(root) || strings.HasPrefix(p, strings.ToLower(root)+string(filepath.Separator)) {
			return ""
		}
	}
	return root
}

func resolved(p string) string {
	if r, ok := walk(p); ok {
		return r
	}
	return p
}

func toolInput(payload []byte) map[string]json.RawMessage {
	var in struct {
		ToolInput map[string]json.RawMessage `json:"tool_input"`
	}
	_ = json.Unmarshal(payload, &in)
	return in.ToolInput
}

// pathsInRoot returns the resolved targets of every path the call names
// (file_path, notebook_path, path), and whether all lie inside root by the
// same fail-closed walk as protectedFile (targets): a path that cannot be
// checked is not inside. A call that names none returns no paths and ok.
func pathsInRoot(ev model.Event, in map[string]json.RawMessage, root string) (paths []string, ok bool) {
	if root == "" {
		return nil, false
	}
	home, _ := os.UserHomeDir()
	roots := []string{strings.ToLower(root), strings.ToLower(resolved(root))}
	for _, key := range []string{"file_path", "notebook_path", "path"} {
		var p string
		if json.Unmarshal(in[key], &p) != nil || p == "" {
			continue
		}
		found, fine := targets(p, ev.Cwd, home)
		if !fine || slices.ContainsFunc(found, func(t string) bool { return !inside(t, roots) }) {
			return nil, false
		}
		paths = append(paths, found...)
	}
	return paths, true
}

// editAllowed: the edit names a path, all inside root, none in auto-executed
// configuration (autoExecNames, or the gitdir a .git file points to).
func editAllowed(ev model.Event, in map[string]json.RawMessage, root string) bool {
	paths, ok := pathsInRoot(ev, in, root)
	return ok && len(paths) > 0 && !slices.ContainsFunc(paths, func(p string) bool { return autoExec(p, root) })
}

// autoExec reports whether p is in auto-executed configuration. A .git file
// in an ancestor directory (a worktree or submodule pointer) makes the
// gitdir it names auto-executed too; one that cannot be read counts as yes.
func autoExec(p, root string) bool {
	for _, c := range strings.Split(strings.ToLower(p), string(filepath.Separator)) {
		if slices.Contains(autoExecNames, c) {
			return true
		}
	}
	for d := filepath.Dir(p); len(d) > len(root) && filepath.Dir(d) != d; d = filepath.Dir(d) {
		fi, err := os.Lstat(filepath.Join(d, ".git"))
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.Size() > 4096 {
			return true
		}
		b, err := os.ReadFile(filepath.Clean(filepath.Join(d, ".git")))
		g, found := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if err != nil || !found {
			return true
		}
		if g = strings.TrimSpace(g); !filepath.IsAbs(g) {
			g = d + string(filepath.Separator) + g
		}
		r, err := filepath.EvalSymlinks(filepath.Clean(g))
		if err != nil || inside(p, []string{strings.ToLower(r), strings.ToLower(filepath.Clean(g))}) {
			return true
		}
	}
	return false
}

// readAllowed: a read tool whose paths are inside root; Read, LS, Glob and
// Grep must name one, and Glob and Grep also a pattern that is not absolute,
// not "~..." and has no ".." segment.
func readAllowed(ev model.Event, in map[string]json.RawMessage, root string) bool {
	paths, ok := pathsInRoot(ev, in, root)
	if !ok || (len(paths) == 0 && slices.Contains(pathTools, ev.ToolName)) {
		return false
	}
	if key, search := searchTools[ev.ToolName]; search {
		var pat string
		if raw, has := in[key]; has && json.Unmarshal(raw, &pat) != nil {
			return false
		}
		if strings.HasPrefix(pat, "/") || strings.HasPrefix(pat, "~") || strings.ContainsRune(pat, 0) ||
			slices.Contains(strings.FieldsFunc(pat, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
			return false
		}
	}
	return true
}

// awayCommand reports whether the Bash command is allowed in away mode.
// Departure from the spec's globs, for safety: each away_allow entry is a
// list of words (a "*" in it never matches), the command's first words must
// equal them, and every other argument must be a plain word that does not
// start with "-" (no option can write a file or run a program) and, when it
// looks like a path, lies inside root. The command holds no shell
// metacharacter or control character (spec F20) and is not parsed further.
func awayCommand(ev model.Event, in map[string]json.RawMessage, root string, entries []string) bool {
	var cmd string
	if json.Unmarshal(in["command"], &cmd) != nil || cmd == "" || len(cmd) > maxAwayCommand {
		return false
	}
	if strings.ContainsFunc(cmd, func(r rune) bool { return unicode.IsControl(r) || strings.ContainsRune(shellMeta, r) }) {
		return false
	}
	words := strings.Fields(cmd)
	return slices.ContainsFunc(entries, func(e string) bool {
		ew := strings.Fields(e)
		if len(ew) == 0 || len(words) < len(ew) || !slices.Equal(words[:len(ew)], ew) {
			return false
		}
		return !slices.ContainsFunc(words[len(ew):], func(a string) bool { return !plainArg(a, ev, root) })
	})
}

// plainArg: only letters, digits and . _ / @ : + = , % ^ ~ - (not first),
// no leading "-" or "~"; a path-looking word must resolve inside root.
func plainArg(a string, ev model.Event, root string) bool {
	if strings.HasPrefix(a, "-") || strings.HasPrefix(a, "~") ||
		strings.ContainsFunc(a, func(r rune) bool {
			return r >= 128 || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._/@:+=,%^~-", r))
		}) {
		return false
	}
	if strings.Contains(a, "/") || a == "." || a == ".." || strings.HasPrefix(a, ".") {
		return pathInside(a, ev, root)
	}
	return true
}

func pathInside(p string, ev model.Event, root string) bool {
	_, ok := pathsInRoot(ev, map[string]json.RawMessage{"path": mustJSON(p)}, root)
	return ok
}

func mustJSON(s string) json.RawMessage { b, _ := json.Marshal(s); return b }

// deniedSummary is the line stored for a denial: a Bash command, else the
// first path the call names.
func deniedSummary(tool string, in map[string]json.RawMessage) string {
	for _, key := range []string{"command", "file_path", "notebook_path", "path", "url"} {
		var v string
		if json.Unmarshal(in[key], &v) == nil && v != "" {
			return v
		}
	}
	return tool
}

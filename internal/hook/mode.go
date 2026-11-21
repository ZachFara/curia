package hook

import (
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

// readTools never change anything: away allows them inside the worktree.
// Agent and Task start a subagent whose own calls are checked one by one
// (they carry the parent's session id).
var readTools = []string{"Read", "Glob", "Grep", "LS", "TodoWrite", "Task", "Agent"}

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
	root := modeRoot(ev, sess)
	in := toolInput(payload)
	if mode == model.ModeBypass {
		if slices.Contains(askTools, ev.ToolName) {
			return nil
		}
		return decisionLine("allow", reasonOpen)
	}
	editOK := slices.Contains(editTools, ev.ToolName) && withinRoot(ev, in, root, true)
	if mode == model.ModeEdits {
		if editOK {
			return decisionLine("allow", reasonEdits)
		}
		return nil
	}
	// away: nothing may be left to a prompt nobody will answer.
	switch {
	case editOK,
		slices.Contains(readTools, ev.ToolName) && withinRoot(ev, in, root, false),
		IsCuriaTool(ev.ToolName) || ev.ToolName == "mcp__curia__file_task",
		ev.ToolName == "Bash" && awayCommand(in, cfg.Mode.AwayAllow):
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

// modeRoot is the directory edits are allowed in: the session's worktree,
// else the event's cwd; "" when it is not a usable absolute directory path
// (or the file system root).
func modeRoot(ev model.Event, sess model.Session) string {
	root := sess.Worktree
	if root == "" {
		root = ev.Cwd
	}
	if !filepath.IsAbs(root) || filepath.Dir(filepath.Clean(root)) == filepath.Clean(root) {
		return ""
	}
	return filepath.Clean(root)
}

func toolInput(payload []byte) map[string]json.RawMessage {
	var in struct {
		ToolInput map[string]json.RawMessage `json:"tool_input"`
	}
	_ = json.Unmarshal(payload, &in)
	return in.ToolInput
}

// withinRoot reports whether every path the call names lies inside root,
// resolved by the same fail-closed walk as protectedFile (targets): a path
// that cannot be checked is not inside. needPath: a call that names no path
// is not inside (an edit must name one).
func withinRoot(ev model.Event, in map[string]json.RawMessage, root string, needPath bool) bool {
	if root == "" {
		return false
	}
	home, _ := os.UserHomeDir()
	roots := []string{strings.ToLower(root)}
	if r, ok := walk(root); ok {
		roots = append(roots, strings.ToLower(r))
	}
	named := false
	for _, key := range []string{"file_path", "notebook_path", "path"} {
		var p string
		if json.Unmarshal(in[key], &p) != nil || p == "" {
			continue
		}
		named = true
		found, ok := targets(p, ev.Cwd, home)
		if !ok || slices.ContainsFunc(found, func(t string) bool { return !inside(t, roots) }) {
			return false
		}
	}
	return named || !needPath
}

// awayCommand reports whether the Bash command matches a glob of away_allow
// and holds no shell metacharacter or control character (spec F20). The
// command is not parsed any further.
func awayCommand(in map[string]json.RawMessage, globs []string) bool {
	var cmd string
	if json.Unmarshal(in["command"], &cmd) != nil || cmd == "" || len(cmd) > maxAwayCommand {
		return false
	}
	if strings.ContainsFunc(cmd, func(r rune) bool { return unicode.IsControl(r) || strings.ContainsRune(shellMeta, r) }) {
		return false
	}
	return slices.ContainsFunc(globs, func(g string) bool { return globMatch(g, cmd) })
}

// globMatch matches s against g, where "*" is any run of characters (slashes
// included) and everything else is literal.
func globMatch(g, s string) bool {
	parts := strings.Split(g, "*")
	if len(parts) == 1 {
		return g == s
	}
	rest, ok := strings.CutPrefix(s, parts[0])
	if !ok {
		return false
	}
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, p)
		if i < 0 {
			return false
		}
		rest = rest[i+len(p):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

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

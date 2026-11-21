package hook

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
)

// decideDeadline bounds the store read of a PreToolUse decision. Past it the
// budget check has no opinion (it cannot know) and says so in Result.Undecided.
const decideDeadline = 250 * time.Millisecond

// Problem kinds a PreToolUse decision reports in Result.Undecided.
const (
	undecidedBudget = "budget" // the store could not be read in time
	undecidedFiles  = "files"  // the home directory is unknown, so only configured paths are protected
)

// curiaTools are Curia's own MCP tools, by exact name: the one thing a
// stopped task may still call. A prefix would let another MCP server named
// like Curia imitate them.
var curiaTools = []string{
	"mcp__curia__search_decisions", "mcp__curia__request_decision", "mcp__curia__check_decision",
	"mcp__curia__withdraw_decision", "mcp__curia__check_messages", "mcp__curia__complete_task", "mcp__curia__file_task",
}

// IsCuriaTool reports whether name is exactly one of Curia's seven MCP tools.
func IsCuriaTool(name string) bool { return slices.Contains(curiaTools, name) }

// editTools are the tools whose target path decides file protection. Bash
// is not parsed: a command that writes a protected file is not caught (the
// same-user limit of spec F28).
var editTools = []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}

// PreToolUseNoStore is the decision for a PreToolUse when the store could
// not even be opened: file protection needs no store, so it still holds. It
// returns the line to print, or nil for no opinion.
func PreToolUseNoStore(payload []byte, cfg config.Config) []byte {
	ev, err := Parse(model.PreToolUse, payload, "", time.Time{})
	if err != nil {
		return nil
	}
	out, _ := preToolUse(context.Background(), ev, payload, cfg, nil, "")
	return out
}

// decideAfter sets res.Stdout and res.Undecided from preToolUse when ev is a
// PreToolUse; the session's task is the stored one, else envTask. It is
// deferred by Run, so file protection holds when the update failed.
func decideAfter(ctx context.Context, res *Result, ev model.Event, payload []byte, envTask string, st *store.Store, cfg config.Config) {
	if ev.Kind == model.PreToolUse {
		res.Stdout, res.Undecided = preToolUse(ctx, ev, payload, cfg, st, cmp.Or(res.Session.TaskID, envTask))
	}
}

// preToolUse is THE decision point of a PreToolUse: nil means no opinion
// (Curia prints nothing, as before), else the one line that denies the call
// and shows the reason to the model (probe P1: permissionDecision "deny").
// Denials come first and cannot be undone by anything after them. F20's
// modes will add their "allow" decisions after both denies, at the marked
// place, so an allow can never override a deny. st may be nil (no store).
// A kind of check that could not be made is returned in undecided.
func preToolUse(ctx context.Context, ev model.Event, payload []byte, cfg config.Config, st *store.Store, taskID string) (line []byte, undecided []string) {
	if reason, problem := protectedFile(ev, payload, cfg); reason != "" {
		return denyLine(reason), nil
	} else if problem {
		undecided = append(undecided, undecidedFiles)
	}
	if st != nil && taskID != "" && cfg.Cost.BudgetHardStop && !IsCuriaTool(ev.ToolName) {
		bctx, cancel := context.WithTimeout(ctx, decideDeadline)
		defer cancel()
		b, err := st.TaskBudget(bctx, taskID, cfg.Cost.TaskBudgetDefault)
		if err != nil {
			return nil, append(undecided, undecidedBudget)
		}
		if over, why := b.Exhausted(); over {
			return denyLine(fmt.Sprintf("Curia: %s. Only Curia's own tools still work for this task: request_decision to raise a ticket, "+
				"check_messages, or complete_task to report what is done. Only the user can raise the budget "+
				"(curia task edit %s --budget). The stop can land up to one tool call late.", why, sanitize.Line(taskID, 40))), undecided
		}
	}
	// F20 modes: the "allow" decisions go here.
	return nil, undecided
}

// denyLine is the hook output that denies a tool call: one line of JSON,
// HTML escaping off, and a newline. Claude Code shows reason to the model.
func denyLine(reason string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason,
	}})
	return b.Bytes()
}

// protectedFile returns the denial reason when ev is an edit tool whose
// target resolves inside Curia's configuration, data or state directory or
// is the user's file (spec F28, Ruling 157), in every mode. Resolution:
// cleaned, absolute against the event's cwd, symlinks resolved (the
// longest existing ancestor's, for a file not yet there), compared without
// regard to case. problem is true when the home directory is unknown.
// Limit: Unicode normalisation of a path is not folded.
func protectedFile(ev model.Event, payload []byte, cfg config.Config) (reason string, problem bool) {
	if !slices.Contains(editTools, ev.ToolName) {
		return "", false
	}
	var in struct {
		ToolInput map[string]json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(payload, &in) != nil {
		return "", false
	}
	home, herr := os.UserHomeDir()
	roots := protectedRoots(home, cfg)
	for _, key := range []string{"file_path", "notebook_path"} {
		var p string
		if json.Unmarshal(in.ToolInput[key], &p) != nil || p == "" {
			continue // missing or not a string: the tool itself refuses it
		}
		found, ok := targets(p, ev.Cwd, home)
		if !ok { // fail closed: a path that cannot be checked is not let through
			return "Curia could not verify where this path " + sanitize.Line(p, 60) +
				" leads (too long, unreadable, a symlink loop, or \"..\" after a missing directory), so this tool may not change it. Use a plain absolute path.", false
		}
		if i := slices.IndexFunc(found, func(t string) bool { return inside(t, roots) }); i >= 0 {
			return "Curia protects its own configuration and data: this tool may not change " + sanitize.Line(found[i], 80) +
				". Only the user edits these files (curia config set). Do not retry; tell the user what change you need.", false
		}
	}
	return "", herr != nil
}

// protectedRoots are the lower-cased paths whose subtree (or file) is
// protected, as given and with symlinks resolved.
func protectedRoots(home string, cfg config.Config) []string {
	var raw []string
	if home != "" {
		raw = append(raw, filepath.Join(home, ".config", "curia"), filepath.Join(home, ".local", "share", "curia"),
			filepath.Join(home, ".local", "state", "curia"))
	}
	for _, p := range []string{cfg.DBPath, cfg.LogPath} {
		if p != "" {
			raw = append(raw, filepath.Clean(p), filepath.Clean(p)+"-wal", filepath.Clean(p)+"-shm")
		}
	}
	var roots []string
	for _, r := range raw {
		roots = append(roots, strings.ToLower(r))
		if resolved, ok := walk(r); ok {
			roots = append(roots, strings.ToLower(resolved))
		}
	}
	return roots
}

// targets are the two places path p (relative to cwd) is checked at: the
// cleaned absolute path as written, and where it really lands by one
// fail-closed walk over the UNCLEANED joined path (see walk). Either one
// inside the protected roots denies. ok is false when the walk cannot say:
// longer than maxCheckedPath or any error on the way.
func targets(p, cwd, home string) (found []string, ok bool) {
	if rest, found := strings.CutPrefix(p, "~/"); found && home != "" {
		p = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(p) {
		if !filepath.IsAbs(cwd) { // a hostile cwd leaves the hook's own working directory
			var err error
			if cwd, err = os.Getwd(); err != nil {
				return nil, false
			}
		}
		p = cwd + string(filepath.Separator) + p // not Join: Join would clean ".." away
	}
	if len(p) > maxCheckedPath {
		return nil, false
	}
	resolved, ok := walk(p)
	return []string{filepath.Clean(p), resolved}, ok
}

// maxCheckedPath is the longest path checked; no real edit has a longer one.
const maxCheckedPath = 4096

// maxHops is the most symlinks a walk follows.
const maxHops = 40

// walk resolves the absolute path p component by component from the root:
// "." is skipped; ".." goes to the parent of the resolved path so far; a
// symlink is replaced by its target (relative to the directory so far); the
// first component that does not exist ends the walk, the rest being plain
// names (a ".." among them, or any error, or too many hops, is a failure).
// Limit: a symlink made after the check is not seen (the deny rules in the
// settings file are the backstop).
func walk(p string) (string, bool) {
	cur, hops := string(filepath.Separator), 0
	rest := strings.Split(p, string(filepath.Separator))
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if slices.Contains(rest, "..") {
				return "", false
			}
			return filepath.Join(append([]string{next}, rest...)...), true
		case err != nil:
			return "", false
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(next)
			if hops++; err != nil || hops > maxHops {
				return "", false
			}
			if filepath.IsAbs(target) {
				cur = string(filepath.Separator)
			}
			rest = append(strings.Split(target, string(filepath.Separator)), rest...)
		default:
			cur = next
		}
	}
	return cur, true
}

// inside reports whether p is one of roots or below one.
func inside(p string, roots []string) bool {
	p = strings.ToLower(p)
	return slices.ContainsFunc(roots, func(r string) bool { return p == r || strings.HasPrefix(p, r+string(filepath.Separator)) })
}

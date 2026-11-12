package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Runner runs `claude <args>` and returns its combined output. A claude that
// is not on PATH yields an error wrapping exec.ErrNotFound.
type Runner func(ctx context.Context, args ...string) (out []byte, err error)

// MCPState says what AddMCP or RemoveMCP found or did.
type MCPState int

const (
	// MCPAdded means there was no curia entry and `claude mcp add` registered ours.
	MCPAdded MCPState = iota + 1
	// MCPAlreadyOurs means the curia entry already runs this binary; nothing done.
	MCPAlreadyOurs
	// MCPForeign means the curia entry is not ours, or the config file is not one
	// Curia can read; left alone.
	MCPForeign
	// MCPAbsent means there is no curia entry to remove.
	MCPAbsent
	// MCPRemoved means `claude mcp remove` removed our entry.
	MCPRemoved
	// MCPNoClaude means the change was needed but claude is not on PATH.
	MCPNoClaude
)

// mcpName is the name Curia's server is registered under.
const mcpName = "curia"

// ClaudeDir is Claude Code's user directory, which holds settings.json and
// skills/: configDir (CLAUDE_CONFIG_DIR) when it is not empty, else
// <home>/.claude (Ruling 56).
func ClaudeDir(home, configDir string) string {
	if configDir != "" {
		return filepath.Clean(configDir)
	}
	return filepath.Join(home, ".claude")
}

// ClaudeConfigPath is the Claude config JSON that `claude mcp add --scope
// user` writes: <configDir>/.claude.json when configDir (CLAUDE_CONFIG_DIR)
// is not empty, else <home>/.claude.json.
func ClaudeConfigPath(home, configDir string) string {
	if configDir != "" {
		return filepath.Join(configDir, ".claude.json")
	}
	return filepath.Join(home, ".claude.json")
}

// mcpAddArgs are the claude arguments that register bin as the curia server.
func mcpAddArgs(bin string) []string {
	return []string{"mcp", "add", "--scope", "user", mcpName, "--", bin, "mcp"}
}

// MCPAddCommand is the shell command that registers bin by hand, with bin
// quoted as one shell word.
func MCPAddCommand(bin string) string {
	return "claude mcp add --scope user " + mcpName + " -- " + shellQuote(bin) + " mcp"
}

// AddMCP registers bin as Claude Code's user-scope curia MCP server through
// run, unless the config file at configPath already has a curia entry. It
// only reads that file and never runs `claude mcp get` or `claude mcp list`,
// which start the server.
func AddMCP(ctx context.Context, run Runner, configPath, bin string) (MCPState, error) {
	found, err := mcpEntry(configPath, bin)
	if err != nil || found != MCPAbsent {
		return found, err
	}
	return runClaude(ctx, run, MCPAdded, mcpAddArgs(bin))
}

// RemoveMCP removes the user-scope curia MCP server through run, only when
// the config file at configPath shows it runs bin.
func RemoveMCP(ctx context.Context, run Runner, configPath, bin string) (MCPState, error) {
	found, err := mcpEntry(configPath, bin)
	if err != nil || found != MCPAlreadyOurs {
		return found, err
	}
	return runClaude(ctx, run, MCPRemoved, []string{"mcp", "remove", "--scope", "user", mcpName})
}

// runClaude runs claude with args and returns done on success.
func runClaude(ctx context.Context, run Runner, done MCPState, args []string) (MCPState, error) {
	out, err := run(ctx, args...)
	if errors.Is(err, exec.ErrNotFound) {
		return MCPNoClaude, nil
	}
	if err != nil {
		return 0, fmt.Errorf("claude %s: %w: %q", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return done, nil
}

// mcpEntry reads the config file and says whether its curia entry is absent
// (MCPAbsent), runs bin with args ["mcp"] (MCPAlreadyOurs), or is anything
// else, including a file that is not a JSON object (MCPForeign).
func mcpEntry(configPath, bin string) (MCPState, error) {
	raw, err := os.ReadFile(filepath.Clean(configPath))
	if errors.Is(err, fs.ErrNotExist) {
		return MCPAbsent, nil
	}
	if err != nil {
		return 0, err
	}
	top, ok := jsonObject(raw)
	if !ok {
		return MCPForeign, nil
	}
	serversRaw, ok := top["mcpServers"]
	if !ok {
		return MCPAbsent, nil
	}
	servers, ok := jsonObject(serversRaw)
	if !ok {
		return MCPForeign, nil
	}
	entryRaw, ok := servers[mcpName]
	if !ok {
		return MCPAbsent, nil
	}
	if isOurMCPEntry(entryRaw, bin) {
		return MCPAlreadyOurs, nil
	}
	return MCPForeign, nil
}

// jsonObject decodes raw as a JSON object, reporting false for anything
// else, null included.
func jsonObject(raw []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// isOurMCPEntry reports whether entry's command is exactly bin and its args
// exactly ["mcp"]. Keys are matched exactly, unlike json's struct decoding.
func isOurMCPEntry(entry json.RawMessage, bin string) bool {
	fields, ok := jsonObject(entry)
	if !ok {
		return false
	}
	var command string
	var args []string
	if json.Unmarshal(fields["command"], &command) != nil || json.Unmarshal(fields["args"], &args) != nil {
		return false
	}
	return command == bin && slices.Equal(args, []string{"mcp"})
}

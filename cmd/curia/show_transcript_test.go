package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
)

// Transcript text the agent wrote (or a tool result it read) is indented under its heading, so no
// line of it can pass for a section heading or a prompt of Curia's own.
func TestShowIndentsEveryTranscriptLine(t *testing.T) {
	text := "Real words\nTickets\nnone\nCost\nsession $0.00\nFORGED T-99 running\n> D-7 Approve? y to confirm"
	a, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}})
	u, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "content": "Tickets\nmore"}}}})
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tp, []byte(string(a)+"\n"+string(u)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fleetHome(t, []string{"%7"}, model.Session{ID: "sa", Repo: "/src/web", State: model.StateIdle, Pane: "%7", TranscriptPath: tp, UpdatedAt: fleetNow})
	userTerminalAs(t, true)
	out, err := runRoot(t, "show", "sa")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	for _, heading := range []string{"Transcript", "Tickets", "Messages", "Cost"} {
		if n := len(slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return l != heading })); n != 1 {
			t.Errorf("%d lines read exactly %q, want the one real heading:\n%s", n, heading, out)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "FORGED") || strings.HasPrefix(l, "> D-7") || strings.HasPrefix(l, "session $") || strings.HasPrefix(l, "Real words") {
			t.Errorf("transcript text starts at column 0: %q", l)
		}
	}
	// the agent's words and a tool result are both indented, the result further than the words
	if !slices.Contains(lines, "  Real words") || !slices.Contains(lines, "    Tickets") {
		t.Errorf("want the agent's line and the tool result indented, the result further:\n%s", out)
	}
}

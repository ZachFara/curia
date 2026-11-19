package install

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const finishingParagraph = "## Finishing a task\n\n" +
	"If you are working on a Curia task (your first prompt names it, for example `Task T-42`), finish like this: commit everything, then call `complete_task` with a short summary (what you changed, how you tested it, anything left open) and `tests_status` (`passed`, `failed` or `not_run`), and end your turn. Report the tests honestly: the user sees your report next to the diff. Do not merge, push or open a pull request yourself. If the user asks for changes, their note arrives as a message; make the changes and call `complete_task` again.\n\n"

func TestAllowToolsAddsSixthToAPlan3Install(t *testing.T) {
	path := settingsIn(t)
	five, _ := json.Marshal(append([]string{"Bash(ls:*)"}, wantToolRules[:5]...))
	writeFile(t, path, `{"permissions":{"allow":`+string(five)+`}}`)
	if ToolsAllowed(path) {
		t.Fatal("ToolsAllowed is true with five rules")
	}
	if err := AllowTools(path, testNow); err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{"Bash(ls:*)"}, wantToolRules[:5]...), "mcp__curia__complete_task")
	if got := readAllow(t, path); !slices.Equal(got, want) {
		t.Errorf("allow = %q, want %q", got, want)
	}
	if !ToolsAllowed(path) {
		t.Error("ToolsAllowed is false after AllowTools")
	}
	once := readFile(t, path)
	if err := AllowTools(path, testNow.Add(1)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != once {
		t.Errorf("second AllowTools changed the file:\n%s", got)
	}
	if matches, _ := filepath.Glob(path + ".bak.*"); len(matches) != 1 {
		t.Errorf("backups = %v, want one", matches)
	}
}

func TestUnhooksRemovesTheSixToolRules(t *testing.T) {
	if len(wantToolRules) != 6 || !slices.Equal(toolRules, wantToolRules) {
		t.Fatalf("toolRules = %q, want the six %q", toolRules, wantToolRules)
	}
	path := settingsIn(t)
	allow, _ := json.Marshal(append([]string{"Bash(ls:*)", "mcp__curia"}, wantToolRules...))
	writeFile(t, path, `{"permissions":{"allow":`+string(allow)+`}}`)
	if err := Unhooks(path, testBin); err != nil {
		t.Fatal(err)
	}
	if got := readAllow(t, path); !slices.Equal(got, []string{"Bash(ls:*)", "mcp__curia"}) {
		t.Errorf("allow = %q", got)
	}
}

func TestSkillExplainsFinishing(t *testing.T) {
	skill := string(skillMD)
	for _, want := range []string{
		finishingParagraph + "You cannot answer a ticket",
		"## Messages from the user\n",
		"If you are a teammate in an agent team, bring your questions to your team lead: only the lead raises decision tickets.",
		"\n" + skillMarker + "\n",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("embedded skill lacks %q", want)
		}
	}
	if n := bytes.Count(skillMD, []byte("## Finishing a task")); n != 1 {
		t.Errorf("finishing heading appears %d times", n)
	}
}

func TestInstallBindsNoPlan4Key(t *testing.T) {
	snip := snippet(testBin, keyStates(testBin, defaultKeys))
	for _, word := range []string{"review", "restore", "serve", "ntfy"} {
		if strings.Contains(snip, word) {
			t.Errorf("snippet holds %q:\n%s", word, snip)
		}
	}
}

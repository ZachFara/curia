package ui

import (
	"strings"
	"testing"
)

// The help line is derived from the registered commands, so no key can be
// left out of it (T, S, L, P and n were).
func TestHelpLineListsEveryRegisteredKey(t *testing.T) {
	help := Model{mode: ModePopup}.helpText()
	for _, c := range commands {
		if c.help != "" && !strings.Contains(help, "  "+c.help) {
			t.Errorf("help line %q lacks %q", help, c.help)
		}
	}
	for _, want := range []string{"T timeline", "S settings", "L log", "P pause", "n spawn"} {
		if !strings.Contains(help, want) {
			t.Errorf("help line %q lacks %q", help, want)
		}
	}
}

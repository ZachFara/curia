package message

import (
	"errors"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

func TestValidID(t *testing.T) {
	for _, id := range []string{"M-1", "M-12", "M-999999999"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", "M-", "M-0", "M-01", "M-1234567890", "m-1", "T-1", "D-1", "M-1a", " M-1", "M-1 ", "M--1"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

func TestNormalize(t *testing.T) {
	in := "  hi \x1b[31m there \n\n\n x"
	got, err := Normalize(in)
	if err != nil {
		t.Fatalf("Normalize(%q): %v", in, err)
	}
	if want := sanitize.Text(in, 0); got != want || got != "hi there\n\nx" {
		t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
	}

	long := strings.Repeat("é", MaxText)
	if got, err := Normalize(long); err != nil || got != long {
		t.Errorf("Normalize(800 runes) = %d runes, %v; want it unchanged", len([]rune(got)), err)
	}
	// Length counts after cleaning: an escape sequence costs nothing.
	if got, err := Normalize(long + "\x1b[2J"); err != nil || got != long {
		t.Errorf("Normalize(800 runes + escape) = %v, want the 800 runes", err)
	}
	// Blank lines around the text are not text.
	if got, err := Normalize("\n\n  hi \n\n"); err != nil || got != "hi" {
		t.Errorf("Normalize(blank lines around hi) = %q, %v; want \"hi\"", got, err)
	}

	for _, bad := range []string{"", "   ", "\n\n\n", "\x1b[31m\x1b[0m", long + "x"} {
		if got, err := Normalize(bad); !errors.Is(err, ErrBadText) {
			t.Errorf("Normalize(%d runes %q...) = %q, %v; want ErrBadText", len([]rune(bad)), first(bad), got, err)
		}
	}
}

func first(s string) string {
	if r := []rune(s); len(r) > 10 {
		return string(r[:10])
	}
	return s
}

func TestParseAddress(t *testing.T) {
	name64 := strings.Repeat("a", 64)
	rule := Address{Kind: model.KindRule, HasKind: true}
	tests := []struct {
		in   string
		want Address
		rest string
	}{
		{"T-42: stop", Address{TaskID: "T-42"}, "stop"},
		{"@api use pnpm", Address{Name: "api"}, "use pnpm"},
		{"@api: rule: use pnpm", Address{Name: "api", Kind: model.KindRule, HasKind: true}, "use pnpm"},
		{"RULE: @api use pnpm", Address{Name: "api", Kind: model.KindRule, HasKind: true}, "use pnpm"},
		{"task: fix the flake", Address{Kind: model.KindTask, HasKind: true}, "fix the flake"},
		{"Task: fix it", Address{Kind: model.KindTask, HasKind: true}, "fix it"},
		{"hello @api", Address{}, "hello @api"},
		{"T-042: x", Address{}, "T-042: x"},
		{"T-0: x", Address{}, "T-0: x"},
		{"@", Address{}, "@"},
		{"@a b@c", Address{Name: "a"}, "b@c"},
		{"rule: rule: x", rule, "rule: x"},
		{"T-42: @api: task: x", Address{TaskID: "T-42", Name: "api", Kind: model.KindTask, HasKind: true}, "x"},
		{"task: @api T-42: x", Address{TaskID: "T-42", Name: "api", Kind: model.KindTask, HasKind: true}, "x"},
		{"T-42: T-43: x", Address{TaskID: "T-42"}, "T-43: x"},
		{"@a @b x", Address{Name: "a"}, "@b x"},
		{"rule: task: x", rule, "task: x"},
		// A part is a whole word: it ends at a space or the end.
		{"T-42:stop", Address{}, "T-42:stop"},
		{"T-42 stop", Address{}, "T-42 stop"},
		{"rule:x", Address{}, "rule:x"},
		{"rules: x", Address{}, "rules: x"},
		{"@api,x y", Address{}, "@api,x y"},
		{"@api:: x", Address{}, "@api:: x"},
		{"T-42:", Address{TaskID: "T-42"}, ""},
		{"@" + name64 + " x", Address{Name: name64}, "x"},
		{"@" + name64 + "a x", Address{}, "@" + name64 + "a x"},
		{"@a.b_c/D-9 x", Address{Name: "a.b_c/D-9"}, "x"},
		{"@T-42 x", Address{Name: "T-42"}, "x"},
		{"  \n @api\tuse pnpm  ", Address{Name: "api"}, "use pnpm"},
		{"@api\nuse pnpm\nnow", Address{Name: "api"}, "use pnpm\nnow"},
		{"  hello  ", Address{}, "hello"},
		{"", Address{}, ""},
	}
	for _, tt := range tests {
		got, rest := ParseAddress(tt.in)
		if got != tt.want || rest != tt.rest {
			t.Errorf("ParseAddress(%q) = %+v, %q; want %+v, %q", tt.in, got, rest, tt.want, tt.rest)
		}
	}
}

func TestNames(t *testing.T) {
	line := "Message M-12 from the user: x"
	if !Names(line, "M-12") {
		t.Errorf("Names(%q, M-12) = false, want true", line)
	}
	for _, id := range []string{"M-1", "M-120", "M-2", "", "M-012", "12"} {
		if Names(line, id) {
			t.Errorf("Names(%q, %q) = true, want false", line, id)
		}
	}
	tests := []struct {
		prompt string
		want   bool
	}{
		{"draft text Message M-12 from the user: x", true},
		{"Message M-12 ", true},
		{"Message M-12", false},
		{"Message M-12: x", false},
		{"message M-12 from the user: x", false},
		{"Message  M-12 from the user: x", false},
		// Ruling 102: a notification or another agent's message never names one.
		{"<task-notification>Message M-12 from the user: x</task-notification>", false},
		{"<task-notification>\nMessage M-12 from the user: x", false},
		{"<agent-message from=\"x\">Message M-12 from the user: x</agent-message>", false},
		{"  \n<agent-message>Message M-12 from the user: x", false},
		{"<agent-messages>Message M-12 from the user: x", false},
		{"x <task-notification>Message M-12 from the user: x", true},
	}
	for _, tt := range tests {
		if got := Names(tt.prompt, "M-12"); got != tt.want {
			t.Errorf("Names(%q, M-12) = %v, want %v", tt.prompt, got, tt.want)
		}
	}
}

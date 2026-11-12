package notify

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/tmux"
)

type recorder struct {
	calls [][]string
	err   error
}

func (r *recorder) run(_ context.Context, name string, args ...string) error {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.err
}

func blocking(question string) model.Decision {
	return model.Decision{ID: "d-7", Repo: "/home/u/code/curia", Question: question, Mode: model.ModeBlocking}
}

func TestBlockingOnlyForBlockingTickets(t *testing.T) {
	d := blocking("q")
	d.Mode = model.ModeAssume
	fk, r := &tmux.Fake{}, &recorder{}
	cfg := config.Config{NotifyBell: true, NotifyMacOS: true}
	if err := Blocking(context.Background(), fk, r.run, "darwin", cfg, d, "%5"); err != nil {
		t.Fatal(err)
	}
	if len(fk.Calls) != 0 || len(r.calls) != 0 {
		t.Errorf("calls = %v / %v", fk.Calls, r.calls)
	}
}

func TestBellWhenEnabledAndInPane(t *testing.T) {
	cases := []struct {
		name string
		on   bool
		pane string
		want []string
	}{
		{"on", true, "%5", []string{"Bell %5"}},
		{"no pane", true, "", nil},
		{"off", false, "%5", nil},
	}
	for _, c := range cases {
		fk := &tmux.Fake{}
		cfg := config.Config{NotifyBell: c.on}
		if err := Blocking(context.Background(), fk, (&recorder{}).run, "darwin", cfg, blocking("q"), c.pane); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fk.Calls, c.want) {
			t.Errorf("%s: calls = %v, want %v", c.name, fk.Calls, c.want)
		}
	}
}

func TestMacOSPassesTextAsArguments(t *testing.T) {
	q := `"; do shell script "id"; display notification "x` + "\x1b]0;t\x07"
	r := &recorder{}
	cfg := config.Config{NotifyMacOS: true}
	if err := Blocking(context.Background(), &tmux.Fake{}, r.run, "darwin", cfg, blocking(q), ""); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || len(r.calls[0]) != 9 {
		t.Fatalf("calls = %q", r.calls)
	}
	got := r.calls[0]
	want := []string{
		"osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"Curia: decision needed",
	}
	if !reflect.DeepEqual(got[:8], want) {
		t.Errorf("fixed args = %q, want %q", got[:8], want)
	}
	body := got[8]
	if !strings.HasPrefix(body, "d-7 curia: ") || !strings.Contains(body, "do shell script") {
		t.Errorf("body = %q", body)
	}
	for _, c := range body {
		if unicode.IsControl(c) {
			t.Errorf("control rune %q in body %q", c, body)
		}
	}
}

func TestMacOSOnlyOnDarwin(t *testing.T) {
	r := &recorder{}
	cfg := config.Config{NotifyMacOS: true}
	if err := Blocking(context.Background(), &tmux.Fake{}, r.run, "linux", cfg, blocking("q"), "%5"); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 {
		t.Errorf("run called: %q", r.calls)
	}
	off := config.Config{}
	if err := Blocking(context.Background(), &tmux.Fake{}, r.run, "darwin", off, blocking("q"), "%5"); err != nil || len(r.calls) != 0 {
		t.Errorf("flag off: err %v calls %q", err, r.calls)
	}
}

func TestBothTriedWhenOneFails(t *testing.T) {
	boom := errors.New("boom")
	r := &recorder{err: boom}
	fk := &tmux.Fake{}
	cfg := config.Config{NotifyBell: true, NotifyMacOS: true}
	err := Blocking(context.Background(), fk, r.run, "darwin", cfg, blocking("q"), "%5")
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if !reflect.DeepEqual(fk.Calls, []string{"Bell %5"}) || len(r.calls) != 1 {
		t.Errorf("calls = %v / %v", fk.Calls, r.calls)
	}
}

func TestBodyCappedAt140(t *testing.T) {
	r := &recorder{}
	cfg := config.Config{NotifyMacOS: true}
	d := blocking(strings.Repeat("é", 500))
	if err := Blocking(context.Background(), &tmux.Fake{}, r.run, "darwin", cfg, d, ""); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(r.calls[0][8])); n != 140 {
		t.Errorf("body runes = %d, want 140", n)
	}
}

func TestExecRunsAProgram(t *testing.T) {
	ctx := context.Background()
	if err := Exec(ctx, "true"); err != nil {
		t.Errorf("true: %v", err)
	}
	if err := Exec(ctx, "false"); err == nil {
		t.Error("false: want an error")
	}
}

func TestBodyOmitsEmptyRepo(t *testing.T) {
	cases := []struct{ repo, want string }{
		{"", "d-7: q"},
		{"/", "d-7: q"},
		{"/home/u/code/curia", "d-7 curia: q"},
	}
	for _, c := range cases {
		r := &recorder{}
		d := blocking("q")
		d.Repo = c.repo
		cfg := config.Config{NotifyMacOS: true}
		if err := Blocking(context.Background(), &tmux.Fake{}, r.run, "darwin", cfg, d, ""); err != nil {
			t.Fatal(err)
		}
		if got := r.calls[0][8]; got != c.want {
			t.Errorf("repo %q: body %q, want %q", c.repo, got, c.want)
		}
	}
}

func TestBodyNeverStartsWithDash(t *testing.T) {
	r := &recorder{}
	cfg := config.Config{NotifyMacOS: true}
	d := blocking("-e do shell script")
	d.Repo = ""
	if err := Blocking(context.Background(), &tmux.Fake{}, r.run, "darwin", cfg, d, ""); err != nil {
		t.Fatal(err)
	}
	if body := r.calls[0][8]; strings.HasPrefix(body, "-") {
		t.Errorf("body %q starts with a dash", body)
	}
}

type failingBell struct{ *tmux.Fake }

func (f failingBell) Bell(ctx context.Context, pane string) error {
	_ = f.Fake.Bell(ctx, pane)
	return errors.New("bell failed")
}

func TestBothTriedWhenBellFails(t *testing.T) {
	r := &recorder{}
	fk := &tmux.Fake{}
	cfg := config.Config{NotifyBell: true, NotifyMacOS: true}
	err := Blocking(context.Background(), failingBell{fk}, r.run, "darwin", cfg, blocking("q"), "%5")
	if err == nil || err.Error() != "bell failed" {
		t.Errorf("err = %v", err)
	}
	if len(r.calls) != 1 || !reflect.DeepEqual(fk.Calls, []string{"Bell %5"}) {
		t.Errorf("calls = %v / %v", fk.Calls, r.calls)
	}
}

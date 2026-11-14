package tmux

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Fake is an in-memory Client for tests of packages that use tmux. Set Panes
// before use, Focused for PaneFocused's answer, SendKeysErr for SendKeys'
// error, Screen, Synchronized and CaptureErr for CapturePane's answer,
// NewWindowErr for NewWindow's error and Tail for CaptureTail's answer;
// every call is appended to Calls as "Method arg arg ...".
type Fake struct {
	Panes        []Pane
	Focused      bool
	SendKeysErr  error
	Screen       string
	Synchronized bool
	CaptureErr   error
	NewWindowErr error
	Tail         string
	Calls        []string
	windows      int // windows NewWindow has opened
}

var _ Client = (*Fake)(nil)

func (f *Fake) record(format string, args ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

// ListPanes returns a copy of Panes.
func (f *Fake) ListPanes(context.Context) ([]Pane, error) {
	f.record("ListPanes")
	return slices.Clone(f.Panes), nil
}

// PaneInfo returns the pane with the given id, or an error if absent.
func (f *Fake) PaneInfo(_ context.Context, pane string) (Pane, error) {
	f.record("PaneInfo %s", pane)
	for _, p := range f.Panes {
		if p.ID == pane {
			return p, nil
		}
	}
	return Pane{}, fmt.Errorf("fake tmux: no pane %q", pane)
}

// PaneFocused records the call and returns Focused.
func (f *Fake) PaneFocused(_ context.Context, pane string) (bool, error) {
	f.record("PaneFocused %s", pane)
	return f.Focused, nil
}

// SwitchTo records the call.
func (f *Fake) SwitchTo(_ context.Context, pane string) error {
	f.record("SwitchTo %s", pane)
	return nil
}

// SetWindowOption records the call.
func (f *Fake) SetWindowOption(_ context.Context, window, name, value string) error {
	f.record("SetWindowOption %s %s %s", window, name, value)
	return nil
}

// DisplayMessage records the call.
func (f *Fake) DisplayMessage(_ context.Context, text string) error {
	f.record("DisplayMessage %s", text)
	return nil
}

// KillPane records the call and removes the pane from Panes.
func (f *Fake) KillPane(_ context.Context, pane string) error {
	f.record("KillPane %s", pane)
	f.Panes = slices.DeleteFunc(f.Panes, func(p Pane) bool { return p.ID == pane })
	return nil
}

// SendKeys records the call and returns SendKeysErr. It types nothing and
// does not check its arguments. Set SendKeysErr to ErrPartialSend (or an
// error wrapping it) to act out a line typed but not submitted.
func (f *Fake) SendKeys(_ context.Context, pane, text string) error {
	f.record("SendKeys %s %s", pane, text)
	return f.SendKeysErr
}

// CapturePane records the call and returns Screen, Synchronized and
// CaptureErr, for any pane.
func (f *Fake) CapturePane(_ context.Context, pane string) (Capture, error) {
	f.record("CapturePane %s", pane)
	return Capture{Screen: f.Screen, Synchronized: f.Synchronized}, f.CaptureErr
}

// Bell records the call.
func (f *Fake) Bell(_ context.Context, pane string) error {
	f.record("Bell %s", pane)
	return nil
}

// NewWindow records the call and returns NewWindowErr when it is set.
// Otherwise it appends a pane %<100 + n>, n counting the windows it opened
// before, with the window's session and name to Panes and returns it. It does
// not check its argument.
func (f *Fake) NewWindow(_ context.Context, w WindowSpec) (Pane, error) {
	f.record("NewWindow %s %s %s %s %s", w.Session, w.Name, w.Dir, strings.Join(w.Env, ","), strings.Join(w.Command, " "))
	if f.NewWindowErr != nil {
		return Pane{}, f.NewWindowErr
	}
	p := Pane{ID: fmt.Sprintf("%%%d", 100+f.windows), Session: w.Session, WindowName: w.Name}
	f.windows++
	f.Panes = append(f.Panes, p)
	return p, nil
}

// CaptureTail records the call and returns Tail, for any pane.
func (f *Fake) CaptureTail(_ context.Context, pane string, lines int) (string, error) {
	f.record("CaptureTail %s %d", pane, lines)
	return f.Tail, nil
}

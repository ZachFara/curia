// Package tmux drives tmux through a small interface so that the rest of
// Curia can be tested without a tmux server.
package tmux

import (
	"context"
	"errors"
	"time"
)

// Pane identifies one tmux pane and the window and session that hold it.
type Pane struct {
	ID       string
	Session  string
	WindowID string
	// ServerStart is when the tmux server holding the pane started, to the
	// second. A restarted server numbers its panes from %0 again, so a pane
	// id names the same pane only for sessions started after this.
	ServerStart time.Time
	WindowName  string
}

// Client is everything Curia needs from tmux.
type Client interface {
	// ListPanes returns every pane on the server.
	ListPanes(ctx context.Context) ([]Pane, error)
	// PaneInfo returns the pane with the given id.
	PaneInfo(ctx context.Context, pane string) (Pane, error)
	// PaneFocused reports whether the pane is the active pane of the active
	// window of a session with an attached client.
	PaneFocused(ctx context.Context, pane string) (bool, error)
	// SwitchTo moves the attached client to the pane.
	SwitchTo(ctx context.Context, pane string) error
	// SetWindowOption sets a window option (or @user option) on a window.
	SetWindowOption(ctx context.Context, window, name, value string) error
	// DisplayMessage shows text literally in the client's status line.
	DisplayMessage(ctx context.Context, text string) error
	// KillPane closes the pane.
	KillPane(ctx context.Context, pane string) error
	// SendKeys types text literally into the pane, then Enter. It is the
	// only method that types into a pane, and it refuses a pane that is not
	// a pane id and text that is empty, one character, over 800 characters,
	// not UTF-8, or holds a control character. An error wrapping
	// ErrPartialSend means the text may be in the prompt, unsubmitted or even
	// submitted; the caller must not retry by typing again. Any other error
	// means nothing was typed, as far as tmux reported.
	SendKeys(ctx context.Context, pane, text string) error
	// CapturePane returns the visible text of the pane, one line per row
	// and no escape sequences. It refuses a pane that is not a pane id.
	CapturePane(ctx context.Context, pane string) (string, error)
	// Bell rings the pane's bell.
	Bell(ctx context.Context, pane string) error
}

// ErrNoWindow is wrapped by an error for a window that does not exist.
var ErrNoWindow = errors.New("tmux: no such window")

// ErrPartialSend is wrapped by a SendKeys error when the text may have
// reached the pane: the Enter step failed for any reason, or the text step
// failed while the context was done. The line may be in the prompt
// unsubmitted, or may even have been submitted; do not type it again.
var ErrPartialSend = errors.New("tmux: text may have been typed")

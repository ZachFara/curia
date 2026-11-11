// Package tmux drives tmux through a small interface so that the rest of
// Curia can be tested without a tmux server.
package tmux

import "context"

// Pane identifies one tmux pane and the window and session that hold it.
type Pane struct {
	ID         string
	Session    string
	WindowID   string
	WindowName string
}

// Client is everything Curia needs from tmux.
type Client interface {
	// ListPanes returns every pane on the server.
	ListPanes(ctx context.Context) ([]Pane, error)
	// PaneInfo returns the pane with the given id.
	PaneInfo(ctx context.Context, pane string) (Pane, error)
	// SwitchTo moves the attached client to the pane.
	SwitchTo(ctx context.Context, pane string) error
	// SetWindowOption sets a window option (or @user option) on a window.
	SetWindowOption(ctx context.Context, window, name, value string) error
	// DisplayMessage shows text literally in the client's status line.
	DisplayMessage(ctx context.Context, text string) error
	// KillPane closes the pane.
	KillPane(ctx context.Context, pane string) error
}

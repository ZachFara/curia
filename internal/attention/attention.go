// Package attention decides which tmux windows carry an attention marker and
// applies it, so the user can see which session needs them.
package attention

import (
	"context"
	"fmt"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// markerOption is the tmux window option the status format reads.
const markerOption = "@curia_marker"

// markerRed marks a session that needs the user and has not been looked at.
const markerRed = "red"

// Marker returns "red" when s waits for the user (needs_decision or waiting)
// and SeenAt is before UpdatedAt (a zero SeenAt counts as before); otherwise "".
func Marker(s model.Session) string {
	if s.State != model.StateNeedsDecision && s.State != model.StateWaiting {
		return ""
	}
	if s.SeenAt.Before(s.UpdatedAt) {
		return markerRed
	}
	return ""
}

// WindowMarker returns the strongest marker among the sessions of one window.
func WindowMarker(sessions []model.Session) string {
	for _, s := range sessions {
		if Marker(s) == markerRed {
			return markerRed
		}
	}
	return ""
}

// Apply sets the @curia_marker option of window (a tmux window id) to the
// WindowMarker of the window's live sessions: those in the store with that
// TmuxWindow whose state is neither ended nor stale.
func Apply(ctx context.Context, tm tmux.Client, st *store.Store, window string) error {
	all, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return fmt.Errorf("attention: list sessions: %w", err)
	}
	var live []model.Session
	for _, s := range all {
		if s.TmuxWindow == window && s.State != model.StateStale {
			live = append(live, s)
		}
	}
	if err := tm.SetWindowOption(ctx, window, markerOption, WindowMarker(live)); err != nil {
		return fmt.Errorf("attention: set marker on %s: %w", window, err)
	}
	return nil
}

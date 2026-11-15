// Package attention decides which tmux windows carry an attention marker and
// applies it, so the user can see which session needs them.
package attention

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZachFara/curia/internal/fleet"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// markerOption is the tmux window option the status format reads.
const markerOption = "@curia_marker"

// markerRed marks a session that needs the user and has not been looked at;
// markerBlue one whose task waits for review and that changed since.
const (
	markerRed  = "red"
	markerBlue = "blue"
)

// Marker returns "red" when s waits for the user (needs_decision or waiting)
// and SeenAt is before UpdatedAt (a zero SeenAt counts as before); else
// "blue" when s.ReviewPending is set, s is neither ended nor stale, and
// SeenAt is before UpdatedAt; otherwise "".
func Marker(s model.Session) string {
	if !s.SeenAt.Before(s.UpdatedAt) {
		return ""
	}
	switch {
	case s.State == model.StateNeedsDecision || s.State == model.StateWaiting:
		return markerRed
	case s.ReviewPending && s.State != model.StateEnded && s.State != model.StateStale:
		return markerBlue
	}
	return ""
}

// WindowMarker returns the strongest marker among the sessions of one
// window: red when any session is red, else blue when any is blue.
func WindowMarker(sessions []model.Session) string {
	marker := ""
	for _, s := range sessions {
		switch Marker(s) {
		case markerRed:
			return markerRed
		case markerBlue:
			marker = markerBlue
		}
	}
	return marker
}

// Apply sets the @curia_marker option of window (a tmux window id) to the
// WindowMarker of the window's live sessions: those in the store with that
// TmuxWindow whose state is neither ended nor stale, marked with the tasks
// that have a pending review (store.PendingReviewTasks). When that read
// fails the marker is applied without reviews and the read's error is
// returned. An empty window does nothing.
func Apply(ctx context.Context, tm tmux.Client, st *store.Store, window string) error {
	if window == "" {
		return nil // a session outside tmux has no window
	}
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
	pending, rerr := st.PendingReviewTasks(ctx)
	if rerr == nil {
		live = fleet.WithReviews(live, pending)
	}
	if err := tm.SetWindowOption(ctx, window, markerOption, WindowMarker(live)); err != nil {
		return errors.Join(fmt.Errorf("attention: set marker on %s: %w", window, err), readErr(rerr))
	}
	return readErr(rerr)
}

// readErr wraps the error of the pending reviews read, or is nil.
func readErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("attention: marker applied without reviews: %w", err)
}

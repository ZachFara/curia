// Package fleet is the view over every session at once: it marks sessions
// whose pane is gone as stale, ranks sessions by how much they need the user,
// and builds the status-bar badge.
package fleet

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// Reconcile marks stale, with UpdatedAt now, every session that is neither
// ended nor already stale and whose non-empty Pane is not among live. A
// session with no pane (started outside tmux) is never marked stale. Each
// change goes through store.UpdateSession and re-checks the stored row,
// because a hook may have moved or ended the session since it was listed.
func Reconcile(ctx context.Context, st *store.Store, live []tmux.Pane, now time.Time) error {
	alive := make(map[string]bool, len(live))
	for _, p := range live {
		alive[p.ID] = true
	}
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return fmt.Errorf("fleet: reconcile: %w", err)
	}
	for _, s := range sessions {
		if !goneFrom(s, s.Pane, alive) {
			continue
		}
		pane := s.Pane
		err := st.UpdateSession(ctx, s.ID, func(cur model.Session, _ bool) model.Session {
			// Sessions are never deleted, so the row is found; if it were
			// not, cur has an empty ID and UpdateSession refuses to write it.
			if goneFrom(cur, pane, alive) {
				cur.State, cur.UpdatedAt = model.StateStale, now
			}
			return cur
		})
		if err != nil {
			return fmt.Errorf("fleet: reconcile: %w", err)
		}
	}
	return nil
}

// goneFrom reports whether s is still a live session in pane and pane is not
// alive, that is, whether s should become stale.
func goneFrom(s model.Session, pane string, alive map[string]bool) bool {
	return s.Pane != "" && s.Pane == pane && !alive[pane] &&
		s.State != model.StateEnded && s.State != model.StateStale
}

// Rank groups, in order: needs_decision, waiting, idle-unseen, working,
// idle-seen, stale, then any state it does not know. Within a group the
// oldest UpdatedAt comes first. Rank returns a new slice; s is unchanged.
func Rank(s []model.Session) []model.Session {
	out := slices.Clone(s)
	slices.SortStableFunc(out, func(a, b model.Session) int {
		return cmp.Or(cmp.Compare(group(a), group(b)), a.UpdatedAt.Compare(b.UpdatedAt))
	})
	return out
}

// Rank groups, in order. groupJump is the last group Next jumps to.
const (
	groupNeedsDecision = iota
	groupWaiting
	groupIdleUnseen
	groupWorking
	groupIdleSeen
	groupStale
	groupUnknown

	groupJump = groupIdleUnseen
)

func group(s model.Session) int {
	switch s.State {
	case model.StateNeedsDecision:
		return groupNeedsDecision
	case model.StateWaiting:
		return groupWaiting
	case model.StateIdle:
		if unseen(s) {
			return groupIdleUnseen
		}
		return groupIdleSeen
	case model.StateWorking:
		return groupWorking
	case model.StateStale:
		return groupStale
	default:
		return groupUnknown
	}
}

// unseen reports whether s changed since the user last focused its pane. A
// session never focused (zero SeenAt) is unseen.
func unseen(s model.Session) bool {
	return s.SeenAt.IsZero() || s.SeenAt.Before(s.UpdatedAt)
}

// Counts is how many sessions are in each state the badge shows.
type Counts struct {
	Decisions, Waiting, Reviews int
}

// Count counts sessions waiting on a decision and waiting for input,
// whether or not they have been seen. Reviews stays 0 until Plan 4.
func Count(s []model.Session) Counts {
	var c Counts
	for _, x := range s {
		switch x.State {
		case model.StateNeedsDecision:
			c.Decisions++
		case model.StateWaiting:
			c.Waiting++
		}
	}
	return c
}

// Badge renders c as "D<n> W<n> R<n>", leaving out zero parts; it is the
// empty string when every count is zero.
func Badge(c Counts) string {
	var parts []string
	for _, p := range []struct {
		letter string
		n      int
	}{{"D", c.Decisions}, {"W", c.Waiting}, {"R", c.Reviews}} {
		if p.n != 0 {
			parts = append(parts, fmt.Sprintf("%s%d", p.letter, p.n))
		}
	}
	return strings.Join(parts, " ")
}

// Next returns the first ranked session that has a pane and is
// needs_decision, waiting or idle-unseen, and false if there is none.
func Next(s []model.Session) (model.Session, bool) {
	for _, x := range Rank(s) {
		if x.Pane != "" && group(x) <= groupJump {
			return x, true
		}
	}
	return model.Session{}, false
}

package decision

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// Queue ranks of Triage, in order.
const (
	rankWarm   = iota // blocking, session cache known and still warm
	rankOther         // blocking, cache cold or unknown
	rankAssume        // assume
)

// warm reports whether t's session cache is known and expires after now.
func warm(t model.Ticket, now time.Time) bool {
	return !t.CacheExpiresAt.IsZero() && t.CacheExpiresAt.After(now)
}

func rank(t model.Ticket, now time.Time) int {
	switch {
	case t.Mode == model.ModeAssume:
		return rankAssume
	case warm(t, now):
		return rankWarm
	default:
		return rankOther
	}
}

// Triage returns open tickets in queue order (plan section 4.8): blocking
// tickets with a warm cache, soonest expiry first; then every other
// blocking ticket, oldest first; then assume tickets, oldest first; ties by
// n. The input is not changed.
func Triage(t []model.Ticket, now time.Time) []model.Ticket {
	out := slices.Clone(t)
	slices.SortStableFunc(out, func(a, b model.Ticket) int {
		ra, rb := rank(a, now), rank(b, now)
		if c := cmp.Compare(ra, rb); c != 0 {
			return c
		}
		if ra == rankWarm {
			if c := a.CacheExpiresAt.Compare(b.CacheExpiresAt); c != 0 {
				return c
			}
		} else if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.N, b.N)
	})
	return out
}

// Stakes is the line shown under a ticket about its session's prompt
// cache: how long it stays warm and what a rebuild costs, how much a cold
// cache re-writes, or "" when the cache is unknown.
func Stakes(t model.Ticket, now time.Time) string {
	switch {
	case t.CacheExpiresAt.IsZero():
		return ""
	case warm(t, now):
		mins := max(1, int(t.CacheExpiresAt.Sub(now)/time.Minute))
		s := fmt.Sprintf("answer within %dm to keep cache warm", mins)
		if cents := math.Round(t.CacheRebuildUSD * 100); cents > 0 {
			s += fmt.Sprintf(" (rebuild ≈ $%.2f)", cents/100)
		}
		return s
	case t.ContextUsed > 0:
		k := max(1, int(math.Round(float64(t.ContextUsed)/1000)))
		return fmt.Sprintf("cache cold: next turn re-writes ~%dk tokens", k)
	default:
		return "cache cold"
	}
}

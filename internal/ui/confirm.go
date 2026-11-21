package ui

import "time"

// confirmGate is the rule every "y confirms" prompt shares (the dashboard's
// pause, the board's spawn, the decide view's irreversible answer, the
// review view's approve and drop, the settings change): a y counts only
// once the prompt has been open for wait (dwell, then quietPeriod after a
// refusal) and not within burstGap of another key (a paste the terminal did
// not bracket). A prompt that also waits out burstGap after the y
// (taken, late, due) refuses a y that a following key shows came in a burst.
type confirmGate struct {
	since     time.Time     // when the prompt opened, or a refused y restarted the wait
	wait      time.Duration // how long a y waits from since
	keyAt     time.Time     // when the last key came, acted on or not
	prevKeyAt time.Time     // when the key before it came
	confirmAt time.Time     // when a y was taken, waiting out burstGap; zero when none
	seq       int           // the confirm message the taken y waits for
}

// open starts the wait of a prompt that opens at now.
func (g *confirmGate) open(now time.Time) { g.since, g.wait = now, dwell }

// keyed records that a key came at now, for the burst rule.
func (g *confirmGate) keyed(now time.Time) { g.prevKeyAt, g.keyAt = g.keyAt, now }

// waiting reports whether a taken y waits out burstGap.
func (g *confirmGate) waiting() bool { return !g.confirmAt.IsZero() }

// refuse restarts the wait, now as long as the quiet period, so the repeats
// of a held key never confirm (a held key's first repeat comes 375 to 660 ms
// after the press).
func (g *confirmGate) refuse(now time.Time) { g.since, g.wait = now, quietPeriod }

// soon reports whether a y at now is too soon or within burstGap of the key
// before it.
func (g *confirmGate) soon(now time.Time) bool {
	return now.Sub(g.since) < g.wait || inBurst(now, g.prevKeyAt)
}

// take notes a y that passed soon and returns the seq its confirm message
// carries, to come once burstGap has passed.
func (g *confirmGate) take(now time.Time) int {
	g.confirmAt, g.seq = now, g.seq+1
	return g.seq
}

// late handles a key that came while a taken y waits: within burstGap of the
// y it arrived with it, so the y is refused (and true is returned); later
// the confirm message is late and the y stands.
func (g *confirmGate) late() bool {
	if !inBurst(g.keyAt, g.confirmAt) {
		return false
	}
	g.confirmAt = time.Time{}
	g.refuse(g.keyAt)
	return true
}

// due reports whether the confirm message seq is the one the taken y waits for.
func (g *confirmGate) due(seq int) bool { return seq == g.seq && g.waiting() }

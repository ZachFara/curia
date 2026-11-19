// Package usage holds Curia's pure usage rules: the ingest contract, the
// status-line mapping, the cost-lens summary reader, and the forecast, burn,
// attribution, pause suggestion and cells of forecast.go. It does no I/O.
package usage

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// Forecast windows and horizons (plan section 4.4).
const (
	// BurnWindow is how far back samples count toward a burn.
	BurnWindow = 20 * time.Minute
	// MinBurnSpan is the least time between a series' first and last sample
	// for a burn to be measured. A burst of readings (up to about three a
	// second in a busy turn) spans far less, so it is never mistaken for a
	// trend.
	MinBurnSpan = 3 * time.Minute
	// APIHorizon is how far ahead APIProjection projects spend.
	APIHorizon = 2 * time.Hour
)

const (
	// forecastMaxAhead caps how far past its last sample a LimitAt lands, so
	// a vanishing rate cannot overflow a time.Duration.
	forecastMaxAhead = 366 * 24 * time.Hour
	// burnMaxUSD bounds a believable running cost; larger, negative or
	// non-finite readings are dropped, so every sum stays finite.
	burnMaxUSD = 1e9
)

// Forecast is one usage window as of its newest sample in the burn window.
type Forecast struct {
	Window   model.UsageWindow
	UsedPct  float64
	ResetsAt time.Time // zero: unknown
	PerHour  float64   // percentage points per hour; 0 when no burn could be measured
	LimitAt  time.Time // zero when PerHour is 0 (except at 100%, when it is the last sample's time)
	Warn     bool      // the limit lands before the reset, and the reset is still ahead
}

// Forecasts returns one Forecast per window that has a sample in [now-BurnWindow, now], five_hour first.
// It does not change samples and does not assume they are sorted. A sample
// whose percentage is not a finite number in [0, 100] is ignored.
func Forecasts(samples []model.UsageSample, now time.Time) []Forecast {
	out := []Forecast{}
	for _, w := range []model.UsageWindow{model.WindowFiveHour, model.WindowSevenDay} {
		if s := forecastSeries(samples, w, now); len(s) > 0 {
			out = append(out, forecastOf(s, w, now))
		}
	}
	return out
}

// forecastSeries is window w's samples in the burn window, ordered by time
// then n, cut to the longest suffix whose percentage never decreases (a
// decrease means the window reset in between).
func forecastSeries(samples []model.UsageSample, w model.UsageWindow, now time.Time) []model.UsageSample {
	start := now.Add(-BurnWindow)
	var s []model.UsageSample
	for _, x := range samples {
		if x.Window == w && !x.At.Before(start) && !x.At.After(now) && forecastValidPct(x.UsedPct) {
			s = append(s, x)
		}
	}
	slices.SortStableFunc(s, func(a, b model.UsageSample) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		return cmp.Compare(a.N, b.N)
	})
	i := len(s) - 1
	for i > 0 && s[i-1].UsedPct <= s[i].UsedPct {
		i--
	}
	return s[max(i, 0):]
}

func forecastValidPct(p float64) bool {
	return !math.IsNaN(p) && p >= 0 && p <= 100
}

// forecastOf computes the Forecast of a non-empty series.
func forecastOf(s []model.UsageSample, w model.UsageWindow, now time.Time) Forecast {
	first, last := s[0], s[len(s)-1]
	f := Forecast{Window: w, UsedPct: last.UsedPct, ResetsAt: last.ResetsAt}
	if span := last.At.Sub(first.At); len(s) >= 2 && span >= MinBurnSpan {
		if rate := (last.UsedPct - first.UsedPct) / span.Hours(); rate > 0 {
			f.PerHour = rate
			f.LimitAt = last.At.Add(forecastAhead((100 - last.UsedPct) / rate))
		}
	}
	if last.UsedPct >= 100 {
		f.LimitAt = last.At
	}
	f.Warn = forecastWarns(f, now)
	return f
}

// forecastAhead turns hours to the limit into a duration, capped.
func forecastAhead(hours float64) time.Duration {
	if hours >= forecastMaxAhead.Hours() {
		return forecastMaxAhead
	}
	return time.Duration(max(hours, 0) * float64(time.Hour))
}

// forecastWarns: the limit lands before a known reset that is still ahead.
func forecastWarns(f Forecast, now time.Time) bool {
	return !f.LimitAt.IsZero() && f.ResetsAt.After(now) && f.LimitAt.Before(f.ResetsAt)
}

// Warning returns the warning forecast whose limit is soonest.
// A forecast whose reset is no longer ahead of now does not count.
func Warning(f []Forecast, now time.Time) (Forecast, bool) {
	var best Forecast
	found := false
	for _, x := range f {
		if !x.Warn || !forecastWarns(x, now) {
			continue
		}
		if !found || x.LimitAt.Before(best.LimitAt) {
			best, found = x, true
		}
	}
	return best, found
}

// BadgeText is "5h 62% → limit ~48m (resets 1h50m)" for a warning forecast,
// "5h 100% → at limit (resets 1h50m)" when LimitAt is not after now. A
// forecast that does not warn makes no limit claim: "5h 62% (resets 30m)".
func BadgeText(f Forecast, now time.Time) string {
	b := forecastHead(f)
	if f.Warn && !f.LimitAt.IsZero() {
		b += " →" + forecastLimit(f, now)
	}
	if f.ResetsAt.After(now) {
		b += " (resets " + Short(f.ResetsAt.Sub(now)) + ")"
	}
	return b
}

// Line is one line of curia usage for f: "5h 62% · resets 1h50m · burning 51%/h → limit ~48m", or "… · steady".
// The limit is named only for a warning forecast; a burn whose limit lands
// after the reset (or with no reset known) is "… · burning 51%/h".
func Line(f Forecast, now time.Time) string {
	parts := []string{forecastHead(f)}
	if f.ResetsAt.After(now) {
		parts = append(parts, "resets "+Short(f.ResetsAt.Sub(now)))
	}
	switch {
	case f.PerHour > 0:
		burn := "burning " + forecastRate(f.PerHour)
		if f.Warn && !f.LimitAt.IsZero() {
			burn += " →" + forecastLimit(f, now)
		}
		parts = append(parts, burn)
	case f.Warn && !f.LimitAt.IsZero():
		parts = append(parts, strings.TrimSpace(forecastLimit(f, now)))
	default:
		parts = append(parts, "steady")
	}
	return strings.Join(parts, " · ")
}

// forecastHead is "5h 62%": the window's label and the rounded percentage.
func forecastHead(f Forecast) string {
	label := "?"
	switch f.Window {
	case model.WindowFiveHour:
		label = "5h"
	case model.WindowSevenDay:
		label = "7d"
	}
	if !forecastValidPct(f.UsedPct) {
		return label + " ?%"
	}
	return fmt.Sprintf("%s %d%%", label, int(math.Round(f.UsedPct)))
}

// forecastLimit is " at limit" or " limit ~48m".
func forecastLimit(f Forecast, now time.Time) string {
	if !f.LimitAt.After(now) {
		return " at limit"
	}
	return " limit ~" + Short(f.LimitAt.Sub(now))
}

// forecastRate is "51%/h", "<1%/h" below half a point, "9999+%/h" at most.
func forecastRate(r float64) string {
	v := math.Round(r)
	switch {
	case math.IsNaN(v) || v < 1:
		return "<1%/h"
	case v > 9999:
		return "9999+%/h"
	}
	return fmt.Sprintf("%d%%/h", int(v))
}

// Short renders a duration as "48m", "1h50m" or "3d4h"; never less than "1m".
// Minutes and hours are truncated; at most 10 characters ("106751d23h").
func Short(d time.Duration) string {
	if d < time.Minute {
		return "1m"
	}
	m := int64(d / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	h := m / 60
	if h < 24 {
		return fmt.Sprintf("%dh%dm", h, m%60)
	}
	return fmt.Sprintf("%dd%dh", h/24, h%24)
}

// Burns is each session's cost burn over the burn window, in USD per hour.
// Every session with a cost sample in the window has an entry; one with a
// single sample, or whose cost fell, burns 0.
func Burns(cost []model.CostSample, now time.Time) map[string]float64 {
	out := map[string]float64{}
	for id, d := range burnDeltas(cost, now) {
		out[id] = d * float64(time.Hour) / float64(BurnWindow)
	}
	return out
}

// burnDeltas is, per session with a sample in [now-BurnWindow, now], its
// cost at its last sample in the window minus its cost at its last sample at
// or before the window's start (or its first in the window when none is
// before), never negative. Samples after now and unbelievable readings are
// ignored.
func burnDeltas(cost []model.CostSample, now time.Time) map[string]float64 {
	start := now.Add(-BurnWindow)
	by := map[string][]model.CostSample{}
	for _, c := range cost {
		if c.SessionID == "" || c.At.After(now) || math.IsNaN(c.CostUSD) || c.CostUSD < 0 || c.CostUSD > burnMaxUSD {
			continue
		}
		by[c.SessionID] = append(by[c.SessionID], c)
	}
	out := map[string]float64{}
	for id, s := range by {
		slices.SortStableFunc(s, func(a, b model.CostSample) int {
			if c := a.At.Compare(b.At); c != 0 {
				return c
			}
			return cmp.Compare(a.N, b.N)
		})
		end := s[len(s)-1]
		if end.At.Before(start) {
			continue
		}
		base := s[0]
		for _, c := range s {
			if c.At.After(start) {
				break
			}
			base = c
		}
		out[id] = max(end.CostUSD-base.CostUSD, 0)
	}
	return out
}

// APIProjection is the spend of the next APIHorizon at the current rate across the live sessions.
// ok is false when usage has a sample in the burn window (a subscription) or cost has none.
func APIProjection(usage []model.UsageSample, cost []model.CostSample, live map[string]bool, now time.Time) (usd float64, ok bool) {
	start := now.Add(-BurnWindow)
	for _, u := range usage {
		if !u.At.Before(start) && !u.At.After(now) {
			return 0, false
		}
	}
	deltas := burnDeltas(cost, now)
	if len(deltas) == 0 {
		return 0, false
	}
	sum := 0.0
	for id, d := range deltas {
		if live[id] {
			sum += d
		}
	}
	return sum * float64(APIHorizon) / float64(BurnWindow), true
}

// Running is one running task, for SuggestPause.
type Running struct {
	TaskID   string
	N        int64
	Priority int     // 1 to 3; 0 counts as 2
	BurnUSD  float64 // per hour, summed over its sessions
}

// SuggestPause picks the task to pause: the lowest priority, then the highest burn, then the newest.
// A priority outside 1 to 3 counts as 2; a burn that is not a finite,
// non-negative number counts as 0. The caller asks only while Warning
// reports a warning, and only the user acts on the answer.
func SuggestPause(r []Running) (taskID string, ok bool) {
	var best Running
	for _, x := range r {
		if x.TaskID != "" && (!ok || pauseBefore(x, best)) {
			best, ok = x, true
		}
	}
	return best.TaskID, ok
}

func pauseBefore(a, b Running) bool {
	if pa, pb := pausePriority(a.Priority), pausePriority(b.Priority); pa != pb {
		return pa > pb
	}
	if ba, bb := pauseBurn(a.BurnUSD), pauseBurn(b.BurnUSD); ba != bb {
		return ba > bb
	}
	if a.N != b.N {
		return a.N > b.N
	}
	return a.TaskID < b.TaskID
}

func pausePriority(p int) int {
	if p < model.PriorityHigh || p > model.PriorityLow {
		return model.PriorityNormal
	}
	return p
}

func pauseBurn(usd float64) float64 {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return 0
	}
	return usd
}

// CostCell renders a cost for a cell: "$4.12", "$0.07", whole dollars from
// $100 ("$123"), "$99999+" above that (at most 7 characters). Blank when
// unknown, negative or not a finite number, never "$0.00" for unknown.
func CostCell(usd float64, known bool) string {
	if !known || math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return ""
	}
	if cents := math.Round(usd * 100); cents < 10000 {
		return fmt.Sprintf("$%.2f", cents/100)
	}
	if dollars := math.Round(usd); dollars <= 99999 {
		return fmt.Sprintf("$%d", int64(dollars))
	}
	return "$99999+"
}

// CacheCell renders a session's prompt cache for the dashboard, only for an
// idle, waiting or needs_decision session (blank otherwise): "12m" while warm
// (at least "1m"), "cold $1.68" once the expiry has passed and a rebuild cost
// of at least a cent is known, else "cold 85k tok" when the tokens a rebuild
// re-writes are known (Ruling P5-R2: never an invented dollar figure), else
// "cold", and blank when the cache state is unknown. At most 14 characters.
func CacheCell(s model.Session, now time.Time) string {
	switch s.State {
	case model.StateIdle, model.StateWaiting, model.StateNeedsDecision:
	default:
		return ""
	}
	switch {
	case s.CacheExpiresAt.IsZero():
		return ""
	case s.CacheExpiresAt.After(now):
		return Short(s.CacheExpiresAt.Sub(now))
	}
	known := !math.IsNaN(s.CacheRebuildUSD) && math.Round(s.CacheRebuildUSD*100) > 0
	if c := CostCell(s.CacheRebuildUSD, known); c != "" {
		return "cold " + c
	}
	// Above a billion every count reads ">999M tok", so the clamp changes no
	// text and keeps the conversion exact on a 32-bit int.
	if t := TokensCell(int(min(s.CacheRecacheTokens, 1<<30)), s.CacheRecacheTokens > 0); t != "" {
		return "cold " + t
	}
	return "cold"
}

// TokensCell renders a token count compactly, for views that know a cache
// rebuild in tokens but not in dollars (Ruling P5-R2): "999 tok", "8.4k tok",
// "85k tok", "1.3M tok", "999M tok", ">999M tok" (at most 9 characters).
// Blank when unknown or negative.
func TokensCell(tokens int, known bool) string {
	if !known || tokens < 0 {
		return ""
	}
	if tokens < 1000 {
		return fmt.Sprintf("%d tok", tokens)
	}
	for _, u := range []struct {
		div  float64
		unit string
	}{{1e3, "k"}, {1e6, "M"}} {
		v := float64(tokens) / u.div
		if tenths := math.Round(v * 10); tenths < 100 {
			return fmt.Sprintf("%.1f%s tok", tenths/10, u.unit)
		}
		if whole := math.Round(v); whole < 1000 {
			return fmt.Sprintf("%d%s tok", int(whole), u.unit)
		}
	}
	return ">999M tok"
}

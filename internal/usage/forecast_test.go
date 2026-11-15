package usage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
)

var fcNow = time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)

// fcAt is fcNow shifted by d (negative for the past).
func fcAt(d time.Duration) time.Time { return fcNow.Add(d) }

// fcSeries builds five_hour samples, one per (ago, pct) pair, all with reset.
func fcSeries(w model.UsageWindow, reset time.Time, pairs ...any) []model.UsageSample {
	var out []model.UsageSample
	for i := 0; i+1 < len(pairs); i += 2 {
		ago, _ := pairs[i].(time.Duration)
		pct, _ := pairs[i+1].(float64)
		out = append(out, model.UsageSample{
			N: int64(len(out) + 1), At: fcAt(-ago), Window: w, UsedPct: pct, ResetsAt: reset, SessionID: "s1",
		})
	}
	return out
}

// fcSpec is the spec example: 50, 56, 62 at now-15m, now-8m, now-1m.
func fcSpec(reset time.Time) []model.UsageSample {
	return fcSeries(model.WindowFiveHour, reset, 15*time.Minute, 50.0, 8*time.Minute, 56.0, time.Minute, 62.0)
}

func fcOne(t *testing.T, samples []model.UsageSample, now time.Time) Forecast {
	t.Helper()
	f := Forecasts(samples, now)
	if len(f) != 1 {
		t.Fatalf("Forecasts = %+v, want exactly one", f)
	}
	return f[0]
}

func fcNear(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

func fcNearTime(got, want time.Time, tol time.Duration) bool {
	d := got.Sub(want)
	return d <= tol && d >= -tol
}

func TestForecastSpecExample(t *testing.T) {
	f := fcOne(t, fcSpec(fcAt(110*time.Minute)), fcNow)
	if f.Window != model.WindowFiveHour || f.UsedPct != 62 || !f.ResetsAt.Equal(fcAt(110*time.Minute)) {
		t.Fatalf("forecast = %+v", f)
	}
	if !fcNear(f.PerHour, 12/(14.0/60), 1e-9) || !fcNear(f.PerHour, 51.4, 0.05) {
		t.Errorf("PerHour = %v, want ≈ 51.4", f.PerHour)
	}
	if !fcNearTime(f.LimitAt, fcAt(43*time.Minute+20*time.Second), time.Second) {
		t.Errorf("LimitAt = %v, want ≈ now+43m20s", f.LimitAt.Sub(fcNow))
	}
	if !f.Warn {
		t.Error("Warn = false, want true")
	}
	if got := BadgeText(f, fcNow); got != "5h 62% → limit ~43m (resets 1h50m)" {
		t.Errorf("BadgeText = %q", got)
	}
	if got := Line(f, fcNow); got != "5h 62% · resets 1h50m · burning 51%/h → limit ~43m" {
		t.Errorf("Line = %q", got)
	}
	if w, ok := Warning([]Forecast{f}, fcNow); !ok || w != f {
		t.Errorf("Warning = %+v, %v", w, ok)
	}
}

func TestForecastNoWarnWhenResetComesFirst(t *testing.T) {
	f := fcOne(t, fcSpec(fcAt(30*time.Minute)), fcNow)
	if f.Warn || !fcNear(f.PerHour, 51.4, 0.05) || f.LimitAt.IsZero() {
		t.Fatalf("forecast = %+v, want the burn measured and no warning", f)
	}
	if _, ok := Warning([]Forecast{f}, fcNow); ok {
		t.Error("Warning reports a warning; the reset comes first")
	}
	// No limit claim when the reset comes first.
	if got := Line(f, fcNow); got != "5h 62% · resets 30m · burning 51%/h" {
		t.Errorf("Line = %q", got)
	}
	if got := BadgeText(f, fcNow); strings.Contains(got, "limit") {
		t.Errorf("BadgeText = %q claims a limit without a warning", got)
	}
}

func TestForecastNeedsSpan(t *testing.T) {
	reset := fcAt(110 * time.Minute)
	w := model.WindowFiveHour
	cases := []struct {
		name    string
		samples []model.UsageSample
		present bool
	}{
		{"one sample", fcSeries(w, reset, 5*time.Minute, 60.0), true},
		{"two samples 2m59s apart", fcSeries(w, reset, 5*time.Minute, 60.0, 2*time.Minute+time.Second, 80.0), true},
		{"two samples one second apart", fcSeries(w, reset, 2*time.Second, 60.0, time.Second, 61.0), true},
		{"a burst of three in one second", fcSeries(w, reset, 1000*time.Millisecond, 60.0, 600*time.Millisecond, 61.0, 300*time.Millisecond, 62.0), true},
		{"three samples all older than 20m", fcSeries(w, reset, 40*time.Minute, 10.0, 30*time.Minute, 20.0, 21*time.Minute, 30.0), false},
		{"span reaches back past the window", fcSeries(w, reset, 30*time.Minute, 10.0, 2*time.Minute, 20.0, time.Minute, 21.0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Forecasts(c.samples, fcNow)
			if !c.present {
				if len(got) != 0 {
					t.Fatalf("Forecasts = %+v, want none", got)
				}
				return
			}
			f := fcOne(t, c.samples, fcNow)
			if f.PerHour != 0 || !f.LimitAt.IsZero() || f.Warn {
				t.Errorf("forecast = %+v, want no burn and no warning", f)
			}
		})
	}
}

func TestForecastExactlyMinSpan(t *testing.T) {
	s := fcSeries(model.WindowFiveHour, fcAt(time.Hour), 4*time.Minute, 60.0, time.Minute, 63.0)
	f := fcOne(t, s, fcNow)
	if !fcNear(f.PerHour, 60, 1e-9) {
		t.Errorf("PerHour = %v, want 60 (3 points over exactly 3m)", f.PerHour)
	}
	// 37 points at 60/h from now-1m: now+36m, before the reset in 1h.
	if !f.Warn || !fcNearTime(f.LimitAt, fcAt(36*time.Minute), time.Second) {
		t.Errorf("forecast = %+v", f)
	}
}

func TestForecastBurstIsOneSample(t *testing.T) {
	// A burst at now-10m and a burst at now-8m: the span is about 2m, under the minimum.
	reset := fcAt(time.Hour)
	s := fcSeries(model.WindowFiveHour, reset,
		10*time.Minute+700*time.Millisecond, 60.0, 10*time.Minute+300*time.Millisecond, 60.0, 10*time.Minute, 61.0,
		8*time.Minute+400*time.Millisecond, 70.0, 8*time.Minute, 70.0)
	if f := fcOne(t, s, fcNow); f.PerHour != 0 || f.Warn {
		t.Errorf("forecast = %+v, two bursts 2m apart must not give a burn", f)
	}
}

func TestForecastSteadyIsNoWarning(t *testing.T) {
	s := fcSeries(model.WindowFiveHour, fcAt(time.Hour), 15*time.Minute, 62.0, 8*time.Minute, 62.0, 0*time.Minute, 62.0)
	f := fcOne(t, s, fcNow)
	if f.PerHour != 0 || !f.LimitAt.IsZero() || f.Warn {
		t.Fatalf("forecast = %+v", f)
	}
	if got := Line(f, fcNow); !strings.HasSuffix(got, "· steady") || got != "5h 62% · resets 1h0m · steady" {
		t.Errorf("Line = %q", got)
	}
}

func TestForecastCutsAtReset(t *testing.T) {
	s := fcSeries(model.WindowFiveHour, fcAt(2*time.Hour),
		18*time.Minute, 90.0, 15*time.Minute, 95.0, 12*time.Minute, 2.0, 8*time.Minute, 4.0, 4*time.Minute, 6.0)
	f := fcOne(t, s, fcNow)
	// The series is 2, 4, 6 over 8 minutes: 30 points per hour; the limit
	// (about now+3h4m) lands after the reset in 2h.
	if f.UsedPct != 6 || !fcNear(f.PerHour, 30, 1e-9) || f.Warn {
		t.Errorf("forecast = %+v, want UsedPct 6, PerHour 30, no warning", f)
	}
	// Only one sample after the reset: no burn, even though the samples span 14m.
	s = fcSeries(model.WindowFiveHour, fcAt(4*time.Hour), 15*time.Minute, 90.0, 10*time.Minute, 95.0, time.Minute, 1.0)
	if f := fcOne(t, s, fcNow); f.UsedPct != 1 || f.PerHour != 0 || f.Warn {
		t.Errorf("forecast = %+v, want UsedPct 1 and no burn", f)
	}
}

func TestForecastMergesSessions(t *testing.T) {
	sorted := fcSeries(model.WindowFiveHour, fcAt(4*time.Hour),
		18*time.Minute, 90.0, 15*time.Minute, 95.0, 12*time.Minute, 2.0, 8*time.Minute, 4.0, 4*time.Minute, 6.0)
	want := fcOne(t, sorted, fcNow)
	mixed := slices.Clone(sorted)
	for i := range mixed {
		mixed[i].SessionID = []string{"s1", "s2", "s3"}[i%3]
	}
	mixed[0], mixed[4] = mixed[4], mixed[0]
	mixed[1], mixed[3] = mixed[3], mixed[1]
	if got := fcOne(t, mixed, fcNow); got != want {
		t.Errorf("merged = %+v, want %+v", got, want)
	}
	// Every sample twice (the same reading ingested twice): the same Forecast.
	doubled := append(slices.Clone(mixed), mixed...)
	if got := fcOne(t, doubled, fcNow); got != want {
		t.Errorf("duplicated = %+v, want %+v", got, want)
	}
}

func TestForecastSameInstantOrdersByN(t *testing.T) {
	// Two readings in the same instant: N decides which is newer.
	at := fcAt(-time.Minute)
	s := fcSeries(model.WindowFiveHour, fcAt(time.Hour), 10*time.Minute, 40.0)
	s = append(s,
		model.UsageSample{N: 9, At: at, Window: model.WindowFiveHour, UsedPct: 50, ResetsAt: fcAt(time.Hour)},
		model.UsageSample{N: 8, At: at, Window: model.WindowFiveHour, UsedPct: 48, ResetsAt: fcAt(time.Hour)})
	f := fcOne(t, s, fcNow)
	if f.UsedPct != 50 || !fcNear(f.PerHour, 10/(9.0/60), 1e-9) {
		t.Errorf("forecast = %+v, want the N=9 reading last", f)
	}
}

func TestForecastDoesNotChangeInput(t *testing.T) {
	s := fcSpec(fcAt(time.Hour))
	s[0], s[2] = s[2], s[0]
	before := slices.Clone(s)
	Forecasts(s, fcNow)
	if !slices.Equal(s, before) {
		t.Errorf("input changed: %+v, was %+v", s, before)
	}
}

func TestForecastWithoutResetTimeNeverWarns(t *testing.T) {
	f := fcOne(t, fcSpec(time.Time{}), fcNow)
	if !fcNear(f.PerHour, 51.4, 0.05) || f.Warn || !f.ResetsAt.IsZero() {
		t.Fatalf("forecast = %+v, want PerHour set and no warning", f)
	}
	if _, ok := Warning([]Forecast{f}, fcNow); ok {
		t.Error("Warning without a reset time")
	}
	if got := Line(f, fcNow); got != "5h 62% · burning 51%/h" {
		t.Errorf("Line = %q", got)
	}
	// Only the last sample's reset counts.
	s := fcSpec(fcAt(time.Hour))
	s[2].ResetsAt = time.Time{}
	if f := fcOne(t, s, fcNow); f.Warn {
		t.Errorf("forecast = %+v warns though its last sample has no reset", f)
	}
}

func TestForecastResetAlreadyPast(t *testing.T) {
	// The reset time of the readings is behind now: the window has reset since.
	f := fcOne(t, fcSpec(fcAt(-30*time.Second)), fcNow)
	if f.Warn {
		t.Fatalf("forecast = %+v warns with a past reset", f)
	}
	if _, ok := Warning([]Forecast{f}, fcNow); ok {
		t.Error("Warning with a past reset")
	}
	if got := Line(f, fcNow); got != "5h 62% · burning 51%/h" {
		t.Errorf("Line = %q", got)
	}
	// A warning computed earlier stops counting once its reset is behind now.
	warn := fcOne(t, fcSpec(fcAt(time.Hour)), fcNow)
	if _, ok := Warning([]Forecast{warn}, fcAt(time.Hour)); ok {
		t.Error("Warning still reported at the reset time")
	}
}

func TestForecastAtLimit(t *testing.T) {
	reset := fcAt(110 * time.Minute)
	s := fcSeries(model.WindowFiveHour, reset, 10*time.Minute, 90.0, time.Minute, 100.0)
	f := fcOne(t, s, fcNow)
	if !f.Warn || !f.LimitAt.Equal(fcAt(-time.Minute)) || f.UsedPct != 100 {
		t.Fatalf("forecast = %+v", f)
	}
	if got := BadgeText(f, fcNow); got != "5h 100% → at limit (resets 1h50m)" {
		t.Errorf("BadgeText = %q", got)
	}
	// At 100% the reading itself says so: no burn is needed.
	for _, s := range [][]model.UsageSample{
		fcSeries(model.WindowFiveHour, reset, time.Minute, 100.0),
		fcSeries(model.WindowFiveHour, reset, 10*time.Minute, 100.0, time.Minute, 100.0),
	} {
		f := fcOne(t, s, fcNow)
		if !f.Warn || f.PerHour != 0 || !f.LimitAt.Equal(fcAt(-time.Minute)) {
			t.Errorf("forecast = %+v, want at limit", f)
		}
		if got := Line(f, fcNow); got != "5h 100% · resets 1h50m · at limit" {
			t.Errorf("Line = %q", got)
		}
	}
	// At 100% but the reset has passed: no warning.
	if f := fcOne(t, fcSeries(model.WindowFiveHour, fcAt(-time.Second), time.Minute, 100.0), fcNow); f.Warn {
		t.Errorf("forecast = %+v warns after its reset", f)
	}
}

func TestForecastAtZero(t *testing.T) {
	s := fcSeries(model.WindowFiveHour, fcAt(time.Hour), 15*time.Minute, 0.0, 5*time.Minute, 0.0)
	if f := fcOne(t, s, fcNow); f.UsedPct != 0 || f.PerHour != 0 || f.Warn {
		t.Errorf("forecast = %+v", f)
	}
	s = fcSeries(model.WindowFiveHour, fcAt(time.Hour), 15*time.Minute, 0.0, 5*time.Minute, 10.0)
	f := fcOne(t, s, fcNow)
	// 10 points in 10 minutes: 60 per hour, 90 to go from now-5m: now+1h25m, after the reset.
	if !fcNear(f.PerHour, 60, 1e-9) || f.Warn || !fcNearTime(f.LimitAt, fcAt(85*time.Minute), time.Second) {
		t.Errorf("forecast = %+v", f)
	}
}

func TestForecastPastLimitAt(t *testing.T) {
	// 50 to 90 in 10 minutes: 240 per hour; 10 to go from now-5m: LimitAt now-2m30s.
	s := fcSeries(model.WindowFiveHour, fcAt(time.Hour), 15*time.Minute, 50.0, 5*time.Minute, 90.0)
	f := fcOne(t, s, fcNow)
	if !f.Warn || !f.LimitAt.Before(fcNow) {
		t.Fatalf("forecast = %+v, want a LimitAt before now and a warning", f)
	}
	for _, got := range []string{BadgeText(f, fcNow), Line(f, fcNow)} {
		if !strings.Contains(got, "at limit") || strings.Contains(got, "-") || strings.Contains(got, "~") {
			t.Errorf("text = %q, want \"at limit\" and no negative duration", got)
		}
	}
	if got := BadgeText(f, fcNow); got != "5h 90% → at limit (resets 1h0m)" {
		t.Errorf("BadgeText = %q", got)
	}
	if got := Line(f, fcNow); got != "5h 90% · resets 1h0m · burning 240%/h → at limit" {
		t.Errorf("Line = %q", got)
	}
}

func TestForecastNegativeBurnNeverWarns(t *testing.T) {
	// The newest reading is lower: a reset; what remains is one sample.
	s := fcSeries(model.WindowFiveHour, fcAt(time.Hour), 15*time.Minute, 60.0, 10*time.Minute, 70.0, time.Minute, 50.0)
	f := fcOne(t, s, fcNow)
	if f.PerHour != 0 || f.Warn || f.UsedPct != 50 || !f.LimitAt.IsZero() {
		t.Errorf("forecast = %+v", f)
	}
}

func TestForecastClockBackwards(t *testing.T) {
	s := fcSpec(fcAt(time.Hour))
	// now earlier than every sample: none is in the window.
	if got := Forecasts(s, fcAt(-30*time.Minute)); len(got) != 0 {
		t.Errorf("Forecasts = %+v, want none", got)
	}
	// now between the samples: only those at or before it count (50 and 56).
	f := fcOne(t, s, fcAt(-5*time.Minute))
	if f.UsedPct != 56 || !fcNear(f.PerHour, 6/(7.0/60), 1e-9) {
		t.Errorf("forecast = %+v, want the two samples at or before now", f)
	}
}

func TestForecastIgnoresBadReadings(t *testing.T) {
	s := fcSpec(fcAt(110 * time.Minute))
	want := fcOne(t, s, fcNow)
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -5, 150} {
		with := append(slices.Clone(s), model.UsageSample{
			N: 99, At: fcAt(-30 * time.Second), Window: model.WindowFiveHour, UsedPct: bad, ResetsAt: fcAt(110 * time.Minute),
		})
		if got := fcOne(t, with, fcNow); got != want {
			t.Errorf("with %v: %+v, want %+v", bad, got, want)
		}
	}
	// A window Curia does not know is ignored.
	other := append(slices.Clone(s), model.UsageSample{N: 98, At: fcAt(-time.Minute), Window: "one_hour", UsedPct: 99})
	if got := fcOne(t, other, fcNow); got != want {
		t.Errorf("with an unknown window: %+v", got)
	}
}

func TestForecastHugeGaps(t *testing.T) {
	reset := fcAt(7 * 24 * time.Hour)
	// The first sample exactly at the window's start still counts.
	s := fcSeries(model.WindowSevenDay, reset, BurnWindow, 50.0, 0*time.Minute, 60.0)
	if f := fcOne(t, s, fcNow); !fcNear(f.PerHour, 30, 1e-9) {
		t.Errorf("forecast = %+v, want the sample at now-20m counted", f)
	}
	// Years later the samples are far outside the window.
	if got := Forecasts(s, fcNow.Add(3*365*24*time.Hour)); len(got) != 0 {
		t.Errorf("Forecasts = %+v, want none", got)
	}
	// A vanishing rate cannot overflow LimitAt.
	s = fcSeries(model.WindowSevenDay, reset, 19*time.Minute, 50.0, 0*time.Minute, 50.0000000001)
	f := fcOne(t, s, fcNow)
	if f.PerHour <= 0 || f.LimitAt.Before(fcNow) || f.Warn || f.LimitAt.Sub(fcNow) > 400*24*time.Hour {
		t.Errorf("forecast = %+v (LimitAt in %v)", f, f.LimitAt.Sub(fcNow))
	}
	if got := Line(f, fcNow); got != "7d 50% · resets 7d0h · burning <1%/h" {
		t.Errorf("Line = %q", got)
	}
}

func TestWarningPicksSoonest(t *testing.T) {
	five := Forecast{Window: model.WindowFiveHour, UsedPct: 70, ResetsAt: fcAt(3 * time.Hour), PerHour: 30, LimitAt: fcAt(48 * time.Minute), Warn: true}
	seven := Forecast{Window: model.WindowSevenDay, UsedPct: 95, ResetsAt: fcAt(48 * time.Hour), PerHour: 10, LimitAt: fcAt(30 * time.Minute), Warn: true}
	w, ok := Warning([]Forecast{five, seven}, fcNow)
	if !ok || w != seven {
		t.Fatalf("Warning = %+v, %v; want seven_day", w, ok)
	}
	if got := BadgeText(w, fcNow); !strings.HasPrefix(got, "7d ") || got != "7d 95% → limit ~30m (resets 2d0h)" {
		t.Errorf("BadgeText = %q", got)
	}
	five.LimitAt = fcAt(20 * time.Minute)
	if w, _ := Warning([]Forecast{five, seven}, fcNow); w != five {
		t.Errorf("Warning = %+v, want five_hour", w)
	}
	seven.Warn = false
	five.Warn = false
	if _, ok := Warning([]Forecast{five, seven}, fcNow); ok {
		t.Error("Warning with no warning forecast")
	}
	if _, ok := Warning(nil, fcNow); ok {
		t.Error("Warning(nil)")
	}
}

func TestForecastsOrderAndAbsence(t *testing.T) {
	seven := fcSeries(model.WindowSevenDay, fcAt(48*time.Hour), 10*time.Minute, 70.0)
	got := Forecasts(seven, fcNow)
	if len(got) != 1 || got[0].Window != model.WindowSevenDay {
		t.Fatalf("Forecasts = %+v, want one seven_day", got)
	}
	for _, in := range [][]model.UsageSample{nil, {}} {
		if empty := Forecasts(in, fcNow); empty == nil || len(empty) != 0 {
			t.Errorf("Forecasts(%v) = %#v, want empty and not nil", in, empty)
		}
	}
	both := append(slices.Clone(seven), fcSeries(model.WindowFiveHour, fcAt(time.Hour), 5*time.Minute, 20.0)...)
	got = Forecasts(both, fcNow)
	if len(got) != 2 || got[0].Window != model.WindowFiveHour || got[1].Window != model.WindowSevenDay {
		t.Errorf("Forecasts = %+v, want five_hour first", got)
	}
}

func TestShort(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{20 * time.Second, "1m"},
		{0, "1m"},
		{-time.Hour, "1m"},
		{math.MinInt64, "1m"},
		{time.Minute, "1m"},
		{59*time.Minute + 59*time.Second, "59m"},
		{48*time.Minute + 10*time.Second, "48m"},
		{time.Hour, "1h0m"},
		{110 * time.Minute, "1h50m"},
		{2 * time.Hour, "2h0m"},
		{24*time.Hour - time.Second, "23h59m"},
		{24 * time.Hour, "1d0h"},
		{76 * time.Hour, "3d4h"},
		{7 * 24 * time.Hour, "7d0h"},
		{math.MaxInt64, "106751d23h"},
	}
	for _, c := range cases {
		if got := Short(c.in); got != c.want {
			t.Errorf("Short(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func fcCost(id string, ago time.Duration, usd float64, n int64) model.CostSample {
	return model.CostSample{N: n, At: fcAt(-ago), SessionID: id, CostUSD: usd}
}

func TestBurns(t *testing.T) {
	cost := []model.CostSample{
		fcCost("s1", 2*time.Minute, 4.00, 2),
		fcCost("s1", 25*time.Minute, 3.00, 1),
		fcCost("s2", 5*time.Minute, 7.00, 3),
		fcCost("s3", 10*time.Minute, 4.00, 4),
		fcCost("s3", 3*time.Minute, 1.00, 5),
		fcCost("old", 30*time.Minute, 1.00, 6),
		fcCost("old", 25*time.Minute, 2.00, 7),
	}
	got := Burns(cost, fcNow)
	want := map[string]float64{"s1": 3.00, "s2": 0, "s3": 0}
	if len(got) != len(want) {
		t.Fatalf("Burns = %v, want %v", got, want)
	}
	for id, w := range want {
		if g, ok := got[id]; !ok || !fcNear(g, w, 1e-9) {
			t.Errorf("Burns[%s] = %v (%v), want %v", id, g, ok, w)
		}
	}
}

func TestBurnsEdges(t *testing.T) {
	cases := []struct {
		name string
		cost []model.CostSample
		want float64
	}{
		// The baseline is the last sample at or before the window's start.
		{"baseline at the start", []model.CostSample{fcCost("s", 30*time.Minute, 1, 1), fcCost("s", BurnWindow, 2, 2), fcCost("s", time.Minute, 3, 3)}, 3},
		// No sample before the window: the first in the window is the baseline.
		{"first in window", []model.CostSample{fcCost("s", 15*time.Minute, 2, 1), fcCost("s", 10*time.Minute, 2.5, 2), fcCost("s", time.Minute, 3, 3)}, 3},
		{"unsorted", []model.CostSample{fcCost("s", time.Minute, 3, 3), fcCost("s", 30*time.Minute, 1, 1), fcCost("s", 10*time.Minute, 2, 2)}, 6},
		{"duplicated", []model.CostSample{fcCost("s", 30*time.Minute, 1, 1), fcCost("s", 30*time.Minute, 1, 1), fcCost("s", time.Minute, 2, 2), fcCost("s", time.Minute, 2, 2)}, 3},
		{"future sample ignored", []model.CostSample{fcCost("s", 10*time.Minute, 1, 1), fcCost("s", time.Minute, 2, 2), fcCost("s", -5*time.Minute, 50, 3)}, 3},
		{"bad readings ignored", []model.CostSample{fcCost("s", 10*time.Minute, 1, 1), fcCost("s", 5*time.Minute, math.NaN(), 2), fcCost("s", 4*time.Minute, math.Inf(1), 3), fcCost("s", 3*time.Minute, -9, 4), fcCost("s", time.Minute, 2, 5)}, 3},
		{"negative and huge newest readings ignored", []model.CostSample{fcCost("s", 10*time.Minute, 1, 1), fcCost("s", 2*time.Minute, 2, 2), fcCost("s", time.Minute, -9, 3), fcCost("s", 30*time.Second, 5e12, 4)}, 3},
		{"only a sample at the start", []model.CostSample{fcCost("s", BurnWindow, 2, 1)}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Burns(c.cost, fcNow)
			if g, ok := got["s"]; !ok || !fcNear(g, c.want, 1e-9) {
				t.Errorf("Burns = %v, want s: %v", got, c.want)
			}
		})
	}
	if got := Burns(nil, fcNow); got == nil || len(got) != 0 {
		t.Errorf("Burns(nil) = %#v, want empty", got)
	}
}

func TestAPIProjection(t *testing.T) {
	cost := []model.CostSample{
		fcCost("s1", 30*time.Minute, 2.00, 1),
		fcCost("s1", time.Minute, 3.00, 2),
		fcCost("s2", 15*time.Minute, 1.00, 3),
		fcCost("s2", 2*time.Minute, 1.50, 4),
	}
	// A usage sample older than the window does not make it a subscription.
	usage := fcSeries(model.WindowFiveHour, fcAt(time.Hour), 25*time.Minute, 10.0)
	usd, ok := APIProjection(usage, cost, map[string]bool{"s1": true, "s2": true}, fcNow)
	if !ok || !fcNear(usd, 9.00, 1e-9) {
		t.Errorf("APIProjection = %v, %v; want 9.00, true", usd, ok)
	}
	usd, ok = APIProjection(usage, cost, map[string]bool{"s1": true}, fcNow)
	if !ok || !fcNear(usd, 6.00, 1e-9) {
		t.Errorf("APIProjection without s2 = %v, %v; want 6.00, true", usd, ok)
	}
	usd, ok = APIProjection(nil, cost, nil, fcNow)
	if !ok || usd != 0 {
		t.Errorf("APIProjection with no live session = %v, %v; want 0, true", usd, ok)
	}
}

func TestAPIProjectionOffOnSubscription(t *testing.T) {
	cost := []model.CostSample{fcCost("s1", 10*time.Minute, 1, 1), fcCost("s1", time.Minute, 2, 2)}
	live := map[string]bool{"s1": true}
	usage := fcSeries(model.WindowSevenDay, fcAt(time.Hour), 19*time.Minute, 10.0)
	if usd, ok := APIProjection(usage, cost, live, fcNow); ok || usd != 0 {
		t.Errorf("with a usage sample = %v, %v; want off", usd, ok)
	}
	if usd, ok := APIProjection(nil, nil, live, fcNow); ok || usd != 0 {
		t.Errorf("with no cost sample = %v, %v; want off", usd, ok)
	}
	old := []model.CostSample{fcCost("s1", 30*time.Minute, 1, 1)}
	if usd, ok := APIProjection(nil, old, live, fcNow); ok || usd != 0 {
		t.Errorf("with only an old cost sample = %v, %v; want off", usd, ok)
	}
}

func TestSuggestPause(t *testing.T) {
	cases := []struct {
		name string
		in   []Running
		want string
	}{
		{"priority first", []Running{{TaskID: "T-1", N: 1, Priority: 2, BurnUSD: 5}, {TaskID: "T-2", N: 2, Priority: 3, BurnUSD: 1}}, "T-2"},
		{"then burn", []Running{{TaskID: "T-1", N: 1, Priority: 3, BurnUSD: 1}, {TaskID: "T-2", N: 2, Priority: 3, BurnUSD: 4}, {TaskID: "T-3", N: 3, Priority: 1, BurnUSD: 9}}, "T-2"},
		{"then newest", []Running{{TaskID: "T-7", N: 7, Priority: 2, BurnUSD: 2}, {TaskID: "T-9", N: 9, Priority: 2, BurnUSD: 2}, {TaskID: "T-8", N: 8, Priority: 2, BurnUSD: 2}}, "T-9"},
		{"priority 0 ranks as P2", []Running{{TaskID: "T-1", N: 1, Priority: 0, BurnUSD: 3}, {TaskID: "T-2", N: 2, Priority: 2, BurnUSD: 1}, {TaskID: "T-3", N: 3, Priority: 1, BurnUSD: 9}}, "T-1"},
		{"priority 0 below P3", []Running{{TaskID: "T-1", N: 1, Priority: 0, BurnUSD: 3}, {TaskID: "T-2", N: 2, Priority: 3}}, "T-2"},
		{"a NaN burn counts as none", []Running{{TaskID: "T-1", N: 2, Priority: 3, BurnUSD: math.NaN()}, {TaskID: "T-2", N: 1, Priority: 3, BurnUSD: 0.5}}, "T-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := SuggestPause(c.in)
			if !ok || got != c.want {
				t.Errorf("SuggestPause = %q, %v; want %q", got, ok, c.want)
			}
			rev := slices.Clone(c.in)
			slices.Reverse(rev)
			if got, _ := SuggestPause(rev); got != c.want {
				t.Errorf("reversed: %q, want %q", got, c.want)
			}
		})
	}
	if got, ok := SuggestPause(nil); ok || got != "" {
		t.Errorf("SuggestPause(nil) = %q, %v", got, ok)
	}
}

func TestCostCell(t *testing.T) {
	cases := []struct {
		usd   float64
		known bool
		want  string
	}{
		{4.123, true, "$4.12"},
		{0.07, true, "$0.07"},
		{123.4, true, "$123"},
		{4.123, false, ""},
		{0, false, ""},
		{0, true, "$0.00"},
		{0.004, true, "$0.00"},
		{99.99, true, "$99.99"},
		{99.996, true, "$100"},
		{100, true, "$100"},
		{123.5, true, "$124"},
		{99999, true, "$99999"},
		{1e9, true, "$99999+"},
		{-1, true, ""},
		{math.NaN(), true, ""},
		{math.Inf(1), true, ""},
	}
	for _, c := range cases {
		if got := CostCell(c.usd, c.known); got != c.want {
			t.Errorf("CostCell(%v, %v) = %q, want %q", c.usd, c.known, got, c.want)
		}
	}
}

func fcSession(state model.State, expires time.Duration, rebuild float64) model.Session {
	s := model.Session{ID: "s1", State: state, CacheRebuildUSD: rebuild}
	if expires != 0 {
		s.CacheExpiresAt = fcAt(expires)
	}
	return s
}

func TestCacheCell(t *testing.T) {
	cases := []struct {
		name string
		s    model.Session
		want string
	}{
		{"idle warm", fcSession(model.StateIdle, 12*time.Minute+30*time.Second, 1.68), "12m"},
		{"idle warm 20s", fcSession(model.StateIdle, 20*time.Second, 1.68), "1m"},
		{"idle warm 1h", fcSession(model.StateIdle, time.Hour, 0), "1h0m"},
		{"expired with rebuild", fcSession(model.StateIdle, -time.Minute, 1.68), "cold $1.68"},
		{"expired exactly now", model.Session{State: model.StateIdle, CacheExpiresAt: fcNow, CacheRebuildUSD: 1.68}, "cold $1.68"},
		{"expired without rebuild", fcSession(model.StateIdle, -time.Minute, 0), "cold"},
		{"expired, rebuild under a cent", fcSession(model.StateIdle, -time.Minute, 0.004), "cold"},
		{"expired, rebuild NaN", fcSession(model.StateIdle, -time.Minute, math.NaN()), "cold"},
		{"unknown", fcSession(model.StateIdle, 0, 1.68), ""},
		{"working warm", fcSession(model.StateWorking, 12*time.Minute, 1.68), ""},
		{"working cold", fcSession(model.StateWorking, -time.Minute, 1.68), ""},
		{"ended", fcSession(model.StateEnded, 12*time.Minute, 1.68), ""},
		{"stale", fcSession(model.StateStale, 12*time.Minute, 1.68), ""},
		{"waiting warm", fcSession(model.StateWaiting, 12*time.Minute+30*time.Second, 1.68), "12m"},
		{"waiting cold", fcSession(model.StateWaiting, -time.Minute, 1.68), "cold $1.68"},
		{"needs_decision warm", fcSession(model.StateNeedsDecision, 12*time.Minute+30*time.Second, 1.68), "12m"},
		{"needs_decision cold", fcSession(model.StateNeedsDecision, -time.Minute, 0), "cold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CacheCell(c.s, fcNow); got != c.want {
				t.Errorf("CacheCell = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTokensCell(t *testing.T) {
	cases := []struct {
		n     int
		known bool
		want  string
	}{
		{85_000, true, "85k tok"},
		{0, true, "0 tok"},
		{999, true, "999 tok"},
		{1000, true, "1.0k tok"},
		{8_449, true, "8.4k tok"},
		{9_949, true, "9.9k tok"},
		{9_950, true, "10k tok"},
		{999_499, true, "999k tok"},
		{999_500, true, "1.0M tok"},
		{1_250_000, true, "1.3M tok"},
		{9_950_000, true, "10M tok"},
		{999_499_999, true, "999M tok"},
		{999_500_000, true, ">999M tok"},
		{math.MaxInt, true, ">999M tok"},
		{85_000, false, ""},
		{-1, true, ""},
	}
	for _, c := range cases {
		if got := TokensCell(c.n, c.known); got != c.want {
			t.Errorf("TokensCell(%d, %v) = %q, want %q", c.n, c.known, got, c.want)
		}
	}
}

// fcRand is a small deterministic generator (splitmix64), so the property
// test needs no math/rand.
type fcRand struct{ s uint64 }

func (r *fcRand) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// intn is in [0, n): the top 31 bits, so the conversion cannot overflow.
func (r *fcRand) intn(n int) int { return int(r.next()>>33) % n }

func (r *fcRand) pct() float64 {
	special := []float64{0, 100, 99.999, 1e-300, -1, 101, math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64}
	if r.intn(4) == 0 {
		return special[r.intn(len(special))]
	}
	return float64(r.intn(10001)) / 100
}

func (r *fcRand) usd() float64 {
	special := []float64{0, 1e-9, 0.005, 99.995, 99999.5, 1e12, math.MaxFloat64, -3, math.NaN(), math.Inf(1)}
	if r.intn(4) == 0 {
		return special[r.intn(len(special))]
	}
	return float64(r.intn(100000)) / 100
}

func (r *fcRand) offset() time.Duration {
	special := []time.Duration{0, -BurnWindow, -MinBurnSpan, time.Second, -time.Second, -365 * 24 * time.Hour, 365 * 24 * time.Hour}
	if r.intn(5) == 0 {
		return special[r.intn(len(special))]
	}
	return time.Duration(r.intn(int(30*time.Minute/time.Millisecond))) * -time.Millisecond
}

func (r *fcRand) usage() []model.UsageSample {
	windows := []model.UsageWindow{model.WindowFiveHour, model.WindowSevenDay}
	out := make([]model.UsageSample, r.intn(8))
	for i := range out {
		var reset time.Time
		if r.intn(4) != 0 {
			reset = fcNow.Add(time.Duration(r.intn(8*24*3600)-3600) * time.Second)
		}
		out[i] = model.UsageSample{
			N: int64(r.intn(5)), At: fcNow.Add(r.offset()), Window: windows[r.intn(2)],
			UsedPct: r.pct(), ResetsAt: reset, SessionID: []string{"s1", "s2", "s3"}[r.intn(3)],
		}
	}
	return out
}

func (r *fcRand) cost() []model.CostSample {
	out := make([]model.CostSample, r.intn(8))
	for i := range out {
		out[i] = model.CostSample{N: int64(i), At: fcNow.Add(r.offset()), SessionID: []string{"s1", "s2", "s3"}[r.intn(3)], CostUSD: r.usd()}
	}
	return out
}

func fcFinite(t *testing.T, what string, v float64) {
	t.Helper()
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		t.Fatalf("%s = %v", what, v)
	}
}

func fcCellOK(t *testing.T, what, s string, width int) {
	t.Helper()
	if utf8.RuneCountInString(s) > width || strings.Contains(s, "NaN") || strings.Contains(s, "Inf") || strings.Contains(s, "-") {
		t.Fatalf("%s = %q (width %d)", what, s, width)
	}
}

func fcCheckForecasts(t *testing.T, fs []Forecast, now time.Time) {
	t.Helper()
	for _, f := range fs {
		fcFinite(t, "UsedPct", f.UsedPct)
		fcFinite(t, "PerHour", f.PerHour)
		if f.UsedPct > 100 || (f.PerHour == 0 && f.UsedPct < 100 && !f.LimitAt.IsZero()) {
			t.Fatalf("forecast %+v", f)
		}
		if f.Warn && (f.LimitAt.IsZero() || !f.ResetsAt.After(now) || !f.LimitAt.Before(f.ResetsAt)) {
			t.Fatalf("warning forecast %+v", f)
		}
		fcCellOK(t, "BadgeText", BadgeText(f, now), 60)
		fcCellOK(t, "Line", Line(f, now), 80)
	}
}

func TestForecastPropertyNoNaNOrInf(t *testing.T) {
	r := &fcRand{s: 20261115}
	for range 20000 {
		usage, cost := r.usage(), r.cost()
		now := fcNow.Add(time.Duration(r.intn(3)-1) * time.Duration(r.intn(int(time.Hour))))
		fs := Forecasts(usage, now)
		fcCheckForecasts(t, fs, now)
		if w, ok := Warning(fs, now); ok {
			fcCheckForecasts(t, []Forecast{w}, now)
		}
		for id, b := range Burns(cost, now) {
			fcFinite(t, "Burns["+id+"]", b)
		}
		usd, _ := APIProjection(usage, cost, map[string]bool{"s1": true, "s2": r.intn(2) == 0}, now)
		fcFinite(t, "APIProjection", usd)
		d := time.Duration(r.next() >> 1)
		fcCellOK(t, "Short", Short(d), 10)
		fcCellOK(t, "CostCell", CostCell(r.usd(), true), 7)
		s := model.Session{State: model.StateIdle, CacheExpiresAt: now.Add(r.offset()), CacheRebuildUSD: r.usd()}
		fcCellOK(t, "CacheCell", CacheCell(s, now), 12)
		fcCellOK(t, "TokensCell", TokensCell(int(r.next()>>1), true), 9)
	}
}

// TestForecastFileReadsNoClock: every function here takes now; none reads a clock.
func TestForecastFileReadsNoClock(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "forecast.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"Now": true, "Since": true, "Until": true, "Sleep": true, "After": true, "Tick": true, "NewTimer": true, "NewTicker": true, "AfterFunc": true}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "time" && banned[sel.Sel.Name] {
			t.Errorf("forecast.go calls time.%s", sel.Sel.Name)
		}
		return true
	})
	for _, imp := range f.Imports {
		if p := imp.Path.Value; p != `"cmp"` && p != `"fmt"` && p != `"math"` && p != `"slices"` && p != `"strings"` && p != `"time"` && p != `"github.com/ZachFara/curia/internal/model"` {
			t.Errorf("forecast.go imports %s", p)
		}
	}
}

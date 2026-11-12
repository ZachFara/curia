package decision

import (
	"reflect"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

var triageNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func ticket(n int64, mode model.Mode, age, cacheLeft time.Duration) model.Ticket {
	t := model.Ticket{Decision: model.Decision{N: n, Mode: mode, CreatedAt: triageNow.Add(-age)}}
	if cacheLeft != 0 {
		t.CacheExpiresAt = triageNow.Add(cacheLeft)
	}
	return t
}

func order(ts []model.Ticket) []int64 {
	var ns []int64
	for _, t := range ts {
		ns = append(ns, t.N)
	}
	return ns
}

func TestTriageOrder(t *testing.T) {
	assumeOld := ticket(1, model.ModeAssume, 50*time.Minute, 0)
	warm5 := ticket(2, model.ModeBlocking, time.Minute, 5*time.Minute)
	warm12 := ticket(3, model.ModeBlocking, 2*time.Minute, 12*time.Minute)
	cold := ticket(4, model.ModeBlocking, 30*time.Minute, -time.Minute)
	unknown := ticket(5, model.ModeBlocking, 40*time.Minute, 0)
	assumeNew := ticket(6, model.ModeAssume, 2*time.Minute, 0)

	in := []model.Ticket{assumeOld, warm12, assumeNew, cold, warm5, unknown}
	snapshot := order(in)
	got := order(Triage(in, triageNow))
	if want := []int64{2, 3, 5, 4, 1, 6}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(order(in), snapshot) {
		t.Error("Triage reordered its input")
	}
}

func TestTriageAllUnknownIsOldestBlockingFirst(t *testing.T) {
	in := []model.Ticket{
		ticket(1, model.ModeAssume, 10*time.Minute, 0),
		ticket(2, model.ModeBlocking, 5*time.Minute, 0),
		ticket(3, model.ModeAssume, 30*time.Minute, 0),
		ticket(4, model.ModeBlocking, 20*time.Minute, 0),
		ticket(6, model.ModeBlocking, 5*time.Minute, 0), // same age as 2: tie by n
	}
	if got, want := order(Triage(in, triageNow)), []int64{4, 2, 6, 3, 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestTriageCacheExpiringNowIsCold(t *testing.T) {
	atNow := ticket(1, model.ModeBlocking, time.Minute, 0)
	atNow.CacheExpiresAt = triageNow
	warm := ticket(2, model.ModeBlocking, 0, time.Second)
	if got, want := order(Triage([]model.Ticket{atNow, warm}, triageNow)), []int64{2, 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestStakes(t *testing.T) {
	warm := func(left time.Duration, usd float64) model.Ticket {
		tk := ticket(1, model.ModeBlocking, 0, left)
		tk.CacheRebuildUSD = usd
		return tk
	}
	cold := func(tokens int) model.Ticket {
		tk := ticket(1, model.ModeBlocking, 0, -time.Minute)
		tk.ContextUsed = tokens
		return tk
	}
	tests := []struct {
		name string
		t    model.Ticket
		want string
	}{
		{"warm 12m30s", warm(12*time.Minute+30*time.Second, 1.68), "answer within 12m to keep cache warm (rebuild ≈ $1.68)"},
		{"warm 20s", warm(20*time.Second, 1.68), "answer within 1m to keep cache warm (rebuild ≈ $1.68)"},
		{"rebuild 0", warm(5*time.Minute, 0), "answer within 5m to keep cache warm"},
		{"rebuild under a cent", warm(5*time.Minute, 0.004), "answer within 5m to keep cache warm"},
		{"cold 84200", cold(84200), "cache cold: next turn re-writes ~84k tokens"},
		{"cold 84600", cold(84600), "cache cold: next turn re-writes ~85k tokens"},
		{"cold 300", cold(300), "cache cold: next turn re-writes ~1k tokens"},
		{"cold 0", cold(0), "cache cold"},
		{"unknown", ticket(1, model.ModeBlocking, 0, 0), ""},
	}
	for _, tt := range tests {
		if got := Stakes(tt.t, triageNow); got != tt.want {
			t.Errorf("%s: Stakes = %q, want %q", tt.name, got, tt.want)
		}
	}
}

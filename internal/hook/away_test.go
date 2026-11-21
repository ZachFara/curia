package hook_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/hook"
	latgate "github.com/ZachFara/curia/internal/latency"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func startBody(id string) []byte {
	return []byte(`{"session_id":"` + id + `","transcript_path":"/p/` + id + `.jsonl","cwd":"/src/curia","hook_event_name":"SessionStart","source":"startup"}`)
}

// F20: a session that starts while an away period runs is away until the
// period's end; one that starts after it, or outside its repository, is not.
func TestSessionStartedDuringAwayJoinsIt(t *testing.T) {
	_, _, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	ctx := context.Background()
	run, err := st.StartAway(ctx, store.ModeScope{Repo: "/src/curia"}, t0, t0.Add(6*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hook.Run(ctx, model.SessionStart, startBody("N1"), env, st, cfg); err != nil {
		t.Fatal(err)
	}
	s, _, _ := st.GetSession(ctx, "N1")
	if s.Mode != "away" || !s.ModeUntil.Equal(t0.Add(6*time.Hour)) {
		t.Errorf("new session mode = %q until %v, want away until the period's end", s.Mode, s.ModeUntil)
	}
	if _, won, _ := st.EndAway(ctx, run.ID, t0.Add(time.Hour)); !won {
		t.Fatal("end away")
	}
	if _, err = hook.Run(ctx, model.SessionStart, startBody("N2"), env, st, cfg); err != nil {
		t.Fatal(err)
	}
	if s, _, _ = st.GetSession(ctx, "N2"); s.Mode != "" {
		t.Errorf("a session started after the period has mode %q, want the default", s.Mode)
	}
	if _, err = st.StartAway(ctx, store.ModeScope{Repo: "/elsewhere"}, t0, t0.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if _, err = hook.Run(ctx, model.SessionStart, startBody("N3"), env, st, cfg); err != nil {
		t.Fatal(err)
	}
	if s, _, _ = st.GetSession(ctx, "N3"); s.Mode != "" {
		t.Errorf("a session outside the period's repository has mode %q", s.Mode)
	}
}

// The extra write on a start during away must keep the hook far under its 20 ms budget.
func TestSessionStartDuringAwayLatency(t *testing.T) {
	_, _, cfg := denyHome(t)
	st := openStore(t)
	env := (&fakeEnv{}).env("%3")
	if _, err := st.StartAway(context.Background(), store.ModeScope{All: true}, t0, t0.Add(6*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	n := 0
	latgate.Gate(t, "SessionStart during away", 20*time.Millisecond, func() time.Duration {
		d := make([]time.Duration, 0, 200)
		for range 200 {
			n++
			start := time.Now()
			if _, err := hook.Run(context.Background(), model.SessionStart, startBody(fmt.Sprintf("L%d", n)), env, st, cfg); err != nil {
				t.Fatal(err)
			}
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		t.Logf("SessionStart during away over %d runs: p50 %v, p95 %v", len(d), d[len(d)/2], d[len(d)*95/100])
		return d[len(d)*95/100]
	})
}

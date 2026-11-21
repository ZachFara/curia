package hook_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
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

// awayRootSession makes session A with proj as its worktree and mode (and so root) as given ("" none).
func awayRootSession(t *testing.T, mode string) (*store.Store, hook.Env, config.Config, string, *sql.DB) {
	t.Helper()
	_, proj, cfg := denyHome(t)
	st, raw := openWithRaw(t)
	env := (&fakeEnv{}).env("%3")
	modeSession(t, st, env, cfg, proj, mode, time.Time{})
	return st, env, cfg, proj, raw
}

func rootOf(t *testing.T, st *store.Store) (mode, root string) {
	t.Helper()
	s, _, err := st.GetSession(context.Background(), "A")
	if err != nil {
		t.Fatal(err)
	}
	return s.Mode, s.ModeRoot
}

// F20: curia away freezes the worktree as the root, so edits inside it are allowed and edits outside are not.
func TestAwayFreezesTheWorktreeRoot(t *testing.T) {
	st, env, cfg, proj, _ := awayRootSession(t, "")
	if _, err := st.StartAway(context.Background(), store.ModeScope{All: true}, t0, t0.Add(6*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	wantAllow(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(proj+"/a.go")+`}`, proj, "")), "edit inside the worktree")
	notAllowed(t, pre(t, st, env, cfg, preBody("Write", `{"file_path":`+quote(filepath.Dir(proj)+"/x.go")+`}`, proj, "")), "edit outside the worktree")
}

// Back puts the previous mode and root back exactly; a session with no mode has neither afterwards.
func TestAwayBackRestoresModeAndRoot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ before, wantMode, wantRoot string }{{"edits", "edits", "ROOT"}, {"", "", ""}, {"bypass", "", ""}} {
		st, _, _, proj, _ := awayRootSession(t, tc.before)
		if tc.before == "edits" { // the worktree moves after the mode was set: the old root must come back
			if err := st.UpdateSession(ctx, "A", func(s model.Session, _ bool) model.Session { s.Worktree = proj + "/moved"; return s }); err != nil {
				t.Fatal(err)
			}
		}
		run, err := st.StartAway(ctx, store.ModeScope{All: true}, t0, t0.Add(6*time.Hour), nil)
		if err != nil {
			t.Fatal(err)
		}
		if m, r := rootOf(t, st); m != "away" || r == "" {
			t.Fatalf("%q: during away mode %q root %q", tc.before, m, r)
		}
		if _, won, err := st.EndAway(ctx, run.ID, t0.Add(time.Hour)); err != nil || !won {
			t.Fatal("end away", err)
		}
		want := tc.wantRoot
		if want == "ROOT" {
			want = proj
		}
		if m, r := rootOf(t, st); m != tc.wantMode || r != want {
			t.Errorf("%q: after back mode %q root %q, want %q %q", tc.before, m, r, tc.wantMode, want)
		}
	}
}

// A saved row written before the root was saved restores its mode and leaves no root.
func TestOldSavedAwayRowRestoresWithNoRoot(t *testing.T) {
	ctx := context.Background()
	st, _, _, _, raw := awayRootSession(t, "edits")
	run, err := st.StartAway(ctx, store.ModeScope{All: true}, t0, t0.Add(6*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`UPDATE away_runs SET saved_modes = '{"A":{"mode":"edits","until":"0001-01-01T00:00:00Z"}}'`); err != nil {
		t.Fatal(err)
	}
	if _, won, err := st.EndAway(ctx, run.ID, t0.Add(time.Hour)); err != nil || !won {
		t.Fatal("end away", err)
	}
	if m, r := rootOf(t, st); m != "edits" || r != "" {
		t.Errorf("old row restored mode %q root %q, want edits and no root", m, r)
	}
}

// A session that starts during away gets the root from the worktree git reported for it, once; none without one.
func TestInheritAwaySetsTheRoot(t *testing.T) {
	_, proj, cfg := denyHome(t)
	for _, tc := range []struct{ wt, repo, want string }{{proj, "/r", proj}, {"", "/r", "/r"}, {"", "", ""}} {
		st := openStore(t)
		env := (&fakeEnv{}).env("%3")
		env.Git = func(string) (string, string, string) { return tc.repo, "b", tc.wt }
		ctx := context.Background()
		if _, err := st.StartAway(ctx, store.ModeScope{All: true}, t0, t0.Add(6*time.Hour), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := hook.Run(ctx, model.SessionStart, startBody("N1"), env, st, cfg); err != nil {
			t.Fatal(err)
		}
		s, _, _ := st.GetSession(ctx, "N1")
		if s.Mode != "away" || s.ModeRoot != tc.want {
			t.Errorf("worktree %q repo %q: mode %q root %q, want away and root %q", tc.wt, tc.repo, s.Mode, s.ModeRoot, tc.want)
		}
	}
}

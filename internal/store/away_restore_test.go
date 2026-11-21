package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

var awayT = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func awaySess(t *testing.T, s *store.Store, id, repo string, state model.State) {
	t.Helper()
	upsert(t, s, model.Session{ID: id, Repo: repo, Worktree: repo, State: state, Agent: "claude", StartedAt: awayT})
}

func modeOf(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	sess, _, err := s.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return sess.Mode
}

// Review A-8: a saved mode that lapsed during the run is not restored.
func TestEndAwayDoesNotRestoreALapsedSavedMode(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	awaySess(t, s, "A", "/r/a", model.StateIdle)
	if _, err := s.SetModes(ctx, store.ModeScope{All: true}, model.ModeEdits, awayT.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	run, err := s.StartAway(ctx, store.ModeScope{All: true}, awayT, awayT.Add(6*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := s.EndAway(ctx, run.ID, awayT.Add(2*time.Hour)); err != nil || !won {
		t.Fatalf("EndAway = %v, %v", won, err)
	}
	if got := modeOf(t, s, "A"); got != "" && got != model.ModeNormal {
		t.Errorf("mode after the saved edits lapsed = %q, want none", got)
	}
}

// Review A-8: a run's scope limits whom the restore touches.
func TestEndAwayLeavesSessionsOutsideTheScopeAlone(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	awaySess(t, s, "in", "/r/a", model.StateIdle)
	awaySess(t, s, "out", "/r/b", model.StateIdle)
	run, err := s.StartAway(ctx, store.ModeScope{Repo: "/r/a"}, awayT, awayT.Add(6*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The user puts the other repository's session in away by hand during the run.
	if _, err = s.SetModes(ctx, store.ModeScope{Repo: "/r/b"}, model.ModeAway, awayT.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, won, err := s.EndAway(ctx, run.ID, awayT.Add(time.Hour)); err != nil || !won {
		t.Fatalf("EndAway = %v, %v", won, err)
	}
	if got := modeOf(t, s, "out"); got != model.ModeAway {
		t.Errorf("a session outside the scope has mode %q after the restore, want it untouched (away)", got)
	}
	if got := modeOf(t, s, "in"); got == model.ModeAway {
		t.Error("a session in the scope is still away")
	}
}

// Review A-8: StartAway flips live sessions only; an ended one is not away.
func TestStartAwayDoesNotFlipEndedSessions(t *testing.T) {
	s := open(t, dbPath(t))
	awaySess(t, s, "live", "/r/a", model.StateIdle)
	awaySess(t, s, "gone", "/r/a", model.StateEnded)
	if _, err := s.StartAway(context.Background(), store.ModeScope{All: true}, awayT, awayT.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if got := modeOf(t, s, "live"); got != model.ModeAway {
		t.Errorf("live session mode = %q, want away", got)
	}
	if got := modeOf(t, s, "gone"); got == model.ModeAway {
		t.Error("an ended session was flipped to away")
	}
}

// Review A-8: unknown cost is shown as unknown (Known false), not as 0 spent.
func TestTaskBudgetUnknownCostIsNotKnown(t *testing.T) {
	s := open(t, dbPath(t))
	tk := newTask("budgeted")
	tk.BudgetUSD = 5
	tk = createTask(t, s, tk)
	upsert(t, s, model.Session{ID: "a", State: model.StateIdle, TaskID: tk.ID, Agent: "claude", StartedAt: taskT0}) // a session with no cost
	b, err := s.TaskBudget(context.Background(), tk.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.Known || b.Spent != 0 {
		t.Errorf("TaskBudget = %+v, want unknown spend", b)
	}
	if over, _ := b.Exhausted(); over {
		t.Error("unknown cost counts as exhausted")
	}
}

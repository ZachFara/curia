package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func awaySession(t *testing.T, f *fixture, id string) {
	t.Helper()
	if err := f.st.UpsertSession(context.Background(), model.Session{ID: id, Agent: "claude", Repo: "/r/a", State: model.StateIdle, StartedAt: f.now, UpdatedAt: f.now, SeenAt: f.now}); err != nil {
		t.Fatal(err)
	}
}

// F20: while its session is away a blocking ticket is stored as assume with
// coerced_from = blocking, waits for the user (never answered, never expires),
// raises no alert, and tells the agent what to do. After the period the same
// session raises blocking tickets again.
func TestBlockingTicketWhileAwayIsStoredAsAssumeAndWaits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	awaySession(t, f, "S1")
	run, err := f.st.StartAway(ctx, store.ModeScope{Repo: "/r/a"}, f.now, f.now.Add(6*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := f.mustCall("request_decision", ticket(webhookQ))
	if res["mode"] != "assume" || res["status"] != "open" || !strings.Contains(res["instruction"].(string), "owner is away") {
		t.Fatalf("result = %v, want an open assume ticket whose instruction says the owner is away", res)
	}
	d := f.get(res["id"].(string))
	if d.Mode != model.ModeAssume || d.Status != model.StatusOpen || !d.ExpiresAt.IsZero() || d.Assumption == "" {
		t.Errorf("stored %+v: want open assume with no expiry and an assumption", d)
	}
	w, err := f.st.Waiting(ctx, f.now.Add(-time.Minute), f.now.Add(time.Minute))
	if err != nil || w.Coerced != 1 || w.Assumed != 1 || w.Blocking != 0 {
		t.Errorf("waiting = %+v, %v; want 1 coerced assume, no blocking", w, err)
	}
	if len(f.notified) != 0 {
		t.Errorf("notified %d times for a ticket nobody can answer now", len(f.notified))
	}
	if _, won, _ := f.st.EndAway(ctx, run.ID, f.now); !won {
		t.Fatal("end away")
	}
	res = f.mustCall("request_decision", ticket("Which queue for retries?"))
	if res["mode"] != "blocking" || len(f.notified) != 1 {
		t.Errorf("after the period: %v, notified %d; want a blocking ticket and one alert", res["mode"], len(f.notified))
	}
}

func TestBlockingTicketOfASessionOutsideTheAwayScopeStaysBlocking(t *testing.T) {
	f := newFixture(t)
	awaySession(t, f, "S1")
	if _, err := f.st.StartAway(context.Background(), store.ModeScope{Repo: "/elsewhere"}, f.now, f.now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if res := f.mustCall("request_decision", ticket(webhookQ)); res["mode"] != "blocking" {
		t.Errorf("mode = %v, want blocking for a session the period does not cover", res["mode"])
	}
}

package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// A blocking ticket coerced while its session is away carries no expiry: the sweep past
// assume_expiry leaves it open for the user, who may be away for days.
func TestCoercedTicketNeverExpires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.st.UpsertSession(ctx, model.Session{ID: "S1", Agent: "claude", Repo: "/r/a", State: model.StateIdle, StartedAt: f.now, UpdatedAt: f.now, SeenAt: f.now}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.StartAway(ctx, store.ModeScope{Repo: "/r/a"}, f.now, f.now.Add(7*24*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	res := f.mustCall("request_decision", ticket(webhookQ))
	id := typed[string](t, res["id"])
	if d := f.get(id); d.Mode != model.ModeAssume || !d.ExpiresAt.IsZero() {
		t.Fatalf("stored %+v, want an assume ticket with no expiry", d)
	}
	if n, err := f.st.ExpireDecisions(ctx, f.now.Add(25*time.Hour)); err != nil || n != 0 {
		t.Fatalf("sweep past assume_expiry expired %d, %v; want 0", n, err)
	}
	if d := f.get(id); d.Status != model.StatusOpen {
		t.Errorf("status = %s after the sweep, want open", d.Status)
	}
}

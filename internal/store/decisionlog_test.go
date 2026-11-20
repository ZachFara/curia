package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// logSeed: D-1 answered and reused twice (precedent_of), asked again once;
// D-2 answered, never reused; D-3 and D-4 cite D-1; D-5 raised near D-2;
// D-6 answered, superseded, reused once.
func logSeed(t *testing.T) *store.Store {
	t.Helper()
	path := dbPath(t)
	s := open(t, path)
	db := rawDB(t, path)
	qs := []string{"Which database for webhook events?", "Which logger for the exporter?", "cite one", "cite two", "raised near two", "Which queue for retries?"}
	for i, q := range qs {
		create(t, s, ticket(t, q, model.ModeBlocking, "s1", t0.Add(time.Duration(i)*time.Hour)))
	}
	for _, id := range []string{"D-1", "D-2", "D-6"} {
		tlExec(t, db, `UPDATE decisions SET status='answered', answer='B: yes', answered_at=created_at WHERE id=?`, id)
	}
	tlExec(t, db, `UPDATE decisions SET superseded_at = ? WHERE id = 'D-6'`, tlAt(1))
	tlExec(t, db, `UPDATE decisions SET status='withdrawn', precedent_of='D-1', closed_at=? WHERE id IN ('D-3','D-4')`, tlAt(300))
	tlExec(t, db, `UPDATE decisions SET status='withdrawn', precedent_of='D-6', closed_at=? WHERE id='D-5'`, tlAt(301))
	tlExec(t, db, `UPDATE decisions SET near_match='D-2' WHERE id IN ('D-5')`)
	tlExec(t, db, `UPDATE decisions SET near_match='D-1' WHERE id = 'D-4'`)
	return s
}

func TestDecisionLogReuseCountsPrecedentOf(t *testing.T) {
	s := logSeed(t)
	rows, sum, err := s.DecisionLog(context.Background(), store.DecisionLogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.DecisionLogRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if len(rows) != 3 || got["D-1"].Reuse != 2 || got["D-2"].Reuse != 0 || got["D-6"].Reuse != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	logCheckLast(t, got, sum)
}

func logCheckLast(t *testing.T, got map[string]model.DecisionLogRow, sum model.DecisionLogSummary) {
	t.Helper()
	if !got["D-1"].LastReuse.Equal(t0.Add(300*time.Minute)) || !got["D-2"].LastReuse.IsZero() {
		t.Errorf("last reuse = %v, %v", got["D-1"].LastReuse, got["D-2"].LastReuse)
	}
	if got["D-1"].AskedAgain != 1 || got["D-2"].AskedAgain != 1 || got["D-6"].AskedAgain != 0 {
		t.Errorf("asked again = %d %d %d", got["D-1"].AskedAgain, got["D-2"].AskedAgain, got["D-6"].AskedAgain)
	}
	if !got["D-6"].Superseded || got["D-1"].Superseded {
		t.Errorf("superseded marks = %v %v", got["D-6"].Superseded, got["D-1"].Superseded)
	}
	// Superseded D-6 is shown but is not a reuse target in the summary: 1 of 2.
	if sum.Answered != 2 || sum.Reused != 1 {
		t.Errorf("summary = %+v, want 2 answered, 1 reused", sum)
	}
}

func TestDecisionLogFilters(t *testing.T) {
	s := logSeed(t)
	ctx := context.Background()
	ids := func(q store.DecisionLogQuery) []string {
		rows, _, err := s.DecisionLog(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(store.DecisionLogQuery{Text: "webhook database"}); len(got) != 1 || got[0] != "D-1" {
		t.Errorf("search = %v", got)
	}
	if got := ids(store.DecisionLogQuery{Text: "?!"}); len(got) != 0 {
		t.Errorf("no words = %v", got)
	}
	if got := ids(store.DecisionLogQuery{Status: model.StatusWithdrawn}); len(got) != 3 {
		t.Errorf("status = %v", got)
	}
	if got := ids(store.DecisionLogQuery{Since: t0.Add(90 * time.Minute)}); len(got) != 1 || got[0] != "D-6" {
		t.Errorf("since = %v", got)
	}
	if got := ids(store.DecisionLogQuery{Scope: "global"}); len(got) != 0 {
		t.Errorf("scope = %v", got)
	}
	if got := ids(store.DecisionLogQuery{Repo: "/src/curia", Limit: 1}); len(got) != 1 || got[0] != "D-6" {
		t.Errorf("repo+limit = %v", got)
	}
}

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func exportQueued(t *testing.T, s *store.Store, all, retry bool) []string {
	t.Helper()
	q, err := s.ExportQueueWith(context.Background(), all, retry, 0)
	if err != nil {
		t.Fatal(err)
	}
	return decisionIDs(q)
}

func TestMarkExportedIfGuardsTheVersionWritten(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	now := t0.Add(2 * time.Hour)
	for name, change := range map[string]func(id string){
		"rescope": func(id string) {
			_, err := s.EditDecision(ctx, id, model.DecisionEdit{Scope: model.ScopeGlobal}, now)
			fatal(t, err)
		},
		"supersede": func(id string) {
			_, err := s.EditDecision(ctx, id, model.DecisionEdit{Supersede: true}, now)
			fatal(t, err)
		},
		"answer text": func(id string) {
			_, err := rawDB(t, path).Exec(`UPDATE decisions SET answer = 'changed', answer_reasoning = 'new' WHERE id = ?`, id)
			fatal(t, err)
		},
	} {
		d := decided(t, s, "Guard "+name+"?", "s1", testRepo, model.ScopeRepo)
		written := getDecision(t, s, d.ID)
		change(d.ID) // lands after the note was built, before the mark
		ok, err := s.MarkExportedIf(ctx, written, now)
		if err != nil || ok {
			t.Errorf("%s: MarkExportedIf of a stale version = %v, %v; want false, nil", name, ok, err)
		}
		if got := getDecision(t, s, d.ID); !got.ExportedAt.IsZero() {
			t.Errorf("%s: stale note marked at %v", name, got.ExportedAt)
		}
		if !slicesContains(exportQueued(t, s, false, false), d.ID) {
			t.Errorf("%s: decision not due after a refused mark", name)
		}
		ok, err = s.MarkExportedIf(ctx, getDecision(t, s, d.ID), now)
		if err != nil || !ok {
			t.Errorf("%s: MarkExportedIf of the current version = %v, %v; want true, nil", name, ok, err)
		}
		if slicesContains(exportQueued(t, s, false, false), d.ID) {
			t.Errorf("%s: decision still due after a good mark", name)
		}
	}
	gone, err := s.MarkExportedIf(ctx, model.Decision{ID: "D-999", Status: model.StatusAnswered}, now)
	if err != nil || gone {
		t.Errorf("unknown decision: %v, %v; want false, nil", gone, err)
	}
}

func slicesContains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func fatal(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func skip(t *testing.T, s *store.Store, d model.Decision, at time.Time, want bool) {
	t.Helper()
	ok, err := s.MarkExportSkipped(context.Background(), d, at)
	if err != nil || ok != want {
		t.Fatalf("MarkExportSkipped(%s) = %v, %v; want %v, nil", d.ID, ok, err, want)
	}
}

func TestExportSkipMovesPastAndLapses(t *testing.T) {
	s0path := dbPath(t)
	s := open(t, s0path)
	ctx := context.Background()
	skipAt := t0.Add(3 * time.Hour)
	a := decided(t, s, "Skipped one?", "s1", testRepo, model.ScopeRepo)
	b := decided(t, s, "Skipped two?", "s1", testRepo, model.ScopeRepo)
	c := decided(t, s, "Later one?", "s1", testRepo, model.ScopeRepo)

	// A skip for a stale version is refused.
	stale := getDecision(t, s, a.ID)
	_, err := s.EditDecision(ctx, a.ID, model.DecisionEdit{Scope: model.ScopeGlobal}, skipAt)
	fatal(t, err)
	skip(t, s, stale, skipAt, false)
	for _, d := range []model.Decision{a, b} {
		skip(t, s, getDecision(t, s, d.ID), skipAt, true)
	}
	for _, all := range []bool{false, true} {
		if got := exportQueued(t, s, all, false); len(got) != 1 || got[0] != c.ID {
			t.Errorf("all=%v: queue after skips = %v, want only %s", all, got, c.ID)
		}
	}
	if got := exportQueued(t, s, false, true); len(got) != 3 {
		t.Errorf("retry-skipped queue = %v, want all three", got)
	}

	// A rescope, a supersession or a new answer after the skip lifts it.
	_, err = s.EditDecision(ctx, a.ID, model.DecisionEdit{Scope: model.ScopeRepo}, skipAt.Add(time.Hour))
	fatal(t, err)
	if !slicesContains(exportQueued(t, s, false, false), a.ID) {
		t.Error("a rescope did not make the skipped decision due again")
	}
	_, err = s.EditDecision(ctx, b.ID, model.DecisionEdit{Supersede: true}, skipAt.Add(time.Hour))
	fatal(t, err)
	if !slicesContains(exportQueued(t, s, false, false), b.ID) {
		t.Error("a supersession did not make the skipped decision due again")
	}

	// A successful mark lifts the skip row, so a later retry list is clean.
	if ok, err := s.MarkExportedIf(ctx, getDecision(t, s, b.ID), skipAt.Add(2*time.Hour)); err != nil || !ok {
		t.Fatalf("mark = %v, %v", ok, err)
	}
	var skips int
	if err := rawDB(t, s0path).QueryRow(`SELECT count(*) FROM meta WHERE key = 'export_skip:' || ?`, b.ID).Scan(&skips); err != nil || skips != 0 {
		t.Errorf("skip row of a marked decision: %d, %v", skips, err)
	}
}

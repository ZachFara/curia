package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/team"
)

func filed(title string) model.Task {
	t := newTask(title)
	t.FiledBy, t.FiledReason = "sess-a", "found while testing"
	return t
}

func TestFileTaskRecordsFilerAndLineage(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for _, sess := range []model.Session{
		{ID: "sess-lead", State: model.StateIdle, UpdatedAt: taskT0},
		{ID: "sess-a", State: model.StateIdle, UpdatedAt: taskT0},
	} {
		if err := s.UpsertSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	setTeams(t, s, map[string]team.Membership{
		"sess-lead": {TeamID: "crew", Role: model.RoleLead, Member: "boss"},
		"sess-a":    {TeamID: "crew", Role: model.RoleTeammate, Member: "mate"},
	})
	got, err := s.FileTask(ctx, filed("tidy"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.FiledBy != "sess-a" || got.FiledReason != "found while testing" || got.Status != model.TaskBacklog {
		t.Errorf("stored %+v", got)
	}
	for _, want := range []string{`"team":"crew"`, `"role":"teammate"`, `"lead":"sess-lead"`} {
		if !strings.Contains(got.FiledLineage, want) {
			t.Errorf("lineage %s lacks %s", got.FiledLineage, want)
		}
	}
	if back := getTask(t, s, got.ID); back.FiledBy != "sess-a" || back.FiledLineage != got.FiledLineage {
		t.Errorf("read back %+v", back)
	}
	if plain := createTask(t, s, newTask("mine")); plain.FiledBy != "" {
		t.Errorf("a user task has FiledBy %q", plain.FiledBy)
	}
}

func TestFileTaskCapIsPerSessionAndRolling(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := s.FileTask(ctx, filed("t"), 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.FileTask(ctx, filed("over"), 2); !errors.Is(err, store.ErrFileCap) {
		t.Fatalf("third filing = %v, want ErrFileCap", err)
	}
	other := filed("other")
	other.FiledBy = "sess-b"
	if _, err := s.FileTask(ctx, other, 2); err != nil {
		t.Errorf("another session was refused: %v", err)
	}
	late := filed("later")
	late.CreatedAt = taskT0.Add(24*time.Hour + time.Second)
	if _, err := s.FileTask(ctx, late, 2); err != nil {
		t.Errorf("after the window: %v", err)
	}
}

func TestFileTaskCapHoldsUnderConcurrency(t *testing.T) {
	s := open(t, dbPath(t))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.FileTask(context.Background(), filed("c"), 3); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 3 {
		t.Errorf("%d filings succeeded, want exactly 3", ok)
	}
}

func TestFileTaskRefusesNoFiler(t *testing.T) {
	s := open(t, dbPath(t))
	if _, err := s.FileTask(context.Background(), newTask("x"), 5); err == nil {
		t.Error("a task with no filer was stored")
	}
}

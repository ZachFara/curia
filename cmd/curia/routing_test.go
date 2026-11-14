package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// routingSeed stores n labels, oldest first; label i is rejected when
// rejected(i). Every label has confidence 0.95.
func routingSeed(t *testing.T, db string, n int, rejected func(i int) bool) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	for i := 0; i < n; i++ {
		m, err := st.CreateMessage(context.Background(), model.Message{
			Text: fmt.Sprintf("m%d", i), Kind: model.KindInstruction, Sender: model.SenderUser,
			Confidence: 0.95, HasConfidence: true, RoutedBy: model.RoutedConfirmed,
			Status: model.MsgQueued, CreatedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if rejected(i) {
			if _, err := raw.Exec(`UPDATE messages SET routed_by = 'overridden' WHERE id = ?`, m.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func routingHome(t *testing.T) string {
	t.Helper()
	db := hookHome(t)
	t.Cleanup(func() { routingJSON = false })
	return db
}

func TestRoutingStatsNumbers(t *testing.T) {
	db := routingHome(t)
	// 60 labels; the newest 50 are i = 10..59, with 3 rejected among them
	// and 4 rejected among the oldest ten (which must not count).
	routingSeed(t, db, 60, func(i int) bool { return i < 4 || i == 20 || i == 30 || i == 40 })
	out, err := runRoot(t, "routing", "stats")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"47/50 accepted (94%)", "threshold:   0.9", "configured:  no", "earned:      no", "tripped:     no"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats lacks %q:\n%s", want, out)
		}
	}
	out, err = runRoot(t, "routing", "stats", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var j map[string]any
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		t.Fatal(err)
	}
	if j["labeled"] != 50.0 || j["accepted"] != 47.0 || j["earned"] != false || j["earned_at"] != nil || j["threshold"] != 0.9 {
		t.Errorf("json = %v", j)
	}
}

func TestRoutingStatsNoLabels(t *testing.T) {
	routingHome(t)
	out, err := runRoot(t, "routing", "stats")
	if err != nil || !strings.Contains(out, "0/0 accepted (0%)") {
		t.Errorf("stats = %q, %v", out, err)
	}
}

func TestRoutingStatsShowsTripped(t *testing.T) {
	db := routingHome(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	e := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err = st.SetRoutingEarned(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err = st.SetRoutingTripped(context.Background(), e.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	out, err := runRoot(t, "routing", "stats")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"earned:      yes (2026-11-01T09:00:00Z)", "tripped:     yes (2026-11-01T10:00:00Z)"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats lacks %q:\n%s", want, out)
		}
	}
}

func routingState(t *testing.T, db string) (time.Time, time.Time) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	e, tr, err := st.RoutingState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return e, tr
}

func TestRoutingReset(t *testing.T) {
	db := routingHome(t)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = st.SetRoutingEarned(context.Background(), now)
	_ = st.SetRoutingTripped(context.Background(), now)
	_ = st.Close()

	t.Setenv("CLAUDECODE", "1")
	if _, rerr := runRoot(t, "routing", "reset"); rerr == nil || !strings.Contains(rerr.Error(), "refusing to run inside an agent session") {
		t.Fatalf("inside agent: %v", rerr)
	}
	if e, tr := routingState(t, db); e.IsZero() || tr.IsZero() {
		t.Fatal("refused reset still cleared a key")
	}
	t.Setenv("CLAUDECODE", "")
	out, err := runRoot(t, "routing", "reset")
	if err != nil || !strings.Contains(out, "must be earned again") {
		t.Fatalf("reset = %q, %v", out, err)
	}
	if e, tr := routingState(t, db); !e.IsZero() || !tr.IsZero() {
		t.Errorf("keys not cleared: %v %v", e, tr)
	}
}

func TestRoutingResetRefusesBeforeTheStore(t *testing.T) {
	db := hookHome(t)
	t.Setenv("CLAUDECODE", "1")
	if _, err := runRoot(t, "routing", "reset"); err == nil {
		t.Fatal("no refusal")
	}
	if _, statErr := os.Stat(db); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("touched the database: %v", statErr)
	}
}

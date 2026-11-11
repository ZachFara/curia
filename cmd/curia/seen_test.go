package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func seedSessions(t *testing.T, db string, sessions ...model.Session) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, s := range sessions {
		if err := st.UpsertSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeenClearsWindowMarker(t *testing.T) {
	db := hookHome(t)
	fake := useFakeTmux(t)
	seedSessions(t, db, model.Session{
		ID: "A", Pane: "%1", TmuxWindow: "@7",
		State: model.StateWaiting, UpdatedAt: time.Now().Add(-time.Minute),
	})

	res := runHookCmd(t, strings.NewReader(""), "seen", "%1")
	if res.log != nil || res.output != "" {
		t.Errorf("log %q output %q, want both empty", res.log, res.output)
	}
	want := []string{"SetWindowOption @7 @curia_marker "}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %q, want %q", fake.Calls, want)
	}
}

func TestSeenUnknownPaneDoesNothing(t *testing.T) {
	db := hookHome(t)
	fake := useFakeTmux(t)
	seedSessions(t, db, model.Session{ID: "A", Pane: "%1", TmuxWindow: "@7", State: model.StateWaiting})

	res := runHookCmd(t, strings.NewReader(""), "seen", "%9")
	if res.log != nil {
		t.Errorf("log = %q, want none", res.log)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %q, want none", fake.Calls)
	}
}

func TestSeenBadStoreLogsAndSucceeds(t *testing.T) {
	hookHome(t)
	t.Setenv("CURIA_DB", t.TempDir()) // a directory, not a database
	useFakeTmux(t)
	res := runHookCmd(t, strings.NewReader(""), "seen", "%1")
	if len(res.log) != 1 {
		t.Errorf("log = %q, want one line", res.log)
	}
}

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

// TestSeenOnlyThisServer: curia seen marks seen the pane's session on the
// tmux server named by TMUX (the server running the focus hook), not the
// session in the same pane id of another server.
func TestSeenOnlyThisServer(t *testing.T) {
	db, _ := fleetHome(t, nil,
		model.Session{ID: "here", Agent: "claude", State: model.StateWaiting, Pane: "%0", TmuxWindow: "@1", Socket: "/tmp/tmux-501/a", UpdatedAt: at(10, 0)},
		model.Session{ID: "there", Agent: "claude", State: model.StateWaiting, Pane: "%0", TmuxWindow: "@1", Socket: "/tmp/tmux-501/b", UpdatedAt: at(10, 0)},
	)
	t.Setenv("TMUX", "/tmp/tmux-501/a,123,0")
	if _, err := runRoot(t, "seen", "%0"); err != nil {
		t.Fatal(err)
	}
	if s := getSession(t, db, "here"); s.SeenAt.IsZero() {
		t.Error("here is not seen")
	}
	if s := getSession(t, db, "there"); !s.SeenAt.IsZero() {
		t.Error("the session on another server was marked seen")
	}
	for in, want := range map[string]string{"/tmp/tmux-501/a,123,0": "/tmp/tmux-501/a", "": "", "relative,1,0": "", "/x": ""} {
		if got := seenSocket(in); got != want {
			t.Errorf("seenSocket(%q) = %q, want %q", in, got, want)
		}
	}
}

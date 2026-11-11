package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/hook"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// useFakeTmux makes the commands use a tmux.Fake and returns it.
func useFakeTmux(t *testing.T) *tmux.Fake {
	t.Helper()
	fake := &tmux.Fake{}
	prev := newTmuxClient
	newTmuxClient = func() tmux.Client { return fake }
	t.Cleanup(func() { newTmuxClient = prev })
	return fake
}

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

func TestHookAppliesMarker(t *testing.T) {
	hookHome(t)
	fake := useFakeTmux(t)
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (model.Session, error) {
		return model.Session{ID: "A", TmuxWindow: "@7", State: model.StateWorking}, nil
	}
	t.Cleanup(func() { hookRun = prev })

	res := runHookCmd(t, strings.NewReader(`{}`), "hook", "Stop")
	assertQuietExitZero(t, res)
	want := []string{"SetWindowOption @7 @curia_marker "}
	if !slices.Equal(fake.Calls, want) {
		t.Errorf("calls = %q, want %q", fake.Calls, want)
	}
}

func TestHookWithoutWindowAppliesNothing(t *testing.T) {
	hookHome(t)
	fake := useFakeTmux(t)
	res := runHookCmd(t, strings.NewReader(`{"session_id":"A","cwd":"`+t.TempDir()+`"}`), "hook", "SessionStart")
	assertQuietExitZero(t, res)
	if len(fake.Calls) != 0 {
		t.Errorf("calls = %q, want none", fake.Calls)
	}
}

// failingTmux fails every marker write.
type failingTmux struct{ tmux.Fake }

func (f *failingTmux) SetWindowOption(context.Context, string, string, string) error {
	return errors.New("no server")
}

func TestHookAttentionFailureIsLoggedExitZero(t *testing.T) {
	hookHome(t)
	prevC := newTmuxClient
	newTmuxClient = func() tmux.Client { return &failingTmux{} }
	t.Cleanup(func() { newTmuxClient = prevC })
	prev := hookRun
	hookRun = func(context.Context, model.EventKind, []byte, hook.Env, *store.Store, config.Config) (model.Session, error) {
		return model.Session{ID: "A", TmuxWindow: "@7"}, nil
	}
	t.Cleanup(func() { hookRun = prev })

	res := runHookCmd(t, strings.NewReader(`{}`), "hook", "Stop")
	assertQuietExitZero(t, res)
	if len(res.log) != 1 || !strings.Contains(res.log[0], "no server") {
		t.Errorf("log = %q, want one line with the failure", res.log)
	}
}

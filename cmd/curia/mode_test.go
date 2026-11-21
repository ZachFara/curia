package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func modeReset(t *testing.T) {
	t.Helper()
	modeFlags.all, modeFlags.repo, modeFlags.forDur, modeFlags.native = false, "", 0, false
	t.Cleanup(func() { modeFlags.all, modeFlags.repo, modeFlags.forDur, modeFlags.native = false, "", 0, false })
}

// modeEnv is a user at a terminal typing typed at the bypass prompt, and a
// store with idle session "s1" (repo /r, worktree /wt) and working "s2".
func modeEnv(t *testing.T, typed string) *store.Store {
	t.Helper()
	modeReset(t)
	db := hookHome(t)
	userTerminalAs(t, true)
	old := setupConfirm
	setupConfirm = func(string) (string, error) { return typed, nil }
	t.Cleanup(func() { setupConfirm = old })
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	for _, s := range []model.Session{
		{ID: "s1", Agent: "claude", Repo: "/r", Worktree: "/wt", Cwd: "/wt", State: model.StateIdle, StartedAt: now, UpdatedAt: now, SeenAt: now},
		{ID: "s2", Agent: "claude", Repo: "/r", Cwd: "/r", State: model.StateWorking, StartedAt: now, UpdatedAt: now, SeenAt: now},
	} {
		if err := st.UpsertSession(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func modeOf(t *testing.T, st *store.Store, id string) (string, time.Time) {
	t.Helper()
	s, _, err := st.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.Mode, s.ModeUntil
}

func TestModeSetAndShow(t *testing.T) {
	st := modeEnv(t, "")
	out, err := runRoot(t, "mode", "away", "--all", "--for", "6h")
	if err != nil || !strings.Contains(out, "2 live session(s) of everyone now in mode away") {
		t.Fatalf("away: %q, %v", out, err)
	}
	if m, until := modeOf(t, st, "s1"); m != "away" || until.Before(time.Now().Add(5*time.Hour)) {
		t.Errorf("s1 mode = %q until %v", m, until)
	}
	out, err = runRoot(t, "mode")
	if err != nil || !strings.Contains(out, "default mode: normal") || strings.Count(out, "away until") != 2 {
		t.Errorf("show: %q, %v", out, err)
	}
	// Leaving away says it cannot restore Claude Code's own mode.
	modeReset(t)
	out, err = runRoot(t, "mode", "normal", "--all")
	if err != nil || !strings.Contains(out, "cannot restore Claude Code's own permission mode") {
		t.Errorf("leaving away: %q, %v", out, err)
	}
	if m, _ := modeOf(t, st, "s2"); m != "normal" {
		t.Errorf("s2 mode = %q", m)
	}
}

func TestModeScopeOutsideARepoIsAnError(t *testing.T) {
	st := modeEnv(t, "")
	t.Chdir(t.TempDir())
	if _, err := runRoot(t, "mode", "edits"); err == nil || !strings.Contains(err.Error(), "not inside a git repository") {
		t.Errorf("err = %v", err)
	}
	if m, _ := modeOf(t, st, "s1"); m != "" {
		t.Errorf("mode changed to %q", m)
	}
}

func TestModeBypassNeedsTheTypedWord(t *testing.T) {
	st := modeEnv(t, "yes")
	out, err := runRoot(t, "mode", "bypass", "--all")
	if err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("wrong word: %q, %v", out, err)
	}
	if m, _ := modeOf(t, st, "s1"); m != "" {
		t.Errorf("mode = %q after a wrong word", m)
	}
	if !strings.Contains(out, "Still prompting") || !strings.Contains(out, "deny rule") {
		t.Errorf("bypass did not say what still prompts: %q", out)
	}
}

func TestModeBypassWithWordExpiresAndNeverEmitsTheNativeFlag(t *testing.T) {
	st := modeEnv(t, "bypass")
	out, err := runRoot(t, "mode", "bypass", "--all")
	if err != nil {
		t.Fatal(err)
	}
	m, until := modeOf(t, st, "s1")
	if m != "bypass" || until.Before(time.Now().Add(time.Hour)) || until.After(time.Now().Add(3*time.Hour)) {
		t.Errorf("mode = %q until %v, want bypass for the default 2h", m, until)
	}
	if strings.Contains(out, "dangerously-skip-permissions") {
		t.Errorf("plain bypass printed the native flag: %q", out)
	}
}

func TestModeNative(t *testing.T) {
	modeEnv(t, "bypass")
	if _, err := runRoot(t, "mode", "bypass", "--all", "--native"); err == nil || !strings.Contains(err.Error(), "--native needs --for") {
		t.Errorf("native without --for: %v", err)
	}
	modeReset(t)
	out, err := runRoot(t, "mode", "bypass", "--all", "--native", "--for", "1h")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cd '/wt' && claude --resume s1 --dangerously-skip-permissions") || !strings.Contains(out, "skipped s2: not idle") ||
		!strings.Contains(out, "types nothing") {
		t.Errorf("native report: %q", out)
	}
}

func TestModeAgentCannotSet(t *testing.T) {
	st := modeEnv(t, "bypass")
	for _, env := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID"} {
		t.Setenv(env, "1")
		for _, mode := range []string{"normal", "edits", "away", "bypass"} {
			modeReset(t)
			if _, err := runRoot(t, "mode", mode, "--all", "--for", "1h"); err == nil || !strings.Contains(err.Error(), "only the user may") {
				t.Errorf("%s under %s: err = %v", mode, env, err)
			}
		}
		t.Setenv(env, "")
	}
	if m, _ := modeOf(t, st, "s1"); m != "" {
		t.Errorf("an agent changed the mode to %q", m)
	}
}

// The flag appears in exactly one non-test source file: mode.go.
func TestModeNativeFlagOnlyInModeGo(t *testing.T) {
	var found []string
	for _, pat := range []string{"*.go", "../../internal/*/*.go"} {
		files, _ := filepath.Glob(pat)
		for _, p := range files {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			if b, _ := os.ReadFile(filepath.Clean(p)); strings.Contains(string(b), "dangerously-skip-permissions") {
				found = append(found, filepath.Base(p))
			}
		}
	}
	if len(found) != 1 || found[0] != "mode.go" {
		t.Errorf("the native flag is in %v, want only mode.go", found)
	}
}

func TestModeBypassShowsInTheBadgeOnlyWhileLive(t *testing.T) {
	st := modeEnv(t, "")
	if _, err := st.SetModes(context.Background(), store.ModeScope{All: true}, "bypass", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if badge, err := statusBadge(context.Background(), nil); err != nil || !strings.Contains(badge, "BYPASS") {
		t.Errorf("badge = %q, %v", badge, err)
	}
	if _, err := st.SetModes(context.Background(), store.ModeScope{All: true}, "bypass", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if badge, _ := statusBadge(context.Background(), nil); strings.Contains(badge, "BYPASS") {
		t.Errorf("badge = %q after expiry", badge)
	}
}

func TestModeSaysWhichSessionsHaveNoRoot(t *testing.T) {
	st := modeEnv(t, "")
	now := time.Now()
	if err := st.UpsertSession(context.Background(), model.Session{ID: "s3", Agent: "claude", Cwd: "/x", State: model.StateIdle, StartedAt: now, UpdatedAt: now, SeenAt: now}); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, "mode", "edits", "--all")
	if err != nil || !strings.Contains(out, "1 session(s) have no repository or worktree") || !strings.Contains(out, "s3") {
		t.Errorf("out = %q, %v", out, err)
	}
}

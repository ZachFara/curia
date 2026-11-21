package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
)

func detailSeed(t *testing.T, s model.Session) detailSource {
	t.Helper()
	st := openTestStore(t, filepath.Join(t.TempDir(), "curia.db"))
	if err := st.UpsertSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return detailSource{st: st, cfg: config.Default()}
}

func TestDetailSourceReadsATailThatChangesAndNeverTheWholeFile(t *testing.T) {
	tp := filepath.Join(t.TempDir(), "t.jsonl")
	line := func(s string) string {
		return `{"type":"assistant","message":{"content":[{"type":"text","text":"` + s + `"}]}}` + "\n"
	}
	huge := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("a", 4<<20) + `"}]}}` + "\n"
	first := line("one") + huge
	if err := os.WriteFile(tp, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	d := detailSeed(t, model.Session{ID: "s1", Agent: "claude", State: model.StateEnded, TranscriptPath: tp, UpdatedAt: time.Now()})
	got, err := d.Detail(context.Background(), "s1")
	if err != nil || len(got.Tail) != 0 || got.TailNote == "" {
		t.Fatalf("huge line: %v %v %q", err, got.Tail, got.TailNote)
	}
	if err = os.WriteFile(tp, []byte(first+line("two \\u001b[31mred")), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ = d.Detail(context.Background(), "s1"); len(got.Tail) != 1 || got.Tail[0] != "two red" {
		t.Fatalf("appended line: %q", got.Tail)
	}
	_ = os.Remove(tp) // rotated away
	if got, err = d.Detail(context.Background(), "s1"); err != nil || len(got.Tail) != 0 || got.TailNote == "" {
		t.Fatalf("missing transcript: %v %q %q", err, got.Tail, got.TailNote)
	}
}

func TestDetailSourceNoWorktreeNoDiffAndNoActivityYet(t *testing.T) {
	d := detailSeed(t, model.Session{ID: "s1", Agent: "claude", State: model.StateIdle, UpdatedAt: time.Now()})
	if diff, err := d.Diff(context.Background(), "s1"); err != nil || diff.Has {
		t.Fatalf("diff = %+v, %v", diff, err)
	}
	got, err := d.Detail(context.Background(), "s1")
	if err != nil || got.ActivityOK {
		t.Fatalf("activity shown without F27: %+v %v", got, err)
	}
	if _, err := d.Detail(context.Background(), "nope"); err == nil {
		t.Fatal("unknown session did not fail")
	}
}

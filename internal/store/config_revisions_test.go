package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/store"
)

func TestConfigRevisionsAppendOnly(t *testing.T) {
	ctx := context.Background()
	s := open(t, dbPath(t))
	for i, f := range []string{"a = 1\n", "a = 2\n"} {
		n, err := s.AddConfigRevision(ctx, store.ConfigRevision{At: time.Now(), Actor: "cli", Key: "a", Old: "x", New: "y", File: f})
		if err != nil || n != int64(i+1) {
			t.Fatalf("n = %d, %v", n, err)
		}
	}
	got, err := s.ConfigRevisions(ctx, 10)
	if err != nil || len(got) != 2 || got[0].N != 2 || got[1].File != "a = 1\n" {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if _, ok, _ := s.ConfigRevision(ctx, 9); ok {
		t.Error("revision 9 exists")
	}
	if r, ok, _ := s.ConfigRevision(ctx, 1); !ok || r.Actor != "cli" || r.At.IsZero() {
		t.Errorf("revision 1 = %+v %v", r, ok)
	}
	if n, _ := s.ConfigAck(ctx); n != 0 {
		t.Errorf("ack = %d", n)
	}
	if err = s.AckConfig(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.ConfigAck(ctx); n != 2 {
		t.Errorf("ack = %d", n)
	}
}

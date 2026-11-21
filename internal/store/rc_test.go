package store_test

import (
	"context"
	"testing"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

func TestClaimRCFollowsTheTypingRulesAndBlocksASecondLine(t *testing.T) {
	s := open(t, t.TempDir()+"/c.db")
	ctx := context.Background()
	liveSession(t, s, "idle", "%1", model.StateIdle)
	liveSession(t, s, "busy", "%2", model.StateWorking)

	if c, err := s.ClaimRC(ctx, "busy", t0); err != nil || c.Refusal != store.TypingWorking {
		t.Errorf("working session: refusal %v err %v, want TypingWorking", c.Refusal, err)
	}
	c, err := s.ClaimRC(ctx, "idle", t0)
	if err != nil || c.Refusal != store.TypingClaimed {
		t.Fatalf("idle session: refusal %v err %v, want claimed", c.Refusal, err)
	}
	// The claim itself is the marker: a second claim before the first settles is refused.
	if c, err = s.ClaimRC(ctx, "idle", t0); err != nil || c.Refusal != store.TypingUnsubmitted || c.Blocker != "/rc" {
		t.Errorf("second claim: refusal %v blocker %q err %v, want TypingUnsubmitted by /rc", c.Refusal, c.Blocker, err)
	}
	// Settled as typed-but-unconfirmed it still blocks; settled as seen on it is refused as On.
	if err = s.SetRC(ctx, "idle", "unknown", "unconfirmed", t0); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.ClaimRC(ctx, "idle", t0); c.Refusal != store.TypingUnsubmitted {
		t.Errorf("after an unconfirmed line: refusal %v, want TypingUnsubmitted", c.Refusal)
	}
	if err = s.SetRC(ctx, "idle", "on", "indicator", t0); err != nil {
		t.Fatal(err)
	}
	if c, _ = s.ClaimRC(ctx, "idle", t0); !c.On {
		t.Errorf("a session known on was claimed: %+v", c)
	}
	got, _, _ := s.GetSession(ctx, "idle")
	if got.RC != "on" || got.RCHow != "indicator" || !got.RCCheckedAt.Equal(t0) {
		t.Errorf("stored rc = %q %q %v, want on, indicator, %v", got.RC, got.RCHow, got.RCCheckedAt, t0)
	}
}

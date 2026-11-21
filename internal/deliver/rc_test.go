package deliver_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// rcFix is an idle session s1 in live pane %5 with the empty prompt showing.
func rcFix(t *testing.T, s model.Session, panes ...tmux.Pane) (*store.Store, *tmux.Fake) {
	t.Helper()
	st := openStore(t)
	seedSession(t, st, s)
	return st, &tmux.Fake{Screen: idleScreen, Panes: panes}
}

func typeRC(t *testing.T, st *store.Store, tm tmux.Client) (bool, string) {
	t.Helper()
	log, _ := testLog()
	return deliver.TypeRC(context.Background(), st, tm, log, "s1", "/rc", now)
}

func TestTypeRCTypesOnceAndThenWaitsForTheCheck(t *testing.T) {
	s := idleTarget()
	st, fake := rcFix(t, s, livePane)
	if typed, why := typeRC(t, st, fake); !typed {
		t.Fatalf("first /rc not typed: %s", why)
	}
	typed, why := typeRC(t, st, fake)
	if typed || !strings.Contains(why, "earlier /rc is not confirmed") {
		t.Errorf("second TypeRC = %v, %q, want refused until the first is confirmed", typed, why)
	}
	if got := sendKeysCalls(fake.Calls); !slices.Equal(got, []string{"SendKeys %5 /rc"}) {
		t.Errorf("SendKeys calls = %q, want one /rc", got)
	}
}

func TestTypeRCNeverReadsOrTypesIntoAPaneOfAnotherServer(t *testing.T) {
	st, fake := rcFix(t, askerOnIdle(socketA), paneOn(socketB))
	typed, why := typeRC(t, st, fake)
	if typed || !strings.Contains(why, "another tmux server") {
		t.Errorf("TypeRC = %v, %q, want refused for another server", typed, why)
	}
	if got := paneCalls(fake.Calls); len(got) != 0 {
		t.Errorf("pane calls = %q, want none: pane %%5 of the other server is not the agent's", got)
	}
}

func askerOnIdle(socket string) model.Session {
	s := askerOn(socket)
	s.State = model.StateIdle
	return s
}

// workingAfterCapture moves s1 to working once its pane was read, as a hook
// would between the capture and the typing.
type workingAfterCapture struct {
	*tmux.Fake
	st *store.Store
}

func (w workingAfterCapture) CapturePane(ctx context.Context, pane string) (tmux.Capture, error) {
	c, err := w.Fake.CapturePane(ctx, pane)
	if err != nil {
		return c, err
	}
	err = w.st.UpdateSession(ctx, "s1", func(s model.Session, _ bool) model.Session {
		s.State = model.StateWorking
		return s
	})
	return c, err
}

func TestTypeRCRechecksTheSessionAfterReadingThePane(t *testing.T) {
	st, fake := rcFix(t, idleTarget(), livePane)
	typed, _ := typeRC(t, st, workingAfterCapture{fake, st})
	if typed || len(sendKeysCalls(fake.Calls)) != 0 {
		t.Errorf("typed /rc into a session that started working: %q", sendKeysCalls(fake.Calls))
	}
}

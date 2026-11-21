package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// The literals below are the documented bounds, not the constants: a constant changed to a huge value must fail here.

func servewebTicketAt(t *testing.T, st *store.Store, question string, at time.Time) {
	t.Helper()
	d := model.Decision{
		SessionID: "S1", Repo: "/r/api", Question: question, Options: []model.Option{{Key: "A", Label: "a"}, {Key: "B", Label: "b"}},
		Recommendation: "B", Rationale: "Because.", Reversibility: model.ReversCheap, Mode: model.ModeBlocking, Scope: model.ScopeRepo,
		CreatedAt: at, ArgsHash: question,
	}
	if _, _, err := st.CreateDecision(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

// Five wrong tokens lock an address for the rest of the minute only: the next minute starts at zero, so the owner's own device is not locked out for good.
func TestServewebLimiterWindowResets(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	post := func() int {
		return f.do(http.MethodPost, "/login", "", url.Values{"token": {strings.Repeat("z", 43)}}, func(r *http.Request) { r.Header.Set("Origin", f.origin()) }).StatusCode
	}
	for minute := range 3 {
		for i := range 5 {
			if got := post(); got != http.StatusUnauthorized {
				t.Fatalf("minute %d, wrong token %d = %d, want 401 (the window did not start at zero)", minute, i+1, got)
			}
		}
		if got := post(); got != http.StatusTooManyRequests {
			t.Fatalf("minute %d, sixth wrong token = %d, want 429", minute, got)
		}
		f.now.Store(f.clock().Add(time.Minute).UnixNano())
	}
}

func TestServewebLimiterTableHolds128Addresses(t *testing.T) {
	l := servewebNewLimiter(5)
	for i := range 1000 {
		l.failed(fmt.Sprintf("10.0.%d.%d", i/250, i%250), serveT0)
	}
	if n := l.size(); n > 128 {
		t.Errorf("the limiter holds %d addresses, want at most 128", n)
	}
}

func TestServewebSessionCapIs64(t *testing.T) {
	w := servewebNew(servewebOpts{Now: func() time.Time { return serveT0 }, TTL: time.Hour, PublicURL: "http://x"})
	for range 70 {
		if _, err := w.newSession("phone", "hash", serveT0); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(w.sessions); n != 64 {
		t.Errorf("%d sessions held, want 64", n)
	}
}

// A login body over 1 KiB is refused, even with the right token in it.
func TestServewebLoginBodyIsBoundedTo1KiB(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	tok := f.device("phone")
	post := func(pad int) int {
		return f.do(http.MethodPost, "/login", "", url.Values{"token": {tok}, "pad": {strings.Repeat("p", pad)}}, func(r *http.Request) { r.Header.Set("Origin", f.origin()) }).StatusCode
	}
	if got := post(100); got != http.StatusSeeOther {
		t.Fatalf("a small login = %d, want 303", got)
	}
	if got := post(2000); got != http.StatusUnauthorized {
		t.Errorf("a 2 KB login body = %d, want it refused (401)", got)
	}
}

func servewebGet(t *testing.T, f *servewebFix, cookie, p string) string {
	t.Helper()
	r := f.do(http.MethodGet, p, cookie, nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", p, r.StatusCode)
	}
	return r.body
}

// 100 cards a column (the rest counted), and card text cut near 120 characters.
func TestServewebColumnCapAndTextLimit(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	ctx := context.Background()
	for i := range 103 {
		if _, err := f.st.CreateTask(ctx, model.Task{Title: fmt.Sprintf("cardmark-%03d", i), Repo: "/r/api", Status: model.TaskReady, CreatedAt: serveT0.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.CreateTask(ctx, model.Task{Title: strings.Repeat("a", 150) + "TAILMARK", Repo: "/r/api", Status: model.TaskBacklog, CreatedAt: serveT0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	board := servewebGet(t, f, f.login(f.device("phone")), "/")
	if n := strings.Count(board, "cardmark-"); n != 100 || !strings.Contains(board, "and 3 more") {
		t.Errorf("board shows %d cards of a 103-card column (want 100, and \"and 3 more\")", n)
	}
	if strings.Contains(board, "TAILMARK") || !strings.Contains(board, strings.Repeat("a", 100)) {
		t.Error("a card title is not cut near 120 characters")
	}
}

func servewebAnswerAll(t *testing.T, f *servewebFix) {
	t.Helper()
	ds, err := f.st.ListDecisions(context.Background(), store.DecisionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds { // answered long ago: no new timeline row inside the window
		if _, err := f.st.AnswerDecision(context.Background(), d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, serveT0.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}

// 200 rows on the timeline.
func TestServewebTimelineRowCap(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	for i := range 205 {
		servewebTicketAt(t, f.st, fmt.Sprintf("rowmark-%03d", i), serveT0.Add(-time.Hour))
	}
	servewebAnswerAll(t, f)
	cookie := f.login(f.device("phone"))
	if n := strings.Count(servewebGet(t, f, cookie, "/timeline"), "rowmark-"); n != 200 {
		t.Errorf("timeline shows %d rows, want 200", n)
	}
	// The decision log's own query returns 50 rows by default, so its 200-row cap is never reached.
}

// The timeline holds the last 24 hours, and its text is cut near 120 characters.
func TestServewebTimelineWindowAndTextLimit(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	servewebTicketAt(t, f.st, "oldmark ticket", serveT0.Add(-25*time.Hour))
	servewebTicketAt(t, f.st, "newmark ticket "+strings.Repeat("a", 150)+"TAILMARK", serveT0.Add(-23*time.Hour))
	cookie := f.login(f.device("phone"))
	tl := servewebGet(t, f, cookie, "/timeline")
	if !strings.Contains(tl, "newmark") || strings.Contains(tl, "oldmark") {
		t.Errorf("timeline shows the 23-hour-old ticket %v and the 25-hour-old one %v, want true and false", strings.Contains(tl, "newmark"), strings.Contains(tl, "oldmark"))
	}
	if strings.Contains(tl, "TAILMARK") {
		t.Error("a timeline line is not cut near 120 characters")
	}
}

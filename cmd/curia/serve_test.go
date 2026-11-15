package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// serveT0 is the handler tests' clock start.
var serveT0 = time.Date(2026, 11, 14, 12, 0, 30, 0, time.UTC)

// serveFix is one endpoint under test: serveHandler on the server
// serveNewServer builds, listening on 127.0.0.1 (httptest), over a real
// store and a tmux.Fake whose pane %1 holds session S1, idle at Claude
// Code's empty prompt. Every token it mints, every response body and the
// handler's log are checked at cleanup: no body and no log line may hold
// a token.
type serveFix struct {
	t    *testing.T
	st   *store.Store
	db   string
	fake *tmux.Fake
	log  *pushLogBuffer
	srv  *httptest.Server

	now atomic.Int64 // the handler's clock, in Unix nanoseconds

	mu      sync.Mutex
	tokens  []string
	bodies  []string
	devices int
}

// serveNewFix starts an endpoint whose tmux client is a tmux.Fake.
func serveNewFix(t *testing.T) *serveFix {
	t.Helper()
	fake := &tmux.Fake{
		Panes:  []tmux.Pane{{ID: "%1", Session: "work", WindowID: "@1", ServerStart: serveT0.Add(-time.Hour)}},
		Screen: decideIdleScreen,
	}
	f := serveNewFixWith(t, fake)
	f.fake = fake
	return f
}

// serveNewFixWith starts an endpoint on tm.
func serveNewFixWith(t *testing.T, tm tmux.Client) *serveFix {
	t.Helper()
	st, db := pushStore(t)
	logger, buf := pushLogger()
	f := &serveFix{t: t, st: st, db: db, log: buf}
	f.now.Store(serveT0.UnixNano())
	if err := st.UpsertSession(context.Background(), model.Session{
		ID: "S1", Agent: "claude", State: model.StateIdle, Pane: "%1", TmuxSession: "work",
		TmuxWindow: "@1", StartedAt: serveT0.Add(-30 * time.Minute), UpdatedAt: serveT0.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	h := serveHandler(serveDeps{Store: st, Tmux: tm, Log: logger, Now: f.clock})
	f.srv = httptest.NewUnstartedServer(h)
	f.srv.Config = serveNewServer(h, logger)
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	t.Cleanup(f.checkNoTokenLeaked)
	return f
}

func (f *serveFix) clock() time.Time { return time.Unix(0, f.now.Load()).UTC() }

func (f *serveFix) setNow(at time.Time) { f.now.Store(at.UnixNano()) }

// ticket stores an open blocking ticket of session S1 with options A and
// B (labels "Option A", "Option B").
func (f *serveFix) ticket(rev model.Reversibility) model.Decision {
	f.t.Helper()
	return pushTicket(f.t, f.st, fmt.Sprintf("Question %d?", time.Now().UnixNano()), rev, 2)
}

// mint makes a real one-time token for a push of (kind, ref): the push is
// queued for a device of its own, claimed with the token's hash at at and,
// when sent is true, marked sent at at. It returns the plain token.
func (f *serveFix) mint(kind model.PushKind, ref string, at time.Time, sent bool) string {
	f.t.Helper()
	ctx := context.Background()
	plain, hash, err := notify.NewToken(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.devices++
	dev := fmt.Sprintf("d%d", f.devices)
	f.tokens = append(f.tokens, plain)
	f.mu.Unlock()
	if _, eerr := f.st.EnqueuePushes(ctx, kind, ref, []string{dev}, at, at); eerr != nil {
		f.t.Fatal(eerr)
	}
	p, found, err := f.st.ClaimPush(ctx, hash, at)
	if err != nil || !found || p.Ref != ref || p.Device != dev {
		f.t.Fatalf("claim the push of %s: %+v found=%v err=%v", ref, p, found, err)
	}
	if sent {
		if err := f.st.FinishPush(ctx, p.N, model.PushSent, "", time.Time{}, at); err != nil {
			f.t.Fatal(err)
		}
	}
	return plain
}

// unknownToken is a well-formed token that no push holds; it is checked
// for leaks like the minted ones.
func (f *serveFix) unknownToken() string {
	f.t.Helper()
	plain, _, err := notify.NewToken(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.tokens = append(f.tokens, plain)
	f.mu.Unlock()
	return plain
}

// serveReq is one request to the endpoint.
type serveReq struct {
	method, path string
	auth         []string // Authorization header values; nil for none
	ctype        string   // "" for no Content-Type
	body         string
}

// serveAnswerReq is the request a phone's button sends: the token, the
// ticket and the key.
func serveAnswerReq(token, id, key string) serveReq {
	return serveReq{
		method: http.MethodPost, path: "/v1/answer", auth: []string{"Bearer " + token},
		ctype: "application/json", body: fmt.Sprintf(`{"decision":%q,"key":%q}`, id, key),
	}
}

// do sends r and returns the status, the body and the response headers.
func (f *serveFix) do(r serveReq) (int, string, http.Header) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), r.method, f.srv.URL+r.path, strings.NewReader(r.body))
	if err != nil {
		f.t.Fatal(err)
	}
	for _, a := range r.auth {
		req.Header.Add("Authorization", a)
	}
	if r.ctype != "" {
		req.Header.Set("Content-Type", r.ctype)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", r.method, r.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	return resp.StatusCode, string(b), resp.Header
}

// raw writes request bytes on a new connection and returns the reply.
func (f *serveFix) raw(request []byte) string {
	f.t.Helper()
	c, err := net.Dial("tcp", f.srv.Listener.Addr().String())
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, werr := c.Write(request); werr != nil {
		f.t.Fatal(werr)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		f.t.Fatalf("read the reply: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	return fmt.Sprintf("%d %s", resp.StatusCode, b)
}

// checkNoTokenLeaked fails the test when a response body or the log holds
// a token the test made.
func (f *serveFix) checkNoTokenLeaked() {
	f.mu.Lock()
	defer f.mu.Unlock()
	logText := f.log.String()
	for _, tok := range f.tokens {
		if strings.Contains(logText, tok) || strings.Contains(logText, tok[:20]) {
			f.t.Errorf("the log holds a token: %s", logText)
		}
		for _, b := range f.bodies {
			if strings.Contains(b, tok) || strings.Contains(b, tok[:20]) {
				f.t.Errorf("a response holds a token: %q", b)
			}
		}
	}
}

// decision reads ticket id back.
func (f *serveFix) decision(id string) model.Decision {
	f.t.Helper()
	d, found, err := f.st.GetDecision(context.Background(), id)
	if err != nil || !found {
		f.t.Fatalf("get %s: found=%v err=%v", id, found, err)
	}
	return d
}

// spent reports whether the push holding token has been used.
func (f *serveFix) spent(token string) bool {
	f.t.Helper()
	raw, err := sql.Open("sqlite", f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var used bool
	if err := raw.QueryRow(`SELECT used_at IS NOT NULL FROM pushes WHERE token_hash = ?`, notify.HashToken(token)).Scan(&used); err != nil {
		f.t.Fatalf("read the push of a token: %v", err)
	}
	return used
}

// typed lists the fake's SendKeys calls.
func (f *serveFix) typed() []string {
	var out []string
	for _, c := range f.fake.Calls {
		if strings.HasPrefix(c, "SendKeys") {
			out = append(out, c)
		}
	}
	return out
}

const serveUnauthorized = `{"error":"unauthorized"}`

// ---- the brief's tests ----

func TestServeRoutes(t *testing.T) {
	f := serveNewFix(t)
	code, body, h := f.do(serveReq{method: http.MethodGet, path: "/healthz"})
	if code != http.StatusOK || body != "ok" || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || h.Get("Cache-Control") != "no-store" {
		t.Errorf("GET /healthz = %d %q %v", code, body, h)
	}
	code, body, h = f.do(serveReq{method: http.MethodGet, path: "/v1/answer"})
	if code != http.StatusMethodNotAllowed || body != "" || h.Get("Allow") != http.MethodPost {
		t.Errorf("GET /v1/answer = %d %q allow %q, want 405, empty, POST", code, body, h.Get("Allow"))
	}
	for _, p := range []string{"/v1/review", "/v1/reviews/R-1/approve", "/", "/v1/answer/x"} {
		if code, body, _ := f.do(serveReq{method: http.MethodPost, path: p, ctype: "application/json", body: "{}"}); code != http.StatusNotFound || body != "" {
			t.Errorf("POST %s = %d %q, want 404 and no body", p, code, body)
		}
	}
	// "OPTIONS *" is not answered by net/http's own handler either.
	if got := f.raw([]byte("OPTIONS * HTTP/1.1\r\nHost: x\r\n\r\n")); got != "404 " {
		t.Errorf("OPTIONS * = %q, want 404 and no body", got)
	}
}

func TestServeRoutesAreExactlyTwo(t *testing.T) {
	f := serveNewFix(t)
	// The handler's routes are exactly two: every other method and path is
	// a 404 or a 405 with an empty body, never a redirect.
	paths := []string{
		"/", "/healthz", "/healthz/", "/v1/answer", "/v1/answer/", "/v1/answer/x", "/v1/review",
		"/v1/reviews/R-1/approve", "/v1/decisions", "/v1/tickets", "/V1/ANSWER", "/v1/%61nswer",
		"//v1/answer", "/v1/./answer", "/v1/x/../answer", "/debug/pprof/", "/index.html", "/favicon.ico", "/v1",
	}
	methods := []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, "PROPFIND",
	}
	var served []string
	for _, p := range paths {
		for _, m := range methods {
			code, body, h := f.do(serveReq{method: m, path: p})
			switch code {
			case http.StatusNotFound, http.StatusMethodNotAllowed:
				if body != "" || h.Get("Location") != "" || h.Get("Access-Control-Allow-Origin") != "" {
					t.Errorf("%s %s = %d %q %v, want an empty body and no Location or CORS header", m, p, code, body, h)
				}
			default:
				served = append(served, m+" "+p)
			}
		}
	}
	if want := []string{"GET /healthz", "POST /v1/answer"}; strings.Join(served, ",") != strings.Join(want, ",") {
		t.Errorf("routes served = %q, want exactly %q", served, want)
	}
}

func TestAnswerHappyPath(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	code, body, h := f.do(serveAnswerReq(tok, d.ID, "B"))
	if code != http.StatusOK || body != `{"decision":"D-1","status":"answered","delivery":"delivered"}` {
		t.Fatalf("answer = %d %s", code, body)
	}
	if h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", h)
	}
	got := f.decision(d.ID)
	if got.Status != model.StatusAnswered || got.AnswerKey != "B" || got.AnsweredVia != model.AnsweredNtfy ||
		got.Delivery != model.DeliveryDelivered || got.DeliveredVia != model.ViaSendKeys {
		t.Errorf("ticket = %+v", got)
	}
	if typed := f.typed(); len(typed) != 1 || !strings.Contains(typed[0], "Decision D-1") || !strings.Contains(typed[0], "Option B") {
		t.Errorf("typed = %q, want the answer typed once", typed)
	}
	if !f.spent(tok) {
		t.Error("used_at is not set")
	}
}

func TestAnswerQueuedWhenAgentBusy(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	if err := f.st.UpdateSession(context.Background(), "S1", func(s model.Session, _ bool) model.Session {
		s.State = model.StateWorking
		return s
	}); err != nil {
		t.Fatal(err)
	}
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A"))
	var r struct{ Decision, Status, Delivery string }
	if err := json.Unmarshal([]byte(body), &r); err != nil || code != http.StatusOK {
		t.Fatalf("answer = %d %s (%v)", code, body, err)
	}
	if r.Decision != d.ID || r.Status != "answered" || !strings.Contains(r.Delivery, "agent is working") || !strings.Contains(r.Delivery, "check_decision D-1") {
		t.Errorf("reply = %+v, want the queued note", r)
	}
	if typed := f.typed(); len(typed) != 0 {
		t.Errorf("typed = %q, want nothing", typed)
	}
	if got := f.decision(d.ID); got.Delivery != model.DeliveryQueued || got.AnsweredVia != model.AnsweredNtfy {
		t.Errorf("ticket = %+v", got)
	}
}

func TestAnswerAuthTable(t *testing.T) {
	f := serveNewFix(t)
	d1 := f.ticket(model.ReversCostly)
	d2 := f.ticket(model.ReversCostly)
	good := f.mint(model.PushDecision, d1.ID, serveT0, true)
	other := f.mint(model.PushDecision, d2.ID, serveT0, true)
	tk := mcpTask(t, f.st, "Retry webhooks")
	rv, _, err := f.st.CompleteTask(context.Background(), model.Review{
		TaskID: tk, SessionID: "S1", Summary: "Done.", Tests: model.TestsPassed, CreatedAt: serveT0,
	})
	if err != nil {
		t.Fatal(err)
	}
	review := f.mint(model.PushReview, rv.ID, serveT0, true)
	old := f.mint(model.PushDecision, d1.ID, serveT0.Add(-25*time.Hour), true)
	sending := f.mint(model.PushDecision, d1.ID, serveT0, false) // last: a sending row is reclaimable later
	unknown := f.unknownToken()

	body := fmt.Sprintf(`{"decision":%q,"key":"A"}`, d1.ID)
	rows := []struct {
		name string
		auth []string
		body string
	}{
		{"no header", nil, body},
		{"Basic", []string{"Basic x"}, body},
		{"42 characters", []string{"Bearer " + good[:42]}, body},
		{"44 characters", []string{"Bearer " + good + "A"}, body},
		{"a +", []string{"Bearer " + good[:42] + "+"}, body},
		{"lower-case scheme", []string{"bearer " + good}, body},
		{"two spaces", []string{"Bearer  " + good}, body},
		{"two headers", []string{"Bearer " + good, "Bearer " + good}, body},
		{"empty", []string{""}, body},
		{"unknown token", []string{"Bearer " + unknown}, body},
		{"another ticket's push", []string{"Bearer " + other}, body},
		{"a review push", []string{"Bearer " + review}, body},
		{"still sending", []string{"Bearer " + sending}, body},
		{"25 h old", []string{"Bearer " + old}, body},
		{"an unknown ticket", []string{"Bearer " + good}, `{"decision":"D-999","key":"A"}`},
	}
	for i, r := range rows {
		// One minute each, so the rate limit never answers instead.
		f.setNow(serveT0.Add(time.Duration(i) * time.Minute))
		code, got, h := f.do(serveReq{method: http.MethodPost, path: "/v1/answer", auth: r.auth, ctype: "application/json", body: r.body})
		if code != http.StatusUnauthorized || got != serveUnauthorized || h.Get("Content-Type") != "application/json" || h.Get("Www-Authenticate") != "" {
			t.Errorf("%s: %d %q %v, want 401 %s", r.name, code, got, h, serveUnauthorized)
		}
	}
	if got := f.decision(d1.ID); got.Status != model.StatusOpen {
		t.Errorf("D-1 = %s, want open", got.Status)
	}
	if f.spent(good) || f.spent(other) {
		t.Error("a refused request spent a token")
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("tmux calls = %q, want none", f.fake.Calls)
	}
	// The right token still works.
	if code, got, _ := f.do(serveAnswerReq(good, d1.ID, "A")); code != http.StatusOK {
		t.Errorf("the good token afterwards = %d %s", code, got)
	}
}

func TestAnswerTokenIsSingleUse(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	if code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A")); code != http.StatusOK {
		t.Fatalf("first = %d %s", code, body)
	}
	if code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A")); code != http.StatusUnauthorized || body != serveUnauthorized {
		t.Errorf("second = %d %s, want 401", code, body)
	}
	if typed := f.typed(); len(typed) != 1 {
		t.Errorf("typed %d times, want once", len(typed))
	}
}

// TestAnswerConcurrentSpend: of two identical requests sent at once over
// real connections, exactly one answers the ticket.
func TestAnswerConcurrentSpend(t *testing.T) {
	f := serveNewFix(t)
	for round := range 8 {
		f.setNow(serveT0.Add(time.Duration(round) * time.Minute))
		d := f.ticket(model.ReversCostly)
		tok := f.mint(model.PushDecision, d.ID, f.clock(), true)
		codes := make([]int, 2)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range codes {
			wg.Go(func() {
				<-start
				codes[i], _, _ = f.do(serveAnswerReq(tok, d.ID, "A"))
			})
		}
		close(start)
		wg.Wait()
		ok, refused := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusUnauthorized:
				refused++
			}
		}
		if ok != 1 || refused != 1 {
			t.Errorf("round %d: codes = %v, want one 200 and one 401", round, codes)
		}
		if got := f.decision(d.ID); got.Status != model.StatusAnswered || got.AnsweredVia != model.AnsweredNtfy {
			t.Errorf("round %d: ticket = %+v", round, got)
		}
	}
	// The first answer is typed; it stays unconfirmed, so the later ones
	// wait for the Stop hook (Ruling 86). Nothing is typed twice.
	if typed := f.typed(); len(typed) != 1 {
		t.Errorf("typed %d times, want once", len(typed))
	}
}

func TestAnswerBadRequests(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	good := `{"decision":"D-1","key":"A"}`
	rows := []struct{ name, ctype, body string }{
		{"text/plain", "text/plain", good},
		{"no content type", "", good},
		{"text/json", "text/json", good},
		{"application/jsonx", "application/jsonx", good},
		{"another charset", "application/json; charset=latin1", good},
		{"a broken content type", "application/json; charset", good},
		{"2 KiB", "application/json", `{"decision":"D-1","key":"` + strings.Repeat("A", 2048) + `"}`},
		{"1 KiB of trailing space", "application/json", good + strings.Repeat(" ", 1024)},
		{"empty", "application/json", ""},
		{"[]", "application/json", "[]"},
		{"a string", "application/json", `"D-1"`},
		{"null", "application/json", "null"},
		{"no key", "application/json", `{"decision":"D-1"}`},
		{"no decision", "application/json", `{"key":"A"}`},
		{"an extra field", "application/json", `{"decision":"D-1","key":"A","text":"free text"}`},
		{"a reason", "application/json", `{"decision":"D-1","key":"A","reason":"x"}`},
		{"two objects", "application/json", good + good},
		{"trailing junk", "application/json", good + "x"},
		{"broken JSON", "application/json", `{"decision":"D-1","key":"A"`},
		{"D-01", "application/json", `{"decision":"D-01","key":"A"}`},
		{"R-1", "application/json", `{"decision":"R-1","key":"A"}`},
		{"a number", "application/json", `{"decision":1,"key":"A"}`},
		{"null values", "application/json", `{"decision":null,"key":null}`},
		{"key A;B", "application/json", `{"decision":"D-1","key":"A;B"}`},
		{"key empty", "application/json", `{"decision":"D-1","key":""}`},
		{"key 9 chars", "application/json", `{"decision":"D-1","key":"ABCDEFGHI"}`},
		{"key with a space", "application/json", `{"decision":"D-1","key":"A "}`},
		{"key é", "application/json", `{"decision":"D-1","key":"é"}`},
		{"key escape", "application/json", `{"decision":"D-1","key":"A\u001b"}`},
		{"names in another case", "application/json", `{"Decision":"D-1","KEY":"A"}`},
		{"a duplicate key", "application/json", `{"decision":"D-1","key":"A","key":"B"}`},
		{"a duplicate decision", "application/json", `{"decision":"D-2","decision":"D-1","key":"A"}`},
		{"a nested object", "application/json", `{"decision":{"id":"D-1"},"key":"A"}`},
	}
	for _, r := range rows {
		req := serveAnswerReq(tok, d.ID, "A")
		req.ctype, req.body = r.ctype, r.body
		code, body, _ := f.do(req)
		if code != http.StatusBadRequest || body != `{"error":"bad request"}` {
			t.Errorf("%s: %d %s, want 400 bad request", r.name, code, body)
		}
	}
	if f.spent(tok) {
		t.Error("a bad request spent the token")
	}
	if got := f.decision(d.ID); got.Status != model.StatusOpen {
		t.Errorf("D-1 = %s, want open", got.Status)
	}
	// None of them counted as a failed token check.
	if code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A")); code != http.StatusOK {
		t.Errorf("the good request afterwards = %d %s", code, body)
	}
	if !strings.Contains(f.log.String(), `ctype=text/plain`) {
		t.Errorf("the log does not name the refused media type: %s", f.log.String())
	}
}

// TestAnswerReplaysProbeRequest replays the request probe N2 recorded for
// a button press (curl, from the action ntfy delivered), with a real
// token, and the same with the content types a phone app may send.
func TestAnswerReplaysProbeRequest(t *testing.T) {
	rec, err := os.ReadFile(filepath.Join("testdata", "serve-action-request.txt"))
	if err != nil {
		t.Fatal(err)
	}
	request, _, ok := bytes.Cut(rec, []byte("\n=====END-REQUEST====="))
	if !ok {
		t.Fatal("the recording has no end marker")
	}
	for _, ctype := range []string{"", "application/json; charset=utf-8", "Application/JSON", "application/json;charset=UTF-8"} {
		f := serveNewFix(t)
		d := f.ticket(model.ReversCostly)
		tok := f.mint(model.PushDecision, d.ID, serveT0, true)
		req := bytes.Replace(request, []byte("Bearer TOKEN"), []byte("Bearer "+tok), 1)
		if ctype != "" {
			req = bytes.Replace(req, []byte("Content-Type: application/json\r\n"), []byte("Content-Type: "+ctype+"\r\n"), 1)
		}
		if got := f.raw(req); got != `200 {"decision":"D-1","status":"answered","delivery":"delivered"}` {
			t.Errorf("replay with %q = %s", ctype, got)
		}
		if got := f.decision(d.ID); got.AnswerKey != "A" || got.AnsweredVia != model.AnsweredNtfy {
			t.Errorf("replay with %q: ticket = %+v", ctype, got)
		}
	}
}

func TestAnswerUnknownOption(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	code, body, _ := f.do(serveAnswerReq(tok, d.ID, "Z"))
	if code != http.StatusUnprocessableEntity || body != `{"error":"unknown option"}` {
		t.Errorf("key Z = %d %s", code, body)
	}
	if !f.spent(tok) {
		t.Error("the token is not spent")
	}
	if got := f.decision(d.ID); got.Status != model.StatusOpen {
		t.Errorf("D-1 = %s, want open", got.Status)
	}
	if len(f.typed()) != 0 {
		t.Error("something was typed")
	}
}

func TestAnswerNoLongerOpen(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	if _, err := f.st.AnswerDecision(context.Background(), d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, serveT0); err != nil {
		t.Fatal(err)
	}
	code, body, _ := f.do(serveAnswerReq(tok, d.ID, "B"))
	if code != http.StatusConflict || body != `{"error":"D-1 is no longer open"}` {
		t.Errorf("answered in between = %d %s", code, body)
	}
	if got := f.decision(d.ID); got.AnswerKey != "A" || got.AnsweredVia != model.AnsweredCLI {
		t.Errorf("the first answer changed: %+v", got)
	}
}

func TestAnswerIrreversibleRefused(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversIrreversible)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A"))
	if code != http.StatusUnprocessableEntity || body != `{"error":"answer this decision with curia decide"}` {
		t.Errorf("irreversible = %d %s", code, body)
	}
	if got := f.decision(d.ID); got.Status != model.StatusOpen {
		t.Errorf("D-1 = %s, want open", got.Status)
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("tmux calls = %q", f.fake.Calls)
	}
}

func TestAnswerRateLimit(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	for i := range serveMaxFailures {
		r := serveAnswerReq(f.unknownToken(), d.ID, "A")
		if i%2 == 1 {
			r.auth = []string{"Basic x"} // a malformed header counts too
		}
		if code, _, _ := f.do(r); code != http.StatusUnauthorized {
			t.Fatalf("wrong token %d = %d, want 401", i+1, code)
		}
	}
	for _, r := range []serveReq{serveAnswerReq(tok, d.ID, "A"), serveAnswerReq(f.unknownToken(), d.ID, "A"), {method: http.MethodPost, path: "/v1/answer"}} {
		if code, body, _ := f.do(r); code != http.StatusTooManyRequests || body != `{"error":"too many attempts"}` {
			t.Errorf("after %d failures = %d %s, want 429", serveMaxFailures, code, body)
		}
	}
	if f.spent(tok) {
		t.Error("a refused request spent the token")
	}
	// The window is the minute of the clock (serveT0 is 12:00:30).
	f.setNow(serveT0.Add(29 * time.Second))
	if code, _, _ := f.do(serveAnswerReq(tok, d.ID, "A")); code != http.StatusTooManyRequests {
		t.Errorf("at 12:00:59 = %d, want 429", code)
	}
	f.setNow(serveT0.Add(30 * time.Second))
	if code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A")); code != http.StatusOK {
		t.Errorf("at 12:01:00 = %d %s, want 200", code, body)
	}
}

func TestAnswerStoreErrorIsInternal(t *testing.T) {
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A"))
	if code != http.StatusInternalServerError || body != `{"error":"internal"}` {
		t.Errorf("closed store = %d %s", code, body)
	}
	if !strings.Contains(f.log.String(), "closed") {
		t.Errorf("the cause is not in the log: %s", f.log.String())
	}
}

func TestServeNeverLogsOrEchoesTheToken(t *testing.T) {
	// Every serveFix checks its tokens against every body and its log at
	// cleanup; this test sends one request of every kind through one.
	f := serveNewFix(t)
	d := f.ticket(model.ReversCostly)
	irr := f.ticket(model.ReversIrreversible)
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	irrTok := f.mint(model.PushDecision, irr.ID, serveT0, true)
	bad := serveAnswerReq(tok, d.ID, "A")
	bad.ctype = "text/plain; token=" + tok
	f.do(bad)
	echo := serveAnswerReq(tok, d.ID, "A")
	echo.body = `{"decision":"D-1","key":"A","token":"` + tok + `"}`
	f.do(echo)
	f.do(serveReq{method: http.MethodPost, path: "/v1/answer/" + tok, auth: []string{"Bearer " + tok}})
	f.do(serveReq{method: "GET", path: "/v1/answer?token=" + tok})
	f.do(serveAnswerReq(tok[:42], d.ID, "A"))
	f.do(serveAnswerReq(f.unknownToken(), d.ID, "A"))
	f.do(serveAnswerReq(irrTok, irr.ID, "A"))
	f.do(serveAnswerReq(tok, d.ID, "Z"))
	f.do(serveAnswerReq(tok, d.ID, "A")) // spent
	if strings.Contains(f.log.String(), notify.HashToken(tok)) {
		t.Error("the log holds a token's hash")
	}
	if !strings.Contains(f.log.String(), "serve: request") {
		t.Errorf("no request was logged: %s", f.log.String())
	}
}

func TestServeResponsesAreClean(t *testing.T) {
	f := serveNewFix(t)
	d, _, err := f.st.CreateDecision(context.Background(), model.Decision{
		SessionID: "S1", Repo: "/r", Question: "Which?", Options: []model.Option{
			{Key: "A", Label: "\x1b]8;;http://x\x07click\x1b]8;;\x07\x1b[2J"}, {Key: "B", Label: "B\u202e"},
		},
		Recommendation: "A", Reversibility: model.ReversCostly, Mode: model.ModeBlocking, Scope: model.ScopeRepo,
		CreatedAt: serveT0, ArgsHash: "clean",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.fake.Screen = "not a prompt \x1b[31m\n" // the answer is queued with a note
	tok := f.mint(model.PushDecision, d.ID, serveT0, true)
	code, body, _ := f.do(serveAnswerReq(tok, d.ID, "A"))
	if code != http.StatusOK {
		t.Fatalf("answer = %d %s", code, body)
	}
	var r struct{ Delivery string }
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.Delivery == "" || r.Delivery == "delivered" || len([]rune(r.Delivery)) > 200 {
		t.Errorf("delivery = %q, want a note of at most 200 runes", r.Delivery)
	}
	for _, s := range []string{body, r.Delivery} {
		if strings.ContainsFunc(s, func(c rune) bool { return unicode.IsControl(c) || c == '\u202e' }) {
			t.Errorf("reply holds a control rune: %q", s)
		}
	}
	// Today's notes are Curia's own words; a note carrying the label would
	// still be cleaned and bounded.
	b := serveAnswered(model.Outcome{Decision: d, Note: "label " + d.Options[0].Label + strings.Repeat("x", 500)})
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsFunc(r.Delivery, unicode.IsControl) || len([]rune(r.Delivery)) > 200 {
		t.Errorf("serveAnswered delivery = %q", r.Delivery)
	}
}

func TestServeListenAddrTable(t *testing.T) {
	ip := func(s string) net.Addr { return &net.IPNet{IP: net.ParseIP(s), Mask: net.CIDRMask(32, 32)} }
	lan, ts4, ts4b, ts6 := ip("192.168.1.5"), ip("100.101.102.103"), ip("100.64.0.9"), ip("fd7a:115c:a1e0::1")
	cases := []struct {
		name, flag, listen string
		addrs              []net.Addr
		want               string
	}{
		{"flag wins", "127.0.0.1:9000", "100.101.102.103:8787", []net.Addr{ts4}, "127.0.0.1:9000"},
		{"flag port 0", "127.0.0.1:0", "", nil, "127.0.0.1:0"},
		{"flag IPv6 loopback", "[::1]:9000", "", nil, "[::1]:9000"},
		{"flag Tailscale", "100.101.102.103:9000", "", nil, "100.101.102.103:9000"},
		{"flag mapped loopback", "[::ffff:127.0.0.1]:9000", "", nil, "127.0.0.1:9000"},
		{"then cfg", "", "100.101.102.103:8788", []net.Addr{ts4b}, "100.101.102.103:8788"},
		{"then the first 100.64/10 address", "", "", []net.Addr{lan, ip("127.0.0.1"), ts4, ts4b, ts6}, "100.101.102.103:8787"},
		{"an IPAddr", "", "", []net.Addr{&net.IPAddr{IP: net.ParseIP("100.64.0.9")}}, "100.64.0.9:8787"},
		{"then fd7a", "", "", []net.Addr{lan, ts6}, "[fd7a:115c:a1e0::1]:8787"},
		{"100.64/10 before an earlier fd7a", "", "", []net.Addr{ts6, lan, ts4b}, "100.64.0.9:8787"},
	}
	for _, c := range cases {
		got, err := serveListenAddr(c.flag, config.Serve{Listen: c.listen, PublicURL: "https://x.ts.net"}, c.addrs)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	_, err := serveListenAddr("", config.Serve{}, []net.Addr{lan, ip("127.0.0.1"), ip("100.128.0.1"), ip("fd7a:115c:a1e1::1")})
	if err == nil || !strings.Contains(err.Error(), "[serve] listen") {
		t.Errorf("none found = %v, want an error naming [serve] listen", err)
	}
}

func TestServeRefusesPublicAddress(t *testing.T) {
	listened := serveNoListen(t)
	for _, a := range []string{
		"0.0.0.0:8787", "192.168.1.5:8787", "[::]:8787", ":8787", "localhost:8787", "8.8.8.8:8787",
		"100.128.0.1:8787", "[fe80::1%en0]:8787", "[::1%lo0]:8787", "[2001:db8::1]:8787", "127.0.0.1",
		"[::ffff:0.0.0.0]:8787", "[::ffff:192.168.1.5]:8787",
	} {
		if _, err := serveListenAddr(a, config.Serve{}, nil); err == nil {
			t.Errorf("flag %q accepted", a)
		}
		// A configured address is checked again here, whatever the config
		// file allowed.
		if _, err := serveListenAddr("", config.Serve{Listen: a}, nil); err == nil {
			t.Errorf("[serve] listen %q accepted", a)
		}
	}
	serveConfig(t, "https://curia.example.ts.net", "")
	for _, a := range []string{"0.0.0.0:8787", "192.168.1.5:8787", "[::]:8787"} {
		err := serveRun(t, "--listen", a)
		if err == nil || !strings.Contains(err.Error(), "loopback or Tailscale") {
			t.Errorf("serve --listen %s = %v, want the refusal", a, err)
		}
	}
	// No Tailscale address on this machine and nothing configured: refused.
	prev := serveInterfaceAddrs
	serveInterfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.5"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	t.Cleanup(func() { serveInterfaceAddrs = prev })
	if err := serveRun(t); err == nil || !strings.Contains(err.Error(), "[serve] listen") {
		t.Errorf("serve with no address = %v", err)
	}
	if n := listened(); n != 0 {
		t.Errorf("listened %d times, want never", n)
	}
}

func TestServeNeedsPublicURL(t *testing.T) {
	listened := serveNoListen(t)
	db := serveConfig(t, "", "127.0.0.1:8787")
	err := serveRun(t)
	if err == nil || !strings.Contains(err.Error(), "[serve] public_url") {
		t.Errorf("serve with no public_url = %v, want an error naming [serve] public_url", err)
	}
	if listened() != 0 {
		t.Error("it listened")
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("it opened the store: %v", err)
	}
}

func TestServeRefusesInsideAgent(t *testing.T) {
	listened := serveNoListen(t)
	db := serveConfig(t, "https://curia.example.ts.net", "127.0.0.1:8787")
	t.Setenv("CLAUDECODE", "1")
	err := serveRun(t, "--listen", "127.0.0.1:0")
	if err == nil || !strings.Contains(err.Error(), "inside an agent session") {
		t.Errorf("serve inside an agent = %v", err)
	}
	if listened() != 0 {
		t.Error("it listened")
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("it opened the store: %v", err)
	}
}

func TestServeListensAndShutsDown(t *testing.T) {
	serveConfig(t, "https://curia.example.ts.net", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &pushLogBuffer{}
	done := make(chan error, 1)
	go func() { done <- serveExecute(ctx, out, "--listen", "127.0.0.1:0") }()
	addr := serveWaitAddr(t, out, done)
	resp, err := pushLoopbackClient.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "ok" {
		t.Errorf("/healthz = %d %q", resp.StatusCode, b)
	}
	text := out.String()
	if strings.Count(text, addr) != 1 || !strings.Contains(text, "anyone who can reach") || !strings.Contains(text, "no TLS") {
		t.Errorf("startup output = %q, want the address once and the warning", text)
	}
	// A second serve on the same address fails plainly.
	if err := serveRun(t, "--listen", addr); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("a second serve = %v, want address already in use", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve ended with %v, want nil", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("serve did not end within 6 s of its context")
	}
	if resp, err := pushLoopbackClient.Get("http://" + addr + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Error("still listening after shutdown")
	}
}

// TestServeFinishesInFlightOnShutdown: a request being answered when the
// command is told to stop is finished, and the command then ends.
func TestServeFinishesInFlightOnShutdown(t *testing.T) {
	db := serveConfig(t, "https://curia.example.ts.net", "")
	st := mcpOpenStore(t, db)
	block := &serveBlockingTmux{entered: make(chan struct{}, 8), release: make(chan struct{})}
	prev := newTmuxClient
	newTmuxClient = func() tmux.Client { return block }
	t.Cleanup(func() { newTmuxClient = prev })
	f := &serveFix{t: t, st: st, db: db, log: &pushLogBuffer{}}
	d := f.ticket(model.ReversCostly)
	tok := f.mint(model.PushDecision, d.ID, time.Now().UTC(), true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &pushLogBuffer{}
	done := make(chan error, 1)
	go func() { done <- serveExecute(ctx, out, "--listen", "127.0.0.1:0") }()
	addr := serveWaitAddr(t, out, done)
	answered := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+addr+"/v1/answer", strings.NewReader(`{"decision":"D-1","key":"A"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := pushLoopbackClient.Do(req)
		if err != nil {
			answered <- 0
			return
		}
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	<-block.entered
	cancel()
	time.Sleep(100 * time.Millisecond) // the shutdown has begun
	close(block.release)
	if code := <-answered; code != http.StatusOK {
		t.Errorf("the in-flight answer = %d, want 200", code)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve = %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("serve did not end")
	}
	if got := f.decision(d.ID); got.Status != model.StatusAnswered || got.AnsweredVia != model.AnsweredNtfy {
		t.Errorf("ticket = %+v", got)
	}
	if block.canceled.Load() {
		t.Error("the in-flight answer ran on a context the stop had canceled")
	}
}

// TestAnswerSurvivesHangUp: once a token is spent, a client that hangs up
// does not cut the answer's delivery short.
func TestAnswerSurvivesHangUp(t *testing.T) {
	block := &serveBlockingTmux{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f := serveNewFixWith(t, block)
	d := f.ticket(model.ReversCostly)
	r := serveAnswerReq(f.mint(model.PushDecision, d.ID, serveT0, true), d.ID, "A")
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, r.method, f.srv.URL+r.path, strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", r.auth[0])
	req.Header.Set("Content-Type", r.ctype)
	failed := make(chan error, 1)
	go func() {
		resp, err := f.srv.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		failed <- err
	}()
	<-block.entered
	cancel()
	if err := <-failed; err == nil {
		t.Fatal("the request was not cut off")
	}
	time.Sleep(100 * time.Millisecond) // the server has seen the hang-up
	close(block.release)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(f.log.String(), "status=200") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if block.canceled.Load() {
		t.Error("the delivery ran on the hung-up request's context")
	}
	if got := f.decision(d.ID); got.Status != model.StatusAnswered {
		t.Errorf("ticket = %+v", got)
	}
}

func TestServeHeaderLimits(t *testing.T) {
	f := serveNewFix(t)
	c, err := net.Dial("tcp", f.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		_, _ = fmt.Fprintf(c, "POST /v1/answer HTTP/1.1\r\nHost: x\r\nX-Big: %s\r\n\r\n", strings.Repeat("a", 64<<10))
	}()
	reply, err := io.ReadAll(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the server hung on a 64 KiB header")
	}
	if len(reply) > 0 && !strings.HasPrefix(string(reply), "HTTP/1.1 431") {
		t.Errorf("reply = %.80q, want 431 or a closed connection", reply)
	}
}

// TestServeServerLimits: the server's timeouts and header bound are the
// plan's (section 4.8).
func TestServeServerLimits(t *testing.T) {
	s := serveNewServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if s.ReadHeaderTimeout != 5*time.Second || s.ReadTimeout != 10*time.Second || s.WriteTimeout != 30*time.Second ||
		s.IdleTimeout != 60*time.Second || s.MaxHeaderBytes != 8<<10 || !s.DisableGeneralOptionsHandler ||
		s.TLSConfig != nil || s.ErrorLog == nil {
		t.Errorf("server = %+v", s)
	}
}

// TestServeCapsConcurrentRequests: with serveMaxInFlight requests being
// answered, one more is refused at once.
func TestServeCapsConcurrentRequests(t *testing.T) {
	block := &serveBlockingTmux{entered: make(chan struct{}, serveMaxInFlight), release: make(chan struct{})}
	f := serveNewFixWith(t, block)
	var reqs []serveReq
	for range serveMaxInFlight {
		d := f.ticket(model.ReversCostly)
		reqs = append(reqs, serveAnswerReq(f.mint(model.PushDecision, d.ID, serveT0, true), d.ID, "A"))
	}
	var wg sync.WaitGroup
	codes := make([]int, len(reqs))
	for i, r := range reqs {
		wg.Go(func() { codes[i], _, _ = f.do(r) })
	}
	for range serveMaxInFlight {
		<-block.entered
	}
	if code, body, _ := f.do(serveReq{method: http.MethodGet, path: "/healthz"}); code != http.StatusServiceUnavailable || body != `{"error":"busy"}` {
		t.Errorf("one more = %d %s, want 503 busy", code, body)
	}
	close(block.release)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d = %d, want 200", i, c)
		}
	}
	if code, _, _ := f.do(serveReq{method: http.MethodGet, path: "/healthz"}); code != http.StatusOK {
		t.Errorf("after = %d", code)
	}
}

// ---- command helpers ----

// serveBlockingTmux is a tmux client whose ListPanes waits for release,
// signaling entered first, and records whether its context had ended by
// then; every other method fails as unreachableTmux.
type serveBlockingTmux struct {
	unreachableTmux
	entered  chan struct{}
	release  chan struct{}
	canceled atomic.Bool // a released call's context had ended
}

func (b *serveBlockingTmux) ListPanes(ctx context.Context) ([]tmux.Pane, error) {
	b.entered <- struct{}{}
	<-b.release
	if ctx.Err() != nil {
		b.canceled.Store(true)
	}
	return nil, errTmuxUnreachable
}

// serveConfig gives the test a fresh HOME and CURIA_DB with a config file
// whose [serve] section has public and listen, and resets the flags. It
// returns the database path, which does not exist yet.
func serveConfig(t *testing.T, public, listen string) string {
	t.Helper()
	db := hookHome(t)
	body := "[serve]\n"
	if public != "" {
		body += fmt.Sprintf("public_url = %q\n", public)
	}
	if listen != "" {
		body += fmt.Sprintf("listen = %q\n", listen)
	}
	pushWriteConfig(t, db, body)
	t.Cleanup(func() { serveListenFlag = "" })
	return db
}

// serveNoListen counts calls to serveListen for the rest of the test,
// refusing each.
func serveNoListen(t *testing.T) func() int {
	t.Helper()
	var n atomic.Int32
	prev := serveListen
	serveListen = func(context.Context, string, string) (net.Listener, error) {
		n.Add(1)
		return nil, errors.New("test: listening is not allowed here")
	}
	t.Cleanup(func() { serveListen = prev })
	return func() int { return int(n.Load()) }
}

// serveExecute runs `curia serve <args>` in-process on ctx, writing
// stdout to out.
func serveExecute(ctx context.Context, out io.Writer, args ...string) error {
	cmd, _, err := rootCmd.Find([]string{"serve"})
	if err != nil {
		return err
	}
	cmd.SetOut(out)
	cmd.SetErr(io.Discard)
	cmd.SetContext(ctx)
	defer func() { cmd.SetOut(nil); cmd.SetErr(nil) }()
	if err := cmd.ParseFlags(args); err != nil {
		return err
	}
	return cmd.RunE(cmd, cmd.Flags().Args())
}

// serveRun runs `curia serve <args>` through rootCmd, its output dropped.
func serveRun(t *testing.T, args ...string) error {
	t.Helper()
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs(append([]string{"serve"}, args...))
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		serveListenFlag = ""
	})
	err := rootCmd.ExecuteContext(context.Background())
	serveListenFlag = ""
	return err
}

var serveListeningLine = regexp.MustCompile(`curia serve: listening on (127\.0\.0\.1:\d+)\n`)

// serveWaitAddr waits up to 5 s for the listening line on out and returns
// its address.
func serveWaitAddr(t *testing.T, out *pushLogBuffer, done <-chan error) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := serveListeningLine.FindStringSubmatch(out.String()); m != nil {
			return m[1]
		}
		select {
		case err := <-done:
			t.Fatalf("serve ended early: %v (output %q)", err, out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("no listening line in %q", out.String())
	return ""
}

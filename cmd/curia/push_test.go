package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/mcp"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/store"
)

// pushUnsetPublisher is pushPublisherFor under test until a test sets its
// own: building a publisher in a test that did not ask for one panics, so
// no test can reach a real ntfy server through the seam.
func pushUnsetPublisher(config.Config) notify.Publisher {
	panic("pushPublisherFor called in a test that did not set it (use pushUseServer)")
}

func init() {
	pushPublisherFor = pushUnsetPublisher
}

// errPushOffLoopback is pushLoopbackClient's refusal.
var errPushOffLoopback = errors.New("test client: only loopback addresses may be dialed")

// pushLoopbackClient is the HTTP client of every test publisher: it dials
// only loopback IP addresses (the httptest servers), takes no proxy and
// resolves no name, so no test can connect off the machine.
var pushLoopbackClient = &http.Client{Transport: &http.Transport{
	Proxy: nil,
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			return nil, errPushOffLoopback
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	},
}}

// pushTestPublisher is pushPublisher with the loopback-only client.
func pushTestPublisher(cfg config.Config) notify.Publisher {
	p := pushPublisher(cfg)
	p.Client = pushLoopbackClient
	return p
}

// pushUseServer makes pushPublisherFor build test publishers for the rest
// of the test.
func pushUseServer(t *testing.T) {
	t.Helper()
	prev := pushPublisherFor
	pushPublisherFor = pushTestPublisher
	t.Cleanup(func() { pushPublisherFor = prev })
}

// pushAction and pushBody are the wire shape a test server decodes.
type pushAction struct {
	Action, Label, URL, Method, Body string
	Headers                          map[string]string
	Clear                            bool
}

type pushBody struct {
	Topic, Title, Message string
	Priority              int
	Actions               []pushAction
}

// pushReq is one request a test server received.
type pushReq struct {
	Auth string
	Body pushBody
}

// pushSrv is a local ntfy stand-in on loopback.
type pushSrv struct {
	*httptest.Server
	mu    sync.Mutex
	reqs  []pushReq
	got   chan struct{}
	reply func(w http.ResponseWriter, r *http.Request)
}

// pushServer starts a stand-in that records every request and answers with
// reply (nil: 200). A reply that waits should also wait on
// pushSrv.release, which closes before the server does.
func pushServer(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) *pushSrv {
	t.Helper()
	s := &pushSrv{got: make(chan struct{}, 1000), reply: reply}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var b pushBody
		_ = json.Unmarshal(raw, &b)
		s.mu.Lock()
		s.reqs = append(s.reqs, pushReq{Auth: r.Header.Get("Authorization"), Body: b})
		s.mu.Unlock()
		s.got <- struct{}{}
		if s.reply != nil {
			s.reply(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	t.Cleanup(s.Close)
	return s
}

// pushHang is a reply that never answers until the client gives up or the
// test ends.
func pushHang(t *testing.T) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // runs before the server's Close
	return func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

// pushStatus is a reply with code and an optional Retry-After.
func pushStatus(code int, retryAfter string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
	}
}

func (s *pushSrv) requests() []pushReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// pushWaitLimit is how long a test waits for a push to reach its server.
const pushWaitLimit = 2 * time.Second

// wait waits for the next request, failing the test after pushWaitLimit.
func (s *pushSrv) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.got:
	case <-time.After(pushWaitLimit):
		t.Fatalf("the ntfy server got no request within %v (%d so far)", pushWaitLimit, len(s.requests()))
	}
}

// pushCfg is a configuration with ntfy on: the server, the devices (name ->
// topic), quiet hours and the public URL.
func pushCfg(url string, devices map[string]string, quiet, public string) config.Config {
	cfg := config.Default()
	cfg.Ntfy = config.Ntfy{URL: url, Quiet: quiet, Devices: devices}
	cfg.Serve.PublicURL = public
	return cfg
}

var pushTwoDevices = map[string]string{"phone": "curia-phone-k3j9", "tablet": "curia-tablet-x7q2"}

// pushStore opens a fresh store and returns it and its path.
func pushStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "curia.db")
	return mcpOpenStore(t, db), db
}

// pushT0 is the tests' fixed clock start, outside any quiet window used.
var pushT0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// pushTicket stores an open blocking ticket with n options (A, B, ...) and returns it.
func pushTicket(t *testing.T, st *store.Store, question string, rev model.Reversibility, n int) model.Decision {
	t.Helper()
	var opts []model.Option
	for i := range n {
		k := string(rune('A' + i))
		opts = append(opts, model.Option{Key: k, Label: "Option " + k})
	}
	d := model.Decision{
		SessionID: "S1", Repo: "/Users/someone/code/api-server", Question: question, Options: opts,
		Recommendation: "B", Rationale: "Because.", Reversibility: rev, Mode: model.ModeBlocking, Scope: model.ScopeRepo,
		CreatedAt: pushT0, ArgsHash: question,
	}
	stored, _, err := st.CreateDecision(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// pushEnq is pushEnqueue that must succeed.
func pushEnq(t *testing.T, st *store.Store, cfg config.Config, kind model.PushKind, ref string, now time.Time) {
	t.Helper()
	if err := pushEnqueue(context.Background(), st, cfg, kind, ref, now); err != nil {
		t.Fatalf("pushEnqueue(%s %s): %v", kind, ref, err)
	}
}

// pushFlusherFor is the flusher the loop builds, on the test publisher and
// clock now, logging nowhere.
func pushFlusherFor(st *store.Store, cfg config.Config, now func() time.Time) pushFlusher {
	return pushFlusher{st: st, cfg: cfg, pub: pushTestPublisher(cfg), now: now, log: slog.New(slog.DiscardHandler)}
}

// pushFlushAt flushes at now and returns how many it sent.
func pushFlushAt(t *testing.T, st *store.Store, cfg config.Config, now time.Time) int {
	t.Helper()
	n, err := pushFlusherFor(st, cfg, func() time.Time { return now }).flush(context.Background())
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	return n
}

// pushRow is one pushes row as the tests read it.
type pushRow struct {
	N                                    int64
	Kind, Ref, Device, Status, LastError string
	TokenHash                            string
	Attempts                             int
	NotBefore                            time.Time
	Claimed                              bool
}

// pushRows reads every pushes row of the database at db, in n order.
func pushRows(t *testing.T, db string) []pushRow {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query(`SELECT n, kind, ref, device, status, last_error, token_hash, attempts, not_before, claimed_at IS NOT NULL FROM pushes ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []pushRow
	for rows.Next() {
		var r pushRow
		var nb string
		if serr := rows.Scan(&r.N, &r.Kind, &r.Ref, &r.Device, &r.Status, &r.LastError, &r.TokenHash, &r.Attempts, &nb, &r.Claimed); serr != nil {
			t.Fatal(serr)
		}
		if r.NotBefore, err = time.Parse("2006-01-02T15:04:05.000000000Z", nb); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// pushStatuses lists the rows' statuses.
func pushStatuses(rows []pushRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Status)
	}
	return out
}

// pushLogBuffer is a log destination safe for the loop's goroutine.
type pushLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *pushLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *pushLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func pushLogger() (*slog.Logger, *pushLogBuffer) {
	b := &pushLogBuffer{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

// pushWriteConfig writes body as the config file of the HOME mcpEnv set.
func pushWriteConfig(t *testing.T, db, body string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(db), ".config", "curia") // db is in HOME
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pushNtfyConfig is a config file with ntfy on, pointed at url, with one
// device.
func pushNtfyConfig(url string) string {
	return fmt.Sprintf("[ntfy]\nurl = %q\n[ntfy.devices]\nphone = \"curia-phone-k3j9\"\n", url)
}

// pushSessionOnTask stores session S1 in pane %3 and window @7, working on
// a new task.
func pushSessionOnTask(t *testing.T, st *store.Store) {
	t.Helper()
	id := mcpTask(t, st, "Retry webhooks")
	mcpPutSession(t, st, "S1", "%3", model.StateWorking, time.Now(), func(s *model.Session) {
		s.TaskID, s.TmuxWindow = id, "@7"
	})
	t.Setenv("CURIA_SESSION_ID", "S1")
}

// pushListener is a TCP listener the test owns, counting connections.
func pushListener(t *testing.T) (url string, accepted func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	return "http://" + ln.Addr().String(), func() int { return int(n.Load()) }
}

// ---- the brief's tests ----

func TestNoNtfyConfiguredNoNetwork(t *testing.T) {
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	listen, accepted := pushListener(t)
	for _, cfg := range []config.Config{
		config.Default(),
		pushCfg(listen, map[string]string{}, "", ""),               // a URL, no device
		pushCfg("", map[string]string{"phone": "curia-p"}, "", ""), // a device, no URL
	} {
		pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
		kick, stop := pushStart(context.Background(), st, cfg, slog.New(slog.DiscardHandler))
		kick()
		stop()
	}
	if rows := pushRows(t, db); len(rows) != 0 {
		t.Errorf("pushes = %+v, want none", rows)
	}

	// curia mcp over pipes: a URL the test listens on, but no device.
	for _, body := range []string{"", "[ntfy]\nurl = \"" + listen + "\"\n"} {
		mdb := mcpEnv(t)
		pushWriteConfig(t, mdb, body)
		mst := mcpOpenStore(t, mdb)
		pushSessionOnTask(t, mst)
		calls := &atomic.Int32{}
		prev := pushPublisherFor
		pushPublisherFor = func(cfg config.Config) notify.Publisher { calls.Add(1); return pushUnsetPublisher(cfg) }
		p := mcpStart(t)
		p.request("Store webhook events in Postgres or Redis?")
		if out, isErr := p.tool("complete_task", map[string]any{"summary": "Done.", "tests_status": "passed"}); isErr {
			t.Fatalf("complete_task: %v", out)
		}
		mcpQuietEnd(t, p)
		pushPublisherFor = prev
		if c := calls.Load(); c != 0 {
			t.Errorf("config %q: a publisher was built %d times, want never", body, c)
		}
		if rows := pushRows(t, mdb); len(rows) != 0 {
			t.Errorf("config %q: pushes = %+v, want none", body, rows)
		}
	}
	if n := accepted(); n != 0 {
		t.Errorf("the test's listener saw %d connections, want none", n)
	}
}

func TestEnqueueOnePerDeviceWithQuietHours(t *testing.T) {
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	zone := time.FixedZone("here", 2*3600)
	now := time.Date(2026, 11, 14, 23, 30, 0, 0, zone)
	cfg := pushCfg("https://ntfy.example", pushTwoDevices, "22:00-07:00", "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, now)
	pushEnq(t, st, cfg, model.PushDecision, d.ID, now.Add(time.Minute)) // never twice
	rows := pushRows(t, db)
	want := time.Date(2026, 11, 15, 7, 0, 0, 0, zone)
	if len(rows) != 2 || rows[0].Device != "phone" || rows[1].Device != "tablet" {
		t.Fatalf("pushes = %+v, want one per device", rows)
	}
	for _, r := range rows {
		if r.Status != "pending" || r.Kind != "decision" || r.Ref != d.ID || !r.NotBefore.Equal(want) {
			t.Errorf("push %+v, want pending %s due %v", r, d.ID, want)
		}
	}
	// Outside the window a push is due at once.
	r2 := pushTicket(t, st, "Which cache?", model.ReversCostly, 2)
	day := time.Date(2026, 11, 14, 12, 0, 0, 0, zone)
	pushEnq(t, st, cfg, model.PushDecision, r2.ID, day)
	if rows := pushRows(t, db); len(rows) != 4 || !rows[2].NotBefore.Equal(day) {
		t.Errorf("daytime push = %+v, want due at %v", rows[2:], day)
	}
}

func TestFlushSendsDuePushes(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Store webhook events in Postgres or Redis?", model.ReversCostly, 2)
	cfg := pushCfg(srv.URL, pushTwoDevices, "", "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	if n := pushFlushAt(t, st, cfg, pushT0); n != 2 {
		t.Errorf("flush sent %d, want 2", n)
	}
	reqs := srv.requests()
	var topics []string
	for _, r := range reqs {
		topics = append(topics, r.Body.Topic)
		if r.Body.Title != "D-1 api-server: decision needed" || r.Body.Priority != 4 || r.Auth != "" {
			t.Errorf("request %+v", r)
		}
		if want := "Store webhook events in Postgres or Redis?\nA: Option A\nB: Option B\nRecommended: B"; r.Body.Message != want {
			t.Errorf("message %q, want %q", r.Body.Message, want)
		}
	}
	slices.Sort(topics)
	if want := []string{"curia-phone-k3j9", "curia-tablet-x7q2"}; !slices.Equal(topics, want) {
		t.Errorf("topics = %v, want %v", topics, want)
	}
	if got := pushStatuses(pushRows(t, db)); !slices.Equal(got, []string{"sent", "sent"}) {
		t.Errorf("statuses = %v, want both sent", got)
	}
	if n := pushFlushAt(t, st, cfg, pushT0.Add(time.Hour)); n != 0 || len(srv.requests()) != 2 {
		t.Errorf("second flush sent %d (requests %d), want nothing more", n, len(srv.requests()))
	}
}

func TestFlushHoldsUntilQuietEnds(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	night := time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	cfg := pushCfg(srv.URL, pushTwoDevices, "22:00-07:00", "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, night)
	if n := pushFlushAt(t, st, cfg, night.Add(7*time.Hour)); n != 0 || len(srv.requests()) != 0 {
		t.Fatalf("inside quiet hours: sent %d, %d requests; want none", n, len(srv.requests()))
	}
	if got := pushStatuses(pushRows(t, db)); !slices.Equal(got, []string{"pending", "pending"}) {
		t.Errorf("held statuses = %v, want pending (held, not dropped)", got)
	}
	end := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
	if n := pushFlushAt(t, st, cfg, end); n != 2 || len(srv.requests()) != 2 {
		t.Errorf("at the window's end: sent %d, %d requests; want 2", n, len(srv.requests()))
	}
}

// pushCheckButtons checks three buttons, the recommended B first, that
// answer at the public URL with one token whose hash is hash.
func pushCheckButtons(t *testing.T, actions []pushAction, hash string) {
	t.Helper()
	if len(actions) != 3 {
		t.Fatalf("with public_url: actions %+v, want 3", actions)
	}
	tokens := map[string]bool{}
	var labels []string
	for i, a := range actions {
		tokens[strings.TrimPrefix(a.Headers["Authorization"], "Bearer ")] = true
		labels = append(labels, a.Label)
		if a.URL != "https://curia.example.ts.net/v1/answer" || a.Method != "POST" || !a.Clear {
			t.Errorf("action %d = %+v", i, a)
		}
	}
	if !slices.Equal(labels, []string{"B: Option B", "A: Option A", "C: Option C"}) {
		t.Errorf("labels = %v, want the recommended first", labels)
	}
	if len(tokens) != 1 {
		t.Fatalf("tokens = %v, want one shared token", tokens)
	}
	for tok := range tokens {
		if len(tok) != 43 || notify.HashToken(tok) != hash {
			t.Errorf("token %q does not hash to the row's token_hash %q", tok, hash)
		}
	}
}

func TestFlushButtonsNeedPublicURL(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	one := map[string]string{"phone": "curia-phone-k3j9"}
	d1 := pushTicket(t, st, "Which queue?", model.ReversCostly, 4)
	cfg := pushCfg(srv.URL, one, "", "")
	pushEnq(t, st, cfg, model.PushDecision, d1.ID, pushT0)
	pushFlushAt(t, st, cfg, pushT0)
	if r := srv.requests()[0]; len(r.Body.Actions) != 0 || !strings.HasSuffix(r.Body.Message, "More options: curia decide D-1.") {
		t.Errorf("without public_url: %+v, want no actions", r.Body)
	}
	if h := pushRows(t, db)[0].TokenHash; h != "" {
		t.Errorf("without public_url: token_hash %q, want none", h)
	}

	d2 := pushTicket(t, st, "Which cache?", model.ReversCostly, 4)
	cfg = pushCfg(srv.URL, one, "", "https://curia.example.ts.net/")
	pushEnq(t, st, cfg, model.PushDecision, d2.ID, pushT0)
	pushFlushAt(t, st, cfg, pushT0)
	pushCheckButtons(t, srv.requests()[1].Body.Actions, pushRows(t, db)[1].TokenHash)

	// An irreversible ticket gets no buttons, even with public_url.
	d3 := pushTicket(t, st, "Drop the table?", model.ReversIrreversible, 2)
	pushEnq(t, st, cfg, model.PushDecision, d3.ID, pushT0)
	pushFlushAt(t, st, cfg, pushT0)
	if r := srv.requests()[2]; len(r.Body.Actions) != 0 || !strings.HasSuffix(r.Body.Message, "Irreversible: answer with curia decide D-3.") {
		t.Errorf("irreversible: %+v, want no actions", r.Body)
	}
}

func TestFlushTokenIsNotStoredInClear(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "A question nobody may log?", model.ReversCostly, 2)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "https://curia.example.ts.net")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	logger, logs := pushLogger()
	f := pushFlusher{st: st, cfg: cfg, pub: pushTestPublisher(cfg), now: func() time.Time { return pushT0 }, log: logger}
	if n, err := f.flush(context.Background()); n != 1 || err != nil {
		t.Fatalf("flush = %d, %v", n, err)
	}
	tok := strings.TrimPrefix(srv.requests()[0].Body.Actions[0].Headers["Authorization"], "Bearer ")
	if len(tok) != 43 {
		t.Fatalf("token %q", tok)
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var dump string
	if err := raw.QueryRow(`SELECT group_concat(n || kind || ref || device || status || not_before || attempts || last_error || token_hash ||
		created_at || COALESCE(claimed_at,'') || COALESCE(sent_at,'') || COALESCE(used_at,''), '|') FROM pushes`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, tok) || !strings.Contains(dump, notify.HashToken(tok)) {
		t.Errorf("row %q holds the token in clear or lacks its hash", dump)
	}
	l := logs.String()
	if !strings.Contains(l, "outcome=sent") || !strings.Contains(l, "ref=D-1") {
		t.Errorf("log %q, want the sent line", l)
	}
	for _, secret := range []string{tok, notify.HashToken(tok), "curia-phone-k3j9", "nobody may log", "Option A"} {
		if strings.Contains(l, secret) {
			t.Errorf("log holds %q: %s", secret, l)
		}
	}
}

func TestFlushDropsAnsweredTicket(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	ctx := context.Background()
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	answered := pushTicket(t, st, "Answered?", model.ReversCostly, 2)
	withdrawn := pushTicket(t, st, "Withdrawn?", model.ReversCostly, 2)
	tk := mcpTask(t, st, "A task")
	r, _, err := st.CompleteTask(ctx, model.Review{TaskID: tk, SessionID: "S1", Summary: "Done.", Tests: model.TestsPassed, CreatedAt: pushT0})
	if err != nil {
		t.Fatal(err)
	}
	pushEnq(t, st, cfg, model.PushDecision, answered.ID, pushT0)
	pushEnq(t, st, cfg, model.PushDecision, withdrawn.ID, pushT0)
	pushEnq(t, st, cfg, model.PushReview, r.ID, pushT0)
	if _, err := st.AnswerDecision(ctx, answered.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, pushT0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WithdrawDecision(ctx, withdrawn.ID, "S1", "no longer needed", "", pushT0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CloseReview(ctx, r.ID, store.ReviewClose{Status: model.ReviewDropped, Via: model.AnsweredCLI}, pushT0); err != nil {
		t.Fatal(err)
	}
	if n := pushFlushAt(t, st, cfg, pushT0); n != 0 || len(srv.requests()) != 0 {
		t.Errorf("sent %d, %d requests; want none", n, len(srv.requests()))
	}
	if got := pushStatuses(pushRows(t, db)); !slices.Equal(got, []string{"dropped", "dropped", "dropped"}) {
		t.Errorf("statuses = %v, want all dropped", got)
	}
}

// TestFlushRechecksAfterClaim: a ticket answered or a review closed between
// the claim and the send is dropped by step 3, not sent.
func TestFlushRechecksAfterClaim(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	ctx := context.Background()
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	d := pushTicket(t, st, "Answered after the claim?", model.ReversCostly, 2)
	tk := mcpTask(t, st, "A task")
	r, _, err := st.CompleteTask(ctx, model.Review{TaskID: tk, SessionID: "S1", Summary: "Done.", Tests: model.TestsPassed, CreatedAt: pushT0})
	if err != nil {
		t.Fatal(err)
	}
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	pushEnq(t, st, cfg, model.PushReview, r.ID, pushT0)
	f := pushFlusher{st: st, cfg: cfg, pub: pushTestPublisher(cfg), now: func() time.Time { return pushT0 }, log: slog.New(slog.DiscardHandler)}
	for _, closeSubject := range []func(){
		func() { _, _ = st.AnswerDecision(ctx, d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, pushT0) },
		func() {
			_, _, _ = st.CloseReview(ctx, r.ID, store.ReviewClose{Status: model.ReviewDropped, Via: model.AnsweredCLI}, pushT0)
		},
	} {
		p, found, err := st.ClaimPush(ctx, "", pushT0)
		if err != nil || !found {
			t.Fatalf("claim: %v %v", found, err)
		}
		if _, why, err := f.build(ctx, p, ""); err != nil || why != "" {
			t.Fatalf("build %s before the close: why %q, err %v; want a push", p.Ref, why, err)
		}
		closeSubject()
		if _, why, err := f.build(ctx, p, ""); err != nil || why == "" {
			t.Errorf("build %s after the close: why %q, err %v; want a drop", p.Ref, why, err)
		}
	}
	if len(srv.requests()) != 0 || len(pushRows(t, db)) != 2 {
		t.Errorf("requests %d, want none", len(srv.requests()))
	}
}

// TestFlushDropsTicketExpiredAfterClaim: a blocking ticket that expires
// between the claim and the send counts as closed.
func TestFlushDropsTicketExpiredAfterClaim(t *testing.T) {
	st, _ := pushStore(t)
	ctx := context.Background()
	cfg := pushCfg("https://ntfy.example", map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	e, _, err := st.CreateDecision(ctx, model.Decision{
		SessionID: "S1", Question: "Expiring?", Options: []model.Option{{Key: "A", Label: "a"}, {Key: "B", Label: "b"}},
		Recommendation: "A", Rationale: "r", Reversibility: model.ReversCostly, Mode: model.ModeBlocking,
		Scope: model.ScopeRepo, CreatedAt: pushT0, ExpiresAt: pushT0.Add(time.Hour), ArgsHash: "expiring",
	})
	if err != nil {
		t.Fatal(err)
	}
	pushEnq(t, st, cfg, model.PushDecision, e.ID, pushT0)
	p, found, err := st.ClaimPush(ctx, "", pushT0)
	if err != nil || !found || p.Ref != e.ID {
		t.Fatalf("claim %s: %+v %v %v", e.ID, p, found, err)
	}
	f := pushFlusher{st: st, cfg: cfg, now: func() time.Time { return pushT0.Add(2 * time.Hour) }, log: slog.New(slog.DiscardHandler)}
	if _, why, _ := f.build(ctx, p, ""); why == "" {
		t.Error("a ticket that expired after the claim was not dropped")
	}
}

func TestFlushDropsRemovedDevice(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	pushEnq(t, st, pushCfg(srv.URL, pushTwoDevices, "", ""), model.PushDecision, d.ID, pushT0)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	if n := pushFlushAt(t, st, cfg, pushT0); n != 1 {
		t.Errorf("sent %d, want 1", n)
	}
	rows := pushRows(t, db)
	if rows[0].Status != "sent" || rows[1].Device != "tablet" || rows[1].Status != "dropped" {
		t.Errorf("rows = %+v, want phone sent and tablet dropped", rows)
	}
	if reqs := srv.requests(); len(reqs) != 1 || reqs[0].Body.Topic != "curia-phone-k3j9" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestFlushRetriesThenFails(t *testing.T) {
	srv := pushServer(t, pushStatus(http.StatusServiceUnavailable, ""))
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	now := pushT0
	for attempt := 1; attempt <= store.PushMaxAttempts; attempt++ {
		if n := pushFlushAt(t, st, cfg, now); n != 0 {
			t.Fatalf("attempt %d sent %d", attempt, n)
		}
		row := pushRows(t, db)[0]
		if len(srv.requests()) != attempt || row.Attempts != attempt {
			t.Fatalf("attempt %d: %d requests, row %+v", attempt, len(srv.requests()), row)
		}
		if attempt == store.PushMaxAttempts {
			if row.Status != "failed" || !strings.Contains(row.LastError, "status 503") {
				t.Errorf("after the last attempt: %+v, want failed", row)
			}
			break
		}
		wait := pushBackoff << (attempt - 1) // 30 s, 60 s, 120 s, 240 s
		if row.Status != "pending" || !row.NotBefore.Equal(now.Add(wait)) {
			t.Fatalf("attempt %d: %+v, want pending due %v later", attempt, row, wait)
		}
		if n := pushFlushAt(t, st, cfg, now.Add(wait-time.Nanosecond)); n != 0 || len(srv.requests()) != attempt {
			t.Fatalf("attempt %d: retried before its time", attempt)
		}
		now = now.Add(wait)
	}
	pushFlushAt(t, st, cfg, now.Add(24*time.Hour))
	if n := len(srv.requests()); n != store.PushMaxAttempts {
		t.Errorf("%d requests, want exactly %d", n, store.PushMaxAttempts)
	}
}

func TestFlushRejectedIsNotRetried(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusBadRequest, http.StatusNotFound} {
		srv := pushServer(t, pushStatus(code, ""))
		st, db := pushStore(t)
		d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
		cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
		pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
		logger, logs := pushLogger()
		f := pushFlusher{st: st, cfg: cfg, pub: pushTestPublisher(cfg), now: func() time.Time { return pushT0 }, log: logger}
		if _, err := f.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		pushFlushAt(t, st, cfg, pushT0.Add(24*time.Hour))
		row := pushRows(t, db)[0]
		if len(srv.requests()) != 1 || row.Status != "failed" || !strings.Contains(row.LastError, fmt.Sprintf("status %d", code)) {
			t.Errorf("%d: %d requests, row %+v; want failed after one", code, len(srv.requests()), row)
		}
		if l := logs.String(); strings.Count(l, "level=ERROR") != 1 || !strings.Contains(l, "outcome=\"failed, not retried\"") {
			t.Errorf("%d: log %q, want one error line", code, l)
		}
	}
	// A notification ntfy would refuse (a bad topic) is refused before
	// sending, and failed the same way.
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "bad topic"}, "", "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	pushFlushAt(t, st, cfg, pushT0)
	if row := pushRows(t, db)[0]; len(srv.requests()) != 0 || row.Status != "failed" || row.Attempts != 1 {
		t.Errorf("invalid: %d requests, row %+v; want failed with no request", len(srv.requests()), row)
	}
}

func TestFlushRetryRespectsCap(t *testing.T) {
	for _, c := range []struct {
		retryAfter string
		want       time.Duration
	}{
		{"3600", pushBackoffMax}, // ntfy's own hour, bounded
		{"90", 90 * time.Second}, // later than the backoff: honored
		{"10", pushBackoff},      // earlier: the backoff
		{"", pushBackoff},
	} {
		srv := pushServer(t, pushStatus(http.StatusTooManyRequests, c.retryAfter))
		st, db := pushStore(t)
		d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
		cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
		pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
		now := pushT0
		for attempt := 1; attempt < store.PushMaxAttempts; attempt++ {
			pushFlushAt(t, st, cfg, now)
			row := pushRows(t, db)[0]
			if row.Status != "pending" || row.NotBefore.After(now.Add(pushBackoffMax)) {
				t.Fatalf("Retry-After %q, attempt %d: %+v is more than %v ahead", c.retryAfter, attempt, row, pushBackoffMax)
			}
			if attempt == 1 && !row.NotBefore.Equal(now.Add(c.want)) {
				t.Errorf("Retry-After %q: due %v later, want %v", c.retryAfter, row.NotBefore.Sub(now), c.want)
			}
			now = row.NotBefore
		}
	}
}

func TestTwoFlushersSendOnce(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	cfg := pushCfg(srv.URL, pushTwoDevices, "", "")
	for i := range 5 {
		d := pushTicket(t, st, fmt.Sprintf("Question %d?", i), model.ReversCostly, 2)
		pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	}
	other := mcpOpenStore(t, db)
	var total atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, h := range []*store.Store{st, other} {
		wg.Go(func() {
			for range 50 { // busy-retry, bounded
				n, err := pushFlusherFor(h, cfg, func() time.Time { return pushT0 }).flush(context.Background())
				total.Add(int64(n))
				if err == nil {
					return
				}
			}
			errs <- errors.New("a flusher never finished without an error")
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	reqs := srv.requests()
	seen := map[string]bool{}
	for _, r := range reqs {
		key := r.Body.Topic + " " + r.Body.Title
		if seen[key] {
			t.Errorf("%s was sent twice", key)
		}
		seen[key] = true
	}
	if len(reqs) != 10 || total.Load() != 10 {
		t.Errorf("%d requests, %d counted sent; want exactly 10", len(reqs), total.Load())
	}
	for _, r := range pushRows(t, db) {
		if r.Status != "sent" || r.Attempts != 1 {
			t.Errorf("row %+v, want sent at the first attempt", r)
		}
	}
}

func TestFlushReviewPush(t *testing.T) {
	srv := pushServer(t, nil)
	st, _ := pushStore(t)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "https://curia.example.ts.net")
	tk := mcpTask(t, st, "Retry webhooks")
	r, _, err := st.CompleteTask(context.Background(), model.Review{
		TaskID: tk, SessionID: "S1", Summary: "Added retries with backoff.", Tests: model.TestsPassed, CreatedAt: pushT0,
	})
	if err != nil {
		t.Fatal(err)
	}
	pushEnq(t, st, cfg, model.PushReview, r.ID, pushT0)
	if n := pushFlushAt(t, st, cfg, pushT0); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	b := srv.requests()[0].Body
	want := "Retry webhooks\nTests (the agent's report): passed\nSummary (the agent's):\nAdded retries with backoff."
	if b.Title != "R-1 T-1: ready for review" || len(b.Actions) != 0 || b.Priority != 3 || b.Message != want {
		t.Errorf("review push = %+v", b)
	}
}

func TestPushLoopKickAndTick(t *testing.T) {
	srv := pushServer(t, nil)
	pushUseServer(t)
	st, _ := pushStore(t)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	now := mcpFakeClock(t, pushT0)
	run := func(every time.Duration) (chan<- struct{}, func()) {
		pushEvery = every
		kick := make(chan struct{}, 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); pushLoop(ctx, st, cfg, slog.New(slog.DiscardHandler), kick) }()
		return kick, func() {
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("pushLoop did not end within 1 s of its context")
			}
		}
	}
	prev := pushEvery
	t.Cleanup(func() { pushEvery = prev })

	// A kick flushes at once (no tick for an hour).
	kick, stop := run(time.Hour)
	d := pushTicket(t, st, "Kicked?", model.ReversCostly, 2)
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	kick <- struct{}{}
	srv.wait(t)
	stop()

	// A push that becomes due later is sent by a tick, without a kick.
	_, stop = run(20 * time.Millisecond)
	e := pushTicket(t, st, "Ticked?", model.ReversCostly, 2)
	if _, err := st.EnqueuePushes(context.Background(), model.PushDecision, e.ID, []string{"phone"}, pushT0.Add(time.Minute), pushT0); err != nil {
		t.Fatal(err)
	}
	now.Store(pushT0.Add(time.Minute).UnixNano())
	srv.wait(t)
	stop()
	if reqs := srv.requests(); len(reqs) != 2 || !strings.HasPrefix(reqs[1].Body.Title, e.ID+" ") {
		t.Errorf("requests = %+v, want the kicked and the ticked push", reqs)
	}
}

// pushMCP starts curia mcp with ntfy pointed at srv and session S1 on a
// task, and returns the process, the database and the task id.
func pushMCP(t *testing.T, srv *pushSrv) (*mcpProc, string) {
	t.Helper()
	db := mcpEnv(t)
	pushWriteConfig(t, db, pushNtfyConfig(srv.URL))
	pushSessionOnTask(t, mcpOpenStore(t, db))
	pushUseServer(t)
	return mcpStart(t), db
}

func TestMCPBlockingTicketIsPushed(t *testing.T) {
	srv := pushServer(t, nil)
	p, db := pushMCP(t, srv)
	id := p.request("Store webhook events in Postgres or Redis?")
	srv.wait(t)
	assume := mcpTicket("Use the default port?")
	assume["mode"], assume["assumption"] = "assume", "A"
	if out, isErr := p.tool("request_decision", assume); isErr {
		t.Fatalf("assume ticket: %v", out)
	}
	mcpQuietEnd(t, p)
	reqs := srv.requests()
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0].Body.Title, id+" ") || reqs[0].Body.Topic != "curia-phone-k3j9" {
		t.Errorf("requests = %+v, want one push for %s", reqs, id)
	}
	if rows := pushRows(t, db); len(rows) != 1 || rows[0].Ref != id || rows[0].Status != "sent" {
		t.Errorf("pushes = %+v, want only %s, sent", rows, id)
	}
}

func TestMCPNewReviewIsPushedOnce(t *testing.T) {
	srv := pushServer(t, nil)
	p, db := pushMCP(t, srv)
	for _, s := range []string{"Done.", "Done again."} {
		if out, isErr := p.tool("complete_task", map[string]any{"summary": s, "tests_status": "passed"}); isErr {
			t.Fatalf("complete_task: %v", out)
		}
	}
	srv.wait(t)
	mcpQuietEnd(t, p)
	if reqs := srv.requests(); len(reqs) != 1 || reqs[0].Body.Title != "R-1 T-1: ready for review" {
		t.Errorf("requests = %+v, want one review push", reqs)
	}
	if rows := pushRows(t, db); len(rows) != 1 || rows[0].Kind != "review" || rows[0].Ref != "R-1" {
		t.Errorf("pushes = %+v, want one for R-1", rows)
	}
}

func TestMCPNewReviewMarksWindowBlue(t *testing.T) {
	db := mcpEnv(t) // ntfy not configured
	fake := useFakeTmux(t)
	st := mcpOpenStore(t, db)
	pushSessionOnTask(t, st) // window @7, never seen
	p := mcpStart(t)
	if out, isErr := p.tool("complete_task", map[string]any{"summary": "Done.", "tests_status": "passed"}); isErr {
		t.Fatalf("complete_task: %v", out)
	}
	mcpQuietEnd(t, p)
	if want := []string{"SetWindowOption @7 @curia_marker blue"}; !slices.Equal(fake.Calls, want) {
		t.Errorf("tmux calls = %q, want %q", fake.Calls, want)
	}
	if rows := pushRows(t, db); len(rows) != 0 {
		t.Errorf("pushes = %+v, want none without ntfy", rows)
	}
}

func TestMCPPushDoesNotDelayTheTool(t *testing.T) {
	slow := func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}
	srv := pushServer(t, slow)
	p, _ := pushMCP(t, srv)
	start := time.Now()
	p.request("Store webhook events in Postgres or Redis?")
	if d := time.Since(start); d >= 500*time.Millisecond {
		t.Errorf("request_decision took %v, want under 500 ms", d)
	}
	srv.wait(t) // the push is on its way, in the background
	start = time.Now()
	if out, isErr := p.tool("complete_task", map[string]any{"summary": "Done.", "tests_status": "passed"}); isErr {
		t.Fatalf("complete_task: %v", out)
	} else if d := time.Since(start); d >= 500*time.Millisecond {
		t.Errorf("complete_task took %v while a push hung, want under 500 ms", d)
	}
	mcpQuietEnd(t, p)
}

func TestMCPStopsLoopOnEOF(t *testing.T) {
	srv := pushServer(t, pushHang(t))
	p, db := pushMCP(t, srv)
	p.request("Store webhook events in Postgres or Redis?")
	srv.wait(t) // the loop is inside the hanging send
	start := time.Now()
	mcpQuietEnd(t, p)
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("curia mcp took %v to end, want at most 4 s", d)
	}
	rows := pushRows(t, db)
	if len(rows) != 1 || rows[0].Status != "pending" || rows[0].Attempts != 1 || rows[0].TokenHash != "" {
		t.Errorf("pushes = %+v, want the row released as pending", rows)
	}
}

func TestBadNtfyTokenIgnored(t *testing.T) {
	for _, c := range []struct {
		value, auth string
		warned      bool
	}{
		{"tk_abc\r\nX-Evil: 1", "", true},
		{strings.Repeat("a", pushMaxAccess+1), "", true},
		{"tk_abc\x7f", "", true},
		{"tk_good", "Bearer tk_good", false},
		{"", "", false},
	} {
		srv := pushServer(t, nil)
		pushUseServer(t)
		t.Setenv("CURIA_NTFY_TOKEN", c.value)
		st, _ := pushStore(t)
		cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
		d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
		pushEnq(t, st, cfg, model.PushDecision, d.ID, time.Now())
		logger, logs := pushLogger()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); pushLoop(ctx, st, cfg, logger, nil) }()
		srv.wait(t)
		cancel()
		<-done
		if got := srv.requests()[0].Auth; got != c.auth {
			t.Errorf("token %q: Authorization %q, want %q", c.value, got, c.auth)
		}
		l := logs.String()
		if n := strings.Count(l, "CURIA_NTFY_TOKEN ignored"); (n == 1) != c.warned || n > 1 {
			t.Errorf("token %q: %d warnings in %q", c.value, n, l)
		}
		if c.value != "" && strings.Contains(l, c.value[:6]) {
			t.Errorf("the log holds the token's value: %s", l)
		}
	}
}

// ---- edge cases the rules state ----

// TestPushSendGivesUpBeforeReclaim: a stalled send gives up, and releases
// its row, long before store.PushReclaim lets another process claim it, so
// a send cannot finish on a row someone else is sending.
func TestPushSendGivesUpBeforeReclaim(t *testing.T) {
	if pushSendLimit+pushFinishLimit > store.PushReclaim/4 {
		t.Fatalf("send %v + finish %v is not well under the reclaim %v", pushSendLimit, pushFinishLimit, store.PushReclaim)
	}
	srv := pushServer(t, pushHang(t))
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0)
	start := time.Now()
	if n := pushFlushAt(t, st, cfg, pushT0); n != 0 {
		t.Errorf("sent %d", n)
	}
	if took := time.Since(start); took > pushSendLimit+pushFinishLimit {
		t.Errorf("the stalled flush took %v, want at most %v", took, pushSendLimit+pushFinishLimit)
	}
	if row := pushRows(t, db)[0]; row.Status != "pending" || !row.NotBefore.Equal(pushT0.Add(pushBackoff)) ||
		!strings.Contains(row.LastError, "timed out") {
		t.Errorf("row %+v, want pending, retried after %v", row, pushBackoff)
	}
}

func TestPushTestGuards(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the tests' default pushPublisherFor did not panic")
			}
		}()
		pushPublisherFor(config.Default())
	}()
	resp, err := pushLoopbackClient.Get("http://192.0.2.1:9/") // TEST-NET-1: refused before any dial
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, errPushOffLoopback) {
		t.Errorf("off-loopback request error = %v, want the refusal", err)
	}
	if p := pushPublisher(pushCfg("https://ntfy.example", pushTwoDevices, "", "")); p.URL != "https://ntfy.example" || p.Client != nil {
		t.Errorf("publisher = %+v, want the configured URL and the default client", p)
	}
}

func TestMCPHelpNamesEveryRegisteredTool(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"mcp"})
	if err != nil {
		t.Fatal(err)
	}
	tools := mcp.Tools(mcp.Deps{})
	if len(tools) != 7 || !strings.Contains(cmd.Long, "seven tools") {
		t.Errorf("%d tools registered; mcp help = %q, want seven tools", len(tools), cmd.Long)
	}
	for _, tl := range tools {
		if !strings.Contains(cmd.Long, tl.Name) {
			t.Errorf("mcp help does not name %s: %q", tl.Name, cmd.Long)
		}
	}
}

// pushSetCache stores session S1 and sets its cache columns by raw SQL (the
// columns are filled by another program). expires and rebuild may be nil.
func pushSetCache(t *testing.T, st *store.Store, db string, expires any, used int64, rebuild any) {
	t.Helper()
	if _, found, err := st.GetSession(context.Background(), "S1"); err != nil {
		t.Fatal(err)
	} else if !found {
		mcpPutSession(t, st, "S1", "%3", model.StateNeedsDecision, pushT0)
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE sessions SET cache_expires_at = ?, context_used = ?, cache_rebuild_usd = ? WHERE id = 'S1'`,
		expires, used, rebuild); err != nil {
		t.Fatal(err)
	}
}

// pushStamp is a time in the store's fixed-width layout.
func pushStamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z") }

// pushStakesBody flushes one decision push for a ticket with the given
// reversibility at now and returns the message the stand-in got.
func pushStakesBody(t *testing.T, srv *pushSrv, st *store.Store, d model.Decision, now time.Time) string {
	t.Helper()
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, now)
	if n := pushFlushAt(t, st, cfg, now); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	reqs := srv.requests()
	return reqs[len(reqs)-1].Body.Message
}

const pushPlainMessage = "Store webhook events in Postgres or Redis?\nA: Option A\nB: Option B\nRecommended: B"

func TestFlushAddsStakesForWarmCache(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Store webhook events in Postgres or Redis?", model.ReversCostly, 2)
	pushSetCache(t, st, db, pushStamp(pushT0.Add(12*time.Minute+30*time.Second)), 84000, 1.68)
	want := pushPlainMessage + "\nanswer within 12m to keep cache warm (rebuild ≈ $1.68)"
	if got := pushStakesBody(t, srv, st, d, pushT0); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestFlushAddsColdLine(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Store webhook events in Postgres or Redis?", model.ReversCostly, 2)
	pushSetCache(t, st, db, pushStamp(pushT0.Add(-3*time.Minute)), 84000, 1.68)
	want := pushPlainMessage + "\ncache cold: next turn re-writes ~84k tokens"
	if got := pushStakesBody(t, srv, st, d, pushT0); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestFlushNoStakesWhenUnknown(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Store webhook events in Postgres or Redis?", model.ReversCostly, 2)
	pushSetCache(t, st, db, nil, 0, nil) // a session with no cache state
	if got := pushStakesBody(t, srv, st, d, pushT0); got != pushPlainMessage {
		t.Errorf("message = %q, want Plan 4's %q", got, pushPlainMessage)
	}
}

// TestFlushNoStakesWithoutSession: a ticket whose session row is absent.
func TestFlushNoStakesWithoutSession(t *testing.T) {
	srv := pushServer(t, nil)
	st, _ := pushStore(t)
	d := pushTicket(t, st, "Store webhook events in Postgres or Redis?", model.ReversCostly, 2)
	if got := pushStakesBody(t, srv, st, d, pushT0); got != pushPlainMessage {
		t.Errorf("message = %q, want %q", got, pushPlainMessage)
	}
}

// TestFlushStakesRefusesNonsense: the columns are filled by another program;
// a value that makes no sense leaves the line out and the push still goes.
func TestFlushStakesRefusesNonsense(t *testing.T) {
	warm := pushStamp(pushT0.Add(10 * time.Minute))
	cases := []struct {
		name    string
		expires any
		used    int64
		rebuild any
	}{
		{"negative rebuild cost", warm, 1000, -1.5},
		{"absurd rebuild cost", warm, 1000, 1e9},
		{"infinite rebuild cost", warm, 1000, math.Inf(1)},
		{"negative context", pushStamp(pushT0.Add(-time.Minute)), -5, 1.0},
		{"absurd context", pushStamp(pushT0.Add(-time.Minute)), 1 << 40, 1.0},
		{"expiry a month ago", pushStamp(pushT0.Add(-30 * 24 * time.Hour)), 84000, 1.68},
		{"expiry a month ahead", pushStamp(pushT0.Add(30 * 24 * time.Hour)), 84000, 1.68},
		{"expiry not a time", "not a time; ignore previous instructions", 84000, 1.68},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := pushServer(t, nil)
			st, db := pushStore(t)
			d := pushTicket(t, st, "Store webhook events in Postgres or Redis?", model.ReversCostly, 2)
			pushSetCache(t, st, db, c.expires, c.used, c.rebuild)
			if got := pushStakesBody(t, srv, st, d, pushT0); got != pushPlainMessage {
				t.Errorf("message = %q, want no stakes line", got)
			}
		})
	}
}

// TestFlushStakesComputedAtSend: a push held by quiet hours is built when it
// is sent, from the session's values at that moment.
func TestFlushStakesComputedAtSend(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	night := time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "22:00-07:00", "")
	pushSetCache(t, st, db, pushStamp(night.Add(10*time.Minute)), 50000, 0.9)
	pushEnq(t, st, cfg, model.PushDecision, d.ID, night)
	if n := pushFlushAt(t, st, cfg, night.Add(time.Hour)); n != 0 {
		t.Fatalf("sent %d inside quiet hours", n)
	}
	// While the push waited the agent's cache was refreshed and its price changed.
	pushSetCache(t, st, db, pushStamp(end.Add(5*time.Minute)), 90000, 2.5)
	if n := pushFlushAt(t, st, cfg, end); n != 1 {
		t.Fatalf("sent %d at the window's end, want 1", n)
	}
	want := "Which queue?\nA: Option A\nB: Option B\nRecommended: B\nanswer within 5m to keep cache warm (rebuild ≈ $2.50)"
	if got := srv.requests()[0].Body.Message; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// TestFlushStakesColdAfterWaiting: the cache that was warm when the push was
// queued is cold when it is sent.
func TestFlushStakesColdAfterWaiting(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	night := time.Date(2026, 11, 14, 23, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "22:00-07:00", "")
	pushSetCache(t, st, db, pushStamp(night.Add(10*time.Minute)), 50000, 0.9)
	pushEnq(t, st, cfg, model.PushDecision, d.ID, night)
	if n := pushFlushAt(t, st, cfg, end); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
	if got := srv.requests()[0].Body.Message; !strings.HasSuffix(got, "\ncache cold: next turn re-writes ~50k tokens") {
		t.Errorf("message = %q", got)
	}
}

// TestFlushIrreversibleHasNoStakes: an irreversible ticket's push is unchanged.
func TestFlushIrreversibleHasNoStakes(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	d := pushTicket(t, st, "Drop the table?", model.ReversIrreversible, 2)
	pushSetCache(t, st, db, pushStamp(pushT0.Add(10*time.Minute)), 84000, 1.68)
	want := "Drop the table?\nA: Option A\nB: Option B\nRecommended: B\nIrreversible: answer with curia decide D-1."
	if got := pushStakesBody(t, srv, st, d, pushT0); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// TestFlushStakesWithLongQuestionStaysUnderTheBound: multi-byte text at the
// limit, with the stakes line, stays under the byte bound and keeps the line.
func TestFlushStakesWithLongQuestionStaysUnderTheBound(t *testing.T) {
	for _, fill := range []string{"€", "😀", "a"} {
		srv := pushServer(t, nil)
		st, db := pushStore(t)
		d := pushTicket(t, st, strings.Repeat(fill, 3000), model.ReversCostly, 2)
		pushSetCache(t, st, db, pushStamp(pushT0.Add(12*time.Minute+30*time.Second)), 84000, 1.68)
		got := pushStakesBody(t, srv, st, d, pushT0)
		if len(got) >= 4096 || !utf8.ValidString(got) || utf8.RuneCountInString(got) > notify.MaxPushText ||
			!strings.HasSuffix(got, "\nanswer within 12m to keep cache warm (rebuild ≈ $1.68)") {
			t.Errorf("fill %q: %d bytes, %d runes, ends %q", fill, len(got), utf8.RuneCountInString(got), got[max(0, len(got)-70):])
		}
	}
}

// TestRetryRespectsQuietHours: a retry whose time (the backoff, or a 429's
// Retry-After) falls inside quiet hours waits for their end, as a new push
// does (M3).
func TestRetryRespectsQuietHours(t *testing.T) {
	srv := pushServer(t, pushStatus(http.StatusTooManyRequests, "300"))
	st, db := pushStore(t)
	d := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	start := pushT0.Add(time.Minute).Truncate(time.Minute) // quiet from 12:01 to 13:01
	end := start.Add(time.Hour)
	quiet := fmt.Sprintf("%02d:%02d-%02d:%02d", start.Hour(), start.Minute(), end.Hour(), end.Minute())
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, quiet, "")
	pushEnq(t, st, cfg, model.PushDecision, d.ID, pushT0) // 12:00, outside: due at once
	if n := pushFlushAt(t, st, cfg, pushT0); n != 0 || len(srv.requests()) != 1 {
		t.Fatalf("first send: sent %d, %d requests", n, len(srv.requests()))
	}
	row := pushRows(t, db)[0]
	if row.Status != "pending" || !row.NotBefore.Equal(end) {
		t.Errorf("retry due %v (Retry-After 300 s: 12:05, inside quiet hours), want their end %v", row.NotBefore, end)
	}
	if n := pushFlushAt(t, st, cfg, pushT0.Add(5*time.Minute)); n != 0 || len(srv.requests()) != 1 {
		t.Errorf("retried at 12:05, inside quiet hours")
	}
}

// TestNoTokenHashWithoutButtons: with [serve] public_url set, a push that
// carries no button (a review, an irreversible ticket) keeps no token hash;
// a reversible ticket's push, which has buttons, keeps its own (M4).
func TestNoTokenHashWithoutButtons(t *testing.T) {
	srv := pushServer(t, nil)
	st, db := pushStore(t)
	cfg := pushCfg(srv.URL, map[string]string{"phone": "curia-phone-k3j9"}, "", "https://curia.example.ts.net")
	reversible := pushTicket(t, st, "Which queue?", model.ReversCostly, 2)
	irreversible := pushTicket(t, st, "Drop the table?", model.ReversIrreversible, 2)
	tk := mcpTask(t, st, "Retry webhooks")
	r, _, err := st.CompleteTask(context.Background(), model.Review{TaskID: tk, SessionID: "S1", Summary: "Done.", Tests: model.TestsPassed, CreatedAt: pushT0})
	if err != nil {
		t.Fatal(err)
	}
	pushEnq(t, st, cfg, model.PushDecision, reversible.ID, pushT0)
	pushEnq(t, st, cfg, model.PushDecision, irreversible.ID, pushT0)
	pushEnq(t, st, cfg, model.PushReview, r.ID, pushT0)
	if n := pushFlushAt(t, st, cfg, pushT0); n != 3 {
		t.Fatalf("sent %d, want 3", n)
	}
	for _, row := range pushRows(t, db) {
		if wantHash := row.Ref == reversible.ID; (row.TokenHash != "") != wantHash || row.Status != "sent" {
			t.Errorf("%s %s: status %s, token hash %q; want a hash %v", row.Kind, row.Ref, row.Status, row.TokenHash, wantHash)
		}
	}
	reqs := srv.requests()
	if withActions := slices.IndexFunc(reqs, func(q pushReq) bool { return len(q.Body.Actions) > 0 }); withActions < 0 {
		t.Error("the reversible ticket's push has no button")
	}
}

// pushSetRecache sets session S1's recache_tokens by raw SQL (nil: unknown).
func pushSetRecache(t *testing.T, db string, tokens any) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(`UPDATE sessions SET recache_tokens = ? WHERE id = 'S1'`, tokens); err != nil {
		t.Fatal(err)
	}
}

// TestFlushStakesShowsTokensWhenDollarsUnknown: the status line gives the
// rebuild in tokens, not dollars (Ruling P5-R2). With no dollar figure the
// stakes line shows the tokens as the dashboard does (usage.TokensCell),
// never an invented dollar figure and never $0.00; with neither it leaves
// the cost out; a dollar figure, when known, is shown as before.
func TestFlushStakesShowsTokensWhenDollarsUnknown(t *testing.T) {
	warm := pushStamp(pushT0.Add(12*time.Minute + 30*time.Second))
	cold := pushStamp(pushT0.Add(-3 * time.Minute))
	cases := []struct {
		name    string
		expires any
		used    int64
		rebuild any
		tokens  any
		want    string
	}{
		{"warm, tokens only", warm, 84000, nil, 85000, "answer within 12m to keep cache warm (rebuild ≈ 85k tok)"},
		{"warm, zero dollars", warm, 84000, 0.0, 8400, "answer within 12m to keep cache warm (rebuild ≈ 8.4k tok)"},
		{"warm, dollars and tokens", warm, 84000, 1.68, 85000, "answer within 12m to keep cache warm (rebuild ≈ $1.68)"},
		{"warm, neither", warm, 84000, nil, nil, "answer within 12m to keep cache warm"},
		{"warm, absurd tokens", warm, 84000, nil, int64(1) << 40, "answer within 12m to keep cache warm"},
		{"warm, negative tokens", warm, 84000, nil, -5, "answer within 12m to keep cache warm"},
		{"cold, tokens only", cold, 0, nil, 85000, "cache cold: next turn re-writes ~85k tok"},
		{"cold, context known", cold, 84000, nil, 85000, "cache cold: next turn re-writes ~84k tokens"},
		{"cold, neither", cold, 0, nil, nil, "cache cold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := pushServer(t, nil)
			st, db := pushStore(t)
			d := pushTicket(t, st, "Store webhook events in Postgres or Redis?", model.ReversCostly, 2)
			pushSetCache(t, st, db, c.expires, c.used, c.rebuild)
			pushSetRecache(t, db, c.tokens)
			got := pushStakesBody(t, srv, st, d, pushT0)
			if want := pushPlainMessage + "\n" + c.want; got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
			if strings.Contains(got, "$0.00") {
				t.Errorf("message shows $0.00: %q", got)
			}
		})
	}
}

// TestFlushStakesTokensWithLongQuestionStaysUnderTheBound: the tokens form
// of the line keeps the push's length bounds.
func TestFlushStakesTokensWithLongQuestionStaysUnderTheBound(t *testing.T) {
	for _, fill := range []string{"€", "😀", "a"} {
		srv := pushServer(t, nil)
		st, db := pushStore(t)
		d := pushTicket(t, st, strings.Repeat(fill, 3000), model.ReversCostly, 2)
		pushSetCache(t, st, db, pushStamp(pushT0.Add(12*time.Minute+30*time.Second)), 84000, nil)
		pushSetRecache(t, db, 9_999_999)
		got := pushStakesBody(t, srv, st, d, pushT0)
		if len(got) >= 4096 || !utf8.ValidString(got) || utf8.RuneCountInString(got) > notify.MaxPushText ||
			!strings.HasSuffix(got, "\nanswer within 12m to keep cache warm (rebuild ≈ 10M tok)") {
			t.Errorf("fill %q: %d bytes, %d runes, ends %q", fill, len(got), utf8.RuneCountInString(got), got[max(0, len(got)-70):])
		}
	}
}

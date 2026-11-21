package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// serveweb tests: the authenticated read-only pages (spec F26, part 1).

var servewebPages = []string{"/", "/timeline", "/decisions", "/usage", "/settings", "/web.css"}

type servewebFix struct {
	t    *testing.T
	st   *store.Store
	db   string
	home string
	log  *pushLogBuffer
	srv  *httptest.Server
	tm   *tmux.Fake
	now  atomic.Int64
}

// servewebNewFix serves the endpoint with the web pages on (or off). public
// is the serve.public_url scheme ("http" or "https"); its host is the test
// server's own address.
func servewebNewFix(t *testing.T, on bool, scheme string) *servewebFix {
	t.Helper()
	return servewebNewFixW(t, on, scheme, false)
}

// servewebNewFixW is servewebNewFix with serve.web_write as write.
func servewebNewFixW(t *testing.T, on bool, scheme string, write bool) *servewebFix {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	st, db := pushStore(t)
	logger, buf := pushLogger()
	f := &servewebFix{t: t, st: st, db: db, log: buf, home: home}
	f.now.Store(serveT0.UnixNano())
	srv := httptest.NewUnstartedServer(nil)
	f.tm = &tmux.Fake{}
	deps := serveDeps{Store: st, Tmux: f.tm, Log: logger, Now: f.clock}
	if on {
		deps.Web = servewebNew(servewebOpts{
			Store: st, Tmux: f.tm, Log: logger, Now: f.clock, TTL: time.Hour, Write: write,
			PublicURL: scheme + "://" + srv.Listener.Addr().String(),
		})
	}
	h := serveHandler(deps)
	srv.Config = serveNewServer(h, logger)
	srv.Start()
	t.Cleanup(srv.Close)
	f.srv = srv
	t.Cleanup(func() {
		if s := buf.String(); strings.Contains(s, "curia_session") {
			t.Errorf("the log holds a cookie name or value: %s", s)
		}
	})
	return f
}

func (f *servewebFix) clock() time.Time { return time.Unix(0, f.now.Load()).UTC() }

func (f *servewebFix) origin() string { return "http://" + f.srv.Listener.Addr().String() }

// device stores a device token and returns the plain token.
func (f *servewebFix) device(name string) string {
	f.t.Helper()
	plain, hash, err := notify.NewToken(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.AddDevice(context.Background(), name, hash, f.clock()); err != nil {
		f.t.Fatal(err)
	}
	return plain
}

// servewebResp is a response with its body read and closed.
type servewebResp struct {
	StatusCode int
	Header     http.Header
	body       string
}

func (r *servewebResp) Cookies() []*http.Cookie { return (&http.Response{Header: r.Header}).Cookies() }

func (f *servewebFix) do(method, path, cookie string, form url.Values, edit ...func(*http.Request)) *servewebResp {
	f.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for _, e := range edit {
		e(req)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return &servewebResp{resp.StatusCode, resp.Header, string(b)}
}

func servewebBody(_ *testing.T, r *servewebResp) string { return r.body }

// login exchanges token for a session and returns the Cookie header value.
func (f *servewebFix) login(token string) string {
	f.t.Helper()
	r := f.do(http.MethodPost, "/login", "", url.Values{"token": {token}}, func(r *http.Request) { r.Header.Set("Origin", f.origin()) })
	if r.StatusCode != http.StatusSeeOther {
		f.t.Fatalf("login = %d, want 303", r.StatusCode)
	}
	for _, c := range r.Cookies() {
		return c.Name + "=" + c.Value
	}
	f.t.Fatal("login set no cookie")
	return ""
}

func TestServewebOffServesNothingNew(t *testing.T) {
	f := servewebNewFix(t, false, "http")
	for _, p := range append([]string{"/login"}, servewebPages...) {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			if r := f.do(m, p, "", nil); r.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s = %d with serve.web off, want 404", m, p, r.StatusCode)
			}
		}
	}
	if r := f.do(http.MethodGet, "/healthz", "", nil); r.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 as today", r.StatusCode)
	}
	r := f.do(http.MethodPost, "/v1/answer", "", url.Values{"x": {"y"}})
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /v1/answer = %d, want the unchanged 400 for a non-JSON body", r.StatusCode)
	}
}

func TestServewebNeedsACookie(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	for _, p := range servewebPages {
		if r := f.do(http.MethodGet, p, "", nil); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with no cookie = %d, want 401", p, r.StatusCode)
		}
		if r := f.do(http.MethodGet, p, "curia_session=nonsense", nil); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s with a made-up cookie = %d, want 401", p, r.StatusCode)
		}
	}
	token := f.device("phone")
	cookie := f.login(token)
	for _, p := range servewebPages {
		if r := f.do(http.MethodGet, p, cookie, nil); r.StatusCode != http.StatusOK {
			t.Errorf("GET %s with a session = %d, want 200", p, r.StatusCode)
		}
	}
}

func TestServewebRevokedDeviceFailsAtOnce(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	cookie := f.login(f.device("phone"))
	if r := f.do(http.MethodGet, "/", cookie, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("before revoke = %d", r.StatusCode)
	}
	if ok, err := f.st.DeleteDevice(context.Background(), "phone"); err != nil || !ok {
		t.Fatalf("DeleteDevice = %v, %v", ok, err)
	}
	if r := f.do(http.MethodGet, "/", cookie, nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("after revoke = %d, want 401", r.StatusCode)
	}
	other := f.login(f.device("tablet"))
	if r := f.do(http.MethodGet, "/", other, nil); r.StatusCode != http.StatusOK {
		t.Errorf("another device's session = %d, want 200", r.StatusCode)
	}
	// the same name with a new token does not revive the old session
	f.device("phone")
	if r := f.do(http.MethodGet, "/", cookie, nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("after the name was reused = %d, want 401", r.StatusCode)
	}
}

func TestServewebSessionExpires(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	cookie := f.login(f.device("phone"))
	f.now.Store(f.clock().Add(time.Hour + time.Second).UnixNano())
	if r := f.do(http.MethodGet, "/", cookie, nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("after the lifetime = %d, want 401", r.StatusCode)
	}
}

func TestServewebWrongOriginOrHost(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	token := f.device("phone")
	cookie := f.login(token)
	for _, p := range servewebPages {
		r := f.do(http.MethodGet, p, cookie, nil, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s with a wrong Origin = %d, want 403", p, r.StatusCode)
		}
		r = f.do(http.MethodGet, p, cookie, nil, func(r *http.Request) { r.Host = "evil.example" })
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s with a wrong Host = %d, want 403", p, r.StatusCode)
		}
	}
	for _, edit := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
		func(r *http.Request) { r.Host = "evil.example" },
		func(r *http.Request) { r.Header.Set("Origin", "null") },
	} {
		if r := f.do(http.MethodPost, "/login", "", url.Values{"token": {token}}, edit); r.StatusCode != http.StatusForbidden {
			t.Errorf("login from a wrong Origin or Host = %d, want 403", r.StatusCode)
		}
	}
	// the right Origin is fine
	r := f.do(http.MethodGet, "/", cookie, nil, func(r *http.Request) { r.Header.Set("Origin", f.origin()) })
	if r.StatusCode != http.StatusOK {
		t.Errorf("GET / with the right Origin = %d, want 200", r.StatusCode)
	}
}

func TestServewebOnlyGetAndHead(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	cookie := f.login(f.device("phone"))
	for _, p := range servewebPages {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
			for _, c := range []string{cookie, ""} {
				r := f.do(m, p, c, nil)
				if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "GET, HEAD" {
					t.Errorf("%s %s = %d (Allow %q), want 405 and Allow GET, HEAD", m, p, r.StatusCode, r.Header.Get("Allow"))
				}
			}
		}
		if r := f.do(http.MethodHead, p, cookie, nil); r.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", p, r.StatusCode)
		}
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if r := f.do(m, "/login", "", nil); r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /login = %d, want 405", m, r.StatusCode)
		}
	}
	// no route takes a state-changing path
	for _, p := range []string{"/v1/task/T-1/status", "/v1/say", "/v1/say/M-1/confirm", "/logout"} {
		if r := f.do(http.MethodPost, p, cookie, url.Values{"x": {"y"}}); r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404: part 2 routes are not built", p, r.StatusCode)
		}
	}
}

func TestServewebCookieAttributes(t *testing.T) {
	for _, c := range []struct {
		scheme string
		secure bool
	}{{"http", false}, {"https", true}} {
		f := servewebNewFix(t, true, c.scheme)
		token := f.device("phone")
		r := f.do(http.MethodPost, "/login", "", url.Values{"token": {token}}, func(r *http.Request) { r.Header.Set("Origin", c.scheme+"://"+f.srv.Listener.Addr().String()) })
		if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
			t.Fatalf("%s login = %d to %q, want 303 to /", c.scheme, r.StatusCode, r.Header.Get("Location"))
		}
		cs := r.Cookies()
		if len(cs) != 1 {
			t.Fatalf("%s login set %d cookies", c.scheme, len(cs))
		}
		ck := cs[0]
		if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Secure != c.secure || ck.MaxAge != 3600 || ck.Path != "/" {
			t.Errorf("%s cookie = %+v, want HttpOnly, Strict, Secure=%v, Max-Age 3600, Path /", c.scheme, ck, c.secure)
		}
		if strings.Contains(ck.Value, token) || len(ck.Value) < 40 {
			t.Errorf("the cookie value %q is the token or too short", ck.Value)
		}
	}
}

func TestServewebWrongTokenIsUniformAndLimited(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	good := f.device("phone")
	post := func(tok string) *servewebResp {
		return f.do(http.MethodPost, "/login", "", url.Values{"token": {tok}}, func(r *http.Request) { r.Header.Set("Origin", f.origin()) })
	}
	var first string
	for i := range servewebMaxFailures {
		r := post(strings.Repeat("a", 43+i%2))
		b := servewebBody(t, r)
		if r.StatusCode != http.StatusUnauthorized || len(r.Cookies()) != 0 {
			t.Fatalf("wrong token %d = %d with %d cookies, want 401 and none", i, r.StatusCode, len(r.Cookies()))
		}
		if first == "" {
			first = b
		} else if b != first {
			t.Errorf("a wrong token's body differs: %q vs %q", b, first)
		}
	}
	if r := post(strings.Repeat("b", 43)); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("wrong token after %d failures = %d, want 429", servewebMaxFailures, r.StatusCode)
	}
	if r := post(good); r.StatusCode != http.StatusSeeOther {
		t.Errorf("the right token while limited = %d, want 303 (a valid token always works)", r.StatusCode)
	}
	f.now.Store(f.clock().Add(2 * time.Minute).UnixNano())
	if r := post(strings.Repeat("c", 43)); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong token after the window = %d, want 401", r.StatusCode)
	}
	if strings.Contains(f.log.String(), strings.Repeat("a", 43)) || strings.Contains(f.log.String(), good) {
		t.Error("the log holds a token")
	}
}

func TestServewebLimiterTableIsBounded(t *testing.T) {
	l := servewebNewLimiter(servewebMaxFailures)
	now := serveT0
	for i := range 10 * servewebMaxAddrs {
		l.failed(string(rune('a'+i%26))+strings.Repeat("x", i), now)
	}
	if n := l.size(); n > servewebMaxAddrs {
		t.Errorf("the limiter holds %d addresses, want at most %d", n, servewebMaxAddrs)
	}
}

func TestServewebTokenIsShownOnceAndOnlyItsHashStored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	db := filepath.Join(home, "curia.db")
	t.Setenv("CURIA_DB", db)
	userTerminalAs(t, true)
	out, err := runRoot(t, "serve", "token", "laptop")
	if err != nil {
		t.Fatalf("serve token: %v\n%s", err, out)
	}
	token := servewebTokenIn(t, out)
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	devs, err := st.Devices(context.Background())
	if err != nil || len(devs) != 1 || devs[0].Name != "laptop" || devs[0].TokenSHA256 != notify.HashToken(token) || devs[0].CreatedAt.IsZero() {
		t.Fatalf("devices = %+v, %v; want laptop with the token's SHA-256", devs, err)
	}
	_ = st.Close()
	servewebNoTokenInFiles(t, db, token)
	list, err := runRoot(t, "serve", "devices")
	if err != nil || !strings.Contains(list, "laptop") || strings.Contains(list, token) || strings.Contains(list, notify.HashToken(token)) {
		t.Errorf("serve devices = %q, %v: want the name and no token or hash", list, err)
	}
	servewebRefusals(t)
}

func servewebTokenIn(t *testing.T, out string) string {
	t.Helper()
	for _, w := range strings.Fields(out) {
		if len(w) == 43 && !strings.ContainsAny(w, "/:.") {
			return w
		}
	}
	t.Fatalf("the output shows no 43-character token:\n%s", out)
	return ""
}

func servewebNoTokenInFiles(t *testing.T, db, token string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal"} {
		if b, rerr := os.ReadFile(filepath.Clean(db + suffix)); rerr == nil && strings.Contains(string(b), token) {
			t.Errorf("the plain token is in %s%s", filepath.Base(db), suffix)
		}
	}
}

func servewebRefusals(t *testing.T) {
	t.Helper()
	if _, err := runRoot(t, "serve", "token", "laptop"); err == nil {
		t.Error("a second token for the same device name was made")
	}
	if _, err := runRoot(t, "serve", "token", "Bad Name"); err == nil {
		t.Error("a bad device name was accepted")
	}
	if _, err := runRoot(t, "serve", "revoke", "laptop"); err != nil {
		t.Fatalf("serve revoke: %v", err)
	}
	if _, err := runRoot(t, "serve", "revoke", "laptop"); err == nil {
		t.Error("revoking a device that is not there succeeded")
	}
	if list, _ := runRoot(t, "serve", "devices"); strings.Contains(list, "laptop") {
		t.Errorf("a revoked device is still listed: %q", list)
	}
}

// servewebHostile are task titles an agent or an import could carry.
var servewebHostile = []string{
	`<script>alert(1)</script>`,
	`<img src=x onerror=alert(1)>`,
	"red\x1b[31mtext\x1b]0;title\x07",
	"bidi \u202eevil\u202c end",
}

func servewebSeed(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, title := range servewebHostile {
		if _, err := st.CreateTask(ctx, model.Task{Title: title, Body: "SECRET-BODY-TEXT", Repo: "/r/api", Status: model.TaskReady, CreatedAt: serveT0.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		d := pushTicket(t, st, title, model.ReversCheap, 2)
		if _, err := st.AnswerDecision(ctx, d.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, serveT0.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
}

func servewebCheckClean(t *testing.T, page, body string) {
	t.Helper()
	for _, bad := range []string{"<script>alert", "<img src=x", "\x1b", "\u202e", "\u202c", "\x07"} {
		if strings.Contains(body, bad) {
			t.Errorf("%s holds %q unescaped or uncleaned", page, bad)
		}
	}
}

func TestServewebPagesEscapeHostileText(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	servewebSeed(t, f.st)
	cookie := f.login(f.device("phone"))
	seen := false
	for _, p := range []string{"/", "/timeline", "/decisions", "/usage", "/settings"} {
		r := f.do(http.MethodGet, p, cookie, nil)
		b := servewebBody(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", p, r.StatusCode)
		}
		servewebCheckClean(t, p, b)
		if strings.Contains(b, "&lt;script&gt;") {
			seen = true
		}
	}
	if !seen {
		t.Error("no page shows the hostile title escaped: the seed did not reach a page")
	}
}

func TestServewebPagesHoldNoForbiddenData(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	ctx := context.Background()
	const (
		transcript = "/Users/someone/.claude/projects/-secret-proj/TRANSCRIPT-9f3a.jsonl"
		topic      = "ntfy-topic-s3cr3t-xyz"
		linearKey  = "lin_api_SECRETKEYVALUE123"
		lastLine   = "LAST-ASSISTANT-MESSAGE-TEXT"
	)
	t.Setenv("CURIA_LINEAR_API_KEY", linearKey)
	cfgDir := filepath.Join(f.home, ".config", "curia")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgBody := ("[ntfy]\nurl = \"https://ntfy.example\"\n[ntfy.devices]\nphone = \"" + topic + "\"\n[linear]\nteam = \"ENG\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.st.UpsertSession(ctx, model.Session{
		ID: "S9", Agent: "claude", State: model.StateIdle, Repo: "/r/api", TranscriptPath: transcript,
		StartedAt: serveT0.Add(-time.Hour), UpdatedAt: serveT0.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	d := pushTicket(t, f.st, "Which index?", model.ReversCheap, 2)
	plain, hash, _ := notify.NewToken(rand.Reader)
	if _, err := f.st.EnqueuePushes(ctx, model.PushDecision, d.ID, []string{"phone"}, serveT0, serveT0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.ClaimPush(ctx, hash, serveT0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.CreateTask(ctx, model.Task{Title: "plain task", Body: "SECRET-BODY-TEXT", Repo: "/r/api", Status: model.TaskReady, CreatedAt: serveT0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, `INSERT INTO events (session_id, kind, at, payload) VALUES ('S9', 'Stop', ?, json_object('last_assistant_message', ?))`,
		serveT0.Add(-time.Minute).UTC().Format("2006-01-02T15:04:05.000000000Z"), lastLine); err != nil {
		t.Fatal(err)
	}
	cookie := f.login(f.device("phone"))
	for _, p := range servewebPages {
		b := servewebBody(t, f.do(http.MethodGet, p, cookie, nil))
		for _, bad := range []string{transcript, "TRANSCRIPT-9f3a", plain, hash, topic, linearKey, lastLine, "SECRET-BODY-TEXT", "Because."} {
			if strings.Contains(b, bad) {
				t.Errorf("GET %s shows %q", p, bad)
			}
		}
	}
	// the pages do show the data they are for
	if b := servewebBody(t, f.do(http.MethodGet, "/", cookie, nil)); !strings.Contains(b, "plain task") {
		t.Errorf("the board does not show the task")
	}
	if b := servewebBody(t, f.do(http.MethodGet, "/timeline", cookie, nil)); !strings.Contains(b, "Which index?") {
		t.Errorf("the timeline does not show the ticket")
	}
	if b := servewebBody(t, f.do(http.MethodGet, "/settings", cookie, nil)); !strings.Contains(b, "(1 entries)") {
		t.Errorf("the settings page does not show ntfy.devices as a count")
	}
}

func TestServewebHeadersOnEveryResponse(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	token := f.device("phone")
	cookie := f.login(token)
	post := func(tok string) *servewebResp {
		return f.do(http.MethodPost, "/login", "", url.Values{"token": {tok}}, func(r *http.Request) { r.Header.Set("Origin", f.origin()) })
	}
	rs := map[string]*servewebResp{
		"page":         f.do(http.MethodGet, "/", cookie, nil),
		"css":          f.do(http.MethodGet, "/web.css", cookie, nil),
		"login form":   f.do(http.MethodGet, "/login", "", nil),
		"401":          f.do(http.MethodGet, "/", "", nil),
		"403":          f.do(http.MethodGet, "/", cookie, nil, func(r *http.Request) { r.Host = "evil.example" }),
		"405":          f.do(http.MethodPost, "/", cookie, nil),
		"login 303":    post(token),
		"login 401":    post("nope"),
		"HEAD":         f.do(http.MethodHead, "/", cookie, nil),
		"login 403":    f.do(http.MethodPost, "/login", "", url.Values{"token": {token}}, func(r *http.Request) { r.Host = "evil.example" }),
		"login 405":    f.do(http.MethodPut, "/login", "", nil),
		"login 400":    f.do(http.MethodPost, "/login", "", url.Values{}, func(r *http.Request) { r.Header.Set("Origin", f.origin()) }),
		"settings 200": f.do(http.MethodGet, "/settings", cookie, nil),
	}
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"X-Frame-Options":         "DENY",
		"Cache-Control":           "no-store",
	}
	for name, r := range rs {
		for h, v := range want {
			if got := r.Header.Get(h); got != v {
				t.Errorf("%s: %s = %q, want %q", name, h, got, v)
			}
		}
	}
	// no inline script, no third-party origin in any page
	for _, p := range servewebPages[:5] {
		b := servewebBody(t, f.do(http.MethodGet, p, cookie, nil))
		for _, bad := range []string{"<script", `src="http`, `href="http`, " style=", " onclick="} {
			if strings.Contains(b, bad) {
				t.Errorf("%s holds %q", p, bad)
			}
		}
	}
}

func TestServewebStartLine(t *testing.T) {
	var out strings.Builder
	serveWebStartLine(&out, "127.0.0.1:8787", false)
	s := out.String()
	for _, want := range []string{"127.0.0.1:8787", "device token", "private", "tailnet", "Funnel"} {
		if !strings.Contains(s, want) {
			t.Errorf("start line %q lacks %q", s, want)
		}
	}
}

func TestServewebServerUsesNoSlogForTokens(t *testing.T) {
	// the handler logs through the one logger it was given: a discard logger must work
	st, _ := pushStore(t)
	w := servewebNew(servewebOpts{Store: st, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return serveT0 }, TTL: time.Hour, PublicURL: "http://127.0.0.1:1"})
	if w == nil {
		t.Fatal("servewebNew returned nil")
	}
}

func TestServewebSettingsHoldNoPathsOrCommands(t *testing.T) {
	f := servewebNewFix(t, true, "http")
	path, err := configUserPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := "db_path = \"" + f.home + "/PATH-DB/curia.db\"\nlog_path = \"" + f.home + "/PATH-LOG/c.log\"\neditor = \"EDITOR-CMD -w\"\n[obsidian]\ndir = \"" + f.home + "/PATH-VAULT\"\n[costlens]\nanalyzer = \"" + f.home + "/PATH-AN.js\"\n"
	if err = os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	b := servewebBody(t, f.do(http.MethodGet, "/settings", f.login(f.device("phone")), nil))
	for _, bad := range []string{f.home, "PATH-DB", "PATH-LOG", "PATH-VAULT", "PATH-AN", "EDITOR-CMD"} {
		if strings.Contains(b, bad) {
			t.Errorf("the settings page shows %q", bad)
		}
	}
	if !strings.Contains(b, "(set)") {
		t.Errorf("the settings page does not say a path key is set")
	}
}

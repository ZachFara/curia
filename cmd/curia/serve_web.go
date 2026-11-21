package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
	"github.com/ZachFara/curia/internal/ui"
)

// The read-only web pages of `curia serve` (spec F26 part 1, Ruling 153). No
// route here changes anything but the login session in this process's memory.

//go:embed webtmpl/*
var servewebFiles embed.FS

var (
	servewebPageTmpl  = template.Must(template.ParseFS(servewebFiles, "webtmpl/page.html"))
	servewebLoginPage = mustRead(servewebFiles, "webtmpl/login.html")
	servewebCSS       = mustRead(servewebFiles, "webtmpl/web.css")
)

func mustRead(fs embed.FS, name string) []byte {
	b, err := fs.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// The web page's bounds.
const (
	servewebCookie       = "curia_session"
	servewebMaxFailures  = 5   // wrong tokens per address per minute before a wrong one is 429
	servewebMaxAddrs     = 128 // addresses the limiter remembers
	servewebMaxSessions  = 64
	servewebMaxWrites    = 20      // state-changing requests per device per minute
	servewebMaxWriteBody = 8 << 10 // bytes of a state-changing request
	servewebMaxLoginBody = 1 << 10
	servewebColumnCap    = 100 // cards per board column
	servewebRowCap       = 200 // rows of the timeline and the decision log
	servewebTextLimit    = 120
	servewebSince        = 24 * time.Hour
	servewebCSP          = "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
)

// servewebRoutes are the routes; the value is whether POST is also allowed.
var servewebRoutes = map[string]bool{
	"/": false, "/timeline": false, "/decisions": false, "/usage": false, "/settings": false, "/web.css": false, "/login": true,
}

type servewebOpts struct {
	Store     *store.Store
	Log       *slog.Logger
	Now       func() time.Time
	TTL       time.Duration // a session cookie's lifetime
	PublicURL string        // serve.public_url, or the listener's own URL
	Tmux      tmux.Client   // delivers a confirmed message
	Write     bool          // serve.web_write
}

type servewebSession struct {
	device, hash string // the device and its token hash when the session began
	csrf         string // 256 random bits, made with the session (so rotated on every login)
	flash        string // the last result of a write, shown for a minute
	flashUntil   time.Time
	exp          time.Time
}

type serveWeb struct {
	servewebOpts
	host, origin string
	secure       bool
	lim, wlim    *servewebLimiter // wrong tokens per address; writes per device

	mu       sync.Mutex
	sessions map[[32]byte]servewebSession // by SHA-256 of the cookie value
}

func servewebNew(o servewebOpts) *serveWeb {
	w := &serveWeb{servewebOpts: o, lim: servewebNewLimiter(servewebMaxFailures), wlim: servewebNewLimiter(servewebMaxWrites), sessions: map[[32]byte]servewebSession{}}
	if u, err := url.Parse(o.PublicURL); err == nil {
		w.host, w.origin, w.secure = u.Host, u.Scheme+"://"+u.Host, u.Scheme == "https"
	}
	return w
}

// handles reports whether path is one of the web routes.
func (w *serveWeb) handles(path string) bool {
	_, ok := servewebRoutes[path]
	_, wok := servewebWriteRoutes[path]
	return ok || wok && w.Write
}

func (w *serveWeb) reply(rw http.ResponseWriter, r *http.Request, route string, code int, ctype string, body []byte) {
	h := rw.Header()
	if ctype != "" {
		h.Set("Content-Type", ctype)
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	rw.WriteHeader(code)
	_, _ = rw.Write(body)
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	w.Log.Info("serve: web", "remote", host, "method", serveMethod(r.Method), "path", route, "status", code)
}

func (w *serveWeb) text(rw http.ResponseWriter, r *http.Request, route string, code int, msg string) {
	w.reply(rw, r, route, code, "text/plain; charset=utf-8", []byte(msg+"\n"))
}

// ServeHTTP: headers, then Host and Origin (403), method (405), login, the
// session (401), and only then a page.
func (w *serveWeb) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	h := rw.Header()
	h.Set("Content-Security-Policy", servewebCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	route := r.URL.EscapedPath()
	if !w.originOK(r) {
		w.text(rw, r, route, http.StatusForbidden, "forbidden")
		return
	}
	if _, ok := servewebWriteRoutes[route]; ok && w.Write && servewebWriteRoutes[route] {
		w.action(rw, r, route)
		return
	}
	post := servewebRoutes[route]
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
	case r.Method == http.MethodPost && post:
		w.login(rw, r)
		return
	default:
		allow := "GET, HEAD"
		if post {
			allow += ", POST"
		}
		h.Set("Allow", allow)
		w.text(rw, r, route, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if route == "/login" {
		w.reply(rw, r, route, http.StatusOK, "text/html; charset=utf-8", servewebLoginPage)
		return
	}
	_, sess, ok := w.authed(r)
	if !ok {
		w.text(rw, r, route, http.StatusUnauthorized, "unauthorized: log in at /login")
		return
	}
	w.page(rw, r, route, sess)
}

// originOK: the Host is the public URL's host and an Origin, when sent, is its origin.
func (w *serveWeb) originOK(r *http.Request) bool {
	if w.host == "" || !strings.EqualFold(r.Host, w.host) {
		return false
	}
	o := r.Header.Values("Origin")
	return len(o) == 0 || len(o) == 1 && strings.EqualFold(o[0], w.origin)
}

// login exchanges a device token for a session cookie.
func (w *serveWeb) login(rw http.ResponseWriter, r *http.Request) {
	now := w.Now()
	r.Body = http.MaxBytesReader(rw, r.Body, servewebMaxLoginBody)
	_ = r.ParseForm()
	hash := notify.HashToken(r.PostForm.Get("token")) // a malformed token costs the same lookup
	devs, err := w.Store.Devices(r.Context())
	if err != nil {
		w.Log.Error("serve: web login failed", "err", sanitize.Line(err.Error(), 200))
		w.text(rw, r, "/login", http.StatusInternalServerError, "internal")
		return
	}
	dev := ""
	for _, d := range devs { // all of them, in constant time
		if subtle.ConstantTimeCompare([]byte(d.TokenSHA256), []byte(hash)) == 1 {
			dev = d.Name
		}
	}
	addr, _, _ := net.SplitHostPort(r.RemoteAddr)
	if dev == "" {
		if w.lim.limited(addr, now) {
			w.text(rw, r, "/login", http.StatusTooManyRequests, "too many attempts")
			return
		}
		w.lim.failed(addr, now)
		w.text(rw, r, "/login", http.StatusUnauthorized, "unauthorized")
		return
	}
	value, err := w.newSession(dev, hash, now)
	if err != nil {
		w.text(rw, r, "/login", http.StatusInternalServerError, "internal")
		return
	}
	ck := &http.Cookie{
		Name: servewebCookie, Value: value, Path: "/", MaxAge: int(w.TTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: true,
	}
	ck.Secure = w.secure || r.TLS != nil // plain HTTP only when the public URL is plain HTTP
	http.SetCookie(rw, ck)
	rw.Header().Set("Location", "/")
	w.reply(rw, r, "/login", http.StatusSeeOther, "", nil)
}

func (w *serveWeb) newSession(dev, hash string, now time.Time) (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(b)
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, s := range w.sessions {
		if !now.Before(s.exp) {
			delete(w.sessions, k)
		}
	}
	for len(w.sessions) >= servewebMaxSessions { // the one that ends soonest
		var oldest [32]byte
		first := true
		for k, s := range w.sessions {
			if first || s.exp.Before(w.sessions[oldest].exp) {
				oldest, first = k, false
			}
		}
		delete(w.sessions, oldest)
	}
	c := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, c); err != nil {
		return "", err
	}
	w.sessions[sha256.Sum256([]byte(value))] = servewebSession{device: dev, hash: hash, csrf: base64.RawURLEncoding.EncodeToString(c), exp: now.Add(w.TTL)}
	return value, nil
}

// authed: a live session whose device still holds the same token.
func (w *serveWeb) authed(r *http.Request) ([32]byte, servewebSession, bool) {
	c, err := r.Cookie(servewebCookie)
	if err != nil {
		return [32]byte{}, servewebSession{}, false
	}
	key := sha256.Sum256([]byte(c.Value))
	w.mu.Lock()
	s, ok := w.sessions[key]
	w.mu.Unlock()
	if !ok || !w.Now().Before(s.exp) {
		return key, s, false
	}
	devs, err := w.Store.Devices(r.Context())
	if err != nil {
		return key, s, false
	}
	for _, d := range devs {
		if d.Name == s.device && subtle.ConstantTimeCompare([]byte(d.TokenSHA256), []byte(s.hash)) == 1 {
			return key, s, true
		}
	}
	return key, s, false
}

// servewebLimiter counts wrong tokens per remote address in one-minute windows.
type servewebLimiter struct {
	max int // events per window before limited
	mu  sync.Mutex
	m   map[string]*servewebWindow
}

type servewebWindow struct {
	minute time.Time
	n      int
}

func servewebNewLimiter(limit int) *servewebLimiter {
	return &servewebLimiter{max: limit, m: map[string]*servewebWindow{}}
}

func (l *servewebLimiter) size() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.m) }

func (l *servewebLimiter) limited(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[addr]
	return e != nil && e.minute.Equal(now.Truncate(time.Minute)) && e.n >= l.max
}

func (l *servewebLimiter) failed(addr string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := now.Truncate(time.Minute)
	e := l.m[addr]
	if e == nil {
		for k, o := range l.m { // make room: stale windows first, then any one
			if len(l.m) < servewebMaxAddrs {
				break
			}
			if !o.minute.Equal(m) {
				delete(l.m, k)
			}
		}
		for k := range l.m {
			if len(l.m) < servewebMaxAddrs {
				break
			}
			delete(l.m, k)
		}
		e = &servewebWindow{}
		l.m[addr] = e
	}
	if !e.minute.Equal(m) {
		e.minute, e.n = m, 0
	}
	e.n++
}

// servewebText is agent or imported text as a page shows it: cleaned and bounded.
func servewebText(s string, limit int) string { return ui.Cell(sanitize.Line(s, limit)) }

// servewebHiddenValue are the keys whose value is a path or a command: the
// config registry has no kind for a key, so they are listed here.
var servewebHiddenValue = map[string]bool{
	"db_path": true, "log_path": true, "obsidian.dir": true, "editor": true, "costlens.analyzer": true,
}

type servewebPage struct {
	Kind, Title string
	Columns     []servewebColumn
	Events      []servewebEvent
	Rows        []servewebDecision
	Summary     string
	Text        string
	Settings    []ui.SettingRow
	// Writes (serve.web_write): the forms, their token and the last result.
	Write       bool
	CSRF, Flash string
	Targets     []servewebTarget
	Held        []servewebHeld
}

type servewebColumn struct {
	Heading     string
	Count, More int
	Cards       []servewebCard
}

type servewebCard struct {
	ID, Title, Repo, Priority string
	Moves                     []string // the drop targets this card may go to (writes on)
}

type servewebEvent struct{ At, Who, Kind, Line string }

type servewebDecision struct{ ID, Status, Scope, Date, Reuse, Asked, Question, Answer string }

func (w *serveWeb) page(rw http.ResponseWriter, r *http.Request, route string, sess servewebSession) {
	if route == "/web.css" {
		w.reply(rw, r, route, http.StatusOK, "text/css; charset=utf-8", servewebCSS)
		return
	}
	p, err := w.build(r.Context(), route)
	if w.Write {
		p.Write, p.CSRF = true, sess.csrf
		if w.Now().Before(sess.flashUntil) {
			p.Flash = sess.flash
		}
	}
	var buf bytes.Buffer
	if err == nil {
		err = servewebPageTmpl.Execute(&buf, p)
	}
	if err != nil {
		w.Log.Error("serve: web page failed", "path", route, "err", sanitize.Line(err.Error(), 200))
		w.text(rw, r, route, http.StatusInternalServerError, "internal")
		return
	}
	w.reply(rw, r, route, http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}

// build reads one page's data through the same functions the commands use.
func (w *serveWeb) build(ctx context.Context, route string) (servewebPage, error) {
	now := w.Now()
	switch route {
	case "/timeline":
		evs, err := w.Store.Timeline(ctx, store.TimelineQuery{Since: now.Add(-servewebSince), Limit: servewebRowCap})
		p := servewebPage{Kind: "timeline", Title: "Timeline"}
		for _, e := range evs {
			line := servewebText(e.Line, servewebTextLimit)
			if e.Kind == "turn_end" { // the agent's last words come from its transcript
				line = "turn ended"
			}
			p.Events = append(p.Events, servewebEvent{e.At.UTC().Format("01-02 15:04:05"), servewebText(timelineWho(e), 12), servewebText(e.Kind, 30), line})
		}
		return p, err
	case "/decisions":
		rows, sum, err := w.Store.DecisionLog(ctx, store.DecisionLogQuery{})
		p := servewebPage{Kind: "decisions", Title: "Decision log", Summary: ui.DecisionLogSummaryLine(sum)}
		for _, r := range rows[:min(len(rows), servewebRowCap)] {
			p.Rows = append(p.Rows, servewebDecision{
				servewebText(r.ID, 20), servewebText(ui.DecisionLogStatus(r), 30), servewebText(r.Scope, 10), r.CreatedAt.UTC().Format("2006-01-02"),
				ui.DecisionLogReuse(r, now), fmt.Sprint(r.AskedAgain), servewebText(r.Question, servewebTextLimit), servewebText(r.Answer, 60),
			})
		}
		return p, err
	case "/usage":
		rep, err := usageBuild(ctx, w.Store, now)
		var buf bytes.Buffer
		if err == nil {
			err = usagePrint(&buf, rep, now)
		}
		return servewebPage{Kind: "usage", Title: "Usage", Text: buf.String()}, err
	case "/settings":
		rows, err := settingsSource{}.Rows(ctx)
		for i, r := range rows {
			if servewebHiddenValue[r.Key] { // a path or command under the home directory never leaves the machine
				rows[i].Value = "(set)"
				if r.Source == string(config.SourceDefault) {
					rows[i].Value = "(default)"
				}
			}
		}
		return servewebPage{Kind: "settings", Title: "Settings", Settings: rows}, err
	}
	if route == "/say" {
		return w.sayPage(ctx, now)
	}
	return w.board(ctx, now)
}

func (w *serveWeb) board(ctx context.Context, now time.Time) (servewebPage, error) {
	cards, err := taskCards(ctx, w.Store, now)
	p := servewebPage{Kind: "board", Title: "Board"}
	for _, st := range []model.TaskStatus{model.TaskBacklog, model.TaskReady, model.TaskRunning, model.TaskNeedsDecision, model.TaskReview, model.TaskDone, model.TaskDropped} {
		col := servewebColumn{Heading: strings.ToUpper(strings.ReplaceAll(string(st), "_", " "))}
		for _, c := range cards {
			if c.Status != st {
				continue
			}
			if col.Count++; col.Count > servewebColumnCap {
				col.More++
				continue
			}
			pri := c.Task.Priority
			if pri == 0 {
				pri = model.PriorityNormal
			}
			card := servewebCard{ID: c.Task.ID, Title: servewebText(c.Task.Title, servewebTextLimit), Repo: servewebText(filepath.Base(c.Task.Repo), 40), Priority: fmt.Sprint(pri)}
			if w.Write {
				card.Moves = servewebMovesFrom(st)
			}
			col.Cards = append(col.Cards, card)
		}
		p.Columns = append(p.Columns, col)
	}
	return p, err
}

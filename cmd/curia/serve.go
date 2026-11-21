package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/deliver"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/tmux"
)

// The endpoint's bounds (plan section 4.8).
const (
	// serveDefaultListen is the address with no --listen and no [serve]
	// listen (Ruling P4-R9): loopback only, reached through tailscale serve.
	serveDefaultListen = "127.0.0.1:8787"
	// serveMaxBody bounds the body of POST /v1/answer, in bytes.
	serveMaxBody = 1 << 10
	// serveMaxFailures is how many token checks may fail in one minute of
	// the clock; after that a request whose token does not match is 429
	// instead of 401 until the minute ends. A valid token always works.
	serveMaxFailures = 10
	// serveShutdownLimit bounds the wait for requests in flight when the
	// command is told to stop.
	serveShutdownLimit = 5 * time.Second
	// serveMaxInFlight bounds the requests handled at once; one more is
	// refused with 503 at once.
	serveMaxInFlight = 4
	// serveMaxNote bounds the delivery note of a 200, in runes.
	serveMaxNote = 200
)

// serveListen opens the listener. Tests replace it to prove that a refused
// address is never listened on.
var serveListen = (&net.ListenConfig{}).Listen

// serveListenFlag is --listen; tests reset it.
var serveListenFlag string

// serveDeps is what the handler runs on.
type serveDeps struct {
	Store *store.Store
	Tmux  tmux.Client
	Log   *slog.Logger
	Now   func() time.Time
	Web   *serveWeb // nil unless serve.web is on
}

func init() {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Answer blocking tickets from phone push buttons (the user only)",
		Long: "Answers a blocking ticket when a push button presents its one-time token:\n" +
			"POST /v1/answer with one of the ticket's options, through the same path as\n" +
			"curia decide. An irreversible ticket is never answered here. It listens on\n" +
			"127.0.0.1:8787 unless --listen or [serve] listen names another address, which\n" +
			"must be loopback or this machine's Tailscale address; nothing is detected.\n" +
			"There is no TLS: put tailscale serve in front of the loopback address (never\n" +
			"Funnel), so only your tailnet reaches it. The token is the only check, and it\n" +
			"travels in the push: keep the ntfy topic private (self-hosted or\n" +
			"access-controlled). [serve] public_url must be set. With [ntfy] configured it\n" +
			"also sends the queued phone pushes. Refuses to run inside an agent session.",
		Annotations: userOnlyMark(),
		Args:        cobra.NoArgs,
		RunE:        runServe,
	}
	cmd.Flags().StringVar(&serveListenFlag, "listen", "", "the ip:port to listen on (loopback or Tailscale only; default "+serveDefaultListen+")")
	rootCmd.AddCommand(cmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	// Only the user answers a decision (spec section 7).
	if err := userOnly("serve", "answer a decision"); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Serve.PublicURL == "" {
		return errors.New("curia serve needs [serve] public_url in ~/.config/curia/config.toml: the URL your phone reaches this endpoint at, such as https://<machine>.<tailnet>.ts.net")
	}
	addr, err := serveListenAddr(serveListenFlag, cfg.Serve)
	if err != nil {
		return err
	}
	ln, err := serveListen(cmd.Context(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("curia serve: %s", sanitize.Line(err.Error(), 200))
	}
	defer func() { _ = ln.Close() }()
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	logger, closeLog, err := openLog(cfg.LogPath)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	return serveRunServer(cmd, ln, st, cfg, logger)
}

// serveRunServer serves on ln until SIGINT, SIGTERM or the command's
// context ends, then stops within serveShutdownLimit and returns nil. A
// request does not see the stop: an answer in flight runs on a context the
// stop does not cancel (serveEndpoint.answer), and Shutdown waits for it.
func serveRunServer(cmd *cobra.Command, ln net.Listener, st *store.Store, cfg config.Config, logger *slog.Logger) error {
	ctx, stopSignals := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	_, stopPush := pushStart(ctx, st, cfg, logger) // nobody kicks: the loop's own timer sends
	where := ln.Addr().String()
	deps := serveDeps{Store: st, Tmux: newTmuxClient(), Log: logger, Now: clock}
	if cfg.Serve.Web {
		pub := cfg.Serve.PublicURL
		if pub == "" {
			pub = "http://" + where
		}
		deps.Web = servewebNew(servewebOpts{Store: st, Log: logger, Now: clock, TTL: cfg.Serve.WebSession, PublicURL: pub})
	}
	h := serveHandler(deps)
	srv := serveNewServer(h, logger)
	logger.Info("serve: listening", "addr", where)
	serveStartLines(cmd.OutOrStdout(), where)
	if cfg.Serve.Web {
		serveWebStartLine(cmd.OutOrStdout(), where)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		stopPush()
		return fmt.Errorf("curia serve: %s", sanitize.Line(err.Error(), 200))
	case <-ctx.Done():
	}
	logger.Info("serve: stopping")
	pushDone := make(chan struct{})
	go func() { defer close(pushDone); stopPush() }()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownLimit)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		logger.Warn("serve: requests still running at the shutdown limit; closing", "err", err.Error())
		_ = srv.Close()
	}
	<-pushDone
	return nil
}

// serveNewServer is the endpoint's http.Server around h (section 4.8):
// its timeouts, its header bound, no TLS, and net/http's own "OPTIONS *"
// answer turned off; the server's own errors go to logger.
func serveNewServer(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:                      h,
		ReadHeaderTimeout:            5 * time.Second,
		ReadTimeout:                  10 * time.Second,
		WriteTimeout:                 30 * time.Second,
		IdleTimeout:                  60 * time.Second,
		MaxHeaderBytes:               8 << 10,
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// serveStartLines prints where the endpoint listens and what that means (Rulings P4-R6, P4-R9).
func serveStartLines(out io.Writer, where string) {
	_, _ = fmt.Fprintf(out, "curia serve: listening on %s\n", where)
	if ap, err := netip.ParseAddrPort(where); err == nil && !ap.Addr().IsLoopback() {
		_, _ = fmt.Fprintf(out, "curia serve: listening on a non-loopback address: make sure %s is this machine's Tailscale address. "+
			"Anyone who can reach it and read your ntfy topic can answer the ticket a push names, once\n", ap.Addr())
	} else {
		_, _ = fmt.Fprintln(out, "curia serve: loopback only: put tailscale serve in front of it so your phone reaches it (never Funnel)")
	}
	_, _ = fmt.Fprintln(out, "curia serve: plain HTTP, no TLS here: a push's one-time token is the only check, so anyone who can "+
		"reach this address and holds an unspent token can answer that token's ticket. Keep the ntfy topic private "+
		"(self-hosted or access-controlled) and this endpoint on your tailnet")
}

// serveWebStartLine says the web page is on (spec F26): the same two conditions as the answer buttons.
func serveWebStartLine(out io.Writer, where string) {
	_, _ = fmt.Fprintf(out, "curia serve: the read-only web page is on at %s: every page needs a device token (curia serve token <device>). "+
		"Keep the ntfy topic private and this endpoint on your tailnet only, never Tailscale Funnel\n", where)
}

// serveListenAddr is the address to listen on: flag, else cfg, else serveDefaultListen. It returns an
// error for an address that is neither loopback nor Tailscale. Nothing is detected: a non-loopback address
// is used only when the user names it (Ruling P4-R9). An address must be an IP literal and a port, with no
// zone; an IPv4 address written as IPv6 is used as IPv4.
func serveListenAddr(flag string, cfg config.Serve) (string, error) {
	switch {
	case flag != "":
		return serveCheckAddr("--listen", flag)
	case cfg.Listen != "":
		return serveCheckAddr("[serve] listen", cfg.Listen)
	}
	return serveDefaultListen, nil
}

// serveCheckAddr checks the listen address s, named by what.
func serveCheckAddr(what, s string) (string, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return "", fmt.Errorf("curia serve: %s must be ip:port with an IP literal, got %q", what, sanitize.Line(s, 64))
	}
	a := ap.Addr().Unmap()
	if a.Zone() != "" || (!a.IsLoopback() && !config.TailscaleAddr(a)) {
		return "", fmt.Errorf("curia serve: %s must be a loopback or Tailscale address, got %q; refusing to listen", what, sanitize.Line(s, 64))
	}
	return netip.AddrPortFrom(a, ap.Port()).String(), nil
}

// The fixed response bodies.
var (
	serveBusy         = []byte(`{"error":"busy"}`)
	serveTooMany      = []byte(`{"error":"too many attempts"}`)
	serveBadRequest   = []byte(`{"error":"bad request"}`)
	serveUnauth       = []byte(`{"error":"unauthorized"}`)
	serveIrreversible = []byte(`{"error":"answer this decision with curia decide"}`)
	serveUnknownKey   = []byte(`{"error":"unknown option"}`)
	serveInternal     = []byte(`{"error":"internal"}`)
)

var (
	serveKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9]{1,8}$`)
	serveTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	serveMediaPattern = regexp.MustCompile(`^[a-z0-9.+-]{1,40}/[a-z0-9.+-]{1,40}$`)
)

// serveEndpoint is serveHandler's state: the request slots and the
// failure counter of the current minute.
type serveEndpoint struct {
	d     serveDeps
	slots chan struct{}

	mu       sync.Mutex
	minute   time.Time // the window failures counts in
	failures int
}

// serveReply is what one request gets, and what its log line says.
type serveReply struct {
	status int
	body   []byte
	id     string // the validated decision id, or ""
	why    string // a fixed reason, or ""
	ctype  string // a refused media type, cleaned, or ""
}

// serveHandler is the endpoint's whole HTTP surface (section 4.8).
func serveHandler(d serveDeps) http.Handler {
	return &serveEndpoint{d: d, slots: make(chan struct{}, serveMaxInFlight)}
}

// ServeHTTP routes by the exact escaped path, never redirecting.
func (e *serveEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		e.reply(w, r, "", serveReply{status: http.StatusServiceUnavailable, body: serveBusy, why: "busy"})
		return
	}
	path := r.URL.EscapedPath()
	if e.d.Web != nil && e.d.Web.handles(path) {
		e.d.Web.ServeHTTP(w, r)
		return
	}
	var rep serveReply
	switch {
	case path == "/healthz" && r.Method == http.MethodGet:
		rep = serveReply{status: http.StatusOK, body: []byte("ok")}
	case path == "/v1/answer" && r.Method == http.MethodPost:
		rep = e.answer(w, r)
	case path == "/healthz":
		w.Header().Set("Allow", http.MethodGet)
		rep = serveReply{status: http.StatusMethodNotAllowed}
	case path == "/v1/answer":
		w.Header().Set("Allow", http.MethodPost)
		rep = serveReply{status: http.StatusMethodNotAllowed}
	default:
		path = "other"
		rep = serveReply{status: http.StatusNotFound}
	}
	e.reply(w, r, path, rep)
}

// reply writes rep and logs the request: its remote address, method,
// route, status, decision id and reason, never a header or the body.
func (e *serveEndpoint) reply(w http.ResponseWriter, r *http.Request, route string, rep serveReply) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if len(rep.body) > 0 {
		ct := "application/json"
		if rep.status == http.StatusOK && route == "/healthz" {
			ct = "text/plain; charset=utf-8"
		}
		h.Set("Content-Type", ct)
		h.Set("Content-Length", strconv.Itoa(len(rep.body)))
	}
	w.WriteHeader(rep.status)
	_, _ = w.Write(rep.body)
	args := []any{"remote", r.RemoteAddr, "method", serveMethod(r.Method), "path", route, "status", rep.status}
	if rep.id != "" {
		args = append(args, "decision", rep.id)
	}
	if rep.why != "" {
		args = append(args, "why", rep.why)
	}
	if rep.ctype != "" {
		args = append(args, "ctype", rep.ctype)
	}
	e.d.Log.Info("serve: request", args...)
}

// serveMethod is m as logged: a standard method, or "other".
func serveMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "other"
}

// answer is POST /v1/answer, section 4.8's steps in their order, except that the token is first only
// matched: the ticket and the option are checked before it is spent, so a refused option costs no token
// (M1), and only a request whose token does not match counts against the limiter or is refused by it, so
// a valid token always works (M2).
func (e *serveEndpoint) answer(w http.ResponseWriter, r *http.Request) serveReply {
	now := e.d.Now()
	req, rep := serveReadRequest(w, r)
	if rep.status != 0 {
		return rep
	}
	// Once the token is spent the answer must be stored whatever the
	// client does, so a hang-up does not cancel what follows.
	ctx := context.WithoutCancel(r.Context())
	hash, rep := e.match(ctx, r.Header, req.Decision, now)
	if rep.status != 0 {
		return rep
	}
	id := req.Decision
	if rep := e.checkTicket(ctx, id, req.Key, now); rep.status != 0 {
		return rep
	}
	if _, err := e.d.Store.UsePushToken(ctx, hash, id, now); err != nil {
		if errors.Is(err, store.ErrBadToken) { // spent by a request a moment ago
			return serveReply{status: http.StatusUnauthorized, body: serveUnauth, id: id, why: "token refused"}
		}
		return e.internal(id, err)
	}
	out, err := deliver.Answer(ctx, e.d.Store, e.d.Tmux, e.d.Log, id, model.Answer{Key: req.Key, Via: model.AnsweredNtfy}, now)
	switch {
	case errors.Is(err, store.ErrNotOpen) || errors.Is(err, store.ErrNotFound):
		return serveReply{status: http.StatusConflict, body: serveNotOpen(id), id: id, why: "not open"}
	case errors.Is(err, decision.ErrBadAnswer):
		return serveReply{status: http.StatusUnprocessableEntity, body: serveUnknownKey, id: id, why: "unknown option"}
	case err != nil:
		return e.internal(id, err)
	}
	return serveReply{status: http.StatusOK, body: serveAnswered(out), id: id}
}

// checkTicket refuses, before the token is spent, a ticket that does not exist, an irreversible one and an
// option it does not offer (decision.Resolve's own rule).
func (e *serveEndpoint) checkTicket(ctx context.Context, id, key string, now time.Time) serveReply {
	d, found, err := e.d.Store.GetDecision(ctx, id)
	switch {
	case err != nil:
		return e.internal(id, err)
	case !found:
		return serveReply{status: http.StatusConflict, body: serveNotOpen(id), id: id, why: "not open"}
	case d.Reversibility == model.ReversIrreversible:
		return serveReply{status: http.StatusUnprocessableEntity, body: serveIrreversible, id: id, why: "irreversible"}
	}
	if _, err := decision.Resolve(d, model.Answer{Key: key, Via: model.AnsweredNtfy}, now); errors.Is(err, decision.ErrBadAnswer) {
		return serveReply{status: http.StatusUnprocessableEntity, body: serveUnknownKey, id: id, why: "unknown option"}
	}
	return serveReply{}
}

// match is step 3 without the spend: the bearer token is hashed at once and matched for id; the hash
// comes back for the spend. A malformed header still costs a lookup (of a hash no push holds), so every
// refusal does the same work; each is the same 401 and counts as a failure, or is 429 once
// serveMaxFailures have failed this minute.
func (e *serveEndpoint) match(ctx context.Context, h http.Header, id string, now time.Time) (string, serveReply) {
	token, ok := serveBearer(h)
	hash := notify.HashToken(token) // "" when malformed: NewToken never makes it
	err := e.d.Store.MatchPushToken(ctx, hash, id, now)
	switch {
	case ok && err == nil:
		return hash, serveReply{}
	case ok && !errors.Is(err, store.ErrBadToken):
		return "", e.internal(id, err)
	case e.limited(now):
		return "", serveReply{status: http.StatusTooManyRequests, body: serveTooMany, id: id, why: "too many failed token checks this minute"}
	}
	e.failed(now)
	return "", serveReply{status: http.StatusUnauthorized, body: serveUnauth, id: id, why: "token refused"}
}

// internal is a 500; its cause goes only to the log.
func (e *serveEndpoint) internal(id string, err error) serveReply {
	e.d.Log.Error("serve: answer failed", "decision", id, "err", sanitize.Line(err.Error(), 300))
	return serveReply{status: http.StatusInternalServerError, body: serveInternal, id: id, why: "internal"}
}

// limited reports whether serveMaxFailures token checks have failed in
// the minute of now.
func (e *serveEndpoint) limited(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.minute.Equal(now.Truncate(time.Minute)) && e.failures >= serveMaxFailures
}

// failed counts one failed token check in the minute of now.
func (e *serveEndpoint) failed(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m := now.Truncate(time.Minute); !e.minute.Equal(m) {
		e.minute, e.failures = m, 0
	}
	e.failures++
}

// serveBearer is the token of the request's one Authorization header,
// "Bearer " and 43 characters of base64url, and whether it is that.
func serveBearer(h http.Header) (string, bool) {
	v := h.Values("Authorization")
	if len(v) != 1 {
		return "", false
	}
	token, ok := strings.CutPrefix(v[0], "Bearer ")
	if !ok || !serveTokenPattern.MatchString(token) {
		return "", false
	}
	return token, true
}

// serveAnswerBody is the one body POST /v1/answer accepts.
type serveAnswerBody struct {
	Decision string `json:"decision"`
	Key      string `json:"key"`
}

// serveReadRequest is step 2. It checks the content type and reads and
// checks the body; a refusal is a 400 reply.
func serveReadRequest(w http.ResponseWriter, r *http.Request) (serveAnswerBody, serveReply) {
	bad := func(why string) serveReply {
		return serveReply{status: http.StatusBadRequest, body: serveBadRequest, why: why}
	}
	if mt, ok := serveJSONType(r.Header.Get("Content-Type")); !ok {
		rep := bad("content type")
		rep.ctype = mt
		return serveAnswerBody{}, rep
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, serveMaxBody))
	if err != nil {
		return serveAnswerBody{}, bad("body unreadable or over 1 KiB")
	}
	req, ok := serveDecode(b)
	if !ok {
		return serveAnswerBody{}, bad("body is not {\"decision\",\"key\"}")
	}
	if !decision.ValidID(req.Decision) || !serveKeyPattern.MatchString(req.Key) {
		return serveAnswerBody{}, bad("decision id or key malformed")
	}
	return req, serveReply{}
}

// serveJSONType reports whether ct is application/json, in any case, with
// no charset or utf-8. It also returns the media type as logged: cleaned,
// or "unparsable".
func serveJSONType(ct string) (string, bool) {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || !serveMediaPattern.MatchString(mt) {
		return "unparsable", false
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		return mt, false
	}
	return mt, mt == "application/json"
}

// serveDecode reads b as exactly one object with exactly the members
// "decision" and "key", each once and a string, spelled exactly so (the
// struct decode alone would match any case), and nothing after it but
// white space.
func serveDecode(b []byte) (serveAnswerBody, bool) {
	var req serveAnswerBody
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return serveAnswerBody{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return serveAnswerBody{}, false
	}
	return req, serveExactMembers(b)
}

// serveExactMembers reports whether b's object has the members "decision"
// and "key", each exactly once and a string, and no other.
func serveExactMembers(b []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		name, isString := t.(string)
		if err != nil || !isString || (name != "decision" && name != "key") || seen[name] {
			return false
		}
		seen[name] = true
		if v, err := dec.Token(); err != nil {
			return false
		} else if _, isString := v.(string); !isString {
			return false
		}
	}
	return len(seen) == 2
}

// serveNotOpen is the 409 body naming id, which is validated.
func serveNotOpen(id string) []byte {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{id + " is no longer open"})
	return b
}

// serveAnswered is the 200 body: the ticket, its status and the delivery,
// "delivered" or the outcome's note, cleaned and bounded.
func serveAnswered(out model.Outcome) []byte {
	delivery := "delivered"
	if out.Note != "" {
		delivery = sanitize.Line(out.Note, serveMaxNote)
	}
	b, _ := json.Marshal(struct {
		Decision string `json:"decision"`
		Status   string `json:"status"`
		Delivery string `json:"delivery"`
	}{sanitize.Line(out.Decision.ID, 20), sanitize.Line(string(out.Decision.Status), 20), delivery})
	return b
}

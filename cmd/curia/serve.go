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
	// servePort is the port of a listen address found on a Tailscale interface.
	servePort = 8787
	// serveMaxBody bounds the body of POST /v1/answer, in bytes.
	serveMaxBody = 1 << 10
	// serveMaxFailures is how many token checks may fail in one minute of
	// the clock; after that every answer is 429 until the minute ends.
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

// serveInterfaceAddrs lists this machine's addresses. Tests replace it.
var serveInterfaceAddrs = net.InterfaceAddrs

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
}

func init() {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Answer blocking tickets from phone push buttons (the user only)",
		Long: "Listens on a loopback or Tailscale address, never on any other, and answers a\n" +
			"blocking ticket when a push button presents its one-time token: POST\n" +
			"/v1/answer with one of the ticket's options, through the same path as curia\n" +
			"decide. An irreversible ticket is never answered here. The address is\n" +
			"--listen, else [serve] listen, else this machine's first Tailscale address\n" +
			"with port 8787. [serve] public_url must be set. There is no TLS: use the\n" +
			"Tailscale tunnel, or tailscale serve in front of a loopback address. With\n" +
			"[ntfy] configured it also sends the queued phone pushes. Refuses to run\n" +
			"inside an agent session.",
		Annotations: userOnlyMark(),
		Args:        cobra.NoArgs,
		RunE:        runServe,
	}
	cmd.Flags().StringVar(&serveListenFlag, "listen", "", "the ip:port to listen on (loopback or Tailscale only)")
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
	addrs, err := serveInterfaceAddrs()
	if err != nil {
		addrs = nil // only a flag or [serve] listen can then give the address
	}
	addr, err := serveListenAddr(serveListenFlag, cfg.Serve, addrs)
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
	h := serveHandler(serveDeps{Store: st, Tmux: newTmuxClient(), Log: logger, Now: clock})
	srv := serveNewServer(h, logger)
	where := ln.Addr().String()
	logger.Info("serve: listening", "addr", where)
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "curia serve: listening on %s\n", where)
	_, _ = fmt.Fprintln(out, "curia serve: plain HTTP, no TLS here: anyone who can reach this address and holds an unspent push token can answer that token's ticket")
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

// serveListenAddr is the address to listen on: flag, else cfg, else the first Tailscale address in addrs with port 8787.
// It returns an error for an address that is neither loopback nor Tailscale, and when none is found.
// An address must be an IP literal and a port, with no zone; an IPv4
// address written as IPv6 is used as IPv4.
func serveListenAddr(flag string, cfg config.Serve, addrs []net.Addr) (string, error) {
	switch {
	case flag != "":
		return serveCheckAddr("--listen", flag)
	case cfg.Listen != "":
		return serveCheckAddr("[serve] listen", cfg.Listen)
	}
	var v6 netip.Addr
	for _, a := range addrs {
		ip := serveIP(a)
		switch {
		case !config.TailscaleAddr(ip):
		case ip.Is4():
			return netip.AddrPortFrom(ip, servePort).String(), nil
		case !v6.IsValid():
			v6 = ip
		}
	}
	if v6.IsValid() {
		return netip.AddrPortFrom(v6, servePort).String(), nil
	}
	return "", errors.New("curia serve: no Tailscale address on this machine: set [serve] listen (such as 127.0.0.1:8787, behind tailscale serve) or pass --listen")
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

// serveIP is a's IP, unmapped, or the zero Addr.
func serveIP(a net.Addr) netip.Addr {
	var ip net.IP
	switch v := a.(type) {
	case *net.IPNet:
		ip = v.IP
	case *net.IPAddr:
		ip = v.IP
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}
	}
	return addr.Unmap()
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

// answer is POST /v1/answer, section 4.8's steps in their order.
func (e *serveEndpoint) answer(w http.ResponseWriter, r *http.Request) serveReply {
	now := e.d.Now()
	if e.limited(now) {
		return serveReply{status: http.StatusTooManyRequests, body: serveTooMany, why: "too many failed token checks this minute"}
	}
	req, rep := serveReadRequest(w, r)
	if rep.status != 0 {
		return rep
	}
	// Once the token is spent the answer must be stored whatever the
	// client does, so a hang-up does not cancel what follows.
	ctx := context.WithoutCancel(r.Context())
	if rep := e.spend(ctx, r.Header, req.Decision, now); rep.status != 0 {
		return rep
	}
	id := req.Decision
	d, found, err := e.d.Store.GetDecision(ctx, id)
	switch {
	case err != nil:
		return e.internal(id, err)
	case !found:
		return serveReply{status: http.StatusConflict, body: serveNotOpen(id), id: id, why: "not open"}
	case d.Reversibility == model.ReversIrreversible:
		return serveReply{status: http.StatusUnprocessableEntity, body: serveIrreversible, id: id, why: "irreversible"}
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

// spend is step 3: the bearer token is hashed at once and spent for id.
// A malformed header still costs a lookup (of a hash no push holds), so
// every refusal does the same work; each is the same 401 and counts as a
// failure.
func (e *serveEndpoint) spend(ctx context.Context, h http.Header, id string, now time.Time) serveReply {
	token, ok := serveBearer(h)
	hash := notify.HashToken(token) // "" when malformed: NewToken never makes it
	_, err := e.d.Store.UsePushToken(ctx, hash, id, now)
	switch {
	case !ok || errors.Is(err, store.ErrBadToken):
		e.failed(now)
		return serveReply{status: http.StatusUnauthorized, body: serveUnauth, id: id, why: "token refused"}
	case err != nil:
		return e.internal(id, err)
	}
	return serveReply{}
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

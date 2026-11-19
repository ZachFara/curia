package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/decision"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
)

// The push outbox's timing (plan section 4.7).
const (
	// pushBackoff is the wait before a push's second attempt; it doubles
	// with each attempt after that.
	pushBackoff = 30 * time.Second
	// pushBackoffMax bounds every wait before a retry, a 429's Retry-After
	// included.
	pushBackoffMax = 15 * time.Minute
	// pushSendLimit bounds one claimed push from its claim to its send's
	// end. It is far shorter than store.PushReclaim, so a stalled send has
	// given up before its row can be claimed again by another process.
	pushSendLimit = notify.PublishLimit
	// pushFinishLimit bounds recording a push's result. It runs on a
	// context detached from the flush's cancellation, so a send cut short
	// by shutdown still releases its row.
	pushFinishLimit = 2 * time.Second
	// pushGrace is how long a send in progress may go on once its flush
	// has ended (the process is stopping).
	pushGrace = time.Second
	// pushStopWait is how long mcpServe waits for its pushLoop to end.
	pushStopWait = 3 * time.Second
	// pushMaxAccess bounds CURIA_NTFY_TOKEN, in bytes.
	pushMaxAccess = 512
	// pushMaxReason bounds a failure reason as stored and logged, in runes.
	pushMaxReason = 200
)

// The values a push's stakes line is built from come from session columns
// that another program fills, so only sane ones are used (the line is left
// out otherwise).
const (
	// pushStakesPast and pushStakesFuture bound a cache expiry's distance
	// from now. A prompt cache lives at most an hour.
	pushStakesPast   = 24 * time.Hour
	pushStakesFuture = 2 * time.Hour
	// pushStakesMaxTokens and pushStakesMaxUSD bound the context size and
	// the rebuild cost.
	pushStakesMaxTokens = 10_000_000
	pushStakesMaxUSD    = 1000.0
)

// pushEvery is how often pushLoop flushes without a kick; tests shorten it.
var pushEvery = 60 * time.Second

// pushPublisherFor builds the publisher pushLoop sends with. It is a seam:
// the tests replace it, and their default fails any test that reaches it
// without setting its own, so no test can reach a real server.
var pushPublisherFor = pushPublisher

// pushEnqueue queues one push per configured device for a new blocking ticket or a new review.
// It does nothing, and touches no table, when ntfy is not enabled.
// A push made inside quiet hours is due at their end.
func pushEnqueue(ctx context.Context, st *store.Store, cfg config.Config, kind model.PushKind, ref string, now time.Time) error {
	if !cfg.Ntfy.Enabled() {
		return nil
	}
	_, err := st.EnqueuePushes(ctx, kind, ref, cfg.Ntfy.DeviceNames(), notify.NotBefore(now, cfg.Ntfy.Quiet), now)
	return err
}

// pushPublisher is the publisher for cfg: its URL, CURIA_NTFY_TOKEN when that is usable, no redirects.
// The URL is cfg.Ntfy.URL and nothing else; the default client follows no
// redirect and takes no proxy from the environment.
func pushPublisher(cfg config.Config) notify.Publisher {
	access, _ := pushAccess()
	return notify.Publisher{URL: cfg.Ntfy.URL, AccessToken: access}
}

// pushAccess reads CURIA_NTFY_TOKEN. A value over pushMaxAccess bytes, not
// UTF-8 or holding a control character is not used, and ignored is true.
func pushAccess() (access string, ignored bool) {
	v := os.Getenv("CURIA_NTFY_TOKEN")
	if v == "" {
		return "", false
	}
	if len(v) > pushMaxAccess || !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
		return "", true
	}
	return v, false
}

// pushLoop flushes when kick fires and every pushEvery, until ctx ends. It never returns an error: failures are logged.
// It also flushes once at start, so a push held back while no Curia
// process was alive goes out when the next one starts.
func pushLoop(ctx context.Context, st *store.Store, cfg config.Config, logger *slog.Logger, kick <-chan struct{}) {
	if _, ignored := pushAccess(); ignored {
		logger.Warn("push: CURIA_NTFY_TOKEN ignored: longer than 512 bytes or holds a control character")
	}
	f := pushFlusher{st: st, cfg: cfg, pub: pushPublisherFor(cfg), now: clock, log: logger}
	tick := time.NewTicker(pushEvery)
	defer tick.Stop()
	for {
		if _, err := f.flush(ctx); err != nil && ctx.Err() == nil {
			logger.Warn("push: flush", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-kick:
		case <-tick.C:
		}
	}
}

// pushStart starts pushLoop for a long-lived process when ntfy is enabled,
// on a context that stop cancels; stop then waits at most pushStopWait
// for the loop to end. kick asks the loop to flush, without blocking.
// When ntfy is not enabled no goroutine is started, no publisher is built
// and both functions do nothing.
func pushStart(ctx context.Context, st *store.Store, cfg config.Config, logger *slog.Logger) (kick, stop func()) {
	if !cfg.Ntfy.Enabled() {
		return func() {}, func() {}
	}
	ch := make(chan struct{}, 1)
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pushLoop(lctx, st, cfg, logger, ch)
	}()
	kick = func() {
		select {
		case ch <- struct{}{}:
		default: // a flush is already asked for
		}
	}
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(pushStopWait):
			logger.Warn("push: loop did not stop in time")
		}
	}
	return kick, stop
}

// pushFlusher is one flush's dependencies.
type pushFlusher struct {
	st  *store.Store
	cfg config.Config
	pub notify.Publisher
	now func() time.Time
	log *slog.Logger
}

// flush is pushFlush, logging each push's outcome.
func (f pushFlusher) flush(ctx context.Context) (int, error) {
	sent := 0
	for ctx.Err() == nil {
		due, err := f.st.PushesDue(ctx, f.now())
		if err != nil || !due {
			return sent, err
		}
		ok, found, err := f.one(ctx)
		if ok {
			sent++
		}
		if err != nil || !found {
			return sent, err
		}
	}
	return sent, nil
}

// one claims the oldest due push and sends it (section 4.7, steps 2 to 4).
// found is false when there was nothing to claim. The whole of it is
// bounded by pushSendLimit (and by pushGrace after ctx ends), its
// result's record by pushFinishLimit.
func (f pushFlusher) one(ctx context.Context) (sent, found bool, err error) {
	// The send is not cut off the moment the flush ends: it gets
	// pushGrace more to finish, so a reply already on its way is counted
	// and the push is not sent twice.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushSendLimit)
	defer cancel()
	defer context.AfterFunc(ctx, func() { time.AfterFunc(pushGrace, cancel) })()
	token, hash, err := f.token()
	if err != nil {
		return false, false, err
	}
	p, found, err := f.st.ClaimPush(sctx, hash, f.now())
	if err != nil || !found {
		return false, found, err
	}
	n, why, err := f.build(sctx, p, token)
	if err != nil {
		f.finish(ctx, p, model.PushPending, "store unreadable", f.retryAt(p, nil))
		return false, true, err
	}
	if hash != "" && len(n.Actions) == 0 { // a review, an irreversible ticket: no button holds the token
		if cerr := f.st.ClearPushToken(sctx, p.N); cerr != nil {
			f.logPush(slog.LevelWarn, p, "token hash not cleared", "err", cerr.Error())
		}
	}
	if why != "" {
		f.finish(ctx, p, model.PushDropped, why, time.Time{})
		f.logPush(slog.LevelInfo, p, "dropped", "why", why)
		return false, true, nil
	}
	perr := f.pub.Publish(sctx, n)
	f.result(ctx, p, perr) // ctx, not sctx: only the flush's own end releases a row at once
	return perr == nil, true, nil
}

// token is a new one-time token and its hash when buttons can be built
// ([serve] public_url is set), else "".
func (f pushFlusher) token() (plain, hash string, err error) {
	if f.cfg.Serve.PublicURL == "" {
		return "", "", nil
	}
	plain, hash, err = notify.NewToken(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("push: %w", err)
	}
	return plain, hash, nil
}

// build reads p's subject again and makes its notification. why is not
// empty when the push must be dropped: its subject no longer wants it, or
// its device is no longer configured.
func (f pushFlusher) build(ctx context.Context, p model.Push, token string) (n notify.Notification, why string, err error) {
	topic, ok := f.cfg.Ntfy.Devices[p.Device]
	if !ok {
		return notify.Notification{}, "device no longer configured", nil
	}
	if p.Kind == model.PushReview {
		return f.buildReview(ctx, p, topic)
	}
	d, found, err := f.st.GetDecision(ctx, p.Ref)
	if err != nil {
		return notify.Notification{}, "", err
	}
	if !found || d.Mode != model.ModeBlocking || d.Status != model.StatusOpen ||
		(!d.ExpiresAt.IsZero() && !d.ExpiresAt.After(f.now())) {
		return notify.Notification{}, "ticket no longer open", nil
	}
	answerURL := ""
	if token != "" {
		answerURL = strings.TrimRight(f.cfg.Serve.PublicURL, "/") + "/v1/answer"
	}
	return notify.DecisionPush(d, topic, answerURL, token, f.stakes(ctx, d)), "", nil
}

// stakes is the stakes line for decision d, worked out now from its
// session's stored cache state, or "" when that is unknown, makes no sense,
// or d is not among the open tickets. A failure to read the tickets is
// logged and leaves the line out: the push is not held back for it. It is
// not used for an irreversible ticket's push, which stays as it was.
func (f pushFlusher) stakes(ctx context.Context, d model.Decision) string {
	if d.Reversibility == model.ReversIrreversible {
		return ""
	}
	now := f.now()
	tickets, err := f.st.OpenTickets(ctx, now)
	if err != nil {
		f.log.Warn("push: stakes left out", "ref", d.ID, "err", sanitize.Line(err.Error(), pushMaxReason))
		return ""
	}
	for _, t := range tickets {
		if t.ID == d.ID {
			return pushStakes(t, now)
		}
	}
	return ""
}

// pushStakes is decision.Stakes(t, now) when t's cache values are sane, else "".
func pushStakes(t model.Ticket, now time.Time) string {
	switch {
	case t.CacheExpiresAt.IsZero(),
		t.CacheExpiresAt.Before(now.Add(-pushStakesPast)), t.CacheExpiresAt.After(now.Add(pushStakesFuture)),
		t.ContextUsed < 0, t.ContextUsed > pushStakesMaxTokens,
		math.IsNaN(t.CacheRebuildUSD), t.CacheRebuildUSD < 0, t.CacheRebuildUSD > pushStakesMaxUSD:
		return ""
	}
	return decision.Stakes(t, now)
}

// buildReview is build for a review push.
func (f pushFlusher) buildReview(ctx context.Context, p model.Push, topic string) (notify.Notification, string, error) {
	r, found, err := f.st.GetReview(ctx, p.Ref)
	if err != nil {
		return notify.Notification{}, "", err
	}
	if !found || r.Status != model.ReviewPending {
		return notify.Notification{}, "review no longer pending", nil
	}
	t, found, err := f.st.GetTask(ctx, r.TaskID)
	if err != nil {
		return notify.Notification{}, "", err
	}
	if !found {
		t = model.Task{ID: r.TaskID}
	}
	return notify.ReviewPush(r, t, topic), "", nil
}

// result records a send's outcome (section 4.7, step 4). Only
// ErrUnavailable and ErrRateLimited are retried; the store fails a push
// that has used its last attempt. A send cut short because the flush
// ended is released at once, to be sent by the next flush.
func (f pushFlusher) result(ctx context.Context, p model.Push, err error) {
	switch {
	case err == nil:
		f.finish(ctx, p, model.PushSent, "", time.Time{})
		f.logPush(slog.LevelInfo, p, "sent")
	case ctx.Err() != nil && !errors.Is(err, notify.ErrRejected):
		f.finish(ctx, p, model.PushPending, pushReason(err), notify.NotBefore(f.now(), f.cfg.Ntfy.Quiet))
		f.logPush(slog.LevelInfo, p, "released", "why", pushReason(err))
	case !errors.Is(err, notify.ErrUnavailable) && !errors.Is(err, notify.ErrRateLimited):
		f.finish(ctx, p, model.PushFailed, pushReason(err), time.Time{})
		f.logPush(slog.LevelError, p, "failed, not retried", "why", pushReason(err))
	case p.Attempts >= store.PushMaxAttempts:
		f.finish(ctx, p, model.PushFailed, pushReason(err), time.Time{})
		f.logPush(slog.LevelError, p, "failed, no attempt left", "why", pushReason(err))
	default:
		at := f.retryAt(p, err)
		f.finish(ctx, p, model.PushPending, pushReason(err), at)
		f.logPush(slog.LevelWarn, p, "retry", "why", pushReason(err), "at", at.UTC().Format(time.RFC3339))
	}
}

// retryAt is when p is tried again after err: 30 s doubled for each
// attempt after the first, or a 429's Retry-After when that is later, at
// most pushBackoffMax from now, and never inside quiet hours: a time inside
// them moves to their end, as a new push's does (M3).
func (f pushFlusher) retryAt(p model.Push, err error) time.Time {
	wait := pushBackoffMax
	if p.Attempts >= 1 && p.Attempts <= 6 { // 30 s × 2^5 is past the bound
		wait = min(pushBackoff<<(p.Attempts-1), pushBackoffMax)
	}
	var se *notify.StatusError
	if errors.As(err, &se) && se.RetryAfter > wait {
		wait = min(se.RetryAfter, pushBackoffMax)
	}
	return notify.NotBefore(f.now().Add(wait), f.cfg.Ntfy.Quiet)
}

// finish records p's result on a context detached from ctx's
// cancellation, bounded by pushFinishLimit, so a flush that is ending
// still releases the row it claimed. A failure is logged.
func (f pushFlusher) finish(ctx context.Context, p model.Push, status model.PushStatus, why string, retryAt time.Time) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushFinishLimit)
	defer cancel()
	if err := f.st.FinishPush(fctx, p.N, p.Attempts, status, why, retryAt, f.now()); err != nil {
		f.logPush(slog.LevelWarn, p, "result not recorded", "err", err.Error())
	}
}

// logPush logs one push's outcome. It names the push, never its token,
// topic or text.
func (f pushFlusher) logPush(level slog.Level, p model.Push, outcome string, args ...any) {
	attrs := append([]any{
		"n", p.N, "kind", string(p.Kind), "ref", p.Ref, "device", p.Device,
		"attempt", p.Attempts, "outcome", outcome,
	}, args...)
	f.log.Log(context.Background(), level, "push", attrs...)
}

// pushReason is a send error's text, cleaned and bounded. notify's error
// text names only the error's class, a status code or a kind of transport
// failure, never the URL, a token or the message.
func pushReason(err error) string {
	return sanitize.Line(err.Error(), pushMaxReason)
}

package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// pushHash is the hex SHA-256 of seed, the form a push's token hash takes.
func pushHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// pushIsBadToken reports whether err is ErrBadToken itself: the same
// error, unwrapped, with the same text, whatever the reason.
func pushIsBadToken(err error) bool {
	return errors.Is(err, store.ErrBadToken) && err.Error() == store.ErrBadToken.Error()
}

// pushTicket stores an open blocking ticket.
func pushTicket(t *testing.T, s *store.Store, question string) model.Decision {
	t.Helper()
	return create(t, s, ticket(t, question, model.ModeBlocking, "s1", t0))
}

// pushEnqueue enqueues (kind, ref) for devices due at notBefore and returns how many rows were added.
func pushEnqueue(t *testing.T, s *store.Store, kind model.PushKind, ref string, devices []string, notBefore time.Time) int {
	t.Helper()
	n, err := s.EnqueuePushes(context.Background(), kind, ref, devices, notBefore, t0)
	if err != nil {
		t.Fatalf("EnqueuePushes(%s %s): %v", kind, ref, err)
	}
	return n
}

// pushClaim runs ClaimPush and requires it to succeed.
func pushClaim(t *testing.T, s *store.Store, hash string, now time.Time) (model.Push, bool) {
	t.Helper()
	p, ok, err := s.ClaimPush(context.Background(), hash, now)
	if err != nil {
		t.Fatalf("ClaimPush(%v): %v", now, err)
	}
	return p, ok
}

// pushFinish runs FinishPush and requires it to succeed.
func pushFinish(t *testing.T, s *store.Store, n int64, status model.PushStatus, errText string, retryAt, now time.Time) {
	t.Helper()
	if err := s.FinishPush(context.Background(), n, pushGet(t, s, n).Attempts, status, errText, retryAt, now); err != nil {
		t.Fatalf("FinishPush(%d, %s): %v", n, status, err)
	}
}

// pushGet reads push n.
func pushGet(t *testing.T, s *store.Store, n int64) model.Push {
	t.Helper()
	p, err := s.PushForTest(context.Background(), n)
	if err != nil {
		t.Fatalf("push %d: %v", n, err)
	}
	return p
}

// pushAssert compares two pushes field by field, times with Equal.
func pushAssert(t *testing.T, what string, got, want model.Push) {
	t.Helper()
	same := got.N == want.N && got.Kind == want.Kind && got.Ref == want.Ref && got.Device == want.Device &&
		got.Status == want.Status && got.NotBefore.Equal(want.NotBefore) && got.Attempts == want.Attempts &&
		got.LastError == want.LastError && got.TokenHash == want.TokenHash && got.CreatedAt.Equal(want.CreatedAt) &&
		got.ClaimedAt.Equal(want.ClaimedAt) && got.SentAt.Equal(want.SentAt) && got.UsedAt.Equal(want.UsedAt)
	if !same {
		t.Errorf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

// pushAll reads every push row, by n.
func pushAll(t *testing.T, s *store.Store, db *sql.DB) []model.Push {
	t.Helper()
	var ns []int64
	rows, err := db.Query(`SELECT n FROM pushes ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		ns = append(ns, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	out := make([]model.Push, 0, len(ns))
	for _, n := range ns {
		out = append(out, pushGet(t, s, n))
	}
	return out
}

// pushConn pins one raw connection, so PRAGMA data_version, which is per
// connection, shows whether anything else committed between two reads.
func pushConn(t *testing.T, path string) *sql.Conn {
	t.Helper()
	conn, err := rawDB(t, path).Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func pushDataVersion(t *testing.T, conn *sql.Conn) int64 {
	t.Helper()
	var v int64
	if err := conn.QueryRowContext(context.Background(), `PRAGMA data_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// pushSent enqueues one push of a decision for device, claims it with hash
// at sentAt and records it sent at sentAt; it returns the push.
func pushSent(t *testing.T, s *store.Store, kind model.PushKind, ref, device, hash string, sentAt time.Time) model.Push {
	t.Helper()
	if n := pushEnqueue(t, s, kind, ref, []string{device}, sentAt); n != 1 {
		t.Fatalf("enqueue %s %s %s added %d", kind, ref, device, n)
	}
	p, ok := pushClaim(t, s, hash, sentAt)
	if !ok || p.Ref != ref || p.Device != device {
		t.Fatalf("claim for %s %s = %+v, %v", ref, device, p, ok)
	}
	pushFinish(t, s, p.N, model.PushSent, "", time.Time{}, sentAt)
	return pushGet(t, s, p.N)
}

func TestEnqueueOnePerDeviceOnce(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	db := rawDB(t, path)
	tk := pushTicket(t, s, "Where do the events go?")
	due := t0.Add(time.Minute)
	if n := pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone", "tablet"}, due); n != 2 {
		t.Fatalf("first enqueue added %d rows, want 2", n)
	}
	before := pushAll(t, s, db)
	if n := pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone", "tablet"}, due.Add(time.Hour)); n != 0 {
		t.Errorf("second enqueue added %d rows, want 0", n)
	}
	after := pushAll(t, s, db)
	if len(after) != 2 {
		t.Fatalf("%d rows after the second enqueue, want 2", len(after))
	}
	for i := range before {
		pushAssert(t, "row after a repeated enqueue", after[i], before[i])
	}
	if n := pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone", "laptop", "laptop"}, due); n != 1 {
		t.Errorf("enqueue with a third device added %d rows, want 1", n)
	}
	// The same ref of another kind is another push.
	if n := pushEnqueue(t, s, model.PushReview, tk.ID, []string{"phone"}, due); n != 1 {
		t.Errorf("review push of the same ref added %d rows, want 1", n)
	}
	if n := pushEnqueue(t, s, model.PushDecision, tk.ID, nil, due); n != 0 {
		t.Errorf("enqueue for no device added %d rows, want 0", n)
	}
	for _, p := range pushAll(t, s, db) {
		if p.Status != model.PushPending || p.Attempts != 0 || p.TokenHash != "" || !p.CreatedAt.Equal(t0) {
			t.Errorf("new row %+v, want pending with no attempt and no token, created at t0", p)
		}
	}
}

func TestPushEnqueueRefusesBadInput(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		kind    model.PushKind
		ref     string
		devices []string
		at      time.Time
	}{
		{"unknown kind", "task", "T-1", []string{"phone"}, t0},
		{"empty ref", model.PushDecision, "", []string{"phone"}, t0},
		{"empty device", model.PushDecision, "D-1", []string{"phone", ""}, t0},
		{"no not_before", model.PushDecision, "D-1", []string{"phone"}, time.Time{}},
	} {
		if n, err := s.EnqueuePushes(ctx, c.kind, c.ref, c.devices, c.at, t0); err == nil || n != 0 {
			t.Errorf("%s: EnqueuePushes = %d, %v; want 0 and an error", c.name, n, err)
		}
	}
	if due, err := s.PushesDue(ctx, t0.Add(time.Hour)); err != nil || due {
		t.Errorf("after refused enqueues PushesDue = %v, %v; want false (nothing stored)", due, err)
	}
}

func TestPushRoundTrip(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	tk := pushTicket(t, s, "Which queue?")
	created := t0.Add(1_234_567_891)
	notBefore := t0.Add(2*time.Hour + 987_654_321)
	if n, err := s.EnqueuePushes(ctx, model.PushDecision, tk.ID, []string{"phone"}, notBefore, created); err != nil || n != 1 {
		t.Fatalf("EnqueuePushes = %d, %v", n, err)
	}
	want := model.Push{
		N: 1, Kind: model.PushDecision, Ref: tk.ID, Device: "phone", Status: model.PushPending,
		NotBefore: notBefore, CreatedAt: created,
	}
	pushAssert(t, "enqueued", pushGet(t, s, 1), want)

	claim1 := notBefore.Add(111)
	h1 := pushHash("first")
	p, ok := pushClaim(t, s, h1, claim1)
	want.Status, want.Attempts, want.ClaimedAt, want.TokenHash = model.PushSending, 1, claim1, h1
	if !ok {
		t.Fatal("first claim found nothing")
	}
	pushAssert(t, "first claim", p, want)
	pushAssert(t, "first claim stored", pushGet(t, s, 1), want)

	retryAt := claim1.Add(30*time.Second + 5)
	pushFinish(t, s, 1, model.PushPending, "ntfy is unavailable (503)", retryAt, claim1.Add(time.Second))
	want.Status, want.NotBefore, want.LastError, want.TokenHash = model.PushPending, retryAt, "ntfy is unavailable (503)", ""
	pushAssert(t, "back to pending", pushGet(t, s, 1), want)

	claim2 := retryAt.Add(7)
	h2 := pushHash("second")
	p, ok = pushClaim(t, s, h2, claim2)
	want.Status, want.Attempts, want.ClaimedAt, want.TokenHash = model.PushSending, 2, claim2, h2
	if !ok {
		t.Fatal("second claim found nothing")
	}
	pushAssert(t, "second claim", p, want)

	sentAt := claim2.Add(3)
	pushFinish(t, s, 1, model.PushSent, "", time.Time{}, sentAt)
	want.Status, want.SentAt, want.LastError = model.PushSent, sentAt, ""
	pushAssert(t, "sent", pushGet(t, s, 1), want)

	usedAt := sentAt.Add(9)
	p, err := s.UsePushToken(ctx, h2, tk.ID, usedAt)
	if err != nil {
		t.Fatalf("UsePushToken: %v", err)
	}
	want.UsedAt = usedAt
	pushAssert(t, "spent", p, want)
	pushAssert(t, "spent stored", pushGet(t, s, 1), want)
}

func TestPushesDueRespectsNotBefore(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	conn := pushConn(t, path)
	due := func(at time.Time) bool {
		t.Helper()
		got, err := s.PushesDue(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if due(t0) {
		t.Error("PushesDue on an empty outbox = true")
	}
	tk := pushTicket(t, s, "Which cache?")
	at := t0.Add(time.Hour)
	pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, at)

	v := pushDataVersion(t, conn)
	if due(t0) {
		t.Error("due in 1 h: PushesDue = true, want false")
	}
	if due(at.Add(-1)) {
		t.Error("1 ns before not_before: PushesDue = true, want false")
	}
	if !due(at) {
		t.Error("at not_before: PushesDue = false, want true")
	}
	if got := pushDataVersion(t, conn); got != v {
		t.Errorf("PushesDue wrote: data_version %d -> %d", v, got)
	}

	// A sending row is due again only once its sender may have died.
	p, _ := pushClaim(t, s, "", at)
	if due(at.Add(store.PushReclaim - 1)) {
		t.Error("a row claimed under PushReclaim ago is reported due")
	}
	if !due(at.Add(store.PushReclaim)) {
		t.Error("a row claimed PushReclaim ago is not reported due")
	}
	pushFinish(t, s, p.N, model.PushSent, "", time.Time{}, at)
	if due(at.Add(48 * time.Hour)) {
		t.Error("a sent row is reported due")
	}
	// The check above is real: a write does move data_version.
	if v2 := pushDataVersion(t, conn); v2 == v {
		t.Error("data_version did not move after writes; the PushesDue check proves nothing")
	}
}

func TestClaimPushOldestDueFirst(t *testing.T) {
	s := open(t, dbPath(t))
	a := pushTicket(t, s, "First ticket, due later")
	b := pushTicket(t, s, "Second ticket, due now")
	c := pushTicket(t, s, "Third ticket, due an hour ago")
	pushEnqueue(t, s, model.PushDecision, a.ID, []string{"phone"}, t0.Add(time.Hour))
	pushEnqueue(t, s, model.PushDecision, b.ID, []string{"phone"}, t0)
	pushEnqueue(t, s, model.PushDecision, c.ID, []string{"phone"}, t0.Add(-time.Hour))
	var got []string
	for i := range 4 {
		p, ok := pushClaim(t, s, pushHash(fmt.Sprint(i)), t0)
		if !ok {
			break
		}
		got = append(got, p.Ref)
	}
	if want := []string{b.ID, c.ID}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("claimed %v at t0, want %v (by n, only rows due)", got, want)
	}
	p, ok := pushClaim(t, s, "", t0.Add(time.Hour))
	if !ok || p.Ref != a.ID {
		t.Errorf("claim at its not_before = %+v, %v; want %s", p, ok, a.ID)
	}
}

func TestClaimPushReclaimsDeadSender(t *testing.T) {
	s := open(t, dbPath(t))
	tk := pushTicket(t, s, "Which port?")
	pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, t0)
	old := pushHash("old")
	first, ok := pushClaim(t, s, old, t0)
	if !ok || first.Attempts != 1 {
		t.Fatalf("first claim = %+v, %v", first, ok)
	}
	if p, again := pushClaim(t, s, pushHash("early"), t0.Add(time.Minute)); again {
		t.Fatalf("a row sending for 1 min was claimed again: %+v", p)
	}
	if got := pushGet(t, s, first.N); got.TokenHash != old || got.Attempts != 1 {
		t.Errorf("a refused reclaim changed the row: %+v", got)
	}
	fresh := pushHash("fresh")
	at := t0.Add(3 * time.Minute)
	p, ok := pushClaim(t, s, fresh, at)
	if !ok || p.N != first.N || p.Attempts != 2 || p.TokenHash != fresh || !p.ClaimedAt.Equal(at) || p.Status != model.PushSending {
		t.Fatalf("reclaim after 3 min = %+v, %v; want row %d, attempts 2, the new hash, claimed at +3m", p, ok, first.N)
	}
	// The dead sender's token can never be spent, even if it reports back.
	pushFinish(t, s, p.N, model.PushSent, "", time.Time{}, at)
	if _, err := s.UsePushToken(context.Background(), old, tk.ID, at); !errors.Is(err, store.ErrBadToken) {
		t.Errorf("the replaced token was accepted: %v", err)
	}
	if _, err := s.UsePushToken(context.Background(), fresh, tk.ID, at); err != nil {
		t.Errorf("the new token was refused: %v", err)
	}
}

// pushDrain claims pushes at t0 until none is left and returns their ns.
func pushDrain(s *store.Store, g int) ([]int64, error) {
	var claimed []int64
	for i := 0; ; i++ {
		var p model.Push
		var ok bool
		// A transaction: SQLITE_BUSY at its BEGIN means it changed nothing.
		err := retryBusy(func() error {
			var err error
			p, ok, err = s.ClaimPush(context.Background(), pushHash(fmt.Sprintf("%d-%d", g, i)), t0)
			return err
		}, busyAtBegin)
		if err != nil || !ok {
			return claimed, err
		}
		claimed = append(claimed, p.N)
	}
}

func TestPushClaimedExactlyOnce(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	tk := pushTicket(t, hs[0], "Which region?")
	var devices []string
	for i := range 10 {
		devices = append(devices, fmt.Sprintf("device%d", i))
	}
	pushEnqueue(t, hs[0], model.PushDecision, tk.ID, devices, t0)
	const goroutines = 20
	claimed := make([][]int64, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := range goroutines {
		wg.Go(func() {
			<-start
			claimed[g], errs[g] = pushDrain(hs[g%len(hs)], g)
		})
	}
	close(start)
	wg.Wait()
	seen := map[int64]int{}
	for g := range goroutines {
		if errs[g] != nil {
			t.Fatalf("goroutine %d: %v", g, errs[g])
		}
		for _, n := range claimed[g] {
			seen[n]++
		}
	}
	if len(seen) != 10 {
		t.Errorf("%d distinct rows claimed, want 10", len(seen))
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("row %d claimed %d times, want once", n, c)
		}
	}
	for _, p := range pushAll(t, hs[0], rawDB(t, path)) {
		if p.Status != model.PushSending || p.Attempts != 1 {
			t.Errorf("row %d: %s with %d attempts, want sending with 1", p.N, p.Status, p.Attempts)
		}
	}
}

func TestFinishPushTable(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	retryAt := t0.Add(5 * time.Minute)
	now := t0.Add(time.Second)
	for i, c := range []struct {
		status            model.PushStatus
		keepsHash, isSent bool
	}{
		{model.PushSent, true, true},
		{model.PushDropped, false, false},
		{model.PushFailed, false, false},
		{model.PushPending, false, false},
	} {
		tk := pushTicket(t, s, fmt.Sprintf("Finish table question %d", i))
		pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, t0)
		h := pushHash(fmt.Sprint("finish", i))
		p, ok := pushClaim(t, s, h, t0)
		if !ok || p.Ref != tk.ID {
			t.Fatalf("%s: claim = %+v, %v", c.status, p, ok)
		}
		pushFinish(t, s, p.N, c.status, "boom", retryAt, now)
		got := pushGet(t, s, p.N)
		want := p
		want.Status, want.LastError = c.status, "boom"
		if !c.keepsHash {
			want.TokenHash = ""
		}
		if c.isSent {
			want.SentAt = now
		}
		if c.status == model.PushPending {
			want.NotBefore = retryAt
		}
		pushAssert(t, string(c.status), got, want)

		// The row is no longer sending: nothing changes it now.
		for _, again := range []model.PushStatus{model.PushSent, model.PushFailed, model.PushPending, model.PushDropped} {
			pushFinish(t, s, p.N, again, "later", t0.Add(time.Hour), t0.Add(time.Hour))
		}
		pushAssert(t, string(c.status)+" after more finishes", pushGet(t, s, p.N), got)
	}

	// Bad calls are refused and change nothing.
	tk := pushTicket(t, s, "Finish refusals")
	pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, t0)
	p, _ := pushClaim(t, s, pushHash("refusals"), t0)
	for _, c := range []struct {
		status  model.PushStatus
		retryAt time.Time
	}{
		{model.PushSending, retryAt},
		{"", retryAt},
		{"queued", retryAt},
		{model.PushPending, time.Time{}},
	} {
		if err := s.FinishPush(ctx, p.N, p.Attempts, c.status, "x", c.retryAt, now); err == nil {
			t.Errorf("FinishPush(%q, retry %v) = nil, want an error", c.status, c.retryAt)
		}
	}
	pushAssert(t, "after refused finishes", pushGet(t, s, p.N), p)
	if err := s.FinishPush(ctx, 999, 1, model.PushSent, "", time.Time{}, now); err != nil {
		t.Errorf("FinishPush of an unknown row = %v, want nil (nothing to change)", err)
	}
}

func TestFinishPushCutsErrorText(t *testing.T) {
	s := open(t, dbPath(t))
	for i, c := range []struct {
		text string
		want string
	}{
		{strings.Repeat("a", 300), strings.Repeat("a", 200)},
		{strings.Repeat("b", 200), strings.Repeat("b", 200)},
		{"short", "short"},
		// A rune that would straddle byte 200 is left out whole.
		{strings.Repeat("c", 199) + "€€", strings.Repeat("c", 199)},
		{strings.Repeat("€", 100), strings.Repeat("€", 66)},
	} {
		tk := pushTicket(t, s, fmt.Sprintf("Cut question %d", i))
		pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, t0)
		p, _ := pushClaim(t, s, "", t0)
		pushFinish(t, s, p.N, model.PushFailed, c.text, time.Time{}, t0)
		got := pushGet(t, s, p.N).LastError
		if got != c.want || len(got) > 200 || !utf8.ValidString(got) {
			t.Errorf("case %d: last_error %q (%d bytes), want %q", i, got, len(got), c.want)
		}
	}
}

func TestPushRetriesAreBounded(t *testing.T) {
	s := open(t, dbPath(t))
	tk := pushTicket(t, s, "Retry bound")
	pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, t0)
	at := t0
	for attempt := 1; attempt <= store.PushMaxAttempts; attempt++ {
		p, ok := pushClaim(t, s, pushHash(fmt.Sprint("retry", attempt)), at)
		if !ok || p.Attempts != attempt {
			t.Fatalf("claim %d = %+v, %v", attempt, p, ok)
		}
		pushFinish(t, s, p.N, model.PushPending, "unavailable", at, at)
		got := pushGet(t, s, p.N)
		if attempt < store.PushMaxAttempts && got.Status != model.PushPending {
			t.Fatalf("after attempt %d: %s, want pending", attempt, got.Status)
		}
		if attempt == store.PushMaxAttempts && got.Status != model.PushFailed {
			t.Errorf("a retry asked after attempt %d stored %s, want failed", attempt, got.Status)
		}
		at = at.Add(time.Minute)
	}
	if p, ok := pushClaim(t, s, "", at.Add(time.Hour)); ok {
		t.Errorf("a row past PushMaxAttempts was claimed: %+v", p)
	}
}

// A sender that dies on the last attempt does not earn a sixth.
func TestPushDeadLastAttemptFails(t *testing.T) {
	s := open(t, dbPath(t))
	at := t0
	tk2 := pushTicket(t, s, "Dead on the last attempt")
	pushEnqueue(t, s, model.PushDecision, tk2.ID, []string{"phone"}, at)
	var last model.Push
	for attempt := 1; attempt <= store.PushMaxAttempts; attempt++ {
		var ok bool
		last, ok = pushClaim(t, s, "", at)
		if !ok {
			t.Fatalf("claim %d of the second row found nothing", attempt)
		}
		if attempt < store.PushMaxAttempts {
			pushFinish(t, s, last.N, model.PushPending, "unavailable", at, at)
		}
	}
	if p, ok := pushClaim(t, s, "", at.Add(time.Hour)); ok {
		t.Errorf("a dead sender's last attempt was reclaimed: %+v", p)
	}
	if got := pushGet(t, s, last.N); got.Status != model.PushFailed || got.Attempts != store.PushMaxAttempts || got.TokenHash != "" {
		t.Errorf("dead last attempt: %+v, want failed with %d attempts and no token", got, store.PushMaxAttempts)
	}
	if due, err := s.PushesDue(context.Background(), at.Add(2*time.Hour)); err != nil || due {
		t.Errorf("PushesDue after both rows failed = %v, %v; want false", due, err)
	}
}

// pushErr keeps the error of a call that also returns a decision.
func pushErr(_ model.Decision, err error) error { return err }

// pushReviewErr keeps the error of CloseReview.
func pushReviewErr(_ model.Review, _ int, err error) error { return err }

func TestPushClaimDropsClosedSubjects(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	open1 := pushTicket(t, s, "Still open")
	answered := pushTicket(t, s, "Answered at the desk")
	withdrawn := pushTicket(t, s, "Withdrawn by the agent")
	assumed := create(t, s, ticket(t, "An assume ticket", model.ModeAssume, "s1", t0))
	retried := pushTicket(t, s, "Answered between two attempts")
	pending := reviewFor(t, s, "Pending review")
	dropped := reviewFor(t, s, "Dropped review")

	refs := []struct {
		kind model.PushKind
		ref  string
	}{
		{model.PushDecision, retried.ID}, // first by n, so it is claimed once before it is answered
		{model.PushDecision, open1.ID},
		{model.PushDecision, answered.ID},
		{model.PushDecision, withdrawn.ID},
		{model.PushDecision, assumed.ID},
		{model.PushDecision, "D-999"},
		{model.PushReview, pending.ID},
		{model.PushReview, dropped.ID},
		{model.PushReview, "R-99"},
	}
	for _, r := range refs {
		pushEnqueue(t, s, r.kind, r.ref, []string{"phone"}, t0)
	}
	first, ok := pushClaim(t, s, pushHash("retried"), t0)
	if !ok || first.Ref != retried.ID {
		t.Fatalf("first claim = %+v, %v; want %s", first, ok, retried.ID)
	}
	pushFinish(t, s, first.N, model.PushPending, "unavailable", t0, t0)

	for _, err := range []error{
		pushErr(s.AnswerDecision(ctx, answered.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, t0)),
		pushErr(s.AnswerDecision(ctx, retried.ID, model.Answer{Key: "B", Via: model.AnsweredCLI}, t0)),
		pushErr(s.WithdrawDecision(ctx, withdrawn.ID, "s1", "no longer needed", "", t0)),
		pushReviewErr(s.CloseReview(ctx, dropped.ID, store.ReviewClose{Status: model.ReviewDropped, Via: model.AnsweredCLI}, t0)),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}

	claimed, err := pushDrain(s, 0)
	if err != nil {
		t.Fatal(err)
	}
	var refs2 []string
	for _, n := range claimed {
		refs2 = append(refs2, pushGet(t, s, n).Ref)
	}
	if want := []string{open1.ID, pending.ID}; strings.Join(refs2, ",") != strings.Join(want, ",") {
		t.Errorf("claimed %v, want only the open ticket and the pending review %v", refs2, want)
	}
	for _, p := range pushAll(t, s, rawDB(t, path)) {
		want := model.PushDropped
		if p.Ref == open1.ID || p.Ref == pending.ID {
			want = model.PushSending
		}
		if p.Status != want || (want == model.PushDropped && p.TokenHash != "") {
			t.Errorf("%s: %s with token %q, want %s (a dropped row without a token)", p.Ref, p.Status, p.TokenHash, want)
		}
	}
}

func TestPushClaimRefusesMalformedHash(t *testing.T) {
	s := open(t, dbPath(t))
	tk := pushTicket(t, s, "Hash format")
	pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, t0)
	for _, h := range []string{"abc", strings.ToUpper(pushHash("x")), pushHash("x") + "0", "the raw token itself, not a hash at all!!!"} {
		if p, ok, err := s.ClaimPush(context.Background(), h, t0); err == nil || ok {
			t.Errorf("ClaimPush(%q) = %+v, %v, %v; want an error", h, p, ok, err)
		}
	}
	if p := pushGet(t, s, 1); p.Status != model.PushPending || p.Attempts != 0 {
		t.Errorf("refused claims changed the row: %+v", p)
	}
}

func TestUsePushTokenOnce(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	tk := pushTicket(t, s, "Spend once")
	h := pushHash("once")
	sentAt := t0.Add(time.Minute)
	sent := pushSent(t, s, model.PushDecision, tk.ID, "phone", h, sentAt)
	// Another device's push of the same ticket has its own token.
	other := pushSent(t, s, model.PushDecision, tk.ID, "tablet", pushHash("tablet"), sentAt)

	usedAt := sentAt.Add(store.PushTokenTTL - 1)
	p, err := s.UsePushToken(ctx, h, tk.ID, usedAt)
	if err != nil {
		t.Fatalf("UsePushToken just inside the TTL: %v", err)
	}
	want := sent
	want.UsedAt = usedAt
	pushAssert(t, "spent push", p, want)
	pushAssert(t, "stored", pushGet(t, s, sent.N), want)
	pushAssert(t, "the other device's push", pushGet(t, s, other.N), other)

	if _, err := s.UsePushToken(ctx, h, tk.ID, usedAt.Add(-time.Minute)); !pushIsBadToken(err) {
		t.Errorf("second spend = %v, want ErrBadToken", err)
	}
	pushAssert(t, "after a second spend", pushGet(t, s, sent.N), want)
}

func TestUsePushTokenRefusals(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	ctx := context.Background()
	db := rawDB(t, path)
	conn := pushConn(t, path)
	now := t0.Add(48 * time.Hour)

	tk := pushTicket(t, s, "Refusals ticket")
	other := pushTicket(t, s, "Another ticket")
	good := pushHash("good")
	pushSent(t, s, model.PushDecision, tk.ID, "phone", good, now.Add(-time.Hour))
	pushSent(t, s, model.PushDecision, other.ID, "phone", pushHash("other"), now.Add(-time.Hour))
	rv := reviewFor(t, s, "A review")
	reviewHash := pushHash("review")
	pushSent(t, s, model.PushReview, rv.ID, "phone", reviewHash, now.Add(-time.Hour))

	sending := pushHash("sending")
	failed := pushHash("failed")
	dropped := pushHash("dropped")
	old := pushHash("old")
	edge := pushHash("edge")
	for _, c := range []struct {
		device, hash string
		finish       model.PushStatus
		at           time.Time
	}{
		{"sending", sending, "", now.Add(-time.Minute)},
		{"failed", failed, model.PushFailed, now.Add(-time.Minute)},
		{"dropped", dropped, model.PushDropped, now.Add(-time.Minute)},
		{"old", old, model.PushSent, now.Add(-25 * time.Hour)},
		{"edge", edge, model.PushSent, now.Add(-store.PushTokenTTL)},
	} {
		pushEnqueue(t, s, model.PushDecision, tk.ID, []string{c.device}, c.at)
		p, ok := pushClaim(t, s, c.hash, c.at)
		if !ok || p.Device != c.device {
			t.Fatalf("claim for %s = %+v, %v", c.device, p, ok)
		}
		if c.finish != "" {
			pushFinish(t, s, p.N, c.finish, "", time.Time{}, c.at)
		}
	}

	before := pushAll(t, s, db)
	v := pushDataVersion(t, conn)
	var texts []string
	for _, c := range []struct {
		name, hash, decision string
	}{
		{"empty hash", "", tk.ID},
		{"malformed hash", "abc", tk.ID},
		{"upper-case hash", strings.ToUpper(good), tk.ID},
		{"unknown hash", pushHash("unknown"), tk.ID},
		{"another decision id", good, other.ID},
		{"an unknown decision id", good, "D-999"},
		{"an empty decision id", good, ""},
		{"a review push", reviewHash, rv.ID},
		{"a review push for the ticket", reviewHash, tk.ID},
		{"still sending", sending, tk.ID},
		{"failed", failed, tk.ID},
		{"dropped", dropped, tk.ID},
		{"sent 25 h ago", old, tk.ID},
		{"sent exactly PushTokenTTL ago", edge, tk.ID},
	} {
		p, err := s.UsePushToken(ctx, c.hash, c.decision, now)
		if !pushIsBadToken(err) {
			t.Errorf("%s: UsePushToken = %+v, %v; want ErrBadToken itself", c.name, p, err)
		}
		if err != nil {
			texts = append(texts, err.Error())
		}
		if p != (model.Push{}) {
			t.Errorf("%s: a refusal returned a push: %+v", c.name, p)
		}
	}
	for _, txt := range texts {
		if txt != store.ErrBadToken.Error() {
			t.Errorf("refusal text %q differs from ErrBadToken's", txt)
		}
	}
	if got := pushDataVersion(t, conn); got != v {
		t.Errorf("refusals wrote: data_version %d -> %d", v, got)
	}
	after := pushAll(t, s, db)
	for i := range before {
		pushAssert(t, "row after refusals", after[i], before[i])
	}
	// The good token still works after all that.
	if _, err := s.UsePushToken(ctx, good, tk.ID, now); err != nil {
		t.Errorf("the good token after the refusals: %v", err)
	}
}

func TestUsePushTokenRace(t *testing.T) {
	path := dbPath(t)
	hs := handles(t, path, 5)
	ctx := context.Background()
	for round := range 20 {
		tk := pushTicket(t, hs[0], fmt.Sprintf("Race ticket %d", round))
		h := pushHash(fmt.Sprint("race", round))
		pushSent(t, hs[0], model.PushDecision, tk.ID, "phone", h, t0)
		const goroutines = 20
		errs := make([]error, goroutines)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := range goroutines {
			wg.Go(func() {
				<-start
				// Single statements: SQLITE_BUSY means it wrote nothing.
				errs[g] = retryBusy(func() error {
					_, err := hs[g%len(hs)].UsePushToken(ctx, h, tk.ID, t0.Add(time.Minute))
					return err
				}, isBusy)
			})
		}
		close(start)
		wg.Wait()
		won := 0
		for g, err := range errs {
			switch {
			case err == nil:
				won++
			case !pushIsBadToken(err):
				t.Fatalf("round %d goroutine %d: %v", round, g, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d spends of one token succeeded, want exactly 1", round, won)
		}
	}
}

func TestPushQueriesUseIndexes(t *testing.T) {
	path := dbPath(t)
	open(t, path)
	db := rawDB(t, path)
	ts := "2026-11-14T09:00:00.000000000Z"
	for _, c := range []struct {
		name, query string
		args        []any
		want        []string
	}{
		{"PushesDue", store.PushesDueQuery, []any{ts, ts}, []string{"USING INDEX pushes_due (not_before<?)"}},
		{"ClaimPush's pick", store.ClaimPushPickQuery, []any{ts, ts, store.PushMaxAttempts, ts}, []string{
			"SEARCH p USING INDEX pushes_due (not_before<?)",
			"SEARCH d USING INDEX sqlite_autoindex_decisions_1 (id=?)",
			"SEARCH r USING INDEX sqlite_autoindex_reviews_1 (id=?)",
		}},
		{"ClaimPush's settling", store.SettlePushesQuery, []any{ts, ts, store.PushMaxAttempts, ts, 64}, []string{
			"SEARCH p USING INDEX pushes_due (not_before<?)",
			"SEARCH d USING INDEX sqlite_autoindex_decisions_1 (id=?)",
			"SEARCH r USING INDEX sqlite_autoindex_reviews_1 (id=?)",
		}},
		{"UsePushToken's candidates", store.SpendablePushesQuery, []any{"D-1", ts, 8}, []string{
			"SEARCH pushes USING INDEX pushes_once (kind=? AND ref=?)",
		}},
	} {
		plan := queryPlan(t, db, c.query, c.args...)
		t.Logf("%s:\n%s", c.name, plan)
		for _, w := range c.want {
			if !strings.Contains(plan, w) {
				t.Errorf("%s: plan lacks %q:\n%s", c.name, w, plan)
			}
		}
		for _, line := range strings.Split(plan, "\n") {
			if line == "SCAN p" || strings.HasPrefix(line, "SCAN p ") || strings.HasPrefix(line, "SCAN pushes") {
				t.Errorf("%s scans pushes:\n%s", c.name, plan)
			}
		}
	}
}

// pushRawRows inserts count pushes straight into the table: refs of kind
// decision for ref, on devices <prefix>0.., with the given status,
// attempts and claim time.
func pushRawRows(t *testing.T, db *sql.DB, ref, prefix string, count int, status model.PushStatus, attempts int, claimed time.Time) {
	t.Helper()
	ts := func(at time.Time) any {
		if at.IsZero() {
			return nil
		}
		return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	for i := range count {
		if _, err := db.Exec(`INSERT INTO pushes (kind, ref, device, status, not_before, attempts, created_at, claimed_at)
VALUES ('decision', ?, ?, ?, ?, ?, ?, ?)`, ref, fmt.Sprintf("%s%d", prefix, i), status, ts(t0), attempts, ts(t0), ts(claimed)); err != nil {
			t.Fatal(err)
		}
	}
}

// More rows to settle than one ClaimPush settles: the claim itself still
// never takes a closed subject or a row out of attempts.
func TestPushClaimPastSettleBatch(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	db := rawDB(t, path)
	ctx := context.Background()
	closed := pushTicket(t, s, "Answered, with many devices")
	if _, err := s.AnswerDecision(ctx, closed.ID, model.Answer{Key: "A", Via: model.AnsweredCLI}, t0); err != nil {
		t.Fatal(err)
	}
	exhausted := pushTicket(t, s, "Open, but its senders died")
	live := pushTicket(t, s, "Open and fresh")
	const many = 100 // over the store's settle batch
	pushRawRows(t, db, closed.ID, "closed", many, model.PushPending, 0, time.Time{})
	pushRawRows(t, db, exhausted.ID, "dead", many, model.PushSending, store.PushMaxAttempts, t0.Add(-time.Hour))
	pushEnqueue(t, s, model.PushDecision, live.ID, []string{"phone"}, t0)

	p, ok := pushClaim(t, s, "", t0)
	if !ok || p.Ref != live.ID || p.Attempts != 1 {
		t.Fatalf("first claim = %+v, %v; want the fresh push of %s", p, ok, live.ID)
	}
	pushFinish(t, s, p.N, model.PushSent, "", time.Time{}, t0)
	for i := range 10 {
		if p, ok := pushClaim(t, s, "", t0.Add(time.Duration(i)*time.Second)); ok {
			t.Fatalf("claim %d took %+v, want nothing", i, p)
		}
	}
	for _, p := range pushAll(t, s, db) {
		switch p.Ref {
		case closed.ID:
			if p.Status != model.PushDropped {
				t.Errorf("closed-subject row %d: %s, want dropped", p.N, p.Status)
			}
		case exhausted.ID:
			if p.Status != model.PushFailed || p.Attempts != store.PushMaxAttempts {
				t.Errorf("exhausted row %d: %s with %d attempts, want failed with %d", p.N, p.Status, p.Attempts, store.PushMaxAttempts)
			}
		}
	}
	if due, err := s.PushesDue(ctx, t0.Add(time.Hour)); err != nil || due {
		t.Errorf("PushesDue after settling = %v, %v; want false", due, err)
	}
}

// TestFinishIgnoresReclaimedRow: a sender that stalled past PushReclaim and
// reports late cannot finish the row another sender claimed again since
// (M5); the new claim's own finish works.
func TestFinishIgnoresReclaimedRow(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	tk := pushTicket(t, s, "Which port?")
	pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone"}, t0)
	first, ok := pushClaim(t, s, pushHash("old"), t0)
	if !ok {
		t.Fatal("no claim")
	}
	at := t0.Add(3 * time.Minute)
	second, ok := pushClaim(t, s, pushHash("fresh"), at)
	if !ok || second.Attempts != 2 {
		t.Fatalf("reclaim = %+v, %v", second, ok)
	}
	// The stalled first sender reports its send now.
	if err := s.FinishPush(ctx, first.N, first.Attempts, model.PushSent, "", time.Time{}, at); err != nil {
		t.Fatal(err)
	}
	if got := pushGet(t, s, first.N); got.Status != model.PushSending || got.Attempts != 2 || !got.SentAt.IsZero() {
		t.Errorf("the stalled sender finished the reclaimed row: %+v", got)
	}
	if err := s.FinishPush(ctx, second.N, second.Attempts, model.PushSent, "", time.Time{}, at); err != nil {
		t.Fatal(err)
	}
	if got := pushGet(t, s, first.N); got.Status != model.PushSent || got.TokenHash != pushHash("fresh") {
		t.Errorf("the second sender's finish = %+v, want sent with its token", got)
	}
}

// TestClearPushToken: the hash of a push being sent is cleared; a row that
// is not sending is left alone.
func TestClearPushToken(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	tk := pushTicket(t, s, "Which port?")
	pushEnqueue(t, s, model.PushDecision, tk.ID, []string{"phone", "tablet"}, t0)
	p, _ := pushClaim(t, s, pushHash("a"), t0)
	if err := s.ClearPushToken(ctx, p.N); err != nil {
		t.Fatal(err)
	}
	if got := pushGet(t, s, p.N); got.TokenHash != "" || got.Status != model.PushSending {
		t.Errorf("after clear: %+v", got)
	}
	q, _ := pushClaim(t, s, pushHash("b"), t0)
	pushFinish(t, s, q.N, model.PushSent, "", time.Time{}, t0)
	if err := s.ClearPushToken(ctx, q.N); err != nil {
		t.Fatal(err)
	}
	if got := pushGet(t, s, q.N); got.TokenHash != pushHash("b") {
		t.Errorf("a sent push lost its hash: %+v", got)
	}
}

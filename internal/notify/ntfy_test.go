package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
)

// Every server in this file is an httptest server on loopback: no test here
// opens a connection to any other host. The answer URL below is only a string
// placed in the body; nothing connects to it.
const (
	ntfyTopic     = "phone-7Hq2"
	ntfyAnswerURL = "https://curia.example.ts.net/v1/answer"
	ntfyAccess    = "tk_accessSECRETaccessSECRET"
)

// ntfyToken is a one-time token of the real shape (43 base64url characters).
var ntfyToken = strings.Repeat("Q", 40) + "x_-"

func ntfyTicket(n int) model.Decision {
	opts := []model.Option{
		{Key: "A", Label: "Postgres LISTEN & NOTIFY"},
		{Key: "B", Label: "Redis stream"},
		{Key: "C", Label: "Kafka"},
		{Key: "D", Label: "NATS"},
	}
	return model.Decision{
		ID: "D-117", Repo: "/src/api-server", Question: "Which queue should carry job events?",
		Options: opts[:n], Recommendation: "B", Reversibility: model.ReversCheap, Mode: model.ModeBlocking,
		Context: "context SECRET-CONTEXT", Rationale: "rationale SECRET-RATIONALE",
	}
}

func TestNotBeforeTable(t *testing.T) {
	loc := time.FixedZone("test", -5*3600)
	at := func(d, h, m, s int) time.Time { return time.Date(2026, 11, d, h, m, s, 0, loc) }
	cases := []struct {
		quiet string
		now   time.Time
		want  time.Time
	}{
		{"22:00-07:00", at(14, 21, 59, 59), at(14, 21, 59, 59)},
		{"22:00-07:00", at(14, 22, 0, 0), at(15, 7, 0, 0)},
		{"22:00-07:00", at(14, 23, 59, 59), at(15, 7, 0, 0)},
		{"22:00-07:00", at(15, 0, 0, 0), at(15, 7, 0, 0)},
		{"22:00-07:00", at(15, 3, 30, 0), at(15, 7, 0, 0)},
		{"22:00-07:00", at(15, 6, 59, 59), at(15, 7, 0, 0)},
		{"22:00-07:00", at(15, 7, 0, 0), at(15, 7, 0, 0)},
		{"22:00-07:00", at(15, 12, 0, 0), at(15, 12, 0, 0)},
		{"09:00-17:00", at(14, 8, 59, 0), at(14, 8, 59, 0)},
		{"09:00-17:00", at(14, 9, 0, 0), at(14, 17, 0, 0)},
		{"09:00-17:00", at(14, 12, 0, 0), at(14, 17, 0, 0)},
		{"09:00-17:00", at(14, 16, 59, 59), at(14, 17, 0, 0)},
		{"09:00-17:00", at(14, 17, 0, 0), at(14, 17, 0, 0)},
		{"", at(14, 23, 0, 0), at(14, 23, 0, 0)},
		{"junk", at(14, 23, 0, 0), at(14, 23, 0, 0)},
		{"22:00-22:00", at(14, 22, 0, 0), at(14, 22, 0, 0)},
		// the end of the month and of the year roll over
		{"22:00-07:00", time.Date(2026, 12, 31, 23, 0, 0, 0, loc), time.Date(2027, 1, 1, 7, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		got := NotBefore(c.now, c.quiet)
		if !got.Equal(c.want) {
			t.Errorf("NotBefore(%s, %q) = %s, want %s", c.now.Format(time.DateTime), c.quiet, got.Format(time.DateTime), c.want.Format(time.DateTime))
		}
		if got.Location() != loc {
			t.Errorf("NotBefore(%s, %q) is in %v, want now's location", c.now, c.quiet, got.Location())
		}
	}
}

func TestDecisionPushWording(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	if n.Topic != ntfyTopic {
		t.Errorf("topic = %q", n.Topic)
	}
	if n.Title != "D-117 api-server: decision needed" {
		t.Errorf("title = %q", n.Title)
	}
	want := "Which queue should carry job events?\nA: Postgres LISTEN & NOTIFY\nB: Redis stream\nC: Kafka\nRecommended: B"
	if n.Message != want {
		t.Errorf("message = %q, want %q", n.Message, want)
	}
	if n.Priority != 4 {
		t.Errorf("priority = %d", n.Priority)
	}
	if strings.Contains(n.Message, "SECRET") {
		t.Errorf("message holds the ticket's context or rationale: %q", n.Message)
	}
	d := ntfyTicket(3)
	d.Repo = ""
	if got := DecisionPush(d, ntfyTopic, "", "", "").Title; got != "D-117: decision needed" {
		t.Errorf("title without repo = %q", got)
	}
}

func TestDecisionPushButtons(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	if len(n.Actions) != 3 {
		t.Fatalf("actions = %+v", n.Actions)
	}
	wantLabels := []string{"B: Redis stream", "A: Postgres LISTEN & NOTIFY", "C: Kafka"}
	for i, a := range n.Actions {
		if a.Label != wantLabels[i] {
			t.Errorf("action %d label = %q, want %q", i, a.Label, wantLabels[i])
		}
		key := wantLabels[i][:1]
		if want := `{"decision":"D-117","key":"` + key + `"}`; a.Body != want {
			t.Errorf("action %d body = %q, want %q", i, a.Body, want)
		}
		if a.Token != ntfyToken {
			t.Errorf("action %d token = %q", i, a.Token)
		}
		if a.URL != ntfyAnswerURL {
			t.Errorf("action %d url = %q", i, a.URL)
		}
	}
	// A recommendation that names no option: ticket order.
	d := ntfyTicket(3)
	d.Recommendation = "Z"
	n = DecisionPush(d, ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	if got := []string{n.Actions[0].Label[:1], n.Actions[1].Label[:1], n.Actions[2].Label[:1]}; !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Errorf("order without a matching recommendation = %v", got)
	}
}

func TestDecisionPushFourOptions(t *testing.T) {
	n := DecisionPush(ntfyTicket(4), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	if len(n.Actions) != MaxActions {
		t.Fatalf("actions = %d, want %d", len(n.Actions), MaxActions)
	}
	if got := []string{n.Actions[0].Label, n.Actions[1].Label, n.Actions[2].Label}; !reflect.DeepEqual(got, []string{"B: Redis stream", "A: Postgres LISTEN & NOTIFY", "C: Kafka"}) {
		t.Errorf("labels = %v", got)
	}
	want := "Which queue should carry job events?\nA: Postgres LISTEN & NOTIFY\nB: Redis stream\nC: Kafka\nD: NATS\nRecommended: B\nMore options: curia decide D-117."
	if n.Message != want {
		t.Errorf("message = %q, want %q", n.Message, want)
	}
}

func TestDecisionPushIrreversibleHasNoButtons(t *testing.T) {
	for _, opts := range []int{3, 4} {
		d := ntfyTicket(opts)
		d.Reversibility = model.ReversIrreversible
		n := DecisionPush(d, ntfyTopic, ntfyAnswerURL, ntfyToken, "")
		if len(n.Actions) != 0 {
			t.Errorf("%d options: irreversible ticket has actions %+v", opts, n.Actions)
		}
		if !strings.HasSuffix(n.Message, "\nRecommended: B\nIrreversible: answer with curia decide D-117.") {
			t.Errorf("%d options: message = %q", opts, n.Message)
		}
		if strings.Contains(n.Message, "More options") {
			t.Errorf("%d options: irreversible message also says more options: %q", opts, n.Message)
		}
	}
}

func TestDecisionPushWithoutServe(t *testing.T) {
	for _, opts := range []int{3, 4} {
		with := DecisionPush(ntfyTicket(opts), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
		for _, c := range []struct{ url, token string }{{"", ntfyToken}, {ntfyAnswerURL, ""}, {"", ""}} {
			n := DecisionPush(ntfyTicket(opts), ntfyTopic, c.url, c.token, "")
			if len(n.Actions) != 0 {
				t.Errorf("url %q token %q: actions %+v", c.url, c.token, n.Actions)
			}
			if n.Message != with.Message || n.Title != with.Title || n.Priority != with.Priority {
				t.Errorf("url %q token %q: push differs from the one with buttons:\n%+v\n%+v", c.url, c.token, n, with)
			}
		}
	}
}

func ntfyAssertClean(t *testing.T, what, s string, limit int, multiline bool) {
	t.Helper()
	if n := utf8.RuneCountInString(s); n > limit {
		t.Errorf("%s has %d runes, limit %d", what, n, limit)
	}
	for _, r := range s {
		if r == '\n' && multiline {
			continue
		}
		if unicode.IsControl(r) || !unicode.IsPrint(r) && r != ' ' {
			t.Errorf("%s holds rune %U: %q", what, r, s)
			return
		}
	}
}

func TestPushTextIsClean(t *testing.T) {
	dirty := "\x1b[31mred\x1b[0m \a bell \u202e reversed \x1b]8;;http://x\a link " + strings.Repeat("long ", 1000)
	d := model.Decision{
		ID: "D-9", Repo: "/src/" + dirty, Question: dirty + "\nRecommended: Z",
		Options: []model.Option{
			{Key: "A", Label: dirty + "\nRecommended: A"},
			{Key: "B", Label: "\x1b[2J" + dirty},
			{Key: "C", Label: "plain"},
			{Key: "D", Label: dirty},
		},
		Recommendation: "A", Mode: model.ModeBlocking, Reversibility: model.ReversCostly,
	}
	n := DecisionPush(d, ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	ntfyAssertClean(t, "decision title", n.Title, MaxPushTitle, false)
	ntfyAssertClean(t, "decision message", n.Message, MaxPushText, true)
	if len(n.Actions) != 3 {
		t.Fatalf("actions = %d", len(n.Actions))
	}
	for _, a := range n.Actions {
		ntfyAssertClean(t, "label", a.Label, MaxPushLabel, false)
	}
	// A line break inside a label or the question cannot forge a line.
	for _, l := range strings.Split(n.Message, "\n") {
		if l == "Recommended: Z" {
			t.Errorf("the question forged a line: %q", n.Message)
		}
	}
	if strings.Count(n.Message, "\nRecommended: ") > 1 {
		t.Errorf("a label forged a Recommended line: %q", n.Message)
	}

	r := model.Review{ID: "R-3", TaskID: "T-42", Summary: dirty + strings.Repeat("x", 5000), Tests: model.TestsPassed}
	rn := ReviewPush(r, model.Task{ID: "T-42", Title: dirty}, ntfyTopic)
	ntfyAssertClean(t, "review title", rn.Title, MaxPushTitle, false)
	ntfyAssertClean(t, "review message", rn.Message, MaxPushText, true)
}

func TestReviewPushWording(t *testing.T) {
	r := model.Review{
		ID: "R-3", TaskID: "T-42", Summary: "Added the retry loop.\nAll callers updated.",
		Tests: model.TestsPassed, TestsDetail: "detail SECRET-DETAIL", DiffStat: "stat SECRET-STAT",
	}
	task := model.Task{ID: "T-42", Title: "Retry failed uploads", Body: "body SECRET-BODY"}
	n := ReviewPush(r, task, ntfyTopic)
	if n.Topic != ntfyTopic || n.Title != "R-3 T-42: ready for review" || n.Priority != 3 || len(n.Actions) != 0 {
		t.Errorf("push = %+v", n)
	}
	want := "Retry failed uploads\nTests (the agent's report): passed\nSummary (the agent's):\nAdded the retry loop.\nAll callers updated."
	if n.Message != want {
		t.Errorf("message = %q, want %q", n.Message, want)
	}
	if strings.Contains(n.Message, "SECRET") {
		t.Errorf("message holds a field that is not pushed: %q", n.Message)
	}
	// The summary is cut to 600 runes.
	r.Summary = strings.Repeat("s", 3000)
	n = ReviewPush(r, task, ntfyTopic)
	lines := strings.Split(n.Message, "\n")
	if got := utf8.RuneCountInString(lines[len(lines)-1]); got != 600 {
		t.Errorf("summary is %d runes, want 600", got)
	}
	// An unknown tests value is not passed through as written.
	r.Tests = "passed\x1b[2J"
	if n = ReviewPush(r, task, ntfyTopic); strings.Contains(n.Message, "\x1b") {
		t.Errorf("message = %q", n.Message)
	}
}

type ntfyFailReader struct{}

func (ntfyFailReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestNewTokenShapeAndHash(t *testing.T) {
	shape := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	hexShape := regexp.MustCompile(`^[0-9a-f]{64}$`)
	plain, hash, err := NewToken(strings.NewReader(strings.Repeat("\xff", 64)))
	if err != nil {
		t.Fatal(err)
	}
	if !shape.MatchString(plain) || hash != HashToken(plain) || !hexShape.MatchString(hash) {
		t.Errorf("plain %q hash %q", plain, hash)
	}
	if HashToken("abc") != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("HashToken(abc) = %s", HashToken("abc"))
	}
	p1, _, err1 := NewToken(rand.Reader)
	p2, _, err2 := NewToken(rand.Reader)
	if err1 != nil || err2 != nil || p1 == p2 || !shape.MatchString(p1) {
		t.Errorf("two tokens %q %q (%v, %v)", p1, p2, err1, err2)
	}
	if p, h, err := NewToken(ntfyFailReader{}); err == nil || p != "" || h != "" {
		t.Errorf("failing reader: %q %q %v", p, h, err)
	}
	if _, _, err := NewToken(strings.NewReader("short")); err == nil {
		t.Error("a reader with fewer than 32 bytes gave a token")
	}
}

// ntfyServer records what it received and answers with status.
type ntfyServer struct {
	*httptest.Server
	hits    atomic.Int32
	body    []byte
	headers http.Header
	method  string
	path    string
}

func newNtfyServer(t *testing.T, status int, reply []byte) *ntfyServer {
	t.Helper()
	s := &ntfyServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.body, _ = io.ReadAll(r.Body)
		s.headers = r.Header.Clone()
		s.method, s.path = r.Method, r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write(reply)
	}))
	t.Cleanup(s.Close)
	return s
}

func ntfyFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "ntfy", name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestPublishMatchesCapturedRequest checks the request against probe N1's
// recording (publish-accepted.json): a JSON POST that ntfy 2.38.0 accepted.
// The input is the probe's ticket; the token is replaced by TOKEN. The probe
// also sent "tags", which section 4.7 does not, so that one field is left
// out of the comparison; every other field must be equal.
func TestPublishMatchesCapturedRequest(t *testing.T) {
	s := newNtfyServer(t, http.StatusOK, ntfyFixture(t, "publish-reply.json"))
	d := model.Decision{
		ID: "D-1", Repo: "/src/api-server", Question: "Decision D-1 needs you",
		Options:        []model.Option{{Key: "A", Label: "option A"}, {Key: "B", Label: "option B"}, {Key: "C", Label: "option C"}},
		Recommendation: "A", Reversibility: model.ReversCheap, Mode: model.ModeBlocking,
	}
	n := DecisionPush(d, "curia-probe-one", "http://127.0.0.1:56841/v1/answer", ntfyToken, "")
	if err := (Publisher{URL: s.URL}).Publish(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if s.method != http.MethodPost || s.path != "/" {
		t.Errorf("request = %s %s, want POST / (a JSON body at /<topic> is stored as plain text)", s.method, s.path)
	}
	if ct := s.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if ua := s.headers.Get("User-Agent"); ua != "curia" {
		t.Errorf("User-Agent = %q", ua)
	}
	var got, want map[string]any
	if err := json.Unmarshal(bytes.ReplaceAll(s.body, []byte(ntfyToken), []byte("TOKEN")), &got); err != nil {
		t.Fatalf("body %s: %v", s.body, err)
	}
	if err := json.Unmarshal(ntfyFixture(t, "publish-accepted.json"), &want); err != nil {
		t.Fatal(err)
	}
	if _, ok := want["tags"]; !ok {
		t.Fatal("the recording has no tags: drop this exception")
	}
	delete(want, "tags")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request body\n got %s\nwant %s", s.body, ntfyFixture(t, "publish-accepted.json"))
	}
}

func TestPublishWireDetails(t *testing.T) {
	s := newNtfyServer(t, http.StatusOK, nil)
	n := ReviewPush(model.Review{ID: "R-3", TaskID: "T-42", Summary: "x", Tests: model.TestsPassed}, model.Task{Title: "Kafka & <ZooKeeper>"}, ntfyTopic)
	n.Priority = 0
	for _, base := range []string{s.URL, s.URL + "/"} {
		if err := (Publisher{URL: base}).Publish(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		if s.path != "/" {
			t.Errorf("%s: posted to %q, want /", base, s.path)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(s.body, &got); err != nil {
		t.Fatal(err)
	}
	if got["priority"] != float64(3) {
		t.Errorf("zero priority sent as %v", got["priority"])
	}
	for _, k := range []string{"actions", "clear", "tags"} {
		if _, ok := got[k]; ok {
			t.Errorf("sent a top-level %q: %s", k, s.body)
		}
	}
	if !bytes.Contains(s.body, []byte("Kafka & <ZooKeeper>")) {
		t.Errorf("HTML escaping is on: %s", s.body)
	}
}

func TestPublishSetsBearerOnlyWhenGiven(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, "", "", "")
	s := newNtfyServer(t, http.StatusOK, nil)
	if err := (Publisher{URL: s.URL}).Publish(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.headers["Authorization"]; ok {
		t.Errorf("Authorization sent without an access token: %v", s.headers)
	}
	if err := (Publisher{URL: s.URL, AccessToken: ntfyAccess}).Publish(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if got := s.headers.Get("Authorization"); got != "Bearer "+ntfyAccess {
		t.Errorf("Authorization = %q", got)
	}
	if bytes.Contains(s.body, []byte(ntfyAccess)) {
		t.Errorf("the access token is in the body: %s", s.body)
	}
}

// ntfyClasses are the four kinds of failure a caller tells apart.
var ntfyClasses = []error{ErrNotAllowed, ErrRateLimited, ErrRejected, ErrUnavailable}

// ntfyAssertClass checks that err matches class and no other class, except
// that ErrNotAllowed also matches ErrRejected (neither is retried).
func ntfyAssertClass(t *testing.T, what string, err, class error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: no error, want %v", what, class)
		return
	}
	for _, c := range ntfyClasses {
		want := errors.Is(c, class) || errors.Is(class, ErrNotAllowed) && errors.Is(c, ErrRejected)
		if errors.Is(err, c) != want {
			t.Errorf("%s: errors.Is(%v) = %v, want %v (err %v)", what, c, !want, want, err)
		}
	}
}

// ntfyFailure is one way a publish fails, for the status and leak tests.
type ntfyFailure struct {
	name    string
	class   error
	publish func(t *testing.T, p Publisher, n Notification) error
}

func ntfyStatusFailure(status int, class error) ntfyFailure {
	return ntfyFailure{http.StatusText(status), class, func(t *testing.T, p Publisher, n Notification) error {
		// The reply echoes the secrets: it must be discarded, never quoted.
		s := newNtfyServer(t, status, []byte(`{"error":"`+ntfyAccess+" "+ntfyToken+" "+n.Message+`"}`))
		p.URL = s.URL
		return p.Publish(context.Background(), n)
	}}
}

func ntfyFailures() []ntfyFailure {
	f := []ntfyFailure{}
	for _, c := range []int{400, 404, 413} {
		f = append(f, ntfyStatusFailure(c, ErrRejected))
	}
	for _, c := range []int{401, 403} {
		f = append(f, ntfyStatusFailure(c, ErrNotAllowed))
	}
	f = append(f, ntfyStatusFailure(429, ErrRateLimited))
	for _, c := range []int{302, 500, 503} {
		f = append(f, ntfyStatusFailure(c, ErrUnavailable))
	}
	f = append(f, ntfyFailure{"closed server", ErrUnavailable, func(_ *testing.T, p Publisher, n Notification) error {
		s := httptest.NewServer(http.NotFoundHandler())
		s.Close()
		p.URL = s.URL
		return p.Publish(context.Background(), n)
	}}, ntfyFailure{"redirect", ErrUnavailable, func(t *testing.T, p Publisher, n Notification) error {
		_, err := ntfyRedirect(t, p, n)
		return err
	}}, ntfyFailure{"timeout", ErrUnavailable, func(t *testing.T, p Publisher, n Notification) error {
		_, err := ntfyHang(t, p, n)
		return err
	}})
	return f
}

func TestPublishStatusTable(t *testing.T) {
	ok := newNtfyServer(t, http.StatusOK, nil)
	n := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	if err := (Publisher{URL: ok.URL}).Publish(context.Background(), n); err != nil {
		t.Errorf("200: %v", err)
	}
	for _, code := range []int{201, 204} {
		s := newNtfyServer(t, code, nil)
		if err := (Publisher{URL: s.URL}).Publish(context.Background(), n); err != nil {
			t.Errorf("%d: %v", code, err)
		}
	}
	for _, f := range ntfyFailures() {
		ntfyAssertClass(t, f.name, f.publish(t, Publisher{AccessToken: ntfyAccess}, n), f.class)
	}
}

// ntfyReplay serves a response recorded by probe N1: either a full HTTP
// response (after optional "#" comment lines) or "HTTP <code>" then the body.
func ntfyReplay(t *testing.T, name string) *httptest.Server {
	t.Helper()
	raw := string(ntfyFixture(t, name))
	for strings.HasPrefix(raw, "#") {
		_, raw, _ = strings.Cut(raw, "\n")
	}
	code, header, body := 0, http.Header{}, ""
	if strings.HasPrefix(raw, "HTTP/1.1 ") {
		resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		code, header, body = resp.StatusCode, resp.Header, string(b)
	} else {
		first, rest, _ := strings.Cut(raw, "\n")
		if _, err := fmt.Sscanf(first, "HTTP %d", &code); err != nil {
			t.Fatalf("%s: first line %q", name, first)
		}
		header.Set("Content-Type", "application/json")
		body = rest
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		for k, v := range header {
			if k != "Content-Length" && k != "Date" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestPublishFixtureReplies(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	for _, name := range []string{"publish-reply.full.txt", "auth-valid-token.txt"} {
		if err := (Publisher{URL: ntfyReplay(t, name).URL, AccessToken: ntfyAccess}).Publish(context.Background(), n); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	cases := []struct {
		name  string
		class error
	}{
		{"auth-no-token.txt", ErrNotAllowed},
		{"auth-wrong-token.txt", ErrNotAllowed},
		{"rejected-rate-limited.full.txt", ErrRateLimited},
		{"rejected-four-actions.txt", ErrRejected},
		{"rejected-topic-invalid-slash.txt", ErrRejected},
		{"rejected-priority-9.txt", ErrRejected},
		{"rejected-message-8000-body-too-large.txt", ErrRejected},
		{"rejected-message-5000-attachments-disabled.txt", ErrRejected},
		{"rejected-message-5000.txt", ErrUnavailable},
	}
	for _, c := range cases {
		err := (Publisher{URL: ntfyReplay(t, c.name).URL, AccessToken: ntfyAccess}).Publish(context.Background(), n)
		ntfyAssertClass(t, c.name, err, c.class)
		var se *StatusError
		if !errors.As(err, &se) {
			t.Errorf("%s: %v is not a *StatusError", c.name, err)
			continue
		}
		wantRetry := time.Duration(0)
		if errors.Is(c.class, ErrRateLimited) {
			wantRetry = time.Hour // the recording's Retry-After: 3600
		}
		if se.RetryAfter != wantRetry {
			t.Errorf("%s: RetryAfter = %v, want %v", c.name, se.RetryAfter, wantRetry)
		}
	}
}

func TestPublishRetryAfterIsBounded(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, "", "", "")
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"30", 30 * time.Second},
		{" 45 ", 45 * time.Second},
		{"3600", time.Hour},
		{"86400", MaxRetryAfter},
		{"99999999999999999999", MaxRetryAfter},
		{"-5", 0},
		{"junk", 0},
		{"1.5", 0},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
	}
	for _, c := range cases {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if c.header != "" {
				w.Header().Set("Retry-After", c.header)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		err := (Publisher{URL: s.URL}).Publish(context.Background(), n)
		s.Close()
		var se *StatusError
		if !errors.As(err, &se) || !errors.Is(err, ErrRateLimited) || se.RetryAfter != c.want {
			t.Errorf("Retry-After %q: err %v, RetryAfter %v, want %v", c.header, err, se, c.want)
		}
	}
	if MaxRetryAfter > time.Hour {
		t.Errorf("MaxRetryAfter = %v, want at most an hour", MaxRetryAfter)
	}
}

// ntfyLimitsServer enforces the limits probe N1 observed on ntfy 2.38.0: a
// body of about 8 KiB or more is 413, a message of 4096 bytes or more is 400,
// more than three actions is 400, a topic outside [-_A-Za-z0-9]{1,64} is 400.
func ntfyLimitsServer(t *testing.T) (*httptest.Server, *[]byte) {
	t.Helper()
	topic := regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)
	var last []byte
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		last = body
		var m struct {
			Topic   string `json:"topic"`
			Message string `json:"message"`
			Actions []any  `json:"actions"`
		}
		switch {
		case len(body) >= 8192:
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		case json.Unmarshal(body, &m) != nil, len(m.Message) >= 4096, len(m.Actions) > 3, !topic.MatchString(m.Topic):
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(s.Close)
	return s, &last
}

func TestPushFitsNtfyByteLimits(t *testing.T) {
	s, last := ntfyLimitsServer(t)
	for _, fill := range []string{"😀", "€", `"`, `\`, "é\"", "😀\"", "😀\\", "a"} {
		long := strings.Repeat(fill, 5000)
		d := model.Decision{
			ID: "D-117", Repo: "/src/" + long, Question: long, Recommendation: "B",
			Options: []model.Option{
				{Key: "A", Label: long}, {Key: "B", Label: long}, {Key: "C", Label: long}, {Key: "D", Label: long},
			},
			Mode: model.ModeBlocking, Reversibility: model.ReversCheap,
		}
		dn := DecisionPush(d, ntfyTopic, "https://"+strings.Repeat("h", 180)+".ts.net/v1/answer", ntfyToken, "")
		rn := ReviewPush(model.Review{ID: "R-3", TaskID: "T-42", Summary: long, Tests: model.TestsPassed}, model.Task{Title: long}, ntfyTopic)
		for _, n := range []Notification{dn, rn} {
			if !utf8.ValidString(n.Message) || len(n.Message) >= 4096 {
				t.Errorf("fill %q: message of %d bytes (valid UTF-8 %v)", fill, len(n.Message), utf8.ValidString(n.Message))
			}
			if err := (Publisher{URL: s.URL}).Publish(context.Background(), n); err != nil {
				t.Errorf("fill %q: the limits server refused a %d-byte body: %v", fill, len(*last), err)
			}
			if len(*last) > maxBody {
				t.Errorf("fill %q: body of %d bytes, bound %d", fill, len(*last), maxBody)
			}
		}
		if !strings.HasSuffix(dn.Message, "…") || !strings.HasSuffix(rn.Message, "…") {
			t.Errorf("fill %q: a cut message does not end in an ellipsis", fill)
		}
	}
	// A message that fits is not cut.
	if n := DecisionPush(ntfyTicket(3), ntfyTopic, "", "", ""); strings.Contains(n.Message, "…") {
		t.Errorf("short message cut: %q", n.Message)
	}
}

// ntfyRedirect publishes to a server that answers 307 to a second server,
// and returns the error and how many requests the second server saw.
func ntfyRedirect(t *testing.T, p Publisher, n Notification) (int32, error) {
	t.Helper()
	second := newNtfyServer(t, http.StatusOK, nil)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(first.Close)
	p.URL = first.URL
	err := p.Publish(context.Background(), n)
	return second.hits.Load(), err
}

func TestPublishDoesNotFollowRedirect(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	hits, err := ntfyRedirect(t, Publisher{AccessToken: ntfyAccess}, n)
	if err == nil || hits != 0 {
		t.Errorf("err %v, second server saw %d requests", err, hits)
	}
	// A caller's client that would follow redirects does not either.
	follows := &http.Client{}
	hits, err = ntfyRedirect(t, Publisher{AccessToken: ntfyAccess, Client: follows}, n)
	if err == nil || hits != 0 {
		t.Errorf("caller's client: err %v, second server saw %d requests", err, hits)
	}
	if follows.CheckRedirect != nil {
		t.Error("Publish changed the caller's client")
	}
}

// ntfyHang publishes with a 50 ms context to a server that never answers.
func ntfyHang(t *testing.T, p Publisher, n Notification) (time.Duration, error) {
	t.Helper()
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); s.Close() })
	p.URL = s.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.Publish(ctx, n)
	return time.Since(start), err
}

func TestPublishTimesOut(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	took, err := ntfyHang(t, Publisher{}, n)
	if err == nil || took > time.Second {
		t.Errorf("err %v after %v", err, took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v does not say it timed out", err)
	}
}

// ntfyDeadlines is a RoundTripper that records the request's deadline and
// answers 200 itself, so nothing is dialed.
type ntfyDeadlines struct {
	deadline time.Time
	has      bool
}

func (d *ntfyDeadlines) RoundTrip(r *http.Request) (*http.Response, error) {
	d.deadline, d.has = r.Context().Deadline()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
}

func TestPublishBoundsItselfWithPublishLimit(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, "", "", "")
	rt := &ntfyDeadlines{}
	p := Publisher{URL: "http://127.0.0.1:9", Client: &http.Client{Transport: rt}}
	start := time.Now()
	if err := p.Publish(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if !rt.has || rt.deadline.Before(start.Add(PublishLimit-time.Second)) || rt.deadline.After(time.Now().Add(PublishLimit)) {
		t.Errorf("no deadline: has %v, %v after start", rt.has, rt.deadline.Sub(start))
	}
	// An earlier deadline of the caller's is kept.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	want, _ := ctx.Deadline()
	if err := p.Publish(ctx, n); err != nil {
		t.Fatal(err)
	}
	if !rt.deadline.Equal(want) {
		t.Errorf("deadline %v, want the caller's %v", rt.deadline, want)
	}
}

func TestPublishReadsAtMost4KiB(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64<<10))
		_ = http.NewResponseController(w).Flush()
		select { // never ends the reply on its own
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); s.Close() })
	start := time.Now()
	err := (Publisher{URL: s.URL}).Publish(context.Background(), DecisionPush(ntfyTicket(3), ntfyTopic, "", "", ""))
	if err != nil || time.Since(start) > time.Second {
		t.Errorf("err %v after %v: the reply was read to its end", err, time.Since(start))
	}
}

func TestPublishErrorLeaksNothing(t *testing.T) {
	n := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	secrets := []string{ntfyToken, ntfyAccess, ntfyTopic, n.Message, "Which queue", "Redis", "127.0.0.1", "http:"}
	for _, f := range ntfyFailures() {
		err := f.publish(t, Publisher{AccessToken: ntfyAccess}, n)
		if err == nil {
			t.Errorf("%s: no error", f.name)
			continue
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("%s: error %q holds %q", f.name, err, s)
			}
		}
	}
}

func TestPublishRefusesBadNotification(t *testing.T) {
	good := func() Notification { return DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "") }
	cases := []struct {
		name   string
		change func(*Notification, *Publisher)
	}{
		{"newline in title", func(n *Notification, _ *Publisher) { n.Title = "a\nX-Injected: 1" }},
		{"CR in title", func(n *Notification, _ *Publisher) { n.Title = "a\rb" }},
		{"U+2028 in title", func(n *Notification, _ *Publisher) { n.Title = "a\u2028b" }},
		{"long title", func(n *Notification, _ *Publisher) { n.Title = strings.Repeat("t", MaxPushTitle+1) }},
		{"ESC in message", func(n *Notification, _ *Publisher) { n.Message = "a\x1b[2Jb" }},
		{"long message", func(n *Notification, _ *Publisher) { n.Message = strings.Repeat("m", MaxPushText+1) }},
		{"empty topic", func(n *Notification, _ *Publisher) { n.Topic = "" }},
		{"topic with a slash", func(n *Notification, _ *Publisher) { n.Topic = "a/b" }},
		{"priority 6", func(n *Notification, _ *Publisher) { n.Priority = 6 }},
		{"four actions", func(n *Notification, _ *Publisher) { n.Actions = append(n.Actions, n.Actions[0]) }},
		{"newline in label", func(n *Notification, _ *Publisher) { n.Actions[0].Label = "B\nb" }},
		{"long label", func(n *Notification, _ *Publisher) { n.Actions[0].Label = strings.Repeat("l", MaxPushLabel+1) }},
		{"token with a newline", func(n *Notification, _ *Publisher) { n.Actions[0].Token = ntfyToken[:42] + "\n" }},
		{"short token", func(n *Notification, _ *Publisher) { n.Actions[1].Token = "abc" }},
		{"action url not http", func(n *Notification, _ *Publisher) { n.Actions[0].URL = "file:///etc/passwd" }},
		{"newline in action body", func(n *Notification, _ *Publisher) { n.Actions[0].Body = "{}\n" }},
		{"access token with a newline", func(_ *Notification, p *Publisher) { p.AccessToken = ntfyAccess + "\r\nX-Injected: 1" }},
		{"long access token", func(_ *Notification, p *Publisher) { p.AccessToken = strings.Repeat("a", 513) }},
		{"server url not http", func(_ *Notification, p *Publisher) { p.URL = "ftp://127.0.0.1/secret-path" }},
		{"server url with user info", func(_ *Notification, p *Publisher) {
			p.URL = strings.Replace(p.URL, "http://", "http://u:"+ntfyAccess+"@", 1)
		}},
		{"server url with a topic path", func(_ *Notification, p *Publisher) { p.URL += "/" + ntfyTopic }},
		{"server url with a deeper path", func(_ *Notification, p *Publisher) { p.URL += "/a/" }},
		{"server url with a query", func(_ *Notification, p *Publisher) { p.URL += "/?topic=x" }},
		{"message over the byte bound", func(n *Notification, _ *Publisher) { n.Message = strings.Repeat("😀", 1000) }},
		{"message of quotes over the byte bound", func(n *Notification, _ *Publisher) { n.Message = strings.Repeat(`"`, 1900) }},
		{"long action url", func(n *Notification, _ *Publisher) { n.Actions[0].URL = "https://h/" + strings.Repeat("u", 600) }},
		{"body over the bound", func(n *Notification, _ *Publisher) {
			for i := range n.Actions {
				n.Actions[i].Body = strings.Repeat(`"`, maxActionBody)
				n.Actions[i].URL = "https://h/" + strings.Repeat("u", maxActionURL-10)
			}
			n.Message = strings.Repeat("😀", 890)
		}},
	}
	for _, c := range cases {
		s := newNtfyServer(t, http.StatusOK, nil)
		n, p := good(), Publisher{URL: s.URL, AccessToken: ntfyAccess}
		c.change(&n, &p)
		err := p.Publish(context.Background(), n)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", c.name, err)
		}
		ntfyAssertClass(t, c.name, err, ErrRejected)
		if s.hits.Load() != 0 {
			t.Errorf("%s: the server saw the request", c.name)
		}
		if err != nil {
			for _, secret := range []string{ntfyAccess, ntfyToken, "Injected", n.Message, "secret-path"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s: error %q holds %q", c.name, err, secret)
				}
			}
		}
	}
}

func TestPublishDefaultClientIgnoresProxyEnvironment(t *testing.T) {
	tr, ok := defaultClient.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("the default transport may route through a proxy from the environment: %#v", defaultClient.Transport)
	}
	if defaultClient.CheckRedirect == nil {
		t.Error("the default client follows redirects")
	}
}

const ntfyWarmStakes = "answer within 12m to keep cache warm (rebuild ≈ $1.68)"

func TestDecisionPushStakesLine(t *testing.T) {
	plain := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	with := DecisionPush(ntfyTicket(3), ntfyTopic, ntfyAnswerURL, ntfyToken, ntfyWarmStakes)
	if want := plain.Message + "\n" + ntfyWarmStakes; with.Message != want {
		t.Errorf("message = %q, want %q", with.Message, want)
	}
	lines := strings.Split(with.Message, "\n")
	if lines[len(lines)-1] != ntfyWarmStakes {
		t.Errorf("last line = %q, want the stakes line", lines[len(lines)-1])
	}
	if with.Title != plain.Title || with.Priority != plain.Priority || !reflect.DeepEqual(with.Actions, plain.Actions) {
		t.Errorf("stakes changed more than the message: %+v vs %+v", with, plain)
	}
	// No stakes: the message of Plan 4, byte for byte.
	want := "Which queue should carry job events?\nA: Postgres LISTEN & NOTIFY\nB: Redis stream\nC: Kafka\nRecommended: B"
	if plain.Message != want {
		t.Errorf("message without stakes = %q, want %q", plain.Message, want)
	}
}

func TestDecisionPushStakesIsCleanedAndBounded(t *testing.T) {
	n := DecisionPush(ntfyTicket(2), ntfyTopic, "", "", "line one\nforged: line\x1b[31m "+strings.Repeat("x", 500))
	lines := strings.Split(n.Message, "\n")
	last := lines[len(lines)-1]
	if utf8.RuneCountInString(last) > 120 || strings.ContainsAny(n.Message, "\x1b") || strings.Contains(last, "forged: line\n") {
		t.Errorf("stakes line not cleaned to one line of at most 120 runes: %q", last)
	}
	if !strings.HasPrefix(last, "line one forged: line") {
		t.Errorf("stakes line = %q", last)
	}
	// Only whitespace: no line at all.
	plain := DecisionPush(ntfyTicket(2), ntfyTopic, "", "", "")
	if got := DecisionPush(ntfyTicket(2), ntfyTopic, "", "", " \n\t "); got.Message != plain.Message {
		t.Errorf("blank stakes added %q", got.Message)
	}
}

func TestDecisionPushIrreversibleIgnoresStakes(t *testing.T) {
	d := ntfyTicket(3)
	d.Reversibility = model.ReversIrreversible
	a := DecisionPush(d, ntfyTopic, ntfyAnswerURL, ntfyToken, "")
	b := DecisionPush(d, ntfyTopic, ntfyAnswerURL, ntfyToken, ntfyWarmStakes)
	if a.Message != b.Message || strings.Contains(b.Message, "cache") {
		t.Errorf("irreversible push changed by stakes: %q", b.Message)
	}
}

func TestDecisionPushStakesSurvivesTheCut(t *testing.T) {
	for _, fill := range []string{"a", "é", "€", "😀", `"`, "\\"} {
		for _, qlen := range []int{1500, 5000} {
			d := ntfyTicket(2)
			d.Question = strings.Repeat(fill, qlen)
			n := DecisionPush(d, ntfyTopic, "", "", ntfyWarmStakes)
			if !utf8.ValidString(n.Message) || len(n.Message) >= 4096 || jsonLen(n.Message) > maxMessageBytes {
				t.Errorf("fill %q: message of %d bytes (json %d)", fill, len(n.Message), jsonLen(n.Message))
			}
			if utf8.RuneCountInString(n.Message) > MaxPushText {
				t.Errorf("fill %q: %d runes, want at most %d", fill, utf8.RuneCountInString(n.Message), MaxPushText)
			}
			if !strings.HasSuffix(n.Message, "\n"+ntfyWarmStakes) {
				t.Errorf("fill %q: message does not end with the stakes line", fill)
			}
			lines := strings.Split(n.Message, "\n")
			if !strings.HasSuffix(lines[0], "…") || !strings.Contains(n.Message, "Recommended: B") {
				t.Errorf("fill %q: the question should be the cut text, with the options kept: %q", fill, n.Message[:min(60, len(n.Message))])
			}
		}
	}
}

func TestDecisionPushStakesWithHugeOptionsStillEndsWithIt(t *testing.T) {
	s, last := ntfyLimitsServer(t)
	for _, fill := range []string{"😀", "€", `"`, "a"} {
		long := strings.Repeat(fill, 5000)
		d := model.Decision{
			ID: "D-117", Repo: "/src/api", Question: long, Recommendation: "B",
			Options: []model.Option{{Key: "A", Label: long}, {Key: "B", Label: long}, {Key: "C", Label: long}, {Key: "D", Label: long}},
			Mode:    model.ModeBlocking, Reversibility: model.ReversCheap,
		}
		n := DecisionPush(d, ntfyTopic, "https://h.ts.net/v1/answer", ntfyToken, ntfyWarmStakes)
		if !strings.HasSuffix(n.Message, "\n"+ntfyWarmStakes) || len(n.Message) >= 4096 || !utf8.ValidString(n.Message) ||
			utf8.RuneCountInString(n.Message) > MaxPushText {
			t.Errorf("fill %q: %d bytes, %d runes, tail %q", fill, len(n.Message), utf8.RuneCountInString(n.Message), n.Message[max(0, len(n.Message)-70):])
		}
		if err := (Publisher{URL: s.URL}).Publish(context.Background(), n); err != nil {
			t.Errorf("fill %q: the limits server refused a %d-byte body: %v", fill, len(*last), err)
		}
	}
}

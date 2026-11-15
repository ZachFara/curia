package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/task"
)

// Every server in this file is an httptest server on 127.0.0.1, and every
// client that is not one of those servers' answers itself without dialing:
// no test here opens a connection to any other host. The linear.app URLs
// below are only strings inside responses; nothing connects to them.

// linearKey has the shape of a personal API key.
const (
	linearKey = "lin_api_SECRETk3y9Q2x7Zw"
	// linearForbidden is the one word the request and the query never hold.
	linearForbidden = "mutation"
)

// linearRequest is one request a test server saw.
type linearRequest struct {
	method, path, rawQuery, contentType, auth, userAgent string
	body                                                 []byte
}

// linearRecorder records every request and answers each with reply.
type linearRecorder struct {
	mu    sync.Mutex
	seen  []linearRequest
	reply func(w http.ResponseWriter, r *http.Request, n int, body []byte)
}

func (rec *linearRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	rec.seen = append(rec.seen, linearRequest{
		method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery,
		contentType: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization"),
		userAgent: r.Header.Get("User-Agent"), body: body,
	})
	n := len(rec.seen)
	rec.mu.Unlock()
	rec.reply(w, r, n, body)
}

func (rec *linearRecorder) requests() []linearRequest {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]linearRequest(nil), rec.seen...)
}

// linearServe starts a loopback server answering with reply and returns a
// client pointed at its /graphql endpoint, the default client in use.
func linearServe(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, n int, body []byte)) (Client, *linearRecorder) {
	t.Helper()
	rec := &linearRecorder{reply: reply}
	s := httptest.NewServer(rec)
	t.Cleanup(s.Close)
	return Client{URL: s.URL + "/graphql", Key: linearKey}, rec
}

func linearFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.Clean(name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// linearAfter is the "after" variable of a request body: "" for null.
func linearAfter(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Variables struct {
			After *string `json:"after"`
		} `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Errorf("request body is not JSON: %v", err)
		return ""
	}
	if req.Variables.After == nil {
		return ""
	}
	return *req.Variables.After
}

// linearPages serves built-issues.json for the first page and
// built-issues-page2.json for the cursor it names.
func linearPages(t *testing.T) (Client, *linearRecorder) {
	t.Helper()
	page1, page2 := linearFixture(t, "built-issues.json"), linearFixture(t, "built-issues-page2.json")
	return linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, body []byte) {
		switch linearAfter(t, body) {
		case "":
			_, _ = w.Write(page1)
		case "built-cursor-1":
			_, _ = w.Write(page2)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

// linearBuiltIssues is what the two built files hold, in order.
var linearBuiltIssues = []Issue{
	{
		Identifier: "ENG-101", Title: "Fix the login redirect loop",
		Description: "Users bounce between /login and /home.\n\n- [ ] reproduce\n- [ ] fix",
		URL:         "https://linear.app/built-team/issue/ENG-101/fix-the-login-redirect-loop",
		StateType:   "unstarted", Priority: 2, Labels: []string{"Backend", "Needs Review!"},
	},
	{
		Identifier: "ENG-102", Title: "Write the onboarding guide",
		URL:       "https://linear.app/built-team/issue/ENG-102/write-the-onboarding-guide",
		StateType: "backlog", Priority: 0,
	},
	{
		Identifier: "eng 7", Title: "An issue whose identifier is malformed", Description: "Never imported.",
		URL: "https://linear.app/built-team/issue/eng-7", StateType: "backlog", Priority: 3,
	},
	{
		Identifier: "OPS-9", Title: "Rotate the CI runners", Description: "Old runners use the retired image.",
		URL: "https://linear.app.evil.example/OPS-9", StateType: "backlog", Priority: 4,
		Labels: []string{"Infra", "infra", "CI/CD pipeline"},
	},
	{
		Identifier: "ENG-103", Title: "Show the \x1b[31mred\x1b[0m banner", Description: "The banner is \x1b[31mred\x1b[0m text.",
		URL:       "https://linear.app/built-team/issue/ENG-103/show-the-red-banner",
		StateType: "unstarted", Priority: 1, Labels: []string{"UI"},
	},
}

func TestClientReadsBuiltPages(t *testing.T) {
	c, rec := linearPages(t)
	got, err := c.Issues(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, linearBuiltIssues) {
		t.Errorf("issues:\n got %#v\nwant %#v", got, linearBuiltIssues)
	}
	if n := len(rec.requests()); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	fields, invalid := Map(got)
	if want := []Invalid{{Index: 2, Reason: reasonIdentifier}}; !reflect.DeepEqual(invalid, want) {
		t.Errorf("invalid %#v, want %#v", invalid, want)
	}
	var refs []string
	for _, f := range fields {
		refs = append(refs, f.SourceRef)
	}
	if want := []string{"ENG-101", "ENG-102", "OPS-9", "ENG-103"}; !reflect.DeepEqual(refs, want) {
		t.Errorf("mapped %v, want %v", refs, want)
	}
}

// TestClientReadsCapturedResponse replays every response file in testdata:
// the built ones, which must be there, and issues-captured.json, which probe
// L1 (the owner's, with a real key) records from the real API. Each file is
// served as the first page (a later page is an empty last one); it must
// decode, give one issue per node, and map at least one issue.
func TestClientReadsCapturedResponse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[filepath.Base(f)] = true
		t.Run(filepath.Base(f), func(t *testing.T) { linearReplay(t, filepath.Base(f)) })
	}
	for _, want := range []string{"built-issues.json", "built-issues-page2.json"} {
		if !seen[want] {
			t.Errorf("testdata/%s is missing", want)
		}
	}
	t.Logf("replayed %d files; issues-captured.json present: %v", len(files), seen["issues-captured.json"])
}

func linearReplay(t *testing.T, name string) {
	data := linearFixture(t, name)
	var raw struct {
		Data struct {
			Issues struct {
				Nodes []json.RawMessage `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("not a response: %v", err)
	}
	empty := []byte(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`)
	c, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, n int, _ []byte) {
		if n == 1 {
			_, _ = w.Write(data)
			return
		}
		_, _ = w.Write(empty)
	})
	got, err := c.Issues(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(raw.Data.Issues.Nodes) {
		t.Errorf("%d issues from %d nodes", len(got), len(raw.Data.Issues.Nodes))
	}
	fields, invalid := Map(got)
	if len(fields) == 0 {
		t.Errorf("no issue maps to a task: %#v", invalid)
	}
	if len(fields)+len(invalid) != len(got) {
		t.Errorf("%d mapped + %d invalid != %d issues", len(fields), len(invalid), len(got))
	}
}

// linearBody is the exact body of a request for the first page (after "")
// or a later one, with or without a team.
func linearBody(after, team string) string {
	q, _ := json.Marshal(`query($first: Int!, $after: String, $filter: IssueFilter) {
  issues(first: $first, after: $after, filter: $filter, orderBy: updatedAt) {
    nodes { identifier title description priority url state { type } labels { nodes { name } } }
    pageInfo { hasNextPage endCursor }
  }
}`)
	cursor := "null"
	if after != "" {
		cursor = `"` + after + `"`
	}
	teamPart := ""
	if team != "" {
		teamPart = `,"team":{"key":{"eq":"` + team + `"}}`
	}
	return `{"query":` + string(q) + `,"variables":{"first":50,"after":` + cursor +
		`,"filter":{"assignee":{"isMe":{"eq":true}},"state":{"type":{"in":["backlog","unstarted"]}}` + teamPart + `}}}`
}

// linearCheckRequest checks one request of an Issues call: method, path,
// headers, the query byte for byte, and nothing else of note in the body.
func linearCheckRequest(t *testing.T, i int, r linearRequest) {
	t.Helper()
	if r.method != http.MethodPost || r.path != "/graphql" || r.rawQuery != "" {
		t.Errorf("request %d: %s %s?%s", i, r.method, r.path, r.rawQuery)
	}
	if r.contentType != "application/json" || r.auth != linearKey || r.userAgent != "curia" {
		t.Errorf("request %d headers: Content-Type %q, Authorization %q, User-Agent %q", i, r.contentType, r.auth, r.userAgent)
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil || body.Query != Query {
		t.Errorf("request %d: query is not linear.Query byte for byte (err %v)", i, err)
	}
	for _, s := range []string{Query, string(r.body)} {
		if strings.Contains(strings.ToLower(s), linearForbidden) {
			t.Errorf("request %d or Query holds the forbidden word: %s", i, s)
		}
	}
	if strings.Contains(string(r.body), linearKey) {
		t.Errorf("request %d: the key is in the body", i)
	}
}

func TestClientSendsOnlyTheQuery(t *testing.T) {
	c, rec := linearPages(t)
	if _, err := c.Issues(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	reqs := rec.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	for i, r := range reqs {
		linearCheckRequest(t, i, r)
	}
	if got, want := string(reqs[0].body), linearBody("", ""); got != want {
		t.Errorf("first body\n got %s\nwant %s", got, want)
	}
	if got, want := string(reqs[1].body), linearBody("built-cursor-1", ""); got != want {
		t.Errorf("second body\n got %s\nwant %s", got, want)
	}
}

func TestClientTeamFilter(t *testing.T) {
	for _, team := range []string{"ENG", ""} {
		c, rec := linearPages(t)
		if _, err := c.Issues(context.Background(), team); err != nil {
			t.Fatal(err)
		}
		reqs := rec.requests()
		if got, want := string(reqs[0].body), linearBody("", team); got != want {
			t.Errorf("team %q: body\n got %s\nwant %s", team, got, want)
		}
		var body struct {
			Variables struct {
				Filter map[string]json.RawMessage `json:"filter"`
			} `json:"variables"`
		}
		if err := json.Unmarshal(reqs[1].body, &body); err != nil {
			t.Fatal(err)
		}
		_, has := body.Variables.Filter["team"]
		if has != (team != "") {
			t.Errorf("team %q: second page's filter has a team key: %v", team, has)
		}
	}
}

// linearPage is one response page of n issues ENG-<first>... with the given
// page info.
func linearPage(first, n int, next bool, cursor string) []byte {
	var nodes []string
	for i := range n {
		nodes = append(nodes, fmt.Sprintf(`{"identifier":"ENG-%d","title":"Issue %d","description":"","priority":3,`+
			`"url":"https://linear.app/t/issue/ENG-%d","state":{"type":"backlog"},"labels":{"nodes":[]}}`, first+i, first+i, first+i))
	}
	cur := "null"
	if cursor != "" {
		cur = `"` + cursor + `"`
	}
	return []byte(`{"data":{"issues":{"nodes":[` + strings.Join(nodes, ",") +
		`],"pageInfo":{"hasNextPage":` + fmt.Sprint(next) + `,"endCursor":` + cur + `}}}}`)
}

func TestClientStopsAtMaxPages(t *testing.T) {
	c, rec := linearServe(t, func(w http.ResponseWriter, _ *http.Request, n int, _ []byte) {
		_, _ = w.Write(linearPage(n, 1, true, fmt.Sprintf("cursor-%d", n)))
	})
	got, err := c.Issues(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	reqs := rec.requests()
	if len(reqs) != MaxPages || len(got) != MaxPages {
		t.Errorf("%d requests and %d issues, want %d of each", len(reqs), len(got), MaxPages)
	}
	for i, r := range reqs[1:] {
		if after := linearAfter(t, r.body); after != fmt.Sprintf("cursor-%d", i+1) {
			t.Errorf("request %d sent after %q", i+2, after)
		}
	}
}

// linearRedirect points c at a server that answers 302 to a second listener
// and returns how many requests the second one saw.
func linearRedirect(t *testing.T, c Client) (int64, error) {
	t.Helper()
	var hits atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(linearPage(1, 1, false, ""))
	}))
	t.Cleanup(second.Close)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/graphql", http.StatusFound)
	}))
	t.Cleanup(first.Close)
	c.URL, c.Key = first.URL+"/graphql", linearKey
	_, err := c.Issues(context.Background(), "")
	return hits.Load(), err
}

func TestClientDoesNotFollowRedirect(t *testing.T) {
	hits, err := linearRedirect(t, Client{})
	if err == nil || hits != 0 {
		t.Fatalf("err %v, the second listener saw %d requests", err, hits)
	}
	if err.Error() != "linear: status 302" || !errors.Is(err, ErrUnavailable) {
		t.Errorf("err %q, want linear: status 302 of class ErrUnavailable", err)
	}
	// A caller's client that would follow redirects does not either, and is not changed.
	follows := &http.Client{}
	hits, err = linearRedirect(t, Client{HTTP: follows})
	if err == nil || hits != 0 {
		t.Errorf("caller's client: err %v, the second listener saw %d requests", err, hits)
	}
	if follows.CheckRedirect != nil {
		t.Error("Issues changed the caller's client")
	}
}

// linearErrorCase is one failing server and the error it must give.
type linearErrorCase struct {
	name  string
	reply func(w http.ResponseWriter)
	text  string // the whole error text
	class error
}

func linearStatus(code int, header ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"errors":[{"message":"body of a ` + fmt.Sprint(code) + ` ` + linearKey + `"}]}`))
	}
}

func linearBodyReply(body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = w.Write([]byte(body)) }
}

var linearErrorCases = []linearErrorCase{
	{"401", linearStatus(401), "linear: status 401", ErrUnauthorized},
	{"403", linearStatus(403), "linear: status 403", ErrUnauthorized},
	{"429", linearStatus(429, "Retry-After", "30"), "linear: status 429", ErrRateLimited},
	{"400", linearStatus(400), "linear: status 400", ErrRejected},
	{"404", linearStatus(404), "linear: status 404", ErrRejected},
	{"500", linearStatus(500), "linear: status 500", ErrUnavailable},
	{"503", linearStatus(503), "linear: status 503", ErrUnavailable},
	{"204", linearStatus(204), "linear: status 204", ErrUnavailable},
	{"graphql error", linearBodyReply(`{"errors":[{"message":"x\u001b[2J"}]}`), "linear: x", ErrRejected},
	{"graphql error beside data", linearBodyReply(`{"errors":[{"message":"Field \"foo\" unknown"},{"message":"second"}],` +
		`"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), `linear: Field "foo" unknown`, ErrRejected},
	{"graphql error without message", linearBodyReply(`{"errors":[{"code":7}]}`), "linear: the server reported an error", ErrRejected},
	{"graphql error not an object", linearBodyReply(`{"errors":["boom"]}`), "linear: the server reported an error", ErrRejected},
	{"graphql errors of the wrong shape", linearBodyReply(`{"errors":"boom"}`), "linear: response not understood", ErrBadResponse},
	{"invalid JSON", linearBodyReply(`{"data":`), "linear: response not understood", ErrBadResponse},
	{"trailing data", linearBodyReply(string(linearPage(1, 1, false, "")) + `{}`), "linear: response not understood", ErrBadResponse},
	{"empty body", linearBodyReply(``), "linear: response not understood", ErrBadResponse},
	{"data null", linearBodyReply(`{"data":null}`), "linear: response not understood", ErrBadResponse},
	{"no issues", linearBodyReply(`{"data":{}}`), "linear: response not understood", ErrBadResponse},
	{"no nodes", linearBodyReply(`{"data":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`), "linear: response not understood", ErrBadResponse},
	{"nodes not a list", linearBodyReply(`{"data":{"issues":{"nodes":{},"pageInfo":{"hasNextPage":false}}}}`), "linear: response not understood", ErrBadResponse},
	{"no page info", linearBodyReply(`{"data":{"issues":{"nodes":[]}}}`), "linear: response not understood", ErrBadResponse},
	{"no hasNextPage", linearBodyReply(`{"data":{"issues":{"nodes":[],"pageInfo":{"endCursor":"c"}}}}`), "linear: response not understood", ErrBadResponse},
	{"next page without cursor", linearBodyReply(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}}`), "linear: response not understood", ErrBadResponse},
	{"next page with an empty cursor", linearBodyReply(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":""}}}}`), "linear: response not understood", ErrBadResponse},
	{"cursor with a control character", linearBodyReply(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"a\nb"}}}}`), "linear: response not understood", ErrBadResponse},
	{"cursor too long", linearBodyReply(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"` + strings.Repeat("c", 513) + `"}}}}`), "linear: response not understood", ErrBadResponse},
	{"5 MiB", linearBodyReply(`{"data":` + strings.Repeat(" ", 5<<20) + `null}`), "linear: response too large", ErrBadResponse},
	{"one byte over", linearBodyReply(linearPadded(MaxResponse + 1)), "linear: response too large", ErrBadResponse},
	{"truncated", func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write([]byte(`{"data":{"issues":`))
	}, "linear: connection closed", ErrUnavailable},
}

// linearPadded is a valid last page of one issue, padded with spaces to n bytes.
func linearPadded(n int) string {
	p := string(linearPage(1, 1, false, ""))
	return p + strings.Repeat(" ", n-len(p))
}

var linearClasses = []error{ErrInvalid, ErrUnauthorized, ErrRateLimited, ErrRejected, ErrUnavailable, ErrBadResponse}

// linearCheckClass fails unless err matches class and no other class.
func linearCheckClass(t *testing.T, name string, err, class error) {
	t.Helper()
	for _, c := range linearClasses {
		if errors.Is(err, c) != errors.Is(class, c) {
			t.Errorf("%s: errors.Is(%q, %v) = %v", name, err, c, errors.Is(err, c))
		}
	}
}

// linearKeyPiece is the first 8-byte piece of the key that s holds, or "".
func linearKeyPiece(s string) string {
	for i := 0; i+8 <= len(linearKey); i++ {
		if strings.Contains(s, linearKey[i:i+8]) {
			return linearKey[i : i+8]
		}
	}
	return ""
}

// linearCheckClean fails when the error, or any error it wraps, holds a
// piece of the key or an escape, or its text holds the server's address or
// a URL.
func linearCheckClean(t *testing.T, name string, err error) {
	t.Helper()
	for e := err; e != nil; e = errors.Unwrap(e) {
		if p := linearKeyPiece(e.Error()); p != "" || strings.Contains(e.Error(), "\x1b") {
			t.Errorf("%s: error %q holds a piece of the key (%q) or an escape", name, e, p)
		}
	}
	for _, bad := range []string{"127.0.0.1", "http:", "/graphql"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("%s: error %q holds %q", name, err, bad)
		}
	}
}

func TestClientErrors(t *testing.T) {
	for _, c := range linearErrorCases {
		client, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ []byte) { c.reply(w) })
		got, err := client.Issues(context.Background(), "")
		if err == nil || got != nil {
			t.Errorf("%s: issues %v, err %v; want an error and no issues", c.name, got, err)
			continue
		}
		if err.Error() != c.text {
			t.Errorf("%s: err %q, want %q", c.name, err, c.text)
		}
		linearCheckClass(t, c.name, err, c.class)
		linearCheckClean(t, c.name, err)
	}
}

func TestClientErrorMessageIsCut(t *testing.T) {
	c, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ []byte) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"` + strings.Repeat("é", 500) + `"}]}`))
	})
	_, err := c.Issues(context.Background(), "")
	if err == nil {
		t.Fatal("no error")
	}
	msg := strings.TrimPrefix(err.Error(), "linear: ")
	if n := len([]rune(msg)); n != 200 || !strings.HasSuffix(msg, "…") {
		t.Errorf("message of %d runes: %q", n, msg)
	}
}

func TestClientReadsExactlyMaxResponse(t *testing.T) {
	c, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ []byte) {
		_, _ = w.Write([]byte(linearPadded(MaxResponse)))
	})
	got, err := c.Issues(context.Background(), "")
	if err != nil || len(got) != 1 {
		t.Errorf("a response of exactly MaxResponse bytes: %d issues, err %v", len(got), err)
	}
}

func TestClientStopsReadingAtTheLimit(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":` + strings.Repeat(" ", MaxResponse+1)))
		_ = http.NewResponseController(w).Flush()
		select { // never ends the reply on its own
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); s.Close() })
	start := time.Now()
	_, err := Client{URL: s.URL + "/graphql", Key: linearKey}.Issues(context.Background(), "")
	if err == nil || err.Error() != "linear: response too large" || time.Since(start) > 5*time.Second {
		t.Errorf("err %v after %v: the reply was not cut at MaxResponse", err, time.Since(start))
	}
}

func TestClientRetryAfterIsBounded(t *testing.T) {
	for _, c := range []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"30", 30 * time.Second},
		{" 45 ", 45 * time.Second},
		{"3600", time.Hour},
		{"86400", time.Hour},
		{"99999999999999999999", time.Hour},
		{"-5", 0},
		{"1.5", 0},
		{"soon", 0},
		{"Wed, 21 Oct 2026 07:28:00 GMT", 0},
	} {
		client, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ []byte) {
			if c.header != "" {
				w.Header().Set("Retry-After", c.header)
			}
			w.WriteHeader(http.StatusTooManyRequests)
		})
		_, err := client.Issues(context.Background(), "")
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 429 || se.RetryAfter != c.want {
			t.Errorf("Retry-After %q: err %#v, want RetryAfter %v", c.header, err, c.want)
		}
	}
	// Retry-After means nothing on another status.
	client, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ []byte) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := client.Issues(context.Background(), "")
	var se *StatusError
	if !errors.As(err, &se) || se.RetryAfter != 0 {
		t.Errorf("503 with Retry-After: %#v", err)
	}
}

func TestClientFailingLaterPageReturnsNothing(t *testing.T) {
	c, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, n int, _ []byte) {
		if n == 1 {
			_, _ = w.Write(linearPage(1, 2, true, "c1"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	got, err := c.Issues(context.Background(), "")
	if err == nil || got != nil {
		t.Errorf("issues %v, err %v: a failed page must give no issues", got, err)
	}
}

// linearEchoes are servers that echo the Authorization header back in a
// GraphQL error message, plainly and in disguises the cleaning undoes.
var linearEchoes = []struct {
	name string
	msg  func(key string) string
}{
	{"plain", func(k string) string { return "invalid key " + k }},
	{"twice", func(k string) string { return k + " is not " + k }},
	{"split by a control character", func(k string) string { return "key " + k[:10] + "\a" + k[10:] }},
	{"split by an escape sequence", func(k string) string { return "key " + k[:10] + "\x1b[0m" + k[10:] }},
	{"where the message is cut", func(k string) string { return strings.Repeat("a", 185) + k }},
	{"split where the message is cut", func(k string) string { return strings.Repeat("a", 185) + k[:5] + "\a" + k[5:] }},
}

func TestErrorsNeverHoldTheKey(t *testing.T) {
	for _, e := range linearEchoes {
		c, _ := linearServe(t, func(w http.ResponseWriter, r *http.Request, _ int, _ []byte) {
			msg, _ := json.Marshal(e.msg(r.Header.Get("Authorization")))
			_, _ = w.Write([]byte(`{"errors":[{"message":` + string(msg) + `}]}`))
		})
		_, err := c.Issues(context.Background(), "")
		if err == nil {
			t.Fatalf("%s: no error", e.name)
		}
		linearCheckClean(t, e.name, err)
		if e.name == "plain" && err.Error() != "linear: invalid key <key>" {
			t.Errorf("%s: err %q, want the key replaced by <key>", e.name, err)
		}
		linearCheckClass(t, e.name, err, ErrRejected)
	}
	// Bodies of other statuses are never read into an error.
	for _, code := range []int{400, 401, 429, 500} {
		c, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ []byte) { linearStatus(code)(w) })
		_, err := c.Issues(context.Background(), "")
		linearCheckClean(t, fmt.Sprint(code), err)
	}
}

func TestGuardReplacesTheKey(t *testing.T) {
	err := guard(remoteError{"seen " + linearKey}, linearKey)
	if err.Error() != "linear: seen <key>" || !errors.Is(err, ErrRejected) || errors.Unwrap(err) != nil {
		t.Errorf("guard gave %q (unwraps to %v)", err, errors.Unwrap(err))
	}
	if guard(nil, linearKey) != nil {
		t.Error("guard(nil) is not nil")
	}
	plain := remoteError{"nothing secret"}
	var got remoteError
	if !errors.As(guard(plain, linearKey), &got) || got != plain {
		t.Error("guard changed an error without the key")
	}
}

// linearHang points c at a server that never answers and calls Issues with
// a 50 ms context.
func linearHang(t *testing.T) (time.Duration, error) {
	t.Helper()
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Client{URL: s.URL + "/graphql", Key: linearKey}.Issues(ctx, "")
	return time.Since(start), err
}

func TestClientTimeout(t *testing.T) {
	took, err := linearHang(t)
	if err == nil || took > time.Second {
		t.Fatalf("err %v after %v", err, took)
	}
	if err.Error() != "linear: timeout" || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %q does not say it timed out", err)
	}
	linearCheckClass(t, "timeout", err, ErrUnavailable)
	linearCheckClean(t, "timeout", err)
}

// linearTrip is a RoundTripper that records each request's deadline and
// answers a one-issue last page itself, so nothing is dialed.
type linearTrip struct {
	hits     atomic.Int64
	deadline time.Time
	has      bool
}

func (d *linearTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	d.hits.Add(1)
	d.deadline, d.has = r.Context().Deadline()
	return &http.Response{
		StatusCode: http.StatusOK, Request: r,
		Body: io.NopCloser(bytes.NewReader(linearPage(1, 1, false, ""))),
	}, nil
}

func TestClientBoundsItselfWithRequestLimit(t *testing.T) {
	rt := &linearTrip{}
	c := Client{URL: "http://127.0.0.1:9/graphql", Key: linearKey, HTTP: &http.Client{Transport: rt}}
	start := time.Now()
	if _, err := c.Issues(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if !rt.has || rt.deadline.Before(start.Add(RequestLimit-time.Second)) || rt.deadline.After(time.Now().Add(RequestLimit)) {
		t.Errorf("deadline: has %v, %v after start", rt.has, rt.deadline.Sub(start))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	want, _ := ctx.Deadline()
	if _, err := c.Issues(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if !rt.deadline.Equal(want) {
		t.Errorf("deadline %v, want the caller's %v", rt.deadline, want)
	}
}

func TestInvalidKeyOrTeamSendsNothing(t *testing.T) {
	for _, c := range []struct {
		name, url, key, team string
	}{
		{"key with a space", "", "a b", ""},
		{"empty key", "", "", ""},
		{"key with a newline", "", "lin_api\nX-Evil: 1", ""},
		{"key of 201 bytes", "", strings.Repeat("k", 201), ""},
		{"key with a colon", "", "Bearer:abc", ""},
		{"lowercase team", "", linearKey, "eng"},
		{"team with a dash", "", linearKey, "ENG-1"},
		{"team of 11", "", linearKey, "ABCDEFGHIJK"},
		{"team starting with a digit", "", linearKey, "1ENG"},
		{"team with a quote", "", linearKey, `ENG"}`},
		{"empty URL", "-", linearKey, ""},
		{"plain http to another host", "http://api.linear.app/graphql", linearKey, ""},
		{"plain http to localhost by name", "http://localhost:8080/graphql", linearKey, ""},
		{"plain http to a non-loopback address", "http://192.0.2.1/graphql", linearKey, ""},
		{"plain http to an unspecified address", "http://0.0.0.0:8080/graphql", linearKey, ""},
		{"user info", "https://u:p@api.linear.app/graphql", linearKey, ""},
		{"query", "https://api.linear.app/graphql?key=1", linearKey, ""},
		{"empty query", "https://api.linear.app/graphql?", linearKey, ""},
		{"fragment", "https://api.linear.app/graphql#x", linearKey, ""},
		{"other path", "https://api.linear.app/v2/graphql", linearKey, ""},
		{"no path", "https://api.linear.app", linearKey, ""},
		{"escaped path", "https://api.linear.app/graph%71l", linearKey, ""},
		{"other scheme", "ftp://api.linear.app/graphql", linearKey, ""},
		{"no host", "https:///graphql", linearKey, ""},
		{"opaque", "https:api.linear.app/graphql", linearKey, ""},
	} {
		rt := &linearTrip{}
		u := c.url
		switch u {
		case "":
			u = "https://api.linear.app/graphql"
		case "-":
			u = ""
		}
		_, err := Client{URL: u, Key: c.key, HTTP: &http.Client{Transport: rt}}.Issues(context.Background(), c.team)
		if err == nil || rt.hits.Load() != 0 {
			t.Errorf("%s: err %v, %d requests sent", c.name, err, rt.hits.Load())
			continue
		}
		linearCheckClass(t, c.name, err, ErrInvalid)
		if c.key != "" && strings.Contains(err.Error(), c.key) || c.team != "" && strings.Contains(err.Error(), c.team) {
			t.Errorf("%s: err %q quotes the value", c.name, err)
		}
	}
}

func TestClientAcceptsEndpoints(t *testing.T) {
	for _, u := range []string{
		"https://api.linear.app/graphql", "https://api.linear.app:443/graphql",
		"http://127.0.0.1:8080/graphql", "http://127.0.0.2/graphql", "http://[::1]:9/graphql",
	} {
		rt := &linearTrip{}
		_, err := Client{URL: u, Key: linearKey, HTTP: &http.Client{Transport: rt}}.Issues(context.Background(), "")
		if err != nil || rt.hits.Load() != 1 {
			t.Errorf("%s: err %v, %d requests", u, err, rt.hits.Load())
		}
	}
}

func TestClientVerifiesTLS(t *testing.T) {
	var hits atomic.Int64
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(linearPage(1, 1, false, ""))
	}))
	s.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake is expected
	s.StartTLS()
	t.Cleanup(s.Close)
	_, err := Client{URL: s.URL + "/graphql", Key: linearKey}.Issues(context.Background(), "")
	if err == nil || hits.Load() != 0 {
		t.Fatalf("an untrusted certificate: err %v, %d requests answered", err, hits.Load())
	}
	if err.Error() != "linear: certificate not trusted" {
		t.Errorf("err %q", err)
	}
	linearCheckClass(t, "tls", err, ErrUnavailable)
	// With a client that trusts the test certificate, https on loopback works.
	got, err := Client{URL: s.URL + "/graphql", Key: linearKey, HTTP: s.Client()}.Issues(context.Background(), "")
	if err != nil || len(got) != 1 {
		t.Errorf("trusted: %d issues, err %v", len(got), err)
	}
}

func TestClientConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	_, err = Client{URL: "http://" + addr + "/graphql", Key: linearKey}.Issues(context.Background(), "")
	if err == nil || err.Error() != "linear: connection refused" {
		t.Errorf("err %v", err)
	}
	linearCheckClass(t, "refused", err, ErrUnavailable)
	linearCheckClean(t, "refused", err)
}

func TestDefaultClientIgnoresProxyEnvironment(t *testing.T) {
	tr, ok := defaultClient.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("the default transport may route through a proxy from the environment: %#v", defaultClient.Transport)
	}
	if ok && tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("the default transport skips TLS verification")
	}
	if defaultClient.CheckRedirect == nil {
		t.Error("the default client follows redirects")
	}
}

func TestValidKeyAndTeam(t *testing.T) {
	for k, want := range map[string]bool{
		"a": true, linearKey: true, "A.b-c_9": true, strings.Repeat("k", 200): true,
		"": false, "a b": false, "a\nb": false, "a:b": false, "é": false, strings.Repeat("k", 201): false,
		"Bearer x": false, "a\x00": false, "a/b": false, "a=": false,
	} {
		if ValidKey(k) != want {
			t.Errorf("ValidKey(%q) = %v", k, !want)
		}
	}
	for k, want := range map[string]bool{
		"E": true, "ENG": true, "E2E": true, "ABCDEFGHIJ": true, "A123456789": true,
		"": false, "eng": false, "Eng": false, "1ENG": false, "ENG-1": false, "ABCDEFGHIJK": false,
		"EN G": false, "ENG\n": false, "ÉNG": false,
	} {
		if ValidTeam(k) != want {
			t.Errorf("ValidTeam(%q) = %v", k, !want)
		}
	}
}

func TestDecodeIssueShapes(t *testing.T) {
	for _, c := range []struct {
		name, node string
		want       Issue
	}{
		{"not an object", `5`, Issue{Priority: -1}},
		{"null", `null`, Issue{Priority: -1}},
		{"empty object", `{}`, Issue{Priority: -1}},
		{
			"title of the wrong type", `{"identifier":"ENG-1","title":42,"priority":1,"state":{"type":"backlog"}}`,
			Issue{Identifier: "ENG-1", Priority: 1, StateType: "backlog"},
		},
		{"priority as a float", `{"identifier":"ENG-1","priority":2.0}`, Issue{Identifier: "ENG-1", Priority: 2}},
		{"priority fractional", `{"priority":2.5}`, Issue{Priority: -1}},
		{"priority null", `{"priority":null}`, Issue{Priority: -1}},
		{"priority out of range", `{"priority":5}`, Issue{Priority: -1}},
		{"priority negative", `{"priority":-1}`, Issue{Priority: -1}},
		{"priority a string", `{"priority":"1"}`, Issue{Priority: -1}},
		{"state not an object", `{"priority":0,"state":"backlog"}`, Issue{}},
		{
			"labels of mixed shape", `{"priority":0,"labels":{"nodes":[{"name":"A"},5,{"name":7},{},{"name":"B"}]}}`,
			Issue{Labels: []string{"A", "B"}},
		},
		{"labels not an object", `{"priority":0,"labels":["A"]}`, Issue{}},
		{"unknown fields", `{"priority":0,"identifier":"ENG-1","id":"uuid","estimate":3}`, Issue{Identifier: "ENG-1"}},
	} {
		if got := decodeIssue(json.RawMessage(c.node)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %#v, want %#v", c.name, got, c.want)
		}
	}
}

func TestClientKeepsBadNodesInPlace(t *testing.T) {
	c, _ := linearServe(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ []byte) {
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[5,` +
			`{"identifier":"ENG-1","title":"One","priority":1,"state":{"type":"backlog"}},` +
			`{"identifier":"ENG-2","title":["x"],"priority":1,"state":{"type":"backlog"}}],` +
			`"pageInfo":{"hasNextPage":false,"endCursor":null}}}}`))
	})
	got, err := c.Issues(context.Background(), "")
	if err != nil || len(got) != 3 {
		t.Fatalf("%d issues, err %v", len(got), err)
	}
	fields, invalid := Map(got)
	want := []Invalid{{Index: 0, Reason: reasonIdentifier}, {Index: 2, Reason: reasonTitle}}
	if len(fields) != 1 || fields[0].SourceRef != "ENG-1" || !reflect.DeepEqual(invalid, want) {
		t.Errorf("fields %#v, invalid %#v", fields, invalid)
	}
}

// linearIssue is a valid issue that each Map case changes.
func linearIssue(change func(*Issue)) Issue {
	i := Issue{
		Identifier: "ENG-42", Title: "Add retries", Description: "Retry three times.",
		URL: "https://linear.app/acme/issue/ENG-42/add-retries", StateType: "unstarted", Priority: 2,
		Labels: []string{"Backend"},
	}
	if change != nil {
		change(&i)
	}
	return i
}

func linearFields(change func(*Fields)) Fields {
	f := Fields{
		SourceRef: "ENG-42", Title: "Add retries",
		Body: "Linear ENG-42: https://linear.app/acme/issue/ENG-42/add-retries\n\nRetry three times.",
		Tags: []string{"backend"}, Priority: 1, Status: model.TaskBacklog,
	}
	if change != nil {
		change(&f)
	}
	return f
}

type linearMapCase struct {
	name   string
	issue  Issue
	want   Fields
	reason string // when not "", the issue is invalid for this reason
}

func linearMapCases() []linearMapCase {
	long := strings.Repeat("d", task.MaxBody)
	return []linearMapCase{
		{"valid", linearIssue(nil), linearFields(nil), ""},
		{"priority 0 is P2", linearIssue(func(i *Issue) { i.Priority = 0 }), linearFields(func(f *Fields) { f.Priority = 2 }), ""},
		{"priority 1 is P1", linearIssue(func(i *Issue) { i.Priority = 1 }), linearFields(nil), ""},
		{"priority 3 is P2", linearIssue(func(i *Issue) { i.Priority = 3 }), linearFields(func(f *Fields) { f.Priority = 2 }), ""},
		{"priority 4 is P3", linearIssue(func(i *Issue) { i.Priority = 4 }), linearFields(func(f *Fields) { f.Priority = 3 }), ""},
		{"priority -1", linearIssue(func(i *Issue) { i.Priority = -1 }), Fields{}, reasonPriority},
		{"priority 5", linearIssue(func(i *Issue) { i.Priority = 5 }), Fields{}, reasonPriority},
		{"backlog state", linearIssue(func(i *Issue) { i.StateType = "backlog" }), linearFields(nil), ""},
		{"started state", linearIssue(func(i *Issue) { i.StateType = "started" }), Fields{}, reasonState},
		{"completed state", linearIssue(func(i *Issue) { i.StateType = "completed" }), Fields{}, reasonState},
		{"state in capitals", linearIssue(func(i *Issue) { i.StateType = "Backlog" }), Fields{}, reasonState},
		{"no state", linearIssue(func(i *Issue) { i.StateType = "" }), Fields{}, reasonState},
		{"no identifier", linearIssue(func(i *Issue) { i.Identifier = "" }), Fields{}, reasonIdentifier},
		{"lowercase identifier", linearIssue(func(i *Issue) { i.Identifier = "eng-42" }), Fields{}, reasonIdentifier},
		{"identifier with a newline", linearIssue(func(i *Issue) { i.Identifier = "ENG-42\n" }), Fields{}, reasonIdentifier},
		{"identifier number of 8 digits", linearIssue(func(i *Issue) { i.Identifier = "ENG-12345678" }), Fields{}, reasonIdentifier},
		{"identifier team of 11", linearIssue(func(i *Issue) { i.Identifier = "ABCDEFGHIJK-1" }), Fields{}, reasonIdentifier},
		{
			"longest identifier", linearIssue(func(i *Issue) { i.Identifier = "A123456789-1234567"; i.URL = "" }),
			linearFields(func(f *Fields) {
				f.SourceRef = "A123456789-1234567"
				f.Body = "Linear A123456789-1234567\n\nRetry three times."
			}), "",
		},
		{"empty title", linearIssue(func(i *Issue) { i.Title = "" }), Fields{}, reasonTitle},
		{"title of escapes only", linearIssue(func(i *Issue) { i.Title = "\x1b[2J\a \t" }), Fields{}, reasonTitle},
		{
			"title cleaned", linearIssue(func(i *Issue) { i.Title = "Fix\x1b[31m it\nnow\u202e" }),
			linearFields(func(f *Fields) { f.Title = "Fix it now" }), "",
		},
		{
			"title cut to 120", linearIssue(func(i *Issue) { i.Title = strings.Repeat("t", 300) }),
			linearFields(func(f *Fields) { f.Title = strings.Repeat("t", 119) + "…" }), "",
		},
		{
			"no description", linearIssue(func(i *Issue) { i.Description = "" }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42: https://linear.app/acme/issue/ENG-42/add-retries" }), "",
		},
		{
			"description cleaned", linearIssue(func(i *Issue) { i.Description = "a\x1b]8;;https://evil/\aclick\x1b]8;;\a\r\nb\x00" }),
			linearFields(func(f *Fields) {
				f.Body = "Linear ENG-42: https://linear.app/acme/issue/ENG-42/add-retries\n\naclick\nb"
			}), "",
		},
		{
			"description cut to the body limit", linearIssue(func(i *Issue) { i.Description = long + "tail" }),
			linearFields(func(f *Fields) {
				f.Body = ("Linear ENG-42: https://linear.app/acme/issue/ENG-42/add-retries\n\n" + long)[:task.MaxBody]
			}), "",
		},
		{
			"no URL", linearIssue(func(i *Issue) { i.URL = "" }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42\n\nRetry three times." }), "",
		},
		{
			"URL on another host", linearIssue(func(i *Issue) { i.URL = "https://linear.app.evil.example/x" }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42\n\nRetry three times." }), "",
		},
		{
			"plain http URL", linearIssue(func(i *Issue) { i.URL = "http://linear.app/acme/issue/ENG-42" }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42\n\nRetry three times." }), "",
		},
		{
			"URL with a query", linearIssue(func(i *Issue) { i.URL = "https://linear.app/acme?x=1" }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42\n\nRetry three times." }), "",
		},
		{
			"URL with a newline", linearIssue(func(i *Issue) { i.URL = "https://linear.app/acme\nInjected" }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42\n\nRetry three times." }), "",
		},
		{
			"URL with user info", linearIssue(func(i *Issue) { i.URL = "https://linear.app@evil.example/x" }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42\n\nRetry three times." }), "",
		},
		{
			"URL too long", linearIssue(func(i *Issue) { i.URL = "https://linear.app/" + strings.Repeat("a", 201) }),
			linearFields(func(f *Fields) { f.Body = "Linear ENG-42\n\nRetry three times." }), "",
		},
		{"no labels", linearIssue(func(i *Issue) { i.Labels = nil }), linearFields(func(f *Fields) { f.Tags = nil }), ""},
		{"labels made tags", linearIssue(func(i *Issue) {
			i.Labels = []string{"Needs Review!", "CI/CD pipeline", "--x--", "a_b", "Ünïcode", "\x1b[31mred\x1b[0m", "", "!!!"}
		}), linearFields(func(f *Fields) {
			f.Tags = []string{"needs-review", "ci-cd-pipeline", "x", "a_b", "n-code", "red"}
		}), ""},
		{
			"duplicate labels", linearIssue(func(i *Issue) { i.Labels = []string{"Infra", "infra", "INFRA!"} }),
			linearFields(func(f *Fields) { f.Tags = []string{"infra"} }), "",
		},
		{"tag rule", linearIssue(func(i *Issue) {
			i.Labels = []string{"_under", strings.Repeat("x", 32), strings.Repeat("y", 33), strings.Repeat("z", 100)}
		}), linearFields(func(f *Fields) { f.Tags = []string{strings.Repeat("x", 32)} }), ""},
		{"at most 8 tags", linearIssue(func(i *Issue) {
			i.Labels = []string{"a", "b", "!", "c", "d", "e", "f", "g", "h", "i", "j"}
		}), linearFields(func(f *Fields) { f.Tags = []string{"a", "b", "c", "d", "e", "f", "g", "h"} }), ""},
	}
}

func TestMapIssue(t *testing.T) {
	for _, c := range linearMapCases() {
		fields, invalid := Map([]Issue{c.issue})
		if c.reason != "" {
			if want := []Invalid{{Index: 0, Reason: c.reason}}; len(fields) != 0 || !reflect.DeepEqual(invalid, want) {
				t.Errorf("%s: fields %#v, invalid %#v; want invalid %q", c.name, fields, invalid, c.reason)
			}
			continue
		}
		if len(invalid) != 0 || len(fields) != 1 || !reflect.DeepEqual(fields[0], c.want) {
			t.Errorf("%s:\n got %#v (invalid %#v)\nwant %#v", c.name, fields, invalid, c.want)
		}
	}
}

func TestMapReportsIndexesAndDuplicates(t *testing.T) {
	issues := []Issue{
		linearIssue(nil),
		linearIssue(func(i *Issue) { i.Identifier = "bad" }),
		linearIssue(func(i *Issue) { i.Identifier = "ENG-43" }),
		linearIssue(func(i *Issue) { i.Title = "The same issue again, from a later page" }),
		{},
	}
	fields, invalid := Map(issues)
	want := []Invalid{
		{Index: 1, Reason: reasonIdentifier},
		{Index: 3, Reason: "same identifier as issue 0"},
		{Index: 4, Reason: reasonIdentifier},
	}
	if !reflect.DeepEqual(invalid, want) {
		t.Errorf("invalid %#v, want %#v", invalid, want)
	}
	if len(fields) != 2 || fields[0].SourceRef != "ENG-42" || fields[1].SourceRef != "ENG-43" {
		t.Errorf("fields %#v", fields)
	}
	for _, iv := range invalid {
		if strings.Contains(iv.Reason, "bad") {
			t.Errorf("reason %q quotes the issue", iv.Reason)
		}
	}
	if f, iv := Map(nil); f != nil || iv != nil {
		t.Errorf("Map(nil) = %v, %v", f, iv)
	}
}

func TestSourceIsLinear(t *testing.T) {
	if Source != model.TaskSource("linear") {
		t.Errorf("Source %q", Source)
	}
}

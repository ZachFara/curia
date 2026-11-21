// Package linear is Curia's read-only client for the Linear issue tracker
// (plan section 4.9): one fixed GraphQL query, sent to one configured
// endpoint with the API key in the Authorization header and nowhere else,
// and the pure mapping of an issue to a task's fields. Everything in a
// response is untrusted.
package linear

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/task"
)

// Query is the only GraphQL document this package sends.
const Query = `query($first: Int!, $after: String, $filter: IssueFilter) {
  issues(first: $first, after: $after, filter: $filter, orderBy: updatedAt) {
    nodes { identifier title description priority url state { type } labels { nodes { name } } }
    pageInfo { hasNextPage endCursor }
  }
}`

// Limits of one Issues call.
const (
	// PageSize is the number of issues asked for per request.
	PageSize = 50
	// MaxPages is the most requests one Issues call sends.
	MaxPages = 4
	// MaxResponse is the most bytes read of one response; one more is an error.
	MaxResponse = 4 << 20
	// RequestLimit bounds each request when ctx has no earlier deadline.
	RequestLimit = 20 * time.Second
)

// Source is the task source of an imported Linear issue.
const Source model.TaskSource = "linear"

const (
	maxRetryAfter = time.Hour
	maxMessage    = 200     // runes of a GraphQL error message in an error
	maxRawMessage = 4 << 10 // runes of a message cleaned before the key is replaced
	maxCursor     = 512     // bytes of a page cursor
	maxReply      = 4 << 10 // bytes of a non-200 reply drained before closing
	maxLabelRunes = 64
	userAgent     = "curia"
	keyMark       = "<key>"
	graphQLPath   = "/graphql"
)

var (
	keyRe        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)
	teamRe       = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)
	identifierRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-[0-9]{1,7}$`)
	issueURLRe   = regexp.MustCompile(`^https://linear\.app/[A-Za-z0-9._/-]{1,200}$`)
	tagRe        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`) // task's tag rule
	tagRuns      = regexp.MustCompile(`[^a-z0-9_-]+`)
	// states are the state types the filter asks for, in the filter's order.
	states = []string{"backlog", "unstarted"}
	// priorities maps Linear's priority (0 none, 1 urgent, 2 high, 3
	// medium, 4 low) to Curia's P1 to P3.
	priorities = map[int]int{0: 2, 1: 1, 2: 1, 3: 2, 4: 3}
)

// Issue is one Linear issue, as received: every string is untrusted.
// Priority is Linear's 0 to 4, or -1 when the response's value was missing
// or not one of those; a field of the wrong type is read as missing.
type Issue struct {
	Identifier, Title, Description, URL, StateType string
	Priority                                       int
	Labels                                         []string
}

// ValidKey reports whether k can be a Linear API key: 1 to 200 bytes of [A-Za-z0-9_.-].
func ValidKey(k string) bool { return keyRe.MatchString(k) }

// ValidTeam reports whether k is a team key: ^[A-Z][A-Z0-9]{0,9}$.
func ValidTeam(k string) bool { return teamRe.MatchString(k) }

// The classes of a failed Issues call: every error it returns matches
// exactly one of them with errors.Is.
var (
	// ErrInvalid is wrapped when nothing was sent: the key, the team or the
	// endpoint URL is of the wrong shape.
	ErrInvalid = errors.New("linear: request not sent")
	// ErrUnauthorized is wrapped for 401 and 403: the key is wrong or lacks access.
	ErrUnauthorized = errors.New("linear: the API key was refused")
	// ErrRateLimited is wrapped for 429; the *StatusError carries Retry-After.
	ErrRateLimited = errors.New("linear: rate limit reached")
	// ErrRejected is wrapped for any other 4xx and for a GraphQL error.
	ErrRejected = errors.New("linear: request rejected")
	// ErrUnavailable is wrapped for a transport failure, a timeout, a 3xx
	// (redirects are not followed), a 5xx and any other status.
	ErrUnavailable = errors.New("linear: unreachable or failing")
	// ErrBadResponse is wrapped for a 200 whose body is too large or not the
	// expected shape; nothing of such a response is returned.
	ErrBadResponse = errors.New("linear: response not understood")
)

// StatusError is a reply other than 200. Its text names only the status.
type StatusError struct {
	Code int
	// RetryAfter is a 429's Retry-After in whole seconds, at most an hour;
	// 0 when absent, not a number of seconds, or not a 429.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string { return fmt.Sprintf("linear: status %d", e.Code) }

// Is matches the error's class.
func (e *StatusError) Is(target error) bool {
	switch {
	case e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden:
		return target == ErrUnauthorized
	case e.Code == http.StatusTooManyRequests:
		return target == ErrRateLimited
	case e.Code >= 400 && e.Code < 500:
		return target == ErrRejected
	default:
		return target == ErrUnavailable
	}
}

// invalidError names what was wrong, never its value.
type invalidError struct{ what string }

func (e invalidError) Error() string { return "linear: " + e.what }

func (e invalidError) Is(target error) bool { return target == ErrInvalid }

// remoteError holds the first GraphQL error message, cleaned and with the
// key replaced.
type remoteError struct{ msg string }

func (e remoteError) Error() string { return "linear: " + e.msg }

func (e remoteError) Is(target error) bool { return target == ErrRejected }

// badResponse is a 200 that cannot be used.
type badResponse struct{ what string }

func (e badResponse) Error() string { return "linear: " + e.what }

func (e badResponse) Is(target error) bool { return target == ErrBadResponse }

var notUnderstood = badResponse{"response not understood"}

// sendError is a failed request. Its text names only the kind of failure:
// the transport's own error, which quotes the URL, is reachable through
// Unwrap but is never part of the text.
type sendError struct {
	kind string
	err  error
}

func (e sendError) Error() string { return "linear: " + e.kind }

func (e sendError) Is(target error) bool { return target == ErrUnavailable }

func (e sendError) Unwrap() error { return e.err }

// scrubbed is an error whose text held the key: the text with the key
// replaced, and the class of the original, which is not reachable.
type scrubbed struct {
	text string
	err  error
}

func (e scrubbed) Error() string { return e.text }

func (e scrubbed) Is(target error) bool { return errors.Is(e.err, target) }

// guard is the last check on every error Issues returns: an error whose
// text holds the key comes back as that text with each occurrence replaced
// by "<key>", and does not unwrap to the original.
func guard(err error, key string) error {
	if err == nil || key == "" || !strings.Contains(err.Error(), key) {
		return err
	}
	return scrubbed{strings.ReplaceAll(err.Error(), key, keyMark), err}
}

// Client reads issues from one Linear endpoint. It never writes.
type Client struct {
	URL  string       // the GraphQL endpoint
	Key  string       // sent as the Authorization header and nowhere else
	HTTP *http.Client // nil: a client that follows no redirect
}

// defaultClient follows no redirect, takes no proxy from the environment
// and verifies TLS as the standard transport does: the request goes only to
// the configured URL.
var defaultClient = &http.Client{Transport: directTransport(), CheckRedirect: noRedirect}

func directTransport() *http.Transport {
	tr := &http.Transport{}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = def.Clone() // keeps the standard dial, TLS and idle timeouts
	}
	tr.Proxy = nil
	return tr
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// client is the caller's client with redirects turned off (on a copy), or
// defaultClient.
func (c Client) client() *http.Client {
	if c.HTTP == nil {
		return defaultClient
	}
	cp := *c.HTTP
	cp.CheckRedirect = noRedirect
	return &cp
}

// Issues returns the issues assigned to the key's user in backlog or unstarted states, of team when it is not empty, newest update first, at most MaxPages pages.
// It sends one request per page, each bounded by RequestLimit (or ctx's
// earlier deadline), and never retries. Any failure, on any page, returns
// no issues. A node of the wrong shape stays in its place as an Issue with
// the fields it could read; Map reports it. With an invalid key, team or
// URL nothing is sent. No error text holds the URL, a header or the key.
func (c Client) Issues(ctx context.Context, team string) ([]Issue, error) {
	issues, err := c.issues(ctx, team)
	if err != nil {
		return nil, guard(err, c.Key)
	}
	return issues, nil
}

func (c Client) issues(ctx context.Context, team string) ([]Issue, error) {
	if !ValidKey(c.Key) {
		return nil, invalidError{"the API key must be 1 to 200 bytes of letters, digits, '_', '.' and '-'"}
	}
	if team != "" && !ValidTeam(team) {
		return nil, invalidError{"the team key must be a capital letter and up to 9 capitals or digits"}
	}
	target, err := endpoint(c.URL)
	if err != nil {
		return nil, err
	}
	var out []Issue
	var after *string
	for range MaxPages {
		p, err := c.fetch(ctx, target, team, after)
		if err != nil {
			return nil, err
		}
		out = append(out, p.issues...)
		if !p.next {
			break
		}
		after = &p.cursor
	}
	return out, nil
}

// endpoint is the request URL for a configured endpoint: https to any host,
// or plain http to a loopback address, with the path /graphql and no user
// info, query or fragment.
func endpoint(raw string) (string, error) {
	bad := invalidError{"the URL must be https://<host>/graphql (plain http only to a loopback address), with no user info, query or fragment"}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		u.Path != graphQLPath || u.RawPath != "" {
		return "", bad
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return "", bad
	}
	return u.Scheme + "://" + u.Host + graphQLPath, nil
}

// page is one response's issues and where the next page starts.
type page struct {
	issues []Issue
	next   bool
	cursor string
}

func (c Client) fetch(ctx context.Context, target, team string, after *string) (page, error) {
	body, err := json.Marshal(newRequest(team, after))
	if err != nil {
		return page{}, invalidError{"the request does not encode"}
	}
	ctx, cancel := context.WithTimeout(ctx, RequestLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return page{}, invalidError{"the URL does not parse"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", c.Key)
	resp, err := c.client().Do(req)
	if err != nil {
		return page{}, sendError{transportKind(err), unwrapURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReply))
		se := &StatusError{Code: resp.StatusCode}
		if se.Code == http.StatusTooManyRequests {
			se.RetryAfter = retryAfter(resp.Header.Get("Retry-After"))
		}
		return page{}, se
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return page{}, sendError{transportKind(err), err}
	}
	if len(data) > MaxResponse {
		return page{}, badResponse{"response too large"}
	}
	return decodePage(data, c.Key)
}

// retryAfter reads a Retry-After of whole seconds, bounded by an hour. An
// HTTP date or anything else is 0.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" || strings.Trim(v, "0123456789") != "" {
		return 0
	}
	if len(v) > 6 { // more than 999999 s is past the bound anyway
		return maxRetryAfter
	}
	n, _ := strconv.Atoi(v)
	return min(time.Duration(n)*time.Second, maxRetryAfter)
}

// transportKind names a transport failure without quoting it.
func transportKind(err error) string {
	var dns *net.DNSError
	var op *net.OpError
	var cert *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dns):
		return "host not found"
	case errors.As(err, &cert):
		return "certificate not trusted"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &op):
		return "connection failed (" + op.Op + ")"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	default:
		return "transport error"
	}
}

// unwrapURL drops the *url.Error layer, whose text holds the URL.
func unwrapURL(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// The wire shape, from section 4.9 and Linear's published API; unverified
// until probe L1 records a real response (testdata/issues-captured.json).
// This is the one place the request and response shapes live.

type wireRequest struct {
	Query     string        `json:"query"`
	Variables wireVariables `json:"variables"`
}

type wireVariables struct {
	First  int        `json:"first"`
	After  *string    `json:"after"` // null for the first page
	Filter wireFilter `json:"filter"`
}

type wireFilter struct {
	Assignee struct {
		IsMe struct {
			Eq bool `json:"eq"`
		} `json:"isMe"`
	} `json:"assignee"`
	State struct {
		Type struct {
			In []string `json:"in"`
		} `json:"type"`
	} `json:"state"`
	Team *wireTeam `json:"team,omitempty"`
}

type wireTeam struct {
	Key struct {
		Eq string `json:"eq"`
	} `json:"key"`
}

// newRequest is the body for one page: the query and its variables, the
// team (already checked) and the cursor going in only as variables.
func newRequest(team string, after *string) wireRequest {
	var f wireFilter
	f.Assignee.IsMe.Eq = true
	f.State.Type.In = states
	if team != "" {
		f.Team = &wireTeam{}
		f.Team.Key.Eq = team
	}
	return wireRequest{Query: Query, Variables: wireVariables{First: PageSize, After: after, Filter: f}}
}

type wireErrors struct {
	Errors []json.RawMessage `json:"errors"`
}

type wireData struct {
	Data *struct {
		Issues *struct {
			Nodes    *[]json.RawMessage `json:"nodes"`
			PageInfo *struct {
				HasNextPage *bool   `json:"hasNextPage"`
				EndCursor   *string `json:"endCursor"`
			} `json:"pageInfo"`
		} `json:"issues"`
	} `json:"data"`
}

// wireIssue reads each field on its own, so one of the wrong type is lost
// alone and the issue keeps its place.
type wireIssue struct {
	Identifier  json.RawMessage `json:"identifier"`
	Title       json.RawMessage `json:"title"`
	Description json.RawMessage `json:"description"`
	Priority    json.RawMessage `json:"priority"`
	URL         json.RawMessage `json:"url"`
	State       json.RawMessage `json:"state"`
	Labels      json.RawMessage `json:"labels"`
}

// decodePage reads one 200 response. A GraphQL error, even beside data, is
// an error holding its first message.
func decodePage(data []byte, key string) (page, error) {
	var e wireErrors
	if json.Unmarshal(data, &e) != nil {
		return page{}, notUnderstood
	}
	if len(e.Errors) > 0 {
		return page{}, remoteError{errorMessage(e.Errors[0], key)}
	}
	var d wireData
	if json.Unmarshal(data, &d) != nil || d.Data == nil || d.Data.Issues == nil ||
		d.Data.Issues.Nodes == nil || d.Data.Issues.PageInfo == nil || d.Data.Issues.PageInfo.HasNextPage == nil {
		return page{}, notUnderstood
	}
	p := page{next: *d.Data.Issues.PageInfo.HasNextPage}
	if p.next {
		c := d.Data.Issues.PageInfo.EndCursor
		if c == nil || !validCursor(*c) {
			return page{}, notUnderstood
		}
		p.cursor = *c
	}
	for _, raw := range *d.Data.Issues.Nodes {
		p.issues = append(p.issues, decodeIssue(raw))
	}
	return p, nil
}

// errorMessage is a GraphQL error's message, cleaned, with the key
// replaced before it is cut to maxMessage runes, so the cut cannot leave a
// piece of the key.
func errorMessage(raw json.RawMessage, key string) string {
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &m) // of the wrong shape: no message
	msg := strings.ReplaceAll(sanitize.Line(m.Message, maxRawMessage), key, keyMark)
	if msg = sanitize.Line(msg, maxMessage); msg == "" {
		return "the server reported an error"
	}
	return msg
}

// validCursor accepts 1 to maxCursor bytes of printable ASCII: the cursor
// goes back to the server only as a GraphQL variable.
func validCursor(c string) bool {
	if c == "" || len(c) > maxCursor {
		return false
	}
	for i := range len(c) {
		if c[i] < 0x21 || c[i] > 0x7e {
			return false
		}
	}
	return true
}

func decodeIssue(raw json.RawMessage) Issue {
	var w wireIssue
	if json.Unmarshal(raw, &w) != nil {
		return Issue{Priority: -1}
	}
	var state struct {
		Type json.RawMessage `json:"type"`
	}
	_ = json.Unmarshal(w.State, &state) // of the wrong shape: no state
	return Issue{
		Identifier: text(w.Identifier), Title: text(w.Title), Description: text(w.Description),
		URL: text(w.URL), StateType: text(state.Type), Priority: priority(w.Priority), Labels: labels(w.Labels),
	}
}

// text is a JSON string's value, "" for null, a missing field or another type.
func text(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s) // another type: ""
	return s
}

// priority is a whole number from 0 to 4 (Linear sends a Float), else -1.
func priority(raw json.RawMessage) int {
	var f *float64
	if json.Unmarshal(raw, &f) != nil || f == nil || *f != math.Trunc(*f) || *f < 0 || *f > 4 {
		return -1
	}
	return int(*f)
}

// labels are the label names that are non-empty strings, in order.
func labels(raw json.RawMessage) []string {
	var l struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	_ = json.Unmarshal(raw, &l) // of the wrong shape: no labels
	var out []string
	for _, n := range l.Nodes {
		var label struct {
			Name json.RawMessage `json:"name"`
		}
		_ = json.Unmarshal(n, &label) // of the wrong shape: skipped
		if name := text(label.Name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Fields is what one issue gives a task (section 4.9's mapping). The
// caller adds the repository and base branch, and stores it with source
// Source and source ref SourceRef.
type Fields struct {
	SourceRef, Title, Body string
	Tags                   []string
	Priority               int              // 1, 2 or 3: P1, P2 or P3
	Status                 model.TaskStatus // always backlog: Linear's state is never mapped
}

// Invalid is an issue Map did not turn into Fields: its index in the list
// and why, in fixed words that quote nothing from the issue.
type Invalid struct {
	Index  int
	Reason string
}

// The reasons of Invalid, besides "same identifier as issue <index>".
const (
	reasonIdentifier = "identifier missing or not of the form ABC-123"
	reasonState      = "state is not backlog or unstarted"
	reasonPriority   = "priority is not 0 to 4"
	reasonTitle      = "title is empty"
)

// Map turns issues into task fields. An issue with a malformed identifier,
// a state the filter did not ask for, a priority outside 0 to 4, an empty
// title, or the identifier of an earlier issue is listed in invalid
// instead. It is pure.
func Map(issues []Issue) (fields []Fields, invalid []Invalid) {
	first := map[string]int{}
	for i, is := range issues {
		f, reason := mapIssue(is)
		if at, dup := first[f.SourceRef]; reason == "" && dup {
			reason = fmt.Sprintf("same identifier as issue %d", at)
		}
		if reason != "" {
			invalid = append(invalid, Invalid{Index: i, Reason: reason})
			continue
		}
		first[f.SourceRef] = i
		fields = append(fields, f)
	}
	return fields, invalid
}

func mapIssue(is Issue) (Fields, string) {
	if !identifierRe.MatchString(is.Identifier) {
		return Fields{}, reasonIdentifier
	}
	if !slices.Contains(states, is.StateType) {
		return Fields{}, reasonState
	}
	p, ok := priorities[is.Priority]
	if !ok {
		return Fields{}, reasonPriority
	}
	title := sanitize.Line(is.Title, task.MaxTitle)
	if title == "" {
		return Fields{}, reasonTitle
	}
	return Fields{
		SourceRef: is.Identifier, Title: title, Body: body(is), Tags: tags(is.Labels),
		Priority: p, Status: model.TaskBacklog,
	}, ""
}

// body is "Linear <identifier>: <url>" (the URL only when it is an https
// link on linear.app), a blank line and the cleaned description, cut to
// task.MaxBody bytes. The URL is text in the body: it is never fetched.
func body(is Issue) string {
	head := "Linear " + is.Identifier
	if issueURLRe.MatchString(is.URL) {
		head += ": " + is.URL
	}
	return sanitize.Block(head+"\n\n"+is.Description, task.MaxBody)
}

// tags are the label names lowercased, each run outside a-z, 0-9, _ and -
// made one "-", trimmed of "-": the first task.MaxTags distinct ones that
// then match the task's tag rule.
func tags(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		t := strings.Trim(tagRuns.ReplaceAllString(strings.ToLower(sanitize.Line(n, maxLabelRunes)), "-"), "-")
		if !tagRe.MatchString(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == task.MaxTags {
			break
		}
	}
	return out
}

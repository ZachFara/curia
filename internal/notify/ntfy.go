package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Limits of a push (plan section 4.7).
const (
	MaxActions   = 3    // ntfy's limit of buttons on one message
	MaxPushText  = 1200 // runes of a message
	MaxPushTitle = 80   // runes of a title
	MaxPushLabel = 30   // runes of a button label
	PublishLimit = 5 * time.Second
)

const (
	priorityTicket  = 4
	priorityDefault = 3
	maxReviewText   = 600 // runes of a review summary in a push
	maxAccessToken  = 512 // bytes of CURIA_NTFY_TOKEN
	maxReply        = 4 << 10
	tokenBytes      = 32
	userAgent       = "curia"
)

var (
	pushTopic  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	pushToken  = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	recognized = map[model.TestsStatus]bool{model.TestsPassed: true, model.TestsFailed: true, model.TestsNotRun: true}
)

// Action is one button: the phone POSTs Body to URL with "Authorization: Bearer <Token>".
type Action struct{ Label, URL, Token, Body string }

// Notification is one ntfy message, ready to publish.
type Notification struct {
	Topic, Title, Message string
	Priority              int
	Actions               []Action
}

// NotBefore is when a push made at now may go: now outside the quiet window
// (or with none), else the window's end, in now's location. The window is
// "HH:MM-HH:MM" in local time, start inclusive and end exclusive, across
// midnight when the start is later than the end.
func NotBefore(now time.Time, quiet string) time.Time {
	start, end, ok := config.ParseQuiet(quiet)
	if !ok {
		return now
	}
	m := now.Hour()*60 + now.Minute()
	inside := start <= m && m < end
	if start > end {
		inside = m >= start || m < end
	}
	if !inside {
		return now
	}
	day := now.Day()
	if m >= end { // before midnight in a window that crosses it
		day++
	}
	return time.Date(now.Year(), now.Month(), day, end/60, end%60, 0, 0, now.Location())
}

// DecisionPush is the push for blocking ticket d. answerURL and token are "" for a push without buttons.
// Only the id, repo base, question, option keys and labels and the
// recommendation are pushed; each is cleaned, and each option is its own
// line, so ticket text cannot forge a line of the message.
func DecisionPush(d model.Decision, topic, answerURL, token string) Notification {
	lines := []string{sanitize.Line(d.Question, MaxPushText)}
	for _, o := range d.Options {
		lines = append(lines, sanitize.Line(o.Key+": "+o.Label, MaxPushText))
	}
	if d.Recommendation != "" {
		lines = append(lines, "Recommended: "+sanitize.Line(d.Recommendation, MaxPushLabel))
	}
	irreversible := d.Reversibility == model.ReversIrreversible
	switch {
	case irreversible:
		lines = append(lines, "Irreversible: answer with curia decide "+d.ID+".")
	case len(d.Options) > MaxActions:
		lines = append(lines, "More options: curia decide "+d.ID+".")
	}
	n := Notification{
		Topic:    topic,
		Title:    sanitize.Line(d.ID+repoPart(d.Repo)+": decision needed", MaxPushTitle),
		Message:  sanitize.Text(strings.Join(lines, "\n"), MaxPushText),
		Priority: priorityTicket,
	}
	if answerURL != "" && token != "" && !irreversible {
		n.Actions = buttons(d, answerURL, token)
	}
	return n
}

// buttons are at most MaxActions answers to d, the recommended option first
// and then the others in ticket order, all with the same token.
func buttons(d model.Decision, answerURL, token string) []Action {
	ordered := make([]model.Option, 0, len(d.Options))
	for _, o := range d.Options {
		if o.Key == d.Recommendation {
			ordered = append(ordered, o)
		}
	}
	for _, o := range d.Options {
		if o.Key != d.Recommendation {
			ordered = append(ordered, o)
		}
	}
	if len(ordered) > MaxActions {
		ordered = ordered[:MaxActions]
	}
	out := make([]Action, 0, len(ordered))
	for _, o := range ordered {
		body, _ := json.Marshal(struct { // two strings: cannot fail
			Decision string `json:"decision"`
			Key      string `json:"key"`
		}{d.ID, o.Key})
		out = append(out, Action{
			Label: sanitize.Line(o.Key+": "+o.Label, MaxPushLabel),
			URL:   answerURL, Token: token, Body: string(body),
		})
	}
	return out
}

// ReviewPush is the push for a new review of t: the task title, the agent's
// test report and the summary cut to 600 runes. It has no buttons.
func ReviewPush(r model.Review, t model.Task, topic string) Notification {
	tests := "unknown"
	if recognized[r.Tests] {
		tests = string(r.Tests)
	}
	lines := []string{
		sanitize.Line(t.Title, MaxPushText),
		"Tests (the agent's report): " + tests,
		sanitize.Text(r.Summary, maxReviewText),
	}
	return Notification{
		Topic:    topic,
		Title:    sanitize.Line(r.ID+" "+r.TaskID+": ready for review", MaxPushTitle),
		Message:  sanitize.Text(strings.Join(lines, "\n"), MaxPushText),
		Priority: priorityDefault,
	}
}

// NewToken returns a one-time token (43 characters of base64url) and its hash.
func NewToken(random io.Reader) (plain, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", "", fmt.Errorf("notify: reading a random token: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(b)
	return plain, HashToken(plain), nil
}

// HashToken is the lowercase hex SHA-256 of plain.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ErrRejected is wrapped when ntfy answered 4xx other than 429: sending the same message again cannot help.
var ErrRejected = errors.New("notify: ntfy rejected the message")

// ErrInvalid is wrapped when Publish refused to send a notification or to
// use a Publisher field (a control rune in a title, a label, a token or the
// access token, a length over its limit, a topic or URL of the wrong shape).
// Such an error wraps ErrRejected too: sending it again cannot help.
var ErrInvalid = errors.New("notify: notification not sent")

// invalidError names what was wrong, never its value.
type invalidError struct{ what string }

func (e invalidError) Error() string { return ErrInvalid.Error() + ": " + e.what }

func (e invalidError) Is(target error) bool { return target == ErrInvalid || target == ErrRejected }

// sendError is a failed request. Its text names only the kind of failure:
// the transport's own error, which quotes the URL, is reachable through
// Unwrap but is never part of the text.
type sendError struct {
	kind string
	err  error
}

func (e sendError) Error() string { return "notify: ntfy request failed: " + e.kind }

func (e sendError) Unwrap() error { return e.err }

// Publisher sends notifications to one ntfy server.
type Publisher struct {
	URL         string       // the server's base URL
	AccessToken string       // "" for none
	Client      *http.Client // nil: a client that follows no redirect
}

// defaultClient follows no redirect and takes no proxy from the environment:
// the request goes only to the configured URL.
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

// Publish sends n in one attempt bounded by PublishLimit (or ctx's earlier
// deadline). It returns nil for 2xx, an error wrapping ErrRejected for a 4xx
// other than 429 or a notification it refuses to send (ErrInvalid), and a
// plain error otherwise; no error text holds the URL, a token or the body.
// The reply is read up to 4 KiB and discarded. It never retries.
func (p Publisher) Publish(ctx context.Context, n Notification) error {
	body, err := encode(p, n)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, PublishLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return invalidError{"the server URL does not parse"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if p.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.AccessToken)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return sendError{transportKind(err), unwrapURL(err)}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReply))
	_ = resp.Body.Close()
	return statusError(resp.StatusCode)
}

// client is the caller's client with redirects turned off, or defaultClient.
func (p Publisher) client() *http.Client {
	if p.Client == nil {
		return defaultClient
	}
	c := *p.Client
	c.CheckRedirect = noRedirect
	return &c
}

func statusError(code int) error {
	switch {
	case code >= 200 && code < 300:
		return nil
	case code >= 300 && code < 400:
		return fmt.Errorf("notify: ntfy answered status %d, a redirect, which is not followed", code)
	case code >= 400 && code < 500 && code != http.StatusTooManyRequests:
		return fmt.Errorf("%w (status %d)", ErrRejected, code)
	default:
		return fmt.Errorf("notify: ntfy answered status %d", code)
	}
}

// transportKind names a transport failure without quoting it.
func transportKind(err error) string {
	var dns *net.DNSError
	var op *net.OpError
	var cert *tls.CertificateVerificationError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dns):
		return "host not found"
	case errors.As(err, &cert):
		return "certificate not trusted"
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

// The request body as ntfy documents JSON publishing. This is the one place
// the wire shape lives; probe N1 (held) will confirm it against a real server.
type wireAction struct {
	Action  string            `json:"action"`
	Label   string            `json:"label"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Clear   bool              `json:"clear"`
}

type wireMessage struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title"`
	Message  string       `json:"message"`
	Priority int          `json:"priority"`
	Actions  []wireAction `json:"actions,omitempty"`
}

// encode checks p and n and returns the JSON body, HTML escaping off.
func encode(p Publisher, n Notification) ([]byte, error) {
	if err := checkPublisher(p); err != nil {
		return nil, err
	}
	if err := checkNotification(n); err != nil {
		return nil, err
	}
	w := wireMessage{Topic: n.Topic, Title: n.Title, Message: n.Message, Priority: n.Priority}
	if w.Priority == 0 {
		w.Priority = priorityDefault
	}
	for _, a := range n.Actions {
		w.Actions = append(w.Actions, wireAction{
			Action: "http", Label: a.Label, URL: a.URL, Method: http.MethodPost,
			Headers: map[string]string{"Authorization": "Bearer " + a.Token, "Content-Type": "application/json"},
			Body:    a.Body, Clear: true,
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(w); err != nil {
		return nil, invalidError{"the body does not encode"}
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func checkPublisher(p Publisher) error {
	if !httpURL(p.URL) {
		return invalidError{"the server URL must be http or https with a host and no user info"}
	}
	if len(p.AccessToken) > maxAccessToken || !oneLine(p.AccessToken) {
		return invalidError{"the access token is too long or holds a control character"}
	}
	return nil
}

func checkNotification(n Notification) error {
	switch {
	case !pushTopic.MatchString(n.Topic):
		return invalidError{"the topic must match " + pushTopic.String()}
	case !oneLine(n.Title) || utf8.RuneCountInString(n.Title) > MaxPushTitle:
		return invalidError{"the title is too long or holds a control character"}
	case !textLines(n.Message) || utf8.RuneCountInString(n.Message) > MaxPushText:
		return invalidError{"the message is too long or holds a control character"}
	case n.Priority < 0 || n.Priority > 5:
		return invalidError{"the priority must be 1 to 5"}
	case len(n.Actions) > MaxActions:
		return invalidError{"too many actions"}
	}
	for _, a := range n.Actions {
		if err := checkAction(a); err != nil {
			return err
		}
	}
	return nil
}

func checkAction(a Action) error {
	switch {
	case !oneLine(a.Label) || utf8.RuneCountInString(a.Label) > MaxPushLabel:
		return invalidError{"an action label is too long or holds a control character"}
	case !pushToken.MatchString(a.Token):
		return invalidError{"an action token is not 43 characters of base64url"}
	case !httpURL(a.URL):
		return invalidError{"an action URL must be http or https with a host and no user info"}
	case !oneLine(a.Body) || len(a.Body) > MaxPushText:
		return invalidError{"an action body is too long or holds a control character"}
	}
	return nil
}

// httpURL reports whether raw is an http or https URL with a host and no
// user info. Which hosts are allowed is config.Load's check.
func httpURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// oneLine reports that s is valid UTF-8 with no control rune and no line or
// paragraph separator.
func oneLine(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || r == ' ' || r == ' '
	})
}

// textLines is oneLine for each line of s.
func textLines(s string) bool {
	for l := range strings.SplitSeq(s, "\n") {
		if !oneLine(l) {
			return false
		}
	}
	return true
}

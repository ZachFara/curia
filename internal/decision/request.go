// Package decision holds the pure rules of decision tickets: parsing,
// normalizing and validating a request, its duplicate hash, resolving an
// answer, the queue order and the wording of the messages delivered to an
// agent. It does no I/O and reads no clock; callers pass the time.
package decision

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// Ticket and answer limits (plan section 4.4). Lengths are in runes after
// normalizing. The MCP schemas and the popup use these same numbers.
const (
	MaxQuestion       = 300
	MaxContext        = 2000
	MinOptions        = 2
	MaxOptions        = 4
	MaxKey            = 8
	MaxLabel          = 80
	MaxTradeoffs      = 300
	MaxRationale      = 1200
	MaxRationaleWords = 150
	MaxAssumption     = 300
	MaxTags           = 8
	MaxTag            = 32
	MaxDiffersWhy     = 300
	MaxAnswerText     = 500
	MaxReasoning      = 1000
	MaxWithdrawReason = 300
	MaxQuery          = 500
	MaxMessage        = 1 << 20 // bytes in one MCP message
)

// maxWord bounds the one-word fields (reversibility, mode, scope) while
// normalizing; any value that long is invalid anyway.
const maxWord = 16

// maxIDLen is the length of the longest valid id, "D-" and nine digits.
const maxIDLen = 11

// Request is the argument object of request_decision.
type Request struct {
	Question       string         `json:"question"`
	Context        string         `json:"context"`
	Options        []model.Option `json:"options"`
	Recommendation string         `json:"recommendation"`
	Rationale      string         `json:"rationale"`
	Reversibility  string         `json:"reversibility"`
	Mode           string         `json:"mode"`
	Assumption     string         `json:"assumption"`
	Scope          string         `json:"scope"`
	Tags           []string       `json:"tags"`
	DiffersFrom    string         `json:"differs_from"`
	DiffersWhy     string         `json:"differs_why"`
}

// Problem is one reason a request is refused. Field is the JSON name of the
// field, indexed for options ("options[1].key"); it is empty when the
// arguments as a whole are not a JSON object.
type Problem struct{ Field, Problem string }

// Caller is the session that raises a ticket.
type Caller struct{ SessionID, TaskID, Repo string }

// Parse decodes request_decision arguments. Unknown fields are ignored. A
// value of the wrong JSON type, or arguments that are not a JSON object,
// give exactly one Problem.
func Parse(args json.RawMessage) (Request, []Problem) {
	var r Request
	err := json.Unmarshal(args, &r)
	if err == nil {
		return r, nil
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return r, []Problem{{Field: te.Field, Problem: "must be a JSON " + jsonKind(te.Type)}}
	}
	return Request{}, []Problem{{Problem: "the arguments must be a JSON object"}}
}

// jsonKind names the JSON type that decodes into t.
func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Struct, reflect.Map:
		return "object"
	case reflect.Bool:
		return "boolean"
	case reflect.String:
		return "string"
	default:
		return "number"
	}
}

// Normalize trims every string, cleans one-line fields with sanitize.Line
// and prose with sanitize.Text, lowercases, sorts and de-duplicates the
// tags, and applies the default scope "repo". Each field is cut to one rune
// past its limit, so the work is bounded and an over-long field still fails
// Validate. Normalizing twice gives the same request.
func Normalize(r Request) Request {
	out := Request{
		Question:       line(r.Question, MaxQuestion),
		Context:        text(r.Context, MaxContext),
		Recommendation: line(r.Recommendation, MaxKey),
		Rationale:      text(r.Rationale, MaxRationale),
		Reversibility:  line(r.Reversibility, maxWord),
		Mode:           line(r.Mode, maxWord),
		Assumption:     text(r.Assumption, MaxAssumption),
		Scope:          line(r.Scope, maxWord),
		DiffersFrom:    line(r.DiffersFrom, maxIDLen),
		DiffersWhy:     text(r.DiffersWhy, MaxDiffersWhy),
	}
	if out.Scope == "" {
		out.Scope = string(model.ScopeRepo)
	}
	if r.Options != nil {
		out.Options = make([]model.Option, len(r.Options))
		for i, o := range r.Options {
			out.Options[i] = model.Option{
				Key:       line(o.Key, MaxKey),
				Label:     line(o.Label, MaxLabel),
				Tradeoffs: text(o.Tradeoffs, MaxTradeoffs),
			}
		}
	}
	if r.Tags != nil {
		out.Tags = make([]string, len(r.Tags))
		for i, t := range r.Tags {
			out.Tags[i] = strings.ToLower(line(t, MaxTag))
		}
		slices.Sort(out.Tags)
		out.Tags = slices.Compact(out.Tags)
	}
	return out
}

// line cleans one-line text, cut one rune past limit.
func line(s string, limit int) string {
	return sanitize.Line(s, limit+1)
}

// text cleans prose that may hold paragraphs, cut one rune past limit.
func text(s string, limit int) string {
	return strings.TrimSpace(sanitize.Text(s, limit+1))
}

// Validate checks a normalized request against the limits of plan section
// 4.4 and returns every problem, in field order.
func Validate(r Request) []Problem {
	checks := []func(Request) []Problem{
		func(r Request) []Problem { return checkLine("question", r.Question, 1, MaxQuestion) },
		func(r Request) []Problem { return checkLen("context", r.Context, 0, MaxContext) },
		checkOptions,
		checkRecommendation,
		checkRationale,
		checkReversibility,
		checkMode,
		checkAssumption,
		checkScope,
		checkTags,
		checkDiffers,
	}
	var ps []Problem
	for _, check := range checks {
		ps = append(ps, check(r)...)
	}
	return ps
}

func one(field, problem string) []Problem {
	return []Problem{{Field: field, Problem: problem}}
}

// checkLen checks that s holds minLen to maxLen runes.
func checkLen(field, s string, minLen, maxLen int) []Problem {
	n := utf8.RuneCountInString(s)
	switch {
	case n < minLen:
		return one(field, "is required")
	case n > maxLen:
		return one(field, fmt.Sprintf("must be at most %d characters", maxLen))
	}
	return nil
}

// checkLine is checkLen for a field that must be one line.
func checkLine(field, s string, minLen, maxLen int) []Problem {
	if ps := checkLen(field, s, minLen, maxLen); ps != nil {
		return ps
	}
	if strings.ContainsAny(s, "\r\n") {
		return one(field, "must be one line")
	}
	return nil
}

func checkOptions(r Request) []Problem {
	var ps []Problem
	if n := len(r.Options); n < MinOptions || n > MaxOptions {
		ps = one("options", fmt.Sprintf("must hold %d to %d options, not %d", MinOptions, MaxOptions, n))
	}
	for i, o := range r.Options {
		field := fmt.Sprintf("options[%d].", i)
		switch {
		case !validKey(o.Key):
			ps = append(ps, one(field+"key", fmt.Sprintf("must be 1 to %d letters or digits", MaxKey))...)
		case slices.IndexFunc(r.Options[:i], func(p model.Option) bool { return strings.EqualFold(p.Key, o.Key) }) >= 0:
			ps = append(ps, one(field+"key", "must differ from every other key, ignoring case")...)
		}
		ps = append(ps, checkLine(field+"label", o.Label, 1, MaxLabel)...)
		ps = append(ps, checkLen(field+"tradeoffs", o.Tradeoffs, 0, MaxTradeoffs)...)
	}
	return ps
}

func checkRecommendation(r Request) []Problem {
	switch {
	case r.Recommendation == "":
		return one("recommendation", "is required")
	case !slices.ContainsFunc(r.Options, func(o model.Option) bool { return o.Key == r.Recommendation }):
		return one("recommendation", "must be one of the option keys")
	}
	return nil
}

func checkRationale(r Request) []Problem {
	if ps := checkLen("rationale", r.Rationale, 1, MaxRationale); ps != nil {
		return ps
	}
	if len(strings.Fields(r.Rationale)) > MaxRationaleWords {
		return one("rationale", fmt.Sprintf("must be at most %d words", MaxRationaleWords))
	}
	return nil
}

func checkReversibility(r Request) []Problem {
	switch model.Reversibility(r.Reversibility) {
	case model.ReversCheap, model.ReversCostly, model.ReversIrreversible:
		return nil
	}
	return one("reversibility", "must be cheap, costly or irreversible")
}

func checkMode(r Request) []Problem {
	switch model.Mode(r.Mode) {
	case model.ModeBlocking:
		return nil
	case model.ModeAssume:
		if model.Reversibility(r.Reversibility) == model.ReversIrreversible {
			return one("mode", "must be blocking for an irreversible decision")
		}
		return nil
	}
	return one("mode", "must be blocking or assume")
}

func checkAssumption(r Request) []Problem {
	switch model.Mode(r.Mode) {
	case model.ModeAssume:
		return checkLen("assumption", r.Assumption, 1, MaxAssumption)
	case model.ModeBlocking:
		if r.Assumption != "" {
			return one("assumption", "must be empty when mode is blocking")
		}
	}
	return nil
}

func checkScope(r Request) []Problem {
	if !validScope(model.Scope(r.Scope)) {
		return one("scope", "must be task, repo or global")
	}
	return nil
}

func validScope(s model.Scope) bool {
	return s == model.ScopeTask || s == model.ScopeRepo || s == model.ScopeGlobal
}

func checkTags(r Request) []Problem {
	var ps []Problem
	if len(r.Tags) > MaxTags {
		ps = one("tags", fmt.Sprintf("must hold at most %d tags", MaxTags))
	}
	for _, t := range r.Tags {
		if !validTag(t) {
			ps = append(ps, one("tags", fmt.Sprintf("tag %q must be 1 to %d of a-z, 0-9, _ and -, starting with a letter or digit", t, MaxTag))...)
		}
	}
	return ps
}

func checkDiffers(r Request) []Problem {
	if r.DiffersFrom == "" {
		return checkLen("differs_why", r.DiffersWhy, 0, MaxDiffersWhy)
	}
	var ps []Problem
	if !ValidID(r.DiffersFrom) {
		ps = one("differs_from", "must be a decision id such as D-117")
	}
	return append(ps, checkLen("differs_why", r.DiffersWhy, 1, MaxDiffersWhy)...)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLowerAlnum(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'z') }

// validKey matches ^[A-Za-z0-9]{1,8}$.
func validKey(k string) bool {
	if k == "" || len(k) > MaxKey {
		return false
	}
	for i := range len(k) {
		if c := k[i]; !isLowerAlnum(c) && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// validTag matches ^[a-z0-9][a-z0-9_-]{0,31}$.
func validTag(t string) bool {
	if t == "" || len(t) > MaxTag || !isLowerAlnum(t[0]) {
		return false
	}
	for i := 1; i < len(t); i++ {
		if c := t[i]; !isLowerAlnum(c) && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// ValidID reports whether id is a decision id, ^D-[1-9][0-9]{0,8}$.
func ValidID(id string) bool {
	n, ok := strings.CutPrefix(id, "D-")
	if !ok || n == "" || len(n) > 9 || n[0] == '0' {
		return false
	}
	for i := range len(n) {
		if !isDigit(n[i]) {
			return false
		}
	}
	return true
}

// hashed is the canonical form ArgsHash encodes, fields in the order of
// plan section 4.5.
type hashed struct {
	Question       string         `json:"question"`
	Context        string         `json:"context"`
	Options        []model.Option `json:"options"`
	Recommendation string         `json:"recommendation"`
	Rationale      string         `json:"rationale"`
	Reversibility  string         `json:"reversibility"`
	Mode           string         `json:"mode"`
	Assumption     string         `json:"assumption"`
	Scope          string         `json:"scope"`
	Tags           []string       `json:"tags"`
}

// ArgsHash is the duplicate key of a request: the lowercase hex SHA-256 of
// the canonical JSON of the normalized request (Normalize runs first). The
// bytes hashed are exactly one JSON object with these keys in this order:
// question, context, options (an array of objects with key, label,
// tradeoffs, in the order given), recommendation, rationale,
// reversibility, mode, assumption, scope, tags (sorted). Every key is
// present; options and tags are [] when empty, never null. It is
// encoding/json's output with HTML escaping off ("<", ">" and "&" are
// literal), no insignificant whitespace and no trailing newline.
// differs_from and differs_why are not hashed.
func ArgsHash(r Request) string {
	r = Normalize(r)
	h := hashed{
		Question: r.Question, Context: r.Context, Options: r.Options,
		Recommendation: r.Recommendation, Rationale: r.Rationale,
		Reversibility: r.Reversibility, Mode: r.Mode, Assumption: r.Assumption,
		Scope: r.Scope, Tags: r.Tags,
	}
	if h.Options == nil {
		h.Options = []model.Option{}
	}
	if h.Tags == nil {
		h.Tags = []string{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(h) // cannot fail: only strings and slices of them
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// New builds the open ticket for a request from caller at now. The request
// is normalized first and should already be valid. An assume ticket
// expires assumeExpiry after now; a blocking one never does.
func New(r Request, c Caller, now time.Time, assumeExpiry time.Duration) model.Decision {
	r = Normalize(r)
	d := model.Decision{
		SessionID:      c.SessionID,
		TaskID:         c.TaskID,
		Repo:           c.Repo,
		Question:       r.Question,
		Context:        r.Context,
		Options:        r.Options,
		Recommendation: r.Recommendation,
		Rationale:      r.Rationale,
		Reversibility:  model.Reversibility(r.Reversibility),
		Mode:           model.Mode(r.Mode),
		Assumption:     r.Assumption,
		Scope:          model.Scope(r.Scope),
		Tags:           r.Tags,
		Status:         model.StatusOpen,
		DiffersFrom:    r.DiffersFrom,
		DiffersWhy:     r.DiffersWhy,
		ArgsHash:       ArgsHash(r),
		Delivery:       model.DeliveryNone,
		CreatedAt:      now,
	}
	if d.Mode == model.ModeAssume {
		d.ExpiresAt = now.Add(assumeExpiry)
	}
	return d
}

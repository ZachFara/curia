// Package usage parses what other programs write about cost and usage: the
// ingest document, Claude Code's status-line payload and the cost-lens
// analyzer's summary. Everything here is pure: no file, network or process.
// All input is untrusted: it is bounded, strictly typed, range checked and
// cleaned, and a bad optional field is dropped alone.
package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
)

// MaxDoc is the size in bytes of the largest ingest or status-line document.
const MaxDoc = 64 << 10

const (
	maxModelRunes  = 24
	maxStatusCells = 80
	maxTimeText    = 64
	maxTTLText     = 8
	maxUnixSeconds = 1e11
	maxTokens      = 1e10
)

// Limit is one window's reading.
type Limit struct {
	UsedPct  float64
	ResetsAt time.Time // zero: none, or out of range
}

// Doc is one accepted document. A Has field is false when the document did
// not carry that value or it was refused.
type Doc struct {
	SessionID       string
	At              time.Time // the reading's time; now when the document had none or a bad one
	CostUSD         float64
	HasCost         bool
	CacheExpiresAt  time.Time
	HasCacheExpiry  bool
	CacheRebuildUSD float64
	HasCacheRebuild bool
	Limits          map[model.UsageWindow]Limit // never nil
	Dropped         []string                    // names of fields that were present and refused, sorted

	// Status line only. CacheRecacheTokens is the token count a cold cache
	// would re-read (never converted to money, Ruling P5-R2); CacheWarm and
	// CacheTTL are the payload's own words about the cache, kept as read.
	ModelName          string // cleaned, at most 24 runes
	ContextPct         int
	HasContext         bool
	CacheRecacheTokens int64
	HasCacheRecache    bool
	CacheWarm          bool
	HasCacheWarm       bool
	CacheTTL           string // "" when absent or refused
}

// ErrDoc is wrapped when a whole document is refused; the error text names
// the reason and never a value.
var ErrDoc = errors.New("usage: document refused")

func refuse(reason string) error { return fmt.Errorf("%w: %s", ErrDoc, reason) }

// ValidSessionID reports whether id matches ^[A-Za-z0-9_-]{1,64}$.
func ValidSessionID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// ParseDoc reads an ingest document (section 4.2 of the plan, schema in
// docs/ingest-schema.json).
func ParseDoc(raw []byte, now time.Time) (Doc, error) {
	top, err := topObject(raw)
	if err != nil {
		return Doc{}, err
	}
	if v, st := number(top["schema_version"]); st != present || v != 1 {
		return Doc{}, refuse("schema_version is not 1")
	}
	id, ok := sessionID(top["session_id"])
	if !ok {
		return Doc{}, refuse("bad session_id")
	}
	r := newReader(now)
	d := r.doc(id)
	r.at(&d, top, "at")
	if v, ok := r.num(top, "cost_usd", "cost_usd", 100000); ok {
		d.CostUSD, d.HasCost = v, true
	}
	if t, ok := r.when(top, "cache_expires_at", "cache_expires_at", now.Add(-7*24*time.Hour), now.Add(2*time.Hour), false); ok {
		d.CacheExpiresAt, d.HasCacheExpiry = t, true
	}
	if v, ok := r.num(top, "cache_rebuild_usd", "cache_rebuild_usd", 1000); ok {
		d.CacheRebuildUSD, d.HasCacheRebuild = v, true
	}
	r.limits(&d, top["rate_limits"], "rate_limits")
	return r.finish(d), nil
}

// FromStatusLine maps Claude Code's status-line payload to a Doc under the
// bounds of ParseDoc. Dropped holds the payload's own dotted names.
func FromStatusLine(raw []byte, now time.Time) (Doc, error) {
	top, err := topObject(raw)
	if err != nil {
		return Doc{}, err
	}
	id, ok := sessionID(top["session_id"])
	if !ok {
		return Doc{}, refuse("bad session_id")
	}
	r := newReader(now)
	d := r.doc(id)
	r.statusCost(&d, top)
	r.limits(&d, top["rate_limits"], "rate_limits")
	r.statusContext(&d, top)
	r.statusModel(&d, top)
	r.statusCache(&d, top)
	return r.finish(d), nil
}

// sub reads a nested object; a value of another type is dropped under name.
func (r *reader) sub(top map[string]json.RawMessage, name string) map[string]json.RawMessage {
	obj, st := object(top[name])
	if st == bad {
		r.drop(name)
	}
	return obj
}

func (r *reader) statusCost(d *Doc, top map[string]json.RawMessage) {
	if v, ok := r.num(r.sub(top, "cost"), "total_cost_usd", "cost.total_cost_usd", 100000); ok {
		d.CostUSD, d.HasCost = v, true
	}
}

func (r *reader) statusContext(d *Doc, top map[string]json.RawMessage) {
	if v, ok := r.num(r.sub(top, "context_window"), "used_percentage", "context_window.used_percentage", 100); ok {
		d.ContextPct, d.HasContext = int(math.Round(v)), true
	}
}

func (r *reader) statusModel(d *Doc, top map[string]json.RawMessage) {
	mdl := r.sub(top, "model")
	for _, k := range []string{"display_name", "id"} {
		s, ok := r.str(mdl, k, "model."+k)
		if !ok {
			continue
		}
		if name := sanitize.Line(s, maxModelRunes); name != "" {
			d.ModelName = name
			return
		}
	}
}

// statusCache reads prompt_cache (Ruling P5-R1). Each field is optional and
// an absent one is no reading.
func (r *reader) statusCache(d *Doc, top map[string]json.RawMessage) {
	pc := r.sub(top, "prompt_cache")
	if t, ok := r.when(pc, "expires_at", "prompt_cache.expires_at", r.now.Add(-7*24*time.Hour), r.now.Add(2*time.Hour), true); ok {
		d.CacheExpiresAt, d.HasCacheExpiry = t, true
	}
	if v, ok := r.num(pc, "recache_tokens_if_cold", "prompt_cache.recache_tokens_if_cold", maxTokens); ok {
		if v == math.Trunc(v) {
			d.CacheRecacheTokens, d.HasCacheRecache = int64(v), true
		} else {
			r.drop("prompt_cache.recache_tokens_if_cold")
		}
	}
	switch b, st := boolean(pc["warm"]); st {
	case present:
		d.CacheWarm, d.HasCacheWarm = b, true
	case bad:
		r.drop("prompt_cache.warm")
	}
	if s, ok := r.str(pc, "ttl", "prompt_cache.ttl"); ok {
		if validTTL(s) {
			d.CacheTTL = s
		} else {
			r.drop("prompt_cache.ttl")
		}
	}
}

// StatusText is the status line: "<model> · ctx 71% · $4.12 · 5h 62%", each
// part left out when its value is absent, at most 80 cells, "" when d has
// nothing to show.
func StatusText(d Doc) string {
	tail := strings.Join(statusParts(d), " · ")
	name := sanitize.Line(d.ModelName, maxModelRunes)
	// Fit the name into what the other parts leave. A rune from U+1100 on
	// may be two cells wide, so it is counted as two: the bound holds
	// without a width table.
	budget := maxStatusCells - cells(tail)
	if tail != "" {
		budget -= cells(" · ")
	}
	name = fit(name, budget)
	switch {
	case name != "" && tail != "":
		return name + " · " + tail
	case name != "":
		return name
	}
	return tail
}

func statusParts(d Doc) []string {
	var parts []string
	if d.HasContext && d.ContextPct >= 0 && d.ContextPct <= 100 {
		parts = append(parts, fmt.Sprintf("ctx %d%%", d.ContextPct))
	}
	if d.HasCost && d.CostUSD >= 0 && !math.IsNaN(d.CostUSD) && !math.IsInf(d.CostUSD, 0) {
		parts = append(parts, fmt.Sprintf("$%.2f", d.CostUSD))
	}
	if l, ok := d.Limits[model.WindowFiveHour]; ok && l.UsedPct >= 0 && l.UsedPct <= 100 {
		parts = append(parts, fmt.Sprintf("5h %.0f%%", l.UsedPct))
	}
	return parts
}

func cells(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x1100 {
			n++
		}
	}
	return n
}

func fit(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if cells(s) <= budget {
		return s
	}
	used := 1 // the ellipsis
	var b strings.Builder
	for _, r := range s {
		w := 1
		if r >= 0x1100 {
			w = 2
		}
		if used+w > budget {
			break
		}
		used += w
		b.WriteRune(r)
	}
	if budget < 1 {
		return ""
	}
	return strings.TrimRight(b.String(), " ") + "…"
}

func validTTL(s string) bool {
	if s == "" || len(s) > maxTTLText {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	switch s[len(s)-1] {
	case 's', 'm', 'h', 'd':
		return len(s) > 1
	}
	return false
}

// --- reading helpers ---

type state int

const (
	absent state = iota // missing or null
	present
	bad
)

func topObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) > MaxDoc {
		return nil, refuse("document too large")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return nil, refuse("not one JSON object")
	}
	return top, nil
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func number(raw json.RawMessage) (float64, state) {
	if isNull(raw) {
		return 0, absent
	}
	if c := raw[0]; c != '-' && (c < '0' || c > '9') {
		return 0, bad
	}
	v, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, bad
	}
	return v, present
}

func boolean(raw json.RawMessage) (bool, state) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, present
	case "false":
		return false, present
	}
	if isNull(raw) {
		return false, absent
	}
	return false, bad
}

func text(raw json.RawMessage) (string, state) {
	if isNull(raw) {
		return "", absent
	}
	var s string
	if raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", bad
	}
	return s, present
}

func object(raw json.RawMessage) (map[string]json.RawMessage, state) {
	if isNull(raw) {
		return nil, absent
	}
	var m map[string]json.RawMessage
	if raw[0] != '{' || json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, bad
	}
	return m, present
}

func sessionID(raw json.RawMessage) (string, bool) {
	s, st := text(raw)
	if st != present || !ValidSessionID(s) {
		return "", false
	}
	return s, true
}

type reader struct {
	now     time.Time
	dropped map[string]struct{}
}

func newReader(now time.Time) *reader {
	return &reader{now: now, dropped: map[string]struct{}{}}
}

func (r *reader) doc(id string) Doc {
	return Doc{SessionID: id, At: r.now, Limits: map[model.UsageWindow]Limit{}}
}

func (r *reader) drop(name string) { r.dropped[name] = struct{}{} }

func (r *reader) finish(d Doc) Doc {
	d.Dropped = make([]string, 0, len(r.dropped))
	for n := range r.dropped {
		d.Dropped = append(d.Dropped, n)
	}
	sort.Strings(d.Dropped)
	return d
}

// num reads obj[key] as a number from 0 to hi; a present but refused value
// is dropped under name.
func (r *reader) num(obj map[string]json.RawMessage, key, name string, hi float64) (float64, bool) {
	v, st := number(obj[key])
	switch {
	case st == absent:
		return 0, false
	case st == bad || v < 0 || v > hi:
		r.drop(name)
		return 0, false
	}
	return v, true
}

func (r *reader) str(obj map[string]json.RawMessage, key, name string) (string, bool) {
	s, st := text(obj[key])
	switch st {
	case absent:
		return "", false
	case bad:
		r.drop(name)
		return "", false
	}
	return s, true
}

// when reads obj[key] as an RFC 3339 string, or with unix also a number of
// Unix seconds, from lo to hi.
func (r *reader) when(obj map[string]json.RawMessage, key, name string, lo, hi time.Time, unix bool) (time.Time, bool) {
	raw := obj[key]
	if isNull(raw) {
		return time.Time{}, false
	}
	t, ok := parseTime(raw, unix)
	if !ok || t.Before(lo) || t.After(hi) {
		r.drop(name)
		return time.Time{}, false
	}
	return t, true
}

func parseTime(raw json.RawMessage, unix bool) (time.Time, bool) {
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || len(s) > maxTimeText {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, s)
		return t.UTC(), err == nil
	}
	if !unix {
		return time.Time{}, false
	}
	v, st := number(raw)
	if st != present || v < 0 || v > maxUnixSeconds {
		return time.Time{}, false
	}
	return time.Unix(int64(v), 0).UTC(), true
}

// at reads the reading's own time; a bad one is dropped and At stays now.
func (r *reader) at(d *Doc, top map[string]json.RawMessage, key string) {
	if t, ok := r.when(top, key, key, r.now.Add(-24*time.Hour), r.now.Add(time.Minute), false); ok {
		d.At = t
	}
}

// limits reads the two windows under prefix. A window whose used_percentage
// is missing or bad is dropped whole; a bad resets_at is dropped alone.
func (r *reader) limits(d *Doc, raw json.RawMessage, prefix string) {
	rl, st := object(raw)
	if st == bad {
		r.drop(prefix)
		return
	}
	for _, w := range []model.UsageWindow{model.WindowFiveHour, model.WindowSevenDay} {
		name := prefix + "." + string(w)
		obj, st := object(rl[string(w)])
		switch st {
		case absent:
			continue
		case bad:
			r.drop(name)
			continue
		}
		pct, st := number(obj["used_percentage"])
		if st != present || pct < 0 || pct > 100 {
			r.drop(name)
			continue
		}
		l := Limit{UsedPct: pct}
		if t, ok := r.when(obj, "resets_at", name+".resets_at", r.now.Add(-time.Hour), r.now.Add(8*24*time.Hour), true); ok {
			l.ResetsAt = t
		}
		d.Limits[w] = l
	}
}

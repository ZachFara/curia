package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// Ingest's rules (plan section 4.2).
const (
	SampleRepeat = 60 * time.Second // an identical reading inside this adds no row
	UsageKeep    = 8 * 24 * time.Hour
	CostKeep     = 24 * time.Hour
	PruneBatch   = 200
	// BaselineKeep is how long a session's newest cost sample outlives
	// CostKeep as its CostSince baseline: the longest window a reader looks
	// back over (seven days) and a day, as for usage samples.
	BaselineKeep = UsageKeep
)

// The ranges Ingest accepts, checked again at the store although the
// parser checks them first: the values come from other programs. They are
// the ingest contract's (plan section 4.2, docs/ingest-schema.json), with
// times taken against Ingest's now.
const (
	maxCostUSD    = 100_000
	maxRebuildUSD = 1000
	// MaxRecacheTokens bounds CacheRecacheTokens (Ruling P5-R2): the
	// parser's own bound on a token count (internal/usage), so the store
	// never refuses a reading the parser kept.
	MaxRecacheTokens = 10_000_000_000
	maxSessionID     = 64
	atPast           = 24 * time.Hour
	atFuture         = time.Minute
	cachePast        = 7 * 24 * time.Hour
	cacheFuture      = 2 * time.Hour
	resetsPast       = time.Hour
	resetsFuture     = 8 * 24 * time.Hour
)

// errBadReading is why Ingest refuses a reading. Its text names the field,
// never a value.
var errBadReading = errors.New("bad reading")

// Reading is what one ingest stores. A nil pointer or a zero time means "not carried": that value is left as it is.
type Reading struct {
	SessionID       string
	At              time.Time // zero: now
	CostUSD         *float64
	CacheExpiresAt  time.Time
	CacheRebuildUSD *float64
	// CacheRecacheTokens is how many tokens a rebuild of the cold cache would
	// write (Ruling P5-R2: the status line's prompt_cache.recache_tokens_if_cold).
	CacheRecacheTokens *int64
	Limits             []model.UsageSample // Window, UsedPct and ResetsAt set; At and SessionID are filled from the Reading
}

// IngestResult says what an ingest changed.
type IngestResult struct {
	SessionFound          bool
	UsageAdded, CostAdded int
	Pruned                int
}

// The statements of Ingest, in its order.
const (
	// ingestSession writes the carried values; a NULL parameter keeps the
	// stored value. It touches neither updated_at, seen_at nor state:
	// ingest is not activity. Parameters: cost, cache expiry, rebuild cost,
	// recache tokens, the session id.
	ingestSession = `
UPDATE sessions SET cost_usd = COALESCE(?, cost_usd), cache_expires_at = COALESCE(?, cache_expires_at),
  cache_rebuild_usd = COALESCE(?, cache_rebuild_usd), recache_tokens = COALESCE(?, recache_tokens)
WHERE id = ?`
	ingestSessionExists = `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = ?)`
	// newestCost is a session's newest cost sample; newestUsage a window's.
	newestCost  = `SELECT cost_usd, at FROM cost_samples WHERE session_id = ? ORDER BY at DESC, n DESC LIMIT 1`
	newestUsage = `SELECT used_pct, resets_at, at FROM usage_samples WHERE window = ? ORDER BY at DESC, n DESC LIMIT 1`
	insertCost  = `INSERT INTO cost_samples (at, session_id, cost_usd) VALUES (?, ?, ?)`
	insertUsage = `INSERT INTO usage_samples (at, window, used_pct, resets_at, session_id) VALUES (?, ?, ?, ?, ?)`
	// pruneUsage deletes the oldest usage samples taken before the cutoff,
	// at most the limit, never a window's newest. The IN list is the
	// table's CHECK, so the index on (window, at) serves the range.
	// Parameters: the cutoff, the limit.
	pruneUsage = `
DELETE FROM usage_samples WHERE n IN (
  SELECT u.n FROM usage_samples u
  WHERE u.window IN ('five_hour', 'seven_day') AND u.at < ?
    AND u.n != (SELECT v.n FROM usage_samples v WHERE v.window = u.window ORDER BY v.at DESC, v.n DESC LIMIT 1)
  ORDER BY u.at, u.n LIMIT ?)`
	// costSessions walks the distinct session ids of cost_samples down
	// the (session_id, at) index, one seek each, so no statement scans the
	// table for them.
	costSessions = `
WITH RECURSIVE ids(id) AS (
  SELECT (SELECT MIN(session_id) FROM cost_samples)
  UNION ALL
  SELECT (SELECT MIN(session_id) FROM cost_samples WHERE session_id > ids.id) FROM ids WHERE ids.id IS NOT NULL)`
	// costWalk walks, down the same index, at most pruneWalk session ids
	// of cost_samples after a cursor. Parameters: the cursor ("" for the
	// first), pruneWalk.
	costWalk = `
WITH RECURSIVE ids(id, k) AS (
  SELECT (SELECT MIN(session_id) FROM cost_samples WHERE session_id > ?), 1
  UNION ALL
  SELECT (SELECT MIN(session_id) FROM cost_samples WHERE session_id > ids.id), ids.k + 1 FROM ids
  WHERE ids.id IS NOT NULL AND ids.k < ?)`
	pruneWalkIDs = costWalk + ` SELECT id FROM ids WHERE id IS NOT NULL`
	// pruneCost deletes, among the sessions costWalk walks, the oldest cost
	// samples taken before the cutoff, at most the limit, but not a
	// session's newest (CostSince's baseline) unless it is older than the
	// horizon too: without the horizon every session ever seen would keep
	// a row. Parameters: the cursor, pruneWalk, the cutoff, the horizon,
	// the limit.
	pruneCost = costWalk + `
DELETE FROM cost_samples WHERE n IN (
  SELECT c.n FROM ids JOIN cost_samples c ON c.session_id = ids.id AND c.at < ?
  WHERE c.at < ? OR c.n != (SELECT b.n FROM cost_samples b WHERE b.session_id = ids.id ORDER BY b.at DESC, b.n DESC LIMIT 1)
  ORDER BY c.at, c.n LIMIT ?)`
	pruneCursorGet = `SELECT value FROM meta WHERE key = ?`
	pruneCursorSet = `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`
)

// pruneWalk is how many sessions' cost samples one ingest prunes. The
// prune walks the sessions from a cursor kept in meta (pruneCursorKey),
// so every session is reached in turn and one ingest's cost does not grow
// with the number of sessions (probe S1: ingest runs about three times a
// second for every live session).
const pruneWalk = 32

// pruneCursorKey is the meta key of the last session id the cost prune
// walked; "" or absent: start from the first.
const pruneCursorKey = "cost_prune_after"

// Ingest stores r in one write transaction (plan section 4.2): the
// session's cost and cache values (an absent value keeps the stored one;
// no row is created for an unknown session), a cost sample when the
// session exists and r has a cost, one usage sample per window (for an
// unknown session too: the windows are the account's), each unless the
// series' newest sample has the same values and is less than SampleRepeat
// older, and then at most PruneBatch old samples deleted, usage samples
// first. It is not activity: the session's state and times are not
// touched. A reading with any value outside the ingest contract's ranges
// is refused whole and nothing is written. Callers pass the now they
// checked the document against.
func (s *Store) Ingest(ctx context.Context, r Reading, now time.Time) (IngestResult, error) {
	if r.At.IsZero() {
		r.At = now
	}
	if err := checkReading(r, now); err != nil {
		return IngestResult{}, fail("ingest", "", err) // the id may be the bad value
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return IngestResult{}, fail("ingest", r.SessionID, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := ingest(ctx, tx, r, now)
	if err != nil {
		return IngestResult{}, fail("ingest", r.SessionID, err)
	}
	if err = tx.Commit(); err != nil {
		return IngestResult{}, fail("ingest", r.SessionID, fmt.Errorf("commit: %w", err))
	}
	return res, nil
}

// ingest runs Ingest's four steps on tx.
func ingest(ctx context.Context, tx *sql.Tx, r Reading, now time.Time) (IngestResult, error) {
	var res IngestResult
	var err error
	if res.SessionFound, err = ingestSessionRow(ctx, tx, r); err != nil {
		return res, err
	}
	if res.SessionFound && r.CostUSD != nil {
		if res.CostAdded, err = addCost(ctx, tx, r.SessionID, r.At, *r.CostUSD); err != nil {
			return res, err
		}
	}
	for _, l := range r.Limits {
		added, addErr := addUsage(ctx, tx, r, l)
		if addErr != nil {
			return res, addErr
		}
		res.UsageAdded += added
	}
	res.Pruned, err = prune(ctx, tx, now)
	return res, err
}

// ingestSessionRow writes r's session values and reports whether the
// session exists. A reading that carries none only looks.
func ingestSessionRow(ctx context.Context, tx *sql.Tx, r Reading) (bool, error) {
	if r.CostUSD == nil && r.CacheExpiresAt.IsZero() && r.CacheRebuildUSD == nil && r.CacheRecacheTokens == nil {
		var found bool
		err := tx.QueryRowContext(ctx, ingestSessionExists, r.SessionID).Scan(&found)
		return found, err
	}
	var recache any
	if r.CacheRecacheTokens != nil {
		recache = *r.CacheRecacheTokens
	}
	out, err := tx.ExecContext(ctx, ingestSession, floatOrNull(r.CostUSD), formatTime(r.CacheExpiresAt),
		floatOrNull(r.CacheRebuildUSD), recache, r.SessionID)
	if err != nil {
		return false, err
	}
	n, err := out.RowsAffected()
	return n == 1, err
}

func floatOrNull(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// addCost stores a cost sample unless it repeats the session's newest.
func addCost(ctx context.Context, tx *sql.Tx, id string, at time.Time, cost float64) (int, error) {
	var last float64
	var lastAt sql.NullString
	err := tx.QueryRowContext(ctx, newestCost, id).Scan(&last, &lastAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, err
	default:
		t, err := parseTime(lastAt)
		if err != nil {
			return 0, err
		}
		if last == cost && at.Sub(t) < SampleRepeat {
			return 0, nil
		}
	}
	if _, err := tx.ExecContext(ctx, insertCost, formatTime(at), id, cost); err != nil {
		return 0, err
	}
	return 1, nil
}

// addUsage stores l as a usage sample of r unless it repeats the
// window's newest.
func addUsage(ctx context.Context, tx *sql.Tx, r Reading, l model.UsageSample) (int, error) {
	var last float64
	var lastResets, lastAt sql.NullString
	err := tx.QueryRowContext(ctx, newestUsage, l.Window).Scan(&last, &lastResets, &lastAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, err
	default:
		t, err := parseTime(lastAt)
		if err != nil {
			return 0, err
		}
		resets, err := parseTime(lastResets)
		if err != nil {
			return 0, err
		}
		if last == l.UsedPct && resets.Equal(l.ResetsAt) && r.At.Sub(t) < SampleRepeat {
			return 0, nil
		}
	}
	if _, err := tx.ExecContext(ctx, insertUsage, formatTime(r.At), l.Window, l.UsedPct,
		formatTime(l.ResetsAt), r.SessionID); err != nil {
		return 0, err
	}
	return 1, nil
}

// prune deletes at most PruneBatch old samples, usage samples first, then
// the cost samples of the next pruneWalk sessions after the cursor.
func prune(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	out, err := tx.ExecContext(ctx, pruneUsage, formatTime(now.Add(-UsageKeep)), PruneBatch)
	if err != nil {
		return 0, err
	}
	usage, err := out.RowsAffected()
	if err != nil || usage >= PruneBatch {
		return int(usage), err
	}
	cost, err := pruneCostSamples(ctx, tx, now, PruneBatch-usage)
	return int(usage + cost), err
}

// pruneCostSamples deletes at most limit old cost samples of the sessions
// after the cursor and moves the cursor past them, or back to the start
// when the walk reached the last session.
func pruneCostSamples(ctx context.Context, tx *sql.Tx, now time.Time, limit int64) (int64, error) {
	var cursor string
	err := tx.QueryRowContext(ctx, pruneCursorGet, pruneCursorKey).Scan(&cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	walked, err := pruneWalked(ctx, tx, cursor)
	if err != nil {
		return 0, err
	}
	out, err := tx.ExecContext(ctx, pruneCost, cursor, pruneWalk,
		formatTime(now.Add(-CostKeep)), formatTime(now.Add(-BaselineKeep)), limit)
	if err != nil {
		return 0, err
	}
	n, err := out.RowsAffected()
	if err != nil {
		return 0, err
	}
	next := ""
	if len(walked) == pruneWalk {
		next = walked[len(walked)-1]
	}
	if next != cursor {
		if _, err := tx.ExecContext(ctx, pruneCursorSet, pruneCursorKey, next); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// pruneWalked is the session ids pruneCost walks after cursor.
func pruneWalked(ctx context.Context, tx *sql.Tx, cursor string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, pruneWalkIDs, cursor, pruneWalk)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var walked []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		walked = append(walked, id)
	}
	return walked, rows.Err()
}

// bad is checkReading's refusal of field.
func bad(field string) error { return fmt.Errorf("%w: %s", errBadReading, field) }

// checkReading refuses a reading with any value outside the ingest
// contract's ranges. Its errors name the field, never the value.
func checkReading(r Reading, now time.Time) error {
	if !validSessionID(r.SessionID) {
		return bad("session_id")
	}
	if !within(r.At, now.Add(-atPast), now.Add(atFuture)) {
		return bad("at")
	}
	if !inRange(r.CostUSD, maxCostUSD) {
		return bad("cost_usd")
	}
	if !r.CacheExpiresAt.IsZero() && !within(r.CacheExpiresAt, now.Add(-cachePast), now.Add(cacheFuture)) {
		return bad("cache_expires_at")
	}
	if !inRange(r.CacheRebuildUSD, maxRebuildUSD) {
		return bad("cache_rebuild_usd")
	}
	if r.CacheRecacheTokens != nil && (*r.CacheRecacheTokens < 0 || *r.CacheRecacheTokens > MaxRecacheTokens) {
		return bad("recache_tokens")
	}
	return checkLimits(r.Limits, now)
}

// checkLimits refuses an unknown or repeated window, a used percentage
// outside 0 to 100 and a reset time outside the contract's range.
func checkLimits(limits []model.UsageSample, now time.Time) error {
	seen := map[model.UsageWindow]bool{}
	for _, l := range limits {
		if (l.Window != model.WindowFiveHour && l.Window != model.WindowSevenDay) || seen[l.Window] {
			return bad("rate_limits")
		}
		seen[l.Window] = true
		if v := l.UsedPct; math.IsNaN(v) || v < 0 || v > 100 {
			return bad("rate_limits." + string(l.Window) + ".used_percentage")
		}
		if !l.ResetsAt.IsZero() && !within(l.ResetsAt, now.Add(-resetsPast), now.Add(resetsFuture)) {
			return bad("rate_limits." + string(l.Window) + ".resets_at")
		}
	}
	return nil
}

// inRange reports whether v is absent or a finite number from 0 to limit.
func inRange(v *float64, limit float64) bool {
	return v == nil || (!math.IsNaN(*v) && *v >= 0 && *v <= limit)
}

// within reports whether t lies from lo to hi, both included.
func within(t, lo, hi time.Time) bool {
	return !t.Before(lo) && !t.After(hi)
}

// validSessionID is the ingest contract's ^[A-Za-z0-9_-]{1,64}$.
func validSessionID(id string) bool {
	if id == "" || len(id) > maxSessionID {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// usageSince is UsageSince's statement; its parameter is since ("" for
// the beginning).
const usageSince = `
SELECT n, at, window, used_pct, resets_at, session_id FROM usage_samples
WHERE window IN ('five_hour', 'seven_day') AND at >= ?
ORDER BY at, n`

// UsageSince returns the usage samples taken at or after since, by at then n.
// A zero since returns every sample.
func (s *Store) UsageSince(ctx context.Context, since time.Time) ([]model.UsageSample, error) {
	rows, err := s.db.QueryContext(ctx, usageSince, sinceArg(since))
	if err != nil {
		return nil, fail("usage since", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.UsageSample
	for rows.Next() {
		var u model.UsageSample
		var at, resets sql.NullString
		if err = rows.Scan(&u.N, &at, &u.Window, &u.UsedPct, &resets, &u.SessionID); err != nil {
			return nil, fail("usage since", "", err)
		}
		if u.At, err = parseTime(at); err != nil {
			return nil, fail("usage since", "", err)
		}
		if u.ResetsAt, err = parseTime(resets); err != nil {
			return nil, fail("usage since", "", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("usage since", "", err)
	}
	return out, nil
}

// costSince is CostSince's statement: per session (walked down the index),
// its samples at or after since, and its newest sample before since.
// Parameters: since, since.
const costSince = costSessions + `
SELECT c.n, c.at, c.session_id, c.cost_usd FROM ids JOIN cost_samples c ON c.session_id = ids.id AND c.at >= ?
UNION ALL
SELECT c.n, c.at, c.session_id, c.cost_usd FROM ids JOIN cost_samples c ON c.n = (
  SELECT b.n FROM cost_samples b WHERE b.session_id = ids.id AND b.at < ? ORDER BY b.at DESC, b.n DESC LIMIT 1)
ORDER BY 3, 2, 1`

// CostSince returns the cost samples taken at or after since and, per session, its newest sample before since, by session, at, n.
// A zero since returns every sample.
func (s *Store) CostSince(ctx context.Context, since time.Time) ([]model.CostSample, error) {
	rows, err := s.db.QueryContext(ctx, costSince, sinceArg(since), sinceArg(since))
	if err != nil {
		return nil, fail("cost since", "", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.CostSample
	for rows.Next() {
		var c model.CostSample
		var at sql.NullString
		if err = rows.Scan(&c.N, &at, &c.SessionID, &c.CostUSD); err != nil {
			return nil, fail("cost since", "", err)
		}
		if c.At, err = parseTime(at); err != nil {
			return nil, fail("cost since", "", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fail("cost since", "", err)
	}
	return out, nil
}

// sinceArg is a lower bound on stored times: "" (before every stored
// time) for the zero time, which formatTime would make NULL.
func sinceArg(since time.Time) any {
	if since.IsZero() {
		return ""
	}
	return formatTime(since)
}

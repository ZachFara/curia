package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/usage"
)

// ingestLimit bounds the whole run; tests shorten it.
var ingestLimit = 2 * time.Second

// ingestLogNow is the clock of the log's rate limit (not the payload's
// clock): tests move it.
var ingestLogNow = time.Now

// ingestStatusLineFlag is the one argument ingest reads: the input is
// Claude Code's status-line JSON (the form internal/install's
// StatusLineChain suggests).
const ingestStatusLineFlag = "--statusline"

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "ingest [--statusline]",
		Short: "Store one cost and usage reading (run by cost-lens, or behind a status line)",
		Long: "Reads one JSON document on stdin (docs/ingest-schema.json; with --statusline,\n" +
			"Claude Code's status-line JSON) and stores the session's cost and cache state\n" +
			"and the account's usage windows. It never creates a session or the store.\n" +
			"Always exits 0 and prints nothing; problems go to the log file.",
		Args: cobra.ArbitraryArgs,
		// Every argument reaches runIngest, so a bad one is logged, not printed.
		DisableFlagParsing: true,
		Run:                runIngest,
	})
}

// runIngest stores one document under ingestLimit and returns on every
// path, panics included, having written nothing to stdout or stderr.
func runIngest(cmd *cobra.Command, args []string) {
	signal.Ignore(syscall.SIGPIPE) // as the hook: a closed pipe must not kill the run
	statusLine, extra := ingestArgs(args)
	defer func() {
		if r := recover(); r != nil {
			ingestLog("ingest", usage.Doc{}, fmt.Errorf("panic: %s", sanitize.Line(fmt.Sprint(r), 200)), nil)
		}
	}()
	ctx, cancel := context.WithTimeout(cmd.Context(), ingestLimit)
	defer cancel()
	doc, err := ingestRun(ctx, cmd.InOrStdin(), statusLine)
	ingestLog("ingest", doc, err, extra)
}

// ingestArgs splits the arguments into the --statusline switch and the
// rest, which are ignored.
func ingestArgs(args []string) (statusLine bool, extra []string) {
	for _, a := range args {
		if a == ingestStatusLineFlag {
			statusLine = true
			continue
		}
		extra = append(extra, a)
	}
	return statusLine, extra
}

// ingestReading converts an accepted document into what the store takes.
func ingestReading(d usage.Doc) store.Reading {
	r := store.Reading{SessionID: d.SessionID, At: d.At}
	if d.HasCost {
		r.CostUSD = &d.CostUSD
	}
	if d.HasCacheExpiry {
		r.CacheExpiresAt = d.CacheExpiresAt
	}
	if d.HasCacheRebuild {
		r.CacheRebuildUSD = &d.CacheRebuildUSD
	}
	if d.HasCacheRecache {
		r.CacheRecacheTokens = &d.CacheRecacheTokens
	}
	for _, w := range []model.UsageWindow{model.WindowFiveHour, model.WindowSevenDay} {
		if l, ok := d.Limits[w]; ok {
			r.Limits = append(r.Limits, model.UsageSample{Window: w, UsedPct: l.UsedPct, ResetsAt: l.ResetsAt})
		}
	}
	return r
}

// ingestRun reads one document from stdin (statusLine: Claude Code's
// status-line JSON), stores it and returns it. It never panics outward: a
// panic is recovered and returned as an error. It returns by ctx's
// deadline: reading stdin and the store's work run apart and are given up
// on, which a stdin that never closes or a store another process holds
// would otherwise prevent. An accepted document is returned with a store
// error, so the status line can still show it. The store is checked
// against the same now as the parser, since it refuses a whole reading
// the parser would keep with another.
func ingestRun(ctx context.Context, stdin io.Reader, statusLine bool) (doc usage.Doc, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %s", sanitize.Line(fmt.Sprint(r), 200))
		}
	}()
	raw, err := ingestBounded(ctx, "read stdin", func() ([]byte, error) {
		return io.ReadAll(io.LimitReader(stdin, usage.MaxDoc+1))
	})
	if err != nil {
		return usage.Doc{}, err
	}
	now := clock()
	parse := usage.ParseDoc
	if statusLine {
		parse = usage.FromStatusLine
	}
	if doc, err = parse(raw, now); err != nil {
		return usage.Doc{}, err
	}
	r := ingestReading(doc)
	_, err = ingestBounded(ctx, "store", func() (struct{}, error) { return struct{}{}, ingestStore(ctx, r, now) })
	return doc, err
}

// ingestBounded runs f apart and returns its result, or ctx's error when
// ctx ends first (f is then left to finish or to end with the process). A
// panic in f comes back as an error.
func ingestBounded[T any](ctx context.Context, what string, f func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("%s: panic: %s", what, sanitize.Line(fmt.Sprint(r), 200))}
			}
		}()
		v, err := f()
		done <- result{v, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return res.v, fmt.Errorf("%s: %w", what, res.err)
		}
		return res.v, nil
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("%s: %w", what, ctx.Err())
	}
}

// ingestOpen opens the configured store and returns it with the func that
// closes it. A store that does not exist yet is not created: the first
// status line comes before the first hook, which creates it. The latency
// test replaces it with one open store, as TestHookLatency opens one.
var ingestOpen = func() (*store.Store, func(), error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	if _, err = os.Stat(cfg.DBPath); err != nil {
		return nil, nil, fmt.Errorf("not opened: %w", err)
	}
	st, err := openStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	return st, func() { _ = st.Close() }, nil
}

// errIngestUnknownSession is why a reading was not stored: its session id
// is not a session a hook created. Anything can run `curia statusline`
// with any id, and the status line may run before the session's first
// hook; either way nothing is stored and no row is created.
var errIngestUnknownSession = errors.New("unknown session")

// ingestStore stores r in one transaction (store.Ingest) and returns
// errIngestUnknownSession when the store knows no such session.
func ingestStore(ctx context.Context, r store.Reading, now time.Time) error {
	st, closeStore, err := ingestOpen()
	if err != nil {
		return err
	}
	defer closeStore()
	res, err := st.Ingest(ctx, r, now)
	if err == nil && !res.SessionFound {
		return errIngestUnknownSession
	}
	return err
}

// The log's rate limit: a problem of one kind is logged at most once per
// ingestLogEvery, however many runs meet it (a run is a process: the state
// is the file ingestThrottleFile next to the log). A kind that no run has
// met for ingestLogQuiet counts as cleared and is logged at once when it
// returns. The first occurrence is always logged.
const (
	ingestLogEvery     = time.Minute
	ingestLogQuiet     = 10 * time.Second
	ingestLogRefresh   = time.Second // how often a persistent problem's "last seen" is written
	ingestThrottleFile = ".throttle"
	ingestThrottleMax  = 1024 // the most of the file that is read
)

// ingestProblem is one thing a run has to log: its kind (a fixed word),
// the message and the attributes, all already cleaned and bounded.
type ingestProblem struct {
	kind, msg string
	attrs     []any
}

// ingestProblems lists what a run has to log, never a value or the
// document.
func ingestProblems(cmdName string, doc usage.Doc, err error, extra []string) []ingestProblem {
	var ps []ingestProblem
	if len(extra) > 0 {
		ps = append(ps, ingestProblem{
			"arguments", cmdName + " ignored arguments",
			[]any{"args", sanitize.Line(strings.Join(extra, " "), 120)},
		})
	}
	fields := sanitize.Line(strings.Join(doc.Dropped, " "), 120)
	reason := ""
	if err != nil {
		reason = sanitize.Line(err.Error(), 200)
	}
	switch {
	case errors.Is(err, usage.ErrDoc):
		ps = append(ps, ingestProblem{"refused", cmdName + " document refused", []any{"err", reason}})
	case errors.Is(err, errIngestUnknownSession):
		ps = append(ps, ingestProblem{"unknown_session", cmdName + " unknown session, nothing stored", nil})
	case err != nil:
		ps = append(ps, ingestProblem{"failed", cmdName + " failed", []any{"err", reason, "dropped", fields}})
	case fields != "":
		ps = append(ps, ingestProblem{"dropped", cmdName + " dropped fields", []any{"fields", fields}})
	}
	return ps
}

// ingestThrottle returns the problems to log now and records, in the file
// at path, when each kind was last logged and last met ("kind logged seen",
// unix seconds, one line per kind, at most a few). The file is replaced
// atomically, and only when it changes (a suppressed repeat is one read).
// It does not depend on the store, and any failure to read or write it
// only means more lines, never a failed run.
func ingestThrottle(path string, ps []ingestProblem, now time.Time) []ingestProblem {
	old := ingestThrottleRead(path)
	next := make(map[string][2]int64, len(ps))
	var out []ingestProblem
	t := now.Unix()
	for _, p := range ps {
		prev, known := old[p.kind]
		logged, seen := prev[0], prev[1]
		fresh := !known || t < logged || t-logged >= int64(ingestLogEvery/time.Second) || t-seen >= int64(ingestLogQuiet/time.Second)
		if fresh {
			out = append(out, p)
			logged, seen = t, t
		} else if t-seen >= int64(ingestLogRefresh/time.Second) {
			seen = t
		}
		next[p.kind] = [2]int64{logged, seen}
	}
	// A kind this run did not meet keeps its entry until it has been quiet
	// long enough to count as cleared: runs of other sessions must not
	// reset the bound.
	for kind, v := range old {
		if _, met := next[kind]; !met && t >= v[1] && t-v[1] < int64(ingestLogQuiet/time.Second) {
			next[kind] = v
		}
	}
	if !maps.Equal(old, next) {
		ingestThrottleWrite(path, next)
	}
	return out
}

func ingestThrottleRead(path string) map[string][2]int64 {
	m := map[string][2]int64{}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return m
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, ingestThrottleMax))
	for _, line := range strings.Split(string(b), "\n") {
		var kind string
		var logged, seen int64
		if n, _ := fmt.Sscanf(line, "%s %d %d", &kind, &logged, &seen); n == 3 {
			m[kind] = [2]int64{logged, seen}
		}
	}
	return m
}

func ingestThrottleWrite(path string, m map[string][2]int64) {
	if len(m) == 0 {
		_ = os.Remove(path)
		return
	}
	kinds := make([]string, 0, len(m))
	for k := range m {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	var b strings.Builder
	for _, k := range kinds {
		fmt.Fprintf(&b, "%s %d %d\n", k, m[k][0], m[k][1])
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return
	}
	_, werr := tmp.WriteString(b.String())
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// ingestLog writes, at WARN, why a run stored nothing or less than it was
// given: the error's reason, the names of dropped fields and that
// arguments were ignored; never a value or the document. Nothing is
// written, and the log not opened, when all went well or when the same
// kind of problem was logged lately (ingestThrottle).
func ingestLog(cmdName string, doc usage.Doc, err error, extra []string) {
	problems := ingestProblems(cmdName, doc, err, extra)
	if len(problems) == 0 {
		return // the usual run: no config read, no file touched
	}
	cfg, cerr := loadConfig()
	if cerr != nil {
		cfg = config.Default()
	}
	marker := filepath.Clean(cfg.LogPath) + ingestThrottleFile
	ps := ingestThrottle(marker, problems, ingestLogNow())
	if len(ps) == 0 {
		return
	}
	logger, closeLog, lerr := openLog(cfg.LogPath)
	if lerr != nil {
		return
	}
	defer func() { _ = closeLog() }()
	for _, p := range ps {
		logger.Warn(p.msg, p.attrs...)
	}
}

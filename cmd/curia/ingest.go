package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
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
	r := store.Reading{SessionID: d.SessionID, At: d.At, ContextSize: d.ContextSize, ContextPct: d.ContextPct}
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

// ingestStore stores r in one transaction (store.Ingest).
func ingestStore(ctx context.Context, r store.Reading, now time.Time) error {
	st, closeStore, err := ingestOpen()
	if err != nil {
		return err
	}
	defer closeStore()
	_, err = st.Ingest(ctx, r, now)
	return err
}

// ingestLog writes, at WARN, why a run stored nothing or less than it was
// given: the error's reason, the names of dropped fields and that
// arguments were ignored; never a value or the document. Nothing is
// written, and the log not opened, when all went well.
func ingestLog(cmdName string, doc usage.Doc, err error, extra []string) {
	if err == nil && len(doc.Dropped) == 0 && len(extra) == 0 {
		return
	}
	cfg, cerr := loadConfig()
	if cerr != nil {
		cfg = config.Default()
	}
	logger, closeLog, lerr := openLog(cfg.LogPath)
	if lerr != nil {
		return
	}
	defer func() { _ = closeLog() }()
	if len(extra) > 0 {
		logger.Warn(cmdName+" ignored arguments", "args", sanitize.Line(strings.Join(extra, " "), 120))
	}
	fields := strings.Join(doc.Dropped, " ")
	switch {
	case errors.Is(err, usage.ErrDoc):
		logger.Warn(cmdName+" document refused", "err", err.Error())
	case err != nil:
		logger.Warn(cmdName+" failed", "err", err.Error(), "dropped", fields)
	case fields != "":
		logger.Warn(cmdName+" dropped fields", "fields", fields)
	}
}

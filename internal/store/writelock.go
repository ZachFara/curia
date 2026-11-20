package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SQLite's wait for the write lock (busy_timeout) is a poll: a writer that
// finds the lock taken sleeps 1, 2, 5 ... then 100 ms between tries and
// never queues, while a writer that arrives, or commits and writes again,
// takes a free lock at once. Under steady contention, or while a holder is
// slowed by a loaded machine (a slow fsync, a descheduled process), a
// waiter can miss every one of its tries and fail with SQLITE_BUSY after
// busy_timeout although each holder kept the lock only briefly. Curia's
// hooks are separate processes writing one file at the same moments, so
// every write of a Store first takes an exclusive flock(2) on a lock file
// next to the database, the writer lock. A process blocked in flock is
// woken when the lock is released, so it tries at every release instead of
// every 100 ms, and the wait for SQLite's own lock that follows is short.
//
// The writer lock is taken around every write transaction (BeginTx) and
// every statement run outside one (Exec, and Query unless it is a SELECT),
// by a driver.Conn wrapper, so no write path can bypass it. Reads never
// take it. Writers that do not take it (an older binary, the sqlite3 shell)
// are still kept out by SQLite's lock and busy_timeout, as before.

// writerLockSuffix names the writer lock file: the database path plus it.
const writerLockSuffix = "-writer"

// errWriterLockTimeout is returned when the writer lock is not free within
// busyTimeout, as SQLITE_BUSY is when SQLite's own lock is not.
var errWriterLockTimeout = errors.New("writer lock busy")

// writerLock is the writer lock of one database file.
type writerLock struct {
	path string
}

// gates holds, per lock file, the in-process queue in front of the flock.
// Within one process a flock is only as fair as the kernel and the Go
// scheduler make it: a writer that releases and writes again takes it back
// before a woken waiter runs. A channel hands its slot to the longest
// waiting sender, so goroutines of this process (any number of Stores on
// one file) go through the flock one by one, in arrival order.
var gates = struct {
	sync.Mutex
	m map[string]chan struct{} // capacity 1
}{m: map[string]chan struct{}{}}

func (w writerLock) gate() chan struct{} {
	gates.Lock()
	defer gates.Unlock()
	g, ok := gates.m[w.path]
	if !ok {
		g = make(chan struct{}, 1)
		gates.m[w.path] = g
	}
	return g
}

// lock takes the writer lock, waiting at most busyTimeout or until ctx is
// done, and returns its release, which must be called exactly once.
// Every call opens the lock file afresh: a flock belongs to the open file,
// so two connections of one process exclude each other as two processes do.
func (w writerLock) lock(ctx context.Context) (func(), error) {
	gate := w.gate()
	timer := time.NewTimer(busyTimeout)
	defer timer.Stop()
	select {
	case gate <- struct{}{}:
	case <-timer.C:
		return nil, errWriterLockTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release, err := w.flock(ctx, timer)
	if err != nil {
		<-gate
		return nil, err
	}
	return func() { release(); <-gate }, nil
}

// flock takes the flock itself, waiting until timer fires or ctx is done.
func (w writerLock) flock(ctx context.Context, timer *time.Timer) (func(), error) {
	f, err := os.OpenFile(w.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open writer lock: %w", err)
	}
	fd := int(f.Fd())
	release := func() { _ = f.Close() } // closing the file releases the flock
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return release, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		release()
		return nil, fmt.Errorf("writer lock: %w", err)
	}
	got := make(chan error, 1)
	go func() { got <- syscall.Flock(fd, syscall.LOCK_EX) }()
	select {
	case err = <-got:
		if err != nil {
			release()
			return nil, fmt.Errorf("writer lock: %w", err)
		}
		return release, nil
	case <-timer.C:
		err = errWriterLockTimeout
	case <-ctx.Done():
		err = ctx.Err()
	}
	// The blocked flock cannot be interrupted: it releases the lock as
	// soon as it gets it.
	go func() { e := <-got; _ = e; release() }()
	return nil, err
}

// writerConnector opens connections that take the writer lock for writes.
type writerConnector struct {
	driver.Connector
	lock writerLock
}

// newWriterConnector returns the connector Open uses for the database at abs.
func newWriterConnector(c driver.Connector, abs string) writerConnector {
	return writerConnector{Connector: c, lock: writerLock{path: filepath.Clean(abs + writerLockSuffix)}}
}

// Connect opens a connection wrapped in writerConn.
func (c writerConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	sc, ok := conn.(sqliteConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("store: driver connection %T lacks a method the writer lock wraps", conn)
	}
	return &writerConn{conn: sc, lock: c.lock}, nil
}

// sqliteConn is what every connection of modernc.org/sqlite implements.
type sqliteConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// writerConn is a driver connection that holds the writer lock for each
// write transaction and each writing statement outside one. It forwards
// the optional interfaces every connection of modernc.org/sqlite has.
// database/sql uses a connection from one goroutine at a time, so held
// needs no lock of its own.
type writerConn struct {
	conn sqliteConn
	lock writerLock
	held func() // the release of a transaction's writer lock, nil outside one
}

// Unwrap returns the driver's own connection.
func (c *writerConn) Unwrap() driver.Conn { return c.conn }

// Prepare prepares query. Statements are executed through ExecContext and
// QueryContext; a prepared statement is not used by the Store.
func (c *writerConn) Prepare(query string) (driver.Stmt, error) { return c.conn.Prepare(query) }

// PrepareContext prepares query (see Prepare).
func (c *writerConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.conn.PrepareContext(ctx, query)
}

// Close releases a writer lock still held and closes the connection.
func (c *writerConn) Close() error {
	c.unhold()
	return c.conn.Close()
}

// Begin begins a write transaction with the default options.
func (c *writerConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx takes the writer lock, unless the transaction is read-only, and
// begins the transaction (BEGIN IMMEDIATE, from the DSN), which holds the
// lock until it commits or rolls back.
func (c *writerConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	begin := c.conn.BeginTx
	if opts.ReadOnly {
		return begin(ctx, opts)
	}
	release, err := c.lock.lock(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := begin(ctx, opts)
	if err != nil {
		release()
		return nil, err
	}
	c.held = release
	return writerTx{tx: tx, conn: c}, nil
}

// unhold releases the writer lock a transaction holds, if any.
func (c *writerConn) unhold() {
	if c.held != nil {
		c.held()
		c.held = nil
	}
}

// ExecContext runs query, holding the writer lock for it outside a
// transaction.
func (c *writerConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	exec := c.conn.ExecContext
	if c.held != nil {
		return exec(ctx, query, args)
	}
	release, err := c.lock.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return exec(ctx, query, args)
}

// QueryContext runs query. Outside a transaction a statement that is not
// a SELECT (an UPDATE ... RETURNING, a PRAGMA that changes the file) holds
// the writer lock until its rows are closed, when SQLite ends it.
func (c *writerConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q := c.conn.QueryContext
	if c.held != nil || isSelect(query) {
		return q(ctx, query, args)
	}
	release, err := c.lock.lock(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q(ctx, query, args)
	if err != nil {
		release()
		return nil, err
	}
	return &writerRows{Rows: rows, release: release}, nil
}

// isSelect reports whether query is a plain SELECT, which never writes.
func isSelect(query string) bool {
	f := strings.Fields(query)
	return len(f) > 0 && strings.EqualFold(f[0], "SELECT")
}

// Ping checks the connection.
func (c *writerConn) Ping(ctx context.Context) error { return c.conn.Ping(ctx) }

// ResetSession prepares the connection for reuse.
func (c *writerConn) ResetSession(ctx context.Context) error {
	return c.conn.ResetSession(ctx)
}

// IsValid reports whether the connection may be reused.
func (c *writerConn) IsValid() bool { return c.conn.IsValid() }

// writerTx releases its connection's writer lock when it ends.
type writerTx struct {
	tx   driver.Tx
	conn *writerConn
}

// Commit commits the transaction and releases the writer lock.
func (t writerTx) Commit() error {
	defer t.conn.unhold()
	return t.tx.Commit()
}

// Rollback rolls the transaction back and releases the writer lock.
func (t writerTx) Rollback() error {
	defer t.conn.unhold()
	return t.tx.Rollback()
}

// writerRows releases the writer lock its statement holds when closed.
type writerRows struct {
	driver.Rows
	once    sync.Once
	release func()
}

// Close closes the rows, which ends the statement, and releases the lock.
func (r *writerRows) Close() error {
	defer r.once.Do(r.release)
	return r.Rows.Close()
}

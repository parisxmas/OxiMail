package store

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/parisxmas/OxiDB/go/oxidb"
)

// dbConn wraps one *oxidb.Client with transparent reconnect-on-broken-pipe.
//
// The upstream client holds a single long-lived TCP connection and does
// NOT redial when it dies. Any blip — oxidb-server restart, network
// timeout, the OS pruning a half-open socket — therefore poisons every
// subsequent call with `write tcp ...: write: broken pipe` until the
// oximail process is itself restarted. We saw this in prod on
// 2026-05-21: webmail login returned 401, IMAP returned `NO Temporary
// authentication failure`, and the queue runner logged a broken-pipe
// once per 30s — all because Authenticate -> GetAccount -> oxidb.FindOne
// was writing into a corpse socket. A `docker compose restart oximail`
// fixed it instantly.
//
// A call that fails with a connection-dead error (EPIPE / ECONNRESET /
// EOF / "use of closed network connection") always redials, so the next
// call finds a live socket; whether the failed call is also re-run
// depends on the operation — see runDB. Logical OxiDB errors ("no such
// collection", transaction conflicts, validation) are passed through
// unchanged so we don't paper over real bugs by reconnecting.
type dbConn struct {
	host    string
	port    int
	timeout time.Duration

	// mu serialises reconnect attempts so a thundering herd of
	// concurrent broken-pipe callers redial at most once. The upstream
	// oxidb.Client has its own per-call mutex around send/recv, so this
	// only gates the conn-replacement.
	mu  sync.Mutex
	cur atomic.Pointer[oxidb.Client]

	// lastUsed is only touched by the goroutine that has this conn
	// checked out of dbClient.free.
	lastUsed time.Time
}

// dbClient is a small fixed pool of dbConns. oxidb.Client holds its
// mutex for a whole request/response, so one connection means one
// in-flight OxiDB call for the entire process. A call checks a conn out
// for its duration, so it never waits behind a slow neighbour while
// another conn is idle. No store operation relies on connection
// affinity (no transactions, no per-connection server state).
type dbClient struct {
	conns []*dbConn
	free  chan *dbConn
}

// dial creates a single-connection dbClient. If the initial Connect
// fails we propagate the error — there's no useful work to defer to a
// later retry, the daemon should refuse to start.
func dial(host string, port int, timeout time.Duration) (*dbClient, error) {
	return dialPool(host, port, timeout, 1)
}

// dialPool creates a dbClient with `size` live connections.
func dialPool(host string, port int, timeout time.Duration, size int) (*dbClient, error) {
	c := &dbClient{free: make(chan *dbConn, size)}
	for i := 0; i < size; i++ {
		cli, err := oxidb.Connect(host, port, timeout)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		conn := &dbConn{host: host, port: port, timeout: timeout, lastUsed: time.Now()}
		conn.cur.Store(cli)
		c.conns = append(c.conns, conn)
		c.free <- conn
	}
	return c, nil
}

// reconnect dials a fresh oxidb client and atomically swaps it in. If
// another goroutine already replaced the connection since `prev` was
// observed, we skip the dial and return the live one — this keeps a
// burst of concurrent failures from opening N+1 sockets.
func (c *dbConn) reconnect(prev *oxidb.Client) (*oxidb.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.cur.Load(); live != prev {
		return live, nil
	}
	fresh, err := oxidb.Connect(c.host, c.port, c.timeout)
	if err != nil {
		return nil, fmt.Errorf("store: oxidb reconnect to %s:%d: %w", c.host, c.port, err)
	}
	_ = prev.Close()
	c.cur.Store(fresh)
	return fresh, nil
}

// probeIfIdle pings a connection that has sat unused for a while and
// redials it if the ping fails. A write that must not be re-run would
// otherwise be the call that discovers a socket the server dropped
// during a quiet spell, and it could only report the failure.
func (c *dbConn) probeIfIdle() {
	if time.Since(c.lastUsed) < idleProbeAfter {
		return
	}
	cli := c.cur.Load()
	_ = cli.SetDeadline(time.Now().Add(idleProbeTimeout))
	_, err := cli.Ping()
	_ = cli.SetDeadline(time.Time{})
	if err != nil {
		// A failed redial is not reported here: the caller's own call
		// on the old socket surfaces the outage.
		_, _ = c.reconnect(cli)
	}
}

// Close shuts every oxidb connection. dbClient is single-shot after
// Close — there's no resurrection path because every consumer in this
// codebase opens the store once at startup and never closes it until
// shutdown.
func (c *dbClient) Close() error {
	var first error
	for _, conn := range c.conns {
		if cli := conn.cur.Load(); cli != nil {
			if err := cli.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// isConnDead reports whether err is the kind of TCP-level failure that
// a fresh connection would recover from. We deliberately do NOT treat
// `i/o timeout` as dead: that's also how a slow query surfaces, and
// silently re-running it would mask the real problem.
func isConnDead(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ECONNRESET):
		return true
	}
	// String fallback. The oxidb client wraps low-level errors with
	// fmt.Errorf("oxidb: send: %w", ...), and on darwin the inner
	// net.OpError often doesn't unwrap cleanly to a syscall sentinel.
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "connection refused")
}

// sendFailed reports whether a connection-dead error came from writing
// the request. oxidb frames are length-prefixed, so a request whose
// write failed never reached the server as a whole frame and was not
// executed. Any later failure (`oxidb: read length: ...`) is ambiguous:
// the server may have committed the write and died before replying.
func sendFailed(err error) bool {
	return strings.HasPrefix(err.Error(), "oxidb: send:")
}

// runDB checks a connection out and runs fn on it. On a connection-dead
// error it always redials. It re-runs fn only when that cannot apply
// the operation twice: when retrySafe is set, or when the request
// provably never reached the server. If the redial fails, the original
// error is the one returned.
func runDB[T any](c *dbClient, retrySafe bool, fn func(*oxidb.Client) (T, error)) (T, error) {
	conn := <-c.free
	defer func() {
		conn.lastUsed = time.Now()
		c.free <- conn
	}()
	if !retrySafe {
		conn.probeIfIdle()
	}
	cli := conn.cur.Load()
	v, err := fn(cli)
	if err == nil || !isConnDead(err) {
		return v, err
	}
	fresh, rerr := conn.reconnect(cli)
	if rerr != nil {
		var zero T
		return zero, err
	}
	if !retrySafe && !sendFailed(err) {
		var zero T
		return zero, err
	}
	return fn(fresh)
}

// callDB runs an operation that is safe to repeat: a read, or a write
// whose second application changes nothing. Any other error (including
// a successful reconnect followed by another failure) is returned to
// the caller as-is.
func callDB[T any](c *dbClient, fn func(*oxidb.Client) (T, error)) (T, error) {
	return runDB(c, true, fn)
}

// callDBOnce runs an operation that must not be applied twice, such as
// an insert. If the connection dies after the request went out the
// caller gets the error, because the write may or may not have landed;
// the connection is still replaced for the next call.
func callDBOnce[T any](c *dbClient, fn func(*oxidb.Client) (T, error)) (T, error) {
	return runDB(c, false, fn)
}

// callDBErr is the same as callDB but for methods that only return an
// error. Saves the caller from threading a meaningless zero value.
func callDBErr(c *dbClient, fn func(*oxidb.Client) error) error {
	_, err := callDB(c, func(cli *oxidb.Client) (struct{}, error) {
		return struct{}{}, fn(cli)
	})
	return err
}

// ------------------------------------------------------------------
// Wrapped methods. Surface mirrors *oxidb.Client one-to-one for the
// subset the store actually uses. Each one picks callDB or callDBOnce:
// Insert and any $inc-style update are not repeatable; reads, DDL
// (callers accept "already exists"), PutObject (the key is fixed, so a
// second write overwrites identical bytes), and deletes are.
// ------------------------------------------------------------------

func (c *dbClient) Insert(coll string, doc map[string]any) (map[string]any, error) {
	return callDBOnce(c, func(cli *oxidb.Client) (map[string]any, error) {
		return cli.Insert(coll, doc)
	})
}

func (c *dbClient) Find(coll string, query map[string]any, opts *oxidb.FindOptions) ([]map[string]any, error) {
	return callDB(c, func(cli *oxidb.Client) ([]map[string]any, error) {
		return cli.Find(coll, query, opts)
	})
}

func (c *dbClient) FindOne(coll string, query map[string]any) (map[string]any, error) {
	return callDB(c, func(cli *oxidb.Client) (map[string]any, error) {
		return cli.FindOne(coll, query)
	})
}

func (c *dbClient) FindAndModify(coll string, query, update map[string]any) (map[string]any, error) {
	fn := func(cli *oxidb.Client) (map[string]any, error) {
		return cli.FindAndModify(coll, query, update)
	}
	if isPureSet(update) {
		return callDB(c, fn)
	}
	return callDBOnce(c, fn)
}

// isPureSet reports whether an update only assigns fixed values, which
// makes applying it twice the same as applying it once. A blob refcount
// or used_bytes $inc does not qualify.
func isPureSet(update map[string]any) bool {
	_, set := update["$set"]
	return set && len(update) == 1
}

// AllocCounter is FindAndModify for the UID and mod-sequence allocators.
// It may repeat the $inc after a dead connection: the only cost of a
// double increment there is a skipped number, which NextUID and
// NextModSeq both accept, and these calls open most write paths.
func (c *dbClient) AllocCounter(coll string, query, update map[string]any) (map[string]any, error) {
	return callDB(c, func(cli *oxidb.Client) (map[string]any, error) {
		return cli.FindAndModify(coll, query, update)
	})
}

func (c *dbClient) Delete(coll string, query map[string]any) (map[string]any, error) {
	return callDB(c, func(cli *oxidb.Client) (map[string]any, error) {
		return cli.Delete(coll, query)
	})
}

func (c *dbClient) Count(coll string, query map[string]any) (int, error) {
	return callDB(c, func(cli *oxidb.Client) (int, error) {
		return cli.Count(coll, query)
	})
}

func (c *dbClient) CreateCollection(name string) error {
	return callDBErr(c, func(cli *oxidb.Client) error { return cli.CreateCollection(name) })
}

func (c *dbClient) DropCollection(name string) error {
	return callDBErr(c, func(cli *oxidb.Client) error { return cli.DropCollection(name) })
}

func (c *dbClient) CreateIndex(coll, field string) error {
	return callDBErr(c, func(cli *oxidb.Client) error { return cli.CreateIndex(coll, field) })
}

func (c *dbClient) CreateUniqueIndex(coll, field string) error {
	return callDBErr(c, func(cli *oxidb.Client) error { return cli.CreateUniqueIndex(coll, field) })
}

func (c *dbClient) CreateCompositeIndex(coll string, fields []string) error {
	return callDBErr(c, func(cli *oxidb.Client) error { return cli.CreateCompositeIndex(coll, fields) })
}

func (c *dbClient) CreateBucket(bucket string) error {
	return callDBErr(c, func(cli *oxidb.Client) error { return cli.CreateBucket(bucket) })
}

func (c *dbClient) PutObject(bucket, key string, data []byte, contentType string, metadata map[string]string) (map[string]any, error) {
	return callDB(c, func(cli *oxidb.Client) (map[string]any, error) {
		return cli.PutObject(bucket, key, data, contentType, metadata)
	})
}

func (c *dbClient) DeleteObject(bucket, key string) error {
	return callDBErr(c, func(cli *oxidb.Client) error { return cli.DeleteObject(bucket, key) })
}

// idleProbeAfter is how long a connection may sit unused before a
// non-repeatable call pings it first. idleProbeTimeout bounds that ping.
const (
	idleProbeAfter   = 30 * time.Second
	idleProbeTimeout = 2 * time.Second
)

// getObjectResult bundles GetObject's two-value payload so it can ride
// the generic callDB helper. The wrapper unbundles it back to the
// caller's idiomatic (data, meta, err) signature.
type getObjectResult struct {
	data []byte
	meta map[string]any
}

func (c *dbClient) GetObject(bucket, key string) ([]byte, map[string]any, error) {
	r, err := callDB(c, func(cli *oxidb.Client) (getObjectResult, error) {
		d, m, err := cli.GetObject(bucket, key)
		return getObjectResult{data: d, meta: m}, err
	})
	return r.data, r.meta, err
}

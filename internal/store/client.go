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

// dbClient wraps an *oxidb.Client with transparent reconnect-on-broken-pipe.
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
// dbClient redials once when a call fails with a connection-dead error
// (EPIPE / ECONNRESET / EOF / "use of closed network connection") and
// retries the same call exactly once. Logical OxiDB errors ("no such
// collection", transaction conflicts, validation) are passed through
// unchanged so we don't paper over real bugs by reconnecting.
type dbClient struct {
	host    string
	port    int
	timeout time.Duration

	// mu serialises reconnect attempts so a thundering herd of
	// concurrent broken-pipe callers redial at most once. The upstream
	// oxidb.Client has its own per-call mutex around send/recv, so this
	// only gates the conn-replacement.
	mu  sync.Mutex
	cur atomic.Pointer[oxidb.Client]
}

// dial creates a dbClient with an initial live connection. If the
// initial Connect fails we propagate the error — there's no useful work
// to defer to a later retry, the daemon should refuse to start.
func dial(host string, port int, timeout time.Duration) (*dbClient, error) {
	cli, err := oxidb.Connect(host, port, timeout)
	if err != nil {
		return nil, err
	}
	c := &dbClient{host: host, port: port, timeout: timeout}
	c.cur.Store(cli)
	return c, nil
}

// reconnect dials a fresh oxidb client and atomically swaps it in. If
// another goroutine already replaced the connection since `prev` was
// observed, we skip the dial and return the live one — this keeps a
// burst of concurrent failures from opening N+1 sockets.
func (c *dbClient) reconnect(prev *oxidb.Client) (*oxidb.Client, error) {
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

// Close shuts the active oxidb connection. dbClient is single-shot
// after Close — there's no resurrection path because every consumer in
// this codebase opens the store once at startup and never closes it
// until shutdown.
func (c *dbClient) Close() error {
	if cli := c.cur.Load(); cli != nil {
		return cli.Close()
	}
	return nil
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

// callDB runs fn against the live oxidb client. If it fails with a
// connection-dead error we redial once and retry; any other error
// (including a successful reconnect followed by another failure) is
// returned to the caller as-is.
func callDB[T any](c *dbClient, fn func(*oxidb.Client) (T, error)) (T, error) {
	cli := c.cur.Load()
	v, err := fn(cli)
	if err == nil || !isConnDead(err) {
		return v, err
	}
	fresh, rerr := c.reconnect(cli)
	if rerr != nil {
		var zero T
		return zero, err
	}
	return fn(fresh)
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
// subset the store actually uses.
// ------------------------------------------------------------------

func (c *dbClient) Insert(coll string, doc map[string]any) (map[string]any, error) {
	return callDB(c, func(cli *oxidb.Client) (map[string]any, error) {
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

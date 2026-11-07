package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/parisxmas/OxiDB/go/oxidb"
)

func TestIsConnDead(t *testing.T) {
	// Sentinel errors we treat as dead-connection — these are the
	// signatures of a TCP socket whose other end vanished.
	for _, e := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		net.ErrClosed,
		syscall.EPIPE,
		syscall.ECONNRESET,
		fmt.Errorf("oxidb: send: write tcp 192.168.80.3:33856->192.168.80.2:4444: write: broken pipe"),
		fmt.Errorf("oxidb: read length: read tcp: connection reset by peer"),
		fmt.Errorf("oxidb: send: use of closed network connection"),
		// Wrapping survives the unwrap check.
		fmt.Errorf("layer one: %w", fmt.Errorf("layer two: %w", syscall.EPIPE)),
	} {
		if !isConnDead(e) {
			t.Errorf("isConnDead(%v) = false, want true", e)
		}
	}

	// Errors we MUST NOT treat as dead — silently retrying these
	// would hide real bugs. Timeouts in particular get a pass: a slow
	// query is not a dead socket, and reconnecting would lose
	// whatever state the original call was building up.
	for _, e := range []error{
		nil,
		errors.New("oxidb: no such collection 'foo'"),
		errors.New("transaction conflict: try again"),
		errors.New("read tcp 127.0.0.1:993->127.0.0.1:36532: i/o timeout"),
	} {
		if isConnDead(e) {
			t.Errorf("isConnDead(%v) = true, want false", e)
		}
	}
}

// fakeOxidb is a TCP listener that accepts connections and closes them
// immediately. It satisfies the only thing dbClient needs from
// oxidb-server during this test: that the address is dialable.
type fakeOxidb struct {
	t        *testing.T
	listener net.Listener
	port     int

	mu       sync.Mutex
	accepted []net.Conn
}

func newFakeOxidb(t *testing.T) *fakeOxidb {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeOxidb{
		t:        t,
		listener: l,
		port:     l.Addr().(*net.TCPAddr).Port,
	}
	go f.acceptLoop()
	t.Cleanup(func() { f.close() })
	return f
}

func (f *fakeOxidb) acceptLoop() {
	for {
		c, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.accepted = append(f.accepted, c)
		f.mu.Unlock()
	}
}

func (f *fakeOxidb) acceptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accepted)
}

// waitForAccepts blocks until the listener has seen `want` connections
// or the deadline expires. The accept loop is async, so a sleep-poll
// is the cheapest sync primitive — tests should call this whenever
// they care about the count.
func (f *fakeOxidb) waitForAccepts(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.acceptCount() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("only saw %d accepts, want %d", f.acceptCount(), want)
}

func (f *fakeOxidb) close() {
	_ = f.listener.Close()
	f.mu.Lock()
	for _, c := range f.accepted {
		_ = c.Close()
	}
	f.mu.Unlock()
}

func TestReconnectSwapsLiveClient(t *testing.T) {
	// reconnect() must redial AND atomic-swap c.cur, so subsequent
	// callers see the fresh connection without taking the slow path.
	f := newFakeOxidb(t)

	c, err := dial("127.0.0.1", f.port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	prev := c.conns[0].cur.Load()
	if prev == nil {
		t.Fatal("initial connection is nil")
	}
	f.waitForAccepts(t, 1)

	fresh, rerr := c.conns[0].reconnect(prev)
	if rerr != nil {
		t.Fatalf("reconnect: %v", rerr)
	}
	if fresh == prev {
		t.Fatal("reconnect returned the same client pointer")
	}
	if c.conns[0].cur.Load() != fresh {
		t.Fatal("reconnect did not store the fresh client")
	}
	f.waitForAccepts(t, 2)
}

func TestReconnectIsRaceFree(t *testing.T) {
	// Many goroutines hitting reconnect with the same stale `prev`
	// should redial exactly once. The first caller that wins the
	// mutex replaces c.cur; everyone else observes the new value and
	// returns it without dialing.
	f := newFakeOxidb(t)

	c, err := dial("127.0.0.1", f.port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	prev := c.conns[0].cur.Load()

	const callers = 32
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			if _, err := c.conns[0].reconnect(prev); err != nil {
				t.Errorf("reconnect: %v", err)
			}
		}()
	}
	wg.Wait()

	// 1 = initial dial, 1 = the single coalesced redial.
	f.waitForAccepts(t, 2)
	// Give late accepts a beat — a thundering-herd bug would surface as
	// extra connections after this point, not before.
	time.Sleep(50 * time.Millisecond)
	if got := f.acceptCount(); got != 2 {
		t.Fatalf("accept count = %d, want 2 (initial + one coalesced reconnect)", got)
	}
}

func TestCallDBRetriesOnBrokenPipe(t *testing.T) {
	// callDB must transparently redial and re-run fn exactly once
	// when the first call surfaces a connection-dead error.
	f := newFakeOxidb(t)
	c, err := dial("127.0.0.1", f.port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var attempts atomic.Int32
	got, err := callDB(c, func(cli *oxidb.Client) (string, error) {
		n := attempts.Add(1)
		if n == 1 {
			// Pretend the underlying connection just died.
			return "", fmt.Errorf("oxidb: send: write tcp x->y: write: broken pipe")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("callDB: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got = %q, want %q", got, "ok")
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2 (one fail + one retry)", n)
	}
}

func TestCallDBDoesNotRetryOnLogicalError(t *testing.T) {
	// A logical OxiDB error — "no such collection", a validation
	// failure, a transaction conflict — must reach the caller on the
	// first try. Re-running it would just double the load on a
	// healthy server and mask the bug.
	f := newFakeOxidb(t)
	c, err := dial("127.0.0.1", f.port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var attempts atomic.Int32
	logical := errors.New("oxidb: no such collection 'whatever'")
	_, err = callDB(c, func(cli *oxidb.Client) (struct{}, error) {
		attempts.Add(1)
		return struct{}{}, logical
	})
	if !errors.Is(err, logical) {
		t.Fatalf("err = %v, want %v", err, logical)
	}
	if n := attempts.Load(); n != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on logical errors)", n)
	}
}

func TestCallDBSurfacesOriginalErrorWhenReconnectFails(t *testing.T) {
	// If reconnect can't reach a live oxidb (server still down), the
	// caller should see the original network error — that's the
	// useful diagnostic — not the secondary "couldn't redial" noise.
	f := newFakeOxidb(t)
	c, err := dial("127.0.0.1", f.port, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Kill the fake server so the redial fails.
	f.close()

	original := fmt.Errorf("oxidb: send: write tcp 1.2.3.4->5.6.7.8: write: broken pipe")
	_, err = callDB(c, func(cli *oxidb.Client) (struct{}, error) {
		return struct{}{}, original
	})
	if err == nil {
		t.Fatal("expected an error after reconnect failure")
	}
	if !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("err = %v, want the original broken-pipe error preserved", err)
	}
}

// frameServer speaks just enough of the oxidb framing to count requests.
// The first `drop` connections die: each reads one request, as if the
// server executed it, and closes without replying. Later connections
// answer every request with `reply`.
type frameServer struct {
	listener net.Listener
	port     int
	reply    string
	frames   atomic.Int32 // requests received, across all connections

	mu   sync.Mutex
	drop int
}

func newFrameServer(t *testing.T, drop int, reply string) *frameServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &frameServer{listener: l, port: l.Addr().(*net.TCPAddr).Port, reply: reply, drop: drop}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			dying := f.drop > 0
			if dying {
				f.drop--
			}
			f.mu.Unlock()
			go f.serve(c, dying)
		}
	}()
	return f
}

func (f *frameServer) serve(c net.Conn, dying bool) {
	defer c.Close()
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		if _, err := io.ReadFull(c, make([]byte, binary.LittleEndian.Uint32(hdr[:]))); err != nil {
			return
		}
		f.frames.Add(1)
		if dying {
			return
		}
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(f.reply)))
		if _, err := c.Write(append(hdr[:], f.reply...)); err != nil {
			return
		}
	}
}

func TestDeadConnectionRetryRules(t *testing.T) {
	const reply = `{"ok":true,"data":{"id":1,"_id":1}}`
	inc := map[string]any{"$inc": map[string]any{"count": 1}}
	set := map[string]any{"$set": map[string]any{"count": 1}}

	for _, tc := range []struct {
		name  string
		call  func(c *dbClient) error
		retry bool
	}{
		{"Insert", func(c *dbClient) error {
			_, err := c.Insert("x", map[string]any{"a": 1})
			return err
		}, false},
		{"FindAndModify $inc", func(c *dbClient) error {
			_, err := c.FindAndModify("x", map[string]any{}, inc)
			return err
		}, false},
		{"FindAndModify $set", func(c *dbClient) error {
			_, err := c.FindAndModify("x", map[string]any{}, set)
			return err
		}, true},
		{"AllocCounter", func(c *dbClient) error {
			_, err := c.AllocCounter("x", map[string]any{}, inc)
			return err
		}, true},
		{"PutObject", func(c *dbClient) error {
			_, err := c.PutObject("b", "k", []byte("x"), "", nil)
			return err
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrameServer(t, 1, reply)
			c, err := dial("127.0.0.1", f.port, time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.Close()

			err = tc.call(c)
			if tc.retry {
				if err != nil {
					t.Fatalf("repeatable call failed: %v", err)
				}
				if n := f.frames.Load(); n != 2 {
					t.Fatalf("server saw %d requests, want 2 (original + retry)", n)
				}
				return
			}
			if err == nil {
				t.Fatal("call that may have been applied reported success")
			}
			if n := f.frames.Load(); n != 1 {
				t.Fatalf("server saw %d requests, want 1: a non-repeatable write was re-sent", n)
			}
			// No retry, but the connection must still have been healed.
			if err := tc.call(c); err != nil {
				t.Fatalf("next call on the redialed connection: %v", err)
			}
		})
	}
}

func TestCallDBOnceRetriesOnlyUnsentRequests(t *testing.T) {
	f := newFakeOxidb(t)
	c, err := dial("127.0.0.1", f.port, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	for _, tc := range []struct {
		failure  string
		attempts int32
	}{
		{"oxidb: send: write tcp x->y: write: broken pipe", 2},
		{"oxidb: read length: EOF", 1},
		{"oxidb: read payload: read tcp: connection reset by peer", 1},
	} {
		var attempts atomic.Int32
		_, _ = callDBOnce(c, func(cli *oxidb.Client) (string, error) {
			if attempts.Add(1) == 1 {
				return "", errors.New(tc.failure)
			}
			return "ok", nil
		})
		if n := attempts.Load(); n != tc.attempts {
			t.Errorf("%q: attempts = %d, want %d", tc.failure, n, tc.attempts)
		}
	}
}

func TestIdleConnectionIsProbedBeforeAnInsert(t *testing.T) {
	// The server dropped the connection during a quiet spell. The
	// insert must not be the call that finds out, since it could not
	// be re-run.
	f := newFrameServer(t, 1, `{"ok":true,"data":{"id":1}}`)
	c, err := dial("127.0.0.1", f.port, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.conns[0].lastUsed = time.Now().Add(-2 * idleProbeAfter)

	if _, err := c.Insert("x", map[string]any{"a": 1}); err != nil {
		t.Fatalf("insert after idle: %v", err)
	}
}

func TestPoolRunsCallsConcurrently(t *testing.T) {
	f := newFakeOxidb(t)
	c, err := dialPool("127.0.0.1", f.port, time.Second, 3)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	f.waitForAccepts(t, 3)

	var wg sync.WaitGroup
	release := make(chan struct{})
	var entered atomic.Int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = callDB(c, func(*oxidb.Client) (struct{}, error) {
				entered.Add(1)
				<-release
				return struct{}{}, nil
			})
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for entered.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	n := entered.Load()
	close(release)
	wg.Wait()
	if n != 3 {
		t.Fatalf("%d calls in flight at once, want 3", n)
	}
}

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

	prev := c.cur.Load()
	if prev == nil {
		t.Fatal("initial connection is nil")
	}
	f.waitForAccepts(t, 1)

	fresh, rerr := c.reconnect(prev)
	if rerr != nil {
		t.Fatalf("reconnect: %v", rerr)
	}
	if fresh == prev {
		t.Fatal("reconnect returned the same client pointer")
	}
	if c.cur.Load() != fresh {
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

	prev := c.cur.Load()

	const callers = 32
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			if _, err := c.reconnect(prev); err != nil {
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

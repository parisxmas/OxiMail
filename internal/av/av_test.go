package av

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeDaemon is a stand-in clamd-protocol server we drive in
// tests. It accepts INSTREAM (only — Scan never sends anything
// else), captures the streamed payload, and replies with the
// scripted Verdict response. fakeDaemon.respond is set by each
// test; the default is "stream: OK".
type fakeDaemon struct {
	t      *testing.T
	socket string

	mu          sync.Mutex
	lastPayload []byte
	respond     string // "stream: OK" / "stream: NAME FOUND" / "stream: bad ERROR"
	hangup      bool   // when true, close the conn before sending a reply (transport error)

	listener net.Listener
	done     chan struct{}
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "avd.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeDaemon{
		t:        t,
		socket:   sock,
		respond:  "stream: OK",
		listener: l,
		done:     make(chan struct{}),
	}
	go f.accept()
	t.Cleanup(func() {
		_ = l.Close()
		_ = os.Remove(sock)
		<-f.done
	})
	return f
}

func (f *fakeDaemon) accept() {
	defer close(f.done)
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeDaemon) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	// First nine bytes are "zINSTREAM\0".
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	if string(hdr) != "zINSTREAM\x00" {
		return
	}

	// Stream loop: 4-byte length, then N bytes, until length == 0.
	var collected []byte
	for {
		var lb [4]byte
		if _, err := io.ReadFull(conn, lb[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(lb[:])
		if n == 0 {
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		collected = append(collected, buf...)
	}

	f.mu.Lock()
	f.lastPayload = collected
	hangup := f.hangup
	resp := f.respond
	f.mu.Unlock()

	if hangup {
		return
	}
	_, _ = conn.Write([]byte(resp + "\x00"))
}

func TestScanClean(t *testing.T) {
	f := newFakeDaemon(t)
	f.mu.Lock()
	f.respond = "stream: OK"
	f.mu.Unlock()

	c := New(f.socket, time.Second)
	v, err := c.Scan(context.Background(), []byte("hello, world"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !v.OK {
		t.Errorf("OK = false, want true")
	}
	if v.Threat != "" {
		t.Errorf("Threat = %q, want empty", v.Threat)
	}
	if got := string(f.lastPayload); got != "hello, world" {
		t.Errorf("daemon got %q, want %q", got, "hello, world")
	}
}

func TestScanFound(t *testing.T) {
	f := newFakeDaemon(t)
	f.mu.Lock()
	f.respond = "stream: EICAR-Test-File FOUND"
	f.mu.Unlock()

	c := New(f.socket, time.Second)
	v, err := c.Scan(context.Background(), []byte("eicar bytes here"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.OK {
		t.Errorf("OK = true, want false for FOUND")
	}
	if v.Threat != "EICAR-Test-File" {
		t.Errorf("Threat = %q, want EICAR-Test-File", v.Threat)
	}
}

func TestScanMultiWordThreat(t *testing.T) {
	// Daemon-side names can include spaces — the parser must
	// preserve them and only strip the trailing " FOUND".
	f := newFakeDaemon(t)
	f.mu.Lock()
	f.respond = "stream: Win.Trojan.Generic Sample FOUND"
	f.mu.Unlock()

	c := New(f.socket, time.Second)
	v, err := c.Scan(context.Background(), []byte("x"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if v.Threat != "Win.Trojan.Generic Sample" {
		t.Errorf("Threat = %q, want full multi-word name", v.Threat)
	}
}

func TestScanError(t *testing.T) {
	// Daemon reports a scan-level error (corrupt input, etc.).
	// We surface it as a Go error, not a silent clean verdict —
	// the caller chooses fail-open vs fail-closed.
	f := newFakeDaemon(t)
	f.mu.Lock()
	f.respond = "stream: PARSE FAILED ERROR"
	f.mu.Unlock()

	c := New(f.socket, time.Second)
	_, err := c.Scan(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected error on daemon ERROR response")
	}
}

func TestScanUnreachable(t *testing.T) {
	// Socket path that doesn't exist — must error with
	// ErrUnreachable so handlers can branch on fail-open vs
	// fail-closed by checking errors.Is.
	c := New("/no/such/socket/avd.sock", 200*time.Millisecond)
	_, err := c.Scan(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable", err)
	}
}

func TestScanHangup(t *testing.T) {
	// Daemon closes the connection mid-protocol. Same fail-open /
	// fail-closed surface as a hard unreachable: wrapped under
	// ErrUnreachable.
	f := newFakeDaemon(t)
	f.mu.Lock()
	f.hangup = true
	f.mu.Unlock()

	c := New(f.socket, 500*time.Millisecond)
	_, err := c.Scan(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("expected error on early hangup")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want wrapped ErrUnreachable", err)
	}
}

func TestNewDisabled(t *testing.T) {
	// Empty socket path → nil client, which is the "AV disabled"
	// idiom. Scan on a nil receiver short-circuits to clean.
	c := New("", 0)
	if c != nil {
		t.Fatal("New(\"\") should return nil")
	}
	// nil.Scan is the documented contract; calling it should not
	// panic and should return clean.
	v, err := c.Scan(context.Background(), []byte("anything"))
	if err != nil {
		t.Fatalf("nil.Scan: %v", err)
	}
	if !v.OK {
		t.Errorf("nil.Scan OK = false, want true")
	}
}

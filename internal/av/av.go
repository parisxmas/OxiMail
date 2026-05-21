// Package av is a thin clamd-protocol client OxiMail uses to scan
// outbound webmail attachments against the avd sidecar before relay.
// The complement to rspamd's antivirus module on inbound mail
// (Phase A): rspamd guards the receiver, this package guards what
// we send.
//
// Wire protocol — clamd INSTREAM with the zero-terminated framing
// variant:
//
//	zINSTREAM\0
//	<uint32 BE: chunk length><chunk bytes>...
//	<uint32 BE: 0>                       ← terminator
//	→  stream: OK\0                      ← clean
//	→  stream: NAME FOUND\0              ← virus signature matched
//
// The daemon source we run (parisxmas/antivirus) accepts the same
// format. We use a fresh connection per scan: simpler than pooling
// and the per-call cost is one Unix-socket dial (~negligible).
package av

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Verdict is the result of a single scan. Threat is the human-
// readable signature name on a positive hit, empty on a clean
// response. OK is the boolean view for callers that don't care
// about the name.
type Verdict struct {
	OK     bool
	Threat string
}

// ErrUnreachable is returned when the daemon socket can't be
// dialed or the protocol exchange fails. Callers can decide
// whether to fail open (log + accept) or fail closed (reject the
// upload) based on their Required setting.
var ErrUnreachable = errors.New("av: daemon unreachable")

// Client speaks INSTREAM to a clamd-compatible daemon over a Unix
// socket. Stateless and goroutine-safe — each Scan call opens its
// own connection. A nil *Client never errors; Scan returns OK.
// That null-object shape lets handlers gate AV-on/off purely via
// "do we have a client" without conditional branching at every
// call site.
type Client struct {
	socket  string
	timeout time.Duration
}

// New returns a Client dialing the given Unix socket. An empty
// socket path yields nil — the canonical "AV disabled" state.
// Timeout is the per-scan deadline; zero defaults to 10s, which
// fits an avd scan of a 25 MB attachment with plenty of margin
// (typical real-world scan is ~50ms).
func New(socket string, timeout time.Duration) *Client {
	if socket == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{socket: socket, timeout: timeout}
}

// Scan submits data to the daemon and returns the verdict. A nil
// receiver short-circuits to a clean verdict — that's the "AV
// disabled" path. Errors are exclusively transport-level
// (ErrUnreachable wrapped); a daemon that replies with a virus
// match returns no error, just Verdict{OK: false, Threat: name}.
func (c *Client) Scan(ctx context.Context, data []byte) (Verdict, error) {
	if c == nil {
		return Verdict{OK: true}, nil
	}

	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	dialer := &net.Dialer{}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	conn, err := dialer.DialContext(dctx, "unix", c.socket)
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: dial %s: %v", ErrUnreachable, c.socket, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	// zINSTREAM\0 → tells the daemon "use null-terminated framing
	// for the response", which is what our reader expects below.
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return Verdict{}, fmt.Errorf("%w: write command: %v", ErrUnreachable, err)
	}

	// Send the payload as a single big-endian length-prefixed
	// chunk. clamd supports streaming chunks for memory-bounded
	// scanning, but our caller has the whole attachment in memory
	// already (decoded by handleSend), so one chunk is fine and
	// avoids managing a chunked send loop.
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return Verdict{}, fmt.Errorf("%w: write length: %v", ErrUnreachable, err)
	}
	if len(data) > 0 {
		if _, err := conn.Write(data); err != nil {
			return Verdict{}, fmt.Errorf("%w: write payload: %v", ErrUnreachable, err)
		}
	}
	// Zero-length terminator marks end of stream.
	for i := range lenBuf {
		lenBuf[i] = 0
	}
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return Verdict{}, fmt.Errorf("%w: write terminator: %v", ErrUnreachable, err)
	}

	// Response: one null-terminated line. ReadString returns the
	// bytes including the terminator; we trim it before parsing.
	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString(0)
	if err != nil {
		return Verdict{}, fmt.Errorf("%w: read response: %v", ErrUnreachable, err)
	}
	resp = strings.TrimRight(resp, "\x00\r\n ")
	return parseResponse(resp)
}

// parseResponse turns a clamd reply into a Verdict. The format is
// stable across implementations (clamav, clamdtop, our avd, etc.):
//
//	"stream: OK"                  → clean
//	"stream: <name> FOUND"        → hit
//	"stream: <text> ERROR"        → daemon-side error
func parseResponse(resp string) (Verdict, error) {
	const prefix = "stream: "
	if !strings.HasPrefix(resp, prefix) {
		return Verdict{}, fmt.Errorf("av: unexpected response %q", resp)
	}
	body := resp[len(prefix):]
	switch {
	case body == "OK":
		return Verdict{OK: true}, nil
	case strings.HasSuffix(body, " FOUND"):
		name := strings.TrimSuffix(body, " FOUND")
		return Verdict{OK: false, Threat: name}, nil
	case strings.HasSuffix(body, " ERROR"):
		return Verdict{}, fmt.Errorf("av: daemon error: %s", body)
	default:
		return Verdict{}, fmt.Errorf("av: unparseable response %q", resp)
	}
}

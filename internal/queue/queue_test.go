//go:build integration

// Integration test for the outbound queue worker. It boots a live
// oxidb-server, stands up a throwaway "remote MX" to deliver to, and
// drives the worker through one delivery and one deferral.
//
// This is an internal test (package queue) so it can inject a resolver
// and call runOnce directly. Gated behind the `integration` build tag.
package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/store"
)

func TestQueue(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	t.Run("delivers a queued message to the remote MX", func(t *testing.T) {
		mx, mxAddr := startCaptureMX(t)

		q := New(st, "oximail.test")
		q.resolve = func(string) ([]string, error) { return []string{mxAddr}, nil }

		raw := []byte("From: sender@oximail.test\r\nSubject: outbound\r\n\r\nhello remote\r\n")
		m, err := st.Enqueue("sender@oximail.test", []string{"rcpt@remote.test"}, raw)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		q.runOnce(context.Background())

		got := mx.messages()
		if len(got) != 1 || !bytes.Equal(got[0], raw) {
			t.Fatalf("remote MX received %d message(s), not the raw message", len(got))
		}
		// A fully delivered message is removed from the queue.
		if _, err := st.GetOutbound(m.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("queue row still present after delivery: err = %v", err)
		}
	})

	t.Run("defers a message when the MX is unreachable", func(t *testing.T) {
		// A free port with nothing listening: dialing it is refused.
		deadAddr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))

		q := New(st, "oximail.test")
		q.resolve = func(string) ([]string, error) { return []string{deadAddr}, nil }

		raw := []byte("From: sender@oximail.test\r\nSubject: deferred\r\n\r\nnobody home\r\n")
		m, err := st.Enqueue("sender@oximail.test", []string{"rcpt@unreachable.test"}, raw)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		q.runOnce(context.Background())

		got, err := st.GetOutbound(m.ID)
		if err != nil {
			t.Fatalf("queue row should still exist after a temp failure: %v", err)
		}
		if got.Status != store.OutboundDeferred {
			t.Errorf("status = %q, want %q", got.Status, store.OutboundDeferred)
		}
		if got.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", got.Attempts)
		}
		if len(got.Recipients) != 1 {
			t.Errorf("recipients = %v, want the one undelivered recipient", got.Recipients)
		}
		next, perr := time.Parse(time.RFC3339, got.NextRetryAt)
		if perr != nil || !next.After(time.Now()) {
			t.Errorf("next_retry_at = %q, want a future time", got.NextRetryAt)
		}
		if got.LastError == "" {
			t.Error("last_error is empty, want the dial failure recorded")
		}
	})
}

// -----------------------------------------------------------------------
// captureMX — a throwaway SMTP server that records what it receives
// -----------------------------------------------------------------------

type captureMX struct {
	mu  sync.Mutex
	got [][]byte
}

func (c *captureMX) messages() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.got))
	copy(out, c.got)
	return out
}

func (c *captureMX) NewSession(*gosmtp.Conn) (gosmtp.Session, error) {
	return &captureMXSession{mx: c}, nil
}

type captureMXSession struct{ mx *captureMX }

func (s *captureMXSession) Mail(string, *gosmtp.MailOptions) error { return nil }
func (s *captureMXSession) Rcpt(string, *gosmtp.RcptOptions) error { return nil }

func (s *captureMXSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mx.mu.Lock()
	s.mx.got = append(s.mx.got, raw)
	s.mx.mu.Unlock()
	return nil
}

func (s *captureMXSession) Reset()        {}
func (s *captureMXSession) Logout() error { return nil }

// startCaptureMX runs a capture SMTP server on a free port and returns
// it with its address. It is shut down when the test ends.
func startCaptureMX(t *testing.T) (*captureMX, string) {
	t.Helper()
	mx := &captureMX{}
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := gosmtp.NewServer(mx)
	srv.Addr = addr
	srv.Domain = "capture.test"
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	itest.WaitTCP(t, addr)
	return mx, addr
}

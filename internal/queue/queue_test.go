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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
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

	t.Run("signs outbound mail with the sender domain's DKIM key", func(t *testing.T) {
		mx, mxAddr := startCaptureMX(t)

		// A domain with a freshly generated DKIM key.
		if _, err := st.CreateDomain("signing.test"); err != nil {
			t.Fatalf("create domain: %v", err)
		}
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		privPEM := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
		if err := st.SetDKIMKey("signing.test", "sel", string(privPEM)); err != nil {
			t.Fatalf("set DKIM key: %v", err)
		}

		q := New(st, "oximail.test")
		q.resolve = func(string) ([]string, error) { return []string{mxAddr}, nil }

		raw := []byte("From: alice@signing.test\r\nTo: rcpt@remote.test\r\n" +
			"Subject: signed\r\n\r\nhello\r\n")
		if _, err := st.Enqueue("alice@signing.test", []string{"rcpt@remote.test"}, raw); err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		q.runOnce(context.Background())

		got := mx.messages()
		if len(got) != 1 {
			t.Fatalf("remote MX received %d message(s), want 1", len(got))
		}
		if !bytes.Contains(got[0], []byte("DKIM-Signature:")) {
			t.Fatal("delivered message has no DKIM-Signature header")
		}

		// The signature verifies against the published public key.
		pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		dkimTXT := "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pub)
		verifs, err := dkim.VerifyWithOptions(bytes.NewReader(got[0]), &dkim.VerifyOptions{
			LookupTXT: func(name string) ([]string, error) {
				if name == "sel._domainkey.signing.test" {
					return []string{dkimTXT}, nil
				}
				return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
			},
		})
		if err != nil {
			t.Fatalf("dkim verify: %v", err)
		}
		if len(verifs) != 1 || verifs[0].Err != nil {
			t.Fatalf("DKIM signature did not verify: %+v", verifs)
		}
		if verifs[0].Domain != "signing.test" {
			t.Errorf("signature domain = %q, want signing.test", verifs[0].Domain)
		}
	})

	t.Run("sends unsigned when the sender domain has no key", func(t *testing.T) {
		mx, mxAddr := startCaptureMX(t)

		q := New(st, "oximail.test")
		q.resolve = func(string) ([]string, error) { return []string{mxAddr}, nil }

		raw := []byte("From: bob@nokey.test\r\nTo: rcpt@remote.test\r\n" +
			"Subject: unsigned\r\n\r\nhi\r\n")
		if _, err := st.Enqueue("bob@nokey.test", []string{"rcpt@remote.test"}, raw); err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		q.runOnce(context.Background())

		got := mx.messages()
		if len(got) != 1 {
			t.Fatalf("remote MX received %d message(s), want 1", len(got))
		}
		if bytes.Contains(got[0], []byte("DKIM-Signature:")) {
			t.Error("message from a keyless domain was signed")
		}
	})

	t.Run("bounces a permanent failure back to the sender", func(t *testing.T) {
		rejectAddr := startRejectingMX(t)

		// A local sender, to receive the bounce in their INBOX.
		sender, err := st.CreateAccount("bounce-sender@oximail.test", "h", 0)
		if err != nil {
			t.Fatalf("create sender account: %v", err)
		}
		if err := st.EnsureDefaultMailboxes(sender.ID); err != nil {
			t.Fatalf("ensure mailboxes: %v", err)
		}

		q := New(st, "oximail.test")
		q.resolve = func(string) ([]string, error) { return []string{rejectAddr}, nil }

		raw := []byte("From: bounce-sender@oximail.test\r\nTo: nobody@remote.test\r\n" +
			"Subject: will not arrive\r\n\r\nhi\r\n")
		m, err := st.Enqueue("bounce-sender@oximail.test", []string{"nobody@remote.test"}, raw)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		q.runOnce(context.Background())

		// The original is resolved (permanently failed, then bounced).
		if _, err := st.GetOutbound(m.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("queue row still present after a permanent failure: err = %v", err)
		}
		// A bounce landed in the sender's INBOX.
		inbox, err := st.GetMailboxByName(sender.ID, "INBOX")
		if err != nil {
			t.Fatalf("get sender INBOX: %v", err)
		}
		msgs, err := st.ListMessages(sender.ID, inbox.ID)
		if err != nil {
			t.Fatalf("list sender INBOX: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("sender INBOX has %d messages, want 1 bounce", len(msgs))
		}
		if msgs[0].Subject != "Undelivered Mail Returned to Sender" {
			t.Errorf("bounce subject = %q", msgs[0].Subject)
		}
		body, err := st.FetchBody(&msgs[0])
		if err != nil {
			t.Fatalf("fetch bounce body: %v", err)
		}
		if !bytes.Contains(body, []byte("multipart/report")) {
			t.Error("bounce is not a multipart/report DSN")
		}
		if !bytes.Contains(body, []byte("nobody@remote.test")) {
			t.Error("bounce does not name the failed recipient")
		}
		if !bytes.Contains(body, []byte("will not arrive")) {
			t.Error("bounce does not include the original message")
		}
	})

	t.Run("does not bounce a null-sender message", func(t *testing.T) {
		rejectAddr := startRejectingMX(t)

		q := New(st, "oximail.test")
		q.resolve = func(string) ([]string, error) { return []string{rejectAddr}, nil }

		raw := []byte("From: MAILER-DAEMON@oximail.test\r\nTo: x@remote.test\r\n\r\nbounce body\r\n")
		m, err := st.Enqueue("", []string{"x@remote.test"}, raw)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		q.runOnce(context.Background())

		// A null-sender message that fails is dropped, not bounced —
		// bouncing it would loop. It is simply removed from the queue.
		if _, err := st.GetOutbound(m.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("null-sender message still in the queue: err = %v", err)
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

// -----------------------------------------------------------------------
// rejectingMX — an SMTP server that permanently rejects every recipient
// -----------------------------------------------------------------------

type rejectBackend struct{}

func (rejectBackend) NewSession(*gosmtp.Conn) (gosmtp.Session, error) {
	return rejectSession{}, nil
}

type rejectSession struct{}

func (rejectSession) Mail(string, *gosmtp.MailOptions) error { return nil }

func (rejectSession) Rcpt(string, *gosmtp.RcptOptions) error {
	return &gosmtp.SMTPError{
		Code:         550,
		EnhancedCode: gosmtp.EnhancedCode{5, 1, 1},
		Message:      "no such user here",
	}
}

func (rejectSession) Data(io.Reader) error { return nil }
func (rejectSession) Reset()               {}
func (rejectSession) Logout() error        { return nil }

// startRejectingMX runs an SMTP server that 5xx-rejects every recipient,
// on a free port. It is shut down when the test ends.
func startRejectingMX(t *testing.T) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := gosmtp.NewServer(rejectBackend{})
	srv.Addr = addr
	srv.Domain = "reject.test"
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	itest.WaitTCP(t, addr)
	return addr
}

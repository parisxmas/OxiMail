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
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	gosmtp "github.com/emersion/go-smtp"
	dto "github.com/prometheus/client_model/go"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/mtasts"
	"github.com/parisxmas/OxiMail/internal/observability"
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

// openStore boots a private OxiDB, so each test sees an empty queue.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return st
}

// localSender creates an account for bounces to land in.
func localSender(t *testing.T, st *store.Store, address string) *store.Account {
	t.Helper()
	acc, err := st.CreateAccount(address, "h", 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("ensure mailboxes: %v", err)
	}
	return acc
}

// inboxBodies returns the raw messages in an account's INBOX.
func inboxBodies(t *testing.T, st *store.Store, accountID uint64) []string {
	t.Helper()
	inbox, err := st.GetMailboxByName(accountID, "INBOX")
	if err != nil {
		t.Fatalf("get INBOX: %v", err)
	}
	msgs, err := st.ListMessages(accountID, inbox.ID)
	if err != nil {
		t.Fatalf("list INBOX: %v", err)
	}
	var out []string
	for i := range msgs {
		body, err := st.FetchBody(&msgs[i])
		if err != nil {
			t.Fatalf("fetch body: %v", err)
		}
		out = append(out, string(body))
	}
	return out
}

// noPolicy keeps a test's MTA-STS lookup off the real network.
func noPolicy() *mtasts.Cache { return mtasts.NewCacheWithLookuper(fixedPolicy{}) }

func TestRetriesOutliveTheOldFiveAttempts(t *testing.T) {
	st := openStore(t)
	dead := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	q := New(st, "oximail.test")
	q.mtasts = noPolicy()
	q.resolve = func(string) ([]string, error) { return []string{dead}, nil }

	m, err := st.Enqueue("a@oximail.test", []string{"r@remote.test"}, []byte("From: a@oximail.test\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Four attempts have been made; the fifth used to abandon it.
	if err := st.DeferOutbound(m.ID, m.Recipients, 4, time.Now().Add(-time.Minute), "x"); err != nil {
		t.Fatalf("defer: %v", err)
	}
	q.runOnce(context.Background())

	got, err := st.GetOutbound(m.ID)
	if err != nil {
		t.Fatalf("message gone after attempt 5: %v", err)
	}
	if got.Attempts != 5 {
		t.Errorf("attempts = %d, want 5", got.Attempts)
	}
}

func TestBounceAfterMaxQueueAge(t *testing.T) {
	st := openStore(t)
	sender := localSender(t, st, "old@oximail.test")
	dead := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))

	q := New(st, "oximail.test")
	q.mtasts = noPolicy()
	q.resolve = func(string) ([]string, error) { return []string{dead}, nil }
	q.now = func() time.Time { return time.Now().Add(maxQueueAge + time.Hour) }

	m, err := st.Enqueue("old@oximail.test", []string{"r@remote.test"},
		[]byte("From: old@oximail.test\r\nSubject: too late\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	q.runOnce(context.Background())

	if _, err := st.GetOutbound(m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queue row survived its deadline: err = %v", err)
	}
	bodies := inboxBodies(t, st, sender.ID)
	if len(bodies) != 1 {
		t.Fatalf("sender INBOX has %d messages, want 1 bounce", len(bodies))
	}
	if !strings.Contains(bodies[0], "Status: 5.4.7") {
		t.Errorf("no connection ever made: want Status 5.4.7 in\n%s", bodies[0])
	}
}

// The schedule itself: backoff doubles to the 6h cap, the cap is
// actually reached, and following it lasts about five days.
func TestRetrySchedule(t *testing.T) {
	if got := backoff(1); got != 5*time.Minute {
		t.Errorf("backoff(1) = %v, want 5m", got)
	}
	if got := backoff(4); got != 40*time.Minute {
		t.Errorf("backoff(4) = %v, want 40m", got)
	}
	if got := backoff(20); got != maxBackoff {
		t.Errorf("backoff(20) = %v, want %v", got, maxBackoff)
	}

	created := time.Now()
	deadline := created.Add(maxQueueAge)
	now, attempts := created, 0
	for {
		attempts++
		if retriesSpent(now, deadline, true, attempts) {
			break
		}
		next := now.Add(backoff(attempts))
		if next.After(deadline) {
			next = deadline
		}
		now = next
	}
	if got := now.Sub(created); got != maxQueueAge {
		t.Errorf("gave up after %v, want %v", got, maxQueueAge)
	}
	if attempts < 20 || attempts > maxAttemptsUndated+2 {
		t.Errorf("%d attempts over five days, want around 26", attempts)
	}

	// A row with no readable date falls back to counting attempts.
	if retriesSpent(created, time.Time{}, false, maxAttemptsUndated-1) {
		t.Error("undated row gave up early")
	}
	if !retriesSpent(created, time.Time{}, false, maxAttemptsUndated) {
		t.Error("undated row never gives up")
	}
}

// The last retry lands on the deadline itself rather than past it.
func TestLastRetryIsClampedToDeadline(t *testing.T) {
	st := openStore(t)
	dead := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	q := New(st, "oximail.test")
	q.mtasts = noPolicy()
	q.resolve = func(string) ([]string, error) { return []string{dead}, nil }
	q.now = func() time.Time { return time.Now().Add(maxQueueAge - time.Hour) }

	m, err := st.Enqueue("a@oximail.test", []string{"r@remote.test"}, []byte("From: a@oximail.test\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := st.DeferOutbound(m.ID, m.Recipients, 20, time.Now().Add(-time.Minute), "x"); err != nil {
		t.Fatalf("defer: %v", err)
	}
	q.runOnce(context.Background())

	got, err := st.GetOutbound(m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	next, _ := time.Parse(time.RFC3339, got.NextRetryAt)
	created, _ := time.Parse(time.RFC3339, got.CreatedAt)
	if want := created.Add(maxQueueAge); !next.Equal(want) {
		t.Errorf("next retry %v, want the deadline %v", next, want)
	}
}

// codedMX refuses every recipient with a fixed SMTP error.
type codedMX struct{ err *gosmtp.SMTPError }

func (b codedMX) NewSession(*gosmtp.Conn) (gosmtp.Session, error) { return codedSession(b), nil }

type codedSession codedMX

func (codedSession) Mail(string, *gosmtp.MailOptions) error   { return nil }
func (s codedSession) Rcpt(string, *gosmtp.RcptOptions) error { return s.err }
func (codedSession) Data(io.Reader) error                     { return nil }
func (codedSession) Reset()                                   {}
func (codedSession) Logout() error                            { return nil }

func startCodedMX(t *testing.T, e *gosmtp.SMTPError) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := gosmtp.NewServer(codedMX{err: e})
	srv.Addr = addr
	srv.Domain = "coded.test"
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })
	itest.WaitTCP(t, addr)
	return addr
}

func TestBounceCarriesRemoteStatus(t *testing.T) {
	st := openStore(t)

	t.Run("permanent code is kept", func(t *testing.T) {
		sender := localSender(t, st, "perm@oximail.test")
		addr := startCodedMX(t, &gosmtp.SMTPError{Code: 552, EnhancedCode: gosmtp.EnhancedCode{5, 2, 2}, Message: "mailbox full"})
		q := New(st, "oximail.test")
		q.mtasts = noPolicy()
		q.resolve = func(string) ([]string, error) { return []string{addr}, nil }
		if _, err := st.Enqueue("perm@oximail.test", []string{"r@remote.test"}, []byte("From: perm@oximail.test\r\n\r\nhi\r\n")); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		q.runOnce(context.Background())
		bodies := inboxBodies(t, st, sender.ID)
		if len(bodies) != 1 || !strings.Contains(bodies[0], "Status: 5.2.2") {
			t.Fatalf("want one bounce with Status 5.2.2, got %q", bodies)
		}
		if strings.Contains(bodies[0], "Status: 5.0.0") {
			t.Error("bounce still has the generic 5.0.0")
		}
	})

	t.Run("expired temporary code becomes permanent", func(t *testing.T) {
		sender := localSender(t, st, "temp@oximail.test")
		addr := startCodedMX(t, &gosmtp.SMTPError{Code: 452, EnhancedCode: gosmtp.EnhancedCode{4, 2, 2}, Message: "over quota"})
		q := New(st, "oximail.test")
		q.mtasts = noPolicy()
		q.resolve = func(string) ([]string, error) { return []string{addr}, nil }
		q.now = func() time.Time { return time.Now().Add(maxQueueAge + time.Hour) }
		if _, err := st.Enqueue("temp@oximail.test", []string{"r@remote.test"}, []byte("From: temp@oximail.test\r\n\r\nhi\r\n")); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		q.runOnce(context.Background())
		bodies := inboxBodies(t, st, sender.ID)
		if len(bodies) != 1 || !strings.Contains(bodies[0], "Status: 5.2.2") {
			t.Fatalf("want one bounce with Status 5.2.2, got %q", bodies)
		}
	})
}

func TestDSNStatus(t *testing.T) {
	for in, want := range map[string]string{"": "5.0.0", "5.1.1": "5.1.1", "4.2.2": "5.2.2", "5.1.10": "5.1.10"} {
		if got := dsnStatus(in); got != want {
			t.Errorf("dsnStatus(%q) = %q, want %q", in, got, want)
		}
	}
	if got := enhancedStatus(errors.New("dial failed")); got != "" {
		t.Errorf("non-SMTP error status = %q", got)
	}
	if got := enhancedStatus(&gosmtp.SMTPError{Code: 550, Message: "no"}); got != "" {
		t.Errorf("reply with no enhanced code: status = %q", got)
	}
}

// A body that cannot be read must reach the sender as a bounce, not
// vanish.
func TestUnreadableBodyIsBounced(t *testing.T) {
	st := openStore(t)
	sender := localSender(t, st, "lost@oximail.test")
	q := New(st, "oximail.test")
	q.mtasts = noPolicy()
	q.resolve = func(string) ([]string, error) { return nil, errors.New("must not be reached") }

	m, err := st.Enqueue("lost@oximail.test", []string{"r@remote.test"}, []byte("From: lost@oximail.test\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	broken := *m
	broken.BodyBlob = "does-not-exist"
	q.deliver(&broken)

	if _, err := st.GetOutbound(m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queue row survived: err = %v", err)
	}
	bodies := inboxBodies(t, st, sender.ID)
	if len(bodies) != 1 {
		t.Fatalf("sender INBOX has %d messages, want 1 bounce", len(bodies))
	}
	if !strings.Contains(bodies[0], "r@remote.test") || !strings.Contains(bodies[0], "Status: 5.3.0") {
		t.Errorf("bounce lacks the recipient or status:\n%s", bodies[0])
	}
	if strings.Contains(bodies[0], "message/rfc822") {
		t.Error("bounce has an original-message part with nothing to put in it")
	}
}

// settle keeps a message whose bounce could not be filed, which relies
// on bounce reporting that failure.
func TestBounceReportsFailure(t *testing.T) {
	var stop func()
	host, port := itest.StartOxiDB(t, itest.LazySync(), itest.WithStop(&stop))
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	q := New(st, "oximail.test")
	m, err := st.Enqueue("a@remote.test", []string{"r@remote.test"}, []byte("From: a@remote.test\r\n\r\nhi\r\n"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	stop()
	if err := q.bounce(m, nil, []rcptFailure{{recipient: "r@remote.test", reason: "x"}}); err == nil {
		t.Fatal("bounce with the store down reported success")
	}
}

func TestDeliveryIsLogged(t *testing.T) {
	st := openStore(t)
	_, addr := startCaptureMX(t)
	q := New(st, "oximail.test")
	q.mtasts = noPolicy()
	q.resolve = func(string) ([]string, error) { return []string{addr}, nil }

	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	m, err := st.Enqueue("a@oximail.test", []string{"r@remote.test"},
		[]byte("From: a@oximail.test\r\nSubject: secret-subject\r\n\r\nsecret-body\r\n"))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	q.runOnce(context.Background())

	got := logs.String()
	if want := fmt.Sprintf("message %d: delivered to 1 recipient(s) at remote.test", m.ID); !strings.Contains(got, want) {
		t.Errorf("log lacks %q:\n%s", want, got)
	}
	if strings.Contains(got, "secret-") {
		t.Errorf("log contains message content:\n%s", got)
	}
}

// dueGauge reads the current value of the due-messages gauge.
func dueGauge(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := observability.QueueDue.Write(&m); err != nil {
		t.Fatalf("read gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func TestDueGaugeIsARealCount(t *testing.T) {
	st := openStore(t)
	dead := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	q := New(st, "oximail.test")
	q.mtasts = noPolicy()
	q.resolve = func(string) ([]string, error) { return []string{dead}, nil }

	const total = batchSize + 7
	raw := []byte("From: a@oximail.test\r\n\r\nhi\r\n")
	for i := 0; i < total; i++ {
		if _, err := st.Enqueue("a@oximail.test", []string{"r@remote.test"}, raw); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	q.refreshDue()
	if got := dueGauge(t); got != total {
		t.Fatalf("gauge = %v, want %d: it must not be capped at the batch size", got, total)
	}

	// One run defers a batch into the future; only the rest are due.
	q.runOnce(context.Background())
	if got := dueGauge(t); got != total-batchSize {
		t.Fatalf("gauge after a run = %v, want %d", got, total-batchSize)
	}
}

func TestResolveMX(t *testing.T) {
	notFound := &net.DNSError{Err: "no such host", IsNotFound: true}
	servfail := &net.DNSError{Err: "server misbehaving", IsTemporary: true}
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	hasHost := func(string) ([]string, error) { return []string{"192.0.2.1"}, nil }
	noHost := func(string) ([]string, error) { return nil, notFound }
	mx := func(records []*net.MX, err error) func(string) ([]*net.MX, error) {
		return func(string) ([]*net.MX, error) { return records, err }
	}

	t.Run("uses MX records in order", func(t *testing.T) {
		got, err := resolveMX("d.test", mx([]*net.MX{{Host: "a.d.test.", Pref: 1}, {Host: "b.d.test.", Pref: 2}}, nil), noHost)
		if err != nil || len(got) != 2 || got[0] != "a.d.test:25" || got[1] != "b.d.test:25" {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("no MX falls back to the domain", func(t *testing.T) {
		got, err := resolveMX("d.test", mx(nil, notFound), hasHost)
		if err != nil || len(got) != 1 || got[0] != "d.test:25" {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("SERVFAIL defers", func(t *testing.T) {
		got, err := resolveMX("d.test", mx(nil, servfail), hasHost)
		if !errors.Is(err, servfail) || got != nil {
			t.Fatalf("got %v, %v, want the DNS error", got, err)
		}
	})
	t.Run("timeout defers", func(t *testing.T) {
		if _, err := resolveMX("d.test", mx(nil, timeout), hasHost); !errors.Is(err, timeout) {
			t.Fatalf("err = %v, want the DNS error", err)
		}
	})
	t.Run("null MX", func(t *testing.T) {
		if _, err := resolveMX("d.test", mx([]*net.MX{{Host: ".", Pref: 0}}, nil), hasHost); !errors.Is(err, errNullMX) {
			t.Fatalf("err = %v, want errNullMX", err)
		}
	})
	t.Run("no records of any kind", func(t *testing.T) {
		if _, err := resolveMX("d.test", mx(nil, notFound), noHost); !errors.Is(err, errNoSuchDomain) {
			t.Fatalf("err = %v, want errNoSuchDomain", err)
		}
	})
	t.Run("address lookup temp failure defers", func(t *testing.T) {
		_, err := resolveMX("d.test", mx(nil, notFound), func(string) ([]string, error) { return nil, servfail })
		if !errors.Is(err, servfail) {
			t.Fatalf("err = %v, want the DNS error", err)
		}
	})
	t.Run("usable records beside an error are used", func(t *testing.T) {
		got, err := resolveMX("d.test", mx([]*net.MX{{Host: "a.d.test."}}, servfail), noHost)
		if err != nil || len(got) != 1 {
			t.Fatalf("got %v, %v", got, err)
		}
	})
}

func TestResolveFailuresAreClassified(t *testing.T) {
	st := openStore(t)
	sender := localSender(t, st, "dns@oximail.test")
	raw := []byte("From: dns@oximail.test\r\n\r\nhi\r\n")

	q := New(st, "oximail.test")
	q.mtasts = noPolicy()

	t.Run("null MX bounces at once as 5.1.10", func(t *testing.T) {
		q.resolve = func(string) ([]string, error) { return nil, errNullMX }
		m, err := st.Enqueue("dns@oximail.test", []string{"r@null.test"}, raw)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		q.runOnce(context.Background())
		if _, err := st.GetOutbound(m.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("queue row survived: err = %v", err)
		}
		bodies := inboxBodies(t, st, sender.ID)
		if len(bodies) != 1 || !strings.Contains(bodies[0], "Status: 5.1.10") {
			t.Fatalf("want one bounce with Status 5.1.10, got %q", bodies)
		}
	})

	t.Run("temporary DNS error defers", func(t *testing.T) {
		q.resolve = func(string) ([]string, error) {
			return nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}
		}
		m, err := st.Enqueue("dns@oximail.test", []string{"r@flaky.test"}, raw)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		q.runOnce(context.Background())
		got, err := st.GetOutbound(m.ID)
		if err != nil {
			t.Fatalf("message gone on a temporary DNS error: %v", err)
		}
		if got.Status != store.OutboundDeferred {
			t.Errorf("status = %q, want deferred", got.Status)
		}
	})
}

// Relayed mail is not ours to vouch for: a forged From of a hosted
// domain must go out unsigned, while our own bounces are still signed.
func TestRelayedMailIsNotDKIMSigned(t *testing.T) {
	st := openStore(t)
	if _, err := st.CreateDomain("hosted.test"); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := st.SetDKIMKey("hosted.test", "sel", string(privPEM)); err != nil {
		t.Fatalf("set DKIM key: %v", err)
	}

	cases := []struct {
		name, sender, from string
		signed             bool
	}{
		{"local sender", "a@hosted.test", "a@hosted.test", true},
		{"alias forward", "SRS0=ABCD=AA=remote.test=x@hosted.test", "ceo@hosted.test", false},
		{"forwarded null sender", "", "ceo@hosted.test", false},
		{"own bounce", "", "MAILER-DAEMON@hosted.test", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mx, addr := startCaptureMX(t)
			q := New(st, "hosted.test")
			q.mtasts = noPolicy()
			q.resolve = func(string) ([]string, error) { return []string{addr}, nil }
			raw := []byte("From: " + c.from + "\r\nTo: r@remote.test\r\nSubject: s\r\n\r\nhi\r\n")
			if _, err := st.Enqueue(c.sender, []string{"r@remote.test"}, raw); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			q.runOnce(context.Background())
			got := mx.messages()
			if len(got) != 1 {
				t.Fatalf("remote MX received %d messages, want 1", len(got))
			}
			if signed := bytes.Contains(got[0], []byte("DKIM-Signature:")); signed != c.signed {
				t.Errorf("signed = %v, want %v", signed, c.signed)
			}
		})
	}
}

// A display name in a charset net/mail cannot decode must not cost the
// message its signature.
func TestFromHeaderDomainIgnoresDisplayNameCharset(t *testing.T) {
	raw := []byte("From: =?windows-1254?Q?Bar=FD=FE?= <a@Hosted.test>\r\n\r\nhi\r\n")
	if got := fromHeaderDomain(raw); got != "hosted.test" {
		t.Errorf("fromHeaderDomain = %q, want hosted.test", got)
	}
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

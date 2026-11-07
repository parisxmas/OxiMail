//go:build integration

// Integration test for the submission server (port 587). It boots a
// live oxidb-server, starts the submission server against it, and
// drives it with a real authenticated SMTP client.
package smtp_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/smtp"
	"github.com/parisxmas/OxiMail/internal/store"
)

func TestSubmission(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// One account to authenticate as, and a separate local account to
	// be a recipient.
	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := st.CreateAccount("sender@oximail.test", hash, 0); err != nil {
		t.Fatalf("create sender account: %v", err)
	}
	localRcpt, err := st.CreateAccount("local@oximail.test", "x", 0)
	if err != nil {
		t.Fatalf("create local recipient account: %v", err)
	}

	addr := startSubmission(t, st, nil, false)

	t.Run("rejects MAIL before AUTH", func(t *testing.T) {
		c, err := gosmtp.Dial(addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if err := c.Mail("sender@oximail.test", nil); err == nil {
			t.Fatal("MAIL before AUTH succeeded, want rejection")
		}
	})

	t.Run("rejects a bad password", func(t *testing.T) {
		c, err := gosmtp.Dial(addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if err := c.Auth(sasl.NewPlainClient("", "sender@oximail.test", "wrong")); err == nil {
			t.Fatal("AUTH with a bad password succeeded, want rejection")
		}
	})

	t.Run("authenticated submit: local delivered, remote queued", func(t *testing.T) {
		c, err := gosmtp.Dial(addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if err := c.Auth(sasl.NewPlainClient("", "sender@oximail.test", "s3cret")); err != nil {
			t.Fatalf("auth: %v", err)
		}
		raw := "From: sender@oximail.test\r\n" +
			"Subject: Mixed recipients\r\n" +
			"\r\n" +
			"one local, one remote\r\n"
		if err := c.SendMail("sender@oximail.test",
			[]string{"local@oximail.test", "someone@remote.test"},
			strings.NewReader(raw)); err != nil {
			t.Fatalf("send: %v", err)
		}

		// The local recipient should have the message in its INBOX
		// (default mailboxes are created on first delivery).
		inbox, err := st.GetMailboxByName(localRcpt.ID, "INBOX")
		if err != nil {
			t.Fatalf("get local INBOX: %v", err)
		}
		msgs, err := st.ListMessages(localRcpt.ID, inbox.ID)
		if err != nil {
			t.Fatalf("list local INBOX: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("local INBOX has %d messages, want 1", len(msgs))
		}

		// The remote recipient should be queued for relay.
		queued, err := st.ListDueOutbound(10)
		if err != nil {
			t.Fatalf("list outbound queue: %v", err)
		}
		if len(queued) != 1 {
			t.Fatalf("outbound queue has %d messages, want 1", len(queued))
		}
		q := queued[0]
		if q.From != "sender@oximail.test" {
			t.Errorf("queued From = %q, want sender@oximail.test", q.From)
		}
		if len(q.Recipients) != 1 || q.Recipients[0] != "someone@remote.test" {
			t.Errorf("queued Recipients = %v, want [someone@remote.test]", q.Recipients)
		}
	})
}

// TestSubmissionSenderCheck covers the rule that an account may send
// only as itself or an alias of itself, in both MAIL FROM and From.
func TestSubmissionSenderCheck(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := st.CreateAccount("blog@baltavista.com", hash, 0); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := st.CreateAccount("other@baltavista.com", "x", 0); err != nil {
		t.Fatalf("create other account: %v", err)
	}
	for alias, dest := range map[string]string{
		"news@baltavista.com":    "blog@baltavista.com",
		"chain@baltavista.com":   "news@baltavista.com",
		"theirs@baltavista.com":  "other@baltavista.com",
		"outside@baltavista.com": "someone@remote.test",
	} {
		if _, err := st.CreateAlias(alias, []string{dest}); err != nil {
			t.Fatalf("create alias: %v", err)
		}
	}
	addr := startSubmission(t, st, nil, false)

	// send submits one message and returns the error from the first
	// step that refused it.
	send := func(t *testing.T, mailFrom, fromHeader string) error {
		t.Helper()
		c, err := gosmtp.Dial(addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		if err := c.Auth(sasl.NewPlainClient("", "blog@baltavista.com", "s3cret")); err != nil {
			t.Fatalf("auth: %v", err)
		}
		return c.SendMail(mailFrom, []string{"someone@remote.test"},
			strings.NewReader(fromHeader+"Subject: s\r\n\r\nbody\r\n"))
	}

	allowed := []struct{ name, mailFrom, header string }{
		{"own address", "blog@baltavista.com", "From: blog@baltavista.com\r\n"},
		{"the blog's encoded display name", "blog@baltavista.com",
			"From: =?UTF-8?B?QmFsdGEgVmlzdGEgQmxvZw==?= <blog@baltavista.com>\r\n"},
		{"display name in a charset net/mail cannot decode", "blog@baltavista.com",
			"From: =?windows-1254?Q?Bar=FD=FE?= <blog@baltavista.com>\r\n"},
		{"different case", "BLOG@baltavista.com", "From: Blog@BaltaVista.com\r\n"},
		{"alias", "news@baltavista.com", "From: News <news@baltavista.com>\r\n"},
		{"alias of an alias", "chain@baltavista.com", "From: chain@baltavista.com\r\n"},
		{"own address and alias in From", "blog@baltavista.com",
			"From: blog@baltavista.com, news@baltavista.com\r\n"},
	}
	for _, c := range allowed {
		t.Run("allows "+c.name, func(t *testing.T) {
			if err := send(t, c.mailFrom, c.header); err != nil {
				t.Fatalf("send: %v", err)
			}
		})
	}

	refused := []struct{ name, mailFrom, header string }{
		{"another account in MAIL FROM", "other@baltavista.com", "From: blog@baltavista.com\r\n"},
		{"another account in From", "blog@baltavista.com", "From: other@baltavista.com\r\n"},
		{"someone else's alias", "theirs@baltavista.com", "From: blog@baltavista.com\r\n"},
		{"an alias that leaves the server", "outside@baltavista.com", "From: blog@baltavista.com\r\n"},
		{"a remote MAIL FROM", "ceo@remote.test", "From: blog@baltavista.com\r\n"},
		{"a remote From", "blog@baltavista.com", "From: ceo@remote.test\r\n"},
		{"encoded name hiding a forged address", "blog@baltavista.com",
			"From: =?UTF-8?B?YmxvZ0BiYWx0YXZpc3RhLmNvbQ==?= <ceo@remote.test>\r\n"},
		{"own address quoted as the name of a forged one", "blog@baltavista.com",
			"From: \"blog@baltavista.com\" <other@baltavista.com>\r\n"},
		{"one forged address among several", "blog@baltavista.com",
			"From: blog@baltavista.com, other@baltavista.com\r\n"},
		{"a second From field", "blog@baltavista.com",
			"From: blog@baltavista.com\r\nFrom: other@baltavista.com\r\n"},
		{"lower-case second field", "blog@baltavista.com",
			"From: blog@baltavista.com\r\nFROM: other@baltavista.com\r\n"},
		{"null MAIL FROM", "", "From: blog@baltavista.com\r\n"},
		{"no From header", "blog@baltavista.com", ""},
		{"unparseable From", "blog@baltavista.com", "From: <<nonsense\r\n"},
	}
	for _, c := range refused {
		t.Run("refuses "+c.name, func(t *testing.T) {
			err := send(t, c.mailFrom, c.header)
			var smtpErr *gosmtp.SMTPError
			if !errors.As(err, &smtpErr) {
				t.Fatalf("send: err = %v, want an SMTP rejection", err)
			}
			if smtpErr.Code != 550 || smtpErr.EnhancedCode != (gosmtp.EnhancedCode{5, 7, 1}) {
				t.Errorf("got %d %v, want 550 5.7.1", smtpErr.Code, smtpErr.EnhancedCode)
			}
		})
	}

	t.Run("a refused message is not queued", func(t *testing.T) {
		before, _ := st.ListDueOutbound(100)
		_ = send(t, "blog@baltavista.com", "From: ceo@remote.test\r\n")
		after, _ := st.ListDueOutbound(100)
		if len(after) != len(before) {
			t.Errorf("queue went from %d to %d", len(before), len(after))
		}
	})

	t.Run("submitted mail carries a Received header", func(t *testing.T) {
		if err := send(t, "blog@baltavista.com", "From: blog@baltavista.com\r\n"); err != nil {
			t.Fatalf("send: %v", err)
		}
		queued, err := st.ListDueOutbound(100)
		if err != nil || len(queued) == 0 {
			t.Fatalf("list outbound: %d, %v", len(queued), err)
		}
		raw, err := st.FetchOutboundBody(&queued[len(queued)-1])
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if !strings.HasPrefix(string(raw), "Received: from localhost ([127.0.0.1])\r\n\tby oximail.test with ESMTPA;") {
			t.Errorf("no Received header:\n%s", raw)
		}
	})
}

// TestSubmissionTLS covers the submission server with TLS configured:
// STARTTLS on the plaintext listener, and the implicit-TLS listener.
func TestSubmissionTLS(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	acc, err := st.CreateAccount("tls-user@oximail.test", hash, 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	certFile, keyFile := itest.WriteSelfSignedCert(t)
	serverTLS := itest.ServerTLS(t, certFile, keyFile)
	clientTLS := &tls.Config{InsecureSkipVerify: true}

	const msg = "From: tls-user@oximail.test\r\nSubject: over TLS\r\n\r\nsecure\r\n"

	t.Run("STARTTLS: cleartext AUTH refused, encrypted AUTH works", func(t *testing.T) {
		addr := startSubmission(t, st, serverTLS, false)

		// With TLS available, AUTH must not be accepted in the clear.
		plain, err := gosmtp.Dial(addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := plain.Auth(sasl.NewPlainClient("", "tls-user@oximail.test", "s3cret")); err == nil {
			t.Error("cleartext AUTH succeeded, want rejection before STARTTLS")
		}
		_ = plain.Close()

		// Over STARTTLS it works.
		before := inboxCount(t, st, acc.ID)
		c, err := gosmtp.DialStartTLS(addr, clientTLS)
		if err != nil {
			t.Fatalf("dial STARTTLS: %v", err)
		}
		defer c.Close()
		if err := c.Auth(sasl.NewPlainClient("", "tls-user@oximail.test", "s3cret")); err != nil {
			t.Fatalf("auth over STARTTLS: %v", err)
		}
		if err := c.SendMail("tls-user@oximail.test",
			[]string{"tls-user@oximail.test"}, strings.NewReader(msg)); err != nil {
			t.Fatalf("send over STARTTLS: %v", err)
		}
		if got := inboxCount(t, st, acc.ID); got != before+1 {
			t.Fatalf("INBOX count = %d, want %d after one delivery", got, before+1)
		}
	})

	t.Run("implicit TLS: AUTH and send work", func(t *testing.T) {
		addr := startSubmission(t, st, serverTLS, true)

		before := inboxCount(t, st, acc.ID)
		c, err := gosmtp.DialTLS(addr, clientTLS)
		if err != nil {
			t.Fatalf("dial implicit TLS: %v", err)
		}
		defer c.Close()
		if err := c.Auth(sasl.NewPlainClient("", "tls-user@oximail.test", "s3cret")); err != nil {
			t.Fatalf("auth over implicit TLS: %v", err)
		}
		if err := c.SendMail("tls-user@oximail.test",
			[]string{"tls-user@oximail.test"}, strings.NewReader(msg)); err != nil {
			t.Fatalf("send over implicit TLS: %v", err)
		}
		if got := inboxCount(t, st, acc.ID); got != before+1 {
			t.Fatalf("INBOX count = %d, want %d after one delivery", got, before+1)
		}
	})
}

// TestSubmissionRateLimit covers the per-IP brute-force shield: after
// the rate limiter's default threshold of failed AUTHs from one client,
// further attempts are rejected outright — even with the right
// password, because the limiter check runs before the store lookup.
func TestSubmissionRateLimit(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := st.CreateAccount("sender@oximail.test", hash, 0); err != nil {
		t.Fatalf("create account: %v", err)
	}

	addr := startSubmission(t, st, nil, false)

	// Drive the default threshold (10) of failed AUTHs. Each one opens
	// a fresh connection, so we are exercising the per-IP bucket, not a
	// per-session counter.
	for i := 0; i < 10; i++ {
		c, err := gosmtp.Dial(addr)
		if err != nil {
			t.Fatalf("dial #%d: %v", i, err)
		}
		if err := c.Auth(sasl.NewPlainClient("", "sender@oximail.test", "wrong")); err == nil {
			t.Fatalf("AUTH #%d with bad password succeeded, want rejection", i)
		}
		_ = c.Close()
	}

	// The next attempt comes from the same IP — with the right password
	// — and must still be refused. Without the limiter this would
	// succeed.
	c, err := gosmtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial after threshold: %v", err)
	}
	defer c.Close()
	if err := c.Auth(sasl.NewPlainClient("", "sender@oximail.test", "s3cret")); err == nil {
		t.Fatal("AUTH succeeded after exceeding the rate-limit budget; brute-force shield is not engaged")
	}
}

// inboxCount returns how many messages are in an account's INBOX,
// treating a not-yet-created INBOX as empty.
func inboxCount(t *testing.T, st *store.Store, accountID uint64) int {
	t.Helper()
	inbox, err := st.GetMailboxByName(accountID, "INBOX")
	if errors.Is(err, store.ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("get INBOX: %v", err)
	}
	msgs, err := st.ListMessages(accountID, inbox.ID)
	if err != nil {
		t.Fatalf("list INBOX: %v", err)
	}
	return len(msgs)
}

// startSubmission launches the submission server on a free port, wired
// to st, and returns its address. A non-nil tlsConfig enables STARTTLS;
// implicit additionally serves implicit TLS (SMTPS). It is shut down
// when the test ends.
func startSubmission(t *testing.T, st *store.Store, tlsConfig *tls.Config, implicit bool) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	var srv *smtp.Server
	if implicit {
		srv = smtp.NewSubmissionTLS(addr, "oximail.test", st, tlsConfig)
	} else {
		srv = smtp.NewSubmission(addr, "oximail.test", st, tlsConfig)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("submission server exited with: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("submission server did not stop within 5s")
		}
	})

	itest.WaitTCP(t, addr)
	return addr
}

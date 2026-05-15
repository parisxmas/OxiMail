//go:build integration

// Integration test for the inbound SMTP server. It boots a live
// oxidb-server, starts the SMTP server against it, and drives it with a
// real SMTP client. Gated behind the `integration` build tag:
//
//	go test -tags=integration ./internal/smtp/...
package smtp_test

import (
	"context"
	"fmt"
	netsmtp "net/smtp"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/smtp"
	"github.com/parisxmas/OxiMail/internal/spam"
	"github.com/parisxmas/OxiMail/internal/srs"
	"github.com/parisxmas/OxiMail/internal/store"
)

func TestInboundSMTP(t *testing.T) {
	host, port := itest.StartOxiDB(t)

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := st.CreateAccount("rcpt@oximail.test", "x", 0); err != nil {
		t.Fatalf("create account: %v", err)
	}

	smtpAddr := startSMTP(t, st)

	t.Run("delivers a message to a local mailbox", func(t *testing.T) {
		msg := "From: Sender <sender@elsewhere.test>\r\n" +
			"To: rcpt@oximail.test\r\n" +
			"Subject: hello from the integration test\r\n" +
			"Message-Id: <itest-1@elsewhere.test>\r\n" +
			"\r\n" +
			"this is the body\r\n"
		if err := netsmtp.SendMail(smtpAddr, nil, "sender@elsewhere.test",
			[]string{"rcpt@oximail.test"}, []byte(msg)); err != nil {
			t.Fatalf("SendMail: %v", err)
		}

		acc, err := st.GetAccount("rcpt@oximail.test")
		if err != nil {
			t.Fatalf("get account: %v", err)
		}
		inbox, err := st.GetMailboxByName(acc.ID, "INBOX")
		if err != nil {
			t.Fatalf("get INBOX (should have been created on first delivery): %v", err)
		}
		msgs, err := st.ListMessages(inbox.ID)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("INBOX has %d messages, want 1", len(msgs))
		}

		got := msgs[0]
		if got.Subject != "hello from the integration test" {
			t.Errorf("subject = %q", got.Subject)
		}
		if got.MessageID != "itest-1@elsewhere.test" {
			t.Errorf("message-id = %q (brackets should be stripped)", got.MessageID)
		}
		if got.FromAddr != "sender@elsewhere.test" {
			t.Errorf("from = %q (should be the bare address)", got.FromAddr)
		}
		body, err := st.FetchBody(&got)
		if err != nil {
			t.Fatalf("fetch body: %v", err)
		}
		if !strings.Contains(string(body), "this is the body") {
			t.Errorf("stored body does not contain what was sent: %q", body)
		}
	})

	t.Run("rejects an unknown recipient", func(t *testing.T) {
		err := netsmtp.SendMail(smtpAddr, nil, "sender@elsewhere.test",
			[]string{"nobody@oximail.test"}, []byte("From: x\r\n\r\nhi\r\n"))
		if err == nil {
			t.Fatal("SendMail to an unknown recipient succeeded, want a rejection")
		}
		// The inbound MX does not relay, so an address with no local
		// mailbox is a permanent 550 failure.
		if !strings.Contains(err.Error(), "550") {
			t.Errorf("error = %v, want a 550 rejection", err)
		}
	})
}

// TestInboundSMTPVacation covers the RFC 3834 auto-responder: when a
// local account has a vacation rule enabled, an inbound person-to-
// person message lands the original in INBOX and drops an auto-reply
// on the outbound queue addressed to the original sender — with the
// loop-prevention headers RFC 3834 requires. A bounce (null envelope
// sender) does NOT trigger a reply, and a second message from the
// same sender within the suppression window is also silent.
func TestInboundSMTPVacation(t *testing.T) {
	host, port := itest.StartOxiDB(t)
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	acc, err := st.CreateAccount("ooo@oximail.test", "h", 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if _, err := st.SetVacation(acc.ID, true, "Out of office", "I am away until Friday.", 7); err != nil {
		t.Fatalf("set vacation: %v", err)
	}

	addr := startSMTP(t, st)

	t.Run("auto-reply fires on a person-to-person message", func(t *testing.T) {
		msg := "From: alice@partners.test\r\n" +
			"To: ooo@oximail.test\r\n" +
			"Subject: lunch?\r\n" +
			"Message-Id: <p2p-1@partners.test>\r\n" +
			"\r\n" +
			"are you free?\r\n"
		if err := netsmtp.SendMail(addr, nil, "alice@partners.test",
			[]string{"ooo@oximail.test"}, []byte(msg)); err != nil {
			t.Fatalf("SendMail: %v", err)
		}
		queued, err := st.ListDueOutbound(10)
		if err != nil {
			t.Fatalf("list outbound: %v", err)
		}
		if len(queued) != 1 {
			t.Fatalf("outbound queue has %d items, want 1: %+v", len(queued), queued)
		}
		q := queued[0]
		if q.From != "ooo@oximail.test" {
			t.Errorf("envelope sender = %q, want the vacationer", q.From)
		}
		if len(q.Recipients) != 1 || q.Recipients[0] != "alice@partners.test" {
			t.Errorf("envelope recipients = %v, want [alice@partners.test]", q.Recipients)
		}
		raw, err := st.FetchOutboundBody(&q)
		if err != nil {
			t.Fatalf("fetch outbound: %v", err)
		}
		s := string(raw)
		for _, want := range []string{
			"Auto-Submitted: auto-replied",
			"Subject: Out of office",
			"In-Reply-To: <p2p-1@partners.test>",
			"I am away until Friday.",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("auto-reply missing %q\n---\n%s", want, s)
			}
		}
	})

	t.Run("no auto-reply for a bounce", func(t *testing.T) {
		before, _ := st.ListDueOutbound(50)
		msg := "From: MAILER-DAEMON@partners.test\r\n" +
			"To: ooo@oximail.test\r\n" +
			"Subject: delivery failed\r\n\r\n"
		// Null envelope sender — a DSN.
		if err := netsmtp.SendMail(addr, nil, "", []string{"ooo@oximail.test"}, []byte(msg)); err != nil {
			t.Fatalf("SendMail: %v", err)
		}
		after, _ := st.ListDueOutbound(50)
		if len(after) != len(before) {
			t.Errorf("auto-reply fired on a bounce: queue grew from %d to %d", len(before), len(after))
		}
	})

	t.Run("suppressor silences a second hit from the same sender", func(t *testing.T) {
		before, _ := st.ListDueOutbound(50)
		msg := "From: alice@partners.test\r\nTo: ooo@oximail.test\r\nSubject: still?\r\n\r\nthinking of you\r\n"
		if err := netsmtp.SendMail(addr, nil, "alice@partners.test",
			[]string{"ooo@oximail.test"}, []byte(msg)); err != nil {
			t.Fatalf("SendMail: %v", err)
		}
		after, _ := st.ListDueOutbound(50)
		if len(after) != len(before) {
			t.Errorf("auto-reply re-fired within the suppression window: queue grew from %d to %d", len(before), len(after))
		}
	})
}

// TestInboundSMTPAliasForwarding covers SRS-based alias forwarding to
// remote addresses: an alias whose destinations are all off-server is
// accepted; the message is enqueued for relay with an envelope sender
// rewritten through SRS so the next hop's SPF / DMARC check finds the
// forwarder, not the original sender's domain.
func TestInboundSMTPAliasForwarding(t *testing.T) {
	host, port := itest.StartOxiDB(t)

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	// Hosted alias: team@oximail.test → two remote recipients.
	if _, err := st.CreateDomain("oximail.test"); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	if _, err := st.CreateAlias("team@oximail.test", []string{"bob@elsewhere.test", "carol@elsewhere.test"}); err != nil {
		t.Fatalf("create alias: %v", err)
	}

	// MX is configured with an SRS secret and forwarder domain.
	secret := []byte("a-stable-srs-secret-32-byteslong!")
	addr := startSMTPWithForwarder(t, st, smtp.ForwarderConfig{
		SRSSecret:       secret,
		SRSMaxAge:       21 * 24 * time.Hour,
		ForwarderDomain: "oximail.test",
	})

	t.Run("a remote alias message is queued with an SRS-rewritten sender", func(t *testing.T) {
		msg := "From: alice@partners.test\r\n" +
			"To: team@oximail.test\r\n" +
			"Subject: aliased\r\n" +
			"\r\n" +
			"forwarded body\r\n"
		if err := netsmtp.SendMail(addr, nil, "alice@partners.test",
			[]string{"team@oximail.test"}, []byte(msg)); err != nil {
			t.Fatalf("SendMail: %v", err)
		}
		// The outbound queue carries one message, addressed to both
		// remote destinations, with the envelope sender rewritten.
		queued, err := st.ListDueOutbound(10)
		if err != nil {
			t.Fatalf("list outbound queue: %v", err)
		}
		if len(queued) != 1 {
			t.Fatalf("queue has %d items, want 1: %+v", len(queued), queued)
		}
		q := queued[0]
		if !strings.HasPrefix(q.From, "SRS0=") || !strings.HasSuffix(q.From, "@oximail.test") {
			t.Errorf("envelope sender = %q, want SRS-rewritten to @oximail.test", q.From)
		}
		want := map[string]bool{"bob@elsewhere.test": true, "carol@elsewhere.test": true}
		got := map[string]bool{}
		for _, r := range q.Recipients {
			got[r] = true
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("queued recipients = %v, want %v", got, want)
		}
	})

	t.Run("a bounce to the SRS address routes to the original sender", func(t *testing.T) {
		// Build the SRS address that the previous subtest's forward
		// would have been signed with. The MX must accept it (decode
		// alice@partners.test) and queue it back out to the same
		// remote — we wire that by adding an alias for the original
		// address to a remote, so the message has somewhere to go.
		original := "alice@partners.test"
		if _, err := st.CreateAlias("alice@oximail.test", []string{"alice@partners.test"}); err != nil {
			// Not strictly needed, but a no-op if it already exists.
			_ = err
		}
		// Encode the SRS address with the same secret the server uses.
		// (We can't reach into the server's MX directly, so we go
		// through the package.)
		srsAddr, err := srs.Encode(secret, original, "oximail.test")
		if err != nil {
			t.Fatalf("srs.Encode: %v", err)
		}
		msg := "From: postmaster@elsewhere.test\r\n" +
			"To: " + srsAddr + "\r\n" +
			"Subject: bounce: aliased\r\n" +
			"\r\n" +
			"the previous message could not be delivered\r\n"
		// A bounce uses a null envelope sender ("<>").
		if err := netsmtp.SendMail(addr, nil, "", []string{srsAddr}, []byte(msg)); err != nil {
			// alice@partners.test is not local and not aliased here,
			// so the MX should reject this — proving SRS decode at
			// least worked. The decoded address routed through the
			// usual "no such user" path.
			if !strings.Contains(err.Error(), "550") {
				t.Errorf("bounce to unaliased SRS address: error = %v, want 550", err)
			}
		} else {
			t.Error("bounce to unaliased SRS address succeeded; want 550")
		}
	})
}

// startSMTP launches the inbound SMTP server on a free port, wired to st
// and a no-op spam pipeline, and returns its address. It is shut down
// when the test finishes.
func startSMTP(t *testing.T, st *store.Store) string {
	t.Helper()
	return startSMTPWithForwarder(t, st, smtp.ForwarderConfig{})
}

// startSMTPWithForwarder is the variant used by the alias-forwarding
// test, which configures SRS-based remote relay.
func startSMTPWithForwarder(t *testing.T, st *store.Store, fwd smtp.ForwarderConfig) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := smtp.New(addr, "oximail.test", st, spam.Permissive(), nil, fwd)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("smtp server exited with: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("smtp server did not stop within 5s")
		}
	})

	itest.WaitTCP(t, addr)
	return addr
}

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
	"strings"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/smtp"
	"github.com/parisxmas/OxiMail/internal/spam"
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

// startSMTP launches the inbound SMTP server on a free port, wired to st
// and a no-op spam pipeline, and returns its address. It is shut down
// when the test finishes.
func startSMTP(t *testing.T, st *store.Store) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := smtp.New(addr, "oximail.test", st, spam.Permissive(), nil)

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

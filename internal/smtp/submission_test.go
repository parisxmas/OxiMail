//go:build integration

// Integration test for the submission server (port 587). It boots a
// live oxidb-server, starts the submission server against it, and
// drives it with a real authenticated SMTP client.
package smtp_test

import (
	"context"
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

	addr := startSubmission(t, st)

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
		msgs, err := st.ListMessages(inbox.ID)
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

// startSubmission launches the submission server on a free port, wired
// to st, and returns its address. It is shut down when the test ends.
func startSubmission(t *testing.T, st *store.Store) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := smtp.NewSubmission(addr, "oximail.test", st)

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

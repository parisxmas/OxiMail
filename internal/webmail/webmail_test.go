//go:build integration

// Integration test for the webmail backend API. It boots a live
// oxidb-server, starts the webmail server against it, and drives it
// with a real HTTP client. Gated behind the `integration` build tag.
package webmail_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/itest"
	"github.com/parisxmas/OxiMail/internal/store"
	"github.com/parisxmas/OxiMail/internal/webmail"
)

func TestWebmail(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())

	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// An account with the default folders and one seeded INBOX message.
	hash, err := store.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	acc, err := st.CreateAccount("user@oximail.test", hash, 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("ensure mailboxes: %v", err)
	}
	inbox, err := st.GetMailboxByName(acc.ID, "INBOX")
	if err != nil {
		t.Fatalf("get INBOX: %v", err)
	}
	rawMsg := []byte("From: Sender <sender@elsewhere.test>\r\n" +
		"To: user@oximail.test\r\n" +
		"Subject: Hello webmail\r\n" +
		"Message-Id: <wm-1@elsewhere.test>\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"the plain text body\r\n")
	seeded, err := st.AppendMessage(inbox.ID, store.IncomingMessage{
		Raw: rawMsg, Subject: "Hello webmail", MessageID: "wm-1@elsewhere.test",
		FromAddr: "sender@elsewhere.test",
	})
	if err != nil {
		t.Fatalf("seed INBOX message: %v", err)
	}

	// A second account, both to test cross-account isolation and to be
	// a local recipient for the send test.
	other, err := st.CreateAccount("other@oximail.test", hash, 0)
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}

	base := startWebmail(t, st)

	t.Run("login rejects a bad password", func(t *testing.T) {
		if status := post(t, base+"/api/login", "", loginBody("user@oximail.test", "wrong")); status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	var token string
	t.Run("login issues a token", func(t *testing.T) {
		var out struct{ Token, Address string }
		status := postJSON(t, base+"/api/login", "", loginBody("user@oximail.test", "s3cret"), &out)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if out.Token == "" {
			t.Fatal("response has no token")
		}
		if out.Address != "user@oximail.test" {
			t.Errorf("address = %q, want user@oximail.test", out.Address)
		}
		token = out.Token
	})

	t.Run("the API requires authentication", func(t *testing.T) {
		if status := get(t, base+"/api/mailboxes", ""); status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401 without a token", status)
		}
	})

	t.Run("list mailboxes with counts", func(t *testing.T) {
		var out []struct {
			Name   string
			Total  int
			Unseen int
		}
		if status := getJSON(t, base+"/api/mailboxes", token, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if len(out) != 5 {
			t.Fatalf("got %d mailboxes, want the 5 defaults", len(out))
		}
		var sawInbox bool
		for _, mb := range out {
			if mb.Name == "INBOX" {
				sawInbox = true
				if mb.Total != 1 || mb.Unseen != 1 {
					t.Errorf("INBOX total=%d unseen=%d, want 1/1", mb.Total, mb.Unseen)
				}
			}
		}
		if !sawInbox {
			t.Error("INBOX missing from the mailbox list")
		}
	})

	t.Run("list messages in INBOX", func(t *testing.T) {
		var out []struct {
			ID      uint64
			Subject string
			From    string
		}
		if status := getJSON(t, base+"/api/mailboxes/INBOX/messages", token, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if len(out) != 1 {
			t.Fatalf("got %d messages, want 1", len(out))
		}
		if out[0].ID != seeded.ID || out[0].Subject != "Hello webmail" {
			t.Errorf("message summary = %+v", out[0])
		}
	})

	t.Run("get a message with its parsed body", func(t *testing.T) {
		var out struct {
			Subject string
			From    string
			Text    string
		}
		url := fmt.Sprintf("%s/api/messages/%d", base, seeded.ID)
		if status := getJSON(t, url, token, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if out.Subject != "Hello webmail" {
			t.Errorf("subject = %q", out.Subject)
		}
		if !strings.Contains(out.Text, "the plain text body") {
			t.Errorf("text body = %q, want it to contain the message text", out.Text)
		}
	})

	t.Run("cannot read another account's message", func(t *testing.T) {
		var login struct{ Token string }
		postJSON(t, base+"/api/login", "", loginBody("other@oximail.test", "s3cret"), &login)
		url := fmt.Sprintf("%s/api/messages/%d", base, seeded.ID)
		if status := get(t, url, login.Token); status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 for a cross-account message", status)
		}
	})

	t.Run("missing message is a 404", func(t *testing.T) {
		if status := get(t, base+"/api/messages/999999", token); status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", status)
		}
	})

	// --- mutations (these run after the read subtests, in order) ---

	t.Run("send a message to a local recipient", func(t *testing.T) {
		body := mustJSON(map[string]any{
			"to":      []string{"other@oximail.test"},
			"subject": "Sent from webmail",
			"text":    "hello from the API",
		})
		var out struct{ Delivered, Queued int }
		if status := postJSON(t, base+"/api/messages", token, body, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if out.Delivered != 1 || out.Queued != 0 {
			t.Fatalf("routed delivered=%d queued=%d, want 1/0", out.Delivered, out.Queued)
		}
		// The recipient received it.
		otherInbox, err := st.GetMailboxByName(other.ID, "INBOX")
		if err != nil {
			t.Fatalf("get other INBOX: %v", err)
		}
		recvd, err := st.ListMessages(otherInbox.ID)
		if err != nil {
			t.Fatalf("list other INBOX: %v", err)
		}
		if len(recvd) != 1 || recvd[0].Subject != "Sent from webmail" {
			t.Fatalf("recipient INBOX = %+v", recvd)
		}
		// A copy landed in the sender's Sent mailbox.
		sent, err := st.GetMailboxByName(acc.ID, "Sent")
		if err != nil {
			t.Fatalf("get Sent: %v", err)
		}
		if sentMsgs, _ := st.ListMessages(sent.ID); len(sentMsgs) != 1 {
			t.Fatalf("Sent has %d messages, want 1", len(sentMsgs))
		}
	})

	t.Run("change flags", func(t *testing.T) {
		body := mustJSON(map[string]any{"op": "add", "flags": []string{`\Seen`}})
		var out struct{ Seen bool }
		url := fmt.Sprintf("%s/api/messages/%d/flags", base, seeded.ID)
		if status := patchJSON(t, url, token, body, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if !out.Seen {
			t.Error("seen = false after adding \\Seen")
		}
		// Verify it was persisted, not just echoed.
		m, err := st.GetMessage(seeded.ID)
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		if !flagSet(m.Flags, `\Seen`) {
			t.Errorf("\\Seen not persisted to the store: %v", m.Flags)
		}
	})

	t.Run("move a message to another mailbox", func(t *testing.T) {
		body := mustJSON(map[string]string{"mailbox": "Trash"})
		url := fmt.Sprintf("%s/api/messages/%d/move", base, seeded.ID)
		if status := postJSON(t, url, token, body, nil); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		m, err := st.GetMessage(seeded.ID)
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		trash, err := st.GetMailboxByName(acc.ID, "Trash")
		if err != nil {
			t.Fatalf("get Trash: %v", err)
		}
		if m.MailboxID != trash.ID {
			t.Errorf("message mailbox = %d, want Trash (%d)", m.MailboxID, trash.ID)
		}
	})

	t.Run("delete a message", func(t *testing.T) {
		url := fmt.Sprintf("%s/api/messages/%d", base, seeded.ID)
		if status := del(t, url, token); status != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", status)
		}
		if _, err := st.GetMessage(seeded.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("message still present after delete: err = %v", err)
		}
	})
}

// -----------------------------------------------------------------------
// HTTP helpers
// -----------------------------------------------------------------------

func loginBody(address, password string) []byte {
	b, _ := json.Marshal(map[string]string{"address": address, "password": password})
	return b
}

// mustJSON marshals v or panics — for building request bodies in tests.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// flagSet reports whether flag is present in flags.
func flagSet(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

func post(t *testing.T, url, token string, body []byte) int {
	t.Helper()
	return postJSON(t, url, token, body, nil)
}

func postJSON(t *testing.T, url, token string, body []byte, out any) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(t, req, token, out)
}

func get(t *testing.T, url, token string) int {
	t.Helper()
	return getJSON(t, url, token, nil)
}

func getJSON(t *testing.T, url, token string, out any) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return doJSON(t, req, token, out)
}

func patchJSON(t *testing.T, url, token string, body []byte, out any) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doJSON(t, req, token, out)
}

func del(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return doJSON(t, req, token, nil)
}

func doJSON(t *testing.T, req *http.Request, token string, out any) int {
	t.Helper()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s response: %v", req.URL, err)
		}
	}
	return resp.StatusCode
}

// startWebmail launches the webmail server on a free port, wired to st,
// and returns its base URL. It is shut down when the test ends.
func startWebmail(t *testing.T, st *store.Store) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := webmail.New(addr, "", st, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("webmail server exited with: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("webmail server did not stop within 5s")
		}
	})

	itest.WaitTCP(t, addr)
	return "http://" + addr
}

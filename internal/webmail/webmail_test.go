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
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n" +
		"\r\n" +
		"--BOUND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"the plain text body, mentions watermelons\r\n" +
		"--BOUND\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"notes.bin\"\r\n" +
		"\r\n" +
		"\x00\x01\x02\x03ATTACH\r\n" +
		"--BOUND--\r\n")
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
		if len(out) != 6 {
			t.Fatalf("got %d mailboxes, want the 6 defaults", len(out))
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

	t.Run("list filters by subject substring", func(t *testing.T) {
		var out []struct{ ID uint64 }
		if status := getJSON(t, base+"/api/mailboxes/INBOX/messages?q=webmail", token, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if len(out) != 1 {
			t.Fatalf("got %d matches for q=webmail, want 1", len(out))
		}
		var miss []struct{ ID uint64 }
		if status := getJSON(t, base+"/api/mailboxes/INBOX/messages?q=elephants", token, &miss); status != http.StatusOK || len(miss) != 0 {
			t.Fatalf("q=elephants: status=%d count=%d, want 200/0", status, len(miss))
		}
	})

	t.Run("list filters by body substring", func(t *testing.T) {
		var out []struct{ ID uint64 }
		// "watermelons" appears in the body but not the subject or sender.
		if status := getJSON(t, base+"/api/mailboxes/INBOX/messages?q=watermelons", token, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if len(out) != 1 {
			t.Fatalf("got %d matches for q=watermelons, want 1 (body search)", len(out))
		}
	})

	t.Run("download an attachment", func(t *testing.T) {
		// The seeded message has one attachment at index 0.
		url := fmt.Sprintf("%s/api/messages/%d/attachments/0", base, seeded.ID)
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("download: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type = %q, want application/octet-stream", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="notes.bin"`) {
			t.Errorf("Content-Disposition = %q, want it to name notes.bin", cd)
		}
		var body bytes.Buffer
		if _, err := body.ReadFrom(resp.Body); err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !bytes.Contains(body.Bytes(), []byte("ATTACH")) {
			t.Errorf("attachment body missing payload: %q", body.Bytes())
		}
	})

	t.Run("download an out-of-range attachment is a 404", func(t *testing.T) {
		url := fmt.Sprintf("%s/api/messages/%d/attachments/99", base, seeded.ID)
		if status := get(t, url, token); status != http.StatusNotFound {
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

	t.Run("send a message with an HTML body", func(t *testing.T) {
		body := mustJSON(map[string]any{
			"to":      []string{"other@oximail.test"},
			"subject": "Rich email",
			"text":    "fallback plain text",
			"html":    "<p>fallback <b>plain</b> text</p>",
		})
		var out struct{ Delivered, Queued int }
		if status := postJSON(t, base+"/api/messages", token, body, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if out.Delivered != 1 {
			t.Fatalf("delivered = %d, want 1", out.Delivered)
		}
		// The recipient's INBOX has the message and its raw form is
		// multipart/alternative carrying both representations.
		otherInbox, _ := st.GetMailboxByName(other.ID, "INBOX")
		recvd, _ := st.ListMessages(otherInbox.ID)
		var rich *store.Message
		for i := range recvd {
			if recvd[i].Subject == "Rich email" {
				rich = &recvd[i]
				break
			}
		}
		if rich == nil {
			t.Fatal("recipient did not receive the HTML message")
		}
		raw, err := st.FetchBody(rich)
		if err != nil {
			t.Fatalf("fetch HTML body: %v", err)
		}
		s := string(raw)
		if !strings.Contains(s, "multipart/alternative") {
			t.Error("composed message is not multipart/alternative")
		}
		if !strings.Contains(s, "text/plain") || !strings.Contains(s, "text/html") {
			t.Error("composed message is missing one of its parts")
		}
		if !strings.Contains(s, "<p>fallback <b>plain</b> text</p>") {
			t.Error("HTML body not present in the raw message")
		}
	})

	t.Run("send carries In-Reply-To and References for replies", func(t *testing.T) {
		body := mustJSON(map[string]any{
			"to":           []string{"other@oximail.test"},
			"subject":      "Re: Hello webmail",
			"text":         "thanks for the note",
			"in_reply_to":  "wm-1@elsewhere.test",
			"references":   []string{"wm-1@elsewhere.test"},
		})
		var out struct{ Delivered int }
		if status := postJSON(t, base+"/api/messages", token, body, &out); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		// Find the reply in the recipient's INBOX and confirm both
		// threading headers are present.
		otherInbox, _ := st.GetMailboxByName(other.ID, "INBOX")
		recvd, _ := st.ListMessages(otherInbox.ID)
		var reply *store.Message
		for i := range recvd {
			if recvd[i].Subject == "Re: Hello webmail" {
				reply = &recvd[i]
				break
			}
		}
		if reply == nil {
			t.Fatal("recipient did not receive the reply")
		}
		raw, err := st.FetchBody(reply)
		if err != nil {
			t.Fatalf("fetch reply: %v", err)
		}
		s := string(raw)
		if !strings.Contains(s, "In-Reply-To: <wm-1@elsewhere.test>") {
			t.Error("reply is missing the In-Reply-To header")
		}
		if !strings.Contains(s, "References: <wm-1@elsewhere.test>") {
			t.Error("reply is missing the References header")
		}
	})

	t.Run("save a draft and overwrite it in place", func(t *testing.T) {
		// First save — no id; the server allocates one.
		body := mustJSON(map[string]any{
			"to":      []string{"someone@partners.test"},
			"subject": "Work in progress",
			"text":    "first draft",
		})
		var first struct {
			ID      uint64
			Subject string
		}
		if status := postJSON(t, base+"/api/drafts", token, body, &first); status != http.StatusOK {
			t.Fatalf("first draft status = %d, want 200", status)
		}
		if first.ID == 0 {
			t.Fatal("first draft response missing id")
		}
		drafts, err := st.GetMailboxByName(acc.ID, "Drafts")
		if err != nil {
			t.Fatalf("get Drafts: %v", err)
		}
		msgs, err := st.ListMessages(drafts.ID)
		if err != nil {
			t.Fatalf("list Drafts: %v", err)
		}
		if len(msgs) != 1 || msgs[0].Subject != "Work in progress" {
			t.Fatalf("Drafts after first save: %v", msgs)
		}
		// The draft carries the \Draft flag so IMAP clients show it
		// correctly.
		if !flagSet(msgs[0].Flags, `\Draft`) {
			t.Errorf("draft missing \\Draft flag: %v", msgs[0].Flags)
		}

		// Second save — same id, new body. The Drafts folder still
		// contains exactly one message, with the updated subject.
		body = mustJSON(map[string]any{
			"id":      first.ID,
			"to":      []string{"someone@partners.test"},
			"subject": "Work in progress (revised)",
			"text":    "second draft",
		})
		var second struct{ ID uint64 }
		if status := postJSON(t, base+"/api/drafts", token, body, &second); status != http.StatusOK {
			t.Fatalf("second draft status = %d, want 200", status)
		}
		if second.ID == first.ID {
			t.Error("second draft kept the same message id; want a fresh one (the previous was deleted)")
		}
		msgs, _ = st.ListMessages(drafts.ID)
		if len(msgs) != 1 {
			t.Fatalf("Drafts has %d messages after the second save, want 1 (overwrite, not append)", len(msgs))
		}
		if msgs[0].Subject != "Work in progress (revised)" {
			t.Errorf("Drafts subject = %q, want the revised one", msgs[0].Subject)
		}
	})

	t.Run("sieve GET / PUT / DELETE round-trips, rejects bad scripts", func(t *testing.T) {
		// Initially empty.
		var get struct{ Source string }
		if status := getJSON(t, base+"/api/sieve", token, &get); status != http.StatusOK {
			t.Fatalf("initial GET status = %d, want 200", status)
		}
		if get.Source != "" {
			t.Errorf("fresh account has source = %q, want empty", get.Source)
		}
		// PUT a good script.
		script := `if header :contains "Subject" "report" { fileinto "Reports"; }`
		body := mustJSON(map[string]string{"source": script})
		req, _ := http.NewRequest(http.MethodPut, base+"/api/sieve", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
		}
		if status := getJSON(t, base+"/api/sieve", token, &get); status != http.StatusOK || get.Source != script {
			t.Fatalf("GET after PUT = %q (status=%d), want %q", get.Source, status, script)
		}
		// PUT a syntactically broken script — must be 400.
		bad := mustJSON(map[string]string{"source": "if { fileinto; }"})
		req, _ = http.NewRequest(http.MethodPut, base+"/api/sieve", bytes.NewReader(bad))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, _ = http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("bad sieve PUT status = %d, want 400", resp.StatusCode)
		}
		// The good script is still there.
		if status := getJSON(t, base+"/api/sieve", token, &get); status != http.StatusOK || get.Source != script {
			t.Errorf("after a bad PUT, GET returned %q (status=%d), want the previous good script", get.Source, status)
		}
		// DELETE.
		delReq, _ := http.NewRequest(http.MethodDelete, base+"/api/sieve", nil)
		delReq.Header.Set("Authorization", "Bearer "+token)
		resp, _ = http.DefaultClient.Do(delReq)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("DELETE status = %d, want 204", resp.StatusCode)
		}
	})

	t.Run("vacation GET / PUT / DELETE round-trips", func(t *testing.T) {
		// Initially: GET returns the all-zero "off" response.
		var get vacationResp
		if status := getJSON(t, base+"/api/vacation", token, &get); status != http.StatusOK {
			t.Fatalf("initial GET status = %d, want 200", status)
		}
		if get.Enabled {
			t.Errorf("freshly-loaded vacation reports enabled=true: %+v", get)
		}
		// PUT a rule.
		body := mustJSON(map[string]any{
			"enabled":       true,
			"subject":       "Out of office",
			"body":          "Back Monday.",
			"suppress_days": 5,
		})
		req, _ := http.NewRequest(http.MethodPut, base+"/api/vacation", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT vacation: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200", resp.StatusCode)
		}
		// GET reflects it.
		if status := getJSON(t, base+"/api/vacation", token, &get); status != http.StatusOK ||
			!get.Enabled || get.Subject != "Out of office" || get.Body != "Back Monday." ||
			get.SuppressDays != 5 {
			t.Fatalf("after PUT, GET = %+v (status=%d)", get, status)
		}
		// DELETE clears it.
		delReq, _ := http.NewRequest(http.MethodDelete, base+"/api/vacation", nil)
		delReq.Header.Set("Authorization", "Bearer "+token)
		resp, err = http.DefaultClient.Do(delReq)
		if err != nil {
			t.Fatalf("DELETE vacation: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
		}
		// And GET is back to the all-zero default.
		if status := getJSON(t, base+"/api/vacation", token, &get); status != http.StatusOK || get.Enabled {
			t.Fatalf("after DELETE, GET = %+v (status=%d)", get, status)
		}
	})

	t.Run("vacation PUT refuses an enabled rule with empty body", func(t *testing.T) {
		body := mustJSON(map[string]any{"enabled": true, "body": ""})
		req, _ := http.NewRequest(http.MethodPut, base+"/api/vacation", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for an empty body on an enabled rule", resp.StatusCode)
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

// TestWebmailCookieSession covers the browser session path: login sets
// HttpOnly oximail_session and readable oximail_csrf cookies; cookie-
// authenticated GETs go through, mutating requests fail without a
// matching X-CSRF-Token, succeed with one, and POST /api/logout
// revokes the session so the cookie no longer works.
func TestWebmailCookieSession(t *testing.T) {
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
	acc, err := st.CreateAccount("user@oximail.test", hash, 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.EnsureDefaultMailboxes(acc.ID); err != nil {
		t.Fatalf("ensure default mailboxes: %v", err)
	}

	base := startWebmail(t, st)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar}

	// --- Login: cookies land in the jar. ---
	req, _ := http.NewRequest(http.MethodPost, base+"/api/login", bytes.NewReader(loginBody("user@oximail.test", "s3cret")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}

	u, _ := url.Parse(base)
	var sessionVal, csrfVal string
	var sessionHTTPOnly bool
	for _, c := range jar.Cookies(u) {
		switch c.Name {
		case "oximail_session":
			sessionVal = c.Value
		case "oximail_csrf":
			csrfVal = c.Value
		}
	}
	for _, c := range resp.Cookies() {
		if c.Name == "oximail_session" {
			sessionHTTPOnly = c.HttpOnly
		}
	}
	if sessionVal == "" || csrfVal == "" {
		t.Fatalf("login did not set both cookies: session=%q csrf=%q", sessionVal, csrfVal)
	}
	if !sessionHTTPOnly {
		t.Error("oximail_session is not HttpOnly — XSS could steal the session")
	}

	// --- Cookie-authenticated GET: jar carries the session cookie. ---
	req, _ = http.NewRequest(http.MethodGet, base+"/api/mailboxes", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("cookie GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie GET /api/mailboxes status = %d, want 200", resp.StatusCode)
	}

	// --- Mutating request without the CSRF header: must be refused. ---
	moveReq, _ := http.NewRequest(http.MethodPost, base+"/api/messages/999/move", bytes.NewReader(mustJSON(map[string]string{"mailbox": "Trash"})))
	moveReq.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(moveReq)
	if err != nil {
		t.Fatalf("CSRF-less POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CSRF-less POST status = %d, want 403", resp.StatusCode)
	}

	// --- Mutating request with the right CSRF header: ordinary 404
	//     (no such message), proving the CSRF check passed. ---
	moveReq, _ = http.NewRequest(http.MethodPost, base+"/api/messages/999/move", bytes.NewReader(mustJSON(map[string]string{"mailbox": "Trash"})))
	moveReq.Header.Set("Content-Type", "application/json")
	moveReq.Header.Set("X-CSRF-Token", csrfVal)
	resp, err = client.Do(moveReq)
	if err != nil {
		t.Fatalf("CSRF-armed POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("CSRF-armed POST status = %d, want 404 (got past CSRF, message id 999 does not exist)", resp.StatusCode)
	}

	// --- Logout: invalidates the session; the cookie is no longer
	//     accepted on a subsequent request. ---
	logoutReq, _ := http.NewRequest(http.MethodPost, base+"/api/logout", nil)
	logoutReq.Header.Set("X-CSRF-Token", csrfVal)
	resp, err = client.Do(logoutReq)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", resp.StatusCode)
	}
	// Force-restore the old session cookie — the server cleared it via
	// Set-Cookie, but we want to prove that *the value itself* is now
	// rejected even if a client kept it.
	cookies := []*http.Cookie{{Name: "oximail_session", Value: sessionVal}}
	jar.SetCookies(u, cookies)
	req, _ = http.NewRequest(http.MethodGet, base+"/api/mailboxes", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("post-logout GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout GET status = %d, want 401 (session must be invalidated server-side)", resp.StatusCode)
	}
}

// TestWebmailRateLimit covers the per-IP brute-force shield over POST
// /api/login: after the default threshold of failed attempts, further
// requests get 429 — even when the password is right — because the
// limiter check runs before the store lookup.
// TestWebmailRateLimitPerAccount covers the tighter account-scoped
// shield over POST /api/login: after 5 failed attempts at one address
// the next attempt at that SAME address gets 429 — even from another
// IP, which we cannot vary from a single test process but which is
// the attack the per-account gate is meant to stop. The right
// password also gets 429 because the limiter check fires before the
// store lookup.
func TestWebmailRateLimitPerAccount(t *testing.T) {
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
	if _, err := st.CreateAccount("user@oximail.test", hash, 0); err != nil {
		t.Fatalf("create account: %v", err)
	}

	base := startWebmail(t, st)

	// Five wrong attempts at the same address are allowed (each gets
	// 401); the per-account bucket fills up as a side effect.
	for i := 0; i < 5; i++ {
		if status := post(t, base+"/api/login", "", loginBody("user@oximail.test", "wrong")); status != http.StatusUnauthorized {
			t.Fatalf("login #%d: status = %d, want 401", i, status)
		}
	}

	// The 6th call — including with the right password — is refused
	// with 429 because the per-account limiter fires before the store
	// lookup. Address normalisation should make the case variant hit
	// the same bucket.
	if status := post(t, base+"/api/login", "", loginBody("user@oximail.test", "s3cret")); status != http.StatusTooManyRequests {
		t.Fatalf("right password after exhausting per-account budget: status = %d, want 429", status)
	}
	if status := post(t, base+"/api/login", "", loginBody("USER@OXIMAIL.TEST", "wrong")); status != http.StatusTooManyRequests {
		t.Fatalf("case-variant address after exhausting per-account budget: status = %d, want 429", status)
	}
}

// TestWebmailRateLimitPerIP covers the broader per-IP shield: 10
// failed attempts across DIFFERENT addresses from the same source IP
// (the per-account gate is never tripped because every key is fresh)
// exhausts the per-IP budget and the 11th call gets 429.
func TestWebmailRateLimitPerIP(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	base := startWebmail(t, st)

	// 10 attempts at 10 different addresses. None of these accounts
	// exist; the limiter still records each as a per-IP failure.
	for i := 0; i < 10; i++ {
		addr := fmt.Sprintf("none-%d@oximail.test", i)
		if status := post(t, base+"/api/login", "", loginBody(addr, "x")); status != http.StatusUnauthorized {
			t.Fatalf("login #%d (addr=%s): status = %d, want 401", i, addr, status)
		}
	}

	// 11th from the same source is 429 regardless of which (still-fresh)
	// account address it targets.
	if status := post(t, base+"/api/login", "", loginBody("yet-another@oximail.test", "x")); status != http.StatusTooManyRequests {
		t.Fatalf("11th attempt after exhausting per-IP budget: status = %d, want 429", status)
	}
}

// vacationResp mirrors internal/webmail.vacationResponse for the JSON
// round-trip the test makes.
type vacationResp struct {
	Enabled      bool   `json:"enabled"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	SuppressDays int    `json:"suppress_days,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
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

// TestMTASTSPolicyHandler covers the publish side of MTA-STS: when a
// policy is configured the webmail server exposes the canonical
// /.well-known/mta-sts.txt file with the right MIME type and a body
// remote senders can parse; with no policy configured the handler is
// not registered and the path 404s through the SPA fallback.
func TestMTASTSPolicyHandler(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// Server WITH a policy.
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := webmail.New(addr, "", st, nil, webmail.MTASTSPolicy{
		Mode:   "enforce",
		MX:     []string{"mx1.oximail.test", "*.alt.oximail.test"},
		MaxAge: 24 * time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-errc
	})
	itest.WaitTCP(t, addr)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/.well-known/mta-sts.txt", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET policy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		"version: STSv1",
		"mode: enforce",
		"mx: mx1.oximail.test",
		"mx: *.alt.oximail.test",
		"max_age: 86400",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("policy body missing %q\n---\n%s", want, s)
		}
	}
}

// TestWebmailChangePassword covers POST /api/account/password —
// happy path (rotates the hash + invalidates other sessions while
// keeping the caller's session alive), validation rejection (short
// password, mismatched current), and the negative auth case (wrong
// current password gets 401 not 204).
func TestWebmailChangePassword(t *testing.T) {
	host, port := itest.StartOxiDB(t, itest.LazySync())
	st, err := store.Open(host, port)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.EnsureSchema(st); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	hash, err := store.HashPassword("originalPW123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	acc, err := st.CreateAccount("pwuser@oximail.test", hash, 0)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	base := startWebmail(t, st)

	// Two parallel sessions for pwuser — one to use, one to confirm
	// gets revoked by the password change.
	var keepSess, otherSess struct{ Token, Address string }
	if s := postJSON(t, base+"/api/login", "", loginBody("pwuser@oximail.test", "originalPW123"), &keepSess); s != http.StatusOK {
		t.Fatalf("first login: %d", s)
	}
	if s := postJSON(t, base+"/api/login", "", loginBody("pwuser@oximail.test", "originalPW123"), &otherSess); s != http.StatusOK {
		t.Fatalf("second login: %d", s)
	}

	t.Run("wrong current password is rejected with 401", func(t *testing.T) {
		body := mustJSON(map[string]string{"current_password": "WRONG", "new_password": "brandNewPW456"})
		if s := post(t, base+"/api/account/password", keepSess.Token, body); s != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", s)
		}
	})

	t.Run("short new password is rejected with 400", func(t *testing.T) {
		body := mustJSON(map[string]string{"current_password": "originalPW123", "new_password": "short"})
		if s := post(t, base+"/api/account/password", keepSess.Token, body); s != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", s)
		}
	})

	t.Run("new = current is rejected with 400", func(t *testing.T) {
		body := mustJSON(map[string]string{"current_password": "originalPW123", "new_password": "originalPW123"})
		if s := post(t, base+"/api/account/password", keepSess.Token, body); s != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", s)
		}
	})

	t.Run("happy path: 204, hash rotated, other session revoked, caller stays in", func(t *testing.T) {
		body := mustJSON(map[string]string{"current_password": "originalPW123", "new_password": "brandNewPW456"})
		if s := post(t, base+"/api/account/password", keepSess.Token, body); s != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", s)
		}
		// Old password no longer authenticates against the store.
		if _, err := st.Authenticate("pwuser@oximail.test", "originalPW123"); err == nil {
			t.Error("old password still works after rotation")
		}
		// New one does.
		if _, err := st.Authenticate("pwuser@oximail.test", "brandNewPW456"); err != nil {
			t.Errorf("new password does not work: %v", err)
		}
		// The OTHER session is now invalid — any authed call returns 401.
		if s := get(t, base+"/api/mailboxes", otherSess.Token); s != http.StatusUnauthorized {
			t.Errorf("other session after rotation: %d, want 401", s)
		}
		// The current session is still good.
		if s := get(t, base+"/api/mailboxes", keepSess.Token); s != http.StatusOK {
			t.Errorf("caller session after rotation: %d, want 200", s)
		}
	})
	_ = acc
}

// startWebmail launches the webmail server on a free port, wired to st,
// and returns its base URL. It is shut down when the test ends.
func startWebmail(t *testing.T, st *store.Store) string {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", itest.FreePort(t))
	srv := webmail.New(addr, "", st, nil, webmail.MTASTSPolicy{})

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

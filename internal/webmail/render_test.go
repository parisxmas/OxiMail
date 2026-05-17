package webmail

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/emersion/go-message/mail"
)

// addressStrings must return a non-nil empty slice for a missing
// header so the JSON serialisation stays array-typed. A nil slice
// serialises as `null`, which crashes SPA templates that expect to
// call `.length` on the field unconditionally.
func TestAddressStringsEmptyIsNotNil(t *testing.T) {
	// A minimal message with no Cc header.
	raw := []byte("From: a@x.test\r\n" +
		"To: b@x.test\r\n" +
		"Subject: t\r\n" +
		"\r\n" +
		"body\r\n")
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("create reader: %v", err)
	}
	got := addressStrings(mr.Header, "Cc")
	if got == nil {
		t.Fatal("addressStrings returned nil for a missing header; must be non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("addressStrings on missing header = %v, want []", got)
	}
	// Round-trip through JSON to lock the wire contract.
	js, _ := json.Marshal(got)
	if string(js) != "[]" {
		t.Errorf("empty addressStrings JSON = %s, want []", js)
	}
}

// renderBody on a multipart/alternative message must populate BOTH
// text and html, and must never return nil arrays for to/cc.
func TestRenderBodyMultipartAlternative(t *testing.T) {
	raw := strings.ReplaceAll(`From: a@x.test
To: b@x.test
Subject: hi
Content-Type: multipart/alternative; boundary="b"

--b
Content-Type: text/plain; charset=UTF-8

hello
--b
Content-Type: text/html; charset=UTF-8

<p>hello</p>
--b--
`, "\n", "\r\n")
	body := renderBody([]byte(raw))
	if body.Text != "hello\r\n" && body.Text != "hello\n" && body.Text != "hello" {
		t.Errorf("Text = %q, want hello (with optional CRLF)", body.Text)
	}
	if !strings.Contains(body.HTML, "<p>hello</p>") {
		t.Errorf("HTML = %q, want it to contain <p>hello</p>", body.HTML)
	}
	if body.Cc == nil {
		t.Error("Cc is nil — must be non-nil empty slice")
	}
	if body.Attachments == nil {
		t.Error("Attachments is nil — must be non-nil empty slice")
	}
}

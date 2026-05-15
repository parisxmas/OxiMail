package webmail

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"
	"time"
)

// buildMessage assembles a minimal RFC 5322 message from the fields a
// compose form provides. If html is empty, the body is a single
// text/plain part. If html is non-empty, the body is multipart/
// alternative carrying both representations — clients pick the richer
// one they understand.
//
// TODO: attachments (multipart/mixed wrapping the alternative).
func buildMessage(from string, to, cc []string, subject, text, html, messageID string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	// QEncoding.Encode leaves plain ASCII untouched and RFC 2047-encodes
	// anything else, so a non-ASCII subject stays well-formed.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", messageID)
	b.WriteString("MIME-Version: 1.0\r\n")

	if html == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
		b.WriteString("\r\n")
		b.WriteString(normalizeNewlines(text))
		return []byte(b.String())
	}

	// multipart/alternative — text first (so plain-text clients show it),
	// HTML last (so MIME-aware clients prefer it, per RFC 2046 §5.1.4).
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	addPart(mw, "text/plain; charset=utf-8", text)
	if html != "" {
		addPart(mw, "text/html; charset=utf-8", html)
	}
	_ = mw.Close()

	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", mw.Boundary())
	b.WriteString("\r\n")
	b.WriteString(body.String())
	return []byte(b.String())
}

// addPart writes one inline part with the given content type and body.
func addPart(mw *multipart.Writer, contentType, body string) {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "8bit")
	part, err := mw.CreatePart(h)
	if err != nil {
		return
	}
	_, _ = part.Write([]byte(normalizeNewlines(body)))
}

// normalizeNewlines rewrites any line endings to CRLF, as RFC 5322
// requires, and ensures the body ends with one.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	if !strings.HasSuffix(s, "\r\n") {
		s += "\r\n"
	}
	return s
}

// randomID returns a random token for the local part of a Message-ID.
func randomID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// addressDomain returns the domain part of an email address, falling
// back to "localhost" for a malformed one.
func addressDomain(addr string) string {
	if at := strings.LastIndexByte(addr, '@'); at >= 0 && at < len(addr)-1 {
		return addr[at+1:]
	}
	return "localhost"
}

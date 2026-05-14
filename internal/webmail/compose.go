package webmail

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"strings"
	"time"
)

// buildTextMessage assembles a minimal RFC 5322 text/plain message from
// the fields a compose form provides.
//
// TODO: HTML bodies (multipart/alternative) and attachments
// (multipart/mixed) — composing those is a follow-up.
func buildTextMessage(from string, to, cc []string, subject, text, messageID string) []byte {
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
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(normalizeNewlines(text))
	return []byte(b.String())
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

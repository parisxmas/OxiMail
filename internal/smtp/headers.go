package smtp

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	gosmtp "github.com/emersion/go-smtp"
)

// receivedHeader builds the trace field RFC 5321 §4.4 requires of every
// server that accepts a message. The HELO name is whatever the client
// chose to say, so anything unusual in it is left out.
func receivedHeader(hostname string, c *gosmtp.Conn, remoteIP string, authenticated bool, now time.Time) string {
	helo, protocol := "unknown", "ESMTP"
	if c != nil {
		if name := c.Hostname(); isPlainHostname(name) {
			helo = name
		}
		if _, ok := c.TLSConnectionState(); ok {
			protocol += "S"
		}
	}
	if authenticated {
		protocol += "A"
	}
	return fmt.Sprintf("Received: from %s ([%s])\r\n\tby %s with %s;\r\n\t%s\r\n",
		helo, remoteIP, hostname, protocol, now.Format(time.RFC1123Z))
}

// isPlainHostname reports whether s is safe to quote in a header: a
// domain name or an address literal, of a sane length.
func isPlainHostname(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte(".-_:[]", c) >= 0) {
			return false
		}
	}
	return true
}

// stripAuthResults removes Authentication-Results fields that claim to
// come from authservID. Anyone can write such a field into a message
// they send; RFC 8601 §5 has the receiving server delete them, so that
// a mail client trusting this server's verdict sees only the real one.
// Fields naming other servers stay.
func stripAuthResults(raw []byte, authservID string) []byte {
	var out bytes.Buffer
	out.Grow(len(raw))
	rest := raw
	for len(rest) > 0 {
		field := nextHeaderField(rest)
		if len(bytes.TrimRight(field, "\r\n")) == 0 {
			break // the blank line: the header block is over
		}
		if !isAuthResultsFrom(field, authservID) {
			out.Write(field)
		}
		rest = rest[len(field):]
	}
	out.Write(rest)
	return out.Bytes()
}

// nextHeaderField returns the first header field of b, with its
// continuation lines and line endings.
func nextHeaderField(b []byte) []byte {
	end := 0
	for end < len(b) {
		nl := bytes.IndexByte(b[end:], '\n')
		if nl < 0 {
			return b
		}
		end += nl + 1
		if end >= len(b) || (b[end] != ' ' && b[end] != '\t') {
			break
		}
	}
	return b[:end]
}

// isAuthResultsFrom reports whether a raw header field is an
// Authentication-Results field naming authservID.
func isAuthResultsFrom(field []byte, authservID string) bool {
	name, value, ok := bytes.Cut(field, []byte(":"))
	if !ok || !strings.EqualFold(string(bytes.TrimSpace(name)), "Authentication-Results") {
		return false
	}
	id := strings.TrimSpace(string(value))
	for strings.HasPrefix(id, "(") { // a leading RFC 5322 comment
		end := strings.IndexByte(id, ')')
		if end < 0 {
			return false
		}
		id = strings.TrimSpace(id[end+1:])
	}
	if i := strings.IndexAny(id, "; \t\r\n"); i >= 0 {
		id = id[:i]
	}
	return strings.EqualFold(strings.Trim(id, `"`), authservID)
}

package webmail

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

// formatFromHeader returns a From-header value suitable for an
// outbound RFC 5322 message. When displayName is empty we emit the
// bare address — most senders historically had no display name and the
// recipient clients show the address either way; we keep that exact
// wire shape so older tests / threading heuristics don't drift.
//
// When displayName is set we delegate to net/mail.Address.String,
// which:
//   - emits "Alice" <addr@x> for pure-ASCII names with no specials,
//   - escapes/quotes ASCII names containing RFC 5322 specials
//     (e.g. "Alice, Inc." <addr@x>),
//   - encodes non-ASCII names as RFC 2047 encoded-words
//     (e.g. =?utf-8?q?Al=C3=AEce?= <addr@x>).
//
// That covers what any modern receiving client needs to render the
// human name; we don't need to re-implement the rules ourselves.
func formatFromHeader(displayName, address string) string {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return address
	}
	return (&mail.Address{Name: displayName, Address: address}).String()
}

// formatFromAddr returns the value we store on Message.FromAddr — used
// for inbox-list rendering, not for re-emitting onto the wire. The
// crucial difference from formatFromHeader: we keep the display name
// in **decoded** UTF-8 (`Barış Akın <addr>`) instead of RFC 2047
// (`=?utf-8?q?Bar=C4=B1=C5=9F_Ak=C4=B1n?= <addr>`). The SPA's
// senderName parser reads this field, and humans want to see the
// name, not the encoded-word transport form.
//
// Empty display name still collapses to the bare address so the wire
// shape of FromAddr stays unchanged for the historical case.
func formatFromAddr(displayName, address string) string {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return address
	}
	return displayName + " <" + address + ">"
}

// attachment is one file the user attached to an outbound message.
// Content is the raw bytes (already base64-decoded by the handler);
// Filename + ContentType land on the part header. A nil/empty
// ContentType defaults to application/octet-stream.
type attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// composeFields collects the fields a compose form provides. inReplyTo
// is the Message-ID of the message being replied to (no angle
// brackets); references is the existing References chain plus that
// id — buildMessage handles the angle bracketing.
type composeFields struct {
	from, subject, text, html, messageID, inReplyTo string
	to, cc, references                              []string
	attachments                                     []attachment
}

// buildMessage assembles a minimal RFC 5322 message from the fields a
// compose form provides.
//
// MIME structure picked based on what's present:
//   - no html, no attachments  → text/plain
//   - html, no attachments     → multipart/alternative (text + html)
//   - any attachments          → multipart/mixed with the body part
//                                first (either plain or alternative),
//                                followed by one part per attachment
func buildMessage(f composeFields) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", f.from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(f.to, ", "))
	if len(f.cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", strings.Join(f.cc, ", "))
	}
	// QEncoding.Encode leaves plain ASCII untouched and RFC 2047-encodes
	// anything else, so a non-ASCII subject stays well-formed.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", f.subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", f.messageID)
	if f.inReplyTo != "" {
		fmt.Fprintf(&b, "In-Reply-To: <%s>\r\n", f.inReplyTo)
	}
	if len(f.references) > 0 {
		var refs []string
		for _, r := range f.references {
			r = strings.TrimSpace(strings.Trim(r, "<>"))
			if r != "" {
				refs = append(refs, "<"+r+">")
			}
		}
		if len(refs) > 0 {
			fmt.Fprintf(&b, "References: %s\r\n", strings.Join(refs, " "))
		}
	}
	b.WriteString("MIME-Version: 1.0\r\n")

	bodyPart := renderBodyPart(f.text, f.html)
	if len(f.attachments) == 0 {
		// No attachments — the body part IS the message body. Strip
		// the leading MIME-Version header line we'd write twice
		// (renderBodyPart returns headers + body together).
		b.WriteString(bodyPart)
		return []byte(b.String())
	}

	// Attachments — wrap the body part and each attachment in
	// multipart/mixed.
	var mixed strings.Builder
	mw := multipart.NewWriter(&mixed)

	// First part: the body (which is itself either text/plain or a
	// nested multipart/alternative). We write it as a raw part — the
	// part header lines are already in bodyPart and they precede the
	// blank line + body bytes.
	bodyPartHeader, bodyPartBody := splitHeaderBody(bodyPart)
	if err := writeRawPart(mw, bodyPartHeader, bodyPartBody); err != nil {
		return []byte(b.String()) // best-effort; truncate cleanly
	}

	// Each attachment as its own part — base64 encoded, with
	// Content-Disposition: attachment so clients offer it as a file.
	for _, att := range f.attachments {
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ct)
		h.Set("Content-Transfer-Encoding", "base64")
		if att.Filename != "" {
			h.Set("Content-Disposition",
				fmt.Sprintf("attachment; filename=%q", att.Filename))
		} else {
			h.Set("Content-Disposition", "attachment")
		}
		part, err := mw.CreatePart(h)
		if err != nil {
			continue
		}
		// base64 with CRLF line wraps at 76 chars (RFC 2045 §6.8).
		wrapped := wrapBase64(att.Content)
		_, _ = part.Write([]byte(wrapped))
	}
	_ = mw.Close()

	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n", mw.Boundary())
	b.WriteString("\r\n")
	b.WriteString(mixed.String())
	return []byte(b.String())
}

// renderBodyPart returns the headers + body bytes for the message's
// content area — either a single text/plain block or a
// multipart/alternative wrapper. The string starts with one or more
// header lines, then a blank line, then the body.
func renderBodyPart(text, html string) string {
	if html == "" {
		var b strings.Builder
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
		b.WriteString("\r\n")
		b.WriteString(normalizeNewlines(text))
		return b.String()
	}
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	addPart(mw, "text/plain; charset=utf-8", text)
	addPart(mw, "text/html; charset=utf-8", html)
	_ = mw.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", mw.Boundary())
	b.WriteString("\r\n")
	b.WriteString(body.String())
	return b.String()
}

// splitHeaderBody splits a "headers\r\n\r\nbody" blob into the two
// halves. Used by buildMessage to re-wrap the body part as a nested
// multipart part inside multipart/mixed.
func splitHeaderBody(s string) (headers, body string) {
	if i := strings.Index(s, "\r\n\r\n"); i >= 0 {
		return s[:i], s[i+4:]
	}
	return s, ""
}

// writeRawPart writes a part whose Content-Type and other headers are
// already in `headers` (the unfolded "Header: value" lines, one per
// line, separated by CRLF). Used to nest a pre-rendered
// multipart/alternative block inside multipart/mixed without
// re-parsing.
func writeRawPart(mw *multipart.Writer, headers, body string) error {
	h := textproto.MIMEHeader{}
	for _, line := range strings.Split(headers, "\r\n") {
		if line == "" {
			continue
		}
		if c := strings.IndexByte(line, ':'); c > 0 {
			h.Set(strings.TrimSpace(line[:c]), strings.TrimSpace(line[c+1:]))
		}
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write([]byte(body))
	return err
}

// wrapBase64 encodes content and inserts CRLF every 76 characters per
// RFC 2045 §6.8.
func wrapBase64(content []byte) string {
	enc := base64.StdEncoding.EncodeToString(content)
	var b strings.Builder
	const w = 76
	for i := 0; i < len(enc); i += w {
		end := i + w
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end])
		b.WriteString("\r\n")
	}
	return b.String()
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

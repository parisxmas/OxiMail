package webmail

import (
	"bytes"
	"io"
	"strings"

	"github.com/emersion/go-message/mail"
)

// messageBody is the rendered body of a message: its plain-text and
// HTML parts (either may be empty), the recipients pulled from the
// header, and a listing of attachments.
type messageBody struct {
	To          []string
	Cc          []string
	Text        string
	HTML        string
	Attachments []attachmentInfo
}

// attachmentInfo describes one attachment. The content itself is not
// included — fetching it will be a separate endpoint.
type attachmentInfo struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

// renderBody parses a raw RFC 5322 message into a messageBody. It is
// tolerant: a message that will not parse as MIME is returned as a
// single plain-text part holding the raw bytes, so the reader still
// sees something.
func renderBody(raw []byte) messageBody {
	body := messageBody{Attachments: []attachmentInfo{}}

	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		body.Text = string(raw)
		return body
	}
	body.To = addressStrings(mr.Header, "To")
	body.Cc = addressStrings(mr.Header, "Cc")

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // stop at the first malformed part; keep what we have
		}
		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			content, _ := io.ReadAll(part.Body)
			ct, _, _ := h.ContentType()
			if strings.EqualFold(ct, "text/html") {
				if body.HTML == "" {
					body.HTML = string(content)
				}
			} else if body.Text == "" {
				body.Text = string(content)
			}
		case *mail.AttachmentHeader:
			content, _ := io.ReadAll(part.Body)
			filename, _ := h.Filename()
			ct, _, _ := h.ContentType()
			body.Attachments = append(body.Attachments, attachmentInfo{
				Filename:    filename,
				ContentType: ct,
				Size:        len(content),
			})
		}
	}
	return body
}

// extractAttachment re-parses raw, walks to the idx-th attachment, and
// returns its bytes, filename, and content type. ok is false when the
// message will not parse or has fewer than idx+1 attachments.
func extractAttachment(raw []byte, idx int) (content []byte, filename, contentType string, ok bool) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, "", "", false
	}
	n := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			return nil, "", "", false
		}
		if err != nil {
			return nil, "", "", false
		}
		h, isAttachment := part.Header.(*mail.AttachmentHeader)
		if !isAttachment {
			continue
		}
		if n != idx {
			n++
			continue
		}
		body, err := io.ReadAll(part.Body)
		if err != nil {
			return nil, "", "", false
		}
		filename, _ = h.Filename()
		contentType, _, _ = h.ContentType()
		return body, filename, contentType, true
	}
}

// renderText returns just the text representation of a message — for
// substring search over the body. HTML is stripped to its inner text
// in the crudest possible way: angle-bracketed tags removed.
func renderText(raw []byte) string {
	body := renderBody(raw)
	if body.Text != "" {
		return body.Text
	}
	// No text part, just HTML — strip tags so a search for "hello" hits
	// "<p>hello</p>".
	return stripTags(body.HTML)
}

// stripTags removes everything between '<' and '>'. Good enough for
// substring search; not safe for HTML rendering.
func stripTags(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	skip := false
	for _, r := range s {
		switch {
		case r == '<':
			skip = true
		case r == '>':
			skip = false
		case !skip:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// addressStrings parses a header field as an address list and returns
// the addresses formatted as strings; a missing or malformed field
// yields nil.
func addressStrings(h mail.Header, key string) []string {
	addrs, err := h.AddressList(key)
	if err != nil || len(addrs) == 0 {
		return nil
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return out
}

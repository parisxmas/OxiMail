// Package vacation implements the RFC 3834 part of an out-of-office
// auto-responder: deciding whether a message deserves a reply at all,
// and building a reply that another reasonable MTA will not loop on.
//
// Storage and the per-account rule itself live in internal/store
// (store.Vacation); this package is the pure-logic side, taking a
// rule + the incoming raw message and producing either "no reply" or
// the bytes to enqueue.
package vacation

import (
	"bytes"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"sync"
	"time"
)

// ShouldReply applies the RFC 3834 §2 rules: do not auto-reply to
// bounces, to mailing-list traffic, to other auto-replies, or to a
// message that does not have us in any "to" / "cc" header (best-
// effort defense against backscatter). The envelope sender is the
// SMTP MAIL FROM; selfAddresses are the addresses this account
// responds at (the primary plus any aliases — pass at least the
// account address).
func ShouldReply(raw []byte, envelopeSender string, selfAddresses []string) bool {
	// 3834 §2: a null envelope sender (RFC 5321 reverse-path "<>") is a
	// DSN / bounce. Never auto-reply to one.
	if strings.TrimSpace(envelopeSender) == "" {
		return false
	}
	hdr, err := readHeader(raw)
	if err != nil {
		return false
	}

	// Auto-Submitted other than "no" means the message itself is
	// machine-generated (RFC 3834 §5).
	if v := strings.ToLower(strings.TrimSpace(hdr.Get("Auto-Submitted"))); v != "" && v != "no" {
		return false
	}
	// Microsoft-style suppression hint.
	if hdr.Get("X-Auto-Response-Suppress") != "" {
		return false
	}
	// List-Id / List-Unsubscribe / Precedence indicate mailing-list
	// traffic.
	if hdr.Get("List-Id") != "" || hdr.Get("List-Unsubscribe") != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(hdr.Get("Precedence"))) {
	case "bulk", "list", "junk":
		return false
	}
	// Do not reply to "errors-to" type return paths.
	switch strings.ToLower(strings.TrimSpace(hdr.Get("Return-Path"))) {
	case "<>", "":
		// either bounce or absent — bounce already handled above
	}

	// Our address should appear somewhere in the recipient headers —
	// otherwise we're likely BCC'd into a list, and replying outs the
	// recipient.
	if len(selfAddresses) > 0 && !addressedToSelf(hdr, selfAddresses) {
		return false
	}
	return true
}

// Reply assembles the auto-response. The result is a complete RFC 5322
// message ready for store.Enqueue: From the account, To the original
// sender, In-Reply-To the original Message-Id, Auto-Submitted: auto-
// replied (so the receiving side knows not to loop), References
// chained from the original.
func Reply(rule Rule, originalRaw []byte, accountAddress, envelopeSender, messageID string, now time.Time) ([]byte, error) {
	if envelopeSender == "" {
		return nil, fmt.Errorf("vacation: empty envelope sender")
	}
	if rule.Body == "" {
		return nil, fmt.Errorf("vacation: empty body")
	}
	origHdr, _ := readHeader(originalRaw)
	origSubject := ""
	origID := ""
	origRefs := ""
	if origHdr != nil {
		origSubject = origHdr.Get("Subject")
		origID = strings.Trim(origHdr.Get("Message-Id"), "<>")
		origRefs = origHdr.Get("References")
	}

	subject := rule.Subject
	if subject == "" {
		subject = "Auto: " + origSubject
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", accountAddress)
	fmt.Fprintf(&b, "To: %s\r\n", envelopeSender)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", messageID)
	if origID != "" {
		fmt.Fprintf(&b, "In-Reply-To: <%s>\r\n", origID)
		if origRefs != "" {
			fmt.Fprintf(&b, "References: %s <%s>\r\n", origRefs, origID)
		} else {
			fmt.Fprintf(&b, "References: <%s>\r\n", origID)
		}
	}
	// RFC 3834 markers so the receiving MTA recognises this as a
	// machine-generated reply and does not auto-reply to it.
	b.WriteString("Auto-Submitted: auto-replied\r\n")
	b.WriteString("X-Auto-Response-Suppress: All\r\n")
	b.WriteString("Precedence: auto_reply\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	body := strings.ReplaceAll(rule.Body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	if !strings.HasSuffix(body, "\r\n") {
		body += "\r\n"
	}
	b.WriteString(body)
	return []byte(b.String()), nil
}

// Rule is the user-visible vacation configuration. It is the subset
// of store.Vacation that the reply builder needs; the store type
// embeds the same fields.
type Rule struct {
	Subject string
	Body    string
}

// readHeader parses just enough of the message to expose its header
// fields. A malformed message yields a nil header.
func readHeader(raw []byte) (mail.Header, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return msg.Header, nil
}

// addressedToSelf reports whether any of `selfs` appears in To, Cc,
// Resent-To, Resent-Cc, or Delivered-To.
func addressedToSelf(hdr mail.Header, selfs []string) bool {
	selfSet := make(map[string]bool, len(selfs))
	for _, s := range selfs {
		selfSet[strings.ToLower(strings.TrimSpace(s))] = true
	}
	for _, key := range []string{"To", "Cc", "Resent-To", "Resent-Cc", "Delivered-To"} {
		addrs, err := hdr.AddressList(key)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if selfSet[strings.ToLower(a.Address)] {
				return true
			}
		}
	}
	return false
}

// Suppressor caps how often a sender is auto-replied to. It is an
// in-memory LRU keyed by (account, sender) — losing it on restart
// just sends one extra reply, which is acceptable. A multi-instance
// deployment would back this with OxiMem.
type Suppressor struct {
	window time.Duration
	mu     sync.Mutex
	seen   map[suppressKey]time.Time
}

type suppressKey struct {
	accountID uint64
	sender    string
}

// NewSuppressor returns a suppressor that allows at most one auto-
// reply per sender per window.
func NewSuppressor(window time.Duration) *Suppressor {
	return &Suppressor{window: window, seen: map[suppressKey]time.Time{}}
}

// Allow reports whether we should send a reply to sender from account
// right now; it also records the attempt, so a follow-up call within
// the window returns false.
func (s *Suppressor) Allow(accountID uint64, sender string, now time.Time) bool {
	if s == nil {
		return true
	}
	k := suppressKey{accountID: accountID, sender: strings.ToLower(strings.TrimSpace(sender))}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.seen[k]; ok && now.Sub(prev) < s.window {
		return false
	}
	s.seen[k] = now
	return true
}

// Sweep drops entries older than the window so the map does not grow
// unboundedly. Safe to call from a background goroutine.
func (s *Suppressor) Sweep(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.seen {
		if now.Sub(t) >= s.window {
			delete(s.seen, k)
		}
	}
}

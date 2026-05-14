package queue

import (
	"bytes"
	"fmt"
	"log"
	"mime/multipart"
	"net/textproto"
	"strings"
	"time"

	"github.com/parisxmas/OxiMail/internal/store"
)

// bounceSubject is the Subject of a delivery-failure notification.
const bounceSubject = "Undelivered Mail Returned to Sender"

// bounce returns a delivery-status notification to the original sender,
// listing the recipients that could not be reached.
//
// It is a no-op for a message with a null envelope sender: never bounce
// a bounce, or delivery-failure notices loop forever.
func (q *Queue) bounce(m *store.OutboundMessage, original []byte, failures []rcptFailure) {
	for _, f := range failures {
		log.Printf("queue: message %d: %q permanently failed: %s", m.ID, f.recipient, f.reason)
	}
	if m.From == "" {
		return
	}

	report := buildBounce(q.hostname, m.From, failures, original)
	// Route the bounce with a null sender: a local original sender finds
	// it in their INBOX, a remote one is enqueued — and if that enqueued
	// bounce later fails, the null sender stops the loop here.
	if _, err := q.store.Route("", []string{m.From}, store.IncomingMessage{
		Raw:      report,
		Subject:  bounceSubject,
		FromAddr: "MAILER-DAEMON@" + q.hostname,
	}); err != nil {
		log.Printf("queue: message %d: could not return a bounce to %s: %v", m.ID, m.From, err)
	}
}

// buildBounce assembles an RFC 3464 multipart/report delivery-status
// notification: a human-readable explanation, a machine-readable
// per-recipient status, and the original message.
func buildBounce(hostname, sender string, failures []rcptFailure, original []byte) []byte {
	var parts bytes.Buffer
	mw := multipart.NewWriter(&parts)

	// Human-readable explanation.
	human, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/plain; charset=utf-8"},
	})
	fmt.Fprintf(human, "This is the OxiMail mail system at %s.\r\n\r\n", hostname)
	human.Write([]byte("Your message could not be delivered to one or more recipients:\r\n\r\n"))
	for _, f := range failures {
		fmt.Fprintf(human, "  %s\r\n      %s\r\n", f.recipient, oneLine(f.reason))
	}

	// Machine-readable per-recipient status.
	status, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"message/delivery-status"},
	})
	fmt.Fprintf(status, "Reporting-MTA: dns; %s\r\n", hostname)
	for _, f := range failures {
		fmt.Fprintf(status, "\r\nFinal-Recipient: rfc822; %s\r\n", f.recipient)
		status.Write([]byte("Action: failed\r\n"))
		status.Write([]byte("Status: 5.0.0\r\n"))
		fmt.Fprintf(status, "Diagnostic-Code: smtp; %s\r\n", oneLine(f.reason))
	}

	// The original message, so the sender can see and resend it.
	orig, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"message/rfc822"},
	})
	orig.Write(original)

	mw.Close()

	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: MAILER-DAEMON@%s\r\n", hostname)
	fmt.Fprintf(&msg, "To: %s\r\n", sender)
	fmt.Fprintf(&msg, "Subject: %s\r\n", bounceSubject)
	fmt.Fprintf(&msg, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	msg.WriteString("Auto-Submitted: auto-replied\r\n") // RFC 3834 — marks it auto-generated
	msg.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&msg, "Content-Type: multipart/report; report-type=delivery-status; boundary=%q\r\n",
		mw.Boundary())
	msg.WriteString("\r\n")
	msg.Write(parts.Bytes())
	return msg.Bytes()
}

// oneLine flattens a string to a single line, for use in a header value.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

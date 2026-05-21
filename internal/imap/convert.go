package imap

import (
	"bufio"
	"bytes"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"github.com/parisxmas/OxiMail/internal/store"
)

// toIMAPFlags converts the store's string flags to imap.Flag values.
func toIMAPFlags(flags []string) []imap.Flag {
	out := make([]imap.Flag, len(flags))
	for i, f := range flags {
		out[i] = imap.Flag(f)
	}
	return out
}

// flagsToStrings converts imap.Flag values to the store's string flags.
func flagsToStrings(flags []imap.Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}

// hasFlag reports whether flag is present in flags.
func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// unionFlags returns base with add merged in, without duplicates.
func unionFlags(base, add []string) []string {
	out := append([]string(nil), base...)
	for _, f := range add {
		if !hasFlag(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// minusFlags returns base with every flag in remove dropped.
func minusFlags(base, remove []string) []string {
	out := make([]string, 0, len(base))
	for _, f := range base {
		if !hasFlag(remove, f) {
			out = append(out, f)
		}
	}
	return out
}

// parseTime parses a stored RFC 3339 timestamp; an unparseable value
// yields the zero time.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// extractEnvelope parses the message header block into an IMAP envelope.
// A header that will not parse yields a nil envelope, which the caller
// skips rather than failing the whole FETCH.
func extractEnvelope(raw []byte) *imap.Envelope {
	header, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil
	}
	return imapserver.ExtractEnvelope(header)
}

// parseIncoming builds a store.IncomingMessage from raw RFC 5322 bytes,
// pulling out the header fields the store keeps as metadata. A header
// that will not parse is not fatal — the raw bytes are authoritative.
func parseIncoming(raw []byte) store.IncomingMessage {
	in := store.IncomingMessage{Raw: raw}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return in
	}
	in.Subject = msg.Header.Get("Subject")
	in.MessageID = strings.Trim(msg.Header.Get("Message-Id"), "<>")
	if addr, err := mail.ParseAddress(msg.Header.Get("From")); err == nil {
		// Preserve the display name when the From header carries one,
		// so the list view renders `"Alice" <addr>` instead of the bare
		// address. Empty Name keeps the historical bare-address shape.
		if addr.Name != "" {
			in.FromAddr = addr.String()
		} else {
			in.FromAddr = addr.Address
		}
	}
	return in
}

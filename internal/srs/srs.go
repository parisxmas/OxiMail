// Package srs implements the Sender Rewriting Scheme (SRS), which
// makes alias-forwarding survive SPF and DMARC at the next hop.
//
// When OxiMail forwards a message from <alice@aol.example> to
// <bob@gmail.example> through an alias on <us.example>, the destination
// MX sees an envelope sender that doesn't authorize <us.example> in
// its SPF record — and the mail gets rejected. SRS rewrites the
// envelope sender to <SRS0=HHHH=TT=aol.example=alice@us.example> so
// the visible domain is now <us.example>, whose SPF DOES authorize
// us. When the destination MX bounces, the bounce comes back to that
// SRS address; Decode unpacks the original address and we forward the
// bounce there.
//
// This implementation follows the SRS0 scheme described in
// https://www.libsrs2.org/srs/srs.pdf, with two simplifications:
//
//   - HMAC-SHA256 is used for the integrity hash (truncated to four
//     base32 characters), instead of the spec's HMAC-SHA1. Library
//     compat would require SHA1; here we only round-trip our own
//     addresses.
//   - SRS1 chaining (forwards of forwards) is not implemented. Two
//     hops through OxiMail is fine — the second hop sees an SRS0
//     address and rewrites it the same way; we accept its bounce.
package srs

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"time"
)

// prefix is the literal SRS0 marker that introduces a rewritten
// address.
const prefix = "SRS0"

// b32 is base32 without padding; the spec uses uppercase, we follow.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrNotSRS is returned by Decode when the address is well-formed but
// is not an SRS-rewritten address — i.e. it does not start with
// "SRS0=". Callers use this to distinguish "should not have called
// Decode" from "tampered SRS address".
var ErrNotSRS = errors.New("srs: not an SRS-encoded address")

// Encode rewrites sender so that bounces come back to a hosted
// address under forwarderDomain. secret must be a stable, secret value
// of at least 16 bytes — losing it invalidates every outstanding SRS
// address (bounces will then fail to decode).
//
// The produced address is:
//
//	SRS0=HHHH=TT=originalDomain=originalLocal@forwarderDomain
//
// where TT is a two-character base32 day-of-year-mod-1024 timestamp
// and HHHH is the first four base32 characters of
// HMAC-SHA256(secret, TT=originalDomain=originalLocal).
func Encode(secret []byte, sender, forwarderDomain string) (string, error) {
	if len(secret) < 16 {
		return "", fmt.Errorf("srs: secret must be at least 16 bytes")
	}
	if forwarderDomain == "" {
		return "", fmt.Errorf("srs: forwarder domain must not be empty")
	}
	local, domain, err := splitAddr(sender)
	if err != nil {
		return "", err
	}
	tt := timestamp(time.Now())
	payload := tt + "=" + domain + "=" + local
	hash := hmacShort(secret, payload)
	return fmt.Sprintf("%s=%s=%s@%s", prefix, hash, payload, forwarderDomain), nil
}

// Decode reverses Encode: given an SRS-rewritten address that travels
// back to OxiMail (e.g. as the RCPT TO of a bounce), it returns the
// original sender address. The integrity hash must verify and the
// embedded timestamp must be no older than maxAge.
//
// Returns ErrNotSRS when address does not look like an SRS address —
// the caller can fall back to "plain" recipient resolution.
func Decode(secret []byte, address string, maxAge time.Duration) (string, error) {
	if len(secret) < 16 {
		return "", fmt.Errorf("srs: secret must be at least 16 bytes")
	}
	local, fwdDomain, err := splitAddr(address)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(local, prefix+"=") {
		return "", ErrNotSRS
	}
	parts := strings.SplitN(local[len(prefix)+1:], "=", 4)
	if len(parts) != 4 {
		return "", fmt.Errorf("srs: malformed SRS local-part %q", local)
	}
	hash, tt, origDomain, origLocal := parts[0], parts[1], parts[2], parts[3]
	payload := tt + "=" + origDomain + "=" + origLocal
	if !hmac.Equal([]byte(hash), []byte(hmacShort(secret, payload))) {
		return "", fmt.Errorf("srs: hash mismatch on %q", address)
	}
	if maxAge > 0 {
		age, err := decodeAge(tt, time.Now())
		if err != nil {
			return "", err
		}
		if age > maxAge {
			return "", fmt.Errorf("srs: address is %s old (>%s)", age.Round(time.Hour), maxAge)
		}
	}
	_ = fwdDomain // the forwarder domain on the wrapper isn't recovered
	return origLocal + "@" + origDomain, nil
}

// Is reports whether address looks like an SRS-rewritten address —
// callers use it as a quick filter before calling Decode.
func Is(address string) bool {
	local, _, err := splitAddr(address)
	if err != nil {
		return false
	}
	return strings.HasPrefix(local, prefix+"=")
}

// splitAddr returns the local and domain parts of an email address.
func splitAddr(address string) (local, domain string, err error) {
	at := strings.LastIndexByte(address, '@')
	if at <= 0 || at == len(address)-1 {
		return "", "", fmt.Errorf("srs: %q is not a valid address", address)
	}
	return address[:at], address[at+1:], nil
}

// timestamp returns the two-character base32 encoding of the day of
// the SRS epoch (1 January 1970) modulo 1024. The wrap-around horizon
// is 2.8 years, which is far longer than any reasonable bounce window.
func timestamp(now time.Time) string {
	day := uint16(now.Unix()/86400) & 0x3ff
	var b [2]byte
	b[0] = byte((day >> 5) & 0x1f)
	b[1] = byte(day & 0x1f)
	return string([]byte{tableChar(b[0]), tableChar(b[1])})
}

// decodeAge returns how long ago the timestamp in tt was generated.
// It picks the most recent decoding consistent with the 10-bit
// counter, so a one-character drift across the wrap boundary still
// gives a sensible answer.
func decodeAge(tt string, now time.Time) (time.Duration, error) {
	if len(tt) != 2 {
		return 0, fmt.Errorf("srs: timestamp %q is not 2 chars", tt)
	}
	hi, ok1 := charValue(tt[0])
	lo, ok2 := charValue(tt[1])
	if !ok1 || !ok2 {
		return 0, fmt.Errorf("srs: timestamp %q has non-base32 chars", tt)
	}
	day := int64(hi)<<5 | int64(lo)
	todayDay := now.Unix() / 86400
	encodedDay := todayDay - ((todayDay - day) & 0x3ff)
	return time.Since(time.Unix(encodedDay*86400, 0)), nil
}

// hmacShort returns the first four base32 characters of HMAC-SHA256
// of payload, keyed by secret.
func hmacShort(secret []byte, payload string) string {
	h := hmac.New(sha256.New, secret)
	_, _ = h.Write([]byte(payload))
	return b32.EncodeToString(h.Sum(nil))[:4]
}

// tableChar maps a 5-bit value to its uppercase base32 character.
func tableChar(v byte) byte {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	return table[v&0x1f]
}

// charValue is the reverse of tableChar: it returns the 5-bit value
// for an uppercase base32 character.
func charValue(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= '2' && c <= '7':
		return c - '2' + 26, true
	}
	return 0, false
}

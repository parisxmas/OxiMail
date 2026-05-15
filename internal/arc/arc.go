// Package arc implements RFC 8617 Authenticated Received Chain on the
// *sealing* side: when OxiMail forwards a message (alias-forwarding
// to a remote address), it prepends a fresh ARC instance — ARC-
// Authentication-Results, ARC-Message-Signature, ARC-Seal — so the
// destination MTA can attest that the message has not been modified
// since it passed through us, and inherits its prior authentication
// results through the chain.
//
// Scope of this implementation:
//
//   - Sealing is supported for messages with no prior ARC chain
//     (the common alias-forward case: a remote sender → us → another
//     remote recipient). The new instance is i=1 with cv=none.
//   - When a prior chain already exists, Seal does NOT extend it.
//     Verifying an existing chain (cv=pass/fail) requires running
//     RFC 8617 §5.2 chain validation across every prior hop with
//     DNS-fetched public keys; that is a separate effort. Callers
//     can detect the situation via HasChain and decide whether to
//     pass the message through unmodified or refuse forwarding.
//
// The signing primitives are RSA-SHA256 with relaxed/relaxed
// canonicalization (RFC 6376 §3.4) — the same algorithm DKIM uses,
// reused here directly so an operator's existing per-domain DKIM key
// (selector + PEM) also signs ARC headers without further setup.
//
// VerifyLastInstance is a self-check used by tests and operators
// debugging their own sealing path; it is NOT a full ARC chain
// validator.
package arc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ChainValidation is the cv= value for an ARC-Seal: "none" on the
// first instance, "pass" / "fail" on subsequent instances reflecting
// whether the prior chain verified.
type ChainValidation string

const (
	ChainNone ChainValidation = "none"
	ChainPass ChainValidation = "pass"
	ChainFail ChainValidation = "fail"
)

// SealOptions configures one sealing pass.
type SealOptions struct {
	Domain   string          // d= value, signing domain
	Selector string          // s= value, picks the DNS record
	Key      *rsa.PrivateKey // the signing key (PKCS#1 RSA)
	// AuthResults is the body of the ARC-Authentication-Results
	// header, after the "i=N;" prefix that Seal adds itself. A
	// typical value looks like:
	//
	//   oximail.test; spf=pass smtp.mailfrom=alice@partners.test;
	//     dkim=pass header.d=partners.test; dmarc=pass
	AuthResults string
	// Now lets tests pin t=. Defaults to time.Now when zero.
	Now time.Time
}

// HasChain reports whether raw already has at least one ARC instance
// (any of ARC-Authentication-Results / ARC-Message-Signature / ARC-
// Seal headers present).
func HasChain(raw []byte) bool {
	headers, _ := splitHeadersBody(raw)
	for _, h := range splitHeaders(headers) {
		switch strings.ToLower(headerName(h)) {
		case "arc-authentication-results", "arc-message-signature", "arc-seal":
			return true
		}
	}
	return false
}

// Seal prepends a fresh ARC instance to raw and returns the modified
// message. The instance is i=1 with cv=none; when the input already
// has a chain, Seal returns ErrPriorChain unchanged.
//
// Sealing failures (missing key, sign error) bubble up — callers that
// want to fail open should treat any non-nil error as "skip sealing"
// rather than as a delivery failure.
func Seal(raw []byte, opts SealOptions) ([]byte, error) {
	if opts.Key == nil {
		return nil, errors.New("arc: no signing key")
	}
	if opts.Domain == "" || opts.Selector == "" {
		return nil, errors.New("arc: domain and selector are required")
	}
	if HasChain(raw) {
		return raw, ErrPriorChain
	}
	headers, body := splitHeadersBody(raw)
	if len(headers) == 0 {
		return nil, errors.New("arc: empty message")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	const instance = 1
	bh := bodyHash(body)
	aar := fmt.Sprintf("ARC-Authentication-Results: i=%d; %s", instance,
		strings.TrimSpace(opts.AuthResults))

	// Build the AMS that signs the body + a fixed set of headers.
	amsHeaderList := []string{"From", "To", "Cc", "Subject", "Date", "Message-Id"}
	amsBase := fmt.Sprintf(
		"ARC-Message-Signature: i=%d; a=rsa-sha256; c=relaxed/relaxed; d=%s; s=%s; t=%d; h=%s; bh=%s; b=",
		instance, opts.Domain, opts.Selector, now.Unix(),
		strings.Join(amsHeaderList, ":"), bh,
	)
	amsSig, err := signHeaders(opts.Key, headers, amsHeaderList, amsBase)
	if err != nil {
		return nil, fmt.Errorf("arc: AMS sign: %w", err)
	}
	ams := amsBase + amsSig

	// Build the AS that signs every ARC header in this instance.
	asBase := fmt.Sprintf(
		"ARC-Seal: i=%d; a=rsa-sha256; cv=none; d=%s; s=%s; t=%d; b=",
		instance, opts.Domain, opts.Selector, now.Unix(),
	)
	asSig, err := signARCSet(opts.Key, []string{aar, ams, asBase})
	if err != nil {
		return nil, fmt.Errorf("arc: AS sign: %w", err)
	}
	as := asBase + asSig

	// Per RFC 8617 §5.1.3 the new ARC set is prepended to the
	// existing header block, in the order AS, AMS, AAR (most-
	// significant first — readers walk top-down).
	out := []byte(foldHeader(as) + "\r\n")
	out = append(out, []byte(foldHeader(ams)+"\r\n")...)
	out = append(out, []byte(foldHeader(aar)+"\r\n")...)
	out = append(out, headers...)
	out = append(out, '\r', '\n')
	out = append(out, body...)
	return out, nil
}

// ErrPriorChain is returned by Seal when raw already carries an ARC
// chain; the caller decides whether to pass the message through
// unchanged or refuse.
var ErrPriorChain = errors.New("arc: a prior ARC chain exists; not extending")

// VerifyLastInstance validates the most recent ARC instance against
// the current message: it re-computes the body hash, re-canonicalises
// the AMS-signed headers + the AMS-with-empty-b=, re-canonicalises
// the AS-signed ARC headers + the AS-with-empty-b=, and verifies both
// RSA signatures against publicKey.
//
// It does NOT walk the full chain; it confirms the seal we just laid
// down is internally consistent. Tests use it for round-trips.
func VerifyLastInstance(raw []byte, publicKey *rsa.PublicKey) error {
	headers, body := splitHeadersBody(raw)
	arcSet := collectARCSet(headers, highestInstance(headers))
	if arcSet.ams == "" || arcSet.as == "" {
		return errors.New("arc: no ARC-Seal / ARC-Message-Signature found")
	}

	// AMS check: hash body, then verify b= over the listed headers +
	// the AMS itself with b= empty.
	bh := bodyHash(body)
	if got := extractTag(arcSet.ams, "bh"); got != bh {
		return fmt.Errorf("arc: body hash mismatch: header says %s, computed %s", got, bh)
	}
	amsHList := splitColonList(extractTag(arcSet.ams, "h"))
	amsB := extractTag(arcSet.ams, "b")
	amsBase := stripBTag(arcSet.ams)
	if err := verifyHeaderSig(publicKey, headers, amsHList, amsBase, amsB); err != nil {
		return fmt.Errorf("arc: AMS signature: %w", err)
	}

	// AS check: re-canonicalize ARC set in instance order with the
	// last AS's b= stripped, then verify b= against it.
	asB := extractTag(arcSet.as, "b")
	asBase := stripBTag(arcSet.as)
	if err := verifyARCSet(publicKey, []string{arcSet.aar, arcSet.ams, asBase}, asB); err != nil {
		return fmt.Errorf("arc: AS signature: %w", err)
	}
	return nil
}

// arcSet is the three headers of one instance.
type arcSet struct{ aar, ams, as string }

func collectARCSet(headers []byte, instance int) arcSet {
	var set arcSet
	for _, h := range splitHeaders(headers) {
		i := extractTagInt(h, "i")
		if i != instance {
			continue
		}
		switch strings.ToLower(headerName(h)) {
		case "arc-authentication-results":
			set.aar = h
		case "arc-message-signature":
			set.ams = h
		case "arc-seal":
			set.as = h
		}
	}
	return set
}

func highestInstance(headers []byte) int {
	max := 0
	for _, h := range splitHeaders(headers) {
		switch strings.ToLower(headerName(h)) {
		case "arc-authentication-results", "arc-message-signature", "arc-seal":
			if i := extractTagInt(h, "i"); i > max {
				max = i
			}
		}
	}
	return max
}

// headerName returns the field name (before the ':') of a raw header.
func headerName(h string) string {
	if i := strings.IndexByte(h, ':'); i > 0 {
		return strings.TrimSpace(h[:i])
	}
	return ""
}

// extractTag returns the value of a DKIM-style "tag=value" pair from
// a header's body (after the ':'). Whitespace around the value is
// trimmed; the value itself may contain spaces (e.g. base64) as long
// as the next "; " separator marks its end.
func extractTag(header, tag string) string {
	// strip the field name + colon
	body := header
	if i := strings.IndexByte(body, ':'); i >= 0 {
		body = body[i+1:]
	}
	re := regexp.MustCompile(`(?:^|;)\s*` + regexp.QuoteMeta(tag) + `\s*=\s*([^;]*)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	v := strings.TrimSpace(m[1])
	// b= and bh= contain base64 that may include whitespace from
	// folding — collapse it.
	if tag == "b" || tag == "bh" {
		v = strings.Join(strings.Fields(v), "")
	}
	return v
}

// extractTagInt is extractTag for integer tags (i=, t=).
func extractTagInt(header, tag string) int {
	n, _ := strconv.Atoi(extractTag(header, tag))
	return n
}

// stripBTag returns the header with its b= value emptied (the value
// remains visually present in the header for senders to fill in /
// canonicalization to consume, but its content is replaced with "").
// RFC 8617 §5.1.2 and RFC 6376 §3.7 require this before signing or
// verifying.
func stripBTag(header string) string {
	re := regexp.MustCompile(`(?m)(b\s*=\s*)[^;]*`)
	// Only replace the LAST b= occurrence — bh= comes first; we don't
	// want to clobber that, and the b= we care about is at the end.
	loc := re.FindAllStringIndex(header, -1)
	if len(loc) == 0 {
		return header
	}
	last := loc[len(loc)-1]
	// Find the actual "b=" (not "bh=") by scanning back from each
	// match position.
	for i := len(loc) - 1; i >= 0; i-- {
		start := loc[i][0]
		// Ensure this is bare "b" and not "bh".
		if start > 0 && header[start-1] == 'h' {
			continue
		}
		// We found the right one.
		last = loc[i]
		break
	}
	// Replace the run after the "=" up to the end of the match.
	prefix := header[:last[0]]
	match := header[last[0]:last[1]]
	suffix := header[last[1]:]
	// Re-emit "b=" with empty value.
	eq := strings.IndexByte(match, '=')
	return prefix + match[:eq+1] + suffix
}

// splitColonList splits an "h=" header list ("Subject:From:To").
func splitColonList(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ":")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// foldHeader inserts a CRLF + tab fold before each "; " separator
// past a width threshold so the rendered header line is not absurdly
// long. Receivers must unfold; the DKIM/ARC canonicalization treats
// unfolded and folded forms identically.
func foldHeader(h string) string {
	const limit = 76
	var b strings.Builder
	col := 0
	for i := 0; i < len(h); i++ {
		c := h[i]
		if c == ';' && col > limit && i+1 < len(h) {
			b.WriteByte(';')
			b.WriteString("\r\n\t")
			col = 1
			// Skip following space, if present.
			if i+1 < len(h) && h[i+1] == ' ' {
				i++
			}
			continue
		}
		b.WriteByte(c)
		if c == '\n' {
			col = 0
		} else {
			col++
		}
	}
	return b.String()
}

// -----------------------------------------------------------------------
// Signing primitives
// -----------------------------------------------------------------------

// bodyHash returns the base64-encoded SHA-256 of the relaxed-
// canonicalized body.
func bodyHash(body []byte) string {
	h := sha256.Sum256(canonicalizeBody(body))
	return base64.StdEncoding.EncodeToString(h[:])
}

// signHeaders signs an ordered list of headers (looked up in the raw
// header block) plus the to-be-signed header itself (with b= empty),
// per RFC 6376. Returns the base64-encoded signature.
func signHeaders(key *rsa.PrivateKey, rawHeaders []byte, hList []string, tbsHeader string) (string, error) {
	canonical := canonicalHeaderList(rawHeaders, hList)
	// The signing header (e.g. the AMS) terminates the signed input
	// with NO trailing CRLF, per RFC 6376 §3.7.
	tbs := tbsHeader
	tbsStripped := tbs
	// Make sure b= is empty in the to-be-signed copy.
	tbsStripped = stripBTag(tbsStripped)
	// canonicalize the tbs header without a trailing CRLF.
	name := headerName(tbsStripped)
	val := tbsStripped[len(name)+1:]
	cTBS := canonicalizeHeader(name, val)
	cTBS = strings.TrimRight(cTBS, "\r\n")

	input := append([]byte(canonical), cTBS...)
	hash := sha256.Sum256(input)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// signARCSet signs the AS-input: all three ARC headers of the
// instance in fixed order (AAR, AMS, AS-with-b=empty), each
// canonicalized in relaxed form. The final AS canonicalization has
// no trailing CRLF (RFC 8617 §5.1.2).
func signARCSet(key *rsa.PrivateKey, set []string) (string, error) {
	canon := arcSetCanonical(set)
	hash := sha256.Sum256(canon)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// verifyHeaderSig is the inverse of signHeaders.
func verifyHeaderSig(pub *rsa.PublicKey, rawHeaders []byte, hList []string, tbsHeader, sigB64 string) error {
	canonical := canonicalHeaderList(rawHeaders, hList)
	name := headerName(tbsHeader)
	val := tbsHeader[len(name)+1:]
	cTBS := canonicalizeHeader(name, val)
	cTBS = strings.TrimRight(cTBS, "\r\n")
	input := append([]byte(canonical), cTBS...)
	hash := sha256.Sum256(input)
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig)
}

// verifyARCSet is the inverse of signARCSet.
func verifyARCSet(pub *rsa.PublicKey, set []string, sigB64 string) error {
	canon := arcSetCanonical(set)
	hash := sha256.Sum256(canon)
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, hash[:], sig)
}

// canonicalHeaderList concatenates the relaxed canonicalization of
// each header in hList, in order, looking each up by name in
// rawHeaders. A missing header contributes nothing (empty).
func canonicalHeaderList(rawHeaders []byte, hList []string) string {
	var b strings.Builder
	headers := splitHeaders(rawHeaders)
	for _, want := range hList {
		for _, h := range headers {
			if strings.EqualFold(headerName(h), want) {
				name := headerName(h)
				val := h[len(name)+1:]
				b.WriteString(canonicalizeHeader(name, val))
				break
			}
		}
	}
	return b.String()
}

// arcSetCanonical canonicalizes the AAR / AMS / AS triple. The first
// two get standard relaxed treatment with CRLF; the third (AS) gets
// no trailing CRLF, matching the AMS-signing convention.
func arcSetCanonical(set []string) []byte {
	var b strings.Builder
	for i, h := range set {
		name := headerName(h)
		val := h[len(name)+1:]
		c := canonicalizeHeader(name, val)
		if i == len(set)-1 {
			c = strings.TrimRight(c, "\r\n")
		}
		b.WriteString(c)
	}
	return []byte(b.String())
}

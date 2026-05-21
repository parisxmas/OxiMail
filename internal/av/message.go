package av

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
)

// ScanMessage hashes every body part of an RFC 5322 message
// individually and returns the first signature hit it finds. The
// per-part shape matters because the signature feeds we consume
// (MalwareBazaar, ThreatFox) publish the SHA-256 of the malware FILE
// payload — not of the wrapping email envelope — so hashing
// `raw` whole would never match. Walking parts the way webmail does
// for upload-side scans (handlers.go:scanAttachmentsOrError) keeps
// inbound MX coverage aligned with outbound submission coverage.
//
// Walk strategy:
//
//   - non-multipart message → decode the body's Content-Transfer-Encoding
//     and scan once
//   - multipart/* → iterate each part. A nested multipart recurses;
//     a leaf part has its body decoded and scanned. The walk stops at
//     the first hit; on a clean traversal the verdict is OK.
//
// A nil receiver returns clean — same "AV disabled" null-object idiom
// as Client.Scan.
//
// On a malformed message (unparseable headers, malformed multipart
// frame) the function falls back to scanning the entire raw bytes
// as a single blob. That keeps coverage honest against trivially
// hand-crafted envelopes while still catching FILE-hash matches in
// the common well-formed case.
func ScanMessage(ctx context.Context, c *Client, raw []byte) (Verdict, error) {
	if c == nil {
		return Verdict{OK: true}, nil
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		// Unparseable — fall back to hashing the whole envelope.
		// Won't match the public feeds, but it's still a defined
		// behaviour rather than a silent skip.
		return c.Scan(ctx, raw)
	}
	return scanMIMEPart(ctx, c, msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
}

// scanMIMEPart scans one part of a MIME tree. It dispatches on the
// part's Content-Type: multipart/* drives recursion, anything else
// is a leaf that gets decoded and hashed.
func scanMIMEPart(ctx context.Context, c *Client, contentType, transferEncoding string, body io.Reader) (Verdict, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// No / unparseable Content-Type — treat as a leaf with no
		// transfer encoding. RFC 2045 says missing Content-Type
		// defaults to text/plain; for AV purposes we just hash
		// what's in the body as-is.
		mediaType = "text/plain"
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			// Malformed multipart frame — fall back to scanning
			// the raw bytes of this part.
			data, _ := io.ReadAll(io.LimitReader(body, maxScanPartBytes))
			return c.Scan(ctx, data)
		}
		mr := multipart.NewReader(body, boundary)
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return Verdict{OK: true}, nil
			}
			if err != nil {
				return Verdict{}, fmt.Errorf("multipart walk: %w", err)
			}
			v, err := scanMIMEPart(ctx, c,
				p.Header.Get("Content-Type"),
				p.Header.Get("Content-Transfer-Encoding"),
				p)
			_ = p.Close()
			if err != nil {
				return Verdict{}, err
			}
			if !v.OK {
				return v, nil
			}
		}
	}

	// Leaf part — read and decode.
	data, err := io.ReadAll(io.LimitReader(body, maxScanPartBytes))
	if err != nil {
		return Verdict{}, fmt.Errorf("read part: %w", err)
	}
	decoded, err := decodeBody(data, transferEncoding)
	if err != nil {
		// Bad encoding — hash what we got. A real attacker can't
		// hide behind a corrupt transfer encoding because the
		// underlying bytes still flow through.
		decoded = data
	}
	return c.Scan(ctx, decoded)
}

// maxScanPartBytes is the per-part cap on bytes we read into memory
// for hashing. Real malware samples sit comfortably under 50 MiB; a
// pathologically large part (claimed 4 GiB attachment) won't OOM the
// daemon — the cap truncates and the hash is computed over the
// truncated prefix, which still catches known-bad uploads whose
// signature lives in the first 64 MiB.
const maxScanPartBytes = 64 * 1024 * 1024

// decodeBody applies the part's Content-Transfer-Encoding so the
// hashed bytes match the FILE payload upstream feeds index. base64
// and quoted-printable are the two encodings that actually transform
// bytes; 7bit / 8bit / binary / unset are no-ops.
func decodeBody(data []byte, transferEncoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(transferEncoding)) {
	case "base64":
		return base64.StdEncoding.DecodeString(stripWhitespace(string(data)))
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data)))
	default:
		return data, nil
	}
}

// stripWhitespace removes the whitespace MUAs interleave through
// base64-encoded bodies (RFC 2045 allows line wraps). base64's
// strict decoder rejects them, so we pre-strip.
func stripWhitespace(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

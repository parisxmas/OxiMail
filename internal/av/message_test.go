package av

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eicarStr is the public test signature whose SHA-256 is shipped in
// av/builtin.sigdb. Any well-formed MIME wrapper that embeds these
// exact bytes as a leaf body must be flagged by ScanMessage,
// regardless of how it's encoded.
// eicarStr is the string form of the eicar []byte declared in av_test.go.
// Tests in this file embed it into RFC 5322 messages, so a string is
// the convenient shape.
var eicarStr = string(eicar)

func newClientWithBuiltin(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	_ = dir
	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func newClientWithExtraHash(t *testing.T, hash, name string) *Client {
	t.Helper()
	dir := t.TempDir()
	sigdb := filepath.Join(dir, "extra.sigdb")
	if err := os.WriteFile(sigdb, []byte(hash+":"+name+"\n"), 0o644); err != nil {
		t.Fatalf("write sigdb: %v", err)
	}
	c, err := New(sigdb)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestScanMessageNilClientIsClean(t *testing.T) {
	v, err := ScanMessage(context.Background(), nil, []byte("anything"))
	if err != nil {
		t.Fatalf("ScanMessage(nil): %v", err)
	}
	if !v.OK {
		t.Errorf("nil client should be clean, got %+v", v)
	}
}

func TestScanMessageDetectsEicarInPlainBody(t *testing.T) {
	c := newClientWithBuiltin(t)
	raw := "From: a@b.test\r\nTo: c@d.test\r\nSubject: t\r\nContent-Type: text/plain\r\n\r\n" + eicarStr
	v, err := ScanMessage(context.Background(), c, []byte(raw))
	if err != nil {
		t.Fatalf("ScanMessage: %v", err)
	}
	if v.OK {
		t.Error("EICAR in plain body should hit the builtin signature")
	}
}

func TestScanMessageDetectsEicarInMultipartAttachment(t *testing.T) {
	c := newClientWithBuiltin(t)
	raw := buildMultipart("BB",
		mimePart("text/plain", "", "clean body"),
		mimePart("application/octet-stream", "", eicarStr),
	)
	v, err := ScanMessage(context.Background(), c, []byte(raw))
	if err != nil {
		t.Fatalf("ScanMessage: %v", err)
	}
	if v.OK {
		t.Error("EICAR as an attachment should hit even though the wrapping envelope hash doesn't")
	}
}

func TestScanMessageDecodesBase64Body(t *testing.T) {
	c := newClientWithBuiltin(t)
	encoded := base64.StdEncoding.EncodeToString([]byte(eicarStr))
	raw := buildMultipart("BB",
		mimePart("text/plain", "", "clean"),
		mimePart("application/octet-stream", "base64", encoded),
	)
	v, err := ScanMessage(context.Background(), c, []byte(raw))
	if err != nil {
		t.Fatalf("ScanMessage: %v", err)
	}
	if v.OK {
		t.Error("EICAR delivered base64-encoded must decode + hit the builtin signature")
	}
}

func TestScanMessageDecodesQuotedPrintableBody(t *testing.T) {
	// quoted-printable encoded EICAR (only the special chars get
	// escaped; everything else passes through literally).
	c := newClientWithBuiltin(t)
	qp := "X5O!P%=40AP[4\\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*"
	raw := buildMultipart("BB",
		mimePart("application/octet-stream", "quoted-printable", qp),
	)
	v, err := ScanMessage(context.Background(), c, []byte(raw))
	if err != nil {
		t.Fatalf("ScanMessage: %v", err)
	}
	if v.OK {
		t.Error("EICAR delivered quoted-printable must decode + hit the builtin signature")
	}
}

func TestScanMessageDetectsEicarInNestedMultipart(t *testing.T) {
	// outer = multipart/mixed, inner = multipart/alternative with
	// the EICAR sitting in a deeper part. The walker has to recurse.
	c := newClientWithBuiltin(t)
	inner := buildMultipartRaw("INNER",
		mimePart("text/plain", "", "decoy"),
		mimePart("application/octet-stream", "", eicarStr),
	)
	raw := buildMultipartCustom("BB",
		mimePart("text/plain", "", "outer decoy"),
		"Content-Type: multipart/alternative; boundary=INNER\r\n\r\n"+inner,
	)
	v, err := ScanMessage(context.Background(), c, []byte(raw))
	if err != nil {
		t.Fatalf("ScanMessage: %v", err)
	}
	if v.OK {
		t.Error("EICAR in nested multipart must be reached by recursion")
	}
}

func TestScanMessageReportsCleanWhenNoPartMatches(t *testing.T) {
	c := newClientWithBuiltin(t)
	raw := buildMultipart("BB",
		mimePart("text/plain", "", "hello world"),
		mimePart("application/pdf", "base64",
			base64.StdEncoding.EncodeToString([]byte("not a real pdf"))),
	)
	v, err := ScanMessage(context.Background(), c, []byte(raw))
	if err != nil {
		t.Fatalf("ScanMessage: %v", err)
	}
	if !v.OK {
		t.Errorf("clean message reported as hit: %+v", v)
	}
}

func TestScanMessageMatchesPerPartHash(t *testing.T) {
	// Confirm that a feed-style file-hash on a specific attachment
	// payload is matched correctly even when the message envelope's
	// own hash would not match.
	payload := []byte("a specific malware sample payload\n")
	sum := sha256.Sum256(payload)
	hashHex := hex.EncodeToString(sum[:])
	c := newClientWithExtraHash(t, hashHex, "TestFeed")
	raw := buildMultipart("BB",
		mimePart("text/plain", "", "clean body"),
		mimePart("application/octet-stream", "", string(payload)),
	)
	v, err := ScanMessage(context.Background(), c, []byte(raw))
	if err != nil {
		t.Fatalf("ScanMessage: %v", err)
	}
	if v.OK || v.Threat != "TestFeed" {
		t.Errorf("verdict = %+v, want hit on TestFeed", v)
	}
}

// buildMultipart wires a complete RFC 5322 message with a single
// multipart/mixed body assembled from the given parts. Each part is
// already a literal block including its own headers + blank line + body.
func buildMultipart(boundary string, parts ...string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"From: a@b.test\r\nTo: c@d.test\r\nSubject: t\r\n"+
			"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%s\r\n\r\n",
		boundary)
	for _, p := range parts {
		fmt.Fprintf(&sb, "--%s\r\n%s\r\n", boundary, p)
	}
	fmt.Fprintf(&sb, "--%s--\r\n", boundary)
	return sb.String()
}

// buildMultipartCustom is like buildMultipart but the parts can be
// arbitrary literal blocks (including pre-formed multipart sub-trees).
func buildMultipartCustom(boundary string, parts ...string) string {
	return buildMultipart(boundary, parts...)
}

// buildMultipartRaw returns just the body chunk (no top-level
// envelope headers) — useful when embedding a multipart as a nested
// part inside an outer message.
func buildMultipartRaw(boundary string, parts ...string) string {
	var sb strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&sb, "--%s\r\n%s\r\n", boundary, p)
	}
	fmt.Fprintf(&sb, "--%s--\r\n", boundary)
	return sb.String()
}

// mimePart formats a single MIME part block: Content-Type +
// optional Content-Transfer-Encoding + blank line + body.
func mimePart(contentType, transferEncoding, body string) string {
	hdr := "Content-Type: " + contentType + "\r\n"
	if transferEncoding != "" {
		hdr += "Content-Transfer-Encoding: " + transferEncoding + "\r\n"
	}
	return hdr + "\r\n" + body
}

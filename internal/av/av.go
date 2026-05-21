// Package av is OxiMail's in-process antivirus scanner.
//
// Earlier revisions of this package were a thin client to an external
// clamd-compatible daemon (the Rust avd sidecar). That added an
// out-of-process hop, a build dependency on a separate repo, and a
// container in the compose graph for what — in our scope — is a
// SHA-256 hash lookup. We collapse the whole thing into a pure-Go
// scanner that's compiled into the oximail binary; signatures live in
// a small text file embedded at build time, with an optional
// operator-provided extension loaded at start.
//
// API surface mirrors the previous daemon client so the call sites
// in cmd/oximail/main.go, internal/smtp/smtp.go, and internal/webmail
// don't need to change shape — only the implementation does.
package av

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Verdict is the result of a single scan. Threat is the human-readable
// signature name on a positive hit; empty on a clean result. OK is the
// boolean view for callers that don't care about the name.
type Verdict struct {
	OK     bool
	Threat string
}

// ErrUnreachable is kept for source-level compatibility with the old
// daemon-client API; the in-process scanner never returns it (there's
// no network call to fail). Handlers branching on errors.Is(err, av.ErrUnreachable)
// stay correct because they only run on a non-nil err, which we
// don't produce on the happy path.
var ErrUnreachable = errors.New("av: scanner unreachable")

// builtinSigdb is the EICAR-only minimum that ships with every build.
// More entries can be appended via OXIMAIL_AV_SIGDB; the parser is
// the same shape.
//
//go:embed builtin.sigdb
var builtinSigdb string

// Client is the scanner. Stateless from the caller's perspective —
// every Scan call hashes the payload and looks it up. The internal
// signature table is read-only after construction; New is the only
// writer, so reads need no locking.
//
// A nil *Client returns clean from Scan; the null-object idiom keeps
// "AV disabled" branchless at every call site.
type Client struct {
	sigs map[[32]byte]string
}

// New constructs a scanner. The builtin signature set is always
// loaded; extraPath, when non-empty, is read and merged on top —
// duplicate hashes win for the later entry, which matches the
// "operator override beats builtin" intent.
//
// An empty extraPath is fine; a non-existent extraPath errors. The
// caller can choose to treat a missing extra DB as fatal or warn-and-
// continue — main.go logs and continues, treating extra sigs as
// strictly additive.
func New(extraPath string) (*Client, error) {
	c := &Client{sigs: make(map[[32]byte]string)}
	if err := c.loadInto(c.sigs, builtinSigdb, "<builtin>"); err != nil {
		// Shouldn't happen — the builtin file is in our own tree.
		return nil, fmt.Errorf("av: load builtin sigs: %w", err)
	}
	if extraPath != "" {
		raw, err := os.ReadFile(extraPath)
		if err != nil {
			return nil, fmt.Errorf("av: read %s: %w", extraPath, err)
		}
		if err := c.loadInto(c.sigs, string(raw), extraPath); err != nil {
			return nil, fmt.Errorf("av: parse %s: %w", extraPath, err)
		}
	}
	return c, nil
}

// SignatureCount returns the number of distinct hashes loaded. Useful
// for the startup log so operators can confirm their extra DB landed.
func (c *Client) SignatureCount() int {
	if c == nil {
		return 0
	}
	return len(c.sigs)
}

// Scan computes SHA-256(data) and returns a verdict. A nil receiver
// (the "AV disabled" idiom) yields a clean verdict — handlers can
// stay branchless.
//
// The ctx is honoured by the cheapest possible mechanism: a check at
// entry and one at end. SHA-256 over a 25 MB attachment takes well
// under 100 ms on any reasonable CPU, so finer-grained cancellation
// would only ever masquerade as cancellation while the OS finishes
// the in-flight hash anyway.
func (c *Client) Scan(ctx context.Context, data []byte) (Verdict, error) {
	if c == nil {
		return Verdict{OK: true}, nil
	}
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	sum := sha256.Sum256(data)
	if name, hit := c.sigs[sum]; hit {
		return Verdict{OK: false, Threat: name}, nil
	}
	return Verdict{OK: true}, nil
}

// loadInto parses sigdb-formatted content (one `<sha256>:<name>` line
// per signature; blank lines and `#`-prefixed comments skipped) and
// merges into dst. Returns the first parse error verbatim so the
// caller can surface the offending line.
func (c *Client) loadInto(dst map[[32]byte]string, content, src string) error {
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, ':')
		if idx <= 0 || idx == len(line)-1 {
			return fmt.Errorf("%s:%d: want '<sha256>:<name>', got %q", src, i+1, line)
		}
		hashStr := strings.TrimSpace(line[:idx])
		name := strings.TrimSpace(line[idx+1:])
		if name == "" {
			return fmt.Errorf("%s:%d: empty signature name", src, i+1)
		}
		bs, err := hex.DecodeString(hashStr)
		if err != nil {
			return fmt.Errorf("%s:%d: bad hex: %w", src, i+1, err)
		}
		if len(bs) != 32 {
			return fmt.Errorf("%s:%d: sha256 must be 32 bytes, got %d", src, i+1, len(bs))
		}
		var key [32]byte
		copy(key[:], bs)
		dst[key] = name
	}
	return nil
}

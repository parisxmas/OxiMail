// Package av is OxiMail's in-process antivirus scanner.
//
// Earlier revisions of this package were a thin client to an external
// clamd-compatible daemon (the Rust avd sidecar). That added an
// out-of-process hop, a build dependency on a separate repo, and a
// container in the compose graph for what — in our scope — is a
// SHA-256 hash lookup. We collapse the whole thing into a pure-Go
// scanner that's compiled into the oximail binary; signatures live in
// a small text file embedded at build time, plus zero or more
// operator-provided extension files loaded at start (and reloaded by
// the Updater when fresh data arrives).
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
	"sync/atomic"
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
// More entries land via extraPaths configured on New().
//
//go:embed builtin.sigdb
var builtinSigdb string

// Client is the scanner. The signature map sits behind an
// atomic.Pointer so Reload() can swap a fresh version in without
// taking a lock on the hot Scan path. Scan reads the pointer once
// and does a map lookup against the snapshot it observed —
// concurrent reads + concurrent reload are both lock-free.
//
// A nil *Client returns clean from Scan; the null-object idiom keeps
// "AV disabled" branchless at every call site.
type Client struct {
	// extraPaths are the operator-provided sigdb files reloaded on
	// every Reload() call. Each path is opened, parsed, and merged
	// on top of the builtin set. An empty entry is skipped — that
	// matches how main.go threads the env var through (the variadic
	// New() accepts the env value even when unset).
	extraPaths []string

	// sigs holds the current signature map. We use atomic.Pointer so
	// the Updater can atomically swap a fresh map in without
	// blocking concurrent Scan callers, and Scan never needs to
	// lock anything.
	sigs atomic.Pointer[map[[32]byte]string]
}

// New constructs a scanner and loads the builtin signature set plus
// every non-empty extraPath. Empty path entries are skipped — that
// lets callers thread an unset env var straight through without
// guarding it themselves.
func New(extraPaths ...string) (*Client, error) {
	c := &Client{extraPaths: extraPaths}
	if err := c.Reload(); err != nil {
		return nil, err
	}
	return c, nil
}

// Reload rebuilds the signature map from the builtin embed plus the
// current contents of every extraPath, then atomically swaps the new
// map in. Concurrent Scan callers see either the old map or the new
// map for the duration of any single call; never a partial state.
//
// A missing extraPath is tolerated as "the Updater hasn't written it
// yet" rather than an error — the first call to Reload before the
// Updater fires would otherwise hard-fail and refuse to boot the
// daemon. Other I/O errors and parse errors do propagate.
func (c *Client) Reload() error {
	m := map[[32]byte]string{}
	// Source-label interner shared across the whole load: every map
	// value referencing "MalwareBazaar" points at the same Go string
	// header's backing bytes. At 1M+ entries with mostly two or three
	// distinct labels the saved memory is non-trivial (~13 MB per
	// 13-char label that would otherwise allocate per row).
	intern := map[string]string{}
	if err := c.loadInto(m, intern, builtinSigdb, "<builtin>"); err != nil {
		return fmt.Errorf("av: load builtin sigs: %w", err)
	}
	for _, p := range c.extraPaths {
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("av: read %s: %w", p, err)
		}
		if err := c.loadInto(m, intern, string(raw), p); err != nil {
			return fmt.Errorf("av: parse %s: %w", p, err)
		}
	}
	c.sigs.Store(&m)
	return nil
}

// SignatureCount returns the number of distinct hashes currently
// loaded. Useful for the startup log and for the Updater's "refreshed
// — n hashes" line.
func (c *Client) SignatureCount() int {
	if c == nil {
		return 0
	}
	m := c.sigs.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}

// Scan computes SHA-256(data) and returns a verdict. A nil receiver
// (the "AV disabled" idiom) yields a clean verdict — handlers can
// stay branchless.
//
// The ctx is honoured by the cheapest possible mechanism: a check at
// entry. SHA-256 over a 25 MB attachment takes well under 100 ms on
// any reasonable CPU, so finer-grained cancellation would only ever
// masquerade as cancellation while the OS finishes the in-flight
// hash anyway.
func (c *Client) Scan(ctx context.Context, data []byte) (Verdict, error) {
	if c == nil {
		return Verdict{OK: true}, nil
	}
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	sum := sha256.Sum256(data)
	m := c.sigs.Load()
	if m != nil {
		if name, hit := (*m)[sum]; hit {
			return Verdict{OK: false, Threat: name}, nil
		}
	}
	return Verdict{OK: true}, nil
}

// loadInto parses sigdb-formatted content (one `<sha256>:<name>` line
// per signature; blank lines and `#`-prefixed comments skipped) and
// merges into dst. `intern` interns the per-line `name` field so all
// hashes from the same source share one backing string. Returns the
// first parse error verbatim so the caller can surface the offending
// line.
func (c *Client) loadInto(dst map[[32]byte]string, intern map[string]string, content, src string) error {
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
		if canon, ok := intern[name]; ok {
			name = canon
		} else {
			// `name` is currently a substring slice of the 80+ MB
			// parse buffer. Cloning detaches it so the buffer
			// becomes GC-eligible once Reload returns — otherwise
			// the interned canonical aliases keep the whole file
			// resident for the lifetime of the map.
			canon := strings.Clone(name)
			intern[canon] = canon
			name = canon
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

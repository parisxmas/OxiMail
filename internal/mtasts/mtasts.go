// Package mtasts implements RFC 8461 SMTP MTA Strict Transport
// Security on the *sending* side. It does two things:
//
//   - Lookup discovers a recipient domain's MTA-STS policy: the DNS
//     TXT record at _mta-sts.<domain> announces an id; the policy
//     body at https://mta-sts.<domain>/.well-known/mta-sts.txt
//     declares the mode (enforce / testing / none), the allowed MX
//     hostname patterns, and the maximum policy lifetime.
//
//   - Policy.Allows tells the outbound SMTP client whether a
//     resolved MX hostname is permitted by the policy. The caller
//     pairs this with strict TLS verification (no
//     InsecureSkipVerify) for the enforce mode; failures abort the
//     delivery rather than silently downgrading to cleartext.
//
// A Cache memoises policies by domain for their max_age so the
// outbound queue does not hammer DNS / HTTPS on every batch.
//
// Publishing our own policy is the operator's job: serve the
// policy text on https://mta-sts.<our-domain>/ and add a
// _mta-sts.<our-domain> TXT record. The webmail server exposes a
// /.well-known/mta-sts.txt handler when configured so operators do
// not need a separate web server for it.
package mtasts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Mode is the policy enforcement level.
type Mode string

const (
	ModeEnforce Mode = "enforce"
	ModeTesting Mode = "testing"
	ModeNone    Mode = "none"
)

// Policy is a parsed MTA-STS policy.
type Policy struct {
	Version string
	Mode    Mode
	MX      []string // hostname patterns; "*.example.com" allowed
	MaxAge  time.Duration
}

// ErrNoPolicy means the domain has no MTA-STS DNS record or its
// policy file is unreachable.
var ErrNoPolicy = errors.New("mtasts: no policy")

// Lookup discovers the policy for a recipient domain. The two
// network calls (DNS TXT + HTTPS GET) are short-circuited if either
// one fails: a domain that does not opt in returns ErrNoPolicy.
func Lookup(ctx context.Context, domain string) (*Policy, error) {
	return DefaultLookuper().Lookup(ctx, domain)
}

// Lookuper does the IO; Cache uses one. Tests inject a fake.
type Lookuper interface {
	Lookup(ctx context.Context, domain string) (*Policy, error)
}

// netLookuper is the real implementation: DNS over net.Resolver +
// HTTPS over http.Client.
type netLookuper struct {
	resolver *net.Resolver
	client   *http.Client
}

// DefaultLookuper returns a lookuper using the host resolver and a
// shared HTTPS client with a sensible timeout.
func DefaultLookuper() Lookuper {
	return &netLookuper{
		resolver: net.DefaultResolver,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (l *netLookuper) Lookup(ctx context.Context, domain string) (*Policy, error) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	txts, err := l.resolver.LookupTXT(ctx, "_mta-sts."+domain)
	if err != nil || len(txts) == 0 {
		return nil, ErrNoPolicy
	}
	// Any of the TXT records must advertise STSv1.
	var seenV1 bool
	for _, t := range txts {
		if hasField(t, "v", "STSv1") {
			seenV1 = true
			break
		}
	}
	if !seenV1 {
		return nil, ErrNoPolicy
	}

	url := "https://mta-sts." + domain + "/.well-known/mta-sts.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("mtasts: build policy request: %w", err)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mtasts: fetch policy for %s: %w", domain, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mtasts: policy for %s: HTTP %d", domain, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("mtasts: read policy for %s: %w", domain, err)
	}
	return ParsePolicy(body)
}

// ParsePolicy parses the body of a policy file. Required fields are
// version (must be "STSv1"), mode, mx, max_age.
func ParsePolicy(body []byte) (*Policy, error) {
	p := &Policy{}
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitField(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "version":
			p.Version = val
		case "mode":
			p.Mode = Mode(strings.ToLower(val))
		case "mx":
			p.MX = append(p.MX, strings.ToLower(val))
		case "max_age":
			n, err := parseSeconds(val)
			if err != nil {
				return nil, fmt.Errorf("mtasts: bad max_age %q: %w", val, err)
			}
			p.MaxAge = n
		}
	}
	if p.Version != "STSv1" {
		return nil, fmt.Errorf("mtasts: unsupported version %q", p.Version)
	}
	switch p.Mode {
	case ModeEnforce, ModeTesting, ModeNone:
	default:
		return nil, fmt.Errorf("mtasts: unknown mode %q", p.Mode)
	}
	if p.MaxAge <= 0 {
		return nil, fmt.Errorf("mtasts: missing or non-positive max_age")
	}
	return p, nil
}

// Allows reports whether mxHost matches any of the policy's mx
// patterns. The hostname is compared case-insensitively; a leading
// "*." in the pattern is a single-label wildcard (RFC 8461 §3.2).
func (p *Policy) Allows(mxHost string) bool {
	if p == nil {
		return true
	}
	host := strings.ToLower(strings.TrimSuffix(mxHost, "."))
	for _, pat := range p.MX {
		if matchHost(pat, host) {
			return true
		}
	}
	return false
}

// matchHost implements MTA-STS's "*.example.com" rule.
func matchHost(pattern, host string) bool {
	pattern = strings.TrimSuffix(pattern, ".")
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		// Single label before the suffix; no further dots allowed.
		if !strings.HasSuffix(host, suffix) {
			return false
		}
		label := strings.TrimSuffix(host, suffix)
		return label != "" && !strings.Contains(label, ".")
	}
	return pattern == host
}

// hasField reports whether a TXT record contains a key=value pair.
// MTA-STS TXT records are semicolon-separated, like
// "v=STSv1; id=20250515T120000Z".
func hasField(record, key, value string) bool {
	for _, field := range strings.Split(record, ";") {
		k, v, ok := splitField(field)
		if !ok {
			continue
		}
		if strings.EqualFold(k, key) && strings.EqualFold(v, value) {
			return true
		}
	}
	return false
}

// splitField splits "key value" / "key:value" / "key=value" with
// surrounding whitespace.
func splitField(s string) (string, string, bool) {
	for _, sep := range []string{":", "=", " "} {
		if i := strings.Index(s, sep); i > 0 {
			k := strings.TrimSpace(s[:i])
			v := strings.TrimSpace(s[i+1:])
			if k != "" && v != "" {
				return k, v, true
			}
		}
	}
	return "", "", false
}

// parseSeconds reads a max_age value in seconds. Bare integers are
// the spec; we also accept a Go duration string for operator
// convenience.
func parseSeconds(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Second, nil
}

// Cache memoises policy lookups by domain. Each entry expires at
// max_age, or sooner if the cache is asked to refresh.
type Cache struct {
	lookuper Lookuper
	mu       sync.Mutex
	entries  map[string]cacheEntry
}

type cacheEntry struct {
	policy    *Policy
	err       error
	expiresAt time.Time
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{lookuper: DefaultLookuper(), entries: map[string]cacheEntry{}}
}

// NewCacheWithLookuper is the test constructor.
func NewCacheWithLookuper(l Lookuper) *Cache {
	return &Cache{lookuper: l, entries: map[string]cacheEntry{}}
}

// Get returns the cached policy or fetches one. ErrNoPolicy is
// cached too — a domain that does not advertise MTA-STS should not
// re-fetch on every outbound message.
func (c *Cache) Get(ctx context.Context, domain string) (*Policy, error) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	now := time.Now()
	c.mu.Lock()
	e, ok := c.entries[domain]
	c.mu.Unlock()
	if ok && now.Before(e.expiresAt) {
		return e.policy, e.err
	}

	p, err := c.lookuper.Lookup(ctx, domain)
	ttl := 5 * time.Minute // negative cache
	if p != nil {
		ttl = p.MaxAge
	}
	c.mu.Lock()
	c.entries[domain] = cacheEntry{policy: p, err: err, expiresAt: now.Add(ttl)}
	c.mu.Unlock()
	return p, err
}

package mtasts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParsePolicyHappyPath(t *testing.T) {
	src := `version: STSv1
mode: enforce
mx: mail.example.com
mx: *.alt.example.com
max_age: 86400
`
	p, err := ParsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Version != "STSv1" || p.Mode != ModeEnforce {
		t.Fatalf("got version=%q mode=%q, want STSv1/enforce", p.Version, p.Mode)
	}
	if p.MaxAge != 24*time.Hour {
		t.Errorf("max_age = %s, want 24h", p.MaxAge)
	}
	if len(p.MX) != 2 || p.MX[0] != "mail.example.com" || p.MX[1] != "*.alt.example.com" {
		t.Errorf("mx = %v", p.MX)
	}
}

func TestParsePolicyRejectsBadVersion(t *testing.T) {
	_, err := ParsePolicy([]byte("version: STSv2\nmode: enforce\nmx: x\nmax_age: 100\n"))
	if err == nil {
		t.Fatal("STSv2 policy accepted")
	}
}

func TestParsePolicyRejectsUnknownMode(t *testing.T) {
	_, err := ParsePolicy([]byte("version: STSv1\nmode: nope\nmx: x\nmax_age: 100\n"))
	if err == nil {
		t.Fatal("mode: nope accepted")
	}
}

func TestPolicyAllowsExactAndWildcard(t *testing.T) {
	p := &Policy{MX: []string{"mail.example.com", "*.alt.example.com"}}
	cases := []struct {
		host string
		want bool
	}{
		{"mail.example.com", true},
		{"MAIL.example.com", true},
		{"alt.example.com", false},          // *. is single-label, doesn't match the bare apex
		{"mx.alt.example.com", true},        // wildcard match
		{"deep.mx.alt.example.com", false},  // multi-label inside the wildcard
		{"unrelated.example.com", false},
	}
	for _, c := range cases {
		if got := p.Allows(c.host); got != c.want {
			t.Errorf("Allows(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestNilPolicyAllowsEverything(t *testing.T) {
	var p *Policy
	if !p.Allows("anything.example.com") {
		t.Error("nil policy should not block — MTA-STS does not apply")
	}
}

func TestCacheMemoisesAndNegativeCaches(t *testing.T) {
	calls := 0
	fake := lookuperFunc(func(_ context.Context, domain string) (*Policy, error) {
		calls++
		switch domain {
		case "good.test":
			return &Policy{Version: "STSv1", Mode: ModeEnforce, MX: []string{"mx.good.test"}, MaxAge: time.Hour}, nil
		default:
			return nil, ErrNoPolicy
		}
	})
	c := NewCacheWithLookuper(fake)

	// Positive hit: two Get calls share one lookup.
	p, err := c.Get(context.Background(), "good.test")
	if err != nil || p == nil {
		t.Fatalf("first Get: p=%v err=%v", p, err)
	}
	if _, _ = c.Get(context.Background(), "good.test"); calls != 1 {
		t.Errorf("memoise: calls = %d, want 1", calls)
	}

	// Negative hit: ErrNoPolicy cached so the same domain doesn't
	// look up again immediately.
	if _, err := c.Get(context.Background(), "bare.test"); !errors.Is(err, ErrNoPolicy) {
		t.Fatalf("bare.test: err = %v, want ErrNoPolicy", err)
	}
	if _, _ = c.Get(context.Background(), "bare.test"); calls != 2 {
		t.Errorf("negative-cache: calls = %d, want 2 (one positive + one negative)", calls)
	}
}

// lookuperFunc adapts a function value to the Lookuper interface for
// tests.
type lookuperFunc func(ctx context.Context, domain string) (*Policy, error)

func (f lookuperFunc) Lookup(ctx context.Context, domain string) (*Policy, error) {
	return f(ctx, domain)
}

func TestParsePolicyTolerantWhitespace(t *testing.T) {
	// MTA-STS lets fields be separated by whitespace OR colons; the
	// parser accepts both forms operators see in the wild.
	src := strings.Join([]string{
		"version STSv1",
		"mode enforce",
		"mx mx.example.com",
		"max_age 3600",
	}, "\n")
	p, err := ParsePolicy([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Mode != ModeEnforce || p.MaxAge != time.Hour {
		t.Errorf("got %+v", p)
	}
}

// Unit tests for the connection-time spam stage. Everything here is
// in-memory with an injectable clock and resolver, so these are plain
// fast tests — no build tag, no oxidb-server.
package spam

import (
	"errors"
	"net"
	"testing"
	"time"
)

// fakeClock is a controllable time source for the time-based checkers.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

// notListed is a DNSBL resolver stub that reports every name as absent.
func notListed(string) ([]string, error) {
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func TestRateLimiter(t *testing.T) {
	clk := newFakeClock()
	r := newRateLimiter(3, time.Minute)
	r.now = clk.now

	for i := 1; i <= 3; i++ {
		if !r.allow("1.2.3.4") {
			t.Fatalf("message %d: blocked, want allowed (limit 3)", i)
		}
	}
	if r.allow("1.2.3.4") {
		t.Fatal("message 4: allowed, want blocked (over the limit)")
	}
	// A different IP has its own window.
	if !r.allow("5.6.7.8") {
		t.Fatal("a different IP should not share the counter")
	}
	// The counter resets once the window has elapsed.
	clk.advance(time.Minute)
	if !r.allow("1.2.3.4") {
		t.Fatal("after the window elapsed, the counter should reset")
	}
}

func TestRateLimiterSweep(t *testing.T) {
	clk := newFakeClock()
	r := newRateLimiter(3, time.Minute)
	r.now = clk.now

	r.allow("1.2.3.4")
	clk.advance(time.Minute)
	r.sweep()
	if len(r.windows) != 0 {
		t.Fatalf("elapsed window not swept: %d entries", len(r.windows))
	}
}

func TestDNSBLChecker(t *testing.T) {
	d := newDNSBLChecker([]string{"bl.example"})
	d.lookup = func(host string) ([]string, error) {
		// "4.3.2.1.bl.example" — 1.2.3.4 reversed — is the listed one.
		if host == "4.3.2.1.bl.example" {
			return []string{"127.0.0.2"}, nil
		}
		return notListed(host)
	}

	if !d.listed("1.2.3.4") {
		t.Error("1.2.3.4 should be reported as listed")
	}
	if d.listed("8.8.8.8") {
		t.Error("8.8.8.8 should not be reported as listed")
	}
	if d.listed("not-an-ip") {
		t.Error("a non-IPv4 address should never be reported as listed")
	}
}

func TestReverseIP(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":    "4.3.2.1",
		"127.0.0.1":  "1.0.0.127",
		"2001:db8::1": "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2",
		"not-an-ip":  "",
	}
	for in, want := range cases {
		if got := reverseIP(in); got != want {
			t.Errorf("reverseIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDNSBLFailsOpen(t *testing.T) {
	d := newDNSBLChecker([]string{"bl.example"})
	d.lookup = func(string) ([]string, error) {
		return nil, errors.New("resolver unavailable")
	}
	if d.listed("1.2.3.4") {
		t.Error("a resolver error must fail open (not listed), not reject")
	}
}

func TestGreylister(t *testing.T) {
	clk := newFakeClock()
	g := newGreylister(5 * time.Minute)
	g.now = clk.now

	if v := g.check("1.2.3.4", "a@b.test"); v != Greylist {
		t.Fatalf("first contact = %v, want Greylist", v)
	}
	// An immediate retry is still too soon.
	clk.advance(30 * time.Second)
	if v := g.check("1.2.3.4", "a@b.test"); v != Greylist {
		t.Fatalf("retry after 30s = %v, want Greylist", v)
	}
	// A retry past the delay passes, and stays passed.
	clk.advance(5 * time.Minute)
	if v := g.check("1.2.3.4", "a@b.test"); v != Accept {
		t.Fatalf("retry past the delay = %v, want Accept", v)
	}
	if v := g.check("1.2.3.4", "a@b.test"); v != Accept {
		t.Fatalf("subsequent contact = %v, want Accept", v)
	}
	// A different sender from the same IP is greylisted on its own.
	if v := g.check("1.2.3.4", "other@b.test"); v != Greylist {
		t.Fatalf("a new tuple = %v, want Greylist", v)
	}
}

func TestGreylisterSweep(t *testing.T) {
	clk := newFakeClock()
	g := newGreylister(5 * time.Minute)
	g.now = clk.now

	g.check("1.2.3.4", "a@b.test") // a pending tuple
	clk.advance(time.Hour)
	g.sweep()
	if len(g.tuples) != 1 {
		t.Fatalf("pending tuple swept too early: %d entries", len(g.tuples))
	}
	clk.advance(g.pendingTTL)
	g.sweep()
	if len(g.tuples) != 0 {
		t.Fatalf("stale pending tuple not swept: %d entries", len(g.tuples))
	}
}

func TestPipelineCheck(t *testing.T) {
	clk := newFakeClock()
	p := New("")
	p.rateLimit.now = clk.now
	p.greylist.now = clk.now
	p.dnsbl.lookup = notListed

	// No connection info — the connection stage is skipped.
	if v, _ := p.Check("", "a@b.test", []string{"x@local"}, nil); v != Accept {
		t.Fatalf("empty remoteIP = %v, want Accept", v)
	}

	// A fresh sender is greylisted, then accepted on a retry past the delay.
	if v, _ := p.Check("9.9.9.9", "a@b.test", nil, nil); v != Greylist {
		t.Fatalf("first contact = %v, want Greylist", v)
	}
	clk.advance(2 * time.Minute) // past defaultGreylistDelay
	if v, _ := p.Check("9.9.9.9", "a@b.test", nil, nil); v != Accept {
		t.Fatalf("retry past the delay = %v, want Accept", v)
	}

	// A DNSBL-listed IP is rejected outright, before greylisting.
	p.dnsbl.lookup = func(string) ([]string, error) { return []string{"127.0.0.2"}, nil }
	if v, _ := p.Check("6.6.6.6", "a@b.test", nil, nil); v != Reject {
		t.Fatalf("DNSBL-listed IP = %v, want Reject", v)
	}
}

//go:build integration

package queue

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/parisxmas/OxiMail/internal/mtasts"
)

// startStubMX starts a TCP listener that accepts a connection and
// closes it without saying anything — enough to make a real SMTP
// client fail its EHLO. Returns the "host:port" target and a cleanup
// hook.
func startStubMX(t *testing.T) (target string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestDialMXWithoutPolicyFallsBackToCleartext(t *testing.T) {
	target, cleanup := startStubMX(t)
	defer cleanup()

	c, err := dialMX([]string{target}, "remote.test", nil)
	// The stub speaks no SMTP, so NewClient still "succeeds" — the
	// failure surfaces on the next command. But the dial itself
	// returns a non-nil client because we fell through to the
	// cleartext path. The point is: without a policy we never abort
	// during the dial step.
	if err != nil {
		t.Fatalf("dialMX no policy: %v", err)
	}
	if c != nil {
		_ = c.Close()
	}
}

func TestDialMXEnforceRejectsWhenStartTLSFails(t *testing.T) {
	target, cleanup := startStubMX(t)
	defer cleanup()

	// Enforce mode whitelists the stub's hostname but the stub does
	// not speak STARTTLS, so the dial must fail with the MTA-STS
	// sentinel error — and not silently fall back to cleartext.
	policy := &mtasts.Policy{
		Version: "STSv1",
		Mode:    mtasts.ModeEnforce,
		MX:      []string{hostOnly(target)},
		MaxAge:  time.Hour,
	}
	c, err := dialMX([]string{target}, "remote.test", policy)
	if c != nil {
		_ = c.Close()
		t.Fatal("dialMX returned a client under enforce mode against a stub that cannot speak STARTTLS")
	}
	if err == nil || !strings.Contains(err.Error(), "MTA-STS") {
		t.Fatalf("err = %v, want one mentioning MTA-STS", err)
	}
}

func TestDialMXEnforceRejectsDisallowedMX(t *testing.T) {
	target, cleanup := startStubMX(t)
	defer cleanup()

	// Policy in enforce mode does not list our stub's hostname.
	policy := &mtasts.Policy{
		Version: "STSv1",
		Mode:    mtasts.ModeEnforce,
		MX:      []string{"only-this-host.example.test"},
		MaxAge:  time.Hour,
	}
	c, err := dialMX([]string{target}, "remote.test", policy)
	if c != nil {
		_ = c.Close()
		t.Fatal("dialMX returned a client for an MX not on the enforce list")
	}
	if err == nil || !strings.Contains(err.Error(), "MTA-STS") {
		t.Fatalf("err = %v, want one mentioning MTA-STS", err)
	}
}

func TestDialMXTestingModeWarnsButProceeds(t *testing.T) {
	target, cleanup := startStubMX(t)
	defer cleanup()

	policy := &mtasts.Policy{
		Version: "STSv1",
		Mode:    mtasts.ModeTesting,
		MX:      []string{"only-this-host.example.test"},
		MaxAge:  time.Hour,
	}
	// Testing-mode policy with a non-matching MX should LOG but still
	// fall through to the opportunistic-STARTTLS dial, eventually
	// producing a non-nil client (the stub passes a cleartext dial).
	c, err := dialMX([]string{target}, "remote.test", policy)
	if err != nil {
		t.Fatalf("dialMX in testing mode failed: %v", err)
	}
	if c == nil {
		t.Fatal("dialMX in testing mode returned no client")
	}
	_ = c.Close()
}
